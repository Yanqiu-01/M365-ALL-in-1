package outbound

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"
)

// httpsProxyDialer 的 CONNECT 交换曾经没有 deadline：DialContext 和
// HandshakeContext 都尊重 ctx，但紧随其后的 q.Write 和 http.ReadResponse 是裸阻塞
// I/O，ctx 到那里就失效了。
//
// 一个接受了 TLS 之后装死的代理因此能让 ReadResponse 永久阻塞。后果不止这一次探测：
// probeExit 卡住 → guardSweep 的 wg.Wait() 永不返回 → runProxyGuard 的循环到不了
// ticker → 后台巡检永久停摆直到重启。CheckSelected 同样以 wg.Wait() 收尾，所以手工
// 检查会把管理接口一起挂死。一个坏代理足以让整个健康巡检静默死亡。

// silentTLSProxy 完成 TLS 握手，然后什么都不回 —— 精确复现那类代理。
func silentTLSProxy(t *testing.T) (addr string, stop func()) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	held := make(chan net.Conn, 4)
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// 握手后持住连接不回任何字节。
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			select {
			case held <- c:
			default:
				_ = c.Close()
			}
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
		for {
			select {
			case c := <-held:
				_ = c.Close()
			default:
				return
			}
		}
	}
}

func TestHTTPSProxyConnectHasADeadline(t *testing.T) {
	addr, stop := silentTLSProxy(t)
	defer stop()

	// IP 主机名会让 insecureProxyTLS 自动为真，正是真实配置里的常见形态。
	proxyURL, err := url.Parse("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	d := httpsProxyDialer{proxyURL: proxyURL}

	// ctx 没有 deadline：这一条专门验证内建上限存在。若依赖 ctx，这里就会永久挂住。
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := d.DialContext(context.Background(), "tcp", "login.microsoftonline.com:443")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a proxy that never answers CONNECT must not yield a usable tunnel")
		}
		if elapsed := time.Since(start); elapsed > connectExchangeTimeout+10*time.Second {
			t.Errorf("returned after %v, well past connectExchangeTimeout %v", elapsed, connectExchangeTimeout)
		}
	case <-time.After(connectExchangeTimeout + 15*time.Second):
		t.Fatalf("DialContext never returned; a single stalled proxy still hangs the caller "+
			"(this is what killed guardSweep's wg.Wait) after %v", time.Since(start))
	}
}

// ctx 的 deadline 更早时必须取更早的那个，否则一次手工检查仍会被拖到内建上限。
func TestHTTPSProxyConnectHonoursAnEarlierContextDeadline(t *testing.T) {
	addr, stop := silentTLSProxy(t)
	defer stop()

	proxyURL, err := url.Parse("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	d := httpsProxyDialer{proxyURL: proxyURL}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := d.DialContext(ctx, "tcp", "login.microsoftonline.com:443"); err == nil {
		t.Fatal("expected failure against a silent proxy")
	}
	elapsed := time.Since(start)
	if elapsed > 8*time.Second {
		t.Errorf("took %v; the 2s context deadline should have won over the %v builtin",
			elapsed, connectExchangeTimeout)
	}
}

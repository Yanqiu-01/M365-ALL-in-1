// phone-socks 是跑在安卓手机上的最小 SOCKS5 出口。
//
// 为什么需要它：注册站点的 ipRules 是「每个 IP 每天只能成功注册 1 次」，所以一天能注册
// 多少个号 = 当天有多少个能过 Cloudflare 的独立出口。代理池里的公网出口只有约三分之一
// 过得了 CF，且大量落在 geoip 白名单（CN/HK/MO/TW）之外，一天上限只有几十个。
//
// 手机走运营商移动网络：每切一次飞行模式，运营商就换一个 IP，而且天然是国内 IP。把手机
// 当成一个可无限轮换的出口，注册速度就不再受公网出口数量限制。
//
// 用法（PC 侧）：
//
//	GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -o phone-socks ./tools/phone-socks
//	adb push phone-socks /data/local/tmp/ && adb shell chmod 755 /data/local/tmp/phone-socks
//	adb shell /data/local/tmp/phone-socks -listen 127.0.0.1:1081 &
//	adb forward tcp:1081 tcp:1081
//
// 换 DNS 上游用 -dns 223.5.5.5,119.29.29.29（可省端口，默认 53）。
//
// 之后 PC 上的 socks5://127.0.0.1:1081 就是手机的移动网络出口。
//
// 只实现注册流程真正用到的部分：CONNECT + 域名/IPv4/IPv6 地址类型 + 无认证。刻意不做
// BIND、UDP ASSOCIATE 和用户名密码认证 —— 它只监听回环、只经 adb forward 使用，多写的
// 每一行都是多一份出错的可能。
//
// 域名解析必须自己做，不能交给系统解析器：安卓没有 /etc/resolv.conf，Go 的纯 Go 解析器
// 读不到配置就回退到 127.0.0.1:53，而手机上没有本地 DNS，于是每个域名请求都以
// 「host unreachable」告终（实测 curl --proxy socks5h 报 reply 4，socks5 传 IP 反而正常）。
// 这条路绕不开：Chrome 的 --proxy-server=socks5:// 一律在代理侧解析域名。
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	socksVersion = 0x05
	cmdConnect   = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess           = 0x00
	repGeneralFailure    = 0x01
	repHostUnreachable   = 0x04
	repCommandNotSupport = 0x07
	repAddrNotSupport    = 0x08
)

// defaultDNS 是解析域名用的上游。挑的都是在中国移动移动网络里可达的公共 DNS：
// 阿里、DNSPod、114。v4 和 v6 各给一组 —— 手机是 IPv6 为主的双栈，只填一族在对端
// 网络退化时就没有备份了。
var defaultDNS = []string{
	"223.5.5.5:53",
	"119.29.29.29:53",
	"114.114.114.114:53",
	"[2400:3200::1]:53",
	"[2402:4e00::]:53",
}

func main() {
	listen := flag.String("listen", "127.0.0.1:1081", "listen address")
	dialTimeout := flag.Duration("dial-timeout", 20*time.Second, "upstream dial timeout")
	idleTimeout := flag.Duration("idle-timeout", 5*time.Minute, "close idle tunnels after this long")
	dnsTimeout := flag.Duration("dns-timeout", 8*time.Second, "domain resolution timeout")
	dnsServers := flag.String("dns", "", "comma-separated DNS servers (host:port); empty uses the built-in list")
	quiet := flag.Bool("quiet", false, "only log errors")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "phone-socks listening on %s\n", ln.Addr())

	srv := &server{
		dialTimeout: *dialTimeout,
		idleTimeout: *idleTimeout,
		dnsTimeout:  *dnsTimeout,
		dns:         parseDNSList(*dnsServers),
		quiet:       *quiet,
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			// 监听套接字被关掉就退出；单个 accept 失败不该让整个出口停摆。
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept: %v", err)
			continue
		}
		go srv.serve(conn)
	}
}

type server struct {
	dialTimeout time.Duration
	idleTimeout time.Duration
	dnsTimeout  time.Duration
	dns         []string
	quiet       bool
}

// parseDNSList 解析 -dns，允许省略端口。空值回退到内置列表。
func parseDNSList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(item); err != nil {
			item = net.JoinHostPort(item, "53")
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return defaultDNS
	}
	return out
}

func (s *server) logf(format string, args ...any) {
	if !s.quiet {
		log.Printf(format, args...)
	}
}

func (s *server) serve(client net.Conn) {
	defer client.Close()
	// 握手阶段给一个短超时：注册流程里客户端总是立刻发数据，卡住的连接不值得占资源。
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))

	if err := readGreeting(client); err != nil {
		s.logf("greeting: %v", err)
		return
	}
	if _, err := client.Write([]byte{socksVersion, 0x00}); err != nil { // 0x00 = 无认证
		s.logf("greeting reply: %v", err)
		return
	}

	host, port, err := readConnectRequest(client)
	if err != nil {
		var se socksError
		code := byte(repGeneralFailure)
		if errors.As(err, &se) {
			code = se.reply
		}
		writeReply(client, code)
		s.logf("request: %v", err)
		return
	}

	upstream, err := s.dial(host, port)
	if err != nil {
		writeReply(client, repHostUnreachable)
		s.logf("dial %s: %v", net.JoinHostPort(host, strconv.Itoa(int(port))), err)
		return
	}
	defer upstream.Close()

	if err := writeReply(client, repSuccess); err != nil {
		s.logf("reply: %v", err)
		return
	}

	// 隧道建好之后换成空闲超时：一次注册里 CF 的长连接可能安静好几十秒，
	// 握手阶段那个 30 秒硬超时会把它切断。
	relay(client, upstream, s.idleTimeout)
}

// dial 解析目标并逐个候选地址拨号，直到有一个连上。
//
// IPv6 排在前面是有意的：手机的公网出口是运营商下发的 IPv6（实测 2409:895a:…），每切一次
// 飞行模式就整段 /64 换掉；IPv4 那侧是 CGNAT，多个用户共享同一个公网地址，既不受我们控制
// 也不一定跟着换。注册要的是「每次都是新 IP」，所以优先走会轮换的那一族。
func (s *server) dial(host string, port uint16) (net.Conn, error) {
	addrs, err := s.resolve(host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range addrs {
		target := net.JoinHostPort(ip, strconv.Itoa(int(port)))
		conn, err := net.DialTimeout("tcp", target, s.dialTimeout)
		if err == nil {
			return conn, nil
		}
		last = err
		s.logf("dial %s (%s): %v", target, host, err)
	}
	if last == nil {
		last = fmt.Errorf("no address for %s", host)
	}
	return nil, last
}

// resolve 把域名解析成候选 IP 列表（IPv6 优先）。IP 字面量直接原样返回。
//
// 不用系统解析器：安卓没有 /etc/resolv.conf，Go 会退到 127.0.0.1:53 然后必然失败。
// 这里对每个上游 DNS 建一个只往它发包的 net.Resolver —— 拨号函数忽略传入地址，
// 固定连我们选定的服务器，于是 defaultNS 那套回退逻辑完全不参与。
func (s *server) resolve(host string) ([]string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}, nil
	}
	var last error
	for _, server := range s.dns {
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				if network == "tcp" || network == "tcp4" || network == "tcp6" {
					network = "tcp"
				} else {
					network = "udp"
				}
				d := net.Dialer{Timeout: s.dnsTimeout}
				return d.DialContext(ctx, network, server)
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.dnsTimeout)
		ips, err := resolver.LookupIPAddr(ctx, host)
		cancel()
		if err != nil {
			last = err
			continue
		}
		if out := orderIPv6First(ips); len(out) > 0 {
			return out, nil
		}
		last = fmt.Errorf("%s returned no addresses for %s", server, host)
	}
	if last == nil {
		last = fmt.Errorf("no DNS server available for %s", host)
	}
	return nil, fmt.Errorf("resolve %s: %w", host, last)
}

// orderIPv6First 把 IPv6 排到前面，同族内保持解析返回的顺序。
func orderIPv6First(ips []net.IPAddr) []string {
	out := make([]string, 0, len(ips))
	for _, addr := range ips {
		if addr.IP.To4() == nil {
			out = append(out, addr.IP.String())
		}
	}
	for _, addr := range ips {
		if addr.IP.To4() != nil {
			out = append(out, addr.IP.String())
		}
	}
	return out
}

type socksError struct {
	reply byte
	msg   string
}

func (e socksError) Error() string { return e.msg }

func readGreeting(r io.Reader) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if head[0] != socksVersion {
		return fmt.Errorf("unsupported socks version %d", head[0])
	}
	n := int(head[1])
	if n == 0 {
		return errors.New("client offered no auth methods")
	}
	methods := make([]byte, n)
	if _, err := io.ReadFull(r, methods); err != nil {
		return fmt.Errorf("read auth methods: %w", err)
	}
	// 只接受「无认证」。这个出口只监听回环、只经 adb forward 使用，
	// 加一层口令不会提高安全性，只会多一处配置不一致的失败点。
	for _, m := range methods {
		if m == 0x00 {
			return nil
		}
	}
	return errors.New("client requires authentication, only no-auth is supported")
}

// readConnectRequest 返回目标主机（域名或 IP 字面量）和端口，解析留给调用方。
func readConnectRequest(r io.Reader) (string, uint16, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", 0, fmt.Errorf("read request: %w", err)
	}
	if head[0] != socksVersion {
		return "", 0, socksError{repGeneralFailure, fmt.Sprintf("unsupported socks version %d", head[0])}
	}
	if head[1] != cmdConnect {
		return "", 0, socksError{repCommandNotSupport, fmt.Sprintf("unsupported command %d (only CONNECT)", head[1])}
	}

	var host string
	switch head[3] {
	case atypIPv4:
		buf := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", 0, fmt.Errorf("read ipv4: %w", err)
		}
		host = net.IP(buf).String()
	case atypIPv6:
		buf := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", 0, fmt.Errorf("read ipv6: %w", err)
		}
		host = net.IP(buf).String()
	case atypDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return "", 0, fmt.Errorf("read domain length: %w", err)
		}
		if lenBuf[0] == 0 {
			return "", 0, socksError{repAddrNotSupport, "empty domain name"}
		}
		buf := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", 0, fmt.Errorf("read domain: %w", err)
		}
		host = string(buf)
	default:
		return "", 0, socksError{repAddrNotSupport, fmt.Sprintf("unsupported address type %d", head[3])}
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return "", 0, fmt.Errorf("read port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBuf)
	if port == 0 {
		return "", 0, socksError{repAddrNotSupport, "port 0"}
	}
	return host, port, nil
}

// writeReply 回一个绑定地址为 0.0.0.0:0 的应答。
//
// RFC 1928 要求这里填服务端为连接分配的地址，但客户端在 CONNECT 模式下并不使用它，
// 主流实现也都填零 —— 填真实地址反而会把手机的内网地址暴露给客户端。
func writeReply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{socksVersion, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// relay 双向转发，任一方向结束就收掉另一边。
//
// 关键是 idle 超时要在有流量时往后推：注册过程中 Cloudflare 的连接会安静几十秒，
// 固定 deadline 会把正在等挑战结果的连接切断。
func relay(a, b net.Conn, idle time.Duration) {
	var once sync.Once
	stop := func() {
		a.Close()
		b.Close()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	copyDir := func(dst, src net.Conn) {
		defer wg.Done()
		defer once.Do(stop)
		buf := make([]byte, 32*1024)
		for {
			if idle > 0 {
				_ = src.SetReadDeadline(time.Now().Add(idle))
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
			}
			n, rerr := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	go copyDir(a, b)
	go copyDir(b, a)
	wg.Wait()
}

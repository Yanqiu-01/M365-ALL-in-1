package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 表格版式里的 socks4 行原先在 scanIPPortWindows 里被直接丢掉：它既不在
// Discovered 也不在 Invalid，操作员看到的两个数加起来对不上页面上的行数，
// 只能怀疑解析器漏读了整页。
func TestSocks4TableRowCountsAsInvalid(t *testing.T) {
	// 三行：一条可用、一条 socks4、一条私网。Discovered 必须等于 3。
	payload := `<table><tbody>` +
		`<tr><td>HTTP</td><td>8.8.8.8</td><td>80</td></tr>` +
		`<tr><td>SOCKS4</td><td>1.1.1.1</td><td>1080</td></tr>` +
		`<tr><td>HTTP</td><td>192.168.1.9</td><td>3128</td></tr>` +
		`</tbody></table>`
	parsed, invalid := parseFreeProxyCandidates([]byte(payload), "http")
	if len(parsed) != 1 || parsed[0] != "http://8.8.8.8:80" {
		t.Fatalf("可用候选 = %v，期望仅 [http://8.8.8.8:80]", parsed)
	}
	if invalid != 2 {
		t.Fatalf("invalid = %d，期望 2（socks4 一条 + 私网一条）", invalid)
	}
	// Discovered 就是 importFreeProxySource 里的 len(parsed)+invalid。
	if discovered := len(parsed) + invalid; discovered != 3 {
		t.Fatalf("Discovered = %d，页面上有 3 行；被丢掉的行让数字对不上", discovered)
	}
}

// socks4 行仍然绝不能被当成候选送进池子 —— 计入 Invalid 不等于放它进来。
func TestSocks4RowStillNeverBecomesCandidate(t *testing.T) {
	parsed, invalid := parseFreeProxyCandidates([]byte("SOCKS4 8.8.8.8 1080"), "socks5")
	for _, candidate := range parsed {
		if strings.Contains(candidate, "8.8.8.8:1080") {
			t.Fatalf("socks4 行被当成 %s 放进了候选", candidate)
		}
	}
	if invalid != 1 {
		t.Fatalf("invalid = %d，期望 1", invalid)
	}
}

// 紧凑写法 socks4://ip:port 只能被数一次。两遍解析都会命中同一个端点，
// 去重判断必须真的生效 —— 原先它比较的是带 scheme 前缀的键与裸 host:port，
// 永远不相等。
func TestCompactSocks4CountedOnce(t *testing.T) {
	parsed, invalid := parseFreeProxyCandidates([]byte("socks4://8.8.8.8:1080"), "http")
	if len(parsed) != 0 {
		t.Fatalf("socks4 不该产出候选：%v", parsed)
	}
	if invalid != 1 {
		t.Fatalf("invalid = %d，期望 1（同一条记录被两遍各数一次就是 2）", invalid)
	}
}

// 同理，紧凑写法的私网地址也只能数一次。
func TestCompactPrivateAddressCountedOnce(t *testing.T) {
	_, invalid := parseFreeProxyCandidates([]byte("http://192.168.1.9:3128"), "http")
	if invalid != 1 {
		t.Fatalf("invalid = %d，期望 1", invalid)
	}
}

// 既有形态的计数不能被上面的改动带跑偏。
func TestValidRowsStillCountZeroInvalid(t *testing.T) {
	payload := "HTTP 8.8.8.8 80\nSOCKS5 1.1.1.1 1080\nHTTPS 9.9.9.9 8443\n"
	parsed, invalid := parseFreeProxyCandidates([]byte(payload), "http")
	if len(parsed) != 3 {
		t.Fatalf("候选 = %v，期望 3 条", parsed)
	}
	if invalid != 0 {
		t.Fatalf("invalid = %d，期望 0", invalid)
	}
}

// 抓取失败与「来源不受支持」必须是两种不同的答复。
//
// 原先两者共用一句 502「免费代理来源获取失败；仅支持公开可访问的 http/https
// 文本列表」：现场连续三次 502、每次 45s，全都是抓取超时，而那句话却在指向
// 「格式不支持」，操作员无法判断该修网络还是该换来源。
func TestFreeProxyFetchFailureAndUnsupportedSourceDiffer(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	setFreeProxyTestSeams(t,
		func(ctx context.Context, raw string) ([]byte, error) {
			return nil, errors.New("source request failed")
		},
		func(ctx context.Context, candidate string) error { return nil },
	)
	s := proxyPoolFeatureTestServer(t)

	post := func(sourceURL string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"sourceUrl":"` + sourceURL + `","scheme":"http"}`
		rec := httptest.NewRecorder()
		s.proxyPool(rec, httptest.NewRequest(http.MethodPost, "/api/admin/proxy-pool?action=free-import", strings.NewReader(body)))
		return rec
	}

	// 1) 地址合法但抓不到 -> 502，且必须说这是抓取问题、地址格式没问题。
	fetchFail := post("https://source.example/list.txt?token=do-not-echo")
	if fetchFail.Code != http.StatusBadGateway {
		t.Fatalf("抓取失败 status = %d，期望 502；body=%s", fetchFail.Code, fetchFail.Body.String())
	}
	fetchMessage := fetchFail.Body.String()
	if !strings.Contains(fetchMessage, "抓取失败") {
		t.Errorf("502 没有说明这是抓取失败：%s", fetchMessage)
	}
	if !strings.Contains(fetchMessage, "地址格式是合法的") {
		t.Errorf("502 必须排除「格式不支持」这个可能，否则操作员无从判断：%s", fetchMessage)
	}
	if strings.Contains(fetchMessage, "do-not-echo") {
		t.Errorf("来源 URL 的令牌泄漏进了响应：%s", fetchMessage)
	}

	// 2) 协议不受支持 -> 400，且不得声称抓取失败（它根本没联网）。
	unsupported := post("ftp://source.example/list.txt")
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("不受支持的来源 status = %d，期望 400；body=%s", unsupported.Code, unsupported.Body.String())
	}
	unsupportedMessage := unsupported.Body.String()
	if !strings.Contains(unsupportedMessage, "不受支持") {
		t.Errorf("400 没有说明来源不受支持：%s", unsupportedMessage)
	}
	if strings.Contains(unsupportedMessage, "抓取失败") {
		t.Errorf("没联网就不该说抓取失败：%s", unsupportedMessage)
	}

	// 两条消息必须真的不一样 —— 这就是这个修复的全部意义。
	if fetchMessage == unsupportedMessage {
		t.Fatal("两种失败仍然共用同一句答复")
	}
}

// 非公网来源同样属于「来源不受支持」，而不是抓取失败。
func TestNonPublicFreeProxySourceRejectedAsUnsupported(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	fetched := false
	setFreeProxyTestSeams(t,
		func(ctx context.Context, raw string) ([]byte, error) {
			fetched = true
			return []byte("8.8.8.8:80"), nil
		},
		func(ctx context.Context, candidate string) error { return nil },
	)
	s := proxyPoolFeatureTestServer(t)
	rec := httptest.NewRecorder()
	s.proxyPool(rec, httptest.NewRequest(http.MethodPost, "/api/admin/proxy-pool?action=free-import",
		strings.NewReader(`{"sourceUrl":"http://127.0.0.1:8080/list.txt","scheme":"http"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if fetched {
		t.Error("不受支持的来源不该被真的抓取一次")
	}
}

// 超时要单独说清，并带上实际等待预算 —— 45s 三连 502 的现场里，那是唯一有用的
// 信息，它把问题指向网络或来源限流。
func TestFreeProxyTimeoutIsDescribedWithBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	got := describeFreeProxyFetchFailure(ctx, errors.New("source request failed"), 45*time.Second)
	if !strings.Contains(got, "超时") {
		t.Fatalf("describe = %q，期望说明超时", got)
	}
	if !strings.Contains(got, "45s") {
		t.Fatalf("describe = %q，期望带上实际预算 45s", got)
	}
	// 非超时错误应原样带出，而不是一律说成超时。
	plain := describeFreeProxyFetchFailure(context.Background(), errors.New("source returned HTTP 403"), 45*time.Second)
	if !strings.Contains(plain, "403") {
		t.Fatalf("describe = %q，期望带上上游状态码", plain)
	}
	if strings.Contains(plain, "超时") {
		t.Fatalf("describe = %q，不该把 403 说成超时", plain)
	}
}

// 报告里的数字必须自洽：Discovered = Invalid + Duplicates + Limited + Checked。
// socks4 行原先被丢掉，这条等式就不成立。
func TestFreeProxyImportReportAddsUp(t *testing.T) {
	resetProxyPoolForFeatureTest(t)
	payload := `<table><tbody>` +
		`<tr><td>HTTP</td><td>8.8.8.8</td><td>80</td></tr>` +
		`<tr><td>SOCKS4</td><td>1.1.1.1</td><td>1080</td></tr>` +
		`<tr><td>HTTP</td><td>9.9.9.9</td><td>8443</td></tr>` +
		`</tbody></table>`
	setFreeProxyTestSeams(t,
		func(ctx context.Context, raw string) ([]byte, error) { return []byte(payload), nil },
		func(ctx context.Context, candidate string) error { return nil },
	)
	s := proxyPoolFeatureTestServer(t)
	rec := httptest.NewRecorder()
	s.proxyPool(rec, httptest.NewRequest(http.MethodPost, "/api/admin/proxy-pool?action=free-import",
		strings.NewReader(`{"sourceUrl":"https://source.example/list","scheme":"http","limit":30}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Report freeProxyImportReport `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	r := response.Report
	if r.Discovered != 3 {
		t.Errorf("Discovered = %d，页面上有 3 行", r.Discovered)
	}
	if r.Invalid != 1 {
		t.Errorf("Invalid = %d，期望 1（socks4 那行）", r.Invalid)
	}
	if sum := r.Invalid + r.Duplicates + r.Limited + r.Checked; sum != r.Discovered {
		t.Errorf("Invalid(%d)+Duplicates(%d)+Limited(%d)+Checked(%d) = %d ≠ Discovered(%d)",
			r.Invalid, r.Duplicates, r.Limited, r.Checked, sum, r.Discovered)
	}
}

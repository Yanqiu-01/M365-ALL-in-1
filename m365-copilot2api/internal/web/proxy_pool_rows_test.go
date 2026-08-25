package web

import (
	"strings"
	"testing"
)

func hasCandidate(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// 用户购买的 IP 就是这种三字段排布：协议 IP 端口。
func TestParseFreeProxyRowWithSeparateProtocolField(t *testing.T) {
	got, _ := parseFreeProxyCandidates([]byte("HTTP 8.8.8.8 80"), "socks5")
	if len(got) != 1 || got[0] != "http://8.8.8.8:80" {
		t.Fatalf("got %v, want [http://8.8.8.8:80] (row protocol must win over defaultScheme)", got)
	}
}

// 多行、多协议混排，每行用自己的协议。
func TestParseFreeProxyMixedProtocolRows(t *testing.T) {
	payload := "HTTP 8.8.8.8 80\nSOCKS5 1.1.1.1 1080\nHTTPS 9.9.9.9 8443\n"
	got, _ := parseFreeProxyCandidates([]byte(payload), "http")
	for _, want := range []string{"http://8.8.8.8:80", "socks5://1.1.1.1:1080", "https://9.9.9.9:8443"} {
		if !hasCandidate(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d candidates, want 3: %v", len(got), got)
	}
}

// 真实 HTML 表格：表头行不能污染结果，单元格边界要正确切分。
func TestParseFreeProxyHTMLTable(t *testing.T) {
	payload := `<table><thead><tr><th>类型</th><th>IP</th><th>PORT</th></tr></thead>` +
		`<tbody><tr><td>HTTP</td><td>8.8.8.8</td><td>80</td></tr>` +
		`<tr><td>SOCKS5</td><td>1.1.1.1</td><td>1080</td></tr></tbody></table>`
	got, _ := parseFreeProxyCandidates([]byte(payload), "http")
	if !hasCandidate(got, "http://8.8.8.8:80") || !hasCandidate(got, "socks5://1.1.1.1:1080") {
		t.Fatalf("got %v, want both table rows parsed", got)
	}
	if len(got) != 2 {
		t.Errorf("got %d candidates, want exactly 2 (header row must be ignored): %v", len(got), got)
	}
}

// 回归：既有的单 token 形态必须完全不变。
func TestParseFreeProxyLegacyFormsStillWork(t *testing.T) {
	got, _ := parseFreeProxyCandidates([]byte("socks5://1.1.1.1:1080"), "http")
	if len(got) != 1 || got[0] != "socks5://1.1.1.1:1080" {
		t.Errorf("explicit scheme: got %v", got)
	}
	// 裸 host:port 应套用 defaultScheme
	got, _ = parseFreeProxyCandidates([]byte("8.8.8.8:3128"), "https")
	if len(got) != 1 || got[0] != "https://8.8.8.8:3128" {
		t.Errorf("bare host:port with defaultScheme: got %v", got)
	}
	// 逗号分隔的一行多条
	got, _ = parseFreeProxyCandidates([]byte("8.8.8.8:80,1.1.1.1:81"), "http")
	if len(got) != 2 {
		t.Errorf("comma separated: got %v, want 2", got)
	}
}

// 没有协议列时 defaultScheme 仍然生效（host/port 分列）。
func TestParseFreeProxyRowWithoutProtocolUsesDefault(t *testing.T) {
	got, _ := parseFreeProxyCandidates([]byte("8.8.8.8 8080"), "socks5")
	if len(got) != 1 || got[0] != "socks5://8.8.8.8:8080" {
		t.Fatalf("got %v, want [socks5://8.8.8.8:8080]", got)
	}
}

// 中文噪声（地区/匿名度）不应被算成无效代理，否则计数失去意义。
func TestParseFreeProxyIgnoresNoiseColumns(t *testing.T) {
	payload := "HTTP 8.8.8.8 80 高匿 中国 江苏\n"
	got, invalid := parseFreeProxyCandidates([]byte(payload), "http")
	if len(got) != 1 || got[0] != "http://8.8.8.8:80" {
		t.Fatalf("got %v, want the single exit", got)
	}
	if invalid != 0 {
		t.Errorf("invalid = %d, want 0 (noise columns must not count)", invalid)
	}
}

// 私网地址仍必须被拒绝：行式解析不得绕过公网校验。
func TestParseFreeProxyRowStillRejectsPrivateAddress(t *testing.T) {
	got, invalid := parseFreeProxyCandidates([]byte("HTTP 127.0.0.1 80"), "http")
	if len(got) != 0 {
		t.Errorf("loopback must be rejected, got %v", got)
	}
	if invalid == 0 {
		t.Error("rejected private address should be counted as invalid")
	}
}

// socks4 是可识别但不受支持的协议行，静默跳过而不是当成主机名。
func TestParseFreeProxySkipsSocks4Rows(t *testing.T) {
	got, _ := parseFreeProxyCandidates([]byte("SOCKS4 8.8.8.8 1080\nHTTP 1.1.1.1 80"), "http")
	if hasCandidate(got, "http://8.8.8.8:1080") {
		t.Error("socks4 row must not be admitted under a substituted scheme")
	}
	if !hasCandidate(got, "http://1.1.1.1:80") {
		t.Errorf("following valid row lost: %v", got)
	}
}

// HTML 标签剥离与实体还原。解析不再依赖换行（记录靠 IP 锚点识别），
// 所以这里只断言标签消失、实体还原。
func TestHTMLToTextNormalisation(t *testing.T) {
	out := htmlToText("a&nbsp;b<td>c</td>")
	if !strings.Contains(out, "a b") {
		t.Errorf("htmlToText = %q, want &nbsp; decoded", out)
	}
	if strings.Contains(out, "<td>") || strings.Contains(out, "</td>") {
		t.Errorf("htmlToText = %q, want tags stripped", out)
	}
	if !strings.Contains(out, "c") {
		t.Errorf("htmlToText = %q, want cell text preserved", out)
	}
}

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"m365-copilot2api/internal/outbound"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	freeProxySourceMaxBytes      int64 = 256 << 10
	freeProxySourceTimeout             = 12 * time.Second
	freeProxyImportTimeout             = 45 * time.Second
	// 200 个候选按 4 并发 x 10s 超时，最坏要 8 分钟才探完，直接顶穿预算。
	// 探测是纯网络等待，几乎不吃 CPU，提到 16 后最坏约 2 分钟。
	freeProxyProbeConcurrency = 16
	freeProxyDefaultImportLimit        = 10
	freeProxyMaxImportLimit            = 200
	freeProxySourceRedirectLimit       = 3
	// 翻页抓取。快代理这类站点每页只有 12 行，只抓第一页最多就 12 条 ——
	// 想凑够成百上千个候选必须翻页。
	//
	// 页数上限 500（约 6000 个候选）：抓取是纯 IO 等待，真正的瓶颈在后面的
	// 质量探测，而不是抓取本身。间隔压到 150ms 以在礼貌与效率间取平衡；
	// 500 页约 75s 抓完。
	freeProxyMaxPages = 500
	// 页间间隔。实测快代理的节流很凶：150ms 间隔只有 3/12 成功（HTTP 567/403），
	// 1s 是 2/6，2s 是 3/6，3s 才 6/6。所以这里必须串行 + 秒级间隔 ——
	// 并发抓取只会把成功率打下去（40 页并发实测只成功 6 页）。
	freeProxyPageFetchTimeout = 30 * time.Minute
	// 被节流时的重试次数。节流是暂时的，退避后重试比直接丢掉这一页划算得多。
	freeProxyPageRetries = 2
)

// These seams keep network-dependent admission checks out of unit tests. The
// production values always use the same L2 quality gate as manual imports.
var (
	freeProxySourceFetcher      = fetchFreeProxySource
	freeProxyCandidateValidator = outbound.ValidateProxyCandidate

	// 页间间隔与重试退避。做成变量而非常量，是为了让单元测试把它们调成 0 ——
	// 否则一个「抓 5 页」的测试要真等 10 秒，整包测试从 7s 涨到 71s。
	//
	// 生产值来自实测：快代理 150ms 间隔只有 3/12 成功（HTTP 567/403），
	// 1s 是 2/6，2s 是 3/6，3s 才 6/6。
	freeProxyPageDelay      = 2500 * time.Millisecond
	freeProxyPageRetryDelay = 4 * time.Second
)

type freeProxyImportRequest struct {
	SourceURL string `json:"sourceUrl"`
	Scheme    string `json:"scheme"`
	Limit     int    `json:"limit"`
	// Pages 抓取多少页（含首页）。0/1 保持旧行为只抓给定 URL。
	Pages int `json:"pages"`
}

type freeProxyImportReport struct {
	Pages      int `json:"pages"`
	Discovered int `json:"discovered"`
	Invalid    int `json:"invalid"`
	Duplicates int `json:"duplicates"`
	Limited    int `json:"limited"`
	Checked    int `json:"checked"`
	Passed     int `json:"passed"`
	Rejected   int `json:"rejected"`
	Imported   int `json:"imported"`
}

// importFreeProxySource fetches an operator-provided public text list, accepts
// only public IP:port exits, probes them through the normal M365 L2 admission
// gate, and persists only the exits that pass. It intentionally does not expose
// the supplied source URL or candidate values in errors/responses: source URLs
// often contain short-lived provider query parameters.
func (s *Server) importFreeProxySource(w http.ResponseWriter, r *http.Request) {
	var body freeProxyImportRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if strings.TrimSpace(body.SourceURL) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "free proxy source URL is required")
		return
	}
	scheme, err := normalizeFreeProxyScheme(body.Scheme)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	limit := normalizeFreeProxyImportLimit(body.Limit)

	pages := normalizeFreeProxyPages(body.Pages)
	// 翻页抓取要给足时间：45s 是单页预算，20 页会把整个导入掐死在抓取阶段。
	timeout := freeProxyImportTimeout
	if pages > 1 {
		timeout = freeProxyPageFetchTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	payload, fetchedPages, err := fetchFreeProxyPages(ctx, body.SourceURL, pages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "proxy_source_error", "免费代理来源获取失败；仅支持公开可访问的 http/https 文本列表")
		return
	}

	parsed, invalid := parseFreeProxyCandidates(payload, scheme)
	report := freeProxyImportReport{Pages: fetchedPages, Discovered: len(parsed) + invalid, Invalid: invalid}
	existing := existingProxyEndpointKeys()
	seen := make(map[string]struct{}, len(existing)+len(parsed))
	for key := range existing {
		seen[key] = struct{}{}
	}
	candidates := make([]string, 0, min(limit, len(parsed)))
	for _, candidate := range parsed {
		key, ok := proxyEndpointKey(candidate)
		if !ok {
			// parseFreeProxyCandidates has already structurally checked the value;
			// keep this guard in case a future parser changes independently.
			report.Invalid++
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			report.Duplicates++
			continue
		}
		seen[key] = struct{}{}
		if len(candidates) >= limit {
			report.Limited++
			continue
		}
		candidates = append(candidates, candidate)
	}

	report.Checked = len(candidates)
	accepted := validateFreeProxyCandidates(ctx, candidates)
	report.Passed = len(accepted)
	report.Rejected = report.Checked - report.Passed
	if len(accepted) != 0 {
		imported, err := s.appendProxyPool(accepted)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "storage_error", err.Error())
			return
		}
		report.Imported = imported
	}

	jsonOut(w, map[string]any{
		"ok": true, "report": report,
		"proxies": outbound.ProxyPoolStatusRedacted(),
	})
}

// freeProxyPageURL 生成第 n 页的地址。
//
// 两种常见形态都要支持：
//   - 路径分页 https://www.kuaidaili.com/free/inha/    -> /free/inha/2/
//     （该站首页不带页号，第 2 页起是 /N/；实测每页 12 条）
//   - 查询分页 https://example.com/list?page=1        -> ?page=2
//
// 找不到可翻页的形态时返回 ok=false，调用方就只抓这一页 —— 绝不猜。
func freeProxyPageURL(raw string, page int) (string, bool) {
	if page <= 1 {
		return raw, true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	// 查询参数分页优先：显式的 page/pn/p 参数语义最明确。
	q := u.Query()
	for _, key := range []string{"page", "pn", "p", "pageno", "page_no"} {
		if _, ok := q[key]; ok {
			q.Set(key, strconv.Itoa(page))
			u.RawQuery = q.Encode()
			return u.String(), true
		}
	}
	// 路径分页：末段是纯数字则替换，否则在末尾追加一段页号。
	trimmed := strings.TrimSuffix(u.Path, "/")
	hadSlash := strings.HasSuffix(u.Path, "/") || u.Path == ""
	if trimmed == "" {
		return "", false
	}
	segs := strings.Split(trimmed, "/")
	last := segs[len(segs)-1]
	if n, err := strconv.Atoi(last); err == nil && n > 0 {
		segs[len(segs)-1] = strconv.Itoa(page)
	} else {
		segs = append(segs, strconv.Itoa(page))
	}
	u.Path = strings.Join(segs, "/")
	if hadSlash {
		u.Path += "/"
	}
	return u.String(), true
}

// normalizeFreeProxyPages 夹住页数。0/1 表示沿用旧行为（只抓给定 URL）。
func normalizeFreeProxyPages(pages int) int {
	if pages <= 1 {
		return 1
	}
	if pages > freeProxyMaxPages {
		return freeProxyMaxPages
	}
	return pages
}

// fetchFreeProxyPages 依次抓取多页并把正文拼起来，交给同一个解析器。
//
// 单页失败不中断：免费列表站点常见某一页 5xx 或超时，为此丢掉已经抓到的
// 十几页是不划算的。全部页都失败时才向上报错。
func fetchFreeProxyPages(ctx context.Context, rawURL string, pages int) ([]byte, int, error) {
	pages = normalizeFreeProxyPages(pages)
	if pages == 1 {
		payload, err := freeProxySourceFetcher(ctx, rawURL)
		if err != nil {
			return nil, 0, err
		}
		return payload, 1, nil
	}

	// 串行抓取。并发在这里是反效果：来源站点按来源 IP 限流，40 页并发实测
	// 只成功 6 页，而串行 + 秒级间隔能接近全成功。
	var combined []byte
	fetched := 0
	var firstErr error
	for page := 1; page <= pages; page++ {
		if ctx.Err() != nil {
			break
		}
		target := rawURL
		if page > 1 {
			next, ok := freeProxyPageURL(rawURL, page)
			if !ok {
				// 这个 URL 推导不出分页形态，抓到这里为止 —— 绝不猜。
				break
			}
			target = next
		}

		var payload []byte
		var err error
		for attempt := 0; attempt <= freeProxyPageRetries; attempt++ {
			if attempt > 0 {
				// 被节流后退避重试：节流是暂时的，直接丢掉这一页太浪费。
				select {
				case <-ctx.Done():
					return finishFreeProxyPages(combined, fetched, firstErr)
				case <-time.After(freeProxyPageRetryDelay * time.Duration(attempt)):
				}
			} else if page > 1 {
				select {
				case <-ctx.Done():
					return finishFreeProxyPages(combined, fetched, firstErr)
				case <-time.After(freeProxyPageDelay):
				}
			}
			payload, err = freeProxySourceFetcher(ctx, target)
			if err == nil {
				break
			}
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fetched++
		combined = append(combined, payload...)
		combined = append(combined, '\n')
	}
	return finishFreeProxyPages(combined, fetched, firstErr)
}

// finishFreeProxyPages 只有在一页都没抓到时才算失败。部分成功是正常结果：
// 免费列表站点常见某几页被节流，为此丢掉已抓到的十几页不划算。
func finishFreeProxyPages(combined []byte, fetched int, firstErr error) ([]byte, int, error) {
	if fetched == 0 {
		if firstErr != nil {
			return nil, 0, firstErr
		}
		return nil, 0, errors.New("no page could be fetched")
	}
	return combined, fetched, nil
}

func normalizeFreeProxyScheme(raw string) (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(raw))
	if scheme == "" {
		return "http", nil
	}
	switch scheme {
	case "http", "https", "socks5":
		return scheme, nil
	default:
		return "", errors.New("free proxy scheme must be http, https, or socks5")
	}
}

func normalizeFreeProxyImportLimit(limit int) int {
	if limit <= 0 {
		return freeProxyDefaultImportLimit
	}
	if limit > freeProxyMaxImportLimit {
		return freeProxyMaxImportLimit
	}
	return limit
}

// parseFreeProxyCandidates tolerates the common plain-text and simple HTML
// table separators used by public lists. It intentionally admits only literal
// public IP addresses: allowing a remote list to nominate arbitrary hostnames
// would turn the M365 quality probe into an SSRF primitive against local DNS.
// parseFreeProxyCandidates 从公开列表（纯文本或 HTML 表格）里提取出口。
//
// 不按「行」切分，而是以 IP 为锚点向后扫描。原因是真实页面（快代理
// /free/inha/）把每个单元格都排版到独立的一行：
//
//	<td class="kdl-table-cell">106.117.242.183</td>
//	<td class="kdl-table-cell">8090</td>
//	<td class="kdl-table-cell">
//	    HTTP
//	</td>
//
// 一条逻辑记录横跨 5 行以上，任何按行切分的实现都会把 IP、端口、协议拆散，
// 于是整页一条也提不出来。改为：先定位形如 a.b.c.d 的 IPv4，再在其后的
// 有限窗口内找第一个合法端口，并在窗口内找该记录自己的协议关键字。
//
// defaultScheme 仅作兜底：窗口内没有协议关键字时才使用。
func parseFreeProxyCandidates(payload []byte, defaultScheme string) ([]string, int) {
	text := htmlToText(string(payload))

	out := make([]string, 0, 16)
	invalid := 0

	// 第一遍：自带 scheme:// 或 ip:port 的紧凑写法，语义明确，优先采信。
	// compactSeen 记录本遍已产出的端点，供第二遍跳过 —— 同一条记录被两种
	// 形态同时命中属于解析器的自我重复，不应冒充来源里的重复。
	compactSeen := map[string]bool{}
	for _, token := range compactProxyTokens(text) {
		candidate, key, err := normalizeFreeProxyCandidate(token, defaultScheme)
		if err != nil {
			invalid++
			continue
		}
		compactSeen[key] = true
		out = append(out, candidate)
	}

	// 第二遍：以 IPv4 为锚点做窗口扫描，覆盖「IP 与端口分列」的表格形态。
	for _, hit := range scanIPPortWindows(text) {
		scheme := hit.scheme
		if scheme == "" {
			scheme = defaultScheme
		}
		candidate, key, err := normalizeFreeProxyCandidate(hit.host+":"+hit.port, scheme)
		if err != nil {
			// 私网/保留地址在此被拒；只在第一遍没见过时才计入无效，
			// 否则同一条坏记录会被数两次。
			if !compactSeen[proxyEndpointKeyOf(hit.host, hit.port)] {
				invalid++
			}
			continue
		}
		if compactSeen[key] {
			continue // 第一遍已产出，跳过解析器自身的重复
		}
		out = append(out, candidate)
	}
	// 来源自身的重复保留在 out 里，由调用方统计 Duplicates。
	return out, invalid
}

// proxyEndpointKeyOf 构造与 normalizeFreeProxyCandidate 一致的端点键，
// 用于在候选被拒时也能判断第一遍是否已经处理过同一端点。
func proxyEndpointKeyOf(host, port string) string {
	return strings.ToLower(host) + ":" + port
}

// htmlToText 把 HTML 压成纯文本：标签变空白、实体还原。不保留行结构，
// 因为记录边界靠 IP 锚点识别，而不是靠换行。
func htmlToText(text string) string {
	text = strings.TrimPrefix(text, "\ufeff")
	var b strings.Builder
	b.Grow(len(text))
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
				b.WriteByte(' ')
			}
		default:
			if depth == 0 {
				b.WriteByte(text[i])
			}
		}
	}
	text = b.String()
	for _, pair := range [][2]string{
		{"&nbsp;", " "}, {"&#160;", " "}, {"&amp;", "&"}, {"&lt;", "<"},
		{"&gt;", ">"}, {"&quot;", "\""}, {"&#58;", ":"}, {"&#39;", "'"},
	} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

// compactProxyTokens 取出形如 scheme://host:port 或 host:port 的独立 token。
func compactProxyTokens(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '\r', '\n', '\t', ' ', ',', ';', '|', '"', '\'', '(', ')', '{', '}', '[', ']':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, 8)
	for _, f := range fields {
		f = strings.Trim(f, "\ufeff`")
		if f == "" || !strings.Contains(f, ":") {
			continue
		}
		// 排除 "IP:" / ":port" 这类被排版切断的残片。
		if strings.HasSuffix(f, ":") || strings.HasPrefix(f, ":") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ipPortHit 是一次窗口扫描的结果。
type ipPortHit struct {
	host   string
	port   string
	scheme string
}

// ipPortWindow 是 IP 之后允许跨越的字符数。表格里 IP 与端口之间会夹进
// 标签残留的空白，几百字符足够覆盖，同时不至于跨到下一条记录。
const ipPortWindow = 400

// scanIPPortWindows 以 IPv4 为锚点，在其后的窗口内找端口与协议。
func scanIPPortWindows(text string) []ipPortHit {
	out := make([]ipPortHit, 0, 16)
	for i := 0; i < len(text); {
		host, next := scanIPv4At(text, i)
		if host == "" {
			i = next
			continue
		}
		end := next + ipPortWindow
		if end > len(text) {
			end = len(text)
		}
		window := text[next:end]
		// 端口紧跟其后：可能是 ":8090"，也可能被排版分隔成独立数字。
		port, portEnd := firstPortIn(window)
		if port == "" {
			i = next
			continue
		}
		// 协议必须归属于本条记录。两种版式都存在，且都会因为搜索越界而错位：
		//
		//	HTTP 1.2.3.4 80        类型在前（购买的 IP 列表、proxyhub）
		//	1.2.3.4 80 HTTP        类型在后（快代理 /free/inha/）
		//
		// 因此两侧都要看，并且两侧都必须被相邻的 IP 截断：
		// 向前只认「上一个 IP 之后」的部分，向后只认「下一个 IP 之前」的部分。
		// 优先采信更靠近本条 IP 的那一个。
		lead := text[:i]
		if cut := lastIPv4End(lead); cut >= 0 {
			lead = lead[cut:]
		}
		leadScheme, leadUnsupported := schemeNear(lead)
		leadDist := -1
		if leadScheme != "" || leadUnsupported {
			// 距离 = 关键字末尾到本条 IP 的间隔，越小越可能属于本条。
			leadDist = len(lead) - lastSchemeEnd(lead)
		}

		tail := window[portEnd:]
		if cut := nextIPv4Offset(tail); cut >= 0 {
			tail = tail[:cut]
		}
		tailScheme, tailUnsupported := schemeNear(tail)
		tailDist := -1
		if tailScheme != "" || tailUnsupported {
			tailDist = firstSchemeOffset(tail)
		}

		// 选更靠近本条 IP 的那一侧。unsupported 表示该侧确实写了协议，但那是
		// socks4 之类不受支持的类型 —— 此时应跳过本条，而不是套用兜底协议
		// 把它当成 socks5 送进池子。
		scheme, unsupported := "", false
		leadFound := leadScheme != "" || leadUnsupported
		tailFound := tailScheme != "" || tailUnsupported
		switch {
		case leadFound && tailFound:
			if leadDist <= tailDist {
				scheme, unsupported = leadScheme, leadUnsupported
			} else {
				scheme, unsupported = tailScheme, tailUnsupported
			}
		case leadFound:
			scheme, unsupported = leadScheme, leadUnsupported
		case tailFound:
			scheme, unsupported = tailScheme, tailUnsupported
		}
		if unsupported {
			i = next + portEnd
			continue
		}
		out = append(out, ipPortHit{host: host, port: port, scheme: scheme})
		i = next + portEnd
	}
	return out
}

// schemeTokens 是被识别的协议关键字。socks4 可识别但不受支持：识别它的意义在于
// 不把它误当成 socks5，调用方会回落到兜底协议。
var schemeTokens = []struct {
	token  string
	scheme string
}{
	{"socks5", "socks5"},
	{"socks4", ""},
	{"https", "https"},
	{"http", "http"},
}

// schemeNear 报告片段里最靠前的协议关键字。unsupported 为 true 表示确实写了
// 协议、但它不受支持（socks4）；此时 scheme 为空，调用方应跳过该条而不是回落。
func schemeNear(fragment string) (scheme string, unsupported bool) {
	lower := strings.ToLower(fragment)
	bestAt := -1
	for _, c := range schemeTokens {
		at := strings.Index(lower, c.token)
		if at < 0 {
			continue
		}
		if bestAt < 0 || at < bestAt {
			bestAt = at
			scheme = c.scheme
			unsupported = c.scheme == ""
		}
	}
	return scheme, unsupported
}

// firstSchemeOffset 返回片段里最靠前的协议关键字的起始偏移，没有则 -1。
func firstSchemeOffset(fragment string) int {
	lower := strings.ToLower(fragment)
	best := -1
	for _, c := range schemeTokens {
		if at := strings.Index(lower, c.token); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	return best
}

// lastSchemeEnd 返回片段里最靠后的协议关键字的结束偏移，没有则 -1。
func lastSchemeEnd(fragment string) int {
	lower := strings.ToLower(fragment)
	best := -1
	for _, c := range schemeTokens {
		if at := strings.LastIndex(lower, c.token); at >= 0 && at+len(c.token) > best {
			best = at + len(c.token)
		}
	}
	return best
}

// nextIPv4Offset 返回片段中下一个 IPv4 的起始偏移，没有则 -1。
// 用来把「本条记录」的搜索范围与下一条切开。
func nextIPv4Offset(fragment string) int {
	for i := 0; i < len(fragment); {
		host, next := scanIPv4At(fragment, i)
		if host != "" {
			return i
		}
		i = next
	}
	return -1
}

// lastIPv4End 返回片段中最后一个 IPv4 的结束偏移，没有则 -1。
func lastIPv4End(fragment string) int {
	end := -1
	for i := 0; i < len(fragment); {
		host, next := scanIPv4At(fragment, i)
		if host != "" {
			end = next
		}
		i = next
	}
	return end
}

// scanIPv4At 尝试在 i 处读出一个 IPv4，返回它与继续扫描的位置。
func scanIPv4At(text string, i int) (string, int) {
	if text[i] < '0' || text[i] > '9' {
		return "", i + 1
	}
	// 前一个字符是数字或点，说明 i 落在某个更长串的中间，跳过。
	if i > 0 && (text[i-1] == '.' || (text[i-1] >= '0' && text[i-1] <= '9')) {
		return "", i + 1
	}
	j := i
	segments := 0
	for segments < 4 {
		start := j
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		width := j - start
		if width == 0 || width > 3 {
			return "", i + 1
		}
		segments++
		if segments == 4 {
			break
		}
		if j >= len(text) || text[j] != '.' {
			return "", i + 1
		}
		j++
	}
	// 紧跟数字或点表示这是更长的串（例如版本号），不是独立 IP。
	if j < len(text) && (text[j] == '.' || (text[j] >= '0' && text[j] <= '9')) {
		return "", i + 1
	}
	return text[i:j], j
}

// firstPortIn 在窗口里找第一个合法端口，返回它与其结束偏移。
//
// 必须排除「看起来像数字但不是端口」的邻居，否则会抓错。真实页面里踩到的两类：
//
//	响应速度 0.3            → 小数片段
//	last_check_time 09:30:02 → 时间/日期片段（这一项还夹在 JSON 的 ip 与 port 之间）
//
// 判据：数字两侧不得紧邻 . - / : 这些把它绑进更大数值的字符；同时时间形如
// dd:dd:dd，所以后随冒号加两位数字的也一并排除。
func firstPortIn(window string) (string, int) {
	for i := 0; i < len(window); i++ {
		c := window[i]
		if c < '0' || c > '9' {
			continue
		}
		if i > 0 {
			prev := window[i-1]
			if prev == '.' || prev == '-' || prev == '/' || prev == ':' || (prev >= '0' && prev <= '9') {
				continue
			}
		}
		j := i
		for j < len(window) && window[j] >= '0' && window[j] <= '9' {
			j++
		}
		if j < len(window) {
			nxt := window[j]
			if nxt == '.' || nxt == '-' || nxt == '/' {
				continue // 小数或日期的一部分
			}
			// 时间 09:30:02 —— 冒号后紧跟两位数字，说明这是时钟而不是端口。
			if nxt == ':' && j+2 < len(window) &&
				window[j+1] >= '0' && window[j+1] <= '9' &&
				window[j+2] >= '0' && window[j+2] <= '9' {
				continue
			}
		}
		digits := window[i:j]
		if len(digits) < 1 || len(digits) > 5 {
			continue
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		return digits, j
	}
	return "", 0
}

// firstSchemeIn 在片段里找最靠前的协议关键字。socks4 被识别为「不受支持」，
// 返回空串让调用方回落到兜底协议，而不是误当成 socks5。
func firstSchemeIn(fragment string) string {
	lower := strings.ToLower(fragment)
	best, bestAt := "", -1
	for _, c := range schemeTokens {
		if at := strings.Index(lower, c.token); at >= 0 {
			if bestAt < 0 || at < bestAt {
				best, bestAt = c.scheme, at
			}
		}
	}
	return best
}

func normalizeFreeProxyCandidate(raw, defaultScheme string) (canonical, endpointKey string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("empty proxy")
	}
	if !strings.Contains(raw, "://") {
		raw = defaultScheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return "", "", errors.New("invalid proxy URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if _, err := normalizeFreeProxyScheme(scheme); err != nil {
		return "", "", err
	}
	if u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", "", errors.New("proxy URL must not include path, query, or fragment")
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", "", errors.New("proxy must include host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", errors.New("proxy port is invalid")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !isPublicProxyAddress(addr) {
		return "", "", errors.New("free proxy must use a public IP address")
	}

	canonicalURL := &url.URL{Scheme: scheme, Host: net.JoinHostPort(addr.String(), port)}
	canonical = canonicalURL.String()
	if err := outbound.ValidateProxyURL(canonical); err != nil {
		return "", "", err
	}
	return canonical, scheme + "://" + strings.ToLower(canonicalURL.Host), nil
}

func existingProxyEndpointKeys() map[string]struct{} {
	keys := make(map[string]struct{})
	for _, raw := range outbound.ProxyPoolRawURLs() {
		if key, ok := proxyEndpointKey(raw); ok {
			keys[key] = struct{}{}
		}
	}
	return keys
}

// proxyEndpointKey intentionally excludes URL userinfo, so a public IP source
// cannot add a second route for an already configured host:port by changing a
// harmless textual representation.
func proxyEndpointKey(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

func validateFreeProxyCandidates(ctx context.Context, candidates []string) []string {
	type result struct {
		candidate string
		accepted  bool
	}
	results := make(chan result, len(candidates))
	semaphore := make(chan struct{}, freeProxyProbeConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- result{candidate: candidate}
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, proxyAdmissionTimeout)
			err := freeProxyCandidateValidator(probeCtx, candidate)
			cancel()
			results <- result{candidate: candidate, accepted: err == nil}
		}()
	}
	wg.Wait()
	close(results)

	accepted := make([]string, 0, len(candidates))
	for result := range results {
		if result.accepted {
			accepted = append(accepted, result.candidate)
		}
	}
	return accepted
}

func fetchFreeProxySource(ctx context.Context, rawURL string) ([]byte, error) {
	target, err := parseFreeProxySourceURL(rawURL)
	if err != nil {
		return nil, err
	}
	return readFreeProxySource(ctx, newFreeProxySourceHTTPClient(), target)
}

func parseFreeProxySourceURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || len(rawURL) > 8192 {
		return nil, errors.New("invalid source URL")
	}
	u, err := url.ParseRequestURI(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid source URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("unsupported source URL scheme")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !isPublicProxyAddress(ip) {
		return nil, errors.New("non-public source URL")
	}
	return u, nil
}

func newFreeProxySourceHTTPClient() *http.Client {
	return &http.Client{
		Timeout: freeProxySourceTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialPublicProxySource,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			MaxConnsPerHost:       2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= freeProxySourceRedirectLimit {
				return errors.New("too many redirects")
			}
			_, err := parseFreeProxySourceURL(req.URL.String())
			return err
		},
	}
}

func readFreeProxySource(ctx context.Context, client *http.Client, target *url.URL) ([]byte, error) {
	if client == nil || target == nil {
		return nil, errors.New("source client is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("source request is invalid")
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.8,*/*;q=0.5")
	// 免费代理站点普遍按 User-Agent 拦非浏览器客户端（实测快代理对自定义 UA
	// 直接 403）。这里发一个常规浏览器 UA：目标是读公开页面，不是伪装身份。
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("source request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > freeProxySourceMaxBytes {
		return nil, errors.New("source response is too large")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, freeProxySourceMaxBytes+1))
	if err != nil {
		return nil, errors.New("source response could not be read")
	}
	if int64(len(body)) > freeProxySourceMaxBytes {
		return nil, errors.New("source response is too large")
	}
	return body, nil
}

// dialPublicProxySource deliberately bypasses the configured proxy pool and
// rejects loopback, private, and documentation ranges. This prevents an admin
// source URL (or a redirect/DNS answer controlled by it) from reaching local
// services while fetching a public proxy list.
func dialPublicProxySource(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid source destination")
	}
	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{literal}
	} else {
		addresses, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("source host could not be resolved")
		}
	}

	dialer := &net.Dialer{}
	var lastErr error
	for _, addr := range addresses {
		if !isPublicProxyAddress(addr) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, errors.New("public source host is unreachable")
	}
	return nil, errors.New("source host has no public address")
}

var nonPublicProxyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// EnvAllowFakeIPSource 放宽 198.18.0.0/15（RFC 2544 基准测试段）的拦截。
//
// 为什么需要这个开关：Clash/mihomo 等工具的 TUN + fake-ip 模式会把真实域名
// 映射到 198.18.0.0/15，于是 www.kuaidaili.com 解析出 198.18.0.177。这个段
// 本身确实不是公网地址，拦它是对的；但在 fake-ip 环境下它代表的是真实的公网
// 目标，一律拦死会让免费代理拉取在这类环境中永远失败。
//
// 默认仍然拦截（保守），需要时由运维显式打开。
const EnvAllowFakeIPSource = "M365_ALLOW_FAKEIP_SOURCE"

// fakeIPPrefix 是被这个开关豁免的唯一网段。不放宽 10/8、127/8、192.168/16
// 等真正的内网段 —— 那才是 SSRF 的实际风险面。
var fakeIPPrefix = netip.MustParsePrefix("198.18.0.0/15")

func allowFakeIPSource() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvAllowFakeIPSource)))
	return v == "1" || v == "true" || v == "yes"
}

func isPublicProxyAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	fakeIPAllowed := allowFakeIPSource()
	for _, prefix := range nonPublicProxyPrefixes {
		if !prefix.Contains(addr) {
			continue
		}
		// fake-ip 段在开关打开时放行，其余非公网段一律拒绝。
		if fakeIPAllowed && prefix == fakeIPPrefix {
			continue
		}
		return false
	}
	return addr.IsGlobalUnicast()
}

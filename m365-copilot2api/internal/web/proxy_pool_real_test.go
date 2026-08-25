package web

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// 真实页面回归：快代理 /free/inha/ 的表格把每个单元格排版到独立的一行，
// 一条记录横跨 5 行以上。按行切分的实现在这里一条也提不出来。
func TestParseFreeProxyRealKuaidailiPage(t *testing.T) {
	payload, err := os.ReadFile(`E:\download\claude\M365\builds\kuaidaili.html`)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	got, invalid := parseFreeProxyCandidates(payload, "http")
	t.Logf("extracted=%d invalid=%d", len(got), invalid)
	for i, v := range got {
		if i < 15 {
			t.Logf("  %s", v)
		}
	}
	if len(got) < 10 {
		t.Fatalf("only %d candidates from the real page, expected the full table (>=10)", len(got))
	}
	// 去重后必须恰好等于表格行数（12）。页面里还嵌了一份等价的 JSON，
	// 所以原始候选会成对出现；上层 importFreeProxySource 负责去重计数。
	unique := map[string]bool{}
	for _, v := range got {
		unique[v] = true
	}
	t.Logf("unique=%d", len(unique))
	if len(unique) != 12 {
		list := make([]string, 0, len(unique))
		for v := range unique {
			list = append(list, v)
		}
		sort.Strings(list)
		t.Fatalf("unique candidates = %d, want 12 (one per table row): %v", len(unique), list)
	}
	// 协议必须来自各自那一行，全部是 HTTP；错位会表现为 https/socks5 混入。
	for _, v := range got {
		if !strings.HasPrefix(v, "http://") {
			t.Errorf("unexpected scheme in %q; the table lists every row as HTTP", v)
		}
	}
}

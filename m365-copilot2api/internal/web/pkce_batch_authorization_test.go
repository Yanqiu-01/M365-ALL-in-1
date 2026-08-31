package web

import (
	"strings"
	"testing"
)

// 批量授权路径必须让每个账号的 state 都活到自己回调为止。
//
// beginPKCEAuthorization 原先写死 keepOthers=false，于是 attachPKCE 每被调用一次
// 就整表覆盖：runOAuthBatch / runRegister 在循环里逐个账号调它，报告里带回的
// authorizationUrl 除最后一个以外全部作废，那些账号回调时只会得到
// "invalid or expired state"。这正是 fix/concurrent-pkce-batch-oauth 修过的缺陷，
// 它当时只修到 /api/auth/start 那条 HTTP 路径。
func TestAttachPKCEKeepsEveryBatchStateAlive(t *testing.T) {
	s := &Server{}
	emails := []string{"a@example.com", "b@example.com", "c@example.com"}
	states := make([]string, 0, len(emails))
	for _, email := range emails {
		out, err := s.attachPKCE(panelOAuthAccount{Email: email})
		if err != nil {
			t.Fatalf("attachPKCE(%s): %v", email, err)
		}
		if out.State == "" {
			t.Fatalf("attachPKCE(%s) returned an empty state", email)
		}
		states = append(states, out.State)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pkce) != len(emails) {
		t.Fatalf("pkce 表里有 %d 条，期望 %d 条：批量里的 state 被后来者抹掉了", len(s.pkce), len(emails))
	}
	for i, state := range states {
		entry, ok := s.pkce[state]
		if !ok {
			t.Fatalf("第 %d 个账号的 state 不在表里；它回调时会拿到 invalid or expired state", i+1)
		}
		if entry.Status != "pending" {
			t.Fatalf("state %d status=%q，期望 pending", i+1, entry.Status)
		}
		if entry.Verifier == "" {
			t.Fatalf("state %d 没有 verifier，无法兑换 code", i+1)
		}
	}
	// verifier 不能串：每个 state 必须配自己的那一个。
	verifiers := map[string]string{}
	for state, entry := range s.pkce {
		if previous, clash := verifiers[entry.Verifier]; clash {
			t.Fatalf("state %q 与 %q 共用同一个 verifier", state, previous)
		}
		verifiers[entry.Verifier] = state
	}
}

// 交互式单账号路径必须保持独占，否则 UI 会反复进入上一次的回调。
// 这条与上面一条一起把 keepOthers 的两种语义都钉住。
func TestInteractiveBeginPKCEStaysExclusive(t *testing.T) {
	s := &Server{}
	first, _, _, _, err := s.beginPKCEAuthorization("login", false)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, _, err := s.beginPKCEAuthorization("login", false)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pkce[first]; ok {
		t.Fatal("交互式授权没有清掉上一个 state；UI 会重复进入上一次回调")
	}
	if _, ok := s.pkce[second]; !ok {
		t.Fatal("当前 state 必须在表里")
	}
}

// 授权 URL 必须带自己那一份 challenge：state 共存但参数串了同样是坏的。
func TestBatchAuthorizationURLsAreDistinct(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		out, err := s.attachPKCE(panelOAuthAccount{Email: "x@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.AuthorizationURL, "state="+out.State) {
			t.Fatalf("授权 URL 与 state 不匹配：%s", out.AuthorizationURL)
		}
		if !strings.Contains(out.AuthorizationURL, "code_challenge=") {
			t.Fatalf("授权 URL 缺少 code_challenge：%s", out.AuthorizationURL)
		}
		if seen[out.AuthorizationURL] {
			t.Fatal("两个账号拿到了完全相同的授权 URL")
		}
		seen[out.AuthorizationURL] = true
	}
}

// maxPendingPKCE 必须严格大于单批上限，否则满批时自己淘汰自己的 state。
// 这条守着注释里那个不变量 —— 原注释写「批量上限 16，64 留出余量」，
// 两个数都对不上代码。
func TestPendingPKCETableLeavesHeadroomOverBatchCap(t *testing.T) {
	if maxPendingPKCE <= panelOAuthBatchMax {
		t.Fatalf("maxPendingPKCE=%d 不大于 panelOAuthBatchMax=%d：满批授权会淘汰自己的 state",
			maxPendingPKCE, panelOAuthBatchMax)
	}
	if maxPendingPKCE <= panelRegisterMax {
		t.Fatalf("maxPendingPKCE=%d 不大于 panelRegisterMax=%d", maxPendingPKCE, panelRegisterMax)
	}
	// 满批授权后每一条都必须还在。
	s := &Server{}
	states := make([]string, 0, panelOAuthBatchMax)
	for i := 0; i < panelOAuthBatchMax; i++ {
		out, err := s.attachPKCE(panelOAuthAccount{Email: "n@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, out.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, state := range states {
		if _, ok := s.pkce[state]; !ok {
			t.Fatalf("满批 %d 个账号时第 %d 个 state 已被淘汰", panelOAuthBatchMax, i+1)
		}
	}
}

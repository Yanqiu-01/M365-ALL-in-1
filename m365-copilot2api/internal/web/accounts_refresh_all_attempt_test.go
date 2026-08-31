package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// refreshAllAttemptResponse 比 refreshAllResponse 多读 not_attempted 与
// 每条结果的 status，用来区分「网关没轮到它」和「账号刷新失败」。
type refreshAllAttemptResponse struct {
	Total        int `json:"total"`
	Refreshed    int `json:"refreshed"`
	Failed       int `json:"failed"`
	NotAttempted int `json:"not_attempted"`
	Results      []struct {
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"results"`
}

// 预算耗尽时，排在信号量后面、从未发起过刷新的账号不得计入 failed。
//
// 账号数远大于 refreshAllConcurrency(=4)：前 4 个占住信号量并卡在慢端点上，
// 其余账号一直在 select 上等，请求被取消后直接 return，从未调用 ForceRefresh。
// 原实现把这些账号一律写成 ok=false / error="timeout" 并计入 failed —— 管理台
// 于是把一批健康账号报成刷新失败，而失败的其实是网关自己的预算。
func TestRefreshAllDoesNotBookUnattemptedAccountsAsFailures(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	// 清理是 LIFO：先放行阻塞的 handler，ts.Close 才不会等在它上面。
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv("M365_TOKEN_ENDPOINT", server.URL)

	const accounts = 12
	s := &Server{tokens: refreshAllTestStore(t, accounts)}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refreshAllAccounts(rec, req)
	}()
	// 让前 refreshAllConcurrency 个账号拿到信号量并卡住，再取消预算。
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("handler hung after the budget was exhausted")
	}

	var payload refreshAllAttemptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Total != accounts {
		t.Fatalf("total=%d want %d", payload.Total, accounts)
	}
	if payload.NotAttempted == 0 {
		t.Fatalf("no account was reported as not attempted; every queued account was booked as a failure: %s", rec.Body.String())
	}
	// 从未发起的账号数不可能超过「账号总数 - 并发上限」之外的范围，
	// 且 failed 必须只包含真的试过的账号。
	if payload.Failed > refreshAllConcurrency {
		t.Fatalf("failed=%d exceeds the number of accounts that could have been attempted (%d): %s",
			payload.Failed, refreshAllConcurrency, rec.Body.String())
	}
	if got := payload.Refreshed + payload.Failed + payload.NotAttempted; got != payload.Total {
		t.Fatalf("refreshed+failed+not_attempted=%d, want total=%d: %s", got, payload.Total, rec.Body.String())
	}

	statuses := map[string]int{}
	for _, entry := range payload.Results {
		if entry.Status == "" {
			t.Fatalf("entry %+v carries no status", entry)
		}
		statuses[entry.Status]++
		if entry.Status == refreshStatusNotAttempted {
			if entry.OK {
				t.Errorf("not-attempted entry %+v is reported as ok", entry)
			}
			// 错误文案必须说清这是网关的排队结果，不是账号的问题。
			if entry.Error == "timeout" {
				t.Errorf("entry %+v is still labelled a timeout despite never being attempted", entry)
			}
		}
	}
	if statuses[refreshStatusNotAttempted] != payload.NotAttempted {
		t.Fatalf("status counts %v disagree with not_attempted=%d", statuses, payload.NotAttempted)
	}
}

// 正常路径不受影响：全部成功时 status 为 refreshed，not_attempted 为 0。
func TestRefreshAllMarksSuccessfulAccountsRefreshed(t *testing.T) {
	refreshAllTokenEndpoint(t, nil)
	s := &Server{tokens: refreshAllTestStore(t, 3)}
	rec := httptest.NewRecorder()
	s.refreshAllAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil))

	var payload refreshAllAttemptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Refreshed != 3 || payload.Failed != 0 || payload.NotAttempted != 0 {
		t.Fatalf("summary=%+v body=%s", payload, rec.Body.String())
	}
	for _, entry := range payload.Results {
		if entry.Status != refreshStatusRefreshed || !entry.OK {
			t.Errorf("entry %+v should be refreshed", entry)
		}
	}
}

// 上游拒绝是真的失败：status=failed 且计入 failed，不能被误归到 not_attempted。
func TestRefreshAllStillCountsUpstreamRejectionAsFailure(t *testing.T) {
	refreshAllTokenEndpoint(t, map[string]bool{"refresh-token-u-2-SECRET-VALUE": true})
	s := &Server{tokens: refreshAllTestStore(t, 3)}
	rec := httptest.NewRecorder()
	s.refreshAllAccounts(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-all", nil))

	var payload refreshAllAttemptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.Refreshed != 2 || payload.Failed != 1 || payload.NotAttempted != 0 {
		t.Fatalf("summary=%+v body=%s", payload, rec.Body.String())
	}
	for _, entry := range payload.Results {
		if entry.ID == "u-2" && entry.Status != refreshStatusFailed {
			t.Fatalf("rejected account has status %q, want %q", entry.Status, refreshStatusFailed)
		}
	}
}

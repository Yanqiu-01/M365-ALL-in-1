package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// pinDeploymentStore 把全局部署存储的落盘路径钉在临时目录里。
//
// openDeployments 是 sync.OnceValue：路径在包内第一次调用时就定下来了，此后
// 设置 M365_DATA_DIR 也改不动它。若那次调用发生在未设置该变量的时候，路径会落在
// 真实的 ~/.config/m365-copilot2api 下，而 deploymentCheck 结尾无条件 st.save()。
// 因此这里直接改字段并在测试结束后还原，不依赖测试执行顺序。
func pinDeploymentStore(t *testing.T) *deploymentStore {
	t.Helper()
	st := openDeployments()
	dir := t.TempDir()
	st.mu.Lock()
	originalPath, originalItems := st.path, st.Items
	st.path = filepath.Join(dir, "deployments.json")
	st.Items = nil
	pinned := st.path
	st.mu.Unlock()
	if !strings.HasPrefix(pinned, dir) {
		t.Fatalf("deployment store path %q escaped %q", pinned, dir)
	}
	t.Cleanup(func() {
		st.mu.Lock()
		st.path, st.Items = originalPath, originalItems
		st.mu.Unlock()
	})
	return st
}

// customUrl 由运维通过 PUT 存进来，完全没有校验。一个无法解析的地址会让
// http.NewRequestWithContext 返回 (nil, err)，而那个错误以前被丢掉，nil 请求
// 直接交给 Do —— Client.do 立刻解引用 req.URL，整个 handler 以 nil 指针 panic
// 收场，而不是回一条「这个地址不合法」。
func TestDeploymentCheckRejectsUnparsableURLInsteadOfPanicking(t *testing.T) {
	for _, target := range []string{
		"http://exa mple.com",        // 主机名里有空格
		"http://[::1",                // 括号不闭合
		"http://example.com/\x7f",    // 控制字符
		":://not-a-url",              // 缺少 scheme
		"http://example.com/\n/evil", // 换行注入
	} {
		t.Run(target, func(t *testing.T) {
			st := pinDeploymentStore(t)
			st.mu.Lock()
			st.Items = []deployment{{ID: "dep-1", ActiveURL: target, DefaultURL: target, Status: "deployed"}}
			st.mu.Unlock()

			s := &Server{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/deployment/check?id=dep-1", nil)

			// panic 会直接让测试失败并带上栈，这正是要防的行为。
			s.deploymentCheck(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d want 200 with ok=false: %s", rec.Code, rec.Body.String())
			}
			var payload struct {
				OK         bool `json:"ok"`
				Deployment struct {
					Status    string `json:"status"`
					LastError string `json:"lastError"`
				} `json:"deployment"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if payload.OK {
				t.Fatalf("unparsable URL reported healthy: %s", rec.Body.String())
			}
			if payload.Deployment.Status != "unhealthy" {
				t.Fatalf("status=%q want unhealthy: %s", payload.Deployment.Status, rec.Body.String())
			}
			// 错误必须说清是地址不合法，运维据此知道要改 customUrl。
			if !strings.Contains(payload.Deployment.LastError, "invalid deployment URL") {
				t.Fatalf("lastError=%q does not identify the bad URL", payload.Deployment.LastError)
			}
		})
	}
}

// 可解析但不可达的地址仍然走原来的传输错误路径，不能被误报成 URL 不合法。
func TestDeploymentCheckStillReportsTransportFailures(t *testing.T) {
	st := pinDeploymentStore(t)
	// 127.0.0.1:1 上没有监听者：地址合法，连接失败。
	st.mu.Lock()
	st.Items = []deployment{{ID: "dep-2", ActiveURL: "http://127.0.0.1:1", DefaultURL: "http://127.0.0.1:1"}}
	st.mu.Unlock()

	s := &Server{}
	rec := httptest.NewRecorder()
	s.deploymentCheck(rec, httptest.NewRequest(http.MethodPost, "/api/admin/deployment/check?id=dep-2", nil))

	var payload struct {
		OK         bool `json:"ok"`
		Deployment struct {
			Status    string `json:"status"`
			LastError string `json:"lastError"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if payload.OK || payload.Deployment.Status != "unhealthy" {
		t.Fatalf("unreachable deployment should be unhealthy: %s", rec.Body.String())
	}
	if strings.Contains(payload.Deployment.LastError, "invalid deployment URL") {
		t.Fatalf("a transport failure was misreported as an invalid URL: %q", payload.Deployment.LastError)
	}
}

// 健康检查通过的路径保持不变。
func TestDeploymentCheckReportsHealthyTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	st := pinDeploymentStore(t)
	st.mu.Lock()
	st.Items = []deployment{{ID: "dep-3", ActiveURL: server.URL, DefaultURL: server.URL}}
	st.mu.Unlock()

	s := &Server{}
	rec := httptest.NewRecorder()
	s.deploymentCheck(rec, httptest.NewRequest(http.MethodPost, "/api/admin/deployment/check?id=dep-3", nil))
	if !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), `"status":"healthy"`) {
		t.Fatalf("healthy deployment was not reported healthy: %s", rec.Body.String())
	}
}

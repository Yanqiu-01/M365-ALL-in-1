package web

// 停止批量注册时给操作者看的那句话本身就是安全设施。
//
// 取消是在号与号之间生效的，所以「当前这一批会跑完再退出」是假的：操作者信了它就会
// 当这批已完整、从进度之后接着跑，中间那些号被永久跳过 —— 而它们可能已经建在站点上、
// 密码没写进账密清单，再也拿不回来（已有 5831/5832/5881 三个这样的孤号）。
// 这几个用例把「不许这么说」和「必须交出续跑点」都钉住。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 停止路由的提示不许声称本批会跑完，而且必须把续跑点交出来。
func TestRegisterJobStopDetailGivesResumePointAndNoBatchCompletionClaim(t *testing.T) {
	server, cookie := panelTestServer(t)
	job := server.registerJob()
	// 直接把状态摆成「正在跑第一批」，不真的起一个 goroutine：要测的是停止路由怎么
	// 说话，不该顺带依赖一次真实注册跑起来。cancel 只需要可调用。
	job.mu.Lock()
	job.cancel = func() {}
	job.state = registerJobState{
		Running: true, Mode: "phone", StartNum: 5981, Target: 6100,
		BatchSize: 20, NextNum: 5981, StartedAt: time.Now(), UpdatedAt: time.Now(),
		Detail: "注册 5981-6000",
	}
	job.mu.Unlock()

	recorder := panelRequest(t, server, cookie, http.MethodPost, "/api/admin/panel/job/register/stop", `{}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Stopped bool   `json:"stopped"`
		Detail  string `json:"detail"`
		Job     struct {
			Notes []string `json:"notes"`
		} `json:"job"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Stopped {
		t.Fatal("有任务在跑，stop 却报告没有")
	}
	// 「跑完」是那句假话的核心词：本批剩下的号一个都不会注册。
	if strings.Contains(payload.Detail, "跑完") {
		t.Fatalf("提示仍在声称本批会跑完：%s", payload.Detail)
	}
	if !strings.Contains(payload.Detail, strconv.Itoa(5981)) {
		t.Fatalf("提示没有给出续跑点 5981：%s", payload.Detail)
	}
	// 续跑点还要落进 note，否则关掉这个响应之后就只能靠人回忆。
	if !strings.Contains(strings.Join(payload.Job.Notes, "\n"), "5981") {
		t.Fatalf("note 里没有续跑点：%v", payload.Job.Notes)
	}
}

// 实测形态：停止落在批次中间，状态里的 nextNum 还停在批次起点，而清单里已经多写了
// 几个号。日志必须把这两个数都说出来，否则操作者只能按批长自己算，算错就是永久缺口。
func TestRegisterJobStopInsideBatchNamesBothBatchStartAndUntriedNumber(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	server := &Server{}
	var registered int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前两个号照常成功，第二个之后就地请求停止：这样取消一定落在本批中间，
		// 而不是批次边界上。
		if atomic.AddInt64(&registered, 1) == 2 {
			server.registerJob().stop()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, site.URL)

	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}),
		registerJobRequest{Mode: "phone", StartNum: 5981, Target: 6100, BatchSize: 20, SkipOAuth: true}); err != nil {
		t.Fatal(err)
	}
	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
	notes := strings.Join(final.Notes, "\n")
	if final.NextNum != 5981 {
		t.Fatalf("nextNum = %d，中断的批次不该推进续跑点", final.NextNum)
	}
	// 断点：第 3 个号（5983）一次都没尝试过。
	if !strings.Contains(notes, "5983") {
		t.Fatalf("日志没说本批未尝试的第一个号：%v", final.Notes)
	}
	if !strings.Contains(notes, "下一次从 5981 起跑") {
		t.Fatalf("日志没把续跑点交出来：%v", final.Notes)
	}
}

// 任务自己停下时，日志里也必须写清「哪些号没尝试过」，而不是只留一句「已停止」。
func TestRegisterJobStopNoteNamesTheUntriedNumber(t *testing.T) {
	stubRotate(t)
	stubTunnel(t, "2409:895a:1::1", nil)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upn": "ok"})
	}))
	defer site.Close()
	root := t.TempDir()
	writeRunnerPanelConfig(t, root, site.URL)

	server := &Server{}
	if _, err := server.startRegisterJob(newNativePanelManager(nativePanelConfig{Root: root}),
		registerJobRequest{Mode: "phone", StartNum: 5981, Target: 9000, BatchSize: 20, SkipOAuth: true}); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, server, func(st registerJobState) bool { return st.Running })
	if stopped := server.registerJob().stop(); !stopped {
		t.Fatal("有任务在跑，stop 却报告没有")
	}
	final := waitForJob(t, server, func(st registerJobState) bool { return !st.Running })
	notes := strings.Join(final.Notes, "\n")
	if !strings.Contains(notes, "未尝试") {
		t.Fatalf("停止日志没说哪些号没尝试过：%v", final.Notes)
	}
	if !strings.Contains(notes, strconv.Itoa(final.NextNum)) {
		t.Fatalf("停止日志没给出续跑点 %d：%v", final.NextNum, final.Notes)
	}
}

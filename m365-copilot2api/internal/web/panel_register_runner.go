package web

// 批量注册的长跑任务。
//
// 为什么要放进网关：单次 /api/admin/panel/register 上限 20 个号，而目标是几千个 —— 靠外部
// 脚本反复调接口，脚本进程和它派生的 curl / adb 在 Windows 上各自弹控制台窗口，一直闪；
// 而且脚本一关任务就断，进度只存在于它自己的日志里。放进网关之后，任务和网关同生命周期，
// 进度可查，停止可控，也不再需要外部进程。
//
// 一次只允许一个任务：站点按 IP 限流、手机只有一个出口，并发跑只会让两个任务互相抢同一个
// IP 的当日额度。

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/exitrotate"
	"m365-copilot2api/internal/outbound"
)

// registerJobState 是一个批量注册任务的可见进度。
type registerJobState struct {
	Running    bool      `json:"running"`
	Mode       string    `json:"mode"`
	StartNum   int       `json:"startNum"`
	Target     int       `json:"target"`
	BatchSize  int       `json:"batchSize"`
	NextNum    int       `json:"nextNum"`
	Success    int       `json:"success"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	Batches    int       `json:"batches"`
	Imported   int       `json:"oauthImported"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// LastIP 是最近一次实测到的手机出口地址，用来一眼看出轮换还在动。
	LastIP string `json:"lastIp,omitempty"`
	// Detail 说明任务当前在做什么，或者为什么停了。
	Detail string `json:"detail,omitempty"`
	// Notes 只留最近若干条，避免长跑任务把内存吃掉。它背后是一个浏览器在轮询的状态
	// 接口，不能无限长；完整历史在 LogPath 那份落盘日志里。
	Notes []string `json:"notes,omitempty"`
	// LogPath 是落盘日志的位置。要回传：跑完之后要靠它翻出哪些号失败了、该重试哪些，
	// 而内存里那 40 条早就被后面的批次挤掉了。
	LogPath string `json:"logPath,omitempty"`
}

const registerJobMaxNotes = 40

type registerJob struct {
	mu     sync.Mutex
	state  registerJobState
	cancel context.CancelFunc
	// logFile 是落盘日志的句柄，整个任务期间只开一次；nil 表示这次不落盘。
	logFile *os.File
}

// registerJobRequest 是启动一个长跑任务的入参。
type registerJobRequest struct {
	Mode      string `json:"mode"`
	StartNum  int    `json:"startNum"`
	Target    int    `json:"target"`
	BatchSize int    `json:"batchSize"`
	// SkipOAuth 关掉每批之后的 OAuth 补齐。默认是补的：注册时内联那一次会因为池子里
	// 的代理超时而只拿到设备码，不补就会攒下一堆没有 token 的号。
	SkipOAuth bool `json:"skipOAuth"`
}

func (j *registerJob) snapshot() registerJobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.state
	out.Notes = append([]string(nil), j.state.Notes...)
	return out
}

func (j *registerJob) note(format string, args ...any) {
	now := time.Now()
	body := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] %s", now.Format("15:04:05"), body)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendNoteLocked(line)
	// 落盘那份带完整日期。内存里只有 [HH:MM:SS]：一次跑几千个号要跨过午夜，事后翻
	// 日志时「03:12:07 失败」根本分不清是哪一天的哪一轮。
	j.writeLogLocked(now.Format("2006-01-02 15:04:05") + " " + body)
	j.state.UpdatedAt = now
	log.Printf("[register-job] %s", line)
}

func (j *registerJob) appendNoteLocked(line string) {
	j.state.Notes = append(j.state.Notes, line)
	if n := len(j.state.Notes); n > registerJobMaxNotes {
		j.state.Notes = j.state.Notes[n-registerJobMaxNotes:]
	}
}

// openLog 打开落盘日志，整个任务期间只开这一次。
//
// 为什么不是每条 note 都 open-append-close：note 在 j.mu 里被调用，而
// /job/register/status 要拿同一把锁。开文件要走一遍路径解析和目录项查找，把「开、写、
// 关」三次系统调用都塞进锁里，浏览器那边的轮询就得跟着排队；一次开好之后，锁里只剩
// 一次 write —— 代价最小的那部分才留在临界区。
//
// 代价是任务跑着的时候 Windows 会占住这个文件：能读、能 tail，但删不掉也改不了名。
// 对一个「事后拿来查哪些号要重试」的日志来说，这个取舍是划算的。
//
// 打不开就只记一条然后照常注册。日志是诊断，任务才是值钱的东西：不该因为写不了日志
// 让一个准备跑一天的批量注册启动即停。
func (j *registerJob) openLog(manager *nativePanelManager) {
	path, err := manager.registerLogPath()
	if err == nil {
		var file *os.File
		if file, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			j.mu.Lock()
			j.logFile = file
			j.state.LogPath = path
			j.mu.Unlock()
			return
		}
	}
	j.note("日志落盘不可用，进度只留在内存里（最近 %d 条）：%v", registerJobMaxNotes, err)
}

// writeLogLocked 追加一行。调用方必须已经持有 j.mu。
//
// 写失败就地放弃这份日志，只报一次。留着句柄的话每条 note 都会再试一次，而 note 跑在
// j.mu 里、状态接口要拿同一把锁 —— 一个坏掉的目标（磁盘满了、U 盘被拔了）会把面板一起
// 拖住。诊断降级好过任务和面板都停摆。
func (j *registerJob) writeLogLocked(line string) {
	if j.logFile == nil {
		return
	}
	if _, err := j.logFile.WriteString(line + "\n"); err != nil {
		file := j.logFile
		j.logFile = nil
		_ = file.Close()
		j.appendNoteLocked(fmt.Sprintf("[%s] 日志写入失败，后续进度只留在内存里：%v",
			time.Now().Format("15:04:05"), err))
	}
}

func (j *registerJob) closeLog() {
	j.mu.Lock()
	file := j.logFile
	j.logFile = nil
	j.mu.Unlock()
	// Close 放在锁外：它是系统调用，没有理由让状态接口等它。
	if file != nil {
		_ = file.Close()
	}
}

func (j *registerJob) update(mutate func(*registerJobState)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	mutate(&j.state)
	j.state.UpdatedAt = time.Now()
}

// stop 请求取消。返回是否真的有任务在跑 —— 没有就如实说，不假装停掉了什么。
func (j *registerJob) stop() bool {
	j.mu.Lock()
	cancel, running := j.cancel, j.state.Running
	j.mu.Unlock()
	if !running || cancel == nil {
		return false
	}
	cancel()
	return true
}

const (
	// registerJobDefaultBatch 是配置里也没写 register_batch_size 时的兜底值，取单次注册
	// 接口的上限：一批越大，中途停止的粒度越粗。
	registerJobDefaultBatch = 20
	// registerJobMaxTunnelWait 是隧道修不好时的等待上限。手机可能正在重启或线被拔了，
	// 值得等一会儿；但不能无限等下去，否则任务表面在跑、实际什么都没做。
	registerJobMaxTunnelWait  = 30
	registerJobTunnelInterval = 60 * time.Second
	// registerJobMaxDeadBatches 是连续「一个都没成」的批次上限。到了就停 —— 那通常是
	// 链路或站点层面变了，继续跑只会把号段白白划过去。
	registerJobMaxDeadBatches = 3
)

// registerBatchSize 给出长跑任务实际会用的每批数量。
//
// 上限压到 panelRegisterMax：runRegister 本身不接受更大的值，配置里写了 500 也只会让
// 每一批都以「单次最多注册 20 个账号」失败。配置缺失或非法时回落到默认值，老的
// config.json 里没有这个键就跟以前完全一样。
func (cfg nativePanelFileConfig) registerBatchSize() int {
	batch := cfg.Register.RegisterBatch
	if batch <= 0 {
		batch = registerJobDefaultBatch
	}
	if batch > panelRegisterMax {
		batch = panelRegisterMax
	}
	return batch
}

// registerJobConfiguredBatch 读配置里的每批数量。
//
// 读不到就用默认值，不把错误往上抛：数据目录真有问题时该由 runRegister 在第一批报出
// 真正的原因（缺 site_url、账密清单不可写……），而不是让任务以「每批多少个」这种无关
// 的名义启动失败。
func registerJobConfiguredBatch(manager *nativePanelManager) int {
	if manager == nil {
		return registerJobDefaultBatch
	}
	_, cfg, err := manager.panelData()
	if err != nil {
		return registerJobDefaultBatch
	}
	return cfg.registerBatchSize()
}

// registerJobHasProxyExit 判断 proxy 模式起跑时到底有没有出口可用。
//
// 看的是「池子里有没有条目」，不是「条目能不能连通」：可达性是用户自己判断的事，这里只
// 关心 runRegister 会不会拿到一个空出口然后直连。长跑任务没有请求级 proxy 可用，池子空
// 就是真的没有出口。
func registerJobHasProxyExit() bool {
	return len(outbound.ProxyPoolRawURLs()) > 0
}

// startRegisterJob 起一个后台批量注册任务。
//
// ctx 刻意不用请求的 ctx：HTTP 响应一发完请求就取消了，任务必须活得比它长。取消由
// /api/admin/panel/job/register/stop 走 j.cancel 完成。
func (s *Server) startRegisterJob(manager *nativePanelManager, request registerJobRequest) (registerJobState, error) {
	if s == nil {
		return registerJobState{}, fmt.Errorf("服务未就绪")
	}
	if manager == nil {
		return registerJobState{}, fmt.Errorf("本地面板不可用，无法定位账密清单")
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = "phone"
	}
	if mode != "phone" && mode != "clash" && mode != "proxy" {
		return registerJobState{}, fmt.Errorf("未知注册模式，支持 phone / clash / proxy")
	}
	if request.StartNum <= 0 {
		return registerJobState{}, fmt.Errorf("startNum 必须大于 0")
	}
	if request.Target < request.StartNum {
		return registerJobState{}, fmt.Errorf("target 必须不小于 startNum")
	}
	batch := request.BatchSize
	if batch <= 0 {
		// 请求没指定就听配置。这个任务是从界面上按一下就走一天的东西，「每批多少个」
		// 不该只能靠调接口时递参数。请求里显式给的值仍然优先 —— 那是明确意图。
		batch = registerJobConfiguredBatch(manager)
	}
	if batch > panelRegisterMax {
		batch = panelRegisterMax
	}
	// 长跑任务是无人值守的：按一下能跑几千个号。出口一个都没有时 runRegister 会不带代理
	// 启动 Chrome，那就是几千次从本机家庭宽带出口注册 —— 用户明确要求不能走直连。单次
	// 注册是人点出来的、只记一条提示，这里是自动的，所以在启动前就拒绝。
	//
	// phone 模式不查：它有默认出口地址（exitrotate.DefaultPhoneSOCKS），拿不到隧道会在
	// 第一批当场失败，不会静默直连。
	if mode == "proxy" && !registerJobHasProxyExit() {
		return registerJobState{}, fmt.Errorf("proxy 模式没有可用出口：代理池为空且未配置本地出口，长跑会一直用本机 IP 注册。先把代理池加载起来")
	}

	job := s.registerJob()
	job.mu.Lock()
	if job.state.Running {
		state := job.state
		job.mu.Unlock()
		return state, fmt.Errorf("已有批量注册任务在跑（%d -> %d），先停掉它",
			state.StartNum, state.Target)
	}
	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	job.state = registerJobState{
		Running:   true,
		Mode:      mode,
		StartNum:  request.StartNum,
		Target:    request.Target,
		BatchSize: batch,
		NextNum:   request.StartNum,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
		Detail:    "启动中",
	}
	state := job.state
	job.mu.Unlock()

	go s.runRegisterJob(ctx, job, manager, request, mode, batch)
	return state, nil
}

func (s *Server) runRegisterJob(ctx context.Context, job *registerJob, manager *nativePanelManager,
	request registerJobRequest, mode string, batch int) {
	defer func() {
		job.update(func(st *registerJobState) {
			st.Running = false
			st.FinishedAt = time.Now()
			if st.Detail == "" || st.Detail == "启动中" {
				st.Detail = "已结束"
			}
		})
	}()

	// 关日志要排在「把 Running 置回 false」之前。defer 是后进先出，所以这一句写在上面
	// 那个 defer 的后面：状态接口一看到 running=false 就可能立刻起下一个任务，而下一个
	// 任务的 openLog 会写同一个 j.logFile —— 顺序反了就会把它刚开好的句柄置空。
	job.openLog(manager)
	defer job.closeLog()

	job.note("任务启动：%s 模式 %d -> %d，每批 %d", mode, request.StartNum, request.Target, batch)
	num := request.StartNum
	deadBatches := 0
	// spentIP 跨批次传递：上一批最后一个号是从这个 IP 注册的，站点已经把它的当日额度记
	// 上了，下一批开头必须先换掉。
	spentIP := ""

	for num <= request.Target {
		if ctx.Err() != nil {
			job.note("收到停止请求，停在 %d", num)
			job.update(func(st *registerJobState) { st.Detail = "已停止" })
			return
		}
		count := batch
		if remain := request.Target - num + 1; remain < count {
			count = remain
		}

		// phone 模式先确认隧道：断了的话注册的报错完全看不出根因。
		if mode == "phone" {
			ip, err := s.ensurePhoneTunnel(ctx, manager, job)
			if err != nil {
				if ctx.Err() != nil {
					job.update(func(st *registerJobState) { st.Detail = "已停止" })
					return
				}
				job.note("手机隧道不可用：%v", err)
				job.update(func(st *registerJobState) { st.Detail = "等手机隧道恢复" })
				return
			}
			job.update(func(st *registerJobState) {
				st.LastIP = ip
				st.Detail = fmt.Sprintf("注册 %d-%d", num, num+count-1)
			})
		} else {
			job.update(func(st *registerJobState) {
				st.Detail = fmt.Sprintf("注册 %d-%d", num, num+count-1)
			})
		}

		started := time.Now()
		report, err := s.runRegister(ctx, manager, panelRegisterRequest{
			Mode: mode, Count: count, StartNum: num,
			// 上一批最后一个号用掉的出口 IP。站点是一 IP 一天一个号，不把它带进去，
			// 这一批的第一个号必然撞上已用额度、白烧一次 Turnstile 求解和一次提交，
			// 每个批次边界一次。
			SpentIP: spentIP,
		})
		if ctx.Err() != nil {
			job.note("停止请求在批次 %d-%d 中生效", num, num+count-1)
			job.update(func(st *registerJobState) { st.Detail = "已停止" })
			return
		}
		if err != nil {
			deadBatches++
			job.note("批次 %d-%d 出错：%v", num, num+count-1, err)
			if deadBatches >= registerJobMaxDeadBatches {
				job.note("连续 %d 批失败，停下等人看", deadBatches)
				job.update(func(st *registerJobState) { st.Detail = "连续失败已停止" })
				return
			}
			if wErr := waitCtx(ctx, 30*time.Second); wErr != nil {
				job.update(func(st *registerJobState) { st.Detail = "已停止" })
				return
			}
			continue
		}

		var skipped int
		for _, account := range report.Accounts {
			if account.Status == "skipped" {
				skipped++
			}
		}
		job.update(func(st *registerJobState) {
			st.Success += report.Success
			st.Failed += report.Failed
			st.Skipped += skipped
			st.Batches++
		})
		job.note("批次 %d-%d：成功 %d 失败 %d 跳过 %d 用时 %ds",
			num, num+count-1, report.Success, report.Failed, skipped, int(time.Since(started).Seconds()))
		for _, account := range report.Accounts {
			if account.Status == "failed" {
				job.note("  %d 失败：%s", account.Num, account.Detail)
			}
		}
		for _, n := range report.Notes {
			job.note("  %s", n)
		}

		// 整批一个都没成，且不是「全被跳过」，通常是链路层面的问题。
		if report.Success == 0 && skipped < count {
			deadBatches++
			if deadBatches >= registerJobMaxDeadBatches {
				job.note("连续 %d 批一个都没成，停下等人看", deadBatches)
				job.update(func(st *registerJobState) { st.Detail = "连续失败已停止" })
				return
			}
		} else {
			deadBatches = 0
		}

		if !request.SkipOAuth && report.Success > 0 {
			s.backfillOAuth(ctx, manager, job, num, num+count-1)
			if ctx.Err() != nil {
				job.update(func(st *registerJobState) { st.Detail = "已停止" })
				return
			}
		}

		// 推进用 report.NextNum，不用 num += count。两者在整批跑完时相等，但换出口失败
		// 时 runRegister 会中途停下并把 NextNum 指到第一个「没尝试过」的号 —— 早先这里
		// 无条件加 count，一次 adb 抽风就跳过一整批号，事后只能靠对账翻出来。
		spentIP = report.LastIP
		if report.NextNum > num {
			num = report.NextNum
		} else {
			// 防御性兜底：NextNum 没被填（老报文）或没前进时仍然推进一批，否则就是死循环。
			num += count
		}
		job.update(func(st *registerJobState) { st.NextNum = num })
		if report.Stopped {
			job.note("本批中途停下，下一个未尝试的号是 %d", num)
		}
	}

	job.note("跑完：到 %d", request.Target)
	job.update(func(st *registerJobState) { st.Detail = "已完成" })
}

// ensureTunnel 在测试里可替换：真实实现要执行 adb，而测试机上不一定插着手机。
// 同 rotateExit 的做法。
var ensureTunnel = exitrotate.EnsureTunnel

// ensurePhoneTunnel 在预算内反复尝试把隧道修好，返回实测到的出口 IP。
func (s *Server) ensurePhoneTunnel(ctx context.Context, manager *nativePanelManager, job *registerJob) (string, error) {
	_, cfg, err := manager.panelData()
	if err != nil {
		return "", err
	}
	req := exitrotate.TunnelRequest{
		ADB:   cfg.Register.ADB,
		SOCKS: cfg.Register.PhoneSOCKS,
		// Binary 一直是空的，于是 EnsureTunnel 只能用它自己写死的
		// /data/local/tmp/phone-socks。手机上放在别处（有的机型会清那个目录）时，
		// 每一批都以「手机上没有可执行的 …」失败，而配置里改不了。
		Binary: cfg.Register.PhoneSOCKSBin,
	}
	var last error
	for attempt := 0; attempt < registerJobMaxTunnelWait; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		result, err := ensureTunnel(ctx, req)
		if err == nil {
			if result.Forward || result.Process {
				// 修好了也要说：隧道反复需要修复本身就是个信号。
				job.note("手机隧道已修复（forward=%v process=%v），出口 %s",
					result.Forward, result.Process, result.IP)
			}
			return result.IP, nil
		}
		last = err
		if attempt == 0 {
			job.note("手机隧道不通，重试中：%v", err)
			job.update(func(st *registerJobState) { st.Detail = "等手机隧道恢复" })
		}
		if wErr := waitCtx(ctx, registerJobTunnelInterval); wErr != nil {
			return "", wErr
		}
	}
	if last == nil {
		last = fmt.Errorf("手机隧道不可用")
	}
	return "", last
}

// backfillOAuth 给刚注册的这一段补 token。
//
// resume=true 只处理还没有 token 的号：注册时内联那一次可能因为池子里的代理超时而只拿到
// 设备码，不补就会攒下一堆注册成功但用不了的账号。
func (s *Server) backfillOAuth(ctx context.Context, manager *nativePanelManager, job *registerJob, from, to int) {
	report, err := s.runOAuthBatch(ctx, manager, panelOAuthBatchRequest{
		StartNum: from, EndNum: to, Resume: true,
	})
	if err != nil {
		if ctx.Err() == nil {
			job.note("  oauth 补齐失败：%v", err)
		}
		return
	}
	job.update(func(st *registerJobState) { st.Imported += report.Imported })
	job.note("  oauth：imported %d pending %d skipped %d failed %d",
		report.Imported, report.Pending, report.Skipped, report.Failed)
}

func waitCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

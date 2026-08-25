package web

// 实时 TPM 与并发。
//
// 「请求明细」栏顶部要置顶当前 TPM 和并发，这两个都必须是「此刻」的值：
//   - TPM 取尾随 60 秒窗口内的真实 token 速率，而不是把生涯累计值除以运行时长
//     伪装成瞬时速率。窗口外的记录一律不计入。
//   - 并发复用已有的在途计数（chatSlotStats / accountConcurrency.Snapshot），
//     不新增第二个互相矛盾的计数器。

import (
	"net/http"
	"time"
)

// liveRateWindow 是 TPM 的统计窗口。名字里的 "per minute" 就是它。
const liveRateWindow = time.Minute

// liveTokenRate 汇总尾随窗口内的 token 与请求数。
type liveTokenRate struct {
	WindowSeconds int   `json:"windowSeconds"`
	Tokens        int64 `json:"tokens"`
	Requests      int64 `json:"requests"`
	// TPM 是按窗口归一化到每分钟的 token 数。窗口本身就是 1 分钟时等于 Tokens。
	TPM int64 `json:"tpm"`
	// RPM 同理，便于前端一起展示。
	RPM int64 `json:"rpm"`
}

// tokenRateWithin 计算截至 now 的尾随窗口速率。分离出纯函数便于测试。
func (s *usageLog) tokenRateWithin(now time.Time, window time.Duration) liveTokenRate {
	if window <= 0 {
		window = liveRateWindow
	}
	cutoff := now.Add(-window)
	out := liveTokenRate{WindowSeconds: int(window.Seconds())}
	s.mu.Lock()
	// 记录按时间追加，因此从尾部反向扫描，一旦越过窗口即可停止。
	for i := len(s.records) - 1; i >= 0; i-- {
		rec := s.records[i]
		if !rec.Time.After(cutoff) {
			break
		}
		out.Tokens += rec.InputTokens + rec.OutputTokens + rec.CacheTokens
		out.Requests++
	}
	s.mu.Unlock()
	scale := float64(time.Minute) / float64(window)
	out.TPM = int64(float64(out.Tokens) * scale)
	out.RPM = int64(float64(out.Requests) * scale)
	return out
}

// handleLiveMetrics 提供 GET /api/admin/live-metrics。
// 载荷刻意保持小，便于前端每几秒轮询一次。
func (s *Server) handleLiveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	rate := globalUsage.tokenRateWithin(time.Now(), liveRateWindow)
	chat := chatSlotStats()
	payload := map[string]any{
		"rate": rate,
		"tpm":  rate.TPM,
		"rpm":  rate.RPM,
		// concurrency.active 是全局在途聊天数，limit 是配置上限。
		"concurrency": chat,
		"inflight":    len(inflightSnapshot()),
	}
	if s != nil && s.accountConcurrency != nil {
		payload["accounts"] = s.accountConcurrency.Snapshot()
	}
	jsonOut(w, payload)
}

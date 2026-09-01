package turnstile

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// 出口在 Turnstile 这一关的近期表现，只保存在内存里。
//
// 为什么需要它：注册循环每个号都从池子的同一个顺序开头挑出口，于是每个号都要把同一批
// 过不了 CF 的出口重新踩一遍。实测三轮注册里，第一个候选每次都是同一个出口，六次重试
// 有一半以上花在前几轮已经确认不行的出口上。
//
// 这里只做「排序」，绝不删除、不改写、不探测用户的代理列表：候选集合始终是调用方给的那
// 一份，本模块只决定先试谁。记录带 TTL，因为出口的 CF 风险评级会变 —— 过一段时间它值得
// 再试一次，不该被永久打入冷宫。
//
// 全部在内存中：进程重启后忘记一切，这是刻意的。它是一份「本轮经验」，不是要去维护一份
// 与用户列表平行的状态。
const (
	exitMemoryTTL      = 30 * time.Minute
	exitMemoryMaxItems = 512
	// exitQuotaTTL 对齐站点的 ipRules 窗口（windowSeconds: 86400，每 IP 每天 1 次）。
	//
	// 用掉配额的出口和「过不了 CF」的出口性质不同：它其实是好出口，只是今天不能再注册
	// 了。所以它既不该排在最前面（会把重试额度浪费在必然失败的注册上），也不该只压 30
	// 分钟就放回来 —— 那样一整批号会反复撞同一批已用完的出口。
	exitQuotaTTL = 24 * time.Hour
)

type exitOutcome struct {
	okAt        time.Time
	failedAt    time.Time
	quotaUsedAt time.Time
}

var exitMemory struct {
	sync.Mutex
	byExit map[string]*exitOutcome
}

func exitMemoryEntry(raw string) *exitOutcome {
	key := strings.TrimSpace(raw)
	if key == "" {
		return nil
	}
	if exitMemory.byExit == nil {
		exitMemory.byExit = make(map[string]*exitOutcome)
	}
	entry := exitMemory.byExit[key]
	if entry == nil {
		// 简单的容量保护：条目数上限到了就把过期的清掉，清不出来就不再记新的。
		// 宁可少记，也不要让这份「经验」无界增长。
		if len(exitMemory.byExit) >= exitMemoryMaxItems {
			pruneExitMemoryLocked()
		}
		if len(exitMemory.byExit) >= exitMemoryMaxItems {
			return nil
		}
		entry = &exitOutcome{}
		exitMemory.byExit[key] = entry
	}
	return entry
}

func pruneExitMemoryLocked() {
	cutoff := time.Now().Add(-exitMemoryTTL)
	quotaCutoff := time.Now().Add(-exitQuotaTTL)
	for k, v := range exitMemory.byExit {
		if v.okAt.Before(cutoff) && v.failedAt.Before(cutoff) && v.quotaUsedAt.Before(quotaCutoff) {
			delete(exitMemory.byExit, k)
		}
	}
}

// NoteExitQuotaUsed 记下这个出口今天的注册配额已经用掉了。
//
// 成功注册之后要调这个：出口本身是好的（它刚过了 CF），但站点的 ipRules 是每 IP 每天
// 1 次，再拿它注册必然回「已达上限」。不记的话，PreferProvenExits 会把它当成「刚验证过
// 的好出口」排在最前面，于是整批号都会先撞一次必然失败的注册。
func NoteExitQuotaUsed(raw string) {
	exitMemory.Lock()
	defer exitMemory.Unlock()
	if entry := exitMemoryEntry(raw); entry != nil {
		entry.quotaUsedAt = time.Now()
	}
}

// NoteExitTurnstileOK 记下这个出口刚刚过了 Turnstile。
func NoteExitTurnstileOK(raw string) {
	exitMemory.Lock()
	defer exitMemory.Unlock()
	if entry := exitMemoryEntry(raw); entry != nil {
		entry.okAt = time.Now()
		entry.failedAt = time.Time{}
	}
}

// NoteExitTurnstileFailed 记下这个出口刚刚在 Turnstile 这一关失败。
func NoteExitTurnstileFailed(raw string) {
	exitMemory.Lock()
	defer exitMemory.Unlock()
	if entry := exitMemoryEntry(raw); entry != nil {
		entry.failedAt = time.Now()
		entry.okAt = time.Time{}
	}
}

// exitSchemeRank 是同一档内的次级排序键：http/https 先于 socks5。
//
// 依据是实测，不是偏好。三轮注册里 socks5 出口试了 8 次，一次都没能走到站点回话；http
// 出口试了 6 次，有 2 次走到了。另一次八个 http 出口的普查里有 3 个拿到了 token。失败形态
// 集中在 challenges.cloudflare.com 加载不出来 —— 也就是 CF 那侧的连接，socks5 出口在这
// 一段上明显更差。
//
// 这只影响先试谁：socks5 出口仍然全部保留在候选里，前面的都失败时照样会试到它们。
func exitSchemeRank(raw string) int {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "socks") {
		return 1
	}
	return 0
}

// exitRank 越小越先试：
//
//	0 = 近期过过 CF 且今天还有配额（最该先用）
//	1 = 没记录
//	2 = 近期在 CF 这一关失败过
//	3 = 今天的注册配额已经用掉（好出口，但今天再试必然回「已达上限」）
//
// 配额用尽排在最后而不是直接排除：万一整个池子都用完了，它仍然要出现在候选里，让调用方
// 得到站点的真实回复，而不是「没有可用出口」这种自己编的结论。
func exitRank(raw string) int {
	exitMemory.Lock()
	defer exitMemory.Unlock()
	entry := exitMemory.byExit[strings.TrimSpace(raw)]
	if entry == nil {
		return 1
	}
	now := time.Now()
	if entry.quotaUsedAt.After(now.Add(-exitQuotaTTL)) {
		return 3
	}
	cutoff := now.Add(-exitMemoryTTL)
	if entry.okAt.After(cutoff) {
		return 0
	}
	if entry.failedAt.After(cutoff) {
		return 2
	}
	return 1
}

// PreferProvenExits 把候选按近期表现重排：过过 CF 的排前面，近期失败的排后面。
//
// 只重排，不增不删：返回的元素与入参完全相同，长度也相同。同一档内保持原有顺序，
// 这样在没有任何记录时（比如刚启动）行为和原来一模一样。
func PreferProvenExits(exits []string) []string {
	if len(exits) < 2 {
		return exits
	}
	out := make([]string, len(exits))
	copy(out, exits)
	ranks := make(map[string]int, len(out))
	schemes := make(map[string]int, len(out))
	for _, e := range out {
		if _, seen := ranks[e]; !seen {
			ranks[e] = exitRank(e)
			schemes[e] = exitSchemeRank(e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ranks[out[i]] != ranks[out[j]] {
			return ranks[out[i]] < ranks[out[j]]
		}
		return schemes[out[i]] < schemes[out[j]]
	})
	return out
}

// ResetExitMemory 清空记录，供测试使用。
func ResetExitMemory() {
	exitMemory.Lock()
	defer exitMemory.Unlock()
	exitMemory.byExit = nil
}

package web

// 邮箱编号区间（startNum / endNum）是账号注册与批量 OAuth 的对外判据。
//
// 背景：底层 Python 脚本对 --start 的语义并不一致，这是用户踩到的坑。
//
//	Register/register_m365_phone.py:320   base = a.start or START_NUM  → --start 是绝对邮箱编号
//	Register/register_m365_clash.py:216   base = a.start or START_NUM  → --start 是绝对邮箱编号
//	Register/register_accounts.py:594     accounts[a["index"] >= start] → --start 是 1-based 序号
//	Register/register_accounts_concurrent.py:161                        → --start 是 1-based 序号
//	oauth/oauth_batch_concurrent.py:419   all_emails[args.start-1:]     → --start 是 1-based 序号
//
// 用户请求 482 却启动了 24s055526，正是因为 482 被当成了列表下标而不是邮箱
// 编号。本文件把「邮箱编号」这一个用户可理解的量，翻译成每个脚本各自需要的
// 参数，翻译规则集中在此，不再散落成一堆特例。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// nativePanelEmailRangeMaxCount 与既有 count/limit 上限一致，防止一次误操作
// 拉起成百上千个账号 —— 用户已经被一次意外启动 270 个账号的任务坑过。
const nativePanelEmailRangeMaxCount = 1000

// nativePanelEmailNum 从 24s055026@office.bo.edu.kg 这样的地址里取出编号
// 5026。prefix 为空时退化成「取本地部分末尾的连续数字」，这样即使配置里的
// email_prefix 与历史数据不完全一致也能解析。
func nativePanelEmailNum(email, prefix string) (int, bool) {
	local := strings.TrimSpace(email)
	if at := strings.IndexByte(local, '@'); at > 0 {
		local = local[:at]
	}
	if local == "" {
		return 0, false
	}
	if prefix != "" && strings.HasPrefix(local, prefix) {
		if n, err := strconv.Atoi(local[len(prefix):]); err == nil && n > 0 {
			return n, true
		}
	}
	end := len(local)
	for end > 0 && local[end-1] >= '0' && local[end-1] <= '9' {
		end--
	}
	if end == len(local) {
		return 0, false
	}
	n, err := strconv.Atoi(local[end:])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// nativePanelEmailRange 是一段闭区间 [Start, End] 的邮箱编号。
type nativePanelEmailRange struct {
	Start int
	End   int
}

// count 是区间覆盖的编号个数。
func (r nativePanelEmailRange) count() int { return r.End - r.Start + 1 }

// resolveEmailRange 校验并归一化用户提交的编号区间。endNum 省略（<=0）时按
// count 推导出结束编号；两者都缺失则返回 ok=false，调用方回落到旧的
// start/limit 语义，保证既有调用方行为逐字节不变。
func resolveEmailRange(startNum, endNum, count int) (nativePanelEmailRange, bool, error) {
	if startNum <= 0 && endNum <= 0 {
		return nativePanelEmailRange{}, false, nil
	}
	if startNum <= 0 {
		return nativePanelEmailRange{}, true, errors.New("邮箱编号区间无效：缺少起始编号 startNum")
	}
	r := nativePanelEmailRange{Start: startNum, End: endNum}
	if r.End <= 0 {
		n := count
		if n <= 0 {
			n = 1
		}
		r.End = r.Start + n - 1
	}
	if r.End < r.Start {
		return nativePanelEmailRange{}, true, fmt.Errorf("邮箱编号区间无效：结束编号 %d 小于起始编号 %d", r.End, r.Start)
	}
	if r.count() > nativePanelEmailRangeMaxCount {
		return nativePanelEmailRange{}, true, fmt.Errorf("邮箱编号区间过大：%d-%d 共 %d 个，单次上限 %d 个",
			r.Start, r.End, r.count(), nativePanelEmailRangeMaxCount)
	}
	return r, true, nil
}

// registerArgsForRange 把编号区间翻译成注册脚本的参数。
//
// absoluteStart=true 对应 phone/clash：--start 直接就是邮箱编号。
// absoluteStart=false 对应 register_accounts(.py|_concurrent.py)：--start 是
// 1-based 序号，需要用配置里的 email_start_num 作为基准换算。基准之前的编号
// 无法用序号表达，此时明确报错而不是静默注册错误的账号。
func registerArgsForRange(r nativePanelEmailRange, base int, absoluteStart bool) (start, limit int, err error) {
	if absoluteStart {
		return r.Start, r.count(), nil
	}
	if base <= 0 {
		base = 1000
	}
	if r.Start < base {
		return 0, 0, fmt.Errorf("邮箱编号 %d 小于配置基准 email_start_num=%d，该模式无法定位；请改用起始编号 >= %d",
			r.Start, base, base)
	}
	return r.Start - base + 1, r.count(), nil
}

// selectEmailsInRange 按编号区间从账密清单里挑出邮箱，并返回它们在整个已排序
// 清单中的 1-based 起始下标 —— oauth_batch*.py 的 --start 就是这个下标。
//
// 返回的 position 是「排序后清单中的位置」，与脚本自身 sorted() 的顺序一致。
func selectEmailsInRange(credentials map[string]string, prefix string, r nativePanelEmailRange) (matched []string, position int, err error) {
	if len(credentials) == 0 {
		return nil, 0, errors.New("账密清单为空，无法按邮箱编号定位")
	}
	all := make([]string, 0, len(credentials))
	for email := range credentials {
		all = append(all, email)
	}
	sort.Strings(all)
	position = 0
	for i, email := range all {
		num, ok := nativePanelEmailNum(email, prefix)
		if !ok || num < r.Start || num > r.End {
			continue
		}
		if position == 0 {
			position = i + 1
		}
		matched = append(matched, email)
	}
	if len(matched) == 0 {
		return nil, 0, fmt.Errorf("账密清单中没有编号在 %d-%d 之间的账号", r.Start, r.End)
	}
	return matched, position, nil
}

// credentialEmailNumBounds 报告账密清单里实际存在的最小/最大邮箱编号，供前端
// 预填与校验区间，避免用户凭空猜一个不存在的号段。
func credentialEmailNumBounds(credentials map[string]string, prefix string) (min, max int, ok bool) {
	for email := range credentials {
		num, parsed := nativePanelEmailNum(email, prefix)
		if !parsed {
			continue
		}
		if !ok || num < min {
			min = num
		}
		if !ok || num > max {
			max = num
		}
		ok = true
	}
	return min, max, ok
}

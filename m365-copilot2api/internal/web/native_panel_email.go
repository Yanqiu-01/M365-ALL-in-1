package web

// 邮箱编号解析。账号命名规则是 <prefix><num>@<domain>，面板状态据此报告账密
// 清单实际覆盖的编号区间。
//
// 这里原本还负责把「邮箱编号区间」翻译成各个 Python 注册/批量 OAuth 脚本各自
// 的 --start 语义（phone/clash 收绝对编号，register_accounts 系列收 1-based
// 序号）—— 那是用户遇到「请求 482 却启动了 24s055526」的根因。相关脚本与编排
// 已经移除，翻译逻辑随之删除，只留下与展示有关的两个函数。

import (
	"strconv"
	"strings"
)

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

// credentialEmailNumBounds 报告账密清单里实际存在的最小/最大邮箱编号，供界面
// 显示号段范围。
func credentialEmailNumBounds(credentials map[string]string, prefix string) (low, high int, ok bool) {
	for email := range credentials {
		num, parsed := nativePanelEmailNum(email, prefix)
		if !parsed {
			continue
		}
		if !ok || num < low {
			low = num
		}
		if !ok || num > high {
			high = num
		}
		ok = true
	}
	return low, high, ok
}

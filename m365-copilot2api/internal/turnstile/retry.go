package turnstile

import "strings"

// ExitAttributable 判断一次注册失败该不该换个出口重试。
//
// 为什么需要这个判断：注册循环原先每个号只试一个出口，失败即判死。而实测下来，池子里
// 绝大多数 live 出口都过不了这一关 —— 有的页面根本加载不出来（代理隧道建不起来），有的
// 页面正常但 Cloudflare 认定该 IP 高风险、一直不发 token（实测 http://113.45.195.147:3128
// 就是后者：表单 1.97s 就绪，控件也铺开了，点 3 次仍然没有 token）。也就是说「一次抽签
// 抽中坏出口」= 这个号永久失败，这正是用户反复看到的注册失败。
//
// 但也不能无脑重试：站点每个 IP 每天只允许成功注册 1 次，把出口当柴烧掉是有代价的；
// 而且配置类错误（要邮箱验证、要邀请码）换一百个出口也还是同样的错。所以只对「换个出口
// 就可能好」的错误重试，其余立刻返回。
//
// 拿不准的一律当作不可重试：宁可少试一次、把真实错误如实报出来，也不要拿 96 个出口去撞
// 一个其实是站点结构变了的问题。
func ExitAttributable(err error) bool {
	if err == nil {
		return false
	}
	return exitAttributableText(err.Error())
}

func exitAttributableText(msg string) bool {
	if strings.TrimSpace(msg) == "" {
		return false
	}
	lower := strings.ToLower(msg)
	// 明确不该换出口的：换了也是同样的错，只会白烧额度。这一组要先判，
	// 因为下面的关键词可能同时出现在这些文案里。
	for _, keep := range []string{
		// 表单已经提交出去了，只是没等到站点的结果。这一条绝对不能重试：站点很可能已经
		// 把号建出来了（页面停在「正在创建 Office 账号」），再提交一次就是第二个号，还要
		// 多烧一个出口的当日额度。宁可让调用方去站点侧确认，也不要盲目再来一遍。
		"提交后在预算内没有拿到结果",
		"注册表单不存在，无法提交",
		"站点结构可能变了",   // 页面没有 Turnstile 容器
		"需要邮箱验证",     // 配置问题
		"需要邀请码",      // 配置问题
		"缺少用户名或密码",   // 调用方问题
		"缺少注册页地址",    // 调用方问题
		"本机没有可用的浏览器", // 环境问题
		"headless",   // 模式问题，换出口无效
	} {
		if strings.Contains(msg, keep) || strings.Contains(lower, keep) {
			return false
		}
	}
	for _, retry := range []string{
		"注册表单没有出现",   // 页面没加载出来：隧道建不起来或被站点拦下
		"高风险",        // Cloudflare 判定该出口 IP 高风险
		"没能通过当前出口加载", // challenges.cloudflare.com 起不来
		"换一个出口",      // 求解器自己就在建议换出口
		"求解器用不了",     // 出口类型后端不支持
	} {
		if strings.Contains(msg, retry) {
			return true
		}
	}
	// Chrome/代理层的网络错误。net::ERR_* 里除了 NAME_NOT_RESOLVED 之外基本都是
	// 出口侧的问题（隧道、超时、连接被拒）。
	for _, retry := range []string{
		"err_proxy_connection_failed",
		"err_tunnel_connection_failed",
		"err_connection_timed_out",
		"err_connection_reset",
		"err_connection_closed",
		"err_connection_refused",
		"err_empty_response",
		"err_socks_connection_failed",
		"err_timed_out",
		"proxy",
	} {
		if strings.Contains(lower, retry) {
			return true
		}
	}
	// 站点侧的 IP 限流：ipRules 是「每个 IP 每天 1 次」，撞上了就该换 IP 再来。
	// 只在同时出现 ip/频繁/太快 这类信号时才认，避免把「用户名已被注册」误判成
	// 可以换出口解决 —— 那个换出口没有用。
	if strings.Contains(lower, "ip") || strings.Contains(msg, "频繁") || strings.Contains(msg, "太快") {
		for _, limit := range []string{"限制", "已注册", "超出", "过于", "频繁", "太快", "limit", "rate", "too many", "quota"} {
			if strings.Contains(msg, limit) || strings.Contains(lower, limit) {
				return true
			}
		}
	}
	return false
}

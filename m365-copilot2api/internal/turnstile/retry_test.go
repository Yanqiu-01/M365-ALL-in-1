package turnstile

import (
	"errors"
	"testing"
)

func TestExitAttributableRetriesOnExitSymptoms(t *testing.T) {
	// 这些都是实测见过的、换个出口就可能好的失败。
	retry := []string{
		"注册表单没有出现（页面可能被站点拦下或 /api/public/site-config 取不到）",
		"Turnstile 一直没给出 token（点击 3 次无效）：该出口 IP 可能被 Cloudflare 判为高风险，换一个出口再试",
		"challenges.cloudflare.com 的脚本没能通过当前出口加载，Turnstile 控件起不来：换一个出口再试",
		"navigate: net::ERR_PROXY_CONNECTION_FAILED",
		"net::ERR_TUNNEL_CONNECTION_FAILED",
		"Error solving the challenge. net::ERR_CONNECTION_TIMED_OUT",
		"该 IP 今日已注册过，请明天再试",
		"请求过于频繁，请稍后再试",
		// 实测批量注册里真实出现过的站点文案。这四条原先全都没匹配上，于是
		// 5834/5840/5847/5853 一次都没换出口就判死了 —— 而它们恰恰是换个出口
		// 就可能成功的：每 IP 每天 1 次、geoip 只放行 CN/HK/MO/TW。
		"当前 IP 在 1 天 内注册次数已达上限",
		"当前地区暂不支持注册",
		"站点没有下发 Turnstile sitekey（/api/public/site-config 取不到或该出口被拒），控件未加载",
	}
	for _, msg := range retry {
		if !ExitAttributable(errors.New(msg)) {
			t.Errorf("ExitAttributable(%q) = false, want true", msg)
		}
	}
}

func TestExitAttributableKeepsConfigAndStructureFailures(t *testing.T) {
	// 换出口也是同样的错，重试只会白烧「每个 IP 每天 1 次」的额度。
	keep := []string{
		"注册页没有 Turnstile 容器，站点结构可能变了",
		"站点需要邮箱验证，当前流程没有收件箱可用",
		"站点需要邀请码，配置里没有填",
		"缺少用户名或密码，无法填表",
		"缺少注册页地址",
		"本机没有可用的浏览器",
		"Turnstile 一直没给出 token（点击 3 次无效）：当前是 headless 模式，实测这种模式下 Cloudflare 会一直拖着不发 token。去掉 M365_CHROME_HEADLESS 用有头模式",
		"用户名已被注册",
		"该邮箱地址已被注册，请更换用户名",
		"密码不符合要求",
		"站点返回失败但没有给出原因",
	}
	for _, msg := range keep {
		if ExitAttributable(errors.New(msg)) {
			t.Errorf("ExitAttributable(%q) = true, want false", msg)
		}
	}
}

// 提交之后的失败绝对不能换出口重试。
//
// 这不是效率问题而是正确性问题：页面停在「正在创建 Office 账号」时站点很可能已经把号
// 建出来了，再提交一次就是第二个号，本地却只会记下一条 —— 于是又多一个孤儿号，还白烧
// 一个出口的当日额度。编号 5831 就是这么变成孤儿的。
func TestExitAttributableNeverRetriesAfterSubmit(t *testing.T) {
	afterSubmit := []string{
		"提交后在预算内没有拿到结果，页面停在：正在创建 Office 账号，请等待成功响应后再进行下一步操作。",
		"提交后在预算内没有拿到站点结果",
		"注册表单不存在，无法提交",
	}
	for _, msg := range afterSubmit {
		if ExitAttributable(errors.New(msg)) {
			t.Errorf("提交后的失败被当成了可重试：%q —— 重试会造成重复注册", msg)
		}
	}
}

func TestExitAttributableNilAndEmpty(t *testing.T) {
	if ExitAttributable(nil) {
		t.Error("ExitAttributable(nil) = true, want false")
	}
	if ExitAttributable(errors.New("   ")) {
		t.Error("ExitAttributable(blank) = true, want false")
	}
}

// headless 那条要点名：它同时含有「一直没给出 token」和 headless 两个信号，
// 必须走「不重试」那一支，否则会拿一批出口去撞一个模式问题。
func TestExitAttributablePrefersHeadlessDiagnosisOverExitRetry(t *testing.T) {
	msg := "Turnstile 一直没给出 token（点击 3 次无效）：当前是 headless 模式，实测这种模式下 Cloudflare 会一直拖着不发 token"
	if ExitAttributable(errors.New(msg)) {
		t.Error("headless 诊断被当成了可换出口重试的错误")
	}
}

// 配额用尽要能被单独认出来：它和「过不了 CF」都要换出口，但记录方式不同。
func TestExitQuotaExhaustedRecognisesSiteWording(t *testing.T) {
	quota := []string{
		"当前 IP 在 1 天 内注册次数已达上限",
		"该 IP 今日已注册过，请明天再试",
		"IP rate limit exceeded",
	}
	for _, msg := range quota {
		if !ExitQuotaExhausted(errors.New(msg)) {
			t.Errorf("ExitQuotaExhausted(%q) = false, want true", msg)
		}
		// 配额用尽同时也必须是「可换出口重试」的。
		if !ExitAttributable(errors.New(msg)) {
			t.Errorf("ExitAttributable(%q) = false，配额用尽应当换出口重试", msg)
		}
	}
	notQuota := []string{
		"当前地区暂不支持注册",
		"该邮箱地址已被注册，请更换用户名",
		"提交后在预算内没有拿到结果",
		"",
	}
	for _, msg := range notQuota {
		if ExitQuotaExhausted(errors.New(msg)) {
			t.Errorf("ExitQuotaExhausted(%q) = true, want false", msg)
		}
	}
	if ExitQuotaExhausted(nil) {
		t.Error("ExitQuotaExhausted(nil) = true, want false")
	}
}

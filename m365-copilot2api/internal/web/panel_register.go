package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"m365-copilot2api/internal/exitrotate"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/phonecdp"
	"m365-copilot2api/internal/turnstile"
)

// panelRegisterRequest 是一次 Go 内置注册。Turnstile token 优先由本机
// FlareSolverr 从注册页取出；调用方仍可手动提供 token 作为回退。
// 每个号都先联网过 Cloudflare、提交 /api/register 并写入本地账密，
// 确认成功后再换出口 IP，给下一个号用。
type panelRegisterRequest struct {
	Mode           string `json:"mode"`
	Count          int    `json:"count"`
	StartNum       int    `json:"startNum"`
	EndNum         int    `json:"endNum"`
	TurnstileToken string `json:"turnstileToken"`
	Node           string `json:"node"`
	Proxy          string `json:"proxy"`
	// SpentIP 是调用方已知「当日注册额度已用掉」的出口地址。
	//
	// 为什么需要它：批次末尾那一号成功之后不换出口（见循环里的 i < count-1），于是
	// 那个已经用掉额度的 IP 会留在手机上。下一批的第一个号照常探测、照常花十几秒解一
	// 次 Turnstile、照常 POST，然后必然收到「当前 IP 在 1 天内注册次数已达上限」，靠
	// 重试才换出口。实测 5921、5941、5961 三个批次首号全部是「换了 2 个出口才成功」，
	// 而批次内其他号极少这样 —— 每个批次边界白烧一次求解，还白占一次重试额度。
	//
	// 调用方把上一批的 report.LastIP 原样传回来即可。只在探测到的地址与它相同时才提
	// 前换出口：地址自己变了就什么都不做，不为此多切一次飞行模式。
	SpentIP string `json:"spentIp"`
}

type panelRegisterAccount struct {
	Num              int    `json:"num"`
	Email            string `json:"email"`
	Status           string `json:"status"` // success | failed | skipped
	IP               string `json:"ip,omitempty"`
	Detail           string `json:"detail,omitempty"`
	OAuthStatus      string `json:"oauthStatus,omitempty"`
	OAuthDetail      string `json:"oauthDetail,omitempty"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
}

type panelRegisterReport struct {
	OK       bool                   `json:"ok"`
	Mode     string                 `json:"mode"`
	Total    int                    `json:"total"`
	Success  int                    `json:"success"`
	Failed   int                    `json:"failed"`
	Accounts []panelRegisterAccount `json:"accounts"`
	Rotate   *exitrotate.Result     `json:"rotate,omitempty"`
	// LastIP 是最后一次成功注册所用的出口地址，调用方下一批应当作为 SpentIP 传回来。
	// 只有 phone 模式填得有意义：其余模式的出口是代理地址，不是运营商下发的 IP。
	LastIP string `json:"lastIp,omitempty"`
	// NextNum 是「本次没有尝试到的第一个编号」。正常跑完时它等于 start+count；
	// 提前中断时它指向真正的断点。
	//
	// 为什么必须有：原先中断时会给剩下每个号伪造一条 failed 记录，而调用方看到
	// err == nil 就按 num += count 推进 —— 那些号一次都没试过，却被永久跳过了。实测
	// 形态是 adb 抖一下（USB 掉一下、adb server 重启、手机重启），换 IP 报错，于是
	// 一批 20 个号里 19 个被判死，号段照样往前走。
	NextNum int `json:"nextNum,omitempty"`
	// Stopped 表示本次是提前中断的，NextNum 之后的号一次都没尝试过。
	Stopped bool `json:"stopped,omitempty"`
	// Notes 记录「做完了但没完全达到预期」的情况，例如代理池只有一个条目、整批
	// 账号只能复用同一出口。这类事不该只是静默通过，也不该算成失败。
	Notes []string `json:"notes,omitempty"`
}

const panelRegisterMax = 20

func (cfg nativePanelFileConfig) registerReady() bool {
	for _, value := range []string{cfg.Register.SiteURL, cfg.Register.EmailDomain, cfg.Register.EmailPrefix, cfg.Register.Password} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func (s *Server) runRegister(ctx context.Context, manager *nativePanelManager, request panelRegisterRequest) (panelRegisterReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = "proxy"
	}
	if mode != "phone" && mode != "clash" && mode != "proxy" {
		return panelRegisterReport{}, errors.New("未知注册模式，支持 phone / clash / proxy")
	}
	report := panelRegisterReport{Mode: mode, Accounts: []panelRegisterAccount{}}
	if manager == nil {
		return report, errors.New("本地面板不可用")
	}
	paths, cfg, err := manager.panelData()
	if err != nil {
		return report, err
	}
	if !cfg.registerReady() {
		return report, errors.New("注册配置不完整：需要 site_url、email_domain、email_prefix、password")
	}
	// clash 模式的前置条件要在这里一次性说清楚，而不是让 rotateClash 在第二个
	// 账号那里抛「clash api, group and node are required」。那条消息出现在
	// 「上一号已写入本地，但换 IP 失败」里，既不说缺什么，也不说去哪里填。
	if mode == "clash" {
		var missing []string
		if strings.TrimSpace(cfg.Register.ClashAPI) == "" {
			missing = append(missing, "clash_api")
		}
		if strings.TrimSpace(cfg.Register.ClashGroup) == "" {
			missing = append(missing, "clash_group")
		}
		if strings.TrimSpace(request.Node) == "" && len(clashNodes(cfg)) == 0 {
			missing = append(missing, "clash_nodes")
		}
		if len(missing) > 0 {
			return report, fmt.Errorf("clash 模式配置不完整，缺少 %s：请在面板「注册配置」里填写 Clash 外部控制地址、策略组与节点清单（POST /api/admin/panel/config 的 clashApi / clashGroup / clashNodes）", strings.Join(missing, "、"))
		}
	}
	start := request.StartNum
	if start <= 0 {
		start = cfg.Register.EmailStartNum
	}
	if start <= 0 {
		start = 1000
	}
	count := request.Count
	if request.EndNum > 0 {
		if request.EndNum < start {
			return report, fmt.Errorf("结束编号 %d 小于起始编号 %d", request.EndNum, start)
		}
		count = request.EndNum - start + 1
	}
	if count <= 0 {
		count = 1
	}
	if count > panelRegisterMax {
		return report, fmt.Errorf("单次最多注册 %d 个账号", panelRegisterMax)
	}

	credentialPath, err := paths.credentialPath(cfg)
	if err != nil {
		return report, err
	}
	existing, err := nativePanelReadCredentials(credentialPath)
	if err != nil {
		return report, err
	}

	// 出口优先取探测通过的那些。
	//
	// 这里原本是 outbound.PickRawURL()，它返回「最优」条目 —— 而 tier() 把已驱逐和正在
	// 冷却的都归为同一档，没有更好选择时 bestLocked 仍会把它交出来。这个出口随后被递给
	// FlareSolverr，容器里的 Chrome 拿到一个不通的代理只会回
	// ERR_PROXY_CONNECTION_FAILED：整轮注册作废，而报错指向浏览器，完全看不出是出口挑
	// 错了。池子本身知道哪些是 live 的（入池时 ValidateProxyCandidate 探过 L2，之后由巡检
	// 维护状态），所以这里该问它要一个 live 的。
	//
	// PickLiveRawURL 没有 live 出口时返回 ""，此时回落到 PickRawURL：那是刻意的降级 ——
	// 用一个状态未知的去试，仍然好过完全不带代理直连（用户明确要求不能走直连）。
	//
	// 还要看「谁来解 Turnstile」：容器里的 FlareSolverr 连不上宿主的回环代理，也连不
	// 上 socks5 出口（实测 http 出口回 302，socks5/socks5h 都是 000）。把这种出口递给
	// 它，报错就是用户反复贴的 ERR_PROXY_CONNECTION_FAILED —— 而报错指向浏览器，完全
	// 看不出是出口挑错了。本机 Chrome 没有这个限制。
	solverUsable := turnstile.ExitUsableByFlareSolverr
	if useChromeSolver(cfg) {
		solverUsable = turnstile.ExitUsableByChrome
	}
	// proxy 模式的语义是「从网关代理池取出口」。原先这里把配置里的 phone_socks /
	// clash_proxy 放在池子前面，即使用户明确选了 proxy，首轮也会固定走 127.0.0.1 的
	// 本地 SOCKS，再在重试时才进池子。这不但违背模式选择，还让每个号白烧第一次尝试。
	//
	// 显式 request.Proxy 仍然优先：那是调用方把本轮出口钉住的明确意图。phone / clash
	// 模式才使用各自配置的本地出口。
	proxyURL := strings.TrimSpace(request.Proxy)
	if mode != "proxy" {
		proxyURL = firstNonEmpty(proxyURL, cfg.Register.PhoneSOCKS, cfg.Register.ClashProxy)
	}
	// phone 模式没有出口就必须停下，不能回落到代理池、更不能回落到直连。
	//
	// 原先这里的条件是 `mode == "proxy" || proxyURL == ""`，于是 phone 模式在
	// phone_socks 没配的时候会掉进池子分支 —— 而 registerReady() 只校验 site_url /
	// email_domain / email_prefix / password，defaultNativePanelFileConfig 也从不设
	// phone_socks，所以这个组合是可达的。后果是三个组件对「手机出口是什么」的理解不
	// 一致：EnsureTunnel 和 rotatePhone 都默认 socks5://127.0.0.1:1081，而这里什么默
	// 认都不用。表现出来就是：隧道探测拿到运营商 IPv6、状态页显示轮换一切正常，而注册
	// 实际全部从池子里某个代理发出，把那个 IP 的当日额度用掉；飞行模式照切，切的是一
	// 个没人在用的出口，所以连「换出口未生效」都不会报。
	//
	// 池子空的时候更糟：proxyURL 留空，下面那条求解器提示又以 p != "" 为前提不会触发，
	// Chrome 于是不带代理启动，注册直接走本机家庭宽带出口 —— 用户明确要求不能走直连。
	//
	// 所以 phone 模式在这里补上和另外两个组件相同的默认值，而不是回落到代理池。宁可
	// 打一个没监听的本地端口、当场失败，也不要从一个「轮换管不到」的 IP 上悄悄注册。
	if mode == "phone" && strings.TrimSpace(proxyURL) == "" {
		proxyURL = exitrotate.DefaultPhoneSOCKS
		report.Notes = append(report.Notes,
			fmt.Sprintf("未配置 phone_socks，按手机出口默认地址 %s 走；如果手机隧道不在这个端口上，请在面板里显式配置", proxyURL))
	}
	if mode == "proxy" || strings.TrimSpace(proxyURL) == "" {
		// 第一个候选也按近期表现排：否则每一轮注册都从同一个已知过不了 CF 的出口开始，
		// 白烧一次重试额度。
		usableLive := turnstile.PreferProvenExits(turnstile.FilterExits(outbound.LiveProxyPoolRawURLs(), solverUsable))
		usableAll := turnstile.PreferProvenExits(turnstile.FilterExits(outbound.ProxyPoolRawURLs(), solverUsable))
		proxyURL = firstNonEmpty(proxyURL, firstOf(usableLive), firstOf(usableAll))
	}
	// 池子空、又没有本地出口时，proxyURL 到这里仍然是空的，Chrome 会不带代理启动，
	// 注册走本机家庭宽带 —— 用户明确要求不能走直连。单次注册是人点出来的，这里只如实
	// 记一条，不替他改主意；长跑任务是无人值守的，由 runner 在启动前直接拒绝（见
	// panel_register_runner.go），免得几千个号全从家里的 IP 发出去。
	if strings.TrimSpace(proxyURL) == "" {
		report.Notes = append(report.Notes,
			fmt.Sprintf("%s 模式没拿到任何出口（代理池为空且未配置本地出口），这一轮将不带代理注册，本机 IP 会直接暴露给站点", mode))
	}
	// 调用方显式指定的出口也要能被求解器用到，否则整轮注册必然失败，且失败原因
	// 落在浏览器报错上。这里不改它，只如实说清楚。
	if p := strings.TrimSpace(proxyURL); p != "" && !solverUsable(p) {
		report.Notes = append(report.Notes,
			fmt.Sprintf("出口 %s 求解器用不了（容器里的 FlareSolverr 连不上回环地址和 socks5 出口），Turnstile 可能解不出来", redactExitForNote(p)))
	}
	rotateReq := exitrotate.Request{
		Mode:       mode,
		PhoneSOCKS: cfg.Register.PhoneSOCKS,
		// adb 的路径必须带上：exitrotate 拿不到配置就只执行 PATH 上的 "adb"，而
		// Windows 上它通常装在 WinGet 包目录里、并不在 PATH，于是 phone 模式每次
		// 换 IP 都以「adb 找不到」失败，整批注册从第二个号起全部报错。
		ADB:         cfg.Register.ADB,
		ClashAPI:    cfg.Register.ClashAPI,
		ClashSecret: cfg.Register.ClashSecret,
		ClashGroup:  cfg.Register.ClashGroup,
		ClashProxy:  cfg.Register.ClashProxy,
		ClashNode:   firstNonEmpty(request.Node, firstClashNode(cfg)),
		ExpectIP:    firstClashNodeExpectIP(cfg, request.Node),
		ProbeProxy:  firstNonEmpty(request.Proxy, cfg.Register.ClashProxy, cfg.Register.PhoneSOCKS),
	}
	// 出口轮换需要一个候选列表，不能靠「再问一次」拿到不同结果。
	//
	// clash：节点名原先在循环外只算一次，于是每轮都切到同一个节点，等于没换。
	// proxy：outbound.PickRawURL() 是确定性的，原先的 next != proxyURL 恒为假，
	// 整批号全用同一个代理注册。两者都要按列表推进。
	//
	// request.Node 非空表示调用方指定了节点，此时不轮换 —— 那是显式意图。
	nodes := clashNodes(cfg)
	pinnedNode := strings.TrimSpace(request.Node) != ""
	// 轮换候选也只用 live 的，否则轮换会把已驱逐的出口重新轮进来 ——
	// 那正是「注册随机失败」的形态：前一个号成功，下一个号换到死出口就报
	// ERR_PROXY_CONNECTION_FAILED。没有 live 出口时回落到全量列表，理由同上：
	// 状态未知也好过不带代理。
	//
	// 轮换候选同样要过求解器那道筛：否则「每个号换一个出口」有一部分轮次会换到求解器
	// 用不了的出口上，表现成随机失败。
	poolURLs := turnstile.FilterExits(outbound.LiveProxyPoolRawURLs(), solverUsable)
	if len(poolURLs) == 0 {
		poolURLs = turnstile.FilterExits(outbound.ProxyPoolRawURLs(), solverUsable)
	}
	// 按「近期能不能过 CF」重排候选顺序。
	//
	// 实测三轮注册都从池子的同一个顺序开头挑，于是每个号都要把同一批过不了 CF 的出口
	// 重新踩一遍 —— 第一个候选每次都是同一个，六次重试有一半以上花在已经确认不行的出口
	// 上。这里只重排先后，不增不删，用户的代理列表和池子状态都不受影响。
	poolURLs = turnstile.PreferProvenExits(poolURLs)
	exitTurn := 0
	var lastIP string
	// phoneBrowser 非 nil 时注册页在手机上的 Cromite 里打开，本机不起浏览器。
	//
	// 这条要写进 Notes：手机模式下 proxyURL 仍然是那个 SOCKS 地址（隧道还要用来实测出口 IP、
	// 确认轮换真的换了），但页面已经不走它了。日志里只看代理字段的话，两条路线长得一模一样，
	// 而它们暴露给站点的 IP 完全不同。
	phoneBrowser := phoneBrowserConfig(cfg, mode)
	if phoneBrowser != nil {
		report.Notes = append(report.Notes,
			"注册页在手机上的 Cromite 里打开（出口为手机自身运营商 IP），本机不起浏览器；要退回旧路径把配置里的 phone_browser_off 设成 true")
	}
	// carriedIP 是上一批最后一个号用掉的出口 IP，只对 phone 模式有意义：其余模式的
	// 「出口」是代理地址，换出口靠换代理、由 poolURLs 推进，不需要这条信息。
	//
	// 它刻意只表示「跨批带进来的那一个 IP」，用掉即清。批次内部各号之间由每次成功后的
	// 尾部轮换负责，不能也走这条路径 —— 否则每个号开头都会再切一次飞行模式（一次约
	// 10 秒），而它要防的重复只存在于批次边界上。
	carriedIP := ""
	if mode == "phone" {
		carriedIP = strings.TrimSpace(request.SpentIP)
	}
	// 正常跑完时断点就是号段末尾之后一个；提前中断时下面会改写它。
	report.NextNum = start + count
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			// 被取消时也要如实报断点。调用方通常只看 err 就丢掉整个 report，但断点是
			// 唯一能说明「哪个号之后没试过」的信息 —— 停止时用它续跑比按批长推进准确。
			report.Stopped = true
			report.NextNum = start + i
			return report, ctx.Err()
		default:
		}
		num := start + i
		username := fmt.Sprintf("%s%d", strings.TrimSpace(cfg.Register.EmailPrefix), num)
		email := username + "@" + strings.TrimSpace(cfg.Register.EmailDomain)
		item := panelRegisterAccount{Num: num, Email: email}
		if _, ok := existing[email]; ok {
			item.Status, item.Detail = "skipped", "账密清单已有该邮箱"
			report.Accounts = append(report.Accounts, item)
			continue
		}

		displayBase := cfg.Register.DisplayBase
		if displayBase <= 0 {
			displayBase = 1
		}
		emailStart := cfg.Register.EmailStartNum
		if emailStart <= 0 {
			emailStart = start
		}
		display := fmt.Sprintf("User%d", num-(emailStart-displayBase))
		// 一个号要允许在多个出口上试。
		//
		// 原先每个号只试 proxyURL 一个出口，失败即判死 —— 而实测池子里大多数 live 出口
		// 都过不了这一关：有的页面根本加载不出来，有的页面正常但 Cloudflare 认定该 IP
		// 高风险、一直不发 token。于是「抽签抽中坏出口」= 这个号永久失败，这正是用户反
		// 复看到的注册失败。
		//
		// 只在错误确实指向出口时才换（见 turnstile.ExitAttributable）：要邮箱验证、要邀
		// 请码这类换一百个出口也是同样的错，立刻返回。站点每个 IP 每天只允许成功注册 1
		// 次，出口是有成本的资源，不能拿来乱撞。
		var (
			outcome    registerOutcome
			outcomeErr error
			attempts   int
			triedExits []string
		)
		for {
			attempts++
			probeCtx, probeCancel := context.WithTimeout(ctx, 8*time.Second)
			if ip, err := probeRegisterIP(probeCtx, proxyURL); err == nil {
				item.IP = ip
				lastIP = ip
			} else {
				// 探测失败就必须把 lastIP 清掉，不能留着上一个号的地址。
				//
				// 它会作为 PrevIP 交给 exitrotate，而 rotatePhone 有一条捷径：当前 IP
				// 与 PrevIP 不同就直接判定「出口已经变了」，连飞行模式都不切。留着过期
				// 的地址，这条捷径就会在「其实没换」的时候成立 —— 实测 5856 号探测失败、
				// lastIP 停在上一个号的 IP，于是 5857 号复用了 5856 刚用掉配额的那个 IP，
				// 直接撞上「当前 IP 在 1 天内注册次数已达上限」。
				//
				// 清成空串反而是安全的：PrevIP 为空时 rotatePhone 不走捷径，一定真的切一次。
				lastIP = ""
			}
			probeCancel()
			// 出口已知用掉了当日额度就先换，别拿它去解一次 Turnstile。
			//
			// 只在「探测到的地址确实等于调用方告知的已用地址」时才动手：地址自己变了就
			// 什么都不做。所以这不会多切飞行模式，只是把必然失败的那一次尝试省掉 ——
			// 那一次要花十几秒解 Turnstile、POST 一次，然后必然收到「已达上限」。
			if mode == "phone" && attempts == 1 && carriedIP != "" && item.IP != "" && item.IP == carriedIP {
				rotateReq.PrevIP = carriedIP
				rotated, rotateErr := rotateExit(ctx, rotateReq)
				report.Rotate = &rotated
				// 成功也好失败也好，这条跨批信息就地用完清掉：留着只会让本批后面每个号
				// 都再判一次、再切一次。失败的后果只是这一个号白试一次，下一个号有尾部
				// 轮换兜着。
				carriedIP = ""
				if rotateErr != nil {
					report.Notes = append(report.Notes,
						fmt.Sprintf("第 %d 号开始前换出口失败（上一批末号已用掉该 IP 的当日额度）：%v", num, rotateErr))
				} else if rotated.IP != "" {
					item.IP = rotated.IP
					lastIP = rotated.IP
				}
			}
			// phone 模式记的是实际出口 IP，不是代理地址：本机 SOCKS 地址每轮都一样，
			// 记它只会得到「已换 3 个出口：127.0.0.1:1081、127.0.0.1:1081、…」这种
			// 看不出任何信息的报告。真正变的是运营商下发的地址。
			if mode == "phone" {
				if ip := strings.TrimSpace(item.IP); ip != "" {
					triedExits = append(triedExits, ip)
				}
			} else if p := strings.TrimSpace(proxyURL); p != "" {
				triedExits = append(triedExits, redactExitForNote(p))
			}
			outcome, outcomeErr = completeRegister(ctx, cfg, request.TurnstileToken, proxyURL, display, username, phoneBrowser)
			// 记下这个出口在 Turnstile 这一关的表现，只用来决定下一次先试谁。
			// 出口是否可用由代理池自己判断，这里不碰它的状态，也不动用户的列表。
			if p := strings.TrimSpace(proxyURL); p != "" {
				switch {
				case outcomeErr == nil:
					// 成功了：这个出口确实过了 CF，但它今天的注册配额也用掉了
					// （站点 ipRules 每 IP 每天 1 次），下一个号不该再从它开始。
					turnstile.NoteExitTurnstileOK(p)
					turnstile.NoteExitQuotaUsed(p)
				case turnstile.ExitQuotaExhausted(outcomeErr):
					// 站点亲口说这个 IP 今天用完了。它过了 CF，所以不是坏出口，
					// 只是今天不能再用。
					turnstile.NoteExitTurnstileOK(p)
					turnstile.NoteExitQuotaUsed(p)
				case !turnstile.ExitAttributable(outcomeErr):
					// 走到站点侧回话（例如「邮箱已注册」这类业务错误）就说明这个出口
					// 确实过了 CF —— 那正是下一个号该优先用的出口。
					turnstile.NoteExitTurnstileOK(p)
				default:
					turnstile.NoteExitTurnstileFailed(p)
				}
			}
			if outcomeErr == nil {
				break
			}
			if !turnstile.ExitAttributable(outcomeErr) || attempts >= registerExitAttempts || ctx.Err() != nil {
				break
			}
			// 怎么换出口按模式分：
			//
			// phone  手机自己就是一个可无限轮换的出口 —— 切一次飞行模式运营商就下发新
			//        地址（实测整段 /64 都变）。所以这个号不必判死，换个 IP 再试一次。
			//        本机 SOCKS 地址不变，proxyURL 保持原样。
			// proxy  按池子里的顺序推进到下一个出口。
			// clash  节点由 Clash 侧控制，这里不介入，保持原有的「立即返回」。
			retried := false
			switch mode {
			case "phone":
				rotateReq.PrevIP = lastIP
				rotated, rotateErr := rotateExit(ctx, rotateReq)
				report.Rotate = &rotated
				if rotated.IP != "" {
					lastIP = rotated.IP
				}
				// 只有确实换到新 IP 才值得重试：IP 没变就是在同一个出口上撞同一面墙。
				retried = rotateErr == nil && rotated.Changed
				if rotateErr != nil {
					report.Notes = append(report.Notes,
						fmt.Sprintf("第 %d 号重试前换手机 IP 失败：%v", num, rotateErr))
				}
			case "proxy":
				if next := nextExitAfter(poolURLs, proxyURL); next != "" {
					proxyURL = next
					exitTurn++
					retried = true
				}
			}
			if !retried {
				break
			}
		}
		if outcomeErr != nil {
			item.Status = "failed"
			item.Detail = outcomeErr.Error()
			// 试了多个出口就要说清楚试了哪些：否则报错只剩最后一个出口的症状，
			// 看不出前面几个是同样的问题还是各有不同。
			if attempts > 1 {
				item.Detail = fmt.Sprintf("%s（已换 %d 个出口重试：%s）",
					item.Detail, attempts, strings.Join(triedExits, "、"))
			}
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			// 这个号失败后要把出口推进，否则下一个号会从刚刚失败的那个出口重新开始，
			// 把重试额度浪费在同一个已知坏出口上。成功路径不在这里推进 —— 它走下面
			// 那段轮换逻辑，那里还要处理 clash 模式和「池子只有一个出口」的情形。
			if mode == "proxy" {
				if next := nextExitAfter(poolURLs, proxyURL); next != "" {
					proxyURL = next
					exitTurn++
				}
			}
			// phone 模式同理，但只在错误确实指向出口时才换。
			//
			// 循环退出前的最后一步一定是一次失败的尝试（换 IP 之后必然还会再试一次），
			// 所以这里换一次不会和重试循环重复；不换的话下一个号会从刚刚失败的那个运营
			// 商 IP 开始。
			//
			// 反过来，「该邮箱已被注册」这类站点侧业务错误跟出口无关：这个 IP 又好又没用
			// 掉当天额度，为它切一次飞行模式要白等十几秒，还把一个能用的出口换掉了。
			if mode == "phone" && i < count-1 && turnstile.ExitAttributable(outcomeErr) {
				rotateReq.PrevIP = lastIP
				if rotated, rotateErr := rotateExit(ctx, rotateReq); rotateErr == nil {
					report.Rotate = &rotated
					if rotated.IP != "" {
						lastIP = rotated.IP
					}
				} else {
					report.Notes = append(report.Notes,
						fmt.Sprintf("第 %d 号失败后换手机 IP 失败：%v；下一个号可能仍从同一 IP 注册", num, rotateErr))
				}
			}
			continue
		}
		if attempts > 1 {
			report.Notes = append(report.Notes,
				fmt.Sprintf("第 %d 号换了 %d 个出口才成功，前面的出口不可用：%s",
					num, attempts, strings.Join(triedExits[:len(triedExits)-1], "、")))
		}
		if upn := strings.TrimSpace(outcome.UPN); nativePanelValidEmail(upn) {
			email = upn
			item.Email = email
		}
		if err := nativePanelAppendCredential(credentialPath, email, cfg.Register.Password); err != nil {
			item.Status, item.Detail = "failed", "注册成功但写入账密失败: "+err.Error()
			report.Failed++
			report.Accounts = append(report.Accounts, item)
			continue
		}
		existing[email] = cfg.Register.Password
		item.Status, item.Detail = "success", "registered"
		if s != nil && s.tokens != nil {
			if oauth, _ := s.authorizeAccountWithPassword(email, cfg.Register.Password); oauth.Status != "" {
				item.OAuthStatus = oauth.Status
				item.OAuthDetail = oauth.Detail
				item.AuthorizationURL = oauth.AuthorizationURL
				if oauth.Status == "imported" {
					item.Detail = "registered+oauth"
				} else if oauth.AuthorizationURL != "" {
					item.Detail = "registered, oauth pending"
				}
			}
		}
		report.Success++
		report.Accounts = append(report.Accounts, item)
		// 记下这次成功用掉的出口，交给调用方作为下一批的 SpentIP。
		// 站点每 IP 每天只允许成功注册 1 次，所以「成功」等价于「这个 IP 今天用完了」。
		// 记下这个号是从哪个 IP 注册成功的。整批跑完后它就是「最后一个已用掉额度的
		// 出口」，调用方下一批把它作为 SpentIP 传回来，好让下一批第一个号先换出口。
		// 这里不回写 carriedIP：批内的重复由尾部轮换负责，见上面 carriedIP 的注释。
		if mode == "phone" {
			if ip := strings.TrimSpace(item.IP); ip != "" {
				report.LastIP = ip
			}
		}

		// 先联网过 CF、提交注册并写入本地账密，确认成功后再换出口，
		// 给下一个号用。开着飞行模式是过不了 Turnstile 的。
		if i < count-1 {
			exitTurn++
			if mode == "proxy" {
				// 按池子里的顺序推进。池中只有一个代理时确实换不了出口，这不是
				// 静默成功：如实记一行，让用户知道整批号共用同一个出口。
				switch {
				case len(poolURLs) > 1:
					proxyURL = poolURLs[exitTurn%len(poolURLs)]
				case len(poolURLs) == 1 && strings.TrimSpace(proxyURL) == "":
					proxyURL = poolURLs[0]
				default:
					report.Notes = append(report.Notes,
						fmt.Sprintf("第 %d 号之后未能更换出口：代理池仅有 %d 个可用条目，后续账号将复用同一出口", num, len(poolURLs)))
				}
				continue
			}
			if !pinnedNode && len(nodes) > 0 {
				next := nodes[exitTurn%len(nodes)]
				rotateReq.ClashNode = next.Name
				// 顺带把该节点预期的出口 IP 交给 exitrotate，让它的 IP 不符告警
				// 真的能触发。
				rotateReq.ExpectIP = next.ExpectIP
			}
			rotateReq.PrevIP = lastIP
			rotated, rotateErr := rotateExit(ctx, rotateReq)
			report.Rotate = &rotated
			if rotated.IP != "" {
				lastIP = rotated.IP
			}
			// OK 只表示流程没报错，Changed 才表示出口真的换了。两者分开看：轮换没
			// 生效时下一个号会从同一个 IP 注册，这一点必须让用户知道，而不是当成
			// 成功一路走下去。
			if rotateErr == nil && !rotated.Changed {
				detail := strings.TrimSpace(rotated.Detail)
				if detail == "" {
					detail = "出口未发生变化"
				}
				report.Notes = append(report.Notes,
					fmt.Sprintf("第 %d 号之后换出口未生效：%s；后续账号可能仍从同一 IP 注册", num, detail))
			}
			if rotateErr != nil {
				// 换不了出口就到此为止，但**不能**给剩下的号伪造 failed 记录。
				//
				// 原先是那样做的：为 i+1..count-1 每个号各写一条 failed，然后返回
				// err == nil。调用方看到没出错就按 num += count 推进，于是这些一次都没
				// 尝试过的号被永久跳过。触发条件只是 adb 抖一下（USB 掉一下、adb server
				// 重启、手机重启导致 toggleAirplane 报错），一批 20 个里就有 19 个被判死，
				// 号段照样往前走 —— 持续抖动时 2000 个号能只建出 100 个账号。
				//
				// 现在如实报断点：这些号没试过，NextNum 指向第一个没试的号，调用方从那里
				// 接着跑。已经成功的号仍然留在 report 里，不因为中断而丢掉。
				report.Stopped = true
				report.NextNum = start + i + 1
				remaining := count - i - 1
				report.Notes = append(report.Notes,
					fmt.Sprintf("第 %d 号之后换出口失败，本批就此中断：%v；%d 个号（%d-%d）尚未尝试，请从 %d 继续",
						num, rotateErr, remaining, start+i+1, start+count-1, start+i+1))
				break
			}
		}
	}
	report.Total = len(report.Accounts)
	report.OK = report.Failed == 0 && report.Success > 0
	return report, nil
}

// useChromeSolver 决定这一轮用哪个后端解 Turnstile。
//
// auto（默认）：本机有 Chrome/Edge 就用它，否则退回 FlareSolverr。之所以默认偏向浏览
// 器，是因为两者能力不同 —— FlareSolverr 的 request.get 只能回一张 page_source 快照，
// 而 Turnstile 把 token 写在隐藏 input 的 value property 上，序列化的 HTML 里没有这个
// 值；它也不会替你点控件、提交表单。实测下这个站点走 FlareSolverr 永远拿不到 token。
//
// chrome / flaresolverr：显式指定。配置成 chrome 但本机没有浏览器时不硬撑，回落到
// FlareSolverr 并由它自己报出装浏览器的建议 —— 那条消息比这里再造一句更准确。
func useChromeSolver(cfg nativePanelFileConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Register.Solver)) {
	case "flaresolverr", "flare":
		return false
	case "chrome", "browser", "edge":
		return turnstile.ChromeAvailable()
	default:
		return turnstile.ChromeAvailable()
	}
}

// firstOf 取列表里第一个条目，空列表返回空串。
// registerExitAttempts 是单个号最多试几个出口。
//
// 上限存在的意义是别把一次注册变成把整个池子撞完：真正是站点侧的问题（结构变了、
// 参数不对）时，96 个出口会一个不落地失败，每个都要跑满 Turnstile 预算。
//
// 定在 10：实测能过 CF 的 http 出口约占三分之一，10 次把「一个号也挑不到好出口」的概率
// 压到 2% 以下。代价可控 —— 加了提前判死之后，一个坏出口约 22 秒就能识别出来（原先要
// 55 秒），10 次最差约 4 分钟，仍然能在一次请求里把真实错误报回来。
const registerExitAttempts = 10

// nextExitAfter 返回 current 在候选列表里的下一个出口，用于同一个号的换出口重试。
//
// 从 current 的位置往后取，而不是从头开始：这样重试不会反复撞在同一批前缀上。列表里
// 找不到 current（例如调用方钉了一个池外的出口）时从头取。候选不足 2 个时返回空串，
// 由调用方停止重试 —— 拿同一个出口再试一遍没有意义。
func nextExitAfter(pool []string, current string) string {
	if len(pool) == 0 {
		return ""
	}
	cur := strings.TrimSpace(current)
	idx := -1
	for i, p := range pool {
		if strings.TrimSpace(p) == cur {
			idx = i
			break
		}
	}
	if idx < 0 {
		if next := strings.TrimSpace(pool[0]); next != cur {
			return next
		}
		if len(pool) < 2 {
			return ""
		}
		return strings.TrimSpace(pool[1])
	}
	if len(pool) < 2 {
		return ""
	}
	return strings.TrimSpace(pool[(idx+1)%len(pool)])
}

func firstOf(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// redactExitForNote 让出口能出现在报告里而不泄露代理密码。
//
// Notes 会原样进管理接口的响应，也就是进浏览器的 devtools —— 池子里的条目可能带
// user:pass，不能整条抄进去。
func redactExitForNote(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return "(已隐藏)"
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "http"
	}
	if parsed.User != nil {
		return scheme + "://***@" + parsed.Host
	}
	return scheme + "://" + parsed.Host
}

var rotateExit = exitrotate.Rotate

func probeRegisterIP(ctx context.Context, proxyURL string) (string, error) {
	result, err := rotateExit(ctx, exitrotate.Request{Mode: "proxy", ProbeProxy: proxyURL})
	if err != nil {
		return "", err
	}
	return result.IP, nil
}

type registerOutcome struct {
	UPN string
}

// phoneBrowserConfig 决定这一轮要不要用手机上的浏览器，以及用哪台。
//
// 只有 phone 模式才可能返回非 nil：其余模式的出口是代理地址，页面必须从本机带着那个代理
// 发出去，跑到手机上就完全绕开了配置里的出口。
func phoneBrowserConfig(cfg nativePanelFileConfig, mode string) *phonecdp.Config {
	if mode != "phone" || cfg.Register.PhoneBrowserOff {
		return nil
	}
	return &phonecdp.Config{ADB: strings.TrimSpace(cfg.Register.ADB)}
}

func completeRegister(ctx context.Context, cfg nativePanelFileConfig, supplied, proxyURL, display, username string, phone *phonecdp.Config) (registerOutcome, error) {
	token := strings.TrimSpace(supplied)
	if token == "" {
		page := strings.TrimRight(strings.TrimSpace(cfg.Register.SiteURL), "/")
		if page == "" {
			return registerOutcome{}, errors.New("缺少注册页地址")
		}
		// 本机有浏览器就走浏览器。这不是偏好问题：FlareSolverr 的 request.get 只回一
		// 张页面快照，而 Turnstile 把 token 写在隐藏 input 的 value property 上，
		// 序列化的 HTML 里没有它 —— 那条路在这个站点上结构性地取不到 token。
		//
		// 配置里 solver=flaresolverr 时仍然听配置：显式意图不该被代码悄悄改掉。
		if useChromeSolver(cfg) {
			solved, err := turnstile.SolveWithChrome(ctx, turnstile.ChromeRequest{
				PageURL:     page,
				Proxy:       proxyURL,
				DisplayName: display,
				Username:    username,
				Password:    cfg.Register.Password,
				PlanID:      strings.TrimSpace(cfg.Register.PlanID),
				DomainID:    strings.TrimSpace(cfg.Register.DomainID),
				// 非空时页面在手机上加载，Proxy 这一轮不起作用（见 ChromeRequest.Phone）。
				Phone: phone,
			})
			if err != nil {
				return registerOutcome{}, err
			}
			token = strings.TrimSpace(solved.Token)
		} else {
			endpoint := strings.TrimSpace(cfg.Register.FlareSolverrURL)
			if endpoint == "" {
				endpoint = turnstile.DefaultEndpoint
			}
			solved, err := turnstile.Solve(ctx, turnstile.Request{
				Endpoint:    endpoint,
				PageURL:     page,
				Proxy:       proxyURL,
				DisplayName: display,
				Username:    username,
				Password:    cfg.Register.Password,
			})
			if err != nil {
				return registerOutcome{}, err
			}
			token = strings.TrimSpace(solved.Token)
		}
	}
	if strings.HasPrefix(token, "ERROR:") {
		return registerOutcome{}, errors.New(strings.TrimPrefix(token, "ERROR:"))
	}
	if strings.HasPrefix(token, "SUBMITTED:") {
		upn := strings.TrimPrefix(token, "SUBMITTED:")
		if upn == "registered-ok" {
			upn = ""
		}
		return registerOutcome{UPN: upn}, nil
	}
	if token == "" {
		return registerOutcome{}, errors.New("注册页已打开，但没有完成填表和提交")
	}
	if err := postRegister(ctx, cfg, username, display, token, proxyURL); err != nil {
		return registerOutcome{}, err
	}
	return registerOutcome{}, nil
}

// firstClashNodeExpectIP 给出首轮节点预期的出口 IP。
//
// 调用方显式指定了节点时，就按那个节点的配置取；否则用列表里的第一个。没有配
// expect_ip 就返回空串，exitrotate 会跳过 IP 校验。
func firstClashNodeExpectIP(cfg nativePanelFileConfig, requested string) string {
	nodes := clashNodes(cfg)
	if pinned := strings.TrimSpace(requested); pinned != "" {
		for _, node := range nodes {
			if node.Name == pinned {
				return node.ExpectIP
			}
		}
		return ""
	}
	if len(nodes) > 0 {
		return nodes[0].ExpectIP
	}
	return ""
}

// firstClashNode 给出首轮使用的节点名。后续轮换由 clashNodes 提供完整列表。
func firstClashNode(cfg nativePanelFileConfig) string {
	if nodes := clashNodes(cfg); len(nodes) > 0 {
		return nodes[0].Name
	}
	return ""
}

// clashNode 是一个可切换的 Clash 出口：节点名，以及该节点预期的出口 IP。
type clashNode struct {
	Name     string
	ExpectIP string
}

// clashNodes 返回配置里全部可用节点，供批量注册在账号之间轮换出口。
//
// 只取第一个节点是不够的：换出口的目的就是让下一个号从另一个 IP 注册，若每轮都
// 切到同一个节点，Clash 侧其实没有任何变化，注册仍然来自同一出口。
//
// expect_ip 一并带出。配置里早就有这个键，exitrotate 也早就会在实际 IP 与预期不符
// 时给出告警（rotate.go 的 ExpectIP 分支），但两头从来没接起来 —— 于是那个告警永
// 远不可能触发，配置项形同虚设。
func clashNodes(cfg nativePanelFileConfig) []clashNode {
	out := make([]clashNode, 0, len(cfg.Register.ClashNodes))
	for _, node := range cfg.Register.ClashNodes {
		name := strings.TrimSpace(node.Name)
		if name == "" {
			continue
		}
		out = append(out, clashNode{Name: name, ExpectIP: strings.TrimSpace(node.ExpectIP)})
	}
	return out
}

func postRegister(ctx context.Context, cfg nativePanelFileConfig, username, display, turnstile, proxyURL string) error {
	site := strings.TrimRight(strings.TrimSpace(cfg.Register.SiteURL), "/")
	if site == "" {
		return errors.New("site_url 为空")
	}
	planID := strings.TrimSpace(cfg.Register.PlanID)
	if planID == "" {
		planID = "1"
	}
	domainID := strings.TrimSpace(cfg.Register.DomainID)
	if domainID == "" {
		domainID = "1"
	}
	payload, _ := json.Marshal(map[string]any{
		"planId":            planID,
		"domainId":          domainID,
		"inviteCode":        "",
		"displayName":       display,
		"username":          username,
		"password":          cfg.Register.Password,
		"verificationEmail": "",
		"emailCode":         "",
		"turnstileToken":    turnstile,
	})
	client := outbound.HTTPClient()
	if strings.TrimSpace(proxyURL) != "" {
		if clients, err := outbound.New(proxyURL); err == nil && clients != nil && clients.HTTP != nil {
			client = clients.HTTP
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, site+"/api/register", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if v, exists := parsed["ok"]; exists {
		if flag, isBool := v.(bool); isBool {
			ok = flag
		}
	}
	if !ok {
		msg := strings.TrimSpace(fmt.Sprint(parsed["message"]))
		if msg == "" || msg == "<nil>" {
			msg = strings.TrimSpace(string(body))
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return errors.New(msg)
	}
	return nil
}

func nativePanelAppendCredential(path, email, password string) error {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return errors.New("email and password required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s----%s\n", email, password)
	return err
}

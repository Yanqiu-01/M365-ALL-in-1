package chathub

import (
	"log"
	"strings"
	"unicode/utf8"
)

const (
	// snapshotStableBytes 保持原有语义：开头的极短快照可能还在抖动，先不
	// 下发。区别是这些字节现在会被 snapshotTracker 记下来，完成帧再补齐，
	// 因此「按住」不再等于「丢弃」。
	snapshotStableBytes = 64
	// snapshotMinAnchorBytes 是追补时可信锚点的最短长度。沿用
	// internal/web/stream_recovery.go 里 retryTextReconciler 的既有约定：
	// 过短的重合不可信，会把无关位置当成续写点。
	snapshotMinAnchorBytes = 16
	// snapshotAnchorWindow 限制寻找锚点时回看的字节数，避免长回复上出现
	// 二次方级的字符串扫描。
	snapshotAnchorWindow = 512
)

// snapshotTracker 留存上游正文的「真相」。
//
// ChatHub 的正文以递增快照到达（writeAtCursor 或 bot messages[].text），但上游
// 在挂附件、补引用时会整段重写已经发出的文本。实测形状：先发
//
//	](https://kr-prod.asyncgw.teams.microsoft.com/.../hello.py)
//
// 随后重写成
//
//	[hello.py](https://kr-prod.asyncgw.teams.microsoft.com/.../hello.py)
//
// 新快照不是已发送内容的前缀，旧实现因此把整个快照丢掉，只留下残缺的前一版
// （生产日志里这条 skip 出现过 6439 次）。用户看到开头被吃掉的 `](url)`，很容易
// 被误判成模型拒答或输出截断。
//
// tracker 单独留存最后一个（最完整的）快照，完成帧据此校正 Result.Text。已经
// 发给客户端的增量一律不撤回、不重发，因此修复不会带来复读。
type snapshotTracker struct {
	// canon 是上游最后一个完整快照，也就是正文的真相。
	canon string
	// 可观测性计数：rewrites 非前缀重写，shrinks 比已知文本更短的分叉快照，
	// stale 迟到的更短重复快照，citeOnly 只含引用标记因而被跳过的快照。
	rewrites int
	shrinks  int
	stale    int
	citeOnly int
}

// observe 记录一个上游快照。它必须在任何下发决策之前调用，这样即使快照没有被
// 下发（抖动窗口、非前缀重写），内容也不会丢。
func (t *snapshotTracker) observe(snapshot string) {
	if t == nil || snapshot == "" {
		return
	}
	// 引用标记不是正文。剥离后为空的快照纯属元数据：实测上游会在挂附件时用一个
	// 25 字节的纯标记快照（\ue200cite\ue202turn12file12\ue201）覆盖先前 122
	// 字节的真实正文，采纳它会让附件链接整段消失。这里只记账、不采纳，保留已有
	// 的更完整正文。判定见 citation_markers.go，基于私用区分隔符结构而非锚点名。
	visible, markerOnly := visibleSnapshotText(snapshot)
	if markerOnly {
		t.citeOnly++
		return
	}
	// 快照在进入 canon 之前就完成剥离，因此 canon、已发送增量、最终文本出自同一
	// 套剥离规则 —— 流式与非流式不可能给出不同的可见文本。
	snapshot = visible
	switch {
	case t.canon == "":
		t.canon = snapshot
	case snapshot == t.canon:
	case strings.HasPrefix(snapshot, t.canon):
		// 正常的递增快照。
		t.canon = snapshot
	case strings.HasPrefix(t.canon, snapshot):
		if trimTrailingInlineSpace(t.canon) == snapshot {
			// 不是重放，而是剥离回收了一个尾随空格：上一帧的标记还没闭合（例如
			// `结论 \ue200cite…`），闭合后规范化掉了标记前的那个空格。新版更
			// 准确，采纳它。这类「只差尾随行内空格」是剥离唯一可能让文本变短的
			// 情形，因此判定收得很窄，不会掩盖真正的重放。
			t.canon = snapshot
			return
		}
		// 迟到的重放：这只是早前某个光标状态的副本，已知文本更完整。
		// ChatHub 的快照是递增的，所以「更短且是前缀」唯一的合理解释就是重放。
		t.stale++
	default:
		// 上游重写了正文（挂附件 / 补引用）。分叉即代表这是新的一版，采纳它
		// ——「最终文本等于上游最后一个完整快照」是本修复的核心约束。重写后
		// 变短的情况（例如把冗长草稿换成精简终稿）也照此处理：分叉快照不会
		// 是重放，因此不存在被旧内容截断的风险。
		t.rewrites++
		if len(snapshot) < len(t.canon) {
			t.shrinks++
		}
		t.canon = snapshot
	}
}

// diverged 报告已发送内容是否已经无法代表上游最后的完整快照。
func (t *snapshotTracker) diverged(delivered string) bool {
	return t != nil && t.canon != "" && !strings.HasPrefix(delivered, t.canon)
}

// canonicalText 返回应当写入 Result.Text 的正文。已发送内容本身就包含快照全文
// 时（正常前缀增量，或完成帧已补齐）保持原样，否则采用最后一个完整快照。
func (t *snapshotTracker) canonicalText(delivered string) string {
	if t == nil || t.canon == "" {
		return delivered
	}
	if strings.HasPrefix(delivered, t.canon) {
		return delivered
	}
	return t.canon
}

// undelivered 返回还能安全追加到流上的那部分正文。
//
// 约束顺序（越靠前越优先）：
//  1. 绝不重发客户端已经收到的任何一段文本 —— 复读是硬禁止项；
//  2. 在满足 1 的前提下尽量把新内容送出去；
//  3. 找不到可信锚点时返回空串，由 canonicalText 在最终文本上兜底。
//
// 返回值恒为 canon 的一个后缀，且该后缀紧接在「已发送内容的一段尾巴」之后，
// 因此拼接结果里不会出现任何已发送片段的重复。
func (t *snapshotTracker) undelivered(delivered string) string {
	if t == nil || t.canon == "" {
		return ""
	}
	if delivered == "" {
		return t.canon
	}
	// 已发送内容已经涵盖快照全文，无需追补。
	if strings.HasPrefix(delivered, t.canon) {
		return ""
	}
	// 正常递增：快照只是在已发送内容后面长出新尾巴。
	if strings.HasPrefix(t.canon, delivered) {
		return t.canon[len(delivered):]
	}
	// 重写场景：以「已发送内容的最长可信尾巴在快照中的最后出现位置」为锚点，
	// 只追加锚点之后的内容。被重写掉的前缀（如 `[hello.py`）无法在流上追回
	// ——把它接在后面只会得到 `](url)[hello.py` 这种更糟的顺序——因此这里保持
	// 沉默，由 canonicalText 在最终文本上校正。
	minAnchor := snapshotMinAnchorBytes
	if len(delivered) < minAnchor {
		minAnchor = len(delivered)
	}
	start := 0
	if len(delivered) > snapshotAnchorWindow {
		start = len(delivered) - snapshotAnchorWindow
	}
	// 两段文本的分叉点。续写点必须落在它之后：分叉点之前的内容与已发送内容
	// 逐字节相同，从那里开始追加就是复读。
	diverged := commonPrefixBytes(t.canon, delivered)
	// 由长到短尝试锚点：最长的重合最可信。
	for ; start <= len(delivered)-minAnchor; start++ {
		if !utf8.RuneStart(delivered[start]) {
			continue
		}
		tail := delivered[start:]
		index := strings.LastIndex(t.canon, tail)
		if index < 0 {
			continue
		}
		resume := index + len(tail)
		if resume < diverged {
			// 锚点落在分叉点之前，说明这是一次巧合匹配。照它续写会把已发送
			// 内容再发一遍，直接放弃追补。
			continue
		}
		return t.canon[resume:]
	}
	return ""
}

// commonPrefixBytes 返回两段文本的公共前缀长度，切点保持在 UTF-8 边界上。
// 仅用于日志，用来量化一次重写改动了多少已发送内容。
func commonPrefixBytes(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	index := 0
	for index < limit && a[index] == b[index] {
		index++
	}
	for index > 0 && ((index < len(a) && !utf8.RuneStart(a[index])) || (index < len(b) && !utf8.RuneStart(b[index]))) {
		index--
	}
	return index
}

// textStream 是正文的唯一出口：它把「已经发给客户端的增量」与「上游快照真相」
// 分开保存，因此非前缀重写既不会丢内容，也不会造成复读。
type textStream struct {
	tracker snapshotTracker
	sent    strings.Builder
	deltas  []string
	emit    func(string) error
	// cursor 还原 streamingMode=Delta 的 writeAtCursor 片段。增量在入口处被归一
	// 成完整快照，因此 tracker、引用标记剥离与防复读逻辑对两种上游语义完全一致。
	cursor cursorAccumulator
}

func newTextStream(emit func(string) error) *textStream {
	return &textStream{emit: emit}
}

// delivered 返回已经发给客户端的全部正文。
func (s *textStream) delivered() string {
	if s == nil {
		return ""
	}
	return s.sent.String()
}

// joinedDeltas 保留原有的极端兜底取材方式。
func (s *textStream) joinedDeltas() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.deltas, "")
}

// pushDelta 下发一段增量。
func (s *textStream) pushDelta(delta string) error {
	if s == nil || delta == "" {
		return nil
	}
	var err error
	if s.emit != nil {
		err = s.emit(delta)
	}
	s.sent.WriteString(delta)
	s.deltas = append(s.deltas, delta)
	return err
}

// pushUpdateText 是生产路径上的正文入口：它按上游声明的语义分派。
//
// streamingMode=Delta 的 writeAtCursor 是增量片段，先由 cursorAccumulator 还原成
// 等价的完整快照，再走与快照完全相同的下发/校正路径。这样「最终文本 = 上游最后
// 一个完整快照」这条不变量对两种语义同时成立，混合到达时也只有一份正文真相。
func (s *textStream) pushUpdateText(item updateText) error {
	if s == nil || item.text == "" {
		return nil
	}
	if !item.delta {
		return s.pushSnapshot(item.text)
	}
	// 以 tracker 的 canon 作为基准：它是已知最完整的正文，包含此前到达的完整
	// 快照。光标写在正文末尾，因此 canon+delta 就是这一帧之后的完整正文。
	return s.pushSnapshot(s.cursor.push(s.tracker.canon, item.text))
}

// pushSnapshot 处理一个上游完整快照。
func (s *textStream) pushSnapshot(snapshot string) error {
	if s == nil || snapshot == "" {
		return nil
	}
	// 交付文本先剥掉私用区引用标记：内嵌标记对 OpenAI 兼容客户端是乱码，而整段
	// 只有标记的快照根本不是正文。剥离发生在快照入口，所以流式增量与最终文本
	// 必然一致。
	visible, markerOnly := visibleSnapshotText(snapshot)
	if markerOnly {
		// 纯引用标记快照是元数据：不下发，也不覆盖已收到的更完整正文。仍然记账
		// 以便观测。
		s.tracker.citeOnly++
		if chTrace {
			log.Printf("[trace:emitSnapshot] cite-only snapshot skipped: bytes=%d", len(snapshot))
		}
		return nil
	}
	if visible == "" {
		return nil
	}
	// 先记账，再决定下发。任何「不下发」的分支都不会再造成内容丢失。
	s.tracker.observe(visible)
	// 行内尾随空格延后一帧下发：它是唯一可能被后续剥离回收的字节，提前发出去会
	// 让后一个快照不再是已发送内容的前缀（详见 trimTrailingInlineSpace）。被按住
	// 的空格由下一个快照或 finalize 交出。
	snapshot = trimTrailingInlineSpace(visible)
	if snapshot == "" {
		return nil
	}
	cur := s.delivered()
	if cur == "" {
		// 开头的极短快照可能还在抖动，按住不发；finalize 会补齐，
		// 因此短回复（含上游内容过滤定型句）不再依赖 item.result.message。
		if len(snapshot) < snapshotStableBytes {
			return nil
		}
		return s.pushDelta(snapshot)
	}
	if chTrace {
		log.Printf("[trace:emitSnapshot] cur=%d snapshot=%d", len(cur), len(snapshot))
	}
	if strings.HasPrefix(snapshot, cur) {
		return s.pushDelta(snapshot[len(cur):])
	}
	// 非前缀重写。已发给客户端的增量不能撤回，因此这里只补发「锚点之后」确实
	// 新增的内容：被重写掉的前缀（如 `[hello.py`）无法在流上追回，把它接在后面
	// 只会得到 `](url)[hello.py` 这种更糟的顺序。剩下的不一致由完成帧用完整
	// 快照校正 Result.Text（finalize + canonicalText）。
	//
	// 原来这条 log 在正常路径上每次重写都打一遍（生产里 6439 次），现在降级到
	// chTrace；正常路径只在完成帧确认发生过内容不一致时告警一次。
	if chTrace {
		log.Printf("[trace:emitSnapshot] non-prefix rewrite: cur=%d snapshot=%d common_prefix=%d",
			len(cur), len(snapshot), commonPrefixBytes(snapshot, cur))
	}
	return s.pushDelta(s.tracker.undelivered(cur))
}

// finalize 在完成帧把尚未送达的正文补齐，只追加不会造成复读的内容。
func (s *textStream) finalize() error {
	if s == nil {
		return nil
	}
	return s.pushDelta(s.tracker.undelivered(s.delivered()))
}

// finalText 返回最终应当返回给客户端的完整正文。快照入口已经剥离过引用标记，
// 这里再兜一次是为了防止任何绕过 pushSnapshot 的取材路径漏出私用区字符；
// stripCiteMarkers 幂等，对已剥离的文本是零改动。
func (s *textStream) finalText() string {
	if s == nil {
		return ""
	}
	return stripCiteMarkers(s.tracker.canonicalText(s.delivered()))
}

// logReconciliation 保留这次定位问题的唯一线索，但只在真正发生过内容不一致时
// 于正常路径告警一次。
func (s *textStream) logReconciliation() {
	if s == nil {
		return
	}
	delivered := s.delivered()
	if s.tracker.diverged(delivered) {
		log.Printf("[emitSnapshot] final text reconciled from upstream snapshot: delivered=%d canonical=%d rewrites=%d shrinks=%d stale=%d cite_only=%d",
			len(delivered), len(s.tracker.canon), s.tracker.rewrites, s.tracker.shrinks, s.tracker.stale, s.tracker.citeOnly)
		return
	}
	if chTrace && (s.tracker.rewrites > 0 || s.tracker.shrinks > 0 || s.tracker.stale > 0 || s.tracker.citeOnly > 0 || s.cursor.deltas > 0) {
		log.Printf("[trace:emitSnapshot] rewrites=%d shrinks=%d stale=%d cite_only=%d cursor_deltas=%d converged=%d",
			s.tracker.rewrites, s.tracker.shrinks, s.tracker.stale, s.tracker.citeOnly, s.cursor.deltas, len(delivered))
	}
}

package chathub

import (
	"encoding/json"
	"strings"
)

// reasoningPump 把每一帧里新出现的思考文本即时推送出去。
//
// 为什么需要它：客户端（如 RikkaHub）按「第一个思考增量到最后一个思考增量
// 的时间差」计算思考时长。此前思考内容有两条来源，覆盖面不一致：
//
//	实时路径  只看 type=1 target=update 帧的 arguments[].messages
//	兜底路径  reasoningFromFrames 覆盖 arguments、arguments[].messages、
//	          item.messages、messages，且只在完成帧执行一次
//
// 差集里的思考内容（尤其挂在 item.messages 的）只会在完成帧被一次性补发，
// 客户端看到的首末增量几乎同时抵达，思考时长于是被算成 0。
//
// reasoningPump 用与兜底完全相同的取材逻辑（candidateMessages）逐帧扫描，
// 因此两条路径覆盖面一致；已发送过的前缀不再重复，保证增量语义。
// reasoningChunk 是一张思考卡片的当前累计文本。key 取自上游的 messageId，
// 缺失时为空并改用前缀归属。
type reasoningChunk struct {
	key  string
	text string
}

type reasoningPump struct {
	sent   strings.Builder
	emit   func(StreamEvent) error
	chunks []reasoningChunk
}

func newReasoningPump(emit func(StreamEvent) error) *reasoningPump {
	return &reasoningPump{emit: emit}
}

// push 扫描一帧，按出现顺序推送尚未发送过的思考片段。
func (p *reasoningPump) push(raw json.RawMessage) error {
	if p == nil {
		return nil
	}
	var frame map[string]any
	if json.Unmarshal(raw, &frame) != nil {
		return nil
	}
	for _, message := range candidateMessages(frame) {
		text, _ := message["text"].(string)
		if text == "" {
			continue
		}
		origin, _ := message["contentOrigin"].(string)
		addToChainOfThought, _ := message["addToChainOfThought"].(bool)
		if origin != "ChainOfThoughtSummary" && !addToChainOfThought {
			continue
		}
		// ChatHub 会重复投递同一张思考卡片（内容逐步增长），
		// 因此按「该卡片累计文本的新增后缀」推送，而不是整段重发。
		delta, grown := p.advance(chunkKey(message), text)
		if !grown || delta == "" {
			continue
		}
		p.sent.WriteString(delta)
		if p.emit == nil {
			continue
		}
		if err := p.emit(StreamEvent{Kind: "reasoning", Text: delta, Raw: raw}); err != nil {
			return err
		}
	}
	return nil
}

// chunkKey 取上游给这张卡片的稳定标识。ChatHub 的字段名在不同 variant 下不
// 统一，都试一遍；一个都没有就返回空串，由 advance 退回前缀归属。
func chunkKey(message map[string]any) string {
	for _, field := range []string{"messageId", "messageID", "id"} {
		if v, ok := message[field].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// advance 把一段文本归属到它所属的卡片，返回该卡片的新增后缀。
//
// 为什么必须按卡片记账：ChatHub 会交错重投多张各自增长的思考卡片。此前这里只有
// 一个扁平累加器 total，把所有卡片的文本首尾相接。一旦两张卡片交错，
// total 就再也不是任何一张卡片的前缀，于是每一次重投都被判成「全新内容」而整段
// 重发 —— 客户端看到成倍重复的思考文本，total 还在无界增长，前缀判断此后永远
// 命中不了。按卡片记账后，增长只与它自己的上一版比较。
func (p *reasoningPump) advance(key, text string) (string, bool) {
	if key != "" {
		for i := range p.chunks {
			if p.chunks[i].key == key {
				return growth(&p.chunks[i], text)
			}
		}
		p.chunks = append(p.chunks, reasoningChunk{key: key, text: text})
		return text, true
	}
	// 无标识时按前缀归属：能被这段文本延长的卡片就是它的来源，取匹配最长的一张。
	best := -1
	for i := range p.chunks {
		if p.chunks[i].key != "" {
			continue
		}
		// 完全相同或更短的重投是旧快照，丢弃。
		if strings.HasPrefix(p.chunks[i].text, text) {
			return "", false
		}
		if strings.HasPrefix(text, p.chunks[i].text) {
			if best < 0 || len(p.chunks[i].text) > len(p.chunks[best].text) {
				best = i
			}
		}
	}
	if best >= 0 {
		return growth(&p.chunks[best], text)
	}
	p.chunks = append(p.chunks, reasoningChunk{key: key, text: text})
	return text, true
}

// growth 比较一张卡片的新旧文本，返回应当推送的增量。
func growth(chunk *reasoningChunk, text string) (string, bool) {
	prev := chunk.text
	if strings.HasPrefix(prev, text) {
		// 相同或更短：重投的旧快照。
		return "", false
	}
	if strings.HasPrefix(text, prev) {
		delta := text[len(prev):]
		chunk.text = text
		return delta, true
	}
	// 整段被改写而非增长，无法判断哪部分是新的，只能整段推送。
	chunk.text = text
	return text, true
}

// text 返回已推送的全部思考内容，用作 Result.Reasoning，
// 避免完成帧再从原始帧重算而产生重复。
func (p *reasoningPump) text() string {
	if p == nil {
		return ""
	}
	return p.sent.String()
}

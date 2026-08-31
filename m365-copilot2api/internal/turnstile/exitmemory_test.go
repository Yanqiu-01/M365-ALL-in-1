package turnstile

import (
	"reflect"
	"sort"
	"testing"
)

// 最重要的一条：重排绝不能增删用户的出口。
func TestPreferProvenExitsIsAPermutation(t *testing.T) {
	ResetExitMemory()
	in := []string{"http://a:1", "socks5://b:2", "http://c:3", "http://d:4"}
	NoteExitTurnstileFailed("http://a:1")
	NoteExitTurnstileOK("http://c:3")

	out := PreferProvenExits(in)
	if len(out) != len(in) {
		t.Fatalf("长度变了：%d -> %d", len(in), len(out))
	}
	a, b := append([]string{}, in...), append([]string{}, out...)
	sort.Strings(a)
	sort.Strings(b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("集合变了：%v vs %v", a, b)
	}
	// 入参本身不能被改写。
	if in[0] != "http://a:1" || in[2] != "http://c:3" {
		t.Fatalf("入参被就地修改了：%v", in)
	}
}

func TestPreferProvenExitsOrdersByRecentOutcome(t *testing.T) {
	ResetExitMemory()
	in := []string{"http://failed:1", "http://unknown:2", "http://ok:3"}
	NoteExitTurnstileFailed("http://failed:1")
	NoteExitTurnstileOK("http://ok:3")

	out := PreferProvenExits(in)
	want := []string{"http://ok:3", "http://unknown:2", "http://failed:1"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("PreferProvenExits = %v, want %v", out, want)
	}
}

// 没有任何记录时行为必须和原来完全一致 —— 刚启动的那一轮不该被这份「经验」改变。
func TestPreferProvenExitsKeepsOrderWithoutMemory(t *testing.T) {
	ResetExitMemory()
	in := []string{"http://a:1", "http://b:2", "http://c:3"}
	if out := PreferProvenExits(in); !reflect.DeepEqual(out, in) {
		t.Fatalf("无记录时顺序变了：%v", out)
	}
}

// 同一档内保持原有顺序，避免同分项被任意打乱。
func TestPreferProvenExitsIsStableWithinRank(t *testing.T) {
	ResetExitMemory()
	in := []string{"http://u1:1", "http://ok:2", "http://u2:3", "http://u3:4"}
	NoteExitTurnstileOK("http://ok:2")
	out := PreferProvenExits(in)
	want := []string{"http://ok:2", "http://u1:1", "http://u2:3", "http://u3:4"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("同档顺序被打乱：%v, want %v", out, want)
	}
}

// 成功记录要能翻转先前的失败：出口的 CF 风险评级会变，不该被永久打入冷宫。
func TestNoteExitTurnstileOKClearsEarlierFailure(t *testing.T) {
	ResetExitMemory()
	NoteExitTurnstileFailed("http://x:1")
	if got := exitRank("http://x:1"); got != 2 {
		t.Fatalf("失败后 rank=%d, want 2", got)
	}
	NoteExitTurnstileOK("http://x:1")
	if got := exitRank("http://x:1"); got != 0 {
		t.Fatalf("成功后 rank=%d, want 0", got)
	}
}

// 同一档内 http 先于 socks5（实测 socks5 在 CF 这一段上明显更差），
// 但 socks5 必须仍然留在候选里 —— 只是排在后面。
func TestPreferProvenExitsPrefersHTTPWithinRank(t *testing.T) {
	ResetExitMemory()
	in := []string{"socks5://a:1", "http://b:2", "socks5h://c:3", "https://d:4"}
	out := PreferProvenExits(in)
	want := []string{"http://b:2", "https://d:4", "socks5://a:1", "socks5h://c:3"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("PreferProvenExits = %v, want %v", out, want)
	}
}

// 记录优先于 scheme：一个过过 CF 的 socks5 出口要排在没记录的 http 前面。
func TestPreferProvenExitsRanksMemoryAboveScheme(t *testing.T) {
	ResetExitMemory()
	NoteExitTurnstileOK("socks5://proven:1")
	in := []string{"http://unknown:2", "socks5://proven:1"}
	out := PreferProvenExits(in)
	want := []string{"socks5://proven:1", "http://unknown:2"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("PreferProvenExits = %v, want %v", out, want)
	}
}

func TestExitMemoryIgnoresBlankAndShortLists(t *testing.T) {
	ResetExitMemory()
	NoteExitTurnstileOK("   ")
	NoteExitTurnstileFailed("")
	if got := exitRank(""); got != 1 {
		t.Fatalf("空串 rank=%d, want 1", got)
	}
	if out := PreferProvenExits(nil); out != nil {
		t.Fatalf("nil 入参返回了 %v", out)
	}
	one := []string{"http://a:1"}
	if out := PreferProvenExits(one); !reflect.DeepEqual(out, one) {
		t.Fatalf("单元素返回了 %v", out)
	}
}

// 容量上限要真的挡住无界增长。
func TestExitMemoryBounded(t *testing.T) {
	ResetExitMemory()
	for i := 0; i < exitMemoryMaxItems*2; i++ {
		NoteExitTurnstileFailed("http://host:" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)))
	}
	exitMemory.Lock()
	n := len(exitMemory.byExit)
	exitMemory.Unlock()
	if n > exitMemoryMaxItems {
		t.Fatalf("记录数 %d 超过上限 %d", n, exitMemoryMaxItems)
	}
}

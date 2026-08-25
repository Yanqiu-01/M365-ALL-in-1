package chathub

import (
	"math/rand"
	"strings"
	"testing"
)

// 随机化不变量测试。复读是这次修复的硬禁止项，而复读只可能来自 undelivered
// 追补错误的位置，因此这里用随机快照序列（含大量非前缀重写）压这两条性质：
//
//	不丢内容  最终文本 == 上游最后一个完整快照
//	不复读    流上任何一段追补都不重复它之前已经发出的内容
func TestSnapshotStreamInvariantsUnderRandomRewrites(t *testing.T) {
	source := rand.New(rand.NewSource(20260823))
	alphabet := []rune("abcdefg 。，[]()你我他文件链接")

	randomText := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteRune(alphabet[source.Intn(len(alphabet))])
		}
		return b.String()
	}

	for iteration := 0; iteration < 2000; iteration++ {
		// 构造一串快照：多数是递增前缀，少数是非前缀重写（插入/删除/替换）。
		current := randomText(1 + source.Intn(120))
		snapshots := []string{current}
		for step := 0; step < 1+source.Intn(6); step++ {
			switch source.Intn(4) {
			case 0, 1: // 递增
				current += randomText(1 + source.Intn(40))
			case 2: // 在中间插入（挂附件 / 补引用的真实形状）
				if len(current) == 0 {
					continue
				}
				cut := runeBoundaryAt(current, source.Intn(len(current)))
				current = current[:cut] + randomText(1+source.Intn(20)) + current[cut:]
			case 3: // 删掉一段（把草稿换成精简终稿）
				if len(current) < 4 {
					continue
				}
				from := runeBoundaryAt(current, source.Intn(len(current)))
				to := runeBoundaryAt(current, from+source.Intn(len(current)-from))
				current = current[:from] + current[to:]
			}
			if current == "" {
				continue
			}
			// 「比上一版更短且是它的前缀」与「迟到的重放」在协议层无法区分：
			// ChatHub 的快照是递增的，重放确实会出现（旧实现的
			// len(snapshot) <= len(cur) 分支就是为它准备的）。tracker 因此把这种
			// 形状判定为重放并保留更完整的文本，见
			// TestStaleShorterSnapshotDoesNotTruncateFinalText。
			// 这里只生成可判定的序列，避免用一个不存在的上游形状去要求
			// 放弃重放保护。
			if strings.HasPrefix(snapshots[len(snapshots)-1], current) {
				current = snapshots[len(snapshots)-1]
				continue
			}
			snapshots = append(snapshots, current)
		}

		var emitted []string
		stream := newTextStream(func(delta string) error {
			emitted = append(emitted, delta)
			return nil
		})
		for _, snapshot := range snapshots {
			if err := stream.pushSnapshot(snapshot); err != nil {
				t.Fatalf("push snapshot: %v", err)
			}
		}
		if err := stream.finalize(); err != nil {
			t.Fatalf("finalize: %v", err)
		}

		last := snapshots[len(snapshots)-1]
		// 性质一：不丢内容。
		if got := stream.finalText(); got != last {
			t.Fatalf("iteration %d 最终文本丢内容\n snapshots=%q\n got=%q\n want=%q", iteration, snapshots, got, last)
		}
		// 性质二：不复读。每次追补都不得重复它之前已经发出的内容 ——
		// 逐次检查「新增量」是否已完整出现在此前已发送的文本里。
		var before strings.Builder
		for i, delta := range emitted {
			if i > 0 && len(delta) >= snapshotMinAnchorBytes && strings.Contains(before.String(), delta) {
				t.Fatalf("iteration %d 复读：增量[%d]=%q 已在此前发送的 %q 中\n snapshots=%q",
					iteration, i, delta, before.String(), snapshots)
			}
			before.WriteString(delta)
		}
	}
}

// runeBoundaryAt 把任意字节下标向下取整到 UTF-8 边界，保证构造出的随机快照
// 始终是合法字符串。
func runeBoundaryAt(text string, index int) int {
	if index > len(text) {
		index = len(text)
	}
	for index > 0 && index < len(text) && !isRuneStart(text[index]) {
		index--
	}
	return index
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

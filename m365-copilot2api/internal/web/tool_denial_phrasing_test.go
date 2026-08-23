package web

import "testing"

// The model denies tools it was actually handed by describing what "this turn"
// supposedly contains, naming the very tools in the declaration while claiming
// they are absent. Verbatim from a real session on 2026-08-24:
//
//	当前这一轮实际提供给我的工具中，仍没有 PowerShell、Read、Glob、TaskOutput、
//	Workflow、Monitor 或任务管理接口，只有隔离的容器工具，而该容器未映射 C:\ 和
//	E:\。因此，我现在不能执行真实的 Windows 文件读取
//
// Neither detector fired, so no correction round ran and the denial reached the
// client. Both lists have to cover this shape.
func TestToolDenialPhrasingIsDetected(t *testing.T) {
	reported := "当前这一轮实际提供给我的工具中，仍没有 PowerShell、Read、Glob、TaskOutput、" +
		"Workflow、Monitor 或任务管理接口，只有隔离的容器工具，而该容器未映射 C:\\ 和 E:\\。" +
		"因此，我现在不能执行真实的 Windows 文件读取，也不能自行连接一个未开放的工具。"

	if !isToolRefusal(reported) {
		t.Error("isToolRefusal missed the reported denial; no correction round would run")
	}
	if !isSandboxHallucination(reported) {
		t.Error("isSandboxHallucination missed the reported denial")
	}

	for _, variant := range []string{
		"The tools actually provided to me this turn do not include a shell.",
		"I only have isolated container tools available right now.",
		"该容器未映射 C:\\，所以读不到本地文件。",
	} {
		if !isToolRefusal(variant) && !isSandboxHallucination(variant) {
			t.Errorf("no detector matched variant %q", variant)
		}
	}
}

// Detectors must not fire on ordinary prose, or every answer triggers a wasted
// correction round against the upstream account.
func TestToolDenialDetectorsIgnoreNormalText(t *testing.T) {
	for _, ok := range []string{
		"I listed the files in E:\\download and found three directories.",
		"已经用 bash 工具读取了该目录，共 12 个文件。",
		"这个函数在 Windows 上会返回 0.5ms 精度的时间戳。",
	} {
		if isToolRefusal(ok) {
			t.Errorf("isToolRefusal false positive on %q", ok)
		}
		if isSandboxHallucination(ok) {
			t.Errorf("isSandboxHallucination false positive on %q", ok)
		}
	}
}
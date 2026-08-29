package web

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeWorkspaceInstructionNamesRealHostNotMntData(t *testing.T) {
	text := runtimeWorkspaceInstruction()
	if !strings.Contains(text, runtimeWorkspaceMarker) {
		t.Fatal("missing marker")
	}
	if !strings.Contains(text, "not /mnt/data") {
		t.Fatalf("must tell the model it is not in /mnt/data: %s", text)
	}
	if runtime.GOOS == "linux" && !strings.Contains(text, "Linux") {
		t.Fatalf("linux host must be named: %s", text)
	}
	if runtime.GOOS == "android" && !strings.Contains(text, "Android") {
		t.Fatalf("android host must be named: %s", text)
	}
	if runtime.GOOS == "windows" && !strings.Contains(text, "Windows") {
		t.Fatalf("windows host must be named: %s", text)
	}
}

func TestDescribeRuntimeHost(t *testing.T) {
	android := describeRuntimeHost("android", "arm64")
	if !strings.Contains(android, "Android") || !strings.Contains(android, "phone") {
		t.Fatalf("android host: %s", android)
	}
	windows := describeRuntimeHost("windows", "amd64")
	if !strings.Contains(windows, "Windows") || !strings.Contains(windows, "PC") {
		t.Fatalf("windows host: %s", windows)
	}
	linux := describeRuntimeHost("linux", "arm64")
	if !strings.Contains(linux, "RikkaHub") {
		t.Fatalf("linux host should mention RikkaHub: %s", linux)
	}
}

func TestEnsureRuntimeWorkspaceInstructionInsertsOnce(t *testing.T) {
	first := ensureRuntimeWorkspaceInstruction([]oaiMsg{{Role: "user", Content: "list files"}})
	if len(first) != 2 || first[0].Role != "system" || first[1].Role != "user" {
		t.Fatalf("first insert: %#v", first)
	}
	if !strings.Contains(fmt.Sprint(first[0].Content), runtimeWorkspaceMarker) {
		t.Fatalf("missing instruction: %#v", first[0])
	}
	second := ensureRuntimeWorkspaceInstruction(first)
	if len(second) != 2 {
		t.Fatalf("second insert duplicated the instruction: %#v", second)
	}
}

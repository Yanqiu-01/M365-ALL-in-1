package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebugLogMaxBytesBounds(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"", 64 << 20}, {"0", 64 << 20}, {"4097", 64 << 20}, {"bad", 64 << 20},
		{"-5", 64 << 20}, {"1", 1 << 20}, {"64", 64 << 20}, {"4096", 4096 << 20},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("M365_DEBUG_LOG_MAX_MB", test.value)
			if got := debugLogMaxBytes(); got != test.want {
				t.Fatalf("debugLogMaxBytes()=%d want %d", got, test.want)
			}
		})
	}
}

// The file was previously appended to without any bound. Rotation must cap it
// and keep exactly one previous generation.
func TestDebugStoreRotatesAtCap(t *testing.T) {
	t.Setenv("M365_DEBUG_LOG_MAX_MB", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "debug-logs.jsonl")
	d := &debugStore{path: path}

	if err := os.WriteFile(path, []byte(strings.Repeat("x", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.rotateLocked()
	d.mu.Unlock()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("oversized log survived rotation: err=%v", err)
	}
	previous, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("previous generation missing: %v", err)
	}
	if previous.Size() != (1<<20)+1 {
		t.Fatalf("previous generation size=%d", previous.Size())
	}
}

// Only one generation is kept: a second rotation must replace .1, not pile up.
func TestDebugStoreKeepsSingleGeneration(t *testing.T) {
	t.Setenv("M365_DEBUG_LOG_MAX_MB", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "debug-logs.jsonl")
	d := &debugStore{path: path}
	oversized := []byte(strings.Repeat("y", (1<<20)+1))

	for round := 0; round < 3; round++ {
		if err := os.WriteFile(path, oversized, 0o600); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		d.rotateLocked()
		d.mu.Unlock()
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "debug-logs.jsonl.1" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only the single previous generation, got %v", names)
	}
}

func TestDebugStoreLeavesSmallLogAlone(t *testing.T) {
	t.Setenv("M365_DEBUG_LOG_MAX_MB", "64")
	dir := t.TempDir()
	path := filepath.Join(dir, "debug-logs.jsonl")
	d := &debugStore{path: path}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.rotateLocked()
	d.mu.Unlock()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("under-cap log was rotated away: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("rotation happened below the cap")
	}
}

// A missing file must not panic or create anything.
func TestDebugStoreRotateHandlesMissingFile(t *testing.T) {
	dir := t.TempDir()
	d := &debugStore{path: filepath.Join(dir, "absent.jsonl")}
	d.mu.Lock()
	d.rotateLocked()
	d.mu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rotation created files for an absent log: %d", len(entries))
	}
}

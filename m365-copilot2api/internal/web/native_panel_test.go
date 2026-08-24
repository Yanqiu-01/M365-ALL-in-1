package web

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type nativePanelFakeRunner struct{ process *nativePanelFakeProcess }

type nativePanelFakeProcess struct {
	done   chan struct{}
	writer *io.PipeWriter
	once   sync.Once
	killed bool
}

func (r *nativePanelFakeRunner) Start(_ context.Context, _ nativePanelCommand) (nativePanelProcess, io.ReadCloser, error) {
	reader, writer := io.Pipe()
	r.process = &nativePanelFakeProcess{done: make(chan struct{}), writer: writer}
	return r.process, reader, nil
}

func (p *nativePanelFakeProcess) PID() int { return 1 }
func (p *nativePanelFakeProcess) Wait() (int, error) {
	<-p.done
	return 137, nil
}
func (p *nativePanelFakeProcess) KillTree() error {
	p.once.Do(func() {
		p.killed = true
		_ = p.writer.Close()
		close(p.done)
	})
	return nil
}

func TestNativePanelSingleJobStopAndPoll(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "oauth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"gateway":{"host":"127.0.0.1","port":4141}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "oauth", "oauth_batch.py"), []byte("# test worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &nativePanelFakeRunner{}
	manager := newNativePanelManager(nativePanelConfig{Root: root, Python: "fake-python", LogCap: 4, PollLogCap: 2}, runner)
	if _, err := manager.start("oauth:batch", "oauth/oauth_batch.py", nil, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !manager.snapshot().Running {
		t.Fatal("job should be running after start")
	}
	if _, err := manager.start("second", "oauth/oauth_batch.py", nil, nil); err != errNativePanelBusy {
		t.Fatalf("second start error = %v, want %v", err, errNativePanelBusy)
	}
	manager.stop()
	deadline := time.Now().Add(time.Second)
	for manager.snapshot().Running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if manager.snapshot().Running {
		t.Fatal("stop did not settle the job")
	}
	if runner.process == nil || !runner.process.killed {
		t.Fatal("stop did not cancel the worker process")
	}
	poll := manager.snapshot()
	if len(poll.Lines) != len(poll.Log) || len(poll.Lines) > 2 {
		t.Fatalf("unexpected poll log compatibility: %#v", poll)
	}
}

func TestNativePanelOriginAndBatchCompatibility(t *testing.T) {
	concurrent := false
	if !(nativePanelOAuthBatchRequest{Concurrent: &concurrent}).serial() {
		t.Fatal("concurrent=false from the existing 4141 UI must select serial mode")
	}
}

func TestPersistedNativePanelConfigPrefersEnvThenSettings(t *testing.T) {
	t.Setenv(nativePanelRootEnv, "")
	t.Setenv(nativePanelPythonEnv, "")
	server := &Server{settings: &settingsStore{v: runtimeSettings{
		NativePanelRoot:   filepath.Join("E:", "persisted", "worker"),
		NativePanelPython: filepath.Join("C:", "persisted", "python.exe"),
	}}}

	// A restart with no environment variables must still find the worker.
	saved := persistedNativePanelConfig(server)
	if saved.Root != filepath.Join("E:", "persisted", "worker") {
		t.Fatalf("persisted root not used: %q", saved.Root)
	}
	if saved.Python != filepath.Join("C:", "persisted", "python.exe") {
		t.Fatalf("persisted python not used: %q", saved.Python)
	}

	// An explicit environment override still wins over the stored value.
	t.Setenv(nativePanelRootEnv, filepath.Join("E:", "env", "worker"))
	if overridden := persistedNativePanelConfig(server); overridden.Root != filepath.Join("E:", "env", "worker") {
		t.Fatalf("environment override ignored: %q", overridden.Root)
	}

	// A server without settings must not panic and must keep the defaults.
	if fallback := persistedNativePanelConfig(nil); fallback.Python == "" {
		t.Fatal("nil server must still yield a usable python command")
	}
}

package exitrotate

// Regression tests for two ways a rotation reported success it had not achieved.
// Neither test reaches the real network: the Clash control plane and the IP echo are
// local httptest servers, and the CLI fallback test uses an unsupported mode so
// rotateGo returns before any probe.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects the standard logger into a buffer for the test.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buffer bytes.Buffer
	previousOut := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
	})
	return buffer.String
}

// stubIPEndpoint points the IP probe at a local server and restores the package
// globals afterwards, so test order cannot leak a stale endpoint.
func stubIPEndpoint(t *testing.T, ip string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, ip)
	}))
	previousEndpoints, previousSettle := ipEndpoints, clashSettle
	ipEndpoints, clashSettle = []string{server.URL}, 0
	t.Cleanup(func() {
		server.Close()
		ipEndpoints, clashSettle = previousEndpoints, previousSettle
	})
}

// clashServer accepts the node switch and records it.
func clashServer(t *testing.T, switched *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		*switched = body["name"]
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server
}

// A rotation that did not reach the expected exit must not report success.
//
// rotateClash returned OK:true and Changed:true on an ExpectIP mismatch, recording it
// only in Detail - which no caller reads. panel_register.go decides with
// `rotateErr == nil && !rotated.Changed`, so both signals said the rotation had
// worked, and the caller went on to register through an exit it had asked not to use.
func TestRotateClashFailsWhenTheExpectedExitIsNotReached(t *testing.T) {
	var switched string
	clash := clashServer(t, &switched)
	stubIPEndpoint(t, "9.9.9.9")

	result, err := rotateGo(context.Background(), Request{
		Mode:       "clash",
		ClashAPI:   clash.URL,
		ClashGroup: "PROXY",
		ClashNode:  "jp-01",
		PrevIP:     "1.1.1.1",
		ExpectIP:   "8.8.8.8",
	})
	if err == nil {
		t.Fatal("reaching an exit other than the expected one must be an error")
	}
	if result.OK {
		t.Fatalf("OK = true on an ExpectIP mismatch; result = %#v", result)
	}
	if result.Changed {
		t.Fatalf("Changed = true on an ExpectIP mismatch; the caller treats that as a completed rotation. result = %#v", result)
	}
	// The observed and the wanted exit both have to be recoverable from the result,
	// or the operator cannot tell which node answered.
	if result.IP != "9.9.9.9" {
		t.Fatalf("IP = %q, want the exit that actually answered", result.IP)
	}
	if !strings.Contains(result.Detail, "9.9.9.9") || !strings.Contains(result.Detail, "8.8.8.8") {
		t.Fatalf("Detail = %q, want both the observed and the expected exit", result.Detail)
	}
	if !strings.Contains(err.Error(), "8.8.8.8") {
		t.Fatalf("error = %v, want it to name the expected exit", err)
	}
	if switched != "jp-01" {
		t.Fatalf("switched = %q, want the node switch to have been attempted", switched)
	}
}

// The matching case is unchanged: reaching the expected exit is a success.
func TestRotateClashSucceedsWhenTheExpectedExitIsReached(t *testing.T) {
	var switched string
	clash := clashServer(t, &switched)
	stubIPEndpoint(t, "8.8.8.8")

	result, err := rotateGo(context.Background(), Request{
		Mode:       "clash",
		ClashAPI:   clash.URL,
		ClashGroup: "PROXY",
		ClashNode:  "jp-01",
		PrevIP:     "1.1.1.1",
		ExpectIP:   "8.8.8.8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.Changed || result.IP != "8.8.8.8" {
		t.Fatalf("result = %#v, want a successful rotation", result)
	}
	if result.Detail != "" {
		t.Fatalf("Detail = %q, want it empty when the expected exit was reached", result.Detail)
	}
}

// A broken CLI must be visible. The CLI-first path discarded its error with no log, so
// a build that cannot execute, or a JSON contract that drifted, degraded to the Go
// implementation and looked identical to a deployment that has no CLI installed.
func TestRotateLogsTheCLIFailureBeforeFallingBack(t *testing.T) {
	logged := captureLog(t)
	// A regular file that is not a runnable program: findCLI accepts it, exec fails.
	// It lives in the test's own temp dir and nothing outside it is written.
	broken := filepath.Join(t.TempDir(), "exit-rotate-broken")
	if err := os.WriteFile(broken, []byte("not an executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_EXIT_ROTATE", broken)
	if got := findCLI(); got != broken {
		t.Fatalf("findCLI = %q, want the stub at %q", got, broken)
	}

	// An unsupported mode makes the Go fallback fail deterministically without any
	// network access, so the test observes the fallback happening rather than its
	// result.
	if _, err := Rotate(context.Background(), Request{Mode: "browser"}); err == nil {
		t.Fatal("expected the Go fallback to reject an unsupported mode")
	}
	out := logged()
	if !strings.Contains(out, "exit-rotate cli") {
		t.Fatalf("the CLI failure was not logged at all: %q", out)
	}
	if !strings.Contains(out, "falling back") {
		t.Fatalf("the log does not say the run fell back to the Go implementation: %q", out)
	}
	if !strings.Contains(out, "browser") {
		t.Fatalf("the log does not name the mode that was attempted: %q", out)
	}
}

// The CLI succeeding stays silent and its result is used verbatim.
func TestRotateUsesTheCLIResultWhenItWorks(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no POSIX shell available to stand in for the CLI")
	}
	logged := captureLog(t)
	script := filepath.Join(t.TempDir(), "exit-rotate")
	body := "#!/bin/sh\nprintf '%s' '{\"ok\":true,\"mode\":\"clash\",\"ip\":\"5.5.5.5\",\"changed\":true}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_EXIT_ROTATE", script)

	result, err := Rotate(context.Background(), Request{Mode: "clash"})
	if err != nil {
		t.Skipf("the stub CLI is not executable in this environment: %v", err)
	}
	if result.IP != "5.5.5.5" || !result.OK {
		t.Fatalf("result = %#v, want the CLI's own answer", result)
	}
	if strings.Contains(logged(), "falling back") {
		t.Fatalf("a working CLI must not log a fallback: %q", logged())
	}
}

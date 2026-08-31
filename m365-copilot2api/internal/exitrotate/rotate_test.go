package exitrotate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCleanIPv4(t *testing.T) {
	if got := cleanIPv4(" 1.2.3.4\n"); got != "1.2.3.4" {
		t.Fatalf("got %q", got)
	}
	if got := cleanIPv4("<html>429</html>"); got != "" {
		t.Fatalf("garbage should be empty, got %q", got)
	}
}

func TestRotateClashSwitchesNodeThenProbesIP(t *testing.T) {
	var switched string
	clash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/proxies/PROXY" {
			t.Errorf("unexpected clash request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switched = body["name"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer clash.Close()

	ipServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "9.9.9.9")
	}))
	defer ipServer.Close()
	ipEndpoints = []string{ipServer.URL}
	clashSettle = 0

	// No ExpectIP: the switch is reported on its own terms.
	result, err := rotateGo(context.Background(), Request{
		Mode:        "clash",
		ClashAPI:    clash.URL,
		ClashSecret: "s3cret",
		ClashGroup:  "PROXY",
		ClashNode:   "jp-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if switched != "jp-01" {
		t.Fatalf("switched = %q", switched)
	}
	if result.IP != "9.9.9.9" || !result.OK {
		t.Fatalf("result = %#v", result)
	}

	// This case used to assert OK:true with the mismatch only in Detail. It now
	// asserts the opposite, because that was the defect: see
	// TestRotateClashFailsWhenTheExpectedExitIsNotReached.
	result, err = rotateGo(context.Background(), Request{
		Mode:        "clash",
		ClashAPI:    clash.URL,
		ClashSecret: "s3cret",
		ClashGroup:  "PROXY",
		ClashNode:   "jp-01",
		ExpectIP:    "8.8.8.8",
	})
	if err == nil {
		t.Fatalf("an ExpectIP mismatch must be an error, got result %#v", result)
	}
	if result.OK {
		t.Fatalf("result = %#v, want OK false", result)
	}
}

func TestRotateRejectsUnknownMode(t *testing.T) {
	if _, err := rotateGo(context.Background(), Request{Mode: "browser"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestUseLocalAirplaneOnAndroidNeverLooksForAdb(t *testing.T) {
	if useLocalAirplane() {
		if err := toggleAirplane(context.Background(), "adb"); err == nil {
			t.Fatal("expected local airplane-mode failure, not a silent adb success")
		} else if strings.Contains(err.Error(), "adb airplane-mode") {
			t.Fatalf("android/local path still called adb: %v", err)
		}
	}
}

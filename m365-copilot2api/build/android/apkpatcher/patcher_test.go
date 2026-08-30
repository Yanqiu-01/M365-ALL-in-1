package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSmaliPatchesOnOriginalAPK(t *testing.T) {
	src := filepath.Join("testdata", "decoded")
	if _, err := os.Stat(filepath.Join(src, "smali", "com", "m365", "gateway", "MainActivity.smali")); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	copyTree(t, src, work)

	if err := patchIdentity(work, "com.m365.gateway3", "com.m365.gateway.pkcego", "修改版M365", "2", "1.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := patchFileChooser(work); err != nil {
		t.Fatal(err)
	}
	if err := patchHideNativeChrome(work); err != nil {
		t.Fatal(err)
	}
	if err := patchTurnstileCapture(work); err != nil {
		t.Fatal(err)
	}
	if err := patchFlareSolver(work); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "admin.txt")
	if err := patchAdminPassword(work, secret); err != nil {
		t.Fatal(err)
	}

	main, err := readFile(filepath.Join(work, "smali", "com", "m365", "gateway", "MainActivity.smali"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := readFile(filepath.Join(work, "smali", "com", "m365", "gateway", "MainActivity$2.smali"))
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := readFile(filepath.Join(work, "smali", "com", "m365", "gateway", "MainActivity$NativeBridge.smali"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readFile(filepath.Join(work, "AndroidManifest.xml"))
	if err != nil {
		t.Fatal(err)
	}
	stringsXML, err := readFile(filepath.Join(work, "res", "values", "strings.xml"))
	if err != nil {
		t.Fatal(err)
	}

	for _, needle := range []string{"M365Native", "access$keepAlive", "access$web", "REQ_FILE", "filePathCallback"} {
		if !strings.Contains(main, needle) {
			t.Fatalf("MainActivity missing %s", needle)
		}
	}
	if strings.Contains(main, "invoke-virtual {p1, v2, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V") {
		t.Fatal("native chrome overlay still attached")
	}
	for _, needle := range []string{"office.965007.xyz", "onPageFinished", "invoke-virtual {p1}, Landroid/net/Uri;->getHost()Ljava/lang/String;"} {
		if !strings.Contains(client, needle) {
			t.Fatalf("WebViewClient missing %s", needle)
		}
	}
	cond2 := client[strings.Index(client, "    :cond_2\n"):]
	if strings.Contains(cond2, "invoke-virtual {p2, v0}, Ljava/lang/String;->endsWith") {
		t.Fatal("p2 reused as String after login.live.com clobbers the host register")
	}
	for _, needle := range []string{"openUrl", "captureTurnstile", "keepAlive"} {
		if !strings.Contains(bridge, needle) {
			t.Fatalf("NativeBridge missing %s", needle)
		}
	}
	if strings.Contains(bridge, "127.0.0.1:4141/#turnstile=") {
		t.Fatal("captureTurnstile must not navigate the dashboard WebView away from the register page")
	}
	if !strings.Contains(manifest, `package="com.m365.gateway.pkcego"`) {
		t.Fatal(manifest[:200])
	}
	if !strings.Contains(manifest, "android.permission.CHANGE_NETWORK_STATE") {
		t.Fatal("CHANGE_NETWORK_STATE missing")
	}
	if !strings.Contains(stringsXML, ">修改版M365<") {
		t.Fatal(stringsXML)
	}
	if !strings.Contains(stringsXML, ">***REMOVED-CREDENTIAL***<") {
		t.Fatal("admin password not written")
	}
	got, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != defaultAdminPassword {
		t.Fatalf("secret file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(work, "smali", "com", "m365", "gateway", "FlareSolver.smali")); err != nil {
		t.Fatal(err)
	}
	gw, err := readFile(filepath.Join(work, "smali", "com", "m365", "gateway", "GatewayService.smali"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gw, `const-string v8, "PATH"`) {
		t.Fatal("gateway process PATH was not set")
	}
	if !strings.Contains(main, "Lcom/m365/gateway/FlareSolver;-><init>") {
		t.Fatal("hidden FlareSolver WebView was not started")
	}
	flare, err := readFile(filepath.Join(work, "smali", "com", "m365", "gateway", "FlareSolver.smali"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flare, "setAcceptThirdPartyCookies") {
		t.Fatal("Turnstile WebView must accept third-party cookies")
	}
	if !strings.Contains(flare, "gw/data/flare/ready") {
		t.Fatal("ready marker missing")
	}
	if !strings.Contains(flare, "setTranslationX") {
		t.Fatal("solver WebView should stay off-screen until a job starts")
	}
	if !strings.Contains(flare, "showOverlay") || !strings.Contains(flare, "hideOverlay") {
		t.Fatal("solver must keep a paintable register WebView while filling")
	}
	if strings.Contains(flare, "bringToFront") {
		t.Fatal("register WebView must stay behind the dashboard")
	}
	if !strings.Contains(flare, "setTranslationZ") {
		t.Fatal("register WebView must sit under the dashboard")
	}
	if !strings.Contains(flare, "setClickable") {
		t.Fatal("background register WebView must not steal dashboard touches")
	}
	if strings.Contains(flare, "setAlpha") {
		t.Fatal("alpha=0 can stop WebView from painting Turnstile")
	}
	if !strings.Contains(main, "FlareSolver;->onBack()Z") {
		t.Fatal("back press must cancel solve instead of leaving the app")
	}
	clientJS, err := readFile(filepath.Join(work, "smali", "com", "m365", "gateway", "FlareSolver$Client.smali"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"displayName", "turnstileBox", "__m365Filled", "__m365Clicked", "submitBtn", "SUBMITTED:"} {
		if !strings.Contains(clientJS, needle) {
			t.Fatalf("solver script missing %s", needle)
		}
	}
}

func TestReplaceOnceRejectsAmbiguousNeedle(t *testing.T) {
	_, err := replaceOnce("aa", "a", "b", "dup")
	if err == nil {
		t.Fatal("expected error")
	}
}

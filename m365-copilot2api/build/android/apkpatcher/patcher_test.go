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
	if !strings.Contains(manifest, `package="com.m365.gateway.pkcego"`) {
		t.Fatal(manifest[:200])
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
}

func TestReplaceOnceRejectsAmbiguousNeedle(t *testing.T) {
	_, err := replaceOnce("aa", "a", "b", "dup")
	if err == nil {
		t.Fatal("expected error")
	}
}

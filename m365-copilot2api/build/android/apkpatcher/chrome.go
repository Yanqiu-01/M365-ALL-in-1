package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

func patchHideNativeChrome(work string) error {
	path := filepath.Join(work, "smali", "com", "m365", "gateway", "MainActivity.smali")
	source, err := readFile(path)
	if err != nil {
		return err
	}

	old := "    invoke-direct {v1, v6, v4}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V\n\n    .line 118\n    invoke-virtual {p1, v2, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V\n\n    .line 121\n    new-instance v1, Landroid/webkit/WebView;\n"
	new := "    invoke-direct {v1, v6, v4}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V\n\n    .line 121\n    new-instance v1, Landroid/webkit/WebView;\n"
	source, err = replaceOnce(source, old, new, "detach native chrome overlay")
	if err != nil {
		return err
	}

	bridge := "    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V\n\n    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;\n\n    new-instance v1, Lcom/m365/gateway/MainActivity$NativeBridge;\n\n    invoke-direct {v1, p0}, Lcom/m365/gateway/MainActivity$NativeBridge;-><init>(Lcom/m365/gateway/MainActivity;)V\n\n    const-string v2, \"M365Native\"\n\n    invoke-virtual {v0, v1, v2}, Landroid/webkit/WebView;->addJavascriptInterface(Ljava/lang/Object;Ljava/lang/String;)V\n"
	source, err = replaceOnce(
		source,
		"    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V\n",
		bridge,
		"install M365Native javascript bridge",
	)
	if err != nil {
		return err
	}

	if !strings.Contains(source, "access$keepAlive") {
		accessor := `
.method static synthetic access$keepAlive(Lcom/m365/gateway/MainActivity;)V
    .locals 0

    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->requestBatteryExemption()V

    return-void
.end method
`
		if !strings.Contains(source, ".method static synthetic access$400(Lcom/m365/gateway/MainActivity;Ljava/lang/String;)V") {
			return fmt.Errorf("access$400 not found")
		}
		source = strings.Replace(source, ".method static synthetic access$400(Lcom/m365/gateway/MainActivity;Ljava/lang/String;)V", accessor+"\n.method static synthetic access$400(Lcom/m365/gateway/MainActivity;Ljava/lang/String;)V", 1)
	}

	if err := writeFile(path, source); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(filepath.Dir(path), "MainActivity$NativeBridge.smali"), nativeBridgeSmali); err != nil {
		return err
	}

	verify, err := readFile(path)
	if err != nil {
		return err
	}
	if err := mustNotContain(verify, "    invoke-virtual {p1, v2, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V", "native chrome overlay"); err != nil {
		return err
	}
	if err := mustContain(verify, "M365Native", "javascript bridge"); err != nil {
		return err
	}
	if err := mustContain(verify, "access$keepAlive", "javascript bridge"); err != nil {
		return err
	}
	fmt.Printf("hid native chrome overlay: %s\n", path)
	fmt.Printf("created native bridge: %s\n", filepath.Join(filepath.Dir(path), "MainActivity$NativeBridge.smali"))
	return nil
}

const nativeBridgeSmali = `.class Lcom/m365/gateway/MainActivity$NativeBridge;
.super Ljava/lang/Object;
.source "MainActivity.java"

.field final synthetic this$0:Lcom/m365/gateway/MainActivity;

.method constructor <init>(Lcom/m365/gateway/MainActivity;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/MainActivity$NativeBridge;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public keepAlive()V
    .locals 1
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/MainActivity$NativeBridge;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-static {v0}, Lcom/m365/gateway/MainActivity;->access$keepAlive(Lcom/m365/gateway/MainActivity;)V

    return-void
.end method

.method public openDiag()V
    .locals 1
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/MainActivity$NativeBridge;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-static {v0}, Lcom/m365/gateway/DiagActivity;->start(Landroid/content/Context;)V

    return-void
.end method

.method public openTunnel()V
    .locals 1
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/MainActivity$NativeBridge;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-static {v0}, Lcom/m365/gateway/TunnelActivity;->start(Landroid/content/Context;)V

    return-void
.end method
`

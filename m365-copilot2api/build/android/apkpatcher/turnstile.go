package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

const watchJS = `(function(){if(window.__m365TsWatch)return;window.__m365TsWatch=1;function grab(){var el=document.querySelector('input[name=cf-turnstile-response]');var v=el&&el.value;if(v&&v.length>20){if(window.M365Native&&M365Native.captureTurnstile){M365Native.captureTurnstile(v);}else{location.href='http://127.0.0.1:4141/#turnstile='+encodeURIComponent(v);}return true;}return false;}if(grab())return;var n=0;var t=setInterval(function(){n++;if(grab()||n>240)clearInterval(t);},500);})();`

func javaString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

func patchTurnstileCapture(work string) error {
	packageDir := filepath.Join(work, "smali", "com", "m365", "gateway")
	clientPath := filepath.Join(packageDir, "MainActivity$2.smali")
	mainPath := filepath.Join(packageDir, "MainActivity.smali")
	bridgePath := filepath.Join(packageDir, "MainActivity$NativeBridge.smali")

	source, err := readFile(clientPath)
	if err != nil {
		return err
	}
	oldTry := "    :cond_2\n    :try_start_0\n    iget-object p2, p0, Lcom/m365/gateway/MainActivity$2;->this$0:Lcom/m365/gateway/MainActivity;\n\n    new-instance v0, Landroid/content/Intent;\n"
	newTry := `    :cond_2
    invoke-virtual {p1}, Landroid/net/Uri;->getHost()Ljava/lang/String;

    move-result-object v2

    if-eqz v2, :do_external

    const-string v0, "office.965007.xyz"

    invoke-virtual {v2, v0}, Ljava/lang/String;->endsWith(Ljava/lang/String;)Z

    move-result v0

    if-nez v0, :allow_in_webview

    const-string v0, "cloudflare.com"

    invoke-virtual {v2, v0}, Ljava/lang/String;->endsWith(Ljava/lang/String;)Z

    move-result v0

    if-nez v0, :allow_in_webview

    :do_external
    :try_start_0
    iget-object p2, p0, Lcom/m365/gateway/MainActivity$2;->this$0:Lcom/m365/gateway/MainActivity;

    new-instance v0, Landroid/content/Intent;
`
	source, err = replaceOnce(source, oldTry, newTry, "allow register host in WebView")
	if err != nil {
		return err
	}

	oldEnd := "    :cond_4\n    :goto_3\n    const/4 p1, 0x0\n\n    return p1\n.end method\n"
	newEnd := "    :allow_in_webview\n    const/4 p1, 0x0\n\n    return p1\n\n    :cond_4\n    :goto_3\n    const/4 p1, 0x0\n\n    return p1\n.end method\n"
	if !strings.Contains(source, "\n    :allow_in_webview\n") {
		source, err = replaceOnce(source, oldEnd, newEnd, "return in-webview for register host")
		if err != nil {
			return err
		}
	}

	if !strings.Contains(source, "onPageFinished") {
		source += "\n.method public onPageFinished(Landroid/webkit/WebView;Ljava/lang/String;)V\n" +
			"    .locals 2\n\n" +
			"    if-eqz p2, :done\n\n" +
			"    const-string v0, \"office.965007.xyz\"\n\n" +
			"    invoke-virtual {p2, v0}, Ljava/lang/String;->contains(Ljava/lang/CharSequence;)Z\n\n" +
			"    move-result v0\n\n" +
			"    if-eqz v0, :done\n\n" +
			"    const-string v0, \"" + javaString(watchJS) + "\"\n\n" +
			"    const/4 v1, 0x0\n\n" +
			"    invoke-virtual {p1, v0, v1}, Landroid/webkit/WebView;->evaluateJavascript(Ljava/lang/String;Landroid/webkit/ValueCallback;)V\n\n" +
			"    :done\n" +
			"    return-void\n" +
			".end method\n"
	}
	if err := writeFile(clientPath, source); err != nil {
		return err
	}

	mainSource, err := readFile(mainPath)
	if err != nil {
		return err
	}
	if !strings.Contains(mainSource, "access$web") {
		accessor := `
.method static synthetic access$web(Lcom/m365/gateway/MainActivity;)Landroid/webkit/WebView;
    .locals 1

    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    return-object v0
.end method
`
		needle := ".method static synthetic access$keepAlive(Lcom/m365/gateway/MainActivity;)V"
		if !strings.Contains(mainSource, needle) {
			return fmt.Errorf("access$keepAlive not found")
		}
		mainSource = strings.Replace(mainSource, needle, accessor+"\n"+needle, 1)
		if err := writeFile(mainPath, mainSource); err != nil {
			return err
		}
	}

	bridgeSource, err := readFile(bridgePath)
	if err != nil {
		return err
	}
	if !strings.Contains(bridgeSource, "openUrl") {
		bridgeSource += nativeBridgeURLSmali
		if err := writeFile(bridgePath, bridgeSource); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(packageDir, "MainActivity$NativeBridge$1.smali"), nativeBridgeRunnerSmali); err != nil {
		return err
	}

	verifyClient, err := readFile(clientPath)
	if err != nil {
		return err
	}
	verifyBridge, err := readFile(bridgePath)
	if err != nil {
		return err
	}
	if err := mustContain(verifyClient, "office.965007.xyz", "register host"); err != nil {
		return err
	}
	if err := mustContain(verifyClient, "onPageFinished", "onPageFinished"); err != nil {
		return err
	}
	if err := mustContain(verifyClient, "invoke-virtual {p1}, Landroid/net/Uri;->getHost()Ljava/lang/String;", "host re-read"); err != nil {
		return err
	}
	if err := mustContain(verifyBridge, "openUrl", "native bridge"); err != nil {
		return err
	}
	if err := mustContain(verifyBridge, "captureTurnstile", "native bridge"); err != nil {
		return err
	}
	fmt.Printf("patched turnstile capture: %s\n", clientPath)
	fmt.Printf("patched native bridge: %s\n", bridgePath)
	return nil
}

const nativeBridgeURLSmali = `
.method public openUrl(Ljava/lang/String;)V
    .locals 3
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    const-string v0, "https://office.965007.xyz"

    if-eqz p1, :use_default

    invoke-virtual {p1}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object p1

    invoke-virtual {p1}, Ljava/lang/String;->length()I

    move-result v1

    if-eqz v1, :use_default

    const-string v1, "https://"

    invoke-virtual {p1, v1}, Ljava/lang/String;->startsWith(Ljava/lang/String;)Z

    move-result v1

    if-nez v1, :use_arg

    const-string v1, "http://"

    invoke-virtual {p1, v1}, Ljava/lang/String;->startsWith(Ljava/lang/String;)Z

    move-result v1

    if-nez v1, :use_arg

    :use_default
    move-object p1, v0

    :use_arg
    iget-object v0, p0, Lcom/m365/gateway/MainActivity$NativeBridge;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-static {v0}, Lcom/m365/gateway/MainActivity;->access$web(Lcom/m365/gateway/MainActivity;)Landroid/webkit/WebView;

    move-result-object v0

    if-eqz v0, :done

    new-instance v1, Lcom/m365/gateway/MainActivity$NativeBridge$1;

    invoke-direct {v1, v0, p1}, Lcom/m365/gateway/MainActivity$NativeBridge$1;-><init>(Landroid/webkit/WebView;Ljava/lang/String;)V

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->post(Ljava/lang/Runnable;)Z

    :done
    return-void
.end method

.method public captureTurnstile(Ljava/lang/String;)V
    .locals 3
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    if-eqz p1, :done

    invoke-virtual {p1}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object p1

    invoke-virtual {p1}, Ljava/lang/String;->length()I

    move-result v0

    const/16 v1, 0x14

    if-le v0, v1, :done

    iget-object v0, p0, Lcom/m365/gateway/MainActivity$NativeBridge;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-static {v0}, Lcom/m365/gateway/MainActivity;->access$web(Lcom/m365/gateway/MainActivity;)Landroid/webkit/WebView;

    move-result-object v0

    if-eqz v0, :done

    new-instance v1, Ljava/lang/StringBuilder;

    invoke-direct {v1}, Ljava/lang/StringBuilder;-><init>()V

    const-string v2, "http://127.0.0.1:4141/#turnstile="

    invoke-virtual {v1, v2}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-static {p1}, Landroid/net/Uri;->encode(Ljava/lang/String;)Ljava/lang/String;

    move-result-object p1

    invoke-virtual {v1, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p1

    new-instance v1, Lcom/m365/gateway/MainActivity$NativeBridge$1;

    invoke-direct {v1, v0, p1}, Lcom/m365/gateway/MainActivity$NativeBridge$1;-><init>(Landroid/webkit/WebView;Ljava/lang/String;)V

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->post(Ljava/lang/Runnable;)Z

    :done
    return-void
.end method
`

const nativeBridgeRunnerSmali = `.class Lcom/m365/gateway/MainActivity$NativeBridge$1;
.super Ljava/lang/Object;
.source "MainActivity.java"

.implements Ljava/lang/Runnable;

.field final synthetic val$web:Landroid/webkit/WebView;
.field final synthetic val$url:Ljava/lang/String;

.method constructor <init>(Landroid/webkit/WebView;Ljava/lang/String;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/MainActivity$NativeBridge$1;->val$web:Landroid/webkit/WebView;

    iput-object p2, p0, Lcom/m365/gateway/MainActivity$NativeBridge$1;->val$url:Ljava/lang/String;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public run()V
    .locals 2

    iget-object v0, p0, Lcom/m365/gateway/MainActivity$NativeBridge$1;->val$web:Landroid/webkit/WebView;

    iget-object v1, p0, Lcom/m365/gateway/MainActivity$NativeBridge$1;->val$url:Ljava/lang/String;

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->loadUrl(Ljava/lang/String;)V

    return-void
.end method
`

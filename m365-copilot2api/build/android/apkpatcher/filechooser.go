package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

func patchFileChooser(work string) error {
	packageDir := filepath.Join(work, "smali", "com", "m365", "gateway")
	mainPath := filepath.Join(packageDir, "MainActivity.smali")
	source, err := readFile(mainPath)
	if err != nil {
		return err
	}

	if !strings.Contains(source, ".field private static final REQ_FILE:I") {
		source, err = replaceOnce(
			source,
			".field private static final REQ_AUTH:I = 0x14\n",
			".field private static final REQ_AUTH:I = 0x14\n\n.field private static final REQ_FILE:I = 0x15\n",
			"add REQ_FILE",
		)
		if err != nil {
			return err
		}
	}
	if !strings.Contains(source, ".field filePathCallback:Landroid/webkit/ValueCallback;") {
		source, err = replaceOnce(
			source,
			".field private volatile gatewayReady:Z\n",
			".field private volatile gatewayReady:Z\n\n.field filePathCallback:Landroid/webkit/ValueCallback;\n",
			"add filePathCallback",
		)
		if err != nil {
			return err
		}
	}

	oldChrome := "    new-instance v1, Landroid/webkit/WebChromeClient;\n\n    invoke-direct {v1}, Landroid/webkit/WebChromeClient;-><init>()V\n\n    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebChromeClient(Landroid/webkit/WebChromeClient;)V\n"
	newChrome := "    new-instance v1, Lcom/m365/gateway/MainActivity$3;\n\n    invoke-direct {v1, p0}, Lcom/m365/gateway/MainActivity$3;-><init>(Lcom/m365/gateway/MainActivity;)V\n\n    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebChromeClient(Landroid/webkit/WebChromeClient;)V\n"
	if !strings.Contains(source, "Lcom/m365/gateway/MainActivity$3;-><init>") {
		source, err = replaceOnce(source, oldChrome, newChrome, "install file chooser WebChromeClient")
		if err != nil {
			return err
		}
	}

	oldResult := "    const/16 v0, 0x14\n\n    if-eq p1, v0, :cond_0\n\n    return-void\n\n    :cond_0\n"
	newResult := "    const/16 v0, 0x15\n\n    if-ne p1, v0, :check_auth_result\n\n    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->filePathCallback:Landroid/webkit/ValueCallback;\n\n    if-eqz p1, :file_result_done\n\n    const/4 v0, 0x0\n\n    if-eqz p3, :deliver_file_result\n\n    invoke-virtual {p3}, Landroid/content/Intent;->getData()Landroid/net/Uri;\n\n    move-result-object p2\n\n    if-eqz p2, :deliver_file_result\n\n    const/4 p3, 0x1\n\n    new-array p3, p3, [Landroid/net/Uri;\n\n    const/4 v0, 0x0\n\n    aput-object p2, p3, v0\n\n    move-object v0, p3\n\n    :deliver_file_result\n    invoke-interface {p1, v0}, Landroid/webkit/ValueCallback;->onReceiveValue(Ljava/lang/Object;)V\n\n    const/4 p1, 0x0\n\n    iput-object p1, p0, Lcom/m365/gateway/MainActivity;->filePathCallback:Landroid/webkit/ValueCallback;\n\n    :file_result_done\n    return-void\n\n    :check_auth_result\n    const/16 v0, 0x14\n\n    if-eq p1, v0, :cond_0\n\n    return-void\n\n    :cond_0\n"
	if !strings.Contains(source, ":check_auth_result") {
		source, err = replaceOnce(source, oldResult, newResult, "route file picker result")
		if err != nil {
			return err
		}
	}
	if err := writeFile(mainPath, source); err != nil {
		return err
	}

	chromePath := filepath.Join(packageDir, "MainActivity$3.smali")
	if err := writeFile(chromePath, fileChooserChromeSmali); err != nil {
		return err
	}

	verify, err := readFile(mainPath)
	if err != nil {
		return err
	}
	for _, item := range []string{
		".field private static final REQ_FILE:I = 0x15",
		".field filePathCallback:Landroid/webkit/ValueCallback;",
		"Lcom/m365/gateway/MainActivity$3;-><init>",
		":check_auth_result",
	} {
		if err := mustContain(verify, item, "file chooser"); err != nil {
			return err
		}
	}
	fmt.Printf("patched Android WebView file chooser: %s\n", mainPath)
	fmt.Printf("created WebChromeClient: %s\n", chromePath)
	return nil
}

const fileChooserChromeSmali = `.class Lcom/m365/gateway/MainActivity$3;
.super Landroid/webkit/WebChromeClient;
.source "MainActivity.java"

.field final synthetic this$0:Lcom/m365/gateway/MainActivity;

.method constructor <init>(Lcom/m365/gateway/MainActivity;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/MainActivity$3;->this$0:Lcom/m365/gateway/MainActivity;

    invoke-direct {p0}, Landroid/webkit/WebChromeClient;-><init>()V

    return-void
.end method

.method public onShowFileChooser(Landroid/webkit/WebView;Landroid/webkit/ValueCallback;Landroid/webkit/WebChromeClient$FileChooserParams;)Z
    .locals 3

    iget-object p1, p0, Lcom/m365/gateway/MainActivity$3;->this$0:Lcom/m365/gateway/MainActivity;

    iput-object p2, p1, Lcom/m365/gateway/MainActivity;->filePathCallback:Landroid/webkit/ValueCallback;

    new-instance p2, Landroid/content/Intent;

    const-string p3, "android.intent.action.OPEN_DOCUMENT"

    invoke-direct {p2, p3}, Landroid/content/Intent;-><init>(Ljava/lang/String;)V

    const-string p3, "android.intent.category.OPENABLE"

    invoke-virtual {p2, p3}, Landroid/content/Intent;->addCategory(Ljava/lang/String;)Landroid/content/Intent;

    const-string p3, "image/*"

    invoke-virtual {p2, p3}, Landroid/content/Intent;->setType(Ljava/lang/String;)Landroid/content/Intent;

    const-string p3, "android.intent.extra.MIME_TYPES"

    const/4 v0, 0x4

    new-array v0, v0, [Ljava/lang/String;

    const/4 v1, 0x0

    const-string v2, "image/jpeg"

    aput-object v2, v0, v1

    const/4 v1, 0x1

    const-string v2, "image/png"

    aput-object v2, v0, v1

    const/4 v1, 0x2

    const-string v2, "image/webp"

    aput-object v2, v0, v1

    const/4 v1, 0x3

    const-string v2, "image/gif"

    aput-object v2, v0, v1

    invoke-virtual {p2, p3, v0}, Landroid/content/Intent;->putExtra(Ljava/lang/String;[Ljava/lang/String;)Landroid/content/Intent;

    const/16 p3, 0x15

    invoke-virtual {p1, p2, p3}, Lcom/m365/gateway/MainActivity;->startActivityForResult(Landroid/content/Intent;I)V

    const/4 p1, 0x1

    return p1
.end method
`

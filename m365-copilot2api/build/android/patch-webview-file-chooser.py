#!/usr/bin/env python3
"""Patch the decoded Android APK so HTML file inputs work inside MainActivity WebView.

The stock APK installs a plain WebChromeClient, which does not implement
onShowFileChooser. Consequently background-image and TXT import inputs receive
no Android document picker result. This patch installs a dedicated
WebChromeClient, stores the ValueCallback on MainActivity, and routes the
activity result back to the WebView.
"""
from __future__ import annotations

import pathlib
import sys

REQ_FILE = "0x15"


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: patch-webview-file-chooser.py <decoded-apk-root>")

    root = pathlib.Path(sys.argv[1]).resolve()
    package_dir = root / "smali" / "com" / "m365" / "gateway"
    main_path = package_dir / "MainActivity.smali"
    if not main_path.is_file():
        raise SystemExit(f"MainActivity.smali not found: {main_path}")

    source = main_path.read_text(encoding="utf-8")

    if ".field private static final REQ_FILE:I" not in source:
        source = replace_once(
            source,
            ".field private static final REQ_AUTH:I = 0x14\n",
            ".field private static final REQ_AUTH:I = 0x14\n\n.field private static final REQ_FILE:I = 0x15\n",
            "add REQ_FILE",
        )

    if ".field filePathCallback:Landroid/webkit/ValueCallback;" not in source:
        source = replace_once(
            source,
            ".field private volatile gatewayReady:Z\n",
            ".field private volatile gatewayReady:Z\n\n.field filePathCallback:Landroid/webkit/ValueCallback;\n",
            "add filePathCallback",
        )

    old_chrome = """    new-instance v1, Landroid/webkit/WebChromeClient;\n\n    invoke-direct {v1}, Landroid/webkit/WebChromeClient;-><init>()V\n\n    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebChromeClient(Landroid/webkit/WebChromeClient;)V\n"""
    new_chrome = """    new-instance v1, Lcom/m365/gateway/MainActivity$3;\n\n    invoke-direct {v1, p0}, Lcom/m365/gateway/MainActivity$3;-><init>(Lcom/m365/gateway/MainActivity;)V\n\n    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebChromeClient(Landroid/webkit/WebChromeClient;)V\n"""
    if "Lcom/m365/gateway/MainActivity$3;-><init>" not in source:
        source = replace_once(source, old_chrome, new_chrome, "install file chooser WebChromeClient")

    old_result = """    const/16 v0, 0x14\n\n    if-eq p1, v0, :cond_0\n\n    return-void\n\n    :cond_0\n"""
    new_result = """    const/16 v0, 0x15\n\n    if-ne p1, v0, :check_auth_result\n\n    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->filePathCallback:Landroid/webkit/ValueCallback;\n\n    if-eqz p1, :file_result_done\n\n    const/4 v0, 0x0\n\n    if-eqz p3, :deliver_file_result\n\n    invoke-virtual {p3}, Landroid/content/Intent;->getData()Landroid/net/Uri;\n\n    move-result-object p2\n\n    if-eqz p2, :deliver_file_result\n\n    const/4 p3, 0x1\n\n    new-array p3, p3, [Landroid/net/Uri;\n\n    const/4 v0, 0x0\n\n    aput-object p2, p3, v0\n\n    move-object v0, p3\n\n    :deliver_file_result\n    invoke-interface {p1, v0}, Landroid/webkit/ValueCallback;->onReceiveValue(Ljava/lang/Object;)V\n\n    const/4 p1, 0x0\n\n    iput-object p1, p0, Lcom/m365/gateway/MainActivity;->filePathCallback:Landroid/webkit/ValueCallback;\n\n    :file_result_done\n    return-void\n\n    :check_auth_result\n    const/16 v0, 0x14\n\n    if-eq p1, v0, :cond_0\n\n    return-void\n\n    :cond_0\n"""
    if ":check_auth_result" not in source:
        source = replace_once(source, old_result, new_result, "route file picker result")

    main_path.write_text(source, encoding="utf-8")

    chrome_path = package_dir / "MainActivity$3.smali"
    chrome_source = r'''.class Lcom/m365/gateway/MainActivity$3;
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
'''
    chrome_path.write_text(chrome_source, encoding="utf-8")

    verify = main_path.read_text(encoding="utf-8")
    required = (
        ".field private static final REQ_FILE:I = 0x15",
        ".field filePathCallback:Landroid/webkit/ValueCallback;",
        "Lcom/m365/gateway/MainActivity$3;-><init>",
        ":check_auth_result",
    )
    missing = [item for item in required if item not in verify]
    if missing:
        raise SystemExit(f"file chooser patch verification failed: {missing}")
    print(f"patched Android WebView file chooser: {main_path}")
    print(f"created WebChromeClient: {chrome_path}")


if __name__ == "__main__":
    main()

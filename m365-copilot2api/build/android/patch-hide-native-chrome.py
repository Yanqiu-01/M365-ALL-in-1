#!/usr/bin/env python3
"""Detach the native 96dp black chrome overlay from MainActivity.

The APK draws a dark LinearLayout above the WebView (title + 添加账号/保活/诊断/公网).
That bar covers the HTML workbench. This patch skips adding that overlay to the
root layout so the WebView is full-screen. Remaining actions live in Settings
and are reached through a JavascriptInterface named M365Native.
"""
from __future__ import annotations

import pathlib
import sys


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: patch-hide-native-chrome.py <decoded-apk-root>")
    root = pathlib.Path(sys.argv[1]).resolve()
    path = root / "smali" / "com" / "m365" / "gateway" / "MainActivity.smali"
    source = path.read_text(encoding="utf-8")

    old = """    invoke-direct {v1, v6, v4}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    .line 118
    invoke-virtual {p1, v2, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 121
    new-instance v1, Landroid/webkit/WebView;
"""
    new = """    invoke-direct {v1, v6, v4}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    .line 121
    new-instance v1, Landroid/webkit/WebView;
"""
    source = replace_once(source, old, new, "detach native chrome overlay")

    bridge = """    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V

    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    new-instance v1, Lcom/m365/gateway/MainActivity$NativeBridge;

    invoke-direct {v1, p0}, Lcom/m365/gateway/MainActivity$NativeBridge;-><init>(Lcom/m365/gateway/MainActivity;)V

    const-string v2, "M365Native"

    invoke-virtual {v0, v1, v2}, Landroid/webkit/WebView;->addJavascriptInterface(Ljava/lang/Object;Ljava/lang/String;)V
"""
    source = replace_once(
        source,
        "    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V\n",
        bridge,
        "install M365Native javascript bridge",
    )

    if "access$keepAlive" not in source:
        accessor = """
.method static synthetic access$keepAlive(Lcom/m365/gateway/MainActivity;)V
    .locals 0

    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->requestBatteryExemption()V

    return-void
.end method
"""
        source = source.replace(
            ".method static synthetic access$400(Lcom/m365/gateway/MainActivity;Ljava/lang/String;)V",
            accessor + "\n.method static synthetic access$400(Lcom/m365/gateway/MainActivity;Ljava/lang/String;)V",
            1,
        )

    path.write_text(source, encoding="utf-8")
    (path.parent / "MainActivity$NativeBridge.smali").write_text(
        r""".class Lcom/m365/gateway/MainActivity$NativeBridge;
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
""",
        encoding="utf-8",
    )

    verify = path.read_text(encoding="utf-8")
    if "invoke-virtual {p1, v2, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V" in verify:
        raise SystemExit("native chrome overlay still attached to root")
    if "M365Native" not in verify or "access$keepAlive" not in verify:
        raise SystemExit("javascript bridge was not installed")
    print(f"hid native chrome overlay: {path}")
    print(f"created native bridge: {path.parent / 'MainActivity$NativeBridge.smali'}")


if __name__ == "__main__":
    main()

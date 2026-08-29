#!/usr/bin/env python3
"""Keep the register site inside MainActivity WebView and capture Turnstile.

The stock WebViewClient sends any non-localhost host to the system browser, so
the operator never sees the Cloudflare widget. This patch:

  * lets office.965007.xyz and Cloudflare challenge hosts load in-app
  * injects a watcher that reads input[name=cf-turnstile-response]
  * returns to http://127.0.0.1:4141/#turnstile=...
  * exposes M365Native.openUrl / captureTurnstile to the dashboard

Dalvik: if-eqz = jump when zero/null/false; if-nez = jump when nonzero/true.
The login.live.com check clobbers p2 with a boolean, so the office/cloudflare
check must re-read the host from the Uri. Using p2 as a String after that
move-result is a VerifyError and crashes the activity on launch.
"""
from __future__ import annotations

import pathlib
import sys


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


WATCH_JS = (
    "(function(){if(window.__m365TsWatch)return;window.__m365TsWatch=1;"
    "function grab(){var el=document.querySelector('input[name=cf-turnstile-response]');"
    "var v=el&&el.value;if(v&&v.length>20){"
    "if(window.M365Native&&M365Native.captureTurnstile){M365Native.captureTurnstile(v);}"
    "else{location.href='http://127.0.0.1:4141/#turnstile='+encodeURIComponent(v);}"
    "return true;}return false;}"
    "if(grab())return;var n=0;var t=setInterval(function(){n++;if(grab()||n>240)clearInterval(t);},500);})();"
)


def java_string(value: str) -> str:
    return value.replace("\\", "\\\\").replace('"', '\\"')


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: patch-turnstile-capture.py <decoded-apk-root>")
    root = pathlib.Path(sys.argv[1]).resolve()
    package = root / "smali" / "com" / "m365" / "gateway"
    client = package / "MainActivity$2.smali"
    main_path = package / "MainActivity.smali"
    bridge = package / "MainActivity$NativeBridge.smali"
    if not client.is_file() or not main_path.is_file() or not bridge.is_file():
        raise SystemExit("required smali files missing")

    source = client.read_text(encoding="utf-8")
    old_try = """    :cond_2
    :try_start_0
    iget-object p2, p0, Lcom/m365/gateway/MainActivity$2;->this$0:Lcom/m365/gateway/MainActivity;

    new-instance v0, Landroid/content/Intent;
"""
    new_try = """    :cond_2
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
"""
    if old_try not in source:
        raise SystemExit("MainActivity$2 external-browser block not found")
    source = replace_once(source, old_try, new_try, "allow register host in WebView")

    old_end = """    :cond_4
    :goto_3
    const/4 p1, 0x0

    return p1
.end method
"""
    new_end = """    :allow_in_webview
    const/4 p1, 0x0

    return p1

    :cond_4
    :goto_3
    const/4 p1, 0x0

    return p1
.end method
"""
    if "\n    :allow_in_webview\n" not in source:
        if old_end not in source:
            raise SystemExit("MainActivity$2 method end not found")
        source = replace_once(source, old_end, new_end, "return in-webview for register host")

    if "onPageFinished" not in source:
        source += (
            "\n.method public onPageFinished(Landroid/webkit/WebView;Ljava/lang/String;)V\n"
            "    .locals 2\n\n"
            "    if-eqz p2, :done\n\n"
            "    const-string v0, \"office.965007.xyz\"\n\n"
            "    invoke-virtual {p2, v0}, Ljava/lang/String;->contains(Ljava/lang/CharSequence;)Z\n\n"
            "    move-result v0\n\n"
            "    if-eqz v0, :done\n\n"
            "    const-string v0, \"" + java_string(WATCH_JS) + "\"\n\n"
            "    const/4 v1, 0x0\n\n"
            "    invoke-virtual {p1, v0, v1}, Landroid/webkit/WebView;->evaluateJavascript(Ljava/lang/String;Landroid/webkit/ValueCallback;)V\n\n"
            "    :done\n"
            "    return-void\n"
            ".end method\n"
        )
    client.write_text(source, encoding="utf-8")

    main_source = main_path.read_text(encoding="utf-8")
    if "access$web" not in main_source:
        accessor = """
.method static synthetic access$web(Lcom/m365/gateway/MainActivity;)Landroid/webkit/WebView;
    .locals 1

    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    return-object v0
.end method
"""
        needle = ".method static synthetic access$keepAlive(Lcom/m365/gateway/MainActivity;)V"
        if needle not in main_source:
            raise SystemExit("access$keepAlive not found")
        main_source = main_source.replace(needle, accessor + "\n" + needle, 1)
        main_path.write_text(main_source, encoding="utf-8")

    bridge_source = bridge.read_text(encoding="utf-8")
    if "openUrl" not in bridge_source:
        bridge_source += r'''
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
'''
        bridge.write_text(bridge_source, encoding="utf-8")

    runner = package / "MainActivity$NativeBridge$1.smali"
    runner.write_text(
        r""".class Lcom/m365/gateway/MainActivity$NativeBridge$1;
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
""",
        encoding="utf-8",
    )

    verify_client = client.read_text(encoding="utf-8")
    verify_bridge = bridge.read_text(encoding="utf-8")
    if "office.965007.xyz" not in verify_client:
        raise SystemExit("register host was not allowed in WebView")
    if "onPageFinished" not in verify_client:
        raise SystemExit("onPageFinished was not installed")
    if "invoke-virtual {p1}, Landroid/net/Uri;->getHost()Ljava/lang/String;" not in verify_client:
        raise SystemExit("host must be re-read from Uri after login.live.com clobbers p2")
    if "openUrl" not in verify_bridge or "captureTurnstile" not in verify_bridge:
        raise SystemExit("native bridge methods missing")
    print(f"patched turnstile capture: {client}")
    print(f"patched native bridge: {bridge}")


if __name__ == "__main__":
    main()

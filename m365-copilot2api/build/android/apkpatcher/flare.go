package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

func patchFlareSolver(work string) error {
	packageDir := filepath.Join(work, "smali", "com", "m365", "gateway")
	mainPath := filepath.Join(packageDir, "MainActivity.smali")
	source, err := readFile(mainPath)
	if err != nil {
		return err
	}
	if !strings.Contains(source, ".field flare:Lcom/m365/gateway/FlareSolver;") {
		source, err = replaceOnce(
			source,
			".field private web:Landroid/webkit/WebView;\n",
			".field private web:Landroid/webkit/WebView;\n\n.field flare:Lcom/m365/gateway/FlareSolver;\n",
			"add flare field",
		)
		if err != nil {
			return err
		}
	}
	old := "    invoke-virtual {p0, p1}, Lcom/m365/gateway/MainActivity;->setContentView(Landroid/view/View;)V\n\n    .line 164\n    new-instance p1, Landroid/content/Intent;\n"
	new := "    invoke-virtual {p0, p1}, Lcom/m365/gateway/MainActivity;->setContentView(Landroid/view/View;)V\n\n    new-instance p1, Lcom/m365/gateway/FlareSolver;\n\n    invoke-direct {p1, p0}, Lcom/m365/gateway/FlareSolver;-><init>(Landroid/app/Activity;)V\n\n    iput-object p1, p0, Lcom/m365/gateway/MainActivity;->flare:Lcom/m365/gateway/FlareSolver;\n\n    invoke-virtual {p1}, Lcom/m365/gateway/FlareSolver;->start()V\n\n    .line 164\n    new-instance p1, Landroid/content/Intent;\n"
	if !strings.Contains(source, "Lcom/m365/gateway/FlareSolver;-><init>") {
		source, err = replaceOnce(source, old, new, "start hidden FlareSolver WebView")
		if err != nil {
			return err
		}
	}
	if !strings.Contains(source, "FlareSolver;->onBack()Z") {
		oldBack := ".method public onBackPressed()V\n    .locals 1\n\n    .line 375\n    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;\n"
		newBack := `.method public onBackPressed()V
    .locals 1

    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->flare:Lcom/m365/gateway/FlareSolver;

    if-eqz v0, :flare_skip

    invoke-virtual {v0}, Lcom/m365/gateway/FlareSolver;->onBack()Z

    move-result v0

    if-eqz v0, :flare_skip

    return-void

    :flare_skip
    .line 375
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;
`
		source, err = replaceOnce(source, oldBack, newBack, "intercept back during background solve")
		if err != nil {
			return err
		}
	}
	if err := writeFile(mainPath, source); err != nil {
		return err
	}

	files := map[string]string{
		"FlareSolver.smali":        flareSolverSmali,
		"FlareSolver$Bridge.smali": flareBridgeSmali,
		"FlareSolver$Client.smali": flareClientSmali(),
		"FlareSolver$Loop.smali":   flareLoopSmali,
		"FlareSolver$Load.smali":   flareLoadSmali,
		"FlareSolver$Hide.smali":   flareHideSmali,
		"FlareSolver$Tap.smali":    flareTapSmali,
	}
	for name, body := range files {
		if err := writeFile(filepath.Join(packageDir, name), body); err != nil {
			return err
		}
	}

	gwPath := filepath.Join(packageDir, "GatewayService.smali")
	gw, err := readFile(gwPath)
	if err != nil {
		return err
	}
	if !strings.Contains(gw, `"PATH"`) {
		home := "    const-string v8, \"HOME\"\n\n    invoke-virtual {v1}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;\n\n    move-result-object v9\n\n    invoke-interface {v7, v8, v9}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;\n"
		withPath := home + "\n    const-string v8, \"PATH\"\n\n    const-string v9, \"/system/bin:/system/xbin:/vendor/bin:/bin\"\n\n    invoke-interface {v7, v8, v9}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;\n"
		gw, err = replaceOnce(gw, home, withPath, "set PATH for gateway process")
		if err != nil {
			return err
		}
		if err := writeFile(gwPath, gw); err != nil {
			return err
		}
	}

	verify, err := readFile(mainPath)
	if err != nil {
		return err
	}
	if err := mustContain(verify, "Lcom/m365/gateway/FlareSolver;-><init>", "flare start"); err != nil {
		return err
	}
	if err := mustContain(verify, "FlareSolver;->onBack()Z", "back intercept"); err != nil {
		return err
	}
	manifestPath := filepath.Join(work, "AndroidManifest.xml")
	manifest, err := readFile(manifestPath)
	if err != nil {
		return err
	}
	if !strings.Contains(manifest, "android.permission.CHANGE_NETWORK_STATE") {
		manifest = strings.Replace(manifest, `    <uses-permission android:name="android.permission.INTERNET"/>`, `    <uses-permission android:name="android.permission.INTERNET"/>
    <uses-permission android:name="android.permission.CHANGE_NETWORK_STATE"/>`, 1)
		if err := writeFile(manifestPath, manifest); err != nil {
			return err
		}
	}

	fmt.Printf("installed built-in FlareSolver WebView: %s\n", filepath.Join(packageDir, "FlareSolver.smali"))
	return nil
}

const flareWatchJS = `(function(){if(window.__m365Flare)return;window.__m365Flare=1;function val(fn){try{return String(fn()||'')}catch(e){return ''}}function set(id,v){var el=document.getElementById(id);if(!el||!v||el.value===v)return;el.value=v;el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));}function layout(){if(window.__m365Laid)return;window.__m365Laid=1;var info=document.querySelector('.info-column');if(info)info.style.display='none';var shell=document.querySelector('.page-shell');if(shell){shell.style.display='block';shell.style.gridTemplateColumns='1fr';shell.style.width='100%';}}function ready(){var u=document.getElementById('username');var b=document.getElementById('submitBtn');return !!(u&&b&&!b.disabled)}function fill(){if(window.__m365Filled||!ready())return;var plan=document.getElementById('planId');if(plan&&plan.options&&plan.options.length&&!plan.value){plan.selectedIndex=0;plan.dispatchEvent(new Event('change',{bubbles:true}));}set('displayName',val(M365Flare.displayName));set('username',val(M365Flare.username));set('password',val(M365Flare.password));layout();window.__m365Filled=1;}function verified(){var el=document.querySelector('input[name=cf-turnstile-response]');var v=el&&el.value;return !!(v&&v.length>20)}function clickBox(){if(verified())return;if(window.__m365Clicked&&Date.now()-window.__m365Clicked<6000)return;var box=document.getElementById('turnstileBox');if(!box||box.classList.contains('hidden'))return;var t=box.querySelector('iframe')||box;var r=t.getBoundingClientRect();if(r.width<50||r.height<50)return;t.scrollIntoView({block:'center'});var x=r.left+28,y=r.top+r.height/2;try{if(M365Flare.tap)M365Flare.tap(x,y)}catch(e){}window.__m365Clicked=Date.now();}function submit(){if(window.__m365Submitted||!window.__m365Filled||!verified())return;var btn=document.getElementById('submitBtn');if(!btn||btn.disabled)return;var form=document.getElementById('registerForm');try{if(form&&form.requestSubmit)form.requestSubmit();else btn.click()}catch(e){try{btn.click()}catch(x){}}window.__m365Submitted=1;}function finish(){var msg=document.getElementById('message');var text=(msg&&(msg.textContent||'').trim())||'';var cls=(msg&&msg.className)||'';var btn=document.getElementById('submitBtn');if(/success/.test(cls)||/注册成功/.test(text)||(btn&&btn.classList.contains('login-ready'))){var m=text.match(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/);M365Flare.done('SUBMITTED:'+(m?m[0]:'registered-ok'));return true}if(/error/.test(cls)&&text.length>2&&!/正在创建/.test(text)){M365Flare.done('ERROR:'+text.slice(0,160));return true}return false}fill();setInterval(function(){fill();clickBox();submit();finish()},800)})();`

const flareSolverSmali = `.class public Lcom/m365/gateway/FlareSolver;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.field final activity:Landroid/app/Activity;
.field overlay:Landroid/widget/FrameLayout;
.field web:Landroid/webkit/WebView;
.field volatile running:Z
.field currentId:Ljava/lang/String;
.field currentDisplay:Ljava/lang/String;
.field currentUser:Ljava/lang/String;
.field currentPass:Ljava/lang/String;

.method public constructor <init>(Landroid/app/Activity;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public start()V
    .locals 7

    const/4 v0, 0x1

    iput-boolean v0, p0, Lcom/m365/gateway/FlareSolver;->running:Z

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    new-instance v0, Landroid/widget/FrameLayout;

    invoke-direct {v0, v1}, Landroid/widget/FrameLayout;-><init>(Landroid/content/Context;)V

    iput-object v0, p0, Lcom/m365/gateway/FlareSolver;->overlay:Landroid/widget/FrameLayout;

    const/4 v2, -0x1

    invoke-virtual {v0, v2}, Landroid/widget/FrameLayout;->setBackgroundColor(I)V

    const v2, 0x461c4000    # 10000.0f

    invoke-virtual {v0, v2}, Landroid/widget/FrameLayout;->setTranslationX(F)V

    new-instance v2, Landroid/webkit/WebView;

    invoke-direct {v2, v1}, Landroid/webkit/WebView;-><init>(Landroid/content/Context;)V

    iput-object v2, p0, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    invoke-virtual {v2}, Landroid/webkit/WebView;->getSettings()Landroid/webkit/WebSettings;

    move-result-object v3

    const/4 v4, 0x1

    invoke-virtual {v3, v4}, Landroid/webkit/WebSettings;->setJavaScriptEnabled(Z)V

    invoke-virtual {v3, v4}, Landroid/webkit/WebSettings;->setDomStorageEnabled(Z)V

    invoke-virtual {v3, v4}, Landroid/webkit/WebSettings;->setDatabaseEnabled(Z)V

    invoke-virtual {v3, v4}, Landroid/webkit/WebSettings;->setJavaScriptCanOpenWindowsAutomatically(Z)V

    const/4 v5, 0x0

    invoke-virtual {v3, v5}, Landroid/webkit/WebSettings;->setMediaPlaybackRequiresUserGesture(Z)V

    invoke-virtual {v3, v5}, Landroid/webkit/WebSettings;->setMixedContentMode(I)V

    const-string v5, "Mozilla/5.0 (Linux; Android 14; Mobile) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"

    invoke-virtual {v3, v5}, Landroid/webkit/WebSettings;->setUserAgentString(Ljava/lang/String;)V

    const/4 v5, 0x2

    const/4 v6, 0x0

    invoke-virtual {v2, v5, v6}, Landroid/webkit/WebView;->setLayerType(ILandroid/graphics/Paint;)V

    invoke-static {}, Landroid/webkit/CookieManager;->getInstance()Landroid/webkit/CookieManager;

    move-result-object v3

    invoke-virtual {v3, v4}, Landroid/webkit/CookieManager;->setAcceptCookie(Z)V

    invoke-virtual {v3, v2, v4}, Landroid/webkit/CookieManager;->setAcceptThirdPartyCookies(Landroid/webkit/WebView;Z)V

    new-instance v3, Lcom/m365/gateway/FlareSolver$Client;

    invoke-direct {v3, p0}, Lcom/m365/gateway/FlareSolver$Client;-><init>(Lcom/m365/gateway/FlareSolver;)V

    invoke-virtual {v2, v3}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V

    new-instance v3, Lcom/m365/gateway/FlareSolver$Bridge;

    invoke-direct {v3, p0}, Lcom/m365/gateway/FlareSolver$Bridge;-><init>(Lcom/m365/gateway/FlareSolver;)V

    const-string v4, "M365Flare"

    invoke-virtual {v2, v3, v4}, Landroid/webkit/WebView;->addJavascriptInterface(Ljava/lang/Object;Ljava/lang/String;)V

    new-instance v3, Landroid/widget/FrameLayout$LayoutParams;

    const/4 v4, -0x1

    const/4 v5, -0x1

    invoke-direct {v3, v4, v5}, Landroid/widget/FrameLayout$LayoutParams;-><init>(II)V

    invoke-virtual {v0, v2, v3}, Landroid/widget/FrameLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    new-instance v2, Landroid/widget/FrameLayout$LayoutParams;

    const/16 v4, 0x168

    const/16 v5, 0x280

    invoke-direct {v2, v4, v5}, Landroid/widget/FrameLayout$LayoutParams;-><init>(II)V

    const/16 v4, 0x55

    iput v4, v2, Landroid/widget/FrameLayout$LayoutParams;->gravity:I

    const/16 v4, 0x10

    iput v4, v2, Landroid/view/ViewGroup$MarginLayoutParams;->rightMargin:I

    iput v4, v2, Landroid/view/ViewGroup$MarginLayoutParams;->bottomMargin:I

    invoke-virtual {v1, v0, v2}, Landroid/app/Activity;->addContentView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    :try_start_0
    invoke-virtual {p0}, Lcom/m365/gateway/FlareSolver;->readyFile()Ljava/io/File;

    move-result-object v0

    const-string v1, "1\n"

    invoke-static {v0, v1}, Lcom/m365/gateway/FlareSolver;->writeFile(Ljava/io/File;Ljava/lang/String;)V
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    :catch_0
    new-instance v0, Ljava/lang/Thread;

    new-instance v1, Lcom/m365/gateway/FlareSolver$Loop;

    invoke-direct {v1, p0}, Lcom/m365/gateway/FlareSolver$Loop;-><init>(Lcom/m365/gateway/FlareSolver;)V

    invoke-direct {v0, v1}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;)V

    invoke-virtual {v0}, Ljava/lang/Thread;->start()V

    return-void
.end method

.method public onBack()Z
    .locals 3

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver;->currentId:Ljava/lang/String;

    if-eqz v0, :no

    new-instance v1, Ljava/lang/StringBuilder;

    invoke-direct {v1}, Ljava/lang/StringBuilder;-><init>()V

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    const-string v0, "\nCANCEL\n"

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    const/4 v1, 0x0

    iput-object v1, p0, Lcom/m365/gateway/FlareSolver;->currentId:Ljava/lang/String;

    :try_start_0
    invoke-virtual {p0}, Lcom/m365/gateway/FlareSolver;->resultFile()Ljava/io/File;

    move-result-object v1

    invoke-static {v1, v0}, Lcom/m365/gateway/FlareSolver;->writeFile(Ljava/io/File;Ljava/lang/String;)V
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    :catch_0
    invoke-virtual {p0}, Lcom/m365/gateway/FlareSolver;->hideOverlay()V

    const/4 v0, 0x1

    return v0

    :no
    const/4 v0, 0x0

    return v0
.end method

.method showOverlay()V
    .locals 2

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver;->overlay:Landroid/widget/FrameLayout;

    if-eqz v0, :done

    const/4 v1, 0x0

    invoke-virtual {v0, v1}, Landroid/widget/FrameLayout;->setTranslationX(F)V

    const v1, 0x41a00000    # 20.0f

    invoke-virtual {v0, v1}, Landroid/widget/FrameLayout;->setTranslationZ(F)V

    :done
    return-void
.end method

.method hideOverlay()V
    .locals 2

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver;->overlay:Landroid/widget/FrameLayout;

    if-eqz v0, :done

    const v1, 0x461c4000    # 10000.0f

    invoke-virtual {v0, v1}, Landroid/widget/FrameLayout;->setTranslationX(F)V

    :done
    return-void
.end method

.method public tapAt(FF)V
    .locals 11

    iget-object v8, p0, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    if-eqz v8, :done

    invoke-static {}, Landroid/os/SystemClock;->uptimeMillis()J

    move-result-wide v9

    move-wide v0, v9

    move-wide v2, v9

    const/4 v4, 0x0

    move v5, p1

    move v6, p2

    const/4 v7, 0x0

    invoke-static/range {v0 .. v7}, Landroid/view/MotionEvent;->obtain(JJIFFI)Landroid/view/MotionEvent;

    move-result-object v0

    invoke-virtual {v8, v0}, Landroid/webkit/WebView;->dispatchTouchEvent(Landroid/view/MotionEvent;)Z

    invoke-virtual {v0}, Landroid/view/MotionEvent;->recycle()V

    invoke-static {}, Landroid/os/SystemClock;->uptimeMillis()J

    move-result-wide v2

    move-wide v0, v9

    const/4 v4, 0x1

    move v5, p1

    move v6, p2

    const/4 v7, 0x0

    invoke-static/range {v0 .. v7}, Landroid/view/MotionEvent;->obtain(JJIFFI)Landroid/view/MotionEvent;

    move-result-object v0

    invoke-virtual {v8, v0}, Landroid/webkit/WebView;->dispatchTouchEvent(Landroid/view/MotionEvent;)Z

    invoke-virtual {v0}, Landroid/view/MotionEvent;->recycle()V

    :done
    return-void
.end method

.method jobFile()Ljava/io/File;
    .locals 3

    new-instance v0, Ljava/io/File;

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-virtual {v1}, Landroid/app/Activity;->getFilesDir()Ljava/io/File;

    move-result-object v1

    const-string v2, "gw/data/flare/job"

    invoke-direct {v0, v1, v2}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    return-object v0
.end method

.method resultFile()Ljava/io/File;
    .locals 3

    new-instance v0, Ljava/io/File;

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-virtual {v1}, Landroid/app/Activity;->getFilesDir()Ljava/io/File;

    move-result-object v1

    const-string v2, "gw/data/flare/result"

    invoke-direct {v0, v1, v2}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    return-object v0
.end method

.method readyFile()Ljava/io/File;
    .locals 3

    new-instance v0, Ljava/io/File;

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-virtual {v1}, Landroid/app/Activity;->getFilesDir()Ljava/io/File;

    move-result-object v1

    const-string v2, "gw/data/flare/ready"

    invoke-direct {v0, v1, v2}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    return-object v0
.end method

.method statusFile()Ljava/io/File;
    .locals 3

    new-instance v0, Ljava/io/File;

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-virtual {v1}, Landroid/app/Activity;->getFilesDir()Ljava/io/File;

    move-result-object v1

    const-string v2, "gw/data/flare/status"

    invoke-direct {v0, v1, v2}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    return-object v0
.end method

.method static readFile(Ljava/io/File;)Ljava/lang/String;
    .locals 5
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/io/IOException;
        }
    .end annotation

    new-instance v0, Ljava/io/FileInputStream;

    invoke-direct {v0, p0}, Ljava/io/FileInputStream;-><init>(Ljava/io/File;)V

    new-instance v1, Ljava/io/ByteArrayOutputStream;

    invoke-direct {v1}, Ljava/io/ByteArrayOutputStream;-><init>()V

    const/16 v2, 0x400

    new-array v2, v2, [B

    :loop
    invoke-virtual {v0, v2}, Ljava/io/FileInputStream;->read([B)I

    move-result v3

    if-lez v3, :done

    const/4 v4, 0x0

    invoke-virtual {v1, v2, v4, v3}, Ljava/io/ByteArrayOutputStream;->write([BII)V

    goto :loop

    :done
    invoke-virtual {v0}, Ljava/io/FileInputStream;->close()V

    const-string v0, "UTF-8"

    invoke-virtual {v1, v0}, Ljava/io/ByteArrayOutputStream;->toString(Ljava/lang/String;)Ljava/lang/String;

    move-result-object v0

    return-object v0
.end method

.method static writeFile(Ljava/io/File;Ljava/lang/String;)V
    .locals 2
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/io/IOException;
        }
    .end annotation

    invoke-virtual {p0}, Ljava/io/File;->getParentFile()Ljava/io/File;

    move-result-object v0

    if-eqz v0, :write

    invoke-virtual {v0}, Ljava/io/File;->mkdirs()Z

    :write
    new-instance v0, Ljava/io/FileOutputStream;

    invoke-direct {v0, p0}, Ljava/io/FileOutputStream;-><init>(Ljava/io/File;)V

    const-string v1, "UTF-8"

    invoke-virtual {p1, v1}, Ljava/lang/String;->getBytes(Ljava/lang/String;)[B

    move-result-object p1

    invoke-virtual {v0, p1}, Ljava/io/FileOutputStream;->write([B)V

    invoke-virtual {v0}, Ljava/io/FileOutputStream;->close()V

    return-void
.end method
`

const flareBridgeSmali = `.class Lcom/m365/gateway/FlareSolver$Bridge;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.field final synthetic this$0:Lcom/m365/gateway/FlareSolver;

.method constructor <init>(Lcom/m365/gateway/FlareSolver;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public displayName()Ljava/lang/String;
    .locals 1
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-object v0, v0, Lcom/m365/gateway/FlareSolver;->currentDisplay:Ljava/lang/String;

    if-eqz v0, :empty

    return-object v0

    :empty
    const-string v0, ""

    return-object v0
.end method

.method public username()Ljava/lang/String;
    .locals 1
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-object v0, v0, Lcom/m365/gateway/FlareSolver;->currentUser:Ljava/lang/String;

    if-eqz v0, :empty

    return-object v0

    :empty
    const-string v0, ""

    return-object v0
.end method

.method public password()Ljava/lang/String;
    .locals 1
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-object v0, v0, Lcom/m365/gateway/FlareSolver;->currentPass:Ljava/lang/String;

    if-eqz v0, :empty

    return-object v0

    :empty
    const-string v0, ""

    return-object v0
.end method

.method public tap(FF)V
    .locals 3
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-object v1, v0, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    if-eqz v1, :done

    new-instance v2, Lcom/m365/gateway/FlareSolver$Tap;

    invoke-direct {v2, v0, p1, p2}, Lcom/m365/gateway/FlareSolver$Tap;-><init>(Lcom/m365/gateway/FlareSolver;FF)V

    invoke-virtual {v1, v2}, Landroid/webkit/WebView;->post(Ljava/lang/Runnable;)Z

    :done
    return-void
.end method

.method public done(Ljava/lang/String;)V
    .locals 3
    .annotation runtime Landroid/webkit/JavascriptInterface;
    .end annotation

    if-eqz p1, :done

    invoke-virtual {p1}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object p1

    invoke-virtual {p1}, Ljava/lang/String;->length()I

    move-result v0

    const/4 v1, 0x6

    if-le v0, v1, :done

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-object v0, v0, Lcom/m365/gateway/FlareSolver;->currentId:Ljava/lang/String;

    if-eqz v0, :done

    new-instance v1, Ljava/lang/StringBuilder;

    invoke-direct {v1}, Ljava/lang/StringBuilder;-><init>()V

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    const-string v0, "\n"

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p1

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    const/4 v1, 0x0

    iput-object v1, v0, Lcom/m365/gateway/FlareSolver;->currentId:Ljava/lang/String;

    :try_start_0
    invoke-virtual {v0}, Lcom/m365/gateway/FlareSolver;->resultFile()Ljava/io/File;

    move-result-object v1

    invoke-static {v1, p1}, Lcom/m365/gateway/FlareSolver;->writeFile(Ljava/io/File;Ljava/lang/String;)V
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    :catch_0
    iget-object v1, v0, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    if-eqz v1, :done

    new-instance p1, Lcom/m365/gateway/FlareSolver$Hide;

    invoke-direct {p1, v0}, Lcom/m365/gateway/FlareSolver$Hide;-><init>(Lcom/m365/gateway/FlareSolver;)V

    invoke-virtual {v1, p1}, Landroid/webkit/WebView;->post(Ljava/lang/Runnable;)Z

    :done
    return-void
.end method
`

func flareClientSmali() string {
	return `.class Lcom/m365/gateway/FlareSolver$Client;
.super Landroid/webkit/WebViewClient;
.source "FlareSolver.java"

.field final synthetic this$0:Lcom/m365/gateway/FlareSolver;

.method constructor <init>(Lcom/m365/gateway/FlareSolver;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Client;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-direct {p0}, Landroid/webkit/WebViewClient;-><init>()V

    return-void
.end method

.method public shouldOverrideUrlLoading(Landroid/webkit/WebView;Landroid/webkit/WebResourceRequest;)Z
    .locals 0

    const/4 p1, 0x0

    return p1
.end method

.method public onPageFinished(Landroid/webkit/WebView;Ljava/lang/String;)V
    .locals 2

    const-string v0, "` + javaString(flareWatchJS) + `"

    const/4 v1, 0x0

    invoke-virtual {p1, v0, v1}, Landroid/webkit/WebView;->evaluateJavascript(Ljava/lang/String;Landroid/webkit/ValueCallback;)V

    :try_start_0
    iget-object p1, p0, Lcom/m365/gateway/FlareSolver$Client;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-virtual {p1}, Lcom/m365/gateway/FlareSolver;->statusFile()Ljava/io/File;

    move-result-object p1

    const-string v0, "loaded\n"

    invoke-static {p1, v0}, Lcom/m365/gateway/FlareSolver;->writeFile(Ljava/io/File;Ljava/lang/String;)V
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    :catch_0
    return-void
.end method
`
}

const flareLoopSmali = `.class Lcom/m365/gateway/FlareSolver$Loop;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.implements Ljava/lang/Runnable;

.field final synthetic this$0:Lcom/m365/gateway/FlareSolver;

.method constructor <init>(Lcom/m365/gateway/FlareSolver;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Loop;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public run()V
    .locals 6

    :loop
    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Loop;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-boolean v0, v0, Lcom/m365/gateway/FlareSolver;->running:Z

    if-eqz v0, :end

    :try_start_0
    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Loop;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-virtual {v0}, Lcom/m365/gateway/FlareSolver;->jobFile()Ljava/io/File;

    move-result-object v0

    invoke-virtual {v0}, Ljava/io/File;->exists()Z

    move-result v1

    if-eqz v1, :sleep

    invoke-static {v0}, Lcom/m365/gateway/FlareSolver;->readFile(Ljava/io/File;)Ljava/lang/String;

    move-result-object v1

    invoke-virtual {v0}, Ljava/io/File;->delete()Z

    const-string v0, "\n"

    invoke-virtual {v1, v0}, Ljava/lang/String;->split(Ljava/lang/String;)[Ljava/lang/String;

    move-result-object v0

    array-length v1, v0

    const/4 v2, 0x5

    if-lt v1, v2, :sleep

    const/4 v1, 0x0

    aget-object v1, v0, v1

    invoke-virtual {v1}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v1

    const/4 v2, 0x1

    aget-object v2, v0, v2

    invoke-virtual {v2}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v2

    const/4 v3, 0x2

    aget-object v3, v0, v3

    invoke-virtual {v3}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v3

    const/4 v4, 0x3

    aget-object v4, v0, v4

    invoke-virtual {v4}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v4

    const/4 v5, 0x4

    aget-object v0, v0, v5

    invoke-virtual {v0}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v0

    iget-object v5, p0, Lcom/m365/gateway/FlareSolver$Loop;->this$0:Lcom/m365/gateway/FlareSolver;

    iput-object v1, v5, Lcom/m365/gateway/FlareSolver;->currentId:Ljava/lang/String;

    iput-object v3, v5, Lcom/m365/gateway/FlareSolver;->currentDisplay:Ljava/lang/String;

    iput-object v4, v5, Lcom/m365/gateway/FlareSolver;->currentUser:Ljava/lang/String;

    iput-object v0, v5, Lcom/m365/gateway/FlareSolver;->currentPass:Ljava/lang/String;

    iget-object v1, v5, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    if-eqz v1, :sleep

    new-instance v0, Lcom/m365/gateway/FlareSolver$Load;

    invoke-direct {v0, v5, v2}, Lcom/m365/gateway/FlareSolver$Load;-><init>(Lcom/m365/gateway/FlareSolver;Ljava/lang/String;)V

    invoke-virtual {v1, v0}, Landroid/webkit/WebView;->post(Ljava/lang/Runnable;)Z
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    goto :sleep

    :catch_0
    :sleep
    const-wide/16 v0, 0x190

    :try_start_1
    invoke-static {v0, v1}, Ljava/lang/Thread;->sleep(J)V
    :try_end_1
    .catch Ljava/lang/InterruptedException; {:try_start_1 .. :try_end_1} :catch_1

    :catch_1
    goto :loop

    :end
    return-void
.end method
`

const flareLoadSmali = `.class Lcom/m365/gateway/FlareSolver$Load;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.implements Ljava/lang/Runnable;

.field final this$0:Lcom/m365/gateway/FlareSolver;
.field final url:Ljava/lang/String;

.method constructor <init>(Lcom/m365/gateway/FlareSolver;Ljava/lang/String;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Load;->this$0:Lcom/m365/gateway/FlareSolver;

    iput-object p2, p0, Lcom/m365/gateway/FlareSolver$Load;->url:Ljava/lang/String;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public run()V
    .locals 2

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Load;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-virtual {v0}, Lcom/m365/gateway/FlareSolver;->showOverlay()V

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Load;->this$0:Lcom/m365/gateway/FlareSolver;

    iget-object v0, v0, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    if-eqz v0, :done

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver$Load;->url:Ljava/lang/String;

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->loadUrl(Ljava/lang/String;)V

    :done
    return-void
.end method
`

const flareHideSmali = `.class Lcom/m365/gateway/FlareSolver$Hide;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.implements Ljava/lang/Runnable;

.field final this$0:Lcom/m365/gateway/FlareSolver;

.method constructor <init>(Lcom/m365/gateway/FlareSolver;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Hide;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public run()V
    .locals 1

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Hide;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-virtual {v0}, Lcom/m365/gateway/FlareSolver;->hideOverlay()V

    return-void
.end method
`

const flareTapSmali = `.class Lcom/m365/gateway/FlareSolver$Tap;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.implements Ljava/lang/Runnable;

.field final this$0:Lcom/m365/gateway/FlareSolver;
.field final x:F
.field final y:F

.method constructor <init>(Lcom/m365/gateway/FlareSolver;FF)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Tap;->this$0:Lcom/m365/gateway/FlareSolver;

    iput p2, p0, Lcom/m365/gateway/FlareSolver$Tap;->x:F

    iput p3, p0, Lcom/m365/gateway/FlareSolver$Tap;->y:F

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public run()V
    .locals 3

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Tap;->this$0:Lcom/m365/gateway/FlareSolver;

    iget v1, p0, Lcom/m365/gateway/FlareSolver$Tap;->x:F

    iget v2, p0, Lcom/m365/gateway/FlareSolver$Tap;->y:F

    invoke-virtual {v0, v1, v2}, Lcom/m365/gateway/FlareSolver;->tapAt(FF)V

    return-void
.end method
`

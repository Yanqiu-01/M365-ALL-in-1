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
	if err := writeFile(mainPath, source); err != nil {
		return err
	}

	files := map[string]string{
		"FlareSolver.smali":        flareSolverSmali,
		"FlareSolver$Bridge.smali": flareBridgeSmali,
		"FlareSolver$Client.smali": flareClientSmali(),
		"FlareSolver$Loop.smali":   flareLoopSmali,
		"FlareSolver$Load.smali":   flareLoadSmali,
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

const flareWatchJS = `(function(){if(window.__m365Flare)return;window.__m365Flare=1;function grab(){var el=document.querySelector('input[name=cf-turnstile-response]');var v=el&&el.value;if(v&&v.length>20){M365Flare.done(v);return true;}return false;}function poke(){var nodes=document.querySelectorAll('.cf-turnstile,#turnstileBox,iframe[src*="challenges.cloudflare.com"]');for(var i=0;i<nodes.length;i++){try{nodes[i].click();}catch(e){}}}poke();if(grab())return;var n=0;setInterval(function(){n++;poke();grab();},500);})();`

const flareSolverSmali = `.class public Lcom/m365/gateway/FlareSolver;
.super Ljava/lang/Object;
.source "FlareSolver.java"

.field final activity:Landroid/app/Activity;
.field web:Landroid/webkit/WebView;
.field volatile running:Z
.field currentId:Ljava/lang/String;

.method public constructor <init>(Landroid/app/Activity;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public start()V
    .locals 4

    const/4 v0, 0x1

    iput-boolean v0, p0, Lcom/m365/gateway/FlareSolver;->running:Z

    new-instance v0, Landroid/webkit/WebView;

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-direct {v0, v1}, Landroid/webkit/WebView;-><init>(Landroid/content/Context;)V

    iput-object v0, p0, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    invoke-virtual {v0}, Landroid/webkit/WebView;->getSettings()Landroid/webkit/WebSettings;

    move-result-object v1

    const/4 v2, 0x1

    invoke-virtual {v1, v2}, Landroid/webkit/WebSettings;->setJavaScriptEnabled(Z)V

    invoke-virtual {v1, v2}, Landroid/webkit/WebSettings;->setDomStorageEnabled(Z)V

    invoke-virtual {v1, v2}, Landroid/webkit/WebSettings;->setDatabaseEnabled(Z)V

    new-instance v1, Lcom/m365/gateway/FlareSolver$Client;

    invoke-direct {v1, p0}, Lcom/m365/gateway/FlareSolver$Client;-><init>(Lcom/m365/gateway/FlareSolver;)V

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V

    new-instance v1, Lcom/m365/gateway/FlareSolver$Bridge;

    invoke-direct {v1, p0}, Lcom/m365/gateway/FlareSolver$Bridge;-><init>(Lcom/m365/gateway/FlareSolver;)V

    const-string v2, "M365Flare"

    invoke-virtual {v0, v1, v2}, Landroid/webkit/WebView;->addJavascriptInterface(Ljava/lang/Object;Ljava/lang/String;)V

    new-instance v1, Landroid/widget/LinearLayout$LayoutParams;

    const/4 v2, 0x1

    const/4 v3, 0x1

    invoke-direct {v1, v2, v3}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    iget-object v2, p0, Lcom/m365/gateway/FlareSolver;->activity:Landroid/app/Activity;

    invoke-virtual {v2, v0, v1}, Landroid/app/Activity;->addContentView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    new-instance v0, Ljava/lang/Thread;

    new-instance v1, Lcom/m365/gateway/FlareSolver$Loop;

    invoke-direct {v1, p0}, Lcom/m365/gateway/FlareSolver$Loop;-><init>(Lcom/m365/gateway/FlareSolver;)V

    invoke-direct {v0, v1}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;)V

    invoke-virtual {v0}, Ljava/lang/Thread;->start()V

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

.method public done(Ljava/lang/String;)V
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

    :try_start_0
    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Bridge;->this$0:Lcom/m365/gateway/FlareSolver;

    invoke-virtual {v0}, Lcom/m365/gateway/FlareSolver;->resultFile()Ljava/io/File;

    move-result-object v0

    invoke-static {v0, p1}, Lcom/m365/gateway/FlareSolver;->writeFile(Ljava/io/File;Ljava/lang/String;)V
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    :catch_0
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

    const/4 v2, 0x2

    if-lt v1, v2, :sleep

    const/4 v1, 0x0

    aget-object v1, v0, v1

    invoke-virtual {v1}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v1

    const/4 v2, 0x1

    aget-object v0, v0, v2

    invoke-virtual {v0}, Ljava/lang/String;->trim()Ljava/lang/String;

    move-result-object v0

    iget-object v2, p0, Lcom/m365/gateway/FlareSolver$Loop;->this$0:Lcom/m365/gateway/FlareSolver;

    iput-object v1, v2, Lcom/m365/gateway/FlareSolver;->currentId:Ljava/lang/String;

    iget-object v1, v2, Lcom/m365/gateway/FlareSolver;->web:Landroid/webkit/WebView;

    if-eqz v1, :sleep

    new-instance v2, Lcom/m365/gateway/FlareSolver$Load;

    invoke-direct {v2, v1, v0}, Lcom/m365/gateway/FlareSolver$Load;-><init>(Landroid/webkit/WebView;Ljava/lang/String;)V

    invoke-virtual {v1, v2}, Landroid/webkit/WebView;->post(Ljava/lang/Runnable;)Z
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

.field final web:Landroid/webkit/WebView;
.field final url:Ljava/lang/String;

.method constructor <init>(Landroid/webkit/WebView;Ljava/lang/String;)V
    .locals 0

    iput-object p1, p0, Lcom/m365/gateway/FlareSolver$Load;->web:Landroid/webkit/WebView;

    iput-object p2, p0, Lcom/m365/gateway/FlareSolver$Load;->url:Ljava/lang/String;

    invoke-direct {p0}, Ljava/lang/Object;-><init>()V

    return-void
.end method

.method public run()V
    .locals 2

    iget-object v0, p0, Lcom/m365/gateway/FlareSolver$Load;->web:Landroid/webkit/WebView;

    iget-object v1, p0, Lcom/m365/gateway/FlareSolver$Load;->url:Ljava/lang/String;

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->loadUrl(Ljava/lang/String;)V

    return-void
.end method
`

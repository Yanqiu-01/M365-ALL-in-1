.class public Lcom/m365/gateway/MainActivity;
.super Landroid/app/Activity;
.source "MainActivity.java"


# static fields
.field private static final REQ_AUTH:I = 0x14


# instance fields
.field private volatile gatewayReady:Z

.field private status:Landroid/widget/TextView;

.field private final statusTicker:Ljava/lang/Runnable;

.field private final ui:Landroid/os/Handler;

.field private web:Landroid/webkit/WebView;


# direct methods
.method public constructor <init>()V
    .locals 2

    .line 40
    invoke-direct {p0}, Landroid/app/Activity;-><init>()V

    const/4 v0, 0x0

    .line 46
    iput-boolean v0, p0, Lcom/m365/gateway/MainActivity;->gatewayReady:Z

    .line 47
    new-instance v0, Landroid/os/Handler;

    invoke-static {}, Landroid/os/Looper;->getMainLooper()Landroid/os/Looper;

    move-result-object v1

    invoke-direct {v0, v1}, Landroid/os/Handler;-><init>(Landroid/os/Looper;)V

    iput-object v0, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    .line 48
    new-instance v0, Lcom/m365/gateway/MainActivity$1;

    invoke-direct {v0, p0}, Lcom/m365/gateway/MainActivity$1;-><init>(Lcom/m365/gateway/MainActivity;)V

    iput-object v0, p0, Lcom/m365/gateway/MainActivity;->statusTicker:Ljava/lang/Runnable;

    return-void
.end method

.method static synthetic access$000(Lcom/m365/gateway/MainActivity;)Z
    .locals 0

    .line 40
    iget-boolean p0, p0, Lcom/m365/gateway/MainActivity;->gatewayReady:Z

    return p0
.end method

.method static synthetic access$100(Lcom/m365/gateway/MainActivity;)Landroid/widget/TextView;
    .locals 0

    .line 40
    iget-object p0, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    return-object p0
.end method

.method static synthetic access$200(Lcom/m365/gateway/MainActivity;)Ljava/lang/String;
    .locals 0

    .line 40
    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->freezeHint()Ljava/lang/String;

    move-result-object p0

    return-object p0
.end method

.method static synthetic access$300(Lcom/m365/gateway/MainActivity;)Landroid/os/Handler;
    .locals 0

    .line 40
    iget-object p0, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    return-object p0
.end method

.method static synthetic access$400(Lcom/m365/gateway/MainActivity;Ljava/lang/String;)V
    .locals 0

    .line 40
    invoke-direct {p0, p1}, Lcom/m365/gateway/MainActivity;->openAuthActivity(Ljava/lang/String;)V

    return-void
.end method

.method private beginAuthorization()V
    .locals 3

    .line 174
    invoke-static {}, Lcom/m365/gateway/GatewayService;->portOpen()Z

    move-result v0

    if-nez v0, :cond_0

    .line 175
    const-string v0, "\u7f51\u5173\u5c1a\u672a\u5c31\u7eea\uff0c\u8bf7\u7a0d\u5019"

    const/4 v1, 0x0

    invoke-static {p0, v0, v1}, Landroid/widget/Toast;->makeText(Landroid/content/Context;Ljava/lang/CharSequence;I)Landroid/widget/Toast;

    move-result-object v0

    invoke-virtual {v0}, Landroid/widget/Toast;->show()V

    return-void

    .line 178
    :cond_0
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const-string v1, "\u6b63\u5728\u7533\u8bf7\u6388\u6743\u94fe\u63a5\u2026"

    invoke-virtual {v0, v1}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    .line 179
    new-instance v0, Ljava/lang/Thread;

    new-instance v1, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda0;

    invoke-direct {v1, p0}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda0;-><init>(Lcom/m365/gateway/MainActivity;)V

    const-string v2, "auth-start"

    invoke-direct {v0, v1, v2}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    .line 229
    invoke-virtual {v0}, Ljava/lang/Thread;->start()V

    return-void
.end method

.method private compactButton(Ljava/lang/String;)Landroid/widget/Button;
    .locals 4

    .line 317
    new-instance v0, Landroid/widget/Button;

    invoke-direct {v0, p0}, Landroid/widget/Button;-><init>(Landroid/content/Context;)V

    .line 318
    invoke-virtual {v0, p1}, Landroid/widget/Button;->setText(Ljava/lang/CharSequence;)V

    const/4 p1, 0x0

    .line 319
    invoke-virtual {v0, p1}, Landroid/widget/Button;->setAllCaps(Z)V

    const/high16 v1, 0x41500000    # 13.0f

    .line 320
    invoke-virtual {v0, v1}, Landroid/widget/Button;->setTextSize(F)V

    const/4 v1, -0x1

    .line 321
    invoke-virtual {v0, v1}, Landroid/widget/Button;->setTextColor(I)V

    .line 322
    invoke-virtual {v0, p1}, Landroid/widget/Button;->setMinHeight(I)V

    .line 323
    invoke-virtual {v0, p1}, Landroid/widget/Button;->setMinimumHeight(I)V

    .line 324
    invoke-virtual {v0, p1}, Landroid/widget/Button;->setMinWidth(I)V

    .line 325
    invoke-virtual {v0, p1}, Landroid/widget/Button;->setMinimumWidth(I)V

    const/4 v1, 0x6

    .line 326
    invoke-direct {p0, v1}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v2

    invoke-direct {p0, v1}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v1

    invoke-virtual {v0, v2, p1, v1, p1}, Landroid/widget/Button;->setPadding(IIII)V

    const/16 v1, 0x11

    .line 327
    invoke-virtual {v0, v1}, Landroid/widget/Button;->setGravity(I)V

    .line 328
    const-string v1, "#1E293B"

    invoke-static {v1}, Landroid/graphics/Color;->parseColor(Ljava/lang/String;)I

    move-result v1

    invoke-virtual {v0, v1}, Landroid/widget/Button;->setBackgroundColor(I)V

    .line 329
    new-instance v1, Landroid/widget/LinearLayout$LayoutParams;

    const/16 v2, 0x26

    .line 330
    invoke-direct {p0, v2}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v2

    const/high16 v3, 0x3f800000    # 1.0f

    invoke-direct {v1, p1, v2, v3}, Landroid/widget/LinearLayout$LayoutParams;-><init>(IIF)V

    const/4 v2, 0x3

    .line 331
    invoke-direct {p0, v2}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v3

    invoke-direct {p0, v2}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v2

    invoke-virtual {v1, v3, p1, v2, p1}, Landroid/widget/LinearLayout$LayoutParams;->setMargins(IIII)V

    .line 332
    invoke-virtual {v0, v1}, Landroid/widget/Button;->setLayoutParams(Landroid/view/ViewGroup$LayoutParams;)V

    return-object v0
.end method

.method private dp(I)I
    .locals 1

    int-to-float p1, p1

    .line 309
    invoke-virtual {p0}, Lcom/m365/gateway/MainActivity;->getResources()Landroid/content/res/Resources;

    move-result-object v0

    invoke-virtual {v0}, Landroid/content/res/Resources;->getDisplayMetrics()Landroid/util/DisplayMetrics;

    move-result-object v0

    iget v0, v0, Landroid/util/DisplayMetrics;->density:F

    mul-float/2addr p1, v0

    invoke-static {p1}, Ljava/lang/Math;->round(F)I

    move-result p1

    return p1
.end method

.method private freezeHint()Ljava/lang/String;
    .locals 10

    .line 284
    invoke-static {}, Lcom/m365/gateway/GatewayService;->sinceKeepAliveTick()J

    move-result-wide v0

    const-wide/16 v2, 0x0

    cmp-long v4, v0, v2

    .line 285
    const-string v5, "s \u524d"

    const-wide/16 v6, 0x3e8

    if-gez v4, :cond_0

    const-string v0, "tick \u672a\u6536\u5230"

    goto :goto_0

    :cond_0
    new-instance v4, Ljava/lang/StringBuilder;

    const-string v8, "tick "

    invoke-direct {v4, v8}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    div-long/2addr v0, v6

    invoke-virtual {v4, v0, v1}, Ljava/lang/StringBuilder;->append(J)Ljava/lang/StringBuilder;

    invoke-virtual {v4, v5}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v4}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    .line 286
    :goto_0
    invoke-static {}, Lcom/m365/gateway/GatewayService;->sinceHeartbeat()J

    move-result-wide v8

    cmp-long v1, v8, v2

    if-gez v1, :cond_1

    .line 288
    const-string v1, "\u5fc3\u8df3 \u672a\u5f00\u59cb"

    goto :goto_2

    .line 289
    :cond_1
    new-instance v1, Ljava/lang/StringBuilder;

    const-string v2, "\u5fc3\u8df3 "

    invoke-direct {v1, v2}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    div-long/2addr v8, v6

    invoke-virtual {v1, v8, v9}, Ljava/lang/StringBuilder;->append(J)Ljava/lang/StringBuilder;

    invoke-virtual {v1, v5}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-static {}, Lcom/m365/gateway/GatewayService;->heartbeatOk()Z

    move-result v2

    if-eqz v2, :cond_2

    const-string v2, ""

    goto :goto_1

    :cond_2
    const-string v2, "(\u5931\u8d25)"

    :goto_1
    invoke-virtual {v1, v2}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v1

    .line 290
    :goto_2
    new-instance v2, Ljava/lang/StringBuilder;

    const-string v3, " \u00b7 "

    invoke-direct {v2, v3}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-static {}, Lcom/m365/gateway/KeepAliveReceiver;->status()Ljava/lang/String;

    move-result-object v4

    invoke-virtual {v2, v4}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2, v3}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2, v3}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2, v1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    return-object v0
.end method

.method private openAuthActivity(Ljava/lang/String;)V
    .locals 2

    .line 233
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const-string v1, "\u8bf7\u5728\u767b\u5f55\u9875\u8f93\u5165\u8981\u6dfb\u52a0\u7684\u8d26\u53f7"

    invoke-virtual {v0, v1}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    .line 234
    new-instance v0, Landroid/content/Intent;

    const-class v1, Lcom/m365/gateway/AuthActivity;

    invoke-direct {v0, p0, v1}, Landroid/content/Intent;-><init>(Landroid/content/Context;Ljava/lang/Class;)V

    const-string v1, "authUrl"

    invoke-virtual {v0, v1, p1}, Landroid/content/Intent;->putExtra(Ljava/lang/String;Ljava/lang/String;)Landroid/content/Intent;

    move-result-object p1

    const/16 v0, 0x14

    .line 235
    invoke-virtual {p0, p1, v0}, Lcom/m365/gateway/MainActivity;->startActivityForResult(Landroid/content/Intent;I)V

    return-void
.end method

.method private pollUntilReady(I)V
    .locals 2

    .line 255
    new-instance v0, Ljava/lang/Thread;

    new-instance v1, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda2;

    invoke-direct {v1, p0, p1}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda2;-><init>(Lcom/m365/gateway/MainActivity;I)V

    const-string p1, "poll-ready"

    invoke-direct {v0, v1, p1}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    .line 275
    invoke-virtual {v0}, Ljava/lang/Thread;->start()V

    return-void
.end method

.method private requestBatteryExemption()V
    .locals 4

    const-string v0, "package:"

    .line 355
    :try_start_0
    const-string v1, "power"

    invoke-virtual {p0, v1}, Lcom/m365/gateway/MainActivity;->getSystemService(Ljava/lang/String;)Ljava/lang/Object;

    move-result-object v1

    check-cast v1, Landroid/os/PowerManager;

    if-eqz v1, :cond_0

    .line 356
    invoke-virtual {p0}, Lcom/m365/gateway/MainActivity;->getPackageName()Ljava/lang/String;

    move-result-object v2

    invoke-virtual {v1, v2}, Landroid/os/PowerManager;->isIgnoringBatteryOptimizations(Ljava/lang/String;)Z

    move-result v1

    if-eqz v1, :cond_0

    return-void

    .line 360
    :cond_0
    new-instance v1, Landroid/content/Intent;

    const-string v2, "android.settings.REQUEST_IGNORE_BATTERY_OPTIMIZATIONS"

    invoke-direct {v1, v2}, Landroid/content/Intent;-><init>(Ljava/lang/String;)V

    new-instance v2, Ljava/lang/StringBuilder;

    invoke-direct {v2, v0}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    .line 361
    invoke-virtual {p0}, Lcom/m365/gateway/MainActivity;->getPackageName()Ljava/lang/String;

    move-result-object v3

    invoke-virtual {v2, v3}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v2

    invoke-static {v2}, Landroid/net/Uri;->parse(Ljava/lang/String;)Landroid/net/Uri;

    move-result-object v2

    invoke-virtual {v1, v2}, Landroid/content/Intent;->setData(Landroid/net/Uri;)Landroid/content/Intent;

    move-result-object v1

    .line 362
    invoke-virtual {p0, v1}, Lcom/m365/gateway/MainActivity;->startActivity(Landroid/content/Intent;)V
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_0

    goto :goto_0

    .line 366
    :catch_0
    :try_start_1
    new-instance v1, Landroid/content/Intent;

    const-string v2, "android.settings.APPLICATION_DETAILS_SETTINGS"

    invoke-direct {v1, v2}, Landroid/content/Intent;-><init>(Ljava/lang/String;)V

    new-instance v2, Ljava/lang/StringBuilder;

    invoke-direct {v2, v0}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    .line 367
    invoke-virtual {p0}, Lcom/m365/gateway/MainActivity;->getPackageName()Ljava/lang/String;

    move-result-object v0

    invoke-virtual {v2, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    invoke-static {v0}, Landroid/net/Uri;->parse(Ljava/lang/String;)Landroid/net/Uri;

    move-result-object v0

    invoke-virtual {v1, v0}, Landroid/content/Intent;->setData(Landroid/net/Uri;)Landroid/content/Intent;

    move-result-object v0

    .line 366
    invoke-virtual {p0, v0}, Lcom/m365/gateway/MainActivity;->startActivity(Landroid/content/Intent;)V
    :try_end_1
    .catch Ljava/lang/Exception; {:try_start_1 .. :try_end_1} :catch_1

    :catch_1
    :goto_0
    return-void
.end method

.method private requestNotificationPermission()V
    .locals 2

    .line 339
    sget v0, Landroid/os/Build$VERSION;->SDK_INT:I

    const/16 v1, 0x21

    if-lt v0, v1, :cond_0

    .line 340
    const-string v0, "android.permission.POST_NOTIFICATIONS"

    invoke-virtual {p0, v0}, Lcom/m365/gateway/MainActivity;->checkSelfPermission(Ljava/lang/String;)I

    move-result v1

    if-eqz v1, :cond_0

    .line 341
    filled-new-array {v0}, [Ljava/lang/String;

    move-result-object v0

    const/16 v1, 0xa

    invoke-virtual {p0, v0, v1}, Lcom/m365/gateway/MainActivity;->requestPermissions([Ljava/lang/String;I)V

    :cond_0
    return-void
.end method


# virtual methods
.method synthetic lambda$beginAuthorization$4$com-m365-gateway-MainActivity(Ljava/lang/String;Ljava/lang/String;)V
    .locals 0

    .line 0
    if-eqz p1, :cond_0

    .line 223
    iget-object p2, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    invoke-virtual {p2, p1}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    const/4 p2, 0x1

    .line 224
    invoke-static {p0, p1, p2}, Landroid/widget/Toast;->makeText(Landroid/content/Context;Ljava/lang/CharSequence;I)Landroid/widget/Toast;

    move-result-object p1

    invoke-virtual {p1}, Landroid/widget/Toast;->show()V

    return-void

    .line 227
    :cond_0
    invoke-direct {p0, p2}, Lcom/m365/gateway/MainActivity;->openAuthActivity(Ljava/lang/String;)V

    return-void
.end method

.method synthetic lambda$beginAuthorization$5$com-m365-gateway-MainActivity()V
    .locals 9

    .line 0
    const/4 v0, 0x0

    .line 184
    :try_start_0
    new-instance v1, Ljava/net/URL;

    const-string v2, "http://127.0.0.1:4141/api/auth/start"

    invoke-direct {v1, v2}, Ljava/net/URL;-><init>(Ljava/lang/String;)V

    .line 185
    invoke-virtual {v1}, Ljava/net/URL;->openConnection()Ljava/net/URLConnection;

    move-result-object v1

    check-cast v1, Ljava/net/HttpURLConnection;
    :try_end_0
    .catch Ljava/lang/Exception; {:try_start_0 .. :try_end_0} :catch_2
    .catchall {:try_start_0 .. :try_end_0} :catchall_3

    const/16 v2, 0x2710

    .line 186
    :try_start_1
    invoke-virtual {v1, v2}, Ljava/net/HttpURLConnection;->setConnectTimeout(I)V

    const/16 v2, 0x4e20

    .line 187
    invoke-virtual {v1, v2}, Ljava/net/HttpURLConnection;->setReadTimeout(I)V

    .line 188
    invoke-static {}, Landroid/webkit/CookieManager;->getInstance()Landroid/webkit/CookieManager;

    move-result-object v2

    const-string v3, "http://127.0.0.1:4141"

    invoke-virtual {v2, v3}, Landroid/webkit/CookieManager;->getCookie(Ljava/lang/String;)Ljava/lang/String;

    move-result-object v2

    if-eqz v2, :cond_0

    .line 189
    invoke-virtual {v2}, Ljava/lang/String;->isEmpty()Z

    move-result v3

    if-nez v3, :cond_0

    .line 190
    const-string v3, "Cookie"

    invoke-virtual {v1, v3, v2}, Ljava/net/HttpURLConnection;->setRequestProperty(Ljava/lang/String;Ljava/lang/String;)V

    .line 192
    :cond_0
    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->getResponseCode()I

    move-result v2

    .line 193
    new-instance v3, Ljava/lang/StringBuilder;

    invoke-direct {v3}, Ljava/lang/StringBuilder;-><init>()V

    .line 194
    new-instance v4, Ljava/io/BufferedReader;

    new-instance v5, Ljava/io/InputStreamReader;

    const/16 v6, 0x190

    if-lt v2, v6, :cond_1

    .line 195
    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->getErrorStream()Ljava/io/InputStream;

    move-result-object v6

    if-eqz v6, :cond_1

    .line 196
    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->getErrorStream()Ljava/io/InputStream;

    move-result-object v6

    goto :goto_0

    :cond_1
    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->getInputStream()Ljava/io/InputStream;

    move-result-object v6

    :goto_0
    const-string v7, "UTF-8"

    invoke-direct {v5, v6, v7}, Ljava/io/InputStreamReader;-><init>(Ljava/io/InputStream;Ljava/lang/String;)V

    invoke-direct {v4, v5}, Ljava/io/BufferedReader;-><init>(Ljava/io/Reader;)V
    :try_end_1
    .catch Ljava/lang/Exception; {:try_start_1 .. :try_end_1} :catch_1
    .catchall {:try_start_1 .. :try_end_1} :catchall_2

    .line 198
    :goto_1
    :try_start_2
    invoke-virtual {v4}, Ljava/io/BufferedReader;->readLine()Ljava/lang/String;

    move-result-object v5

    if-eqz v5, :cond_2

    .line 199
    invoke-virtual {v3, v5}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;
    :try_end_2
    .catchall {:try_start_2 .. :try_end_2} :catchall_0

    goto :goto_1

    .line 201
    :cond_2
    :try_start_3
    invoke-virtual {v4}, Ljava/io/BufferedReader;->close()V

    const/16 v4, 0x191

    if-ne v2, v4, :cond_3

    .line 203
    const-string v2, "\u8bf7\u5148\u5728\u63a7\u5236\u53f0\u767b\u5f55\u7ba1\u7406\u5458\u5bc6\u7801"

    goto :goto_3

    :cond_3
    const/16 v4, 0xc8

    if-lt v2, v4, :cond_6

    const/16 v4, 0x12c

    if-lt v2, v4, :cond_4

    goto :goto_2

    .line 207
    :cond_4
    new-instance v2, Lorg/json/JSONObject;

    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v3

    invoke-direct {v2, v3}, Lorg/json/JSONObject;-><init>(Ljava/lang/String;)V

    const-string v3, "url"

    const-string v4, ""

    invoke-virtual {v2, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;

    move-result-object v2
    :try_end_3
    .catch Ljava/lang/Exception; {:try_start_3 .. :try_end_3} :catch_1
    .catchall {:try_start_3 .. :try_end_3} :catchall_2

    .line 208
    :try_start_4
    invoke-virtual {v2}, Ljava/lang/String;->isEmpty()Z

    move-result v3

    if-eqz v3, :cond_5

    .line 209
    const-string v0, "\u7f51\u5173\u672a\u8fd4\u56de\u6388\u6743\u94fe\u63a5"
    :try_end_4
    .catch Ljava/lang/Exception; {:try_start_4 .. :try_end_4} :catch_0
    .catchall {:try_start_4 .. :try_end_4} :catchall_2

    :cond_5
    move-object v8, v2

    move-object v2, v0

    move-object v0, v8

    goto :goto_3

    :catch_0
    move-exception v0

    move-object v8, v1

    move-object v1, v0

    move-object v0, v8

    goto :goto_5

    .line 205
    :cond_6
    :goto_2
    :try_start_5
    new-instance v3, Ljava/lang/StringBuilder;

    invoke-direct {v3}, Ljava/lang/StringBuilder;-><init>()V

    const-string v4, "\u83b7\u53d6\u6388\u6743\u94fe\u63a5\u5931\u8d25 (HTTP "

    invoke-virtual {v3, v4}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3, v2}, Ljava/lang/StringBuilder;->append(I)Ljava/lang/StringBuilder;

    const-string v2, ")"

    invoke-virtual {v3, v2}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v2
    :try_end_5
    .catch Ljava/lang/Exception; {:try_start_5 .. :try_end_5} :catch_1
    .catchall {:try_start_5 .. :try_end_5} :catchall_2

    :goto_3
    if-eqz v1, :cond_8

    .line 216
    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->disconnect()V

    goto :goto_6

    :catchall_0
    move-exception v2

    .line 194
    :try_start_6
    invoke-virtual {v4}, Ljava/io/BufferedReader;->close()V
    :try_end_6
    .catchall {:try_start_6 .. :try_end_6} :catchall_1

    goto :goto_4

    :catchall_1
    move-exception v3

    :try_start_7
    invoke-virtual {v2, v3}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :goto_4
    throw v2
    :try_end_7
    .catch Ljava/lang/Exception; {:try_start_7 .. :try_end_7} :catch_1
    .catchall {:try_start_7 .. :try_end_7} :catchall_2

    :catchall_2
    move-exception v0

    goto :goto_7

    :catch_1
    move-exception v2

    move-object v8, v2

    move-object v2, v0

    move-object v0, v1

    move-object v1, v8

    goto :goto_5

    :catchall_3
    move-exception v1

    move-object v8, v1

    move-object v1, v0

    move-object v0, v8

    goto :goto_7

    :catch_2
    move-exception v2

    move-object v1, v2

    move-object v2, v0

    .line 213
    :goto_5
    :try_start_8
    new-instance v3, Ljava/lang/StringBuilder;

    invoke-direct {v3}, Ljava/lang/StringBuilder;-><init>()V

    const-string v4, "\u83b7\u53d6\u6388\u6743\u94fe\u63a5\u5931\u8d25: "

    invoke-virtual {v3, v4}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/Exception;->getMessage()Ljava/lang/String;

    move-result-object v1

    invoke-virtual {v3, v1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v1
    :try_end_8
    .catchall {:try_start_8 .. :try_end_8} :catchall_3

    if-eqz v0, :cond_7

    .line 216
    invoke-virtual {v0}, Ljava/net/HttpURLConnection;->disconnect()V

    :cond_7
    move-object v0, v2

    move-object v2, v1

    .line 221
    :cond_8
    :goto_6
    iget-object v1, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    new-instance v3, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda8;

    invoke-direct {v3, p0, v2, v0}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda8;-><init>(Lcom/m365/gateway/MainActivity;Ljava/lang/String;Ljava/lang/String;)V

    invoke-virtual {v1, v3}, Landroid/os/Handler;->post(Ljava/lang/Runnable;)Z

    return-void

    :goto_7
    if-eqz v1, :cond_9

    .line 216
    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->disconnect()V

    .line 218
    :cond_9
    throw v0
.end method

.method synthetic lambda$onCreate$0$com-m365-gateway-MainActivity(Landroid/view/View;)V
    .locals 0

    .line 101
    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->beginAuthorization()V

    return-void
.end method

.method synthetic lambda$onCreate$1$com-m365-gateway-MainActivity(Landroid/view/View;)V
    .locals 0

    .line 105
    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->requestBatteryExemption()V

    return-void
.end method

.method synthetic lambda$onCreate$2$com-m365-gateway-MainActivity(Landroid/view/View;)V
    .locals 0

    .line 109
    invoke-static {p0}, Lcom/m365/gateway/DiagActivity;->start(Landroid/content/Context;)V

    return-void
.end method

.method synthetic lambda$onCreate$3$com-m365-gateway-MainActivity(Landroid/view/View;)V
    .locals 0

    .line 113
    invoke-static {p0}, Lcom/m365/gateway/TunnelActivity;->start(Landroid/content/Context;)V

    return-void
.end method

.method synthetic lambda$pollUntilReady$6$com-m365-gateway-MainActivity(I)V
    .locals 0

    .line 0
    add-int/lit8 p1, p1, 0x1

    .line 273
    invoke-direct {p0, p1}, Lcom/m365/gateway/MainActivity;->pollUntilReady(I)V

    return-void
.end method

.method synthetic lambda$pollUntilReady$7$com-m365-gateway-MainActivity(ZI)V
    .locals 3

    .line 0
    if-eqz p1, :cond_1

    const/4 p1, 0x1

    .line 259
    iput-boolean p1, p0, Lcom/m365/gateway/MainActivity;->gatewayReady:Z

    .line 260
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    new-instance p2, Ljava/lang/StringBuilder;

    const-string v0, "\u8fd0\u884c\u4e2d"

    invoke-direct {p2, v0}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->freezeHint()Ljava/lang/String;

    move-result-object v0

    invoke-virtual {p2, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {p2}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p2

    invoke-virtual {p1, p2}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    .line 261
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    iget-object p2, p0, Lcom/m365/gateway/MainActivity;->statusTicker:Ljava/lang/Runnable;

    invoke-virtual {p1, p2}, Landroid/os/Handler;->removeCallbacks(Ljava/lang/Runnable;)V

    .line 262
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    iget-object p2, p0, Lcom/m365/gateway/MainActivity;->statusTicker:Ljava/lang/Runnable;

    invoke-virtual {p1, p2}, Landroid/os/Handler;->post(Ljava/lang/Runnable;)Z

    .line 263
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    invoke-virtual {p1}, Landroid/webkit/WebView;->getUrl()Ljava/lang/String;

    move-result-object p1

    if-nez p1, :cond_0

    .line 264
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    const-string p2, "http://127.0.0.1:4141"

    invoke-virtual {p1, p2}, Landroid/webkit/WebView;->loadUrl(Ljava/lang/String;)V

    :cond_0
    return-void

    :cond_1
    const/16 p1, 0x3c

    if-le p2, p1, :cond_2

    .line 269
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const-string p2, "\u542f\u52a8\u8d85\u65f6\uff0c\u8bf7\u67e5\u770b\u901a\u77e5\u680f\u6216\u91cd\u8bd5"

    invoke-virtual {p1, p2}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    return-void

    .line 272
    :cond_2
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    new-instance v0, Ljava/lang/StringBuilder;

    const-string v1, "\u6b63\u5728\u542f\u52a8\u7f51\u5173\u2026 ("

    invoke-direct {v0, v1}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {v0, p2}, Ljava/lang/StringBuilder;->append(I)Ljava/lang/StringBuilder;

    const-string v1, ")"

    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v0}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    invoke-virtual {p1, v0}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    .line 273
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    new-instance v0, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda1;

    invoke-direct {v0, p0, p2}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda1;-><init>(Lcom/m365/gateway/MainActivity;I)V

    const-wide/16 v1, 0x1f4

    invoke-virtual {p1, v0, v1, v2}, Landroid/os/Handler;->postDelayed(Ljava/lang/Runnable;J)Z

    return-void
.end method

.method synthetic lambda$pollUntilReady$8$com-m365-gateway-MainActivity(I)V
    .locals 3

    .line 256
    invoke-static {}, Lcom/m365/gateway/GatewayService;->portOpen()Z

    move-result v0

    .line 257
    iget-object v1, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    new-instance v2, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda3;

    invoke-direct {v2, p0, v0, p1}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda3;-><init>(Lcom/m365/gateway/MainActivity;ZI)V

    invoke-virtual {v1, v2}, Landroid/os/Handler;->post(Ljava/lang/Runnable;)Z

    return-void
.end method

.method protected onActivityResult(IILandroid/content/Intent;)V
    .locals 1

    .line 240
    invoke-super {p0, p1, p2, p3}, Landroid/app/Activity;->onActivityResult(IILandroid/content/Intent;)V

    const/16 v0, 0x14

    if-eq p1, v0, :cond_0

    return-void

    :cond_0
    const/4 p1, -0x1

    if-ne p2, p1, :cond_1

    .line 245
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const-string p2, "\u8d26\u53f7\u5df2\u5bfc\u5165 \u00b7 http://127.0.0.1:4141"

    invoke-virtual {p1, p2}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    .line 246
    const-string p1, "\u8d26\u53f7\u5df2\u52a0\u5165\u8d26\u53f7\u6c60"

    const/4 p2, 0x0

    invoke-static {p0, p1, p2}, Landroid/widget/Toast;->makeText(Landroid/content/Context;Ljava/lang/CharSequence;I)Landroid/widget/Toast;

    move-result-object p1

    invoke-virtual {p1}, Landroid/widget/Toast;->show()V

    .line 247
    iget-object p1, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    const-string p2, "http://127.0.0.1:4141"

    invoke-virtual {p1, p2}, Landroid/webkit/WebView;->loadUrl(Ljava/lang/String;)V

    return-void

    :cond_1
    if-nez p3, :cond_2

    const/4 p1, 0x0

    goto :goto_0

    .line 250
    :cond_2
    const-string p1, "error"

    invoke-virtual {p3, p1}, Landroid/content/Intent;->getStringExtra(Ljava/lang/String;)Ljava/lang/String;

    move-result-object p1

    .line 251
    :goto_0
    iget-object p2, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    if-nez p1, :cond_3

    const-string p1, "\u5df2\u53d6\u6d88\u6388\u6743"

    :cond_3
    invoke-virtual {p2, p1}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    return-void
.end method

.method public onBackPressed()V
    .locals 1

    .line 375
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    if-eqz v0, :cond_0

    invoke-virtual {v0}, Landroid/webkit/WebView;->canGoBack()Z

    move-result v0

    if-eqz v0, :cond_0

    .line 376
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    invoke-virtual {v0}, Landroid/webkit/WebView;->goBack()V

    return-void

    :cond_0
    const/4 v0, 0x1

    .line 380
    invoke-virtual {p0, v0}, Lcom/m365/gateway/MainActivity;->moveTaskToBack(Z)Z

    return-void
.end method

.method protected onCreate(Landroid/os/Bundle;)V
    .locals 12

    .line 59
    invoke-super {p0, p1}, Landroid/app/Activity;->onCreate(Landroid/os/Bundle;)V

    .line 60
    invoke-direct {p0}, Lcom/m365/gateway/MainActivity;->requestNotificationPermission()V

    .line 62
    new-instance p1, Landroid/widget/LinearLayout;

    invoke-direct {p1, p0}, Landroid/widget/LinearLayout;-><init>(Landroid/content/Context;)V

    const/4 v0, 0x1

    .line 63
    invoke-virtual {p1, v0}, Landroid/widget/LinearLayout;->setOrientation(I)V

    .line 64
    const-string v1, "#0B1220"

    invoke-static {v1}, Landroid/graphics/Color;->parseColor(Ljava/lang/String;)I

    move-result v2

    invoke-virtual {p1, v2}, Landroid/widget/LinearLayout;->setBackgroundColor(I)V

    .line 70
    new-instance v2, Landroid/widget/LinearLayout;

    invoke-direct {v2, p0}, Landroid/widget/LinearLayout;-><init>(Landroid/content/Context;)V

    .line 71
    invoke-virtual {v2, v0}, Landroid/widget/LinearLayout;->setOrientation(I)V

    .line 72
    invoke-static {v1}, Landroid/graphics/Color;->parseColor(Ljava/lang/String;)I

    move-result v1

    invoke-virtual {v2, v1}, Landroid/widget/LinearLayout;->setBackgroundColor(I)V

    .line 74
    new-instance v1, Landroid/widget/LinearLayout;

    invoke-direct {v1, p0}, Landroid/widget/LinearLayout;-><init>(Landroid/content/Context;)V

    const/4 v3, 0x0

    .line 75
    invoke-virtual {v1, v3}, Landroid/widget/LinearLayout;->setOrientation(I)V

    const/16 v4, 0x10

    .line 76
    invoke-virtual {v1, v4}, Landroid/widget/LinearLayout;->setGravity(I)V

    .line 77
    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v5

    const/16 v6, 0xa

    invoke-direct {p0, v6}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v6

    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v7

    const/4 v8, 0x4

    invoke-direct {p0, v8}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v8

    invoke-virtual {v1, v5, v6, v7, v8}, Landroid/widget/LinearLayout;->setPadding(IIII)V

    .line 79
    new-instance v5, Landroid/widget/TextView;

    invoke-direct {v5, p0}, Landroid/widget/TextView;-><init>(Landroid/content/Context;)V

    .line 80
    const-string v6, "M365 Copilot \u7f51\u5173"

    invoke-virtual {v5, v6}, Landroid/widget/TextView;->setText(Ljava/lang/CharSequence;)V

    const/4 v6, -0x1

    .line 81
    invoke-virtual {v5, v6}, Landroid/widget/TextView;->setTextColor(I)V

    const/high16 v7, 0x41900000    # 18.0f

    .line 82
    invoke-virtual {v5, v7}, Landroid/widget/TextView;->setTextSize(F)V

    .line 83
    invoke-virtual {v5, v0}, Landroid/widget/TextView;->setMaxLines(I)V

    .line 84
    new-instance v7, Landroid/widget/LinearLayout$LayoutParams;

    const/16 v8, 0x22

    invoke-direct {p0, v8}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v9

    const/high16 v10, 0x3f800000    # 1.0f

    invoke-direct {v7, v3, v9, v10}, Landroid/widget/LinearLayout$LayoutParams;-><init>(IIF)V

    invoke-virtual {v1, v5, v7}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 86
    new-instance v5, Landroid/widget/TextView;

    invoke-direct {v5, p0}, Landroid/widget/TextView;-><init>(Landroid/content/Context;)V

    iput-object v5, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    .line 87
    const-string v7, "#9AA8BC"

    invoke-static {v7}, Landroid/graphics/Color;->parseColor(Ljava/lang/String;)I

    move-result v7

    invoke-virtual {v5, v7}, Landroid/widget/TextView;->setTextColor(I)V

    .line 88
    iget-object v5, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const/high16 v7, 0x41300000    # 11.0f

    invoke-virtual {v5, v7}, Landroid/widget/TextView;->setTextSize(F)V

    .line 89
    iget-object v5, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const/16 v7, 0x15

    invoke-virtual {v5, v7}, Landroid/widget/TextView;->setGravity(I)V

    .line 90
    iget-object v5, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    const/4 v7, 0x2

    invoke-virtual {v5, v7}, Landroid/widget/TextView;->setMaxLines(I)V

    .line 91
    iget-object v5, p0, Lcom/m365/gateway/MainActivity;->status:Landroid/widget/TextView;

    new-instance v9, Landroid/widget/LinearLayout$LayoutParams;

    const/16 v11, 0x96

    invoke-direct {p0, v11}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v11

    invoke-direct {p0, v8}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v8

    invoke-direct {v9, v11, v8}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    invoke-virtual {v1, v5, v9}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 92
    new-instance v5, Landroid/widget/LinearLayout$LayoutParams;

    const/16 v8, 0x30

    .line 93
    invoke-direct {p0, v8}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v9

    invoke-direct {v5, v6, v9}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    .line 92
    invoke-virtual {v2, v1, v5}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 95
    new-instance v1, Landroid/widget/LinearLayout;

    invoke-direct {v1, p0}, Landroid/widget/LinearLayout;-><init>(Landroid/content/Context;)V

    .line 96
    invoke-virtual {v1, v3}, Landroid/widget/LinearLayout;->setOrientation(I)V

    .line 97
    invoke-virtual {v1, v4}, Landroid/widget/LinearLayout;->setGravity(I)V

    const/16 v4, 0x9

    .line 98
    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v5

    invoke-direct {p0, v7}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v7

    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v4

    const/4 v9, 0x7

    invoke-direct {p0, v9}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v9

    invoke-virtual {v1, v5, v7, v4, v9}, Landroid/widget/LinearLayout;->setPadding(IIII)V

    .line 100
    const-string v4, "\u6dfb\u52a0\u8d26\u53f7"

    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->compactButton(Ljava/lang/String;)Landroid/widget/Button;

    move-result-object v4

    .line 101
    new-instance v5, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda4;

    invoke-direct {v5, p0}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda4;-><init>(Lcom/m365/gateway/MainActivity;)V

    invoke-virtual {v4, v5}, Landroid/widget/Button;->setOnClickListener(Landroid/view/View$OnClickListener;)V

    .line 102
    invoke-virtual {v1, v4}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;)V

    .line 104
    const-string v4, "\u4fdd\u6d3b"

    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->compactButton(Ljava/lang/String;)Landroid/widget/Button;

    move-result-object v4

    .line 105
    new-instance v5, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda5;

    invoke-direct {v5, p0}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda5;-><init>(Lcom/m365/gateway/MainActivity;)V

    invoke-virtual {v4, v5}, Landroid/widget/Button;->setOnClickListener(Landroid/view/View$OnClickListener;)V

    .line 106
    invoke-virtual {v1, v4}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;)V

    .line 108
    const-string v4, "\u8bca\u65ad"

    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->compactButton(Ljava/lang/String;)Landroid/widget/Button;

    move-result-object v4

    .line 109
    new-instance v5, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda6;

    invoke-direct {v5, p0}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda6;-><init>(Lcom/m365/gateway/MainActivity;)V

    invoke-virtual {v4, v5}, Landroid/widget/Button;->setOnClickListener(Landroid/view/View$OnClickListener;)V

    .line 110
    invoke-virtual {v1, v4}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;)V

    .line 112
    const-string v4, "\u516c\u7f51"

    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->compactButton(Ljava/lang/String;)Landroid/widget/Button;

    move-result-object v4

    .line 113
    new-instance v5, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda7;

    invoke-direct {v5, p0}, Lcom/m365/gateway/MainActivity$$ExternalSyntheticLambda7;-><init>(Lcom/m365/gateway/MainActivity;)V

    invoke-virtual {v4, v5}, Landroid/widget/Button;->setOnClickListener(Landroid/view/View$OnClickListener;)V

    .line 114
    invoke-virtual {v1, v4}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;)V

    .line 116
    new-instance v4, Landroid/widget/LinearLayout$LayoutParams;

    .line 117
    invoke-direct {p0, v8}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v5

    invoke-direct {v4, v6, v5}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    .line 116
    invoke-virtual {v2, v1, v4}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 118
    new-instance v1, Landroid/widget/LinearLayout$LayoutParams;

    const/16 v4, 0x60

    .line 119
    invoke-direct {p0, v4}, Lcom/m365/gateway/MainActivity;->dp(I)I

    move-result v4

    invoke-direct {v1, v6, v4}, Landroid/widget/LinearLayout$LayoutParams;-><init>(II)V

    .line 118
    invoke-virtual {p1, v2, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 121
    new-instance v1, Landroid/webkit/WebView;

    invoke-direct {v1, p0}, Landroid/webkit/WebView;-><init>(Landroid/content/Context;)V

    iput-object v1, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    .line 122
    invoke-virtual {v1}, Landroid/webkit/WebView;->getSettings()Landroid/webkit/WebSettings;

    move-result-object v1

    .line 123
    invoke-virtual {v1, v0}, Landroid/webkit/WebSettings;->setJavaScriptEnabled(Z)V

    .line 124
    invoke-virtual {v1, v0}, Landroid/webkit/WebSettings;->setDomStorageEnabled(Z)V

    .line 125
    invoke-virtual {v1, v0}, Landroid/webkit/WebSettings;->setDatabaseEnabled(Z)V

    .line 126
    invoke-virtual {v1, v3}, Landroid/webkit/WebSettings;->setMediaPlaybackRequiresUserGesture(Z)V

    .line 127
    invoke-virtual {v1, v3}, Landroid/webkit/WebSettings;->setSupportMultipleWindows(Z)V

    .line 130
    invoke-virtual {v1, v3}, Landroid/webkit/WebSettings;->setUseWideViewPort(Z)V

    .line 131
    invoke-virtual {v1, v3}, Landroid/webkit/WebSettings;->setLoadWithOverviewMode(Z)V

    const/16 v2, 0x64

    .line 132
    invoke-virtual {v1, v2}, Landroid/webkit/WebSettings;->setTextZoom(I)V

    .line 133
    invoke-static {}, Landroid/webkit/CookieManager;->getInstance()Landroid/webkit/CookieManager;

    move-result-object v1

    invoke-virtual {v1, v0}, Landroid/webkit/CookieManager;->setAcceptCookie(Z)V

    .line 134
    invoke-static {}, Landroid/webkit/CookieManager;->getInstance()Landroid/webkit/CookieManager;

    move-result-object v1

    iget-object v2, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    invoke-virtual {v1, v2, v0}, Landroid/webkit/CookieManager;->setAcceptThirdPartyCookies(Landroid/webkit/WebView;Z)V

    .line 136
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    new-instance v1, Landroid/webkit/WebChromeClient;

    invoke-direct {v1}, Landroid/webkit/WebChromeClient;-><init>()V

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebChromeClient(Landroid/webkit/WebChromeClient;)V

    .line 137
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    new-instance v1, Lcom/m365/gateway/MainActivity$2;

    invoke-direct {v1, p0}, Lcom/m365/gateway/MainActivity$2;-><init>(Lcom/m365/gateway/MainActivity;)V

    invoke-virtual {v0, v1}, Landroid/webkit/WebView;->setWebViewClient(Landroid/webkit/WebViewClient;)V

    .line 159
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->web:Landroid/webkit/WebView;

    new-instance v1, Landroid/widget/LinearLayout$LayoutParams;

    invoke-direct {v1, v6, v3, v10}, Landroid/widget/LinearLayout$LayoutParams;-><init>(IIF)V

    invoke-virtual {p1, v0, v1}, Landroid/widget/LinearLayout;->addView(Landroid/view/View;Landroid/view/ViewGroup$LayoutParams;)V

    .line 162
    invoke-virtual {p0, p1}, Lcom/m365/gateway/MainActivity;->setContentView(Landroid/view/View;)V

    .line 164
    new-instance p1, Landroid/content/Intent;

    const-class v0, Lcom/m365/gateway/GatewayService;

    invoke-direct {p1, p0, v0}, Landroid/content/Intent;-><init>(Landroid/content/Context;Ljava/lang/Class;)V

    const-string v0, "com.m365.gateway3.START"

    invoke-virtual {p1, v0}, Landroid/content/Intent;->setAction(Ljava/lang/String;)Landroid/content/Intent;

    move-result-object p1

    invoke-virtual {p0, p1}, Lcom/m365/gateway/MainActivity;->startService(Landroid/content/Intent;)Landroid/content/ComponentName;

    .line 165
    invoke-direct {p0, v3}, Lcom/m365/gateway/MainActivity;->pollUntilReady(I)V

    return-void
.end method

.method protected onPause()V
    .locals 2

    .line 304
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    iget-object v1, p0, Lcom/m365/gateway/MainActivity;->statusTicker:Ljava/lang/Runnable;

    invoke-virtual {v0, v1}, Landroid/os/Handler;->removeCallbacks(Ljava/lang/Runnable;)V

    .line 305
    invoke-super {p0}, Landroid/app/Activity;->onPause()V

    return-void
.end method

.method protected onResume()V
    .locals 2

    .line 295
    invoke-super {p0}, Landroid/app/Activity;->onResume()V

    .line 296
    iget-boolean v0, p0, Lcom/m365/gateway/MainActivity;->gatewayReady:Z

    if-eqz v0, :cond_0

    .line 297
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    iget-object v1, p0, Lcom/m365/gateway/MainActivity;->statusTicker:Ljava/lang/Runnable;

    invoke-virtual {v0, v1}, Landroid/os/Handler;->removeCallbacks(Ljava/lang/Runnable;)V

    .line 298
    iget-object v0, p0, Lcom/m365/gateway/MainActivity;->ui:Landroid/os/Handler;

    iget-object v1, p0, Lcom/m365/gateway/MainActivity;->statusTicker:Ljava/lang/Runnable;

    invoke-virtual {v0, v1}, Landroid/os/Handler;->post(Ljava/lang/Runnable;)Z

    :cond_0
    return-void
.end method

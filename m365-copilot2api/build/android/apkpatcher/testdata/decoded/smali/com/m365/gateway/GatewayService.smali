.class public Lcom/m365/gateway/GatewayService;
.super Landroid/app/Service;
.source "GatewayService.java"


# static fields
.field public static final ACTION_START:Ljava/lang/String; = "com.m365.gateway3.START"

.field public static final ACTION_STOP:Ljava/lang/String; = "com.m365.gateway3.STOP"

.field public static final ACTION_TUNNEL_START:Ljava/lang/String; = "com.m365.gateway3.TUNNEL_START"

.field public static final BASE_URL:Ljava/lang/String; = "http://127.0.0.1:4141"

.field private static final CHANNEL_ID:Ljava/lang/String; = "gateway"

.field private static final HEARTBEAT_INTERVAL_MS:J = 0x61a8L

.field private static final NOTIF_ID:I = 0x102d

.field public static final PORT:I = 0x102d

.field private static final TAG:Ljava/lang/String; = "M365Gateway"

.field private static final WATCHDOG_INTERVAL_MS:J = 0x3a98L

.field private static volatile lastHeartbeatAt:J

.field private static volatile lastHeartbeatOk:Z

.field private static volatile lastKeepAliveTick:J

.field private static volatile running:Z


# instance fields
.field private heartbeat:Ljava/lang/Thread;

.field private logPump:Ljava/lang/Thread;

.field private proc:Ljava/lang/Process;

.field private volatile supervise:Z

.field private wakeLock:Landroid/os/PowerManager$WakeLock;

.field private watchdog:Ljava/lang/Thread;


# direct methods
.method public static synthetic $r8$lambda$p882DWW0qD8Y2j9XUrrvDP9kBYE(Lcom/m365/gateway/GatewayService;)V
    .registers 1

    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->waitForReady()V

    return-void
.end method

.method static constructor <clinit>()V
    .registers 0

    return-void
.end method

.method public constructor <init>()V
    .registers 2

    .line 35
    invoke-direct {p0}, Landroid/app/Service;-><init>()V

    const/4 v0, 0x0

    .line 153
    iput-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    return-void
.end method

.method private acquireWakeLock()V
    .registers 4

    .line 445
    const-string v0, "power"

    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->getSystemService(Ljava/lang/String;)Ljava/lang/Object;

    move-result-object v0

    check-cast v0, Landroid/os/PowerManager;

    if-nez v0, :cond_b

    return-void

    :cond_b
    const/4 v1, 0x1

    .line 449
    const-string v2, "m365:gateway"

    invoke-virtual {v0, v1, v2}, Landroid/os/PowerManager;->newWakeLock(ILjava/lang/String;)Landroid/os/PowerManager$WakeLock;

    move-result-object v0

    iput-object v0, p0, Lcom/m365/gateway/GatewayService;->wakeLock:Landroid/os/PowerManager$WakeLock;

    const/4 v1, 0x0

    .line 450
    invoke-virtual {v0, v1}, Landroid/os/PowerManager$WakeLock;->setReferenceCounted(Z)V

    .line 451
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->wakeLock:Landroid/os/PowerManager$WakeLock;

    invoke-virtual {v0}, Landroid/os/PowerManager$WakeLock;->acquire()V

    return-void
.end method

.method private buildNotification(Ljava/lang/String;)Landroid/app/Notification;
    .registers 7

    .line 495
    new-instance v0, Landroid/content/Intent;

    const-class v1, Lcom/m365/gateway/MainActivity;

    invoke-direct {v0, p0, v1}, Landroid/content/Intent;-><init>(Landroid/content/Context;Ljava/lang/Class;)V

    const/4 v1, 0x0

    const/high16 v2, 0xc000000

    .line 496
    invoke-static {p0, v1, v0, v2}, Landroid/app/PendingIntent;->getActivity(Landroid/content/Context;ILandroid/content/Intent;I)Landroid/app/PendingIntent;

    move-result-object v0

    .line 499
    new-instance v1, Landroid/content/Intent;

    const-class v3, Lcom/m365/gateway/GatewayService;

    invoke-direct {v1, p0, v3}, Landroid/content/Intent;-><init>(Landroid/content/Context;Ljava/lang/Class;)V

    const-string v3, "com.m365.gateway3.STOP"

    invoke-virtual {v1, v3}, Landroid/content/Intent;->setAction(Ljava/lang/String;)Landroid/content/Intent;

    move-result-object v1

    const/4 v3, 0x1

    .line 500
    invoke-static {p0, v3, v1, v2}, Landroid/app/PendingIntent;->getService(Landroid/content/Context;ILandroid/content/Intent;I)Landroid/app/PendingIntent;

    move-result-object v1

    .line 504
    new-instance v2, Landroid/app/Notification$Builder;

    const-string v4, "gateway"

    invoke-direct {v2, p0, v4}, Landroid/app/Notification$Builder;-><init>(Landroid/content/Context;Ljava/lang/String;)V

    const v4, 0x7f030001

    .line 506
    invoke-virtual {p0, v4}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v4

    invoke-virtual {v2, v4}, Landroid/app/Notification$Builder;->setContentTitle(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;

    move-result-object v2

    .line 507
    invoke-virtual {v2, p1}, Landroid/app/Notification$Builder;->setContentText(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;

    move-result-object p1

    const/high16 v2, 0x7f010000

    .line 508
    invoke-virtual {p1, v2}, Landroid/app/Notification$Builder;->setSmallIcon(I)Landroid/app/Notification$Builder;

    move-result-object p1

    .line 509
    invoke-virtual {p1, v0}, Landroid/app/Notification$Builder;->setContentIntent(Landroid/app/PendingIntent;)Landroid/app/Notification$Builder;

    move-result-object p1

    .line 510
    invoke-virtual {p1, v3}, Landroid/app/Notification$Builder;->setOngoing(Z)Landroid/app/Notification$Builder;

    move-result-object p1

    new-instance v0, Landroid/app/Notification$Action$Builder;

    const/high16 v2, 0x7f030000

    .line 511
    invoke-virtual {p0, v2}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v2

    const/4 v3, 0x0

    invoke-direct {v0, v3, v2, v1}, Landroid/app/Notification$Action$Builder;-><init>(Landroid/graphics/drawable/Icon;Ljava/lang/CharSequence;Landroid/app/PendingIntent;)V

    invoke-virtual {v0}, Landroid/app/Notification$Action$Builder;->build()Landroid/app/Notification$Action;

    move-result-object v0

    invoke-virtual {p1, v0}, Landroid/app/Notification$Builder;->addAction(Landroid/app/Notification$Action;)Landroid/app/Notification$Builder;

    move-result-object p1

    .line 512
    invoke-virtual {p1}, Landroid/app/Notification$Builder;->build()Landroid/app/Notification;

    move-result-object p1

    return-object p1
.end method

.method private createChannel()V
    .registers 6

    .line 484
    const-string v0, "notification"

    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->getSystemService(Ljava/lang/String;)Ljava/lang/Object;

    move-result-object v0

    check-cast v0, Landroid/app/NotificationManager;

    if-nez v0, :cond_b

    return-void

    .line 488
    :cond_b
    new-instance v1, Landroid/app/NotificationChannel;

    const v2, 0x7f030002

    .line 489
    invoke-virtual {p0, v2}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v2

    const/4 v3, 0x2

    const-string v4, "gateway"

    invoke-direct {v1, v4, v2, v3}, Landroid/app/NotificationChannel;-><init>(Ljava/lang/String;Ljava/lang/CharSequence;I)V

    const/4 v2, 0x0

    .line 490
    invoke-virtual {v1, v2}, Landroid/app/NotificationChannel;->setShowBadge(Z)V

    .line 491
    invoke-virtual {v0, v1}, Landroid/app/NotificationManager;->createNotificationChannel(Landroid/app/NotificationChannel;)V

    return-void
.end method

.method static heartbeatOk()Z
    .registers 1

    .line 296
    sget-boolean v0, Lcom/m365/gateway/GatewayService;->lastHeartbeatOk:Z

    return v0
.end method

.method private installCaBundle(Ljava/io/File;)Ljava/io/File;
    .registers 8
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/io/IOException;
        }
    .end annotation

    .line 429
    new-instance v0, Ljava/io/File;

    const-string v1, "ca-certificates.crt"

    invoke-direct {v0, p1, v1}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 430
    invoke-virtual {v0}, Ljava/io/File;->exists()Z

    move-result p1

    if-eqz p1, :cond_18

    invoke-virtual {v0}, Ljava/io/File;->length()J

    move-result-wide v2

    const-wide/16 v4, 0x0

    cmp-long p1, v2, v4

    if-lez p1, :cond_18

    return-object v0

    .line 433
    :cond_18
    invoke-virtual {p0}, Lcom/m365/gateway/GatewayService;->getAssets()Landroid/content/res/AssetManager;

    move-result-object p1

    invoke-virtual {p1, v1}, Landroid/content/res/AssetManager;->open(Ljava/lang/String;)Ljava/io/InputStream;

    move-result-object p1

    .line 434
    :try_start_20
    new-instance v1, Ljava/io/FileOutputStream;

    invoke-direct {v1, v0}, Ljava/io/FileOutputStream;-><init>(Ljava/io/File;)V
    :try_end_25
    .catchall {:try_start_20 .. :try_end_25} :catchall_47

    const/16 v2, 0x2000

    .line 435
    :try_start_27
    new-array v2, v2, [B

    .line 437
    :goto_29
    invoke-virtual {p1, v2}, Ljava/io/InputStream;->read([B)I

    move-result v3

    if-lez v3, :cond_34

    const/4 v4, 0x0

    .line 438
    invoke-virtual {v1, v2, v4, v3}, Ljava/io/OutputStream;->write([BII)V
    :try_end_33
    .catchall {:try_start_27 .. :try_end_33} :catchall_3d

    goto :goto_29

    .line 440
    :cond_34
    :try_start_34
    invoke-virtual {v1}, Ljava/io/OutputStream;->close()V
    :try_end_37
    .catchall {:try_start_34 .. :try_end_37} :catchall_47

    if-eqz p1, :cond_3c

    invoke-virtual {p1}, Ljava/io/InputStream;->close()V

    :cond_3c
    return-object v0

    :catchall_3d
    move-exception v0

    .line 433
    :try_start_3e
    invoke-virtual {v1}, Ljava/io/OutputStream;->close()V
    :try_end_41
    .catchall {:try_start_3e .. :try_end_41} :catchall_42

    goto :goto_46

    :catchall_42
    move-exception v1

    :try_start_43
    invoke-virtual {v0, v1}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :goto_46
    throw v0
    :try_end_47
    .catchall {:try_start_43 .. :try_end_47} :catchall_47

    :catchall_47
    move-exception v0

    if-eqz p1, :cond_52

    :try_start_4a
    invoke-virtual {p1}, Ljava/io/InputStream;->close()V
    :try_end_4d
    .catchall {:try_start_4a .. :try_end_4d} :catchall_4e

    goto :goto_52

    :catchall_4e
    move-exception p1

    invoke-virtual {v0, p1}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :cond_52
    :goto_52
    throw v0
.end method

.method private installWebAssets(Ljava/io/File;)V
    .registers 11
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/io/IOException;
        }
    .end annotation

    .line 407
    new-instance v0, Ljava/io/File;

    const-string v1, "web"

    invoke-direct {v0, p1, v1}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 409
    invoke-virtual {v0}, Ljava/io/File;->mkdirs()Z

    .line 410
    invoke-virtual {p0}, Lcom/m365/gateway/GatewayService;->getAssets()Landroid/content/res/AssetManager;

    move-result-object p1

    invoke-virtual {p1, v1}, Landroid/content/res/AssetManager;->list(Ljava/lang/String;)[Ljava/lang/String;

    move-result-object p1

    if-nez p1, :cond_15

    return-void

    .line 414
    :cond_15
    array-length v1, p1

    const/4 v2, 0x0

    move v3, v2

    :goto_18
    if-ge v3, v1, :cond_6b

    aget-object v4, p1, v3

    .line 415
    new-instance v5, Ljava/io/File;

    invoke-direct {v5, v0, v4}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 416
    invoke-virtual {p0}, Lcom/m365/gateway/GatewayService;->getAssets()Landroid/content/res/AssetManager;

    move-result-object v6

    new-instance v7, Ljava/lang/StringBuilder;

    const-string v8, "web/"

    invoke-direct {v7, v8}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {v7, v4}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v7}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v4

    invoke-virtual {v6, v4}, Landroid/content/res/AssetManager;->open(Ljava/lang/String;)Ljava/io/InputStream;

    move-result-object v4

    .line 417
    :try_start_37
    new-instance v6, Ljava/io/FileOutputStream;

    invoke-direct {v6, v5}, Ljava/io/FileOutputStream;-><init>(Ljava/io/File;)V
    :try_end_3c
    .catchall {:try_start_37 .. :try_end_3c} :catchall_5f

    const/16 v5, 0x2000

    .line 418
    :try_start_3e
    new-array v5, v5, [B

    .line 420
    :goto_40
    invoke-virtual {v4, v5}, Ljava/io/InputStream;->read([B)I

    move-result v7

    if-lez v7, :cond_4a

    .line 421
    invoke-virtual {v6, v5, v2, v7}, Ljava/io/OutputStream;->write([BII)V
    :try_end_49
    .catchall {:try_start_3e .. :try_end_49} :catchall_55

    goto :goto_40

    .line 423
    :cond_4a
    :try_start_4a
    invoke-virtual {v6}, Ljava/io/OutputStream;->close()V
    :try_end_4d
    .catchall {:try_start_4a .. :try_end_4d} :catchall_5f

    if-eqz v4, :cond_52

    invoke-virtual {v4}, Ljava/io/InputStream;->close()V

    :cond_52
    add-int/lit8 v3, v3, 0x1

    goto :goto_18

    :catchall_55
    move-exception p1

    .line 416
    :try_start_56
    invoke-virtual {v6}, Ljava/io/OutputStream;->close()V
    :try_end_59
    .catchall {:try_start_56 .. :try_end_59} :catchall_5a

    goto :goto_5e

    :catchall_5a
    move-exception v0

    :try_start_5b
    invoke-virtual {p1, v0}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :goto_5e
    throw p1
    :try_end_5f
    .catchall {:try_start_5b .. :try_end_5f} :catchall_5f

    :catchall_5f
    move-exception p1

    if-eqz v4, :cond_6a

    :try_start_62
    invoke-virtual {v4}, Ljava/io/InputStream;->close()V
    :try_end_65
    .catchall {:try_start_62 .. :try_end_65} :catchall_66

    goto :goto_6a

    :catchall_66
    move-exception v0

    invoke-virtual {p1, v0}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :cond_6a
    :goto_6a
    throw p1

    :cond_6b
    return-void
.end method

.method public static isRunning()Z
    .registers 1

    .line 69
    sget-boolean v0, Lcom/m365/gateway/GatewayService;->running:Z

    return v0
.end method

.method public static noteKeepAliveTick()V
    .registers 2

    .line 52
    invoke-static {}, Ljava/lang/System;->currentTimeMillis()J

    move-result-wide v0

    sput-wide v0, Lcom/m365/gateway/GatewayService;->lastKeepAliveTick:J

    return-void
.end method

.method private notify(Ljava/lang/String;)V
    .registers 4

    .line 516
    const-string v0, "notification"

    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->getSystemService(Ljava/lang/String;)Ljava/lang/Object;

    move-result-object v0

    check-cast v0, Landroid/app/NotificationManager;

    if-eqz v0, :cond_13

    const/16 v1, 0x102d

    .line 518
    invoke-direct {p0, p1}, Lcom/m365/gateway/GatewayService;->buildNotification(Ljava/lang/String;)Landroid/app/Notification;

    move-result-object p1

    invoke-virtual {v0, v1, p1}, Landroid/app/NotificationManager;->notify(ILandroid/app/Notification;)V

    :cond_13
    return-void
.end method

.method public static portOpen()Z
    .registers 4

    .line 73
    new-instance v0, Ljava/net/Socket;

    invoke-direct {v0}, Ljava/net/Socket;-><init>()V

    .line 75
    :try_start_5
    new-instance v1, Ljava/net/InetSocketAddress;

    const-string v2, "127.0.0.1"

    const/16 v3, 0x102d

    invoke-direct {v1, v2, v3}, Ljava/net/InetSocketAddress;-><init>(Ljava/lang/String;I)V

    const/16 v2, 0x190

    invoke-virtual {v0, v1, v2}, Ljava/net/Socket;->connect(Ljava/net/SocketAddress;I)V
    :try_end_13
    .catch Ljava/io/IOException; {:try_start_5 .. :try_end_13} :catch_1d
    .catchall {:try_start_5 .. :try_end_13} :catchall_18

    .line 81
    :try_start_13
    invoke-virtual {v0}, Ljava/net/Socket;->close()V
    :try_end_16
    .catch Ljava/io/IOException; {:try_start_13 .. :try_end_16} :catch_16

    :catch_16
    const/4 v0, 0x1

    return v0

    :catchall_18
    move-exception v1

    :try_start_19
    invoke-virtual {v0}, Ljava/net/Socket;->close()V
    :try_end_1c
    .catch Ljava/io/IOException; {:try_start_19 .. :try_end_1c} :catch_1c

    .line 84
    :catch_1c
    throw v1

    .line 81
    :catch_1d
    :try_start_1d
    invoke-virtual {v0}, Ljava/net/Socket;->close()V
    :try_end_20
    .catch Ljava/io/IOException; {:try_start_1d .. :try_end_20} :catch_20

    :catch_20
    const/4 v0, 0x0

    return v0
.end method

.method private pumpLog(Ljava/io/File;)V
    .registers 6

    .line 391
    const-string v0, "M365Gateway"

    :try_start_2
    new-instance v1, Ljava/io/BufferedReader;

    new-instance v2, Ljava/io/InputStreamReader;

    iget-object v3, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    invoke-virtual {v3}, Ljava/lang/Process;->getInputStream()Ljava/io/InputStream;

    move-result-object v3

    invoke-direct {v2, v3}, Ljava/io/InputStreamReader;-><init>(Ljava/io/InputStream;)V

    invoke-direct {v1, v2}, Ljava/io/BufferedReader;-><init>(Ljava/io/Reader;)V
    :try_end_12
    .catch Ljava/io/IOException; {:try_start_2 .. :try_end_12} :catch_5a

    .line 392
    :try_start_12
    new-instance v2, Ljava/io/FileOutputStream;

    const/4 v3, 0x1

    invoke-direct {v2, p1, v3}, Ljava/io/FileOutputStream;-><init>(Ljava/io/File;Z)V
    :try_end_18
    .catchall {:try_start_12 .. :try_end_18} :catchall_50

    .line 394
    :goto_18
    :try_start_18
    invoke-virtual {v1}, Ljava/io/BufferedReader;->readLine()Ljava/lang/String;

    move-result-object p1

    if-eqz p1, :cond_3f

    .line 395
    invoke-static {v0, p1}, Landroid/util/Log;->i(Ljava/lang/String;Ljava/lang/String;)I

    .line 396
    new-instance v3, Ljava/lang/StringBuilder;

    invoke-direct {v3}, Ljava/lang/StringBuilder;-><init>()V

    invoke-virtual {v3, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    const-string p1, "\n"

    invoke-virtual {v3, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p1

    const-string v3, "UTF-8"

    invoke-virtual {p1, v3}, Ljava/lang/String;->getBytes(Ljava/lang/String;)[B

    move-result-object p1

    invoke-virtual {v2, p1}, Ljava/io/OutputStream;->write([B)V

    .line 397
    invoke-virtual {v2}, Ljava/io/OutputStream;->flush()V
    :try_end_3e
    .catchall {:try_start_18 .. :try_end_3e} :catchall_46

    goto :goto_18

    .line 399
    :cond_3f
    :try_start_3f
    invoke-virtual {v2}, Ljava/io/OutputStream;->close()V
    :try_end_42
    .catchall {:try_start_3f .. :try_end_42} :catchall_50

    :try_start_42
    invoke-virtual {v1}, Ljava/io/BufferedReader;->close()V
    :try_end_45
    .catch Ljava/io/IOException; {:try_start_42 .. :try_end_45} :catch_5a

    goto :goto_70

    :catchall_46
    move-exception p1

    .line 391
    :try_start_47
    invoke-virtual {v2}, Ljava/io/OutputStream;->close()V
    :try_end_4a
    .catchall {:try_start_47 .. :try_end_4a} :catchall_4b

    goto :goto_4f

    :catchall_4b
    move-exception v2

    :try_start_4c
    invoke-virtual {p1, v2}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :goto_4f
    throw p1
    :try_end_50
    .catchall {:try_start_4c .. :try_end_50} :catchall_50

    :catchall_50
    move-exception p1

    :try_start_51
    invoke-virtual {v1}, Ljava/io/BufferedReader;->close()V
    :try_end_54
    .catchall {:try_start_51 .. :try_end_54} :catchall_55

    goto :goto_59

    :catchall_55
    move-exception v1

    :try_start_56
    invoke-virtual {p1, v1}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :goto_59
    throw p1
    :try_end_5a
    .catch Ljava/io/IOException; {:try_start_56 .. :try_end_5a} :catch_5a

    :catch_5a
    move-exception p1

    .line 400
    new-instance v1, Ljava/lang/StringBuilder;

    const-string v2, "log pump ended: "

    invoke-direct {v1, v2}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {p1}, Ljava/io/IOException;->getMessage()Ljava/lang/String;

    move-result-object p1

    invoke-virtual {v1, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p1

    invoke-static {v0, p1}, Landroid/util/Log;->w(Ljava/lang/String;Ljava/lang/String;)I

    :goto_70
    const/4 p1, 0x0

    .line 402
    sput-boolean p1, Lcom/m365/gateway/GatewayService;->running:Z

    return-void
.end method

.method static sinceHeartbeat()J
    .registers 4

    .line 291
    sget-wide v0, Lcom/m365/gateway/GatewayService;->lastHeartbeatAt:J

    const-wide/16 v2, 0x0

    cmp-long v2, v0, v2

    if-nez v2, :cond_b

    const-wide/16 v0, -0x1

    goto :goto_11

    .line 292
    :cond_b
    invoke-static {}, Ljava/lang/System;->currentTimeMillis()J

    move-result-wide v2

    sub-long v0, v2, v0

    :goto_11
    return-wide v0
.end method

.method public static sinceKeepAliveTick()J
    .registers 4

    .line 57
    sget-wide v0, Lcom/m365/gateway/GatewayService;->lastKeepAliveTick:J

    const-wide/16 v2, 0x0

    cmp-long v0, v0, v2

    if-nez v0, :cond_b

    const-wide/16 v0, -0x1

    return-wide v0

    .line 60
    :cond_b
    invoke-static {}, Ljava/lang/System;->currentTimeMillis()J

    move-result-wide v0

    sget-wide v2, Lcom/m365/gateway/GatewayService;->lastKeepAliveTick:J

    sub-long/2addr v0, v2

    return-wide v0
.end method

.method private startGateway()V
    .registers 11
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/io/IOException;
        }
    .end annotation

    .line 334
    new-instance v0, Ljava/io/File;

    invoke-virtual {p0}, Lcom/m365/gateway/GatewayService;->getApplicationInfo()Landroid/content/pm/ApplicationInfo;

    move-result-object v1

    iget-object v1, v1, Landroid/content/pm/ApplicationInfo;->nativeLibraryDir:Ljava/lang/String;

    const-string v2, "libm365.so"

    invoke-direct {v0, v1, v2}, Ljava/io/File;-><init>(Ljava/lang/String;Ljava/lang/String;)V

    .line 335
    invoke-virtual {v0}, Ljava/io/File;->exists()Z

    move-result v1

    if-eqz v1, :cond_ed

    .line 338
    invoke-virtual {v0}, Ljava/io/File;->canExecute()Z

    move-result v1

    const/4 v2, 0x1

    if-nez v1, :cond_1d

    .line 340
    invoke-virtual {v0, v2, v2}, Ljava/io/File;->setExecutable(ZZ)Z

    .line 343
    :cond_1d
    new-instance v1, Ljava/io/File;

    invoke-virtual {p0}, Lcom/m365/gateway/GatewayService;->getFilesDir()Ljava/io/File;

    move-result-object v3

    const-string v4, "gw"

    invoke-direct {v1, v3, v4}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 344
    new-instance v3, Ljava/io/File;

    const-string v4, "data"

    invoke-direct {v3, v1, v4}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 345
    new-instance v4, Ljava/io/File;

    const-string v5, "tmp"

    invoke-direct {v4, v1, v5}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 347
    invoke-virtual {v3}, Ljava/io/File;->mkdirs()Z

    .line 349
    invoke-virtual {v4}, Ljava/io/File;->mkdirs()Z

    .line 350
    invoke-direct {p0, v1}, Lcom/m365/gateway/GatewayService;->installWebAssets(Ljava/io/File;)V

    .line 351
    invoke-direct {p0, v1}, Lcom/m365/gateway/GatewayService;->installCaBundle(Ljava/io/File;)Ljava/io/File;

    move-result-object v5

    .line 353
    new-instance v6, Ljava/lang/ProcessBuilder;

    invoke-virtual {v0}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v7

    filled-new-array {v7}, [Ljava/lang/String;

    move-result-object v7

    invoke-direct {v6, v7}, Ljava/lang/ProcessBuilder;-><init>([Ljava/lang/String;)V

    .line 355
    invoke-virtual {v6, v1}, Ljava/lang/ProcessBuilder;->directory(Ljava/io/File;)Ljava/lang/ProcessBuilder;

    .line 356
    invoke-virtual {v6, v2}, Ljava/lang/ProcessBuilder;->redirectErrorStream(Z)Ljava/lang/ProcessBuilder;

    .line 358
    invoke-virtual {v6}, Ljava/lang/ProcessBuilder;->environment()Ljava/util/Map;

    move-result-object v7

    .line 359
    const-string v8, "HOME"

    invoke-virtual {v1}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v9

    invoke-interface {v7, v8, v9}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 360
    const-string v8, "TMPDIR"

    invoke-virtual {v4}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v4

    invoke-interface {v7, v8, v4}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 361
    const-string v4, "M365_LISTEN"

    const-string v8, "127.0.0.1:4141"

    invoke-interface {v7, v4, v8}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 362
    const-string v4, "M365_DATA_DIR"

    invoke-virtual {v3}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v3

    invoke-interface {v7, v4, v3}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    const v3, 0x7f030003

    .line 363
    invoke-virtual {p0, v3}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v3

    const-string v4, "M365_ADMIN_PASSWORD"

    invoke-interface {v7, v4, v3}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 364
    const-string v3, "M365_LOG_LEVEL"

    const-string v4, "info"

    invoke-interface {v7, v3, v4}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 366
    const-string v3, "M365_DNS"

    const-string v4, "223.5.5.5,119.29.29.29,1.1.1.1,8.8.8.8"

    invoke-interface {v7, v3, v4}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 369
    const-string v3, "M365_PROMPT"

    const-string v4, "login"

    invoke-interface {v7, v3, v4}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 372
    const-string v3, "SSL_CERT_FILE"

    invoke-virtual {v5}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v4

    invoke-interface {v7, v3, v4}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 373
    const-string v3, "SSL_CERT_DIR"

    const-string v4, "/apex/com.android.conscrypt/cacerts:/system/etc/security/cacerts:/data/misc/keychain/certs-added"

    invoke-interface {v7, v3, v4}, Ljava/util/Map;->put(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;

    .line 378
    invoke-virtual {v6}, Ljava/lang/ProcessBuilder;->start()Ljava/lang/Process;

    move-result-object v3

    iput-object v3, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    .line 379
    sput-boolean v2, Lcom/m365/gateway/GatewayService;->running:Z

    .line 381
    new-instance v3, Ljava/io/File;

    const-string v4, "server.log"

    invoke-direct {v3, v1, v4}, Ljava/io/File;-><init>(Ljava/io/File;Ljava/lang/String;)V

    .line 382
    new-instance v1, Ljava/lang/Thread;

    new-instance v4, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda1;

    invoke-direct {v4, p0, v3}, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda1;-><init>(Lcom/m365/gateway/GatewayService;Ljava/io/File;)V

    const-string v3, "gateway-log"

    invoke-direct {v1, v4, v3}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    iput-object v1, p0, Lcom/m365/gateway/GatewayService;->logPump:Ljava/lang/Thread;

    .line 383
    invoke-virtual {v1, v2}, Ljava/lang/Thread;->setDaemon(Z)V

    .line 384
    iget-object v1, p0, Lcom/m365/gateway/GatewayService;->logPump:Ljava/lang/Thread;

    invoke-virtual {v1}, Ljava/lang/Thread;->start()V

    .line 386
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->acquireWakeLock()V

    .line 387
    new-instance v1, Ljava/lang/StringBuilder;

    const-string v2, "gateway started from "

    invoke-direct {v1, v2}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {v0}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v0

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    const-string v1, "M365Gateway"

    invoke-static {v1, v0}, Landroid/util/Log;->i(Ljava/lang/String;Ljava/lang/String;)I

    return-void

    .line 336
    :cond_ed
    new-instance v1, Ljava/io/IOException;

    new-instance v2, Ljava/lang/StringBuilder;

    const-string v3, "\u627e\u4e0d\u5230\u5185\u7f6e\u7f51\u5173\u4e8c\u8fdb\u5236: "

    invoke-direct {v2, v3}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {v0}, Ljava/io/File;->getAbsolutePath()Ljava/lang/String;

    move-result-object v0

    invoke-virtual {v2, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v2}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    invoke-direct {v1, v0}, Ljava/io/IOException;-><init>(Ljava/lang/String;)V

    throw v1
.end method

.method private startHeartbeat()V
    .registers 4

    .line 242
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->heartbeat:Ljava/lang/Thread;

    if-eqz v0, :cond_b

    invoke-virtual {v0}, Ljava/lang/Thread;->isAlive()Z

    move-result v0

    if-eqz v0, :cond_b

    return-void

    .line 245
    :cond_b
    new-instance v0, Ljava/lang/Thread;

    new-instance v1, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda2;

    invoke-direct {v1, p0}, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda2;-><init>(Lcom/m365/gateway/GatewayService;)V

    const-string v2, "gateway-heartbeat"

    invoke-direct {v0, v1, v2}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    iput-object v0, p0, Lcom/m365/gateway/GatewayService;->heartbeat:Ljava/lang/Thread;

    const/4 v1, 0x1

    .line 285
    invoke-virtual {v0, v1}, Ljava/lang/Thread;->setDaemon(Z)V

    .line 286
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->heartbeat:Ljava/lang/Thread;

    invoke-virtual {v0}, Ljava/lang/Thread;->start()V

    return-void
.end method

.method private startWatchdog()V
    .registers 5

    .line 178
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->watchdog:Ljava/lang/Thread;

    if-eqz v0, :cond_b

    invoke-virtual {v0}, Ljava/lang/Thread;->isAlive()Z

    move-result v0

    if-eqz v0, :cond_b

    return-void

    :cond_b
    const/4 v0, 0x1

    .line 181
    iput-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    .line 182
    new-instance v1, Ljava/lang/Thread;

    new-instance v2, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda0;

    invoke-direct {v2, p0}, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda0;-><init>(Lcom/m365/gateway/GatewayService;)V

    const-string v3, "gateway-watchdog"

    invoke-direct {v1, v2, v3}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    iput-object v1, p0, Lcom/m365/gateway/GatewayService;->watchdog:Ljava/lang/Thread;

    .line 221
    invoke-virtual {v1, v0}, Ljava/lang/Thread;->setDaemon(Z)V

    .line 222
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->watchdog:Ljava/lang/Thread;

    invoke-virtual {v0}, Ljava/lang/Thread;->start()V

    return-void
.end method

.method private stopGateway()V
    .registers 3

    const/4 v0, 0x0

    .line 455
    iput-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    .line 456
    sput-boolean v0, Lcom/m365/gateway/GatewayService;->running:Z

    const/4 v0, 0x0

    .line 457
    iput-object v0, p0, Lcom/m365/gateway/GatewayService;->heartbeat:Ljava/lang/Thread;

    .line 458
    invoke-static {}, Lcom/m365/gateway/SilentKeepAlive;->stop()V

    .line 459
    iget-object v1, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    if-eqz v1, :cond_14

    .line 460
    invoke-virtual {v1}, Ljava/lang/Process;->destroy()V

    .line 461
    iput-object v0, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    .line 463
    :cond_14
    iget-object v1, p0, Lcom/m365/gateway/GatewayService;->wakeLock:Landroid/os/PowerManager$WakeLock;

    if-eqz v1, :cond_23

    invoke-virtual {v1}, Landroid/os/PowerManager$WakeLock;->isHeld()Z

    move-result v1

    if-eqz v1, :cond_23

    .line 464
    iget-object v1, p0, Lcom/m365/gateway/GatewayService;->wakeLock:Landroid/os/PowerManager$WakeLock;

    invoke-virtual {v1}, Landroid/os/PowerManager$WakeLock;->release()V

    .line 466
    :cond_23
    iput-object v0, p0, Lcom/m365/gateway/GatewayService;->wakeLock:Landroid/os/PowerManager$WakeLock;

    return-void
.end method

.method private waitForReady()V
    .registers 4

    const/4 v0, 0x0

    :goto_1
    const/16 v1, 0x28

    if-ge v0, v1, :cond_1f

    .line 320
    invoke-static {}, Lcom/m365/gateway/GatewayService;->portOpen()Z

    move-result v1

    if-eqz v1, :cond_16

    const v0, 0x7f030004

    .line 321
    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v0

    invoke-direct {p0, v0}, Lcom/m365/gateway/GatewayService;->notify(Ljava/lang/String;)V

    return-void

    :cond_16
    const-wide/16 v1, 0x1f4

    .line 325
    :try_start_18
    invoke-static {v1, v2}, Ljava/lang/Thread;->sleep(J)V
    :try_end_1b
    .catch Ljava/lang/InterruptedException; {:try_start_18 .. :try_end_1b} :catch_1e

    add-int/lit8 v0, v0, 0x1

    goto :goto_1

    :catch_1e
    return-void

    .line 330
    :cond_1f
    const-string v0, "\u7f51\u5173\u672a\u5728\u9884\u671f\u65f6\u95f4\u5185\u5c31\u7eea\uff0c\u8bf7\u67e5\u770b\u65e5\u5fd7"

    invoke-direct {p0, v0}, Lcom/m365/gateway/GatewayService;->notify(Ljava/lang/String;)V

    return-void
.end method


# virtual methods
.method synthetic lambda$onStartCommand$0$com-m365-gateway-GatewayService()V
    .registers 4

    .line 0
    const/4 v0, 0x0

    :goto_1
    const/16 v1, 0x28

    if-ge v0, v1, :cond_1b

    .line 137
    invoke-static {}, Lcom/m365/gateway/GatewayService;->portOpen()Z

    move-result v1

    if-nez v1, :cond_1b

    const-wide/16 v1, 0x1f4

    .line 139
    :try_start_d
    invoke-static {v1, v2}, Ljava/lang/Thread;->sleep(J)V
    :try_end_10
    .catch Ljava/lang/InterruptedException; {:try_start_d .. :try_end_10} :catch_13

    add-int/lit8 v0, v0, 0x1

    goto :goto_1

    .line 141
    :catch_13
    invoke-static {}, Ljava/lang/Thread;->currentThread()Ljava/lang/Thread;

    move-result-object v0

    invoke-virtual {v0}, Ljava/lang/Thread;->interrupt()V

    return-void

    .line 145
    :cond_1b
    invoke-static {}, Lcom/m365/gateway/GatewayService;->portOpen()Z

    move-result v0

    if-eqz v0, :cond_38

    .line 146
    new-instance v0, Ljava/lang/StringBuilder;

    const-string v1, "tunnel autostart: "

    invoke-direct {v0, v1}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-static {p0}, Lcom/m365/gateway/TunnelManager;->start(Landroid/content/Context;)Ljava/lang/String;

    move-result-object v1

    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v0}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    const-string v1, "M365Gateway"

    invoke-static {v1, v0}, Landroid/util/Log;->i(Ljava/lang/String;Ljava/lang/String;)I

    :cond_38
    return-void
.end method

.method synthetic lambda$startGateway$3$com-m365-gateway-GatewayService(Ljava/io/File;)V
    .registers 2

    .line 382
    invoke-direct {p0, p1}, Lcom/m365/gateway/GatewayService;->pumpLog(Ljava/io/File;)V

    return-void
.end method

.method synthetic lambda$startHeartbeat$2$com-m365-gateway-GatewayService()V
    .registers 8

    .line 246
    :cond_0
    :goto_0
    iget-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    if-eqz v0, :cond_a7

    const-wide/16 v0, 0x61a8

    .line 248
    :try_start_6
    invoke-static {v0, v1}, Ljava/lang/Thread;->sleep(J)V
    :try_end_9
    .catch Ljava/lang/InterruptedException; {:try_start_6 .. :try_end_9} :catch_a0

    .line 253
    iget-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    if-nez v0, :cond_e

    return-void

    :cond_e
    const/4 v0, 0x0

    const/4 v1, 0x0

    .line 258
    :try_start_10
    new-instance v2, Ljava/net/URL;

    const-string v3, "http://127.0.0.1:4141/api/live"

    invoke-direct {v2, v3}, Ljava/net/URL;-><init>(Ljava/lang/String;)V

    invoke-virtual {v2}, Ljava/net/URL;->openConnection()Ljava/net/URLConnection;

    move-result-object v2

    check-cast v2, Ljava/net/HttpURLConnection;
    :try_end_1d
    .catch Ljava/lang/Exception; {:try_start_10 .. :try_end_1d} :catch_71
    .catchall {:try_start_10 .. :try_end_1d} :catchall_6f

    const/16 v1, 0xbb8

    .line 259
    :try_start_1f
    invoke-virtual {v2, v1}, Ljava/net/HttpURLConnection;->setConnectTimeout(I)V

    .line 260
    invoke-virtual {v2, v1}, Ljava/net/HttpURLConnection;->setReadTimeout(I)V

    .line 261
    const-string v1, "Connection"

    const-string v3, "close"

    invoke-virtual {v2, v1, v3}, Ljava/net/HttpURLConnection;->setRequestProperty(Ljava/lang/String;Ljava/lang/String;)V

    .line 262
    invoke-virtual {v2}, Ljava/net/HttpURLConnection;->getResponseCode()I

    move-result v1

    const/16 v3, 0x190

    if-lt v1, v3, :cond_39

    .line 265
    invoke-virtual {v2}, Ljava/net/HttpURLConnection;->getErrorStream()Ljava/io/InputStream;

    move-result-object v3

    goto :goto_3d

    :cond_39
    invoke-virtual {v2}, Ljava/net/HttpURLConnection;->getInputStream()Ljava/io/InputStream;

    move-result-object v3
    :try_end_3d
    .catch Ljava/lang/Exception; {:try_start_1f .. :try_end_3d} :catch_6d
    .catchall {:try_start_1f .. :try_end_3d} :catchall_98

    :goto_3d
    if-eqz v3, :cond_56

    const/16 v4, 0x100

    .line 267
    :try_start_41
    new-array v4, v4, [B

    .line 268
    :goto_43
    invoke-virtual {v3, v4}, Ljava/io/InputStream;->read([B)I

    move-result v5
    :try_end_47
    .catchall {:try_start_41 .. :try_end_47} :catchall_4a

    if-lez v5, :cond_56

    goto :goto_43

    :catchall_4a
    move-exception v1

    if-eqz v3, :cond_55

    .line 265
    :try_start_4d
    invoke-virtual {v3}, Ljava/io/InputStream;->close()V
    :try_end_50
    .catchall {:try_start_4d .. :try_end_50} :catchall_51

    goto :goto_55

    :catchall_51
    move-exception v3

    :try_start_52
    invoke-virtual {v1, v3}, Ljava/lang/Throwable;->addSuppressed(Ljava/lang/Throwable;)V

    :cond_55
    :goto_55
    throw v1

    :cond_56
    if-eqz v3, :cond_5b

    .line 272
    invoke-virtual {v3}, Ljava/io/InputStream;->close()V

    .line 273
    :cond_5b
    invoke-static {}, Ljava/lang/System;->currentTimeMillis()J

    move-result-wide v3

    sput-wide v3, Lcom/m365/gateway/GatewayService;->lastHeartbeatAt:J

    const/16 v3, 0xc8

    if-ne v1, v3, :cond_67

    const/4 v1, 0x1

    goto :goto_68

    :cond_67
    move v1, v0

    .line 274
    :goto_68
    sput-boolean v1, Lcom/m365/gateway/GatewayService;->lastHeartbeatOk:Z
    :try_end_6a
    .catch Ljava/lang/Exception; {:try_start_52 .. :try_end_6a} :catch_6d
    .catchall {:try_start_52 .. :try_end_6a} :catchall_98

    if-eqz v2, :cond_0

    goto :goto_93

    :catch_6d
    move-exception v1

    goto :goto_75

    :catchall_6f
    move-exception v0

    goto :goto_9a

    :catch_71
    move-exception v2

    move-object v6, v2

    move-object v2, v1

    move-object v1, v6

    .line 276
    :goto_75
    :try_start_75
    sput-boolean v0, Lcom/m365/gateway/GatewayService;->lastHeartbeatOk:Z

    .line 277
    const-string v0, "M365Gateway"

    new-instance v3, Ljava/lang/StringBuilder;

    invoke-direct {v3}, Ljava/lang/StringBuilder;-><init>()V

    const-string v4, "heartbeat failed: "

    invoke-virtual {v3, v4}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/Exception;->getMessage()Ljava/lang/String;

    move-result-object v1

    invoke-virtual {v3, v1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v1

    invoke-static {v0, v1}, Landroid/util/Log;->w(Ljava/lang/String;Ljava/lang/String;)I
    :try_end_91
    .catchall {:try_start_75 .. :try_end_91} :catchall_98

    if-eqz v2, :cond_0

    .line 280
    :goto_93
    invoke-virtual {v2}, Ljava/net/HttpURLConnection;->disconnect()V

    goto/16 :goto_0

    :catchall_98
    move-exception v0

    move-object v1, v2

    :goto_9a
    if-eqz v1, :cond_9f

    invoke-virtual {v1}, Ljava/net/HttpURLConnection;->disconnect()V

    .line 282
    :cond_9f
    throw v0

    .line 250
    :catch_a0
    invoke-static {}, Ljava/lang/Thread;->currentThread()Ljava/lang/Thread;

    move-result-object v0

    invoke-virtual {v0}, Ljava/lang/Thread;->interrupt()V

    :cond_a7
    return-void
.end method

.method synthetic lambda$startWatchdog$1$com-m365-gateway-GatewayService()V
    .registers 6

    .line 183
    :goto_0
    iget-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    if-eqz v0, :cond_80

    const-wide/16 v0, 0x3a98

    .line 185
    :try_start_6
    invoke-static {v0, v1}, Ljava/lang/Thread;->sleep(J)V
    :try_end_9
    .catch Ljava/lang/InterruptedException; {:try_start_6 .. :try_end_9} :catch_79

    .line 190
    iget-boolean v0, p0, Lcom/m365/gateway/GatewayService;->supervise:Z

    if-nez v0, :cond_e

    return-void

    .line 196
    :cond_e
    invoke-static {}, Lcom/m365/gateway/SilentKeepAlive;->active()Z

    move-result v0

    const-string v1, "M365Gateway"

    if-nez v0, :cond_1e

    .line 197
    const-string v0, "watchdog: silent keepalive was inactive; restarting it"

    invoke-static {v1, v0}, Landroid/util/Log;->w(Ljava/lang/String;Ljava/lang/String;)I

    .line 198
    invoke-static {}, Lcom/m365/gateway/SilentKeepAlive;->start()V

    .line 200
    :cond_1e
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    const/4 v2, 0x1

    if-eqz v0, :cond_2c

    invoke-virtual {v0}, Ljava/lang/Process;->isAlive()Z

    move-result v0

    if-nez v0, :cond_2a

    goto :goto_2c

    :cond_2a
    const/4 v0, 0x0

    goto :goto_2d

    :cond_2c
    :goto_2c
    move v0, v2

    .line 203
    :goto_2d
    invoke-static {}, Lcom/m365/gateway/GatewayService;->portOpen()Z

    move-result v3

    xor-int/2addr v2, v3

    if-nez v0, :cond_37

    if-nez v2, :cond_37

    goto :goto_0

    .line 207
    :cond_37
    new-instance v3, Ljava/lang/StringBuilder;

    const-string v4, "watchdog: childDead="

    invoke-direct {v3, v4}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {v3, v0}, Ljava/lang/StringBuilder;->append(Z)Ljava/lang/StringBuilder;

    const-string v0, " portDead="

    invoke-virtual {v3, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3, v2}, Ljava/lang/StringBuilder;->append(Z)Ljava/lang/StringBuilder;

    const-string v0, "; restarting"

    invoke-virtual {v3, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    invoke-static {v1, v0}, Landroid/util/Log;->w(Ljava/lang/String;Ljava/lang/String;)I

    .line 209
    :try_start_55
    iget-object v0, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    if-eqz v0, :cond_5f

    .line 210
    invoke-virtual {v0}, Ljava/lang/Process;->destroy()V

    const/4 v0, 0x0

    .line 211
    iput-object v0, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    .line 213
    :cond_5f
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->startGateway()V

    const v0, 0x7f030004

    .line 214
    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v0

    invoke-direct {p0, v0}, Lcom/m365/gateway/GatewayService;->notify(Ljava/lang/String;)V
    :try_end_6c
    .catch Ljava/lang/Exception; {:try_start_55 .. :try_end_6c} :catch_6d

    goto :goto_0

    :catch_6d
    move-exception v0

    .line 216
    const-string v2, "watchdog restart failed"

    invoke-static {v1, v2, v0}, Landroid/util/Log;->e(Ljava/lang/String;Ljava/lang/String;Ljava/lang/Throwable;)I

    .line 217
    const-string v0, "\u7f51\u5173\u5df2\u9000\u51fa\uff0c\u6b63\u5728\u91cd\u8bd5"

    invoke-direct {p0, v0}, Lcom/m365/gateway/GatewayService;->notify(Ljava/lang/String;)V

    goto :goto_0

    .line 187
    :catch_79
    invoke-static {}, Ljava/lang/Thread;->currentThread()Ljava/lang/Thread;

    move-result-object v0

    invoke-virtual {v0}, Ljava/lang/Thread;->interrupt()V

    :cond_80
    return-void
.end method

.method public onBind(Landroid/content/Intent;)Landroid/os/IBinder;
    .registers 2

    const/4 p1, 0x0

    return-object p1
.end method

.method public onCreate()V
    .registers 1

    .line 89
    invoke-super {p0}, Landroid/app/Service;->onCreate()V

    .line 90
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->createChannel()V

    return-void
.end method

.method public onDestroy()V
    .registers 1

    .line 471
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->stopGateway()V

    .line 472
    invoke-super {p0}, Landroid/app/Service;->onDestroy()V

    return-void
.end method

.method public onStartCommand(Landroid/content/Intent;II)I
    .registers 6

    if-nez p1, :cond_5

    .line 95
    const-string p1, "com.m365.gateway3.START"

    goto :goto_9

    :cond_5
    invoke-virtual {p1}, Landroid/content/Intent;->getAction()Ljava/lang/String;

    move-result-object p1

    .line 96
    :goto_9
    const-string p2, "com.m365.gateway3.STOP"

    invoke-virtual {p2, p1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z

    move-result p2

    const/4 p3, 0x2

    const/4 v0, 0x1

    if-eqz p2, :cond_23

    .line 99
    invoke-static {p0}, Lcom/m365/gateway/KeepAliveReceiver;->cancel(Landroid/content/Context;)V

    .line 100
    invoke-static {}, Lcom/m365/gateway/TunnelManager;->stop()Ljava/lang/String;

    .line 101
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->stopGateway()V

    .line 102
    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->stopForeground(Z)V

    .line 103
    invoke-virtual {p0}, Lcom/m365/gateway/GatewayService;->stopSelf()V

    return p3

    .line 106
    :cond_23
    const-string p2, "com.m365.gateway3.TUNNEL_START"

    invoke-virtual {p2, p1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z

    move-result p1

    const-string p2, "M365Gateway"

    if-eqz p1, :cond_43

    .line 109
    invoke-static {p0}, Lcom/m365/gateway/TunnelManager;->start(Landroid/content/Context;)Ljava/lang/String;

    move-result-object p1

    .line 110
    new-instance p3, Ljava/lang/StringBuilder;

    const-string v1, "tunnel start: "

    invoke-direct {p3, v1}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {p3, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {p3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p1

    invoke-static {p2, p1}, Landroid/util/Log;->i(Ljava/lang/String;Ljava/lang/String;)I

    return v0

    :cond_43
    const p1, 0x7f030005

    .line 113
    invoke-virtual {p0, p1}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object p1

    invoke-direct {p0, p1}, Lcom/m365/gateway/GatewayService;->buildNotification(Ljava/lang/String;)Landroid/app/Notification;

    move-result-object p1

    const/16 v1, 0x102d

    invoke-virtual {p0, v1, p1}, Lcom/m365/gateway/GatewayService;->startForeground(ILandroid/app/Notification;)V

    .line 117
    invoke-static {}, Lcom/m365/gateway/SilentKeepAlive;->start()V

    .line 120
    invoke-static {p0}, Lcom/m365/gateway/KeepAliveReceiver;->schedule(Landroid/content/Context;)V

    .line 121
    iget-object p1, p0, Lcom/m365/gateway/GatewayService;->proc:Ljava/lang/Process;

    if-eqz p1, :cond_63

    invoke-virtual {p1}, Ljava/lang/Process;->isAlive()Z

    move-result p1

    if-nez p1, :cond_66

    .line 123
    :cond_63
    :try_start_63
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->startGateway()V
    :try_end_66
    .catch Ljava/lang/Exception; {:try_start_63 .. :try_end_66} :catch_97

    .line 130
    :cond_66
    new-instance p1, Ljava/lang/Thread;

    new-instance p2, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda3;

    invoke-direct {p2, p0}, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda3;-><init>(Lcom/m365/gateway/GatewayService;)V

    const-string p3, "gateway-ready"

    invoke-direct {p1, p2, p3}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    invoke-virtual {p1}, Ljava/lang/Thread;->start()V

    .line 131
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->startWatchdog()V

    .line 132
    invoke-direct {p0}, Lcom/m365/gateway/GatewayService;->startHeartbeat()V

    .line 135
    invoke-static {p0}, Lcom/m365/gateway/TunnelManager;->autoStart(Landroid/content/Context;)Z

    move-result p1

    if-eqz p1, :cond_96

    invoke-static {}, Lcom/m365/gateway/TunnelManager;->running()Z

    move-result p1

    if-nez p1, :cond_96

    .line 136
    new-instance p1, Ljava/lang/Thread;

    new-instance p2, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda4;

    invoke-direct {p2, p0}, Lcom/m365/gateway/GatewayService$$ExternalSyntheticLambda4;-><init>(Lcom/m365/gateway/GatewayService;)V

    const-string p3, "tunnel-autostart"

    invoke-direct {p1, p2, p3}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V

    .line 148
    invoke-virtual {p1}, Ljava/lang/Thread;->start()V

    :cond_96
    return v0

    :catch_97
    move-exception p1

    .line 125
    const-string v0, "start failed"

    invoke-static {p2, v0, p1}, Landroid/util/Log;->e(Ljava/lang/String;Ljava/lang/String;Ljava/lang/Throwable;)I

    .line 126
    new-instance p2, Ljava/lang/StringBuilder;

    invoke-direct {p2}, Ljava/lang/StringBuilder;-><init>()V

    const v0, 0x7f030006

    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->getString(I)Ljava/lang/String;

    move-result-object v0

    invoke-virtual {p2, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    const-string v0, " \u00b7 "

    invoke-virtual {p2, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {p1}, Ljava/lang/Exception;->getMessage()Ljava/lang/String;

    move-result-object p1

    invoke-virtual {p2, p1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {p2}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object p1

    invoke-direct {p0, p1}, Lcom/m365/gateway/GatewayService;->notify(Ljava/lang/String;)V

    return p3
.end method

.method public onTaskRemoved(Landroid/content/Intent;)V
    .registers 5

    .line 305
    new-instance v0, Landroid/content/Intent;

    const-class v1, Lcom/m365/gateway/GatewayService;

    invoke-direct {v0, p0, v1}, Landroid/content/Intent;-><init>(Landroid/content/Context;Ljava/lang/Class;)V

    const-string v1, "com.m365.gateway3.START"

    invoke-virtual {v0, v1}, Landroid/content/Intent;->setAction(Ljava/lang/String;)Landroid/content/Intent;

    move-result-object v0

    .line 308
    :try_start_d
    invoke-virtual {p0, v0}, Lcom/m365/gateway/GatewayService;->startForegroundService(Landroid/content/Intent;)Landroid/content/ComponentName;
    :try_end_10
    .catch Ljava/lang/Exception; {:try_start_d .. :try_end_10} :catch_11

    goto :goto_29

    :catch_11
    move-exception v0

    .line 313
    new-instance v1, Ljava/lang/StringBuilder;

    const-string v2, "onTaskRemoved restart refused: "

    invoke-direct {v1, v2}, Ljava/lang/StringBuilder;-><init>(Ljava/lang/String;)V

    invoke-virtual {v0}, Ljava/lang/Exception;->getMessage()Ljava/lang/String;

    move-result-object v0

    invoke-virtual {v1, v0}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;

    invoke-virtual {v1}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;

    move-result-object v0

    const-string v1, "M365Gateway"

    invoke-static {v1, v0}, Landroid/util/Log;->w(Ljava/lang/String;Ljava/lang/String;)I

    .line 315
    :goto_29
    invoke-static {p0}, Lcom/m365/gateway/KeepAliveReceiver;->schedule(Landroid/content/Context;)V

    .line 316
    invoke-super {p0, p1}, Landroid/app/Service;->onTaskRemoved(Landroid/content/Intent;)V

    return-void
.end method

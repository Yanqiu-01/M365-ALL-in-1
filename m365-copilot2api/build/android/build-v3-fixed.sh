#!/usr/bin/env bash
# 用恢复的源码编译 libm365.so，替换进 APK，产出含修复且可正常启动的 v3。
#
# 关键事实：libm365.so 不是 JNI 库，而是被 GatewayService 以
# ProcessBuilder 启动的 PIE 可执行文件（只导出 main.main、
# INTERP=/system/bin/linker64）。恢复的 cmd/server 正是同一形态。
#
# 必须用 -buildmode=pie。-buildmode=exe 会报：
#   runtime.gcdata: missing Go type information for global symbol .dynsym
#
# 这份脚本还固化了两个重打包陷阱：
#   1. manifest package 改名后，组件不能继续使用相对类名，
#      因为 dex 中的类仍位于 com.m365.gateway.*；
#   2. apktool 会把 lib/*.so 的 ZIP 执行权限清成 000，
#      而 Java 用 ProcessBuilder 直接执行 libm365.so，必须恢复 0700。
set -euo pipefail

SRC_INPUT=${1:?用法: build-v3-fixed.sh <原始 base.apk> [输出目录]}
OUT_INPUT=${2:-./out-v3}
REPO=$(cd "$(dirname "$0")/../.." && pwd)
SRC=$(realpath "$SRC_INPUT")
OUT=$(realpath -m "$OUT_INPUT")
if [ ! -f "$SRC" ]; then
  echo "原始 APK 不存在: $SRC" >&2
  exit 1
fi
NEW_PKG=com.m365.gateway.pkcego
OLD_PKG=com.m365.gateway3
NEW_LABEL='修改版M365'
VERSION_CODE=2
VERSION_NAME=1.0.1
APK_NAME=修改版M365-v1.0.1-arm64.apk
BUILD_COMMIT=${BUILD_COMMIT:-$(git -C "$REPO" rev-parse --short=12 HEAD 2>/dev/null || printf unknown)}
BUILD_TIME=${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
LDFLAGS="-s -w -X m365-copilot2api/internal/web.Version=$VERSION_NAME -X m365-copilot2api/internal/web.Commit=$BUILD_COMMIT -X m365-copilot2api/internal/web.BuildTime=$BUILD_TIME"
# 密钥库必须在所有版本间保持同一份：此前默认落在 $OUT/...，而每版用独立输出
# 目录，keytool 每次都新生成一份密钥，导致每版签名都不一样，升级时必须先卸载。
# 签名一致才能覆盖安装并保留数据。
#
# 同时它绝不能进入 git：仓库一旦公开，任何人都能用这份密钥签出可覆盖安装的
# 冒充版本。默认路径因此放在仓库之外，并由 .gitignore 兜底屏蔽 build/android/keys/。
# 口令同理，不再硬编码 —— 通过环境变量提供，缺失时给出明确提示。
KS=${KS:-${M365_KEYSTORE:-$HOME/.m365-gateway/m365-gateway-v2.jks}}
KS_ALIAS=${KS_ALIAS:-m365v2}
if [ -z "${KS_PASS:-}" ]; then
  printf '%s\n' \
    "错误：未提供密钥库口令。" \
    "  请设置 KS_PASS，例如：KS_PASS=你的口令 bash $0 ..." \
    "  密钥库路径由 KS（或 M365_KEYSTORE）指定，当前解析为：$KS" \
    "  注意：口令与密钥库都不得提交进仓库。" >&2
  exit 2
fi

# go.mod 声明 go 1.23，本机容器实际提供 golang:1.26。优先使用项目固化的
# 工具链，避免系统 Go 触发联网 toolchain 自动下载；也允许调用方通过 GO_BIN 覆盖。
if [ -z "${GO_BIN:-}" ]; then
  if [ -x /workspace/toolchain/go1.23/bin/go ]; then
    GO_BIN=/workspace/toolchain/go1.23/bin/go
  elif [ -x /workspace/toolchain/go1.26/bin/go ]; then
    GO_BIN=/workspace/toolchain/go1.26/bin/go
  else
    GO_BIN=go
  fi
fi
if [[ "$GO_BIN" == */* ]]; then
  GO_BIN=$(realpath "$GO_BIN")
fi
if ! command -v "$GO_BIN" >/dev/null 2>&1 && [ ! -x "$GO_BIN" ]; then
  echo "找不到 Go 编译器: $GO_BIN" >&2
  exit 1
fi

mkdir -p "$OUT"

printf '%s\n' '== 1/6 交叉编译 libm365.so =='
(
  cd "$REPO"
  GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0 GOOS=android GOARCH=arm64 GOARM64=v8.0 \
    "$GO_BIN" build -trimpath -buildvcs=false -buildmode=pie -ldflags "$LDFLAGS" -o "$OUT/libm365.so" ./cmd/server
)
readelf -h "$OUT/libm365.so" | grep -E 'Type|Machine|Entry point'
readelf -p .interp "$OUT/libm365.so" | grep -oE '/[a-z/0-9._]+'

printf '%s\n' '== 2/6 反编译 APK =='
rm -rf "$OUT/work"
apktool d -f -o "$OUT/work" "$SRC"

printf '%s\n' '== 3/6 替换 .so、同步 web 资源、改包名与组件 =='
cp "$OUT/libm365.so" "$OUT/work/lib/arm64-v8a/libm365.so"
for f in index.html login.html debug.html panel.html workbench.html; do
  [ -f "$REPO/web/$f" ] && cp "$REPO/web/$f" "$OUT/work/assets/web/$f"
done

PATCHER="$OUT/apkpatcher"
GOTOOLCHAIN=local GOPROXY=off "$GO_BIN" build -trimpath -buildvcs=false -o "$PATCHER" "$REPO/build/android/apkpatcher"
"$PATCHER" identity "$OUT/work" "$OLD_PKG" "$NEW_PKG" "$NEW_LABEL" "$VERSION_CODE" "$VERSION_NAME"
"$PATCHER" smali "$OUT/work"

# 诊断页 cookie 持久化补丁已停用：2.24.15 实测点击「网关诊断」直接闪退。
# 注入位置在构造函数与登录回调内，寄存器/异常表处理不当会导致 Activity
# 初始化即崩溃。保留脚本供后续验证，但不再参与构建。
# apkpatcher 不再包含诊断页 cookie 补丁：2.24.15 实测点击「网关诊断」直接闪退。

# 3.0.0 稳定版保留原 APK 已存在的原生 OAuth 按钮与 AuthActivity，
# 不再注入“网关就绪后自动拉起 OAuth”。2.24.25-first-run-oauth-safe
# 已在真机出现启动闪退，而同 DEX 基线的 native-oauth 版没有这两个注入方法。
# 用户可在主界面或 /panel 手动启动授权，成功后仍由现有 PKCE 回调保存令牌。
if grep -Rqs 'maybeStartFirstRunAuthorization' "$OUT/work"/smali*; then
  echo '错误：基线 APK 已包含不稳定的首次启动 OAuth 注入，请改用 native-oauth 基线' >&2
  exit 1
fi

# 原始 APK 的管理密码资源中含有用户曾提供的密码。每次构建生成独立随机管理密码,
# 恢复值只写入输出目录中的 0600 文件,不将用户的 Microsoft 账号密码打入 APK。
"$PATCHER" password "$OUT/work" "$OUT/local-admin-password.txt"

printf '%s\n' '== 4/6 打包（必须使用 aapt2）=='
apktool b "$OUT/work" --use-aapt2 -o "$OUT/unsigned.apk"

# apktool 会丢失 native ZIP entry 的执行权限。恢复为原 APK 的 0700，
# 再交给 zipalign；否则 ProcessBuilder 可能因权限不足启动失败。
"$PATCHER" zipmode "$OUT/unsigned.apk" "$OUT/unsigned-mode.apk"
zipalign -p -f 4 "$OUT/unsigned-mode.apk" "$OUT/aligned.apk"
zipalign -c 4 "$OUT/aligned.apk" >/dev/null

printf '%s\n' '== 5/6 签名 =='
if [ ! -f "$KS" ]; then
  mkdir -p "$(dirname "$KS")"
  keytool -genkeypair -v -keystore "$KS" -alias "$KS_ALIAS" \
    -keyalg RSA -keysize 4096 -validity 10950 \
    -storepass "$KS_PASS" -keypass "$KS_PASS" \
    -dname "CN=M365 Gateway v2, OU=Recovery, O=Self-Signed, C=CN"
  echo "已生成密钥库 $KS —— 请立即备份到仓库之外，且不要提交进 git"
fi
printf '签名密钥指纹: '
keytool -list -keystore "$KS" -storepass "$KS_PASS" 2>/dev/null | grep -oE '\(SHA-256\): [0-9A-F:]+' || true
apksigner sign --ks "$KS" --ks-key-alias "$KS_ALIAS" \
  --ks-pass "pass:$KS_PASS" --key-pass "pass:$KS_PASS" \
  --v1-signing-enabled true --v2-signing-enabled true --v3-signing-enabled true \
  --out "$OUT/$APK_NAME" "$OUT/aligned.apk"

printf '%s\n' '== 6/6 验证 =='
apksigner verify "$OUT/$APK_NAME"
aapt dump badging "$OUT/$APK_NAME" | grep -E '^package|application-label|launchable-activity|native-code'
"$PATCHER" verify "$SRC" "$OUT/$APK_NAME"
(
  cd "$OUT"
  sha256sum "$APK_NAME" | tee "$APK_NAME.sha256"
)
echo "完成：$OUT/$APK_NAME"

#!/bin/sh
# 在 golang 容器内交叉编译 Windows 版网关。
# net_mobile.go 带 //go:build android，因此不会进入本次构建，
# 其 init() 也就不会把进程 DNS 换成硬编码公共解析器。
#
# 脚本位置为 build/pc/，仓库根在其上两级；无论从哪个目录调用都先切回仓库根，
# 这样 ./cmd/server 与 .build-out/ 都稳定指向仓库根。
set -e

REPO=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO"

export GOPROXY=https://goproxy.cn,direct
export GOFLAGS=-mod=mod
export CGO_ENABLED=0

echo "=== go mod download ==="
go mod download

echo "=== go vet (linux, 只用于查错) ==="
go vet ./... 2>&1 | tail -30 || echo "VET_WARNINGS_ABOVE"

echo "=== go test ==="
go test ./... 2>&1 | tail -40 || echo "TEST_FAILURES_ABOVE"

echo "=== build windows/amd64 ==="
mkdir -p .build-out
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -H windowsgui" \
  -o .build-out/m365-gateway-pc.exe ./cmd/server

echo BUILD_OK
ls -la .build-out/m365-gateway-pc.exe
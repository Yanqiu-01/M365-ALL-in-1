#!/bin/sh
# 在 golang 容器内交叉编译 Windows 版网关。
# net_mobile.go 带 //go:build android，因此不会进入本次构建，
# 其 init() 也就不会把进程 DNS 换成硬编码公共解析器。
set -e

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
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -H windowsgui" \
  -o .build-out/m365-gateway-pc.exe ./cmd/server

echo BUILD_OK
ls -la .build-out/m365-gateway-pc.exe

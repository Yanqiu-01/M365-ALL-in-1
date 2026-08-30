package main

import (
	"net"
	"strings"
)

// loopbackListenAddr 判断一个监听地址是否只对本机可见。
//
// 用途是守住内建 FlareSolverr：它自带 http.Server，不经过网关中间件，也没有任何
// 鉴权，绑到非回环地址等于把一个无鉴权接口摆到局域网上。
//
// 判定刻意从严 —— 只认明确的回环地址：
//   - 空主机（":8191"）和 "0.0.0.0" / "[::]" 都是通配，一律拒绝；
//   - 主机名只接受 localhost，其它名字不做 DNS 解析（解析结果可能随时变化，
//     也可能把启动流程卡在网络查询上）。
func loopbackListenAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 没有端口时按整串当主机看待，仍然要过同一套判定。
		host = strings.Trim(addr, "[]")
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback()
}

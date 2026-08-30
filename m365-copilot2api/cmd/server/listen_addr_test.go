package main

import "testing"

// 内建 FlareSolverr 无鉴权，所以「哪些地址算本机」这条判定就是它唯一的边界。
// 用户此前有过一次真实事故：服务在局域网上以无密码状态短暂暴露。通配地址必须
// 判否，这是这组用例存在的理由。
func TestLoopbackListenAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8191", true},
		{"127.0.0.1", true},
		{"127.5.6.7:8191", true}, // 整个 127/8 都是回环
		{"localhost:8191", true},
		{"LocalHost:8191", true},
		{"[::1]:8191", true},
		{"::1", true},

		{"0.0.0.0:8191", false},
		{"[::]:8191", false},
		{":8191", false}, // 空主机等于全网通配，最容易误用的一种写法
		{"", false},
		{"192.168.1.20:8191", false},
		{"10.250.223.140:8080", false},
		{"rikkahub.local:8191", false}, // 主机名不做 DNS 解析
		{"example.com:8191", false},
	}
	for _, c := range cases {
		if got := loopbackListenAddr(c.addr); got != c.want {
			t.Errorf("loopbackListenAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

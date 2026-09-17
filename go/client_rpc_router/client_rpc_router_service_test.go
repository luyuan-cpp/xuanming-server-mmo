package main

import (
	"testing"
)

// 注册地址必须能被其他节点拨号，监听通配地址不能直接写进 etcd。
func TestAdvertisedHost(t *testing.T) {
	for _, tc := range []struct {
		name       string
		podIP      string
		listenHost string
		want       string
	}{
		{"PodIP覆盖通配监听", "10.2.3.4", "0.0.0.0", "10.2.3.4"},
		{"PodIP覆盖具体监听", "10.2.3.4", "127.0.0.1", "10.2.3.4"},
		{"保留本地监听", "", "127.0.0.1", "127.0.0.1"},
		{"保留明确地址", "", "10.2.3.5", "10.2.3.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POD_IP", tc.podIP)
			if got := advertisedHost(tc.listenHost); got != tc.want {
				t.Fatalf("注册地址 = %q，期望 %q", got, tc.want)
			}
		})
	}
	t.Run("通配监听回退到具体地址", func(t *testing.T) {
		t.Setenv("POD_IP", "")
		for _, host := range []string{"", "0.0.0.0", "::"} {
			if got := advertisedHost(host); isUnspecifiedHost(got) {
				t.Fatalf("监听 %q 注册成不可拨号的地址 %q", host, got)
			}
		}
	})
}

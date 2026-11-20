package main

import (
	"testing"

	base "proto/common/base"
)

// C++ scene 按枚举名派生的前缀发现本服务,部署脚本和网络策略也写着这个名字:数值和名字都不能悄悄变。
func TestNodeTypeAndDiscoveryPrefix(t *testing.T) {
	if uint32(nodeType) != 32 {
		t.Fatalf("ContentGuardNodeService should be 32, got %d", uint32(nodeType))
	}
	if got := base.ENodeType_name[int32(nodeType)] + ".rpc"; got != "ContentGuardNodeService.rpc" {
		t.Fatalf("discovery prefix should be ContentGuardNodeService.rpc, got %q", got)
	}
}

func TestParseListenOn(t *testing.T) {
	host, port := parseListenOn("127.0.0.1:50250")
	if host != "127.0.0.1" || port != 50250 {
		t.Fatalf("parseListenOn = %q, %d", host, port)
	}
	if host, port = parseListenOn("0.0.0.0:1"); !isUnspecifiedHost(host) || port != 1 {
		t.Fatalf("0.0.0.0 must be treated as unspecified: %q, %d", host, port)
	}
}

func TestAdvertisedHostPrefersPodIP(t *testing.T) {
	t.Setenv("POD_IP", "10.1.2.3")
	if got := advertisedHost("0.0.0.0"); got != "10.1.2.3" {
		t.Fatalf("advertisedHost = %q, want the POD_IP", got)
	}
	t.Setenv("POD_IP", "")
	if got := advertisedHost("127.0.0.1"); got != "127.0.0.1" {
		t.Fatalf("advertisedHost = %q, want the listen host", got)
	}
}

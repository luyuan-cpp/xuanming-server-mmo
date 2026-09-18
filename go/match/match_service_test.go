package main

import (
	"encoding/json"
	"testing"

	base "proto/common/base"
)

// TestNodeInfoValueMatchesRegistryContract:TeamNodeService 的 BuildValue 产物必须通过
// shared/noderegistry 的注册前校验(nodeId==nodeID、nodeType==NodeType、zoneId==ZoneId、nodeUuid 原样、
// endpoint.port / grpcEndpoint.port 非 0、protocolType 为 PROTOCOL_GRPC),否则第二次注册会被拒、
// 路由服发现不到组队服务。按 protojson 的 JSON 字段名解到镜像 struct(照 go/chat/chat_test.go)。
func TestNodeInfoValueMatchesRegistryContract(t *testing.T) {
	build := nodeInfoValueBuilder(base.ENodeType_TeamNodeService, 3, "10.0.0.9", 50500, 1_700_000_000)
	raw, err := build(11, "uuid-team-test")
	if err != nil {
		t.Fatalf("BuildValue 失败: %v", err)
	}

	var mirror struct {
		NodeId       uint32 `json:"nodeId"`
		NodeType     uint32 `json:"nodeType"`
		ZoneId       uint32 `json:"zoneId"`
		NodeUuid     string `json:"nodeUuid"`
		ProtocolType uint32 `json:"protocolType"`
		Endpoint     struct {
			Ip   string `json:"ip"`
			Port uint32 `json:"port"`
		} `json:"endpoint"`
		GrpcEndpoint struct {
			Ip   string `json:"ip"`
			Port uint32 `json:"port"`
		} `json:"grpcEndpoint"`
	}
	if err := json.Unmarshal(raw, &mirror); err != nil {
		t.Fatalf("NodeInfo protojson 无法按镜像 struct 解析: %v\n%s", err, raw)
	}
	if mirror.NodeId != 11 || mirror.ZoneId != 3 || mirror.NodeUuid != "uuid-team-test" {
		t.Fatalf("身份字段不符: %+v", mirror)
	}
	if mirror.NodeType != uint32(base.ENodeType_TeamNodeService) || mirror.NodeType != 10 {
		t.Fatalf("nodeType 应为 TeamNodeService(10),实际 %d", mirror.NodeType)
	}
	if mirror.ProtocolType != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) {
		t.Fatalf("protocolType 应为 PROTOCOL_GRPC,实际 %d", mirror.ProtocolType)
	}
	if mirror.Endpoint.Port != 50500 || mirror.GrpcEndpoint.Port != 50500 ||
		mirror.Endpoint.Ip != "10.0.0.9" || mirror.GrpcEndpoint.Ip != "10.0.0.9" {
		t.Fatalf("端点字段不符: %+v", mirror)
	}
}

// TestAdvertisedHost:K8s 上 ListenOn 恒为 0.0.0.0,写进 NodeInfo 的必须是 POD_IP(§A.3 第 3 条)。
func TestAdvertisedHost(t *testing.T) {
	t.Run("POD_IP 优先", func(t *testing.T) {
		t.Setenv("POD_IP", "10.1.2.3")
		if got := advertisedHost("0.0.0.0"); got != "10.1.2.3" {
			t.Fatalf("advertisedHost(0.0.0.0) = %q,期望 POD_IP", got)
		}
		if got := advertisedHost("127.0.0.1"); got != "10.1.2.3" {
			t.Fatalf("advertisedHost(127.0.0.1) = %q,期望 POD_IP", got)
		}
	})
	t.Run("无 POD_IP 时具体 host 原样使用", func(t *testing.T) {
		t.Setenv("POD_IP", "")
		if got := advertisedHost("127.0.0.1"); got != "127.0.0.1" {
			t.Fatalf("advertisedHost(127.0.0.1) = %q", got)
		}
	})
	t.Run("无 POD_IP 时监听全部网卡不得原样注册", func(t *testing.T) {
		t.Setenv("POD_IP", "")
		for _, host := range []string{"", "0.0.0.0", "::"} {
			if got := advertisedHost(host); isUnspecifiedHost(got) {
				t.Fatalf("advertisedHost(%q) = %q,仍是未指定地址", host, got)
			}
		}
	})
}

func TestParseListenOn(t *testing.T) {
	host, port := parseListenOn("0.0.0.0:50500")
	if host != "0.0.0.0" || port != 50500 {
		t.Fatalf("parseListenOn = (%q, %d)", host, port)
	}
}

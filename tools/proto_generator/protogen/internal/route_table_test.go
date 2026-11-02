package internal

import (
	"strings"
	"testing"

	messageoption "github.com/luyuancpp/protooption"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// routeTestService 造一个只有一个方法的 gRPC service。
func routeTestService(fileName, serviceName string, clientProtocol bool) *RPCServiceInfo {
	generic := false
	service := &descriptorpb.ServiceDescriptorProto{Name: &serviceName}
	if clientProtocol {
		service.Options = &descriptorpb.ServiceOptions{}
		proto.SetExtension(service.Options, messageoption.E_OptionIsClientProtocolService, true)
	}
	info := ProtoFileInfo{
		Fd:                     &descriptorpb.FileDescriptorProto{Name: &fileName, Options: &descriptorpb.FileOptions{CcGenericServices: &generic}},
		ServiceDescriptorProto: service,
	}
	methodName := "Call"
	method := &MethodInfo{ProtoFileInfo: info, MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: &methodName}}
	return &RPCServiceInfo{ProtoFileInfo: info, Methods: RPCMethods{method}}
}

func withNodeEnumValues(t *testing.T, values map[string]bool) {
	t.Helper()
	original := NodeEnumValueSet
	t.Cleanup(func() { NodeEnumValueSet = original })
	NodeEnumValueSet = values
}

// RoleFenceParticipant 由六个业务服各自注册,节点类型枚举里没有它。收进表就是引用不存在的枚举值,路由服编译不过。
func TestRouteEntriesSkipInternalServiceWithoutNodeType(t *testing.T) {
	withNodeEnumValues(t, map[string]bool{"MatchNodeService": true})
	entries, err := collectRouteEntries(RPCServiceInfoList{
		routeTestService("proto/role_fence/role_fence.proto", "RoleFenceParticipant", false),
		routeTestService("proto/match/match.proto", "MatchService", true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("只有对得上节点类型的 service 进表: %+v", entries)
	}
	if got := entries[0]; got.service != "MatchService" || got.nodeEnum != "base.ENodeType_MatchNodeService" || !got.clientProtocol {
		t.Fatalf("路由条目不对: %+v", got)
	}
}

// 客户端协议对不上节点类型时不能悄悄跳过:那样客户端的请求全部被路由服拒绝,生成期却没有任何提示。
func TestRouteEntriesRejectClientServiceWithoutNodeType(t *testing.T) {
	withNodeEnumValues(t, map[string]bool{"MatchNodeService": true})
	_, err := collectRouteEntries(RPCServiceInfoList{routeTestService("proto/lobby/lobby.proto", "LobbyService", true)})
	if err == nil || !strings.Contains(err.Error(), "LobbyService") {
		t.Fatalf("客户端协议没有节点类型必须让生成失败: %v", err)
	}
}

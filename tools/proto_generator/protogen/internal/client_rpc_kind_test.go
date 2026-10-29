package internal

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	messageoption "github.com/luyuancpp/protooption"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func clientRpcRegressionService() (*RPCServiceInfo, *MethodInfo) {
	service := &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("GuildService"), Options: &descriptorpb.ServiceOptions{},
	}
	proto.SetExtension(service.Options, messageoption.E_OptionIsClientProtocolService, true)
	method := &descriptorpb.MethodDescriptorProto{
		Name: proto.String("Call"), InputType: proto.String(".fixture.Request"), OutputType: proto.String(".fixture.Response"),
		Options: &descriptorpb.MethodOptions{},
	}
	service.Method = []*descriptorpb.MethodDescriptorProto{method}
	file := &descriptorpb.FileDescriptorProto{Name: proto.String("proto/test/client.proto"), Service: []*descriptorpb.ServiceDescriptorProto{service}}
	info := ProtoFileInfo{Fd: file, ServiceDescriptorProto: service}
	mi := &MethodInfo{ProtoFileInfo: info, MethodDescriptorProto: method, Id: 20}
	return &RPCServiceInfo{ProtoFileInfo: info, Methods: RPCMethods{mi}}, mi
}

func clientRpcRegressionSetKind(method *MethodInfo, value uint64) {
	method.MethodDescriptorProto.Options.ProtoReflect().SetUnknown(protowire.AppendVarint(
		protowire.AppendTag(nil, OptionClientRpcKindNumber, protowire.VarintType), value))
}

func TestClientRpcKindsRejectInvalidContractsWithMethodIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		scene  bool
		mutate func(*RPCServiceInfo, *MethodInfo)
	}{
		{"Notify必须标推送", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			method.MethodDescriptorProto.Name = proto.String("NotifyChanged")
		}},
		{"推送必须有Notify前缀", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			clientRpcRegressionSetKind(method, uint64(ClientRpcPush))
			method.MethodDescriptorProto.OutputType = proto.String(".Empty")
		}},
		{"单向响应必须精确Empty", true, func(_ *RPCServiceInfo, method *MethodInfo) {
			clientRpcRegressionSetKind(method, uint64(ClientRpcOneWay))
			method.MethodDescriptorProto.OutputType = proto.String(".fixture.LoginEmptyResponse")
		}},
		{"grpc不得单向", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			clientRpcRegressionSetKind(method, uint64(ClientRpcOneWay))
			method.MethodDescriptorProto.OutputType = proto.String(".Empty")
		}},
		{"scene的Empty不能假装请求应答", true, func(_ *RPCServiceInfo, method *MethodInfo) {
			method.MethodDescriptorProto.OutputType = proto.String(".Empty")
		}},
		{"推送响应形态错误", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			clientRpcRegressionSetKind(method, uint64(ClientRpcPush))
			method.MethodDescriptorProto.Name = proto.String("NotifyChanged")
		}},
		{"非客户端服务不得标客户端形态", false, func(service *RPCServiceInfo, method *MethodInfo) {
			service.ServiceDescriptorProto.Options = nil
			clientRpcRegressionSetKind(method, uint64(ClientRpcRequest))
		}},
		{"枚举值越界", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			clientRpcRegressionSetKind(method, 4)
		}},
		{"选项wire类型错误", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			method.MethodDescriptorProto.Options.ProtoReflect().SetUnknown(protowire.AppendBytes(
				protowire.AppendTag(nil, OptionClientRpcKindNumber, protowire.BytesType), []byte("0")))
		}},
		{"选项字节损坏", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			method.MethodDescriptorProto.Options.ProtoReflect().SetUnknown(
				protowire.AppendTag(nil, OptionClientRpcKindNumber, protowire.VarintType))
		}},
		{"消息号尚未分配", false, func(_ *RPCServiceInfo, method *MethodInfo) {
			method.Id = math.MaxUint64
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, method := clientRpcRegressionService()
			test.mutate(service, method)
			classified, err := ClassifyClientRpcServices(RPCServiceInfoList{service}, func(*MethodInfo) bool { return test.scene })
			if err == nil || classified != nil {
				t.Fatalf("非法契约必须拒绝且不返回部分结果: classified=%v err=%v", classified, err)
			}
			identity := service.Service() + "." + method.Method()
			if !strings.Contains(err.Error(), identity) {
				t.Errorf("错误必须包含服务.方法 %s: %v", identity, err)
			}
		})
	}
}

func TestClientRpcKindsAcceptExactEmptyGrpcReplyAndZeroMessageID(t *testing.T) {
	service, method := clientRpcRegressionService()
	method.Id = 0
	method.MethodDescriptorProto.OutputType = proto.String(".Empty")
	classified, err := ClassifyClientRpcServices(RPCServiceInfoList{service}, func(*MethodInfo) bool { return false })
	if err != nil || len(classified) != 1 || len(classified[0].Methods) != 1 || classified[0].Methods[0].Kind != ClientRpcRequest {
		t.Fatalf("gRPC Empty 应答仍是请求应答，消息号0合法: classified=%v err=%v", classified, err)
	}
}

func TestClientRpcKindsDefaultAndRepeatedUnknownOption(t *testing.T) {
	method := &descriptorpb.MethodDescriptorProto{}
	actual, explicit, err := ReadClientRpcKind(method)
	if err != nil || actual != ClientRpcRequest || explicit {
		t.Fatalf("没有option应默认REQUEST: actual=%v explicit=%v err=%v", actual, explicit, err)
	}
	method.Options = &descriptorpb.MethodOptions{}
	wire := protowire.AppendVarint(protowire.AppendTag(nil, OptionClientRpcKindNumber, protowire.VarintType), uint64(ClientRpcPush))
	wire = protowire.AppendVarint(protowire.AppendTag(wire, OptionClientRpcKindNumber, protowire.VarintType), uint64(ClientRpcOneWay))
	method.Options.ProtoReflect().SetUnknown(wire)
	actual, explicit, err = ReadClientRpcKind(method)
	if err != nil || actual != ClientRpcOneWay || !explicit {
		t.Fatalf("重复单值option按protobuf最后值语义读取: actual=%v explicit=%v err=%v", actual, explicit, err)
	}
}

// clientRpcRegressionAddMethod 向夹具服务追加一个方法:描述符与 MethodInfo 共用指针(同流水线),消息号顺延。
func clientRpcRegressionAddMethod(service *RPCServiceInfo, name, input, output string) *MethodInfo {
	method := &descriptorpb.MethodDescriptorProto{
		Name: proto.String(name), InputType: proto.String(input), OutputType: proto.String(output),
		Options: &descriptorpb.MethodOptions{},
	}
	service.ServiceDescriptorProto.Method = append(service.ServiceDescriptorProto.Method, method)
	info := &MethodInfo{ProtoFileInfo: service.ProtoFileInfo, MethodDescriptorProto: method, Id: 20 + uint64(len(service.Methods))}
	service.Methods = append(service.Methods, info)
	return info
}

// clientRpcRegressionViolations 分类并要求得到 *ClientRpcValidationError(不带部分结果),返回其中的全部违例;
// 同时核对错误文本逐条带出了 服务.方法。
func clientRpcRegressionViolations(t *testing.T, services RPCServiceInfoList, scene bool) []ClientRpcViolation {
	t.Helper()
	classified, err := ClassifyClientRpcServices(services, func(*MethodInfo) bool { return scene })
	var validation *ClientRpcValidationError
	if classified != nil || !errors.As(err, &validation) {
		t.Fatalf("违例必须以 *ClientRpcValidationError 返回且不带部分结果: classified=%v err=%v", classified, err)
	}
	for _, violation := range validation.Violations {
		if identity := violation.Service + "." + violation.Method; !strings.Contains(err.Error(), identity) {
			t.Errorf("错误文本缺少违例方法 %s: %v", identity, err)
		}
	}
	return validation.Violations
}

func TestClientRpcKindsCollectEveryViolationInOneError(t *testing.T) {
	const (
		fixtureService = "GuildService"
		fixtureFile    = "proto/test/client.proto"
	)
	t.Run("两个方法各违反一条", func(t *testing.T) {
		service, notify := clientRpcRegressionService()
		// 第一个方法:Notify 前缀却是默认 REQUEST。
		notify.MethodDescriptorProto.Name = proto.String("NotifyChanged")
		// 第二个方法:消息号未分配。
		retired := clientRpcRegressionAddMethod(service, "Retired", ".fixture.Request", ".fixture.Response")
		retired.Id = math.MaxUint64
		violations := clientRpcRegressionViolations(t, RPCServiceInfoList{service}, false)
		want := []string{"NotifyChanged", "Retired"}
		if len(violations) != len(want) {
			t.Fatalf("每个违例方法各记一条,不能在第一条就返回: got %v", violations)
		}
		for index, method := range want {
			if got := violations[index]; got.Service != fixtureService || got.Method != method || got.File != fixtureFile {
				t.Errorf("第 %d 条违例定位错误: got %+v, want %s.%s (%s)", index, got, fixtureService, method, fixtureFile)
			}
		}
	})
	t.Run("一个方法违反两条", func(t *testing.T) {
		service, method := clientRpcRegressionService()
		// gRPC 路径上标 ONE_WAY,且响应不是 Empty:两条原因各记一条。
		clientRpcRegressionSetKind(method, uint64(ClientRpcOneWay))
		violations := clientRpcRegressionViolations(t, RPCServiceInfoList{service}, false)
		if len(violations) != 2 {
			t.Fatalf("同一方法的每条原因各记一条: got %v", violations)
		}
		for _, got := range violations {
			if got.Service != fixtureService || got.Method != "Call" || got.File != fixtureFile {
				t.Errorf("违例定位错误: %+v", got)
			}
		}
	})
}

func TestClientRpcKindsAcceptLegalShapes(t *testing.T) {
	for _, test := range []struct {
		name    string
		service string
		method  string
		kind    ClientRpcKind
		input   string
		output  string
		scene   bool
	}{
		// NotifyMailChanged(MailChangedEvent) returns (MailChangedEvent) 的形状。
		{"推送响应与输入同类型", "GuildService", "NotifyMailChanged", ClientRpcPush, ".fixture.MailChangedEvent", ".fixture.MailChangedEvent", false},
		{"scene推送响应Empty", "SceneClientPlayerTeam", "NotifyTeamChanged", ClientRpcPush, ".fixture.TeamChanged", ".Empty", true},
		{"豁免推送SendTipToClient", "SceneClientPlayerCommon", "SendTipToClient", ClientRpcPush, ".fixture.TipInfoMessage", ".Empty", true},
		{"豁免推送KickPlayer", "SceneClientPlayerCommon", "KickPlayer", ClientRpcPush, ".fixture.KickPlayerRequest", ".Empty", true},
		{"豁免推送RedirectToGate", "SceneClientPlayerCommon", "RedirectToGate", ClientRpcPush, ".fixture.RedirectToGateRequest", ".Empty", true},
		{"scene单向响应精确Empty", "SceneMovementClientPlayer", "MoveStart", ClientRpcOneWay, ".fixture.MoveStartC2S", ".Empty", true},
		{"仅服务端方法不受scene Empty约束", "ClientPlayerLogin", "Disconnect", ClientRpcServerOnly, ".fixture.Request", ".Empty", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, method := clientRpcRegressionService()
			service.ServiceDescriptorProto.Name = proto.String(test.service)
			method.MethodDescriptorProto.Name = proto.String(test.method)
			method.MethodDescriptorProto.InputType = proto.String(test.input)
			method.MethodDescriptorProto.OutputType = proto.String(test.output)
			clientRpcRegressionSetKind(method, uint64(test.kind))
			classified, err := ClassifyClientRpcServices(RPCServiceInfoList{service}, func(*MethodInfo) bool { return test.scene })
			if err != nil || len(classified) != 1 || len(classified[0].Methods) != 1 {
				t.Fatalf("合法形态必须接受: classified=%v err=%v", classified, err)
			}
			if got := classified[0].Methods[0]; got.Method != method || got.Kind != test.kind {
				t.Errorf("分类结果 %s kind=%s, want %s kind=%s", got.Method.Method(), got.Kind, test.method, test.kind)
			}
		})
	}
}

func TestClientRpcKindsPushPrefixExemptionIsScopedToSceneClientPlayerCommon(t *testing.T) {
	for _, test := range []struct{ service, method string }{
		{"SceneClientPlayerTeam", "SendTipToClient"},
		{"SceneClientPlayerTeam", "KickPlayer"},
		{"SceneClientPlayerTeam", "RedirectToGate"},
		{"SceneClientPlayerCommon", "SyncTeamState"},
	} {
		t.Run(test.service+"."+test.method, func(t *testing.T) {
			service, method := clientRpcRegressionService()
			service.ServiceDescriptorProto.Name = proto.String(test.service)
			method.MethodDescriptorProto.Name = proto.String(test.method)
			method.MethodDescriptorProto.OutputType = proto.String(".Empty")
			clientRpcRegressionSetKind(method, uint64(ClientRpcPush))
			violations := clientRpcRegressionViolations(t, RPCServiceInfoList{service}, true)
			if len(violations) != 1 || violations[0].Service != test.service || violations[0].Method != test.method {
				t.Errorf("Notify 前缀豁免只认 SceneClientPlayerCommon 的三个历史推送: got %v", violations)
			}
		})
	}
}

func TestClientRpcKindsSkipUnmarkedServicesWithoutValidatingThem(t *testing.T) {
	marked, _ := clientRpcRegressionService()
	unmarked, method := clientRpcRegressionService()
	unmarked.ServiceDescriptorProto.Name = proto.String("SceneRollbackClientPlayer")
	unmarked.ServiceDescriptorProto.Options = nil
	// 若按客户端协议校验,这个方法会同时违反 Notify 前缀、scene Empty 与消息号三条;
	// 服务没标 OptionIsClientProtocolService、方法也没标 OptionClientRpcKind,应整个被排除且不报错。
	method.MethodDescriptorProto.Name = proto.String("NotifyRollback")
	method.MethodDescriptorProto.OutputType = proto.String(".Empty")
	method.Id = math.MaxUint64
	classified, err := ClassifyClientRpcServices(RPCServiceInfoList{nil, unmarked, marked}, func(*MethodInfo) bool { return true })
	if err != nil || len(classified) != 1 || classified[0].Service != marked {
		t.Fatalf("只保留标了 OptionIsClientProtocolService 的服务且不报错: classified=%v err=%v", classified, err)
	}
}

func TestClientRpcKindsRejectNilSceneTCPPredicate(t *testing.T) {
	service, _ := clientRpcRegressionService()
	if classified, err := ClassifyClientRpcServices(RPCServiceInfoList{service}, nil); err == nil || classified != nil {
		t.Fatalf("缺 scene TCP 判定必须报错: classified=%v err=%v", classified, err)
	}
}

func TestClientRpcKindsReadUnknownOptionAmongOtherOptions(t *testing.T) {
	// 410000 是 OptionMessagePriority,紧挨 OptionClientRpcKind;50001 / 50002 代表其它 wire type 的任意 option。
	const (
		messagePriorityNumber protowire.Number = 410000
		bytesOptionNumber     protowire.Number = 50001
		fixed32OptionNumber   protowire.Number = 50002
	)
	varint := func(number protowire.Number, value uint64) []byte {
		return protowire.AppendVarint(protowire.AppendTag(nil, number, protowire.VarintType), value)
	}
	priority := varint(messagePriorityNumber, 3)
	others := protowire.AppendFixed32(protowire.AppendTag(nil, fixed32OptionNumber, protowire.Fixed32Type), 7)
	others = protowire.AppendBytes(protowire.AppendTag(others, bytesOptionNumber, protowire.BytesType), []byte("x"))
	concat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	for _, test := range []struct {
		name         string
		wire         []byte
		wantKind     ClientRpcKind
		wantExplicit bool
		wantErr      bool
	}{
		{"夹在其它option之间", concat(priority, varint(OptionClientRpcKindNumber, uint64(ClientRpcPush)), others, priority),
			ClientRpcPush, true, false},
		{"显式REQUEST也算已标注", concat(priority, varint(OptionClientRpcKindNumber, uint64(ClientRpcRequest))),
			ClientRpcRequest, true, false},
		{"只有其它option视为未标注", concat(priority, others), ClientRpcRequest, false, false},
		// int32 -1 按 protobuf 符号扩展成 64 位后的 varint 即 math.MaxUint64。
		{"负值越界", varint(OptionClientRpcKindNumber, math.MaxUint64), ClientRpcRequest, false, true},
		// 其它 option 的字节损坏同样无法可靠定位 410001,报错而不是当作未标注。
		{"其它option字节损坏", concat(varint(OptionClientRpcKindNumber, uint64(ClientRpcPush)), priority[:len(priority)-1]),
			ClientRpcRequest, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := &descriptorpb.MethodDescriptorProto{Options: &descriptorpb.MethodOptions{}}
			method.Options.ProtoReflect().SetUnknown(test.wire)
			kind, explicit, err := ReadClientRpcKind(method)
			if test.wantErr {
				if err == nil {
					t.Fatalf("必须报错: kind=%s explicit=%v", kind, explicit)
				}
				return
			}
			if err != nil || kind != test.wantKind || explicit != test.wantExplicit {
				t.Fatalf("got kind=%s explicit=%v err=%v, want kind=%s explicit=%v",
					kind, explicit, err, test.wantKind, test.wantExplicit)
			}
		})
	}
}

// clientRpcRegressionDynamicExtension 用动态描述符构造一个 MethodOptions 上字段号 410001 的扩展,模拟将来
// protooption 绑定里有了 OptionClientRpcKind、读取走已注册扩展路径的情形;name / fieldType 可故意写错以覆盖占用检查。
func clientRpcRegressionDynamicExtension(t *testing.T, name string, fieldType descriptorpb.FieldDescriptorProto_Type) protoreflect.ExtensionType {
	t.Helper()
	extension := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(int32(OptionClientRpcKindNumber)),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     fieldType.Enum(),
		Extendee: proto.String(".google.protobuf.MethodOptions"),
	}
	if fieldType == descriptorpb.FieldDescriptorProto_TYPE_ENUM {
		extension.TypeName = proto.String(".ClientRpcKindFixture")
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:       proto.String("fixture/client_rpc_kind_option.proto"),
		Dependency: []string{"google/protobuf/descriptor.proto"},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("ClientRpcKindFixture"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("FIXTURE_CLIENT_RPC_REQUEST"), Number: proto.Int32(0)},
				{Name: proto.String("FIXTURE_CLIENT_RPC_PUSH"), Number: proto.Int32(2)},
			},
		}},
		Extension: []*descriptorpb.FieldDescriptorProto{extension},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("构造动态扩展描述符失败: %v", err)
	}
	return dynamicpb.NewExtensionType(file.Extensions().Get(0))
}

func TestClientRpcKindsReadRegisteredExtension(t *testing.T) {
	for _, test := range []struct {
		name      string
		extension string
		fieldType descriptorpb.FieldDescriptorProto_Type
		value     protoreflect.Value
		wantErr   bool
	}{
		{"绑定里有了扩展照常读取", "OptionClientRpcKind", descriptorpb.FieldDescriptorProto_TYPE_ENUM,
			protoreflect.ValueOfEnum(protoreflect.EnumNumber(ClientRpcPush)), false},
		{"字段号被别的扩展占用", "OptionOtherRpcKind", descriptorpb.FieldDescriptorProto_TYPE_ENUM,
			protoreflect.ValueOfEnum(protoreflect.EnumNumber(ClientRpcPush)), true},
		{"扩展类型不是枚举", "OptionClientRpcKind", descriptorpb.FieldDescriptorProto_TYPE_STRING,
			protoreflect.ValueOfString("CLIENT_RPC_PUSH"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			extension := clientRpcRegressionDynamicExtension(t, test.extension, test.fieldType)
			method := &descriptorpb.MethodDescriptorProto{Options: &descriptorpb.MethodOptions{}}
			method.Options.ProtoReflect().Set(extension.TypeDescriptor(), test.value)
			kind, explicit, err := ReadClientRpcKind(method)
			if test.wantErr {
				if err == nil {
					t.Fatalf("410001 上不是 enum OptionClientRpcKind 必须报错: kind=%s explicit=%v", kind, explicit)
				}
				return
			}
			if err != nil || kind != ClientRpcPush || !explicit {
				t.Fatalf("已注册扩展路径读取错误: kind=%s explicit=%v err=%v", kind, explicit, err)
			}
		})
	}
}

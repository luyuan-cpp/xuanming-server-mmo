package cpp

// gate Kafka 路由信封分支的「同名同型自动映射」(docs/design/scene-enter-chain-residuals.md §2.4 第 9 条、§2.7)。
//
// 映射规则只有一条:GateCommand 上有同名、同型、非 repeated 的字段才从信封赋值(envelopeCarriesField)。
// 2026-10-30 起(battle-spectate-tiered-fanout.md §14 顺带发现的修正)事件只要有一个字段映射不到就整条要求载荷,
// 不再构造缺字段的事件:KickPlayerEvent 带 login_generation(GateCommand 没有对应字段),空载荷的踢线直接拒绝,
// 全部在树的生产者都带 payload。所以信封分支只服务「全部字段都在信封上」的事件;事件里的字段写错类型
// (比如 player_id 写成 uint32)也不再静默丢字段,而是变成要求载荷。
//
// 本文件自带描述符安装函数 installAutomapGateRouterDescriptors(GateCommand 带字段),不改、也不依赖同包的
// installGateRouterDescriptors(它的 GateCommand 没有字段,且所在文件正被入口生成器重构改写,§7)。

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protogen/internal"
	"protogen/logger"

	"go.uber.org/zap"
)

func automapScalarField(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   typ.Enum(),
	}
}

// automapGateCommandFields 与 proto/contracts/kafka/gate_command.proto 的 GateCommand 同名同型(只取与本用例相关的几项)。
func automapGateCommandFields() []*descriptorpb.FieldDescriptorProto {
	return []*descriptorpb.FieldDescriptorProto{
		automapScalarField("player_id", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
		automapScalarField("session_id", 4, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		automapScalarField("payload", 5, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
		automapScalarField("event_id", 8, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
	}
}

// automapKickEvent 构造带入口标注的 KickPlayerEvent;playerIDType 决定 player_id 的类型(正确的是 uint64)。
func automapKickEvent(playerIDType descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
	event := ingressTestMessage(true)
	event.Name = proto.String("KickPlayerEvent")
	event.Field = []*descriptorpb.FieldDescriptorProto{
		automapScalarField("session_id", 1, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		automapScalarField("player_id", 2, playerIDType),
		automapScalarField("login_generation", 3, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
	}
	return event
}

// automapEnvelopeOnlyEvent 构造全部字段都能从信封映射的事件;playerIDType 决定 player_id 的类型(正确的是 uint64)。
func automapEnvelopeOnlyEvent(playerIDType descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
	event := ingressTestMessage(true)
	event.Name = proto.String("EnvelopeOnlyEvent")
	event.Field = []*descriptorpb.FieldDescriptorProto{
		automapScalarField("session_id", 1, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		automapScalarField("player_id", 2, playerIDType),
	}
	return event
}

func installAutomapGateRouterDescriptors(t *testing.T, commandFields []*descriptorpb.FieldDescriptorProto, events ...*descriptorpb.DescriptorProto) {
	t.Helper()
	previous := internal.FdSet
	t.Cleanup(func() { internal.FdSet = previous })
	internal.FdSet = &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		{Name: proto.String("proto/contracts/kafka/gate_command.proto"), Package: proto.String("contracts.kafka"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("GateCommand"), Field: commandFields}}},
		{Name: proto.String("proto/contracts/kafka/gate_event.proto"), Package: proto.String("contracts.kafka"), MessageType: events},
	}}
}

// 类型不匹配的分支(envelopeCarriesField)会调 logger.Global.Warn,而 logger.Global 默认为 nil;不设会 panic(照 grpc_handler_routing_test.go)。
func useAutomapNopLogger(t *testing.T) {
	t.Helper()
	previous := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previous })
}

func automapSingleCase(t *testing.T) gateKafkaRouterCase {
	t.Helper()
	cases, err := buildGateKafkaRouterCases()
	if err != nil {
		t.Fatalf("buildGateKafkaRouterCases: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases = %+v, want exactly one case", cases)
	}
	return cases[0]
}

func TestGateKafkaRouterAutoMapsSameNameSameTypeFields(t *testing.T) {
	useAutomapNopLogger(t)
	installAutomapGateRouterDescriptors(t, automapGateCommandFields(),
		automapEnvelopeOnlyEvent(descriptorpb.FieldDescriptorProto_TYPE_UINT64))

	envelopeOnly := automapSingleCase(t)
	if envelopeOnly.PayloadRequired {
		t.Fatalf("%+v: every field is on the envelope, it must not require a payload", envelopeOnly)
	}
	for _, want := range []string{
		"event.set_session_id(command.session_id());",
		"event.set_player_id(command.player_id());",
	} {
		if !slices.Contains(envelopeOnly.Assignments, want) {
			t.Fatalf("assignments = %v, want %q", envelopeOnly.Assignments, want)
		}
	}
}

// KickPlayerEvent 带 login_generation,GateCommand 上没有对应字段:信封分支映射不出它,踢线会丢掉代次栅栏,所以要求载荷。
func TestGateKafkaRouterKickWithLoginGenerationRequiresPayload(t *testing.T) {
	useAutomapNopLogger(t)
	installAutomapGateRouterDescriptors(t, automapGateCommandFields(),
		automapKickEvent(descriptorpb.FieldDescriptorProto_TYPE_UINT64))

	kick := automapSingleCase(t)
	if !kick.PayloadRequired || len(kick.Assignments) != 0 {
		t.Fatalf("%+v: login_generation only travels in the payload, want PayloadRequired and no envelope mapping", kick)
	}
}

// 事件里的 player_id 若是 uint32(与 GateCommand.player_id 的 uint64 不同型),信封映射不了:
// 这条用例把「必须同型」的前提固化下来 —— 写错类型不会静默丢字段,而是变成要求载荷。
func TestGateKafkaRouterSameNameDifferentTypeFieldRequiresPayload(t *testing.T) {
	useAutomapNopLogger(t)
	installAutomapGateRouterDescriptors(t, automapGateCommandFields(),
		automapEnvelopeOnlyEvent(descriptorpb.FieldDescriptorProto_TYPE_UINT32))

	got := automapSingleCase(t)
	if !got.PayloadRequired || len(got.Assignments) != 0 {
		t.Fatalf("%+v: a uint32 player_id cannot be mapped from the uint64 GateCommand.player_id", got)
	}
}

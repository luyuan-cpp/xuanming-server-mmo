package cpp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protogen/internal"
	_config "protogen/internal/config"
	"protogen/logger"

	"go.uber.org/zap"
)

func installGateRouterDescriptors(t *testing.T, events ...*descriptorpb.DescriptorProto) {
	t.Helper()
	previous := internal.FdSet
	t.Cleanup(func() { internal.FdSet = previous })
	internal.FdSet = &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		{Name: proto.String("proto/contracts/kafka/gate_command.proto"), Package: proto.String("contracts.kafka"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("GateCommand")}}},
		{Name: proto.String("proto/contracts/kafka/gate_event.proto"), Package: proto.String("contracts.kafka"), MessageType: events},
	}}
}

func TestKafkaEventHandlersOnlyEmitIngressMessages(t *testing.T) {
	previousConfig, previousLogger, previousEvents := _config.Global, logger.Global, globalProtoEventList
	t.Cleanup(func() {
		_config.Global, logger.Global, globalProtoEventList = previousConfig, previousLogger, previousEvents
	})
	_config.Global = _config.Config{}
	logger.Global = zap.NewNop()
	root := t.TempDir()
	_config.Global.Paths.ProtoDir = filepath.Join(root, "proto")
	_config.Global.DirectoryNames.ProtoDirName = "proto"
	_config.Global.FileExtensions.Proto = ".proto"
	_config.Global.FileExtensions.PbH = ".pb.h"
	_config.Global.FileExtensions.HandlerH = "_handler.h"
	_config.Global.FileExtensions.HandlerCpp = "_handler.cpp"
	_config.Global.Naming.HandlerFile = "Handler"
	_config.Global.Naming.YourCodeBegin = "///<<< BEGIN WRITING YOUR CODE"
	_config.Global.Naming.YourCodeEnd = "///<<< END WRITING YOUR CODE"
	_config.Global.Naming.YourCodePair = _config.Global.Naming.YourCodeBegin + "\n" + _config.Global.Naming.YourCodeEnd
	protoDir := filepath.Join(_config.Global.Paths.ProtoDir, "common", "event")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"sample_event.proto":   "message UnmarkedEvent {}\nmessage AllowedEvent {}\nmessage SecondEvent {}\n",
		"internal_event.proto": "message InternalEvent {}\n",
	} {
		if err := os.WriteFile(filepath.Join(protoDir, name), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	globalProtoEventList = []*ProtoEventInfo{
		{MessageName: "SecondEvent", ProtoInclude: "proto/common/event/sample_event.pb.h", Ingress: eventIngressScene},
		{MessageName: "AllowedEvent", ProtoInclude: "proto/common/event/other_event.pb.h", Ingress: eventIngressScene},
		{MessageName: "UnmarkedEvent", ProtoInclude: "proto/common/event/sample_event.pb.h", Ingress: eventIngressNone},
		{MessageName: "AllowedEvent", ProtoInclude: "proto/common/event/sample_event.pb.h", Ingress: eventIngressScene},
	}
	files, err := os.ReadDir(protoDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join("..", "..", ".."))
	outputDir := filepath.Join(root, "handlers")
	var wg sync.WaitGroup
	for _, file := range files {
		wg.Add(1)
		generateEventHandlerFiles(&wg, file, "common/event", outputDir)
	}
	wg.Wait()
	header, err := os.ReadFile(filepath.Join(outputDir, "sample_event_handler.h"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(header)
	if strings.Contains(text, "UnmarkedEventHandler") || !strings.Contains(text, "AllowedEventHandler") ||
		!strings.Contains(text, "SecondEventHandler") || strings.Index(text, "AllowedEventHandler") > strings.Index(text, "SecondEventHandler") {
		t.Fatalf("只生成入口事件,并保持 proto 顺序:\n%s", text)
	}
	for _, ext := range []string{".h", ".cpp"} {
		if _, err := os.Stat(filepath.Join(outputDir, "internal_event_handler"+ext)); !os.IsNotExist(err) {
			t.Fatalf("无入口的文件不得生成 handler: ext=%s err=%v", ext, err)
		}
	}
}

func TestGateKafkaRouterEmptyWhitelistStillRendersRejectingOutput(t *testing.T) {
	installGateRouterDescriptors(t, ingressTestMessage(false))
	cases, err := buildGateKafkaRouterCases()
	if err != nil || len(cases) != 0 {
		t.Fatalf("空白名单应为合法输入: cases=%v err=%v", cases, err)
	}
	tmpl, err := template.ParseFiles(filepath.Join("..", "..", "template", "gate_kafka_command_router.cpp.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, gateKafkaRouterTemplateData{Cases: cases}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "if (!IsGateCommandEvent(command.event_id()))") ||
		strings.Contains(out.String(), "Handler(event)") {
		t.Fatal("空白名单输出必须保留入口拒绝,且不再含事件触发分支")
	}
}

func TestKafkaIngressDispatchCallsHandlerDirectly(t *testing.T) {
	allowed := ingressTestMessage(true)
	allowed.Name = proto.String("AllowedEvent")
	installGateRouterDescriptors(t, allowed)
	cases, err := buildGateKafkaRouterCases()
	if err != nil {
		t.Fatal(err)
	}
	render := func(name string, data any) string {
		t.Helper()
		tmpl, err := template.ParseFiles(filepath.Join("..", "..", "template", name))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	gate := render("gate_kafka_command_router.cpp.tmpl", gateKafkaRouterTemplateData{Cases: cases})
	if !strings.Contains(gate, "GateEventHandler::AllowedEventHandler(event)") || strings.Contains(gate, "dispatcher.trigger") {
		t.Fatalf("Kafka 必需命令须直接调用 handler:\n%s", gate)
	}
	scene := render("scene_kafka_command_router.cpp.tmpl", map[string]any{
		"Includes": []string{"allowed_event_handler.h"},
		"Cases": []map[string]string{{"EventIdConstant": "AllowedEventEventId", "QualifiedName": "AllowedEvent",
			"ClassName": "GateEventHandler", "HandlerName": "AllowedEventHandler"}},
	})
	if !strings.Contains(scene, "GateEventHandler::AllowedEventHandler(event)") || strings.Contains(scene, "dispatcher.trigger") {
		t.Fatalf("scene Kafka 必需命令须直接调用 handler:\n%s", scene)
	}
	handler := render("event_handler.cpp.tmpl", EventTemplateData{ClassName: "GateEventHandler",
		EventMessages: []EventHandlerMessage{{Name: "AllowedEvent", HandlerName: "AllowedEventHandler", QualifiedName: "AllowedEvent"}}})
	if strings.Contains(handler, "sink<") || strings.Contains(handler, "::Register") || strings.Contains(handler, "::UnRegister") {
		t.Fatalf("入口 handler 不再订阅 dispatcher:\n%s", handler)
	}
}

func TestGateKafkaRouterMissingContractFailsInsteadOfKeepingStaleOutput(t *testing.T) {
	installGateRouterDescriptors(t)
	internal.FdSet.File = internal.FdSet.File[:1]
	if _, err := buildGateKafkaRouterCases(); err == nil {
		t.Fatal("缺少网关事件契约必须拒绝生成")
	}
}

func TestGateKafkaRouterOnlyEmitsMarkedEvents(t *testing.T) {
	allowed := ingressTestMessage(true)
	allowed.Name = proto.String("AllowedEvent")
	unmarked := ingressTestMessage(false)
	unmarked.Name = proto.String("UnmarkedEvent")
	installGateRouterDescriptors(t, allowed, unmarked)
	cases, err := buildGateKafkaRouterCases()
	if err != nil || len(cases) != 1 || cases[0].EventMessageName != "AllowedEvent" {
		t.Fatalf("仅标注事件可生成路由: cases=%v err=%v", cases, err)
	}
}

func routerMessageField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(number),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".MessageContent"),
	}
}

func routerRepeatedField(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
		Type:   typ.Enum(),
	}
}

// renderGateRouter 用真实模板渲染整份路由文件。
func renderGateRouter(t *testing.T, cases []gateKafkaRouterCase) string {
	t.Helper()
	tmpl, err := template.ParseFiles(filepath.Join("..", "..", "template", "gate_kafka_command_router.cpp.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, gateKafkaRouterTemplateData{Cases: cases}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// splitAtEnvelopeBranch 把渲染结果切成载荷分支(含文件头)与空载荷的信封分支两段。
func splitAtEnvelopeBranch(t *testing.T, rendered string) (payloadBranch, envelopeBranch string) {
	t.Helper()
	payloadBranch, envelopeBranch, found := strings.Cut(rendered, "// 空载荷的信封分支")
	if !found {
		t.Fatalf("渲染结果缺空载荷的信封分支:\n%s", rendered)
	}
	return payloadBranch, envelopeBranch
}

// envelopeCase 取信封分支里某个事件的 case 文本:从 case 标签到下一个 case / default 之前。
func envelopeCase(t *testing.T, envelopeBranch, eventIDConstant string) string {
	t.Helper()
	_, block, found := strings.Cut(envelopeBranch, "    case "+eventIDConstant+": {")
	if !found {
		t.Fatalf("信封分支缺 %s:\n%s", eventIDConstant, envelopeBranch)
	}
	if end := strings.Index(block, "\n    case "); end >= 0 {
		block = block[:end]
	}
	if end := strings.Index(block, "\n    default:"); end >= 0 {
		block = block[:end]
	}
	return block
}

// 有 GateCommand 信封装不下的字段(message / repeated,也包括信封上没有的 string / bytes / 标量)的事件:空载荷时信封分支
// 必须拒绝并计数,不得构造缺字段的事件下发 —— 旧模板给 PushToPlayer 推一个空 MessageContent、三种广播是空事件、
// RedirectToGate 把客户端重定向到空地址(battle-spectate-tiered-fanout.md §14)。载荷分支不受影响,照常解码并调用 handler。
func TestGateKafkaRouterRejectsEmptyPayloadWhenEnvelopeCannotCarryEvent(t *testing.T) {
	useAutomapNopLogger(t)
	push := ingressTestMessage(true)
	push.Name = proto.String("PushToPlayerEvent")
	push.Field = []*descriptorpb.FieldDescriptorProto{
		automapScalarField("session_id", 1, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		routerMessageField("message_content", 2),
		automapScalarField("player_id", 3, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
	}
	broadcast := ingressTestMessage(true)
	broadcast.Name = proto.String("BroadcastToPlayersEvent")
	broadcast.Field = []*descriptorpb.FieldDescriptorProto{
		routerRepeatedField("session_list", 1, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		routerMessageField("message_content", 2),
		routerRepeatedField("player_list", 5, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
	}
	redirect := ingressTestMessage(true)
	redirect.Name = proto.String("RedirectToGateEvent")
	redirect.Field = []*descriptorpb.FieldDescriptorProto{
		automapScalarField("session_id", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		automapScalarField("target_gate_ip", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		automapScalarField("token_payload", 5, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
	}
	installAutomapGateRouterDescriptors(t, automapGateCommandFields(), push, broadcast, redirect,
		automapKickEvent(descriptorpb.FieldDescriptorProto_TYPE_UINT64),
		automapEnvelopeOnlyEvent(descriptorpb.FieldDescriptorProto_TYPE_UINT64))

	cases, err := buildGateKafkaRouterCases()
	if err != nil || len(cases) != 5 {
		t.Fatalf("cases=%+v err=%v, want 5 cases", cases, err)
	}
	byName := make(map[string]gateKafkaRouterCase, len(cases))
	for _, c := range cases {
		byName[c.EventMessageName] = c
	}

	rendered := renderGateRouter(t, cases)
	if strings.Count(rendered, "void RejectEmptyPayload(") != 1 ||
		!strings.Contains(rendered, "metric=gate_kafka_empty_payload_rejected") {
		t.Fatalf("有需要载荷的事件时必须恰好渲染一份带 metric= 计数行的拒绝函数:\n%s", rendered)
	}
	payloadBranch, envelopeBranch := splitAtEnvelopeBranch(t, rendered)
	for _, name := range []string{"PushToPlayerEvent", "BroadcastToPlayersEvent", "RedirectToGateEvent", "KickPlayerEvent"} {
		c := byName[name]
		if !c.PayloadRequired || len(c.Assignments) != 0 {
			t.Fatalf("%s: PayloadRequired=%v Assignments=%v, want true and no envelope mapping",
				name, c.PayloadRequired, c.Assignments)
		}
		block := envelopeCase(t, envelopeBranch, c.EventIdConstant)
		if !strings.Contains(block, "RejectEmptyPayload(topic, command.event_id());") ||
			!strings.Contains(block, "return;") ||
			strings.Contains(block, "contracts::kafka::"+name+" event;") ||
			strings.Contains(block, "Handler(event)") {
			t.Fatalf("%s 的空载荷必须被拒,不得构造空事件或调用 handler:\n%s", name, block)
		}
		if !strings.Contains(payloadBranch, c.ClassName+"::"+c.HandlerName+"(event)") {
			t.Fatalf("%s 的载荷分支仍须解码后调用 handler:\n%s", name, payloadBranch)
		}
	}

	envelopeOnly := byName["EnvelopeOnlyEvent"]
	envelopeOnlyBlock := envelopeCase(t, envelopeBranch, envelopeOnly.EventIdConstant)
	if envelopeOnly.PayloadRequired || strings.Contains(envelopeOnlyBlock, "RejectEmptyPayload") ||
		!strings.Contains(envelopeOnlyBlock, envelopeOnly.ClassName+"::"+envelopeOnly.HandlerName+"(event);") {
		t.Fatalf("同一份路由里全部字段都在信封上的事件不受影响:\n%s", envelopeOnlyBlock)
	}
}

// 全部字段都在信封上的事件仍走信封映射:同名同型字段照旧从 GateCommand 赋值并调用 handler;
// 没有需要载荷的事件时不渲染拒绝函数,生成物里不留没人调用的函数。
func TestGateKafkaRouterEnvelopeOnlyEventKeepsEnvelopeMapping(t *testing.T) {
	useAutomapNopLogger(t)
	installAutomapGateRouterDescriptors(t, automapGateCommandFields(),
		automapEnvelopeOnlyEvent(descriptorpb.FieldDescriptorProto_TYPE_UINT64))

	envelopeOnly := automapSingleCase(t)
	if envelopeOnly.PayloadRequired {
		t.Fatalf("EnvelopeOnlyEvent 的字段都在信封上,不应要求载荷: %+v", envelopeOnly)
	}
	rendered := renderGateRouter(t, []gateKafkaRouterCase{envelopeOnly})
	if strings.Contains(rendered, "RejectEmptyPayload") {
		t.Fatalf("没有需要载荷的事件时不得渲染拒绝函数:\n%s", rendered)
	}
	_, envelopeBranch := splitAtEnvelopeBranch(t, rendered)
	block := envelopeCase(t, envelopeBranch, envelopeOnly.EventIdConstant)
	for _, want := range []string{
		"contracts::kafka::EnvelopeOnlyEvent event;",
		"event.set_session_id(command.session_id());",
		"event.set_player_id(command.player_id());",
		envelopeOnly.ClassName + "::" + envelopeOnly.HandlerName + "(event);",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("信封分支缺 %q:\n%s", want, block)
		}
	}
}

// 判据按映射规则走:每个字段都要在 GateCommand 上有同名、同型、非 repeated 的字段,否则信封装不下。
func TestEventRequiresPayloadOnlyForFieldsTheEnvelopeCannotCarry(t *testing.T) {
	useAutomapNopLogger(t)
	commandFields := make(map[string]*descriptorpb.FieldDescriptorProto)
	for _, field := range automapGateCommandFields() {
		commandFields[field.GetName()] = field
	}
	sessionID := automapScalarField("session_id", 1, descriptorpb.FieldDescriptorProto_TYPE_UINT32)
	playerID := automapScalarField("player_id", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT64)
	for _, tc := range []struct {
		name   string
		fields []*descriptorpb.FieldDescriptorProto
		want   bool
	}{
		{"无字段", nil, false},
		{"全部同名同型", []*descriptorpb.FieldDescriptorProto{sessionID, playerID}, false},
		{"信封上没有的 string", []*descriptorpb.FieldDescriptorProto{
			sessionID,
			automapScalarField("target_gate_ip", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		}, true},
		{"信封上没有的 bytes", []*descriptorpb.FieldDescriptorProto{
			sessionID,
			automapScalarField("token_payload", 3, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
		}, true},
		{"信封上没有的标量", []*descriptorpb.FieldDescriptorProto{
			sessionID,
			automapScalarField("login_generation", 3, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
		}, true},
		{"同名不同型", []*descriptorpb.FieldDescriptorProto{
			automapScalarField("player_id", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		}, true},
		{"与信封 payload 同名:信封分支里它恒为空", []*descriptorpb.FieldDescriptorProto{
			automapScalarField("payload", 3, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
		}, true},
		{"message 字段", []*descriptorpb.FieldDescriptorProto{sessionID, routerMessageField("message_content", 2)}, true},
		{"repeated 标量", []*descriptorpb.FieldDescriptorProto{
			routerRepeatedField("session_id", 1, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		}, true},
	} {
		event := &descriptorpb.DescriptorProto{Name: proto.String("Event"), Field: tc.fields}
		if got := eventRequiresPayload(event, commandFields); got != tc.want {
			t.Errorf("%s: eventRequiresPayload = %v, want %v", tc.name, got, tc.want)
		}
	}
}

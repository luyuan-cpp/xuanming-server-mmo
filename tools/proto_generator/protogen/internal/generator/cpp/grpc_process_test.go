package cpp

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"protogen/internal"
	_config "protogen/internal/config"

	"google.golang.org/protobuf/types/descriptorpb"
)

func makeGrpcInitService(fileName, serviceName, methodName string, methodID uint64) *internal.RPCServiceInfo {
	fileOptions := &descriptorpb.FileOptions{CcGenericServices: boolPtr(false)}

	file := &descriptorpb.FileDescriptorProto{
		Name:    strPtr(fileName),
		Options: fileOptions,
	}
	service := &descriptorpb.ServiceDescriptorProto{Name: strPtr(serviceName)}
	protoInfo := internal.ProtoFileInfo{
		Fd:                     file,
		ServiceDescriptorProto: service,
	}
	method := &internal.MethodInfo{
		ProtoFileInfo:         protoInfo,
		Id:                    methodID,
		MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: strPtr(methodName)},
	}

	return &internal.RPCServiceInfo{
		ProtoFileInfo: protoInfo,
		Methods:       internal.RPCMethods{method},
	}
}

func TestGrpcInitTemplateSupportsMultipleServicesForSameNodeType(t *testing.T) {
	originalConfig := _config.Global
	originalNodeEnumValues := internal.NodeEnumValueSet
	t.Cleanup(func() {
		_config.Global = originalConfig
		internal.NodeEnumValueSet = originalNodeEnumValues
	})
	_config.Global.FileExtensions.Proto = ".proto"
	internal.NodeEnumValueSet = map[string]bool{"BattleNodeService": true}

	services := []*internal.RPCServiceInfo{
		makeGrpcInitService("proto/battle/battle_node.proto", "BattleAdmin", "CreateBattle", 160),
		makeGrpcInitService("proto/battle/player_battle.proto", "BattleClientPlayer", "SetAutoBattle", 162),
		makeGrpcInitService("proto/battle/battle_node.proto", "BattleNode", "JoinBattle", 159),
	}

	grpcFiles := buildGrpcInitFileInfo(services)
	if len(grpcFiles) != 2 {
		t.Fatalf("expected two generated gRPC client files, got %d", len(grpcFiles))
	}

	var battleNodeFile *grpcInitFileInfo
	for _, grpcFile := range grpcFiles {
		if grpcFile.FileName() == "proto/battle/battle_node.proto" {
			battleNodeFile = grpcFile
			break
		}
	}
	if battleNodeFile == nil {
		t.Fatal("expected battle_node.proto in grouped gRPC clients")
	}
	if len(battleNodeFile.Methods) != 2 || battleNodeFile.Methods[0].Id != 159 || battleNodeFile.Methods[1].Id != 160 {
		t.Fatalf("expected all battle_node.proto methods sorted by ID, got %+v", battleNodeFile.Methods)
	}

	templatePath := filepath.Join("..", "..", "template", "grpc_init_total.cpp.tmpl")
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		t.Fatalf("parse grpc init template: %v", err)
	}

	var rendered bytes.Buffer
	err = tmpl.Execute(&rendered, grpcInitTemplateData{
		GrpcFiles:       grpcFiles,
		NodeEnumCppType: "test::eNodeType",
		NodeInfoCppType: "NodeInfo",
	})
	if err != nil {
		t.Fatalf("render grpc init template: %v", err)
	}
	output := rendered.String()
	// 只检查目标函数，避免新增的关机函数把合法的互斥分发带进 deadline 断言。
	functionOutput := func(signature string) string {
		t.Helper()
		start := strings.Index(output, signature)
		if start < 0 {
			t.Fatalf("生成结果缺少函数 %q", signature)
		}
		end := strings.Index(output[start:], "\n}\n")
		if end < 0 {
			t.Fatalf("生成结果缺少函数 %q 的结束边界", signature)
		}
		return output[start : start+end+len("\n}\n")]
	}

	// deadline 按目标节点类型分发:同一节点类型的两个文件都要收到,分支互相独立(与 InitGrpcNode 同理)。
	deadlineOutput := functionOutput("void SetGrpcCallDeadline(")
	if strings.Contains(deadlineOutput, "else if") {
		t.Fatal("deadline dispatch for same-node gRPC clients must use independent branches")
	}
	if count := strings.Count(deadlineOutput, "if (static_cast<uint32_t>(test::eNodeType::BattleNodeService) == nodeType)"); count != 2 {
		t.Fatalf("expected two independent Battle node deadline branches, got %d", count)
	}
	if !strings.Contains(deadlineOutput, "::SetBattleNodeCallDeadline(deadline)") ||
		!strings.Contains(deadlineOutput, "::SetPlayerBattleCallDeadline(deadline)") {
		t.Fatal("expected both same-node gRPC clients to receive the call deadline")
	}

	// 第 3 批起传输是 muduo_grpc::Channel(docs/design/grpc-muduo-transport.md §14):生成物里不得再有官方 CQ 的
	// 初始化、轮询与排空,也不得再引用官方 gRPC 头。
	for _, forbidden := range []string{"CompletionQueue", "InitGrpcNode", "PollGrpcNodeShutdown", "HandleCompletedQueueMessage",
		"BeginShutdownGrpcNode", "grpcpp", "ClientContext"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("rendered grpc init must not contain %q any more", forbidden)
		}
	}
	for _, forbidden := range []string{"SetIfEmpty", "GrpcReplyContext", "GrpcCallFailure", "void SetHandler(", "void SetFailedHandler("} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("生成客户端已删除全局入口 %q", forbidden)
		}
	}
}

// 没有节点类型的内部 service(RoleFenceParticipant,六个业务服各自注册)不进按节点类型的分发,否则生成的 C++ 引用枚举里不存在的值。
func TestBuildGrpcInitFileInfoSkipsServiceWithoutNodeType(t *testing.T) {
	originalNodeEnumValues := internal.NodeEnumValueSet
	t.Cleanup(func() { internal.NodeEnumValueSet = originalNodeEnumValues })
	internal.NodeEnumValueSet = map[string]bool{"BattleNodeService": true}

	grpcFiles := buildGrpcInitFileInfo([]*internal.RPCServiceInfo{
		makeGrpcInitService("proto/role_fence/role_fence.proto", "RoleFenceParticipant", "ApplyRoleFence", 360),
		makeGrpcInitService("proto/battle/battle_node.proto", "BattleNode", "JoinBattle", 159),
	})
	if len(grpcFiles) != 1 || grpcFiles[0].FileName() != "proto/battle/battle_node.proto" {
		t.Fatalf("只有对得上节点类型的 service 进分发,got %d files", len(grpcFiles))
	}
}

func TestGrpcAsyncClientTemplatesUseMuduoChannelAndRejectStreaming(t *testing.T) {
	originalConfig := _config.Global
	t.Cleanup(func() { _config.Global = originalConfig })
	_config.Global.FileExtensions.Proto = ".proto"

	pkg := "scene_manager"
	file := &descriptorpb.FileDescriptorProto{
		Name:    strPtr("proto/scene_manager/scene_manager_service.proto"),
		Package: &pkg,
		Options: &descriptorpb.FileOptions{CcGenericServices: boolPtr(false)},
	}
	service := &descriptorpb.ServiceDescriptorProto{Name: strPtr("SceneManager")}
	protoInfo := internal.ProtoFileInfo{Fd: file, ServiceDescriptorProto: service}
	streaming := true
	unary := &internal.MethodInfo{
		ProtoFileInfo: protoInfo,
		Id:            46,
		MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{
			Name: strPtr("EnterScene"), InputType: strPtr(".scene_manager.EnterSceneRequest"),
			OutputType: strPtr(".scene_manager.EnterSceneResponse"),
		},
	}
	stream := &internal.MethodInfo{
		ProtoFileInfo: protoInfo,
		Id:            47,
		MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{
			Name: strPtr("Watch"), InputType: strPtr(".scene_manager.WatchRequest"),
			OutputType: strPtr(".scene_manager.WatchResponse"), ServerStreaming: &streaming,
		},
	}
	services := []*internal.RPCServiceInfo{{ProtoFileInfo: protoInfo, Methods: internal.RPCMethods{unary, stream}}}
	data := GrpcServiceTemplateData{
		ServiceInfo:           services,
		GeneratorGrpcFileName: "scene_manager_service_grpc_client",
		Package:               "scene_manager",
		FileBaseNameCamel:     "SceneManagerService",
	}

	render := func(name string) string {
		t.Helper()
		tmpl, err := template.ParseFiles(filepath.Join("..", "..", "template", name))
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return out.String()
	}

	header := render("grpc_async_client.h.tmpl")
	if !strings.Contains(header, "void SendSceneManagerEnterScene(entt::registry& registry, entt::entity nodeEntity, const ::scene_manager::EnterSceneRequest& request, GrpcDone<::scene_manager::EnterSceneResponse> done, const GrpcCallOptions& options = {});") {
		t.Fatal("unary 方法必须声明带必填完成回调的 typed 入口")
	}
	if strings.Contains(header, "const ::scene_manager::EnterSceneRequest& request);") {
		t.Fatal("不得保留不带完成回调的 typed 重载")
	}
	if !strings.Contains(header, `kSceneManagerEnterSceneGrpcPath = "/scene_manager.SceneManager/EnterScene"`) {
		t.Fatal("unary method must carry the wire :path /<package>.<Service>/<Method>")
	}
	if strings.Contains(header, "AsyncSceneManagerWatchHandler") {
		t.Fatal("streaming methods must not get a generated client handler")
	}
	if strings.Contains(header, "void SendSceneManagerWatch(") {
		t.Fatal("流式方法不得生成 Send")
	}
	for _, forbidden := range []string{".grpc.pb.h", "ClientContext", "CompletionQueue", "StubPtr"} {
		if strings.Contains(header, forbidden) {
			t.Fatalf("generated client header must not reference %q", forbidden)
		}
	}

	if !strings.Contains(header, "void SendSceneManagerEnterSceneErased(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, GrpcMessageDone done, const GrpcCallOptions& options);") {
		t.Fatal("unary 方法缺少显式完成回调的类型擦除入口")
	}
	if strings.Contains(header, "SendSceneManagerWatchErased") {
		t.Fatal("流式方法不得生成类型擦除入口")
	}

	source := render("grpc_async_client.cpp.tmpl")
	if !strings.Contains(source, "GetDescriptor() != ::scene_manager::EnterSceneRequest::descriptor()") {
		t.Fatal("类型擦除入口必须先校验请求动态类型")
	}
	if !strings.Contains(source, "dynamic_cast<const ::scene_manager::EnterSceneRequest*>(&message)") ||
		!strings.Contains(source, "nodeEntity, *typedRequest, options.metadata, options.deadline,") ||
		strings.Contains(source, "static_cast<const ::scene_manager::EnterSceneRequest&>(message)") {
		t.Fatal("descriptor 相同也必须核对真实 C++ 类型,不得把 DynamicMessage 静态转成生成类")
	}
	if !strings.Contains(source, "channel->StartUnaryCall(") || !strings.Contains(source, "Base64Encode(value)") {
		t.Fatal("unary calls must go through muduo_grpc::Channel and keep the application-level Base64 layer")
	}
	if strings.Contains(source, "void SendSceneManagerWatch(") {
		t.Fatal("流式方法不得生成发送桩")
	}
	for _, forbidden := range []string{"AsyncSceneManager", "GrpcReplyContext", "GrpcCallFailure", "sentMetadata", "call->request", "metaKeys", "metaValues"} {
		if strings.Contains(header, forbidden) || strings.Contains(source, forbidden) {
			t.Fatalf("已删除客户端旧机制 %q", forbidden)
		}
	}
	start := strings.Index(source, "void StartSceneManagerEnterSceneCall(")
	if start < 0 {
		t.Fatal("缺少统一调用入口")
	}
	end := strings.Index(source[start:], "\nvoid SendSceneManagerEnterScene(")
	if end < 0 {
		t.Fatal("缺少统一调用入口的结束边界")
	}
	body := source[start : start+end]
	serialized := strings.Index(body, "request.SerializeToString(&unaryRequest.message)")
	queued := strings.Index(body, "QueueLocalSceneManagerEnterSceneFailure(")
	sent := strings.Index(body, "channel->StartUnaryCall(")
	if serialized < 0 || queued < 0 || sent < 0 || serialized > queued || serialized > sent {
		t.Fatal("共享请求必须先同步序列化,再排本地完成或交给通道")
	}
	if !strings.Contains(body, "requires a done callback") || !strings.Contains(body, "[done = std::move(done)]") {
		t.Fatal("完成回调必填且独立持有,不得保留请求原型")
	}
	for _, forbidden := range []string{"CompletionQueue", "PrepareAsync", "object_pool", "GrpcTag"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("generated client source must not reference %q", forbidden)
		}
	}
}

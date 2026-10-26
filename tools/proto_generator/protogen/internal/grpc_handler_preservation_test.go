package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_config "protogen/internal/config"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestRpcMetadataIncludesOnlyProtobufMessages(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	for _, generic := range []bool{false, true} {
		method := buildBattleGrpcPreservationMethods()[0]
		method.Fd.Options = &descriptorpb.FileOptions{CcGenericServices: proto.Bool(generic)}
		include := method.IncludeName()
		if !strings.Contains(include, "battle_node.pb.h") || strings.Contains(include, ".grpc.pb.h") {
			t.Fatalf("两类服务元数据都只能引用普通 Protobuf 消息: %s", include)
		}
	}
}

func TestGrpcServiceHandler_NewBattleSkeleton(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	methods := buildBattleGrpcPreservationMethods()
	dir := t.TempDir()
	headerPath := filepath.Join(dir, "battle_node.h")
	cppPath := filepath.Join(dir, "battle_node.cpp")

	header, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
	if err != nil {
		t.Fatalf("新建头文件失败: %v", err)
	}
	for _, name := range []string{"grpc.header.includes", "grpc.header.public", "grpc.header.private"} {
		assertGrpcPreservedOnce(t, header, grpcPreservationBegin(name), "头文件守护段")
	}
	assertGrpcPreservedOnce(t, header, "#include \"proto/battle/battle_node.pb.h\"\n"+grpcPreservationBegin("grpc.header.includes"), "消息头文件紧跟 include 守护段")
	assertGrpcPreservedOnce(t, header, "class BattleNodeImpl final : public muduo_grpc::Service", "muduo_grpc 服务基类")
	assertGrpcPreservedOnce(t, header, "void RegisterMethods(muduo_grpc::MethodTable& table) override;", "方法表声明")
	assertGrpcPreservedOnce(t, header, "muduo::net::EventLoop& loop_;", "loop 引用成员")
	cpp := GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")
	if strings.Contains(cpp, _config.Global.Naming.YourCodeEnd+"}") {
		t.Fatal("结束守护标记不能吞掉函数结束括号")
	}
	if !strings.HasPrefix(cpp, "#include \"battle_node.h\"\n"+_config.Global.Naming.YourCodeBegin+"\n") {
		t.Fatalf("cpp 开头应是本服务头文件紧跟 FirstCode 守护段:\n%s", cpp)
	}
	writeGrpcPreservationFile(t, headerPath, header)
	writeGrpcPreservationFile(t, cppPath, cpp)
	if next, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods); err != nil || next != header {
		t.Fatalf("首次生成的头文件必须可以立即重复生成且字节不变: err=%v", err)
	}
	if next := GetGrpcServiceHandlerCppStr(cppPath, methods, "", ""); next != cpp {
		t.Fatal("首次生成的空骨架必须可以立即重复生成且字节不变")
	}
	assertNoOfficialGrpcServerTokens(t, header, "头文件")
	assertNoOfficialGrpcServerTokens(t, cpp, "cpp")
	for _, method := range methods {
		name := method.Method()
		assertGrpcPreservedOnce(t, header, "static constexpr std::string_view k"+name+"Path = \"/BattleNode/"+name+"\";", "路由常量")
		assertGrpcPreservedOnce(t, header, "    void "+name+"(const std::shared_ptr<"+name+"Call>& call);", "guard 函数声明")
		assertGrpcPreservedOnce(t, cpp, "void BattleNodeImpl::"+name+"(const std::shared_ptr<"+name+"Call>& call)", "guard 函数")
		assertGrpcPreservedOnce(t, cpp, "void BattleNodeImpl::Handle"+name+"(", "业务委托函数")
		assertGrpcPreservedOnce(t, cpp, GenerateGrpcHandlerNameWrapper(method, ""), "Handle* 查找键只能出现在定义行")
		assertGrpcPreservedOnce(t, cpp, grpcPreservationBegin("grpc."+name+".guard"), "guard 守护段")
		assertGrpcPreservedOnce(t, cpp, "table.AddUnary(std::string(k"+name+"Path),", "方法表登记")
		assertGrpcPreservedOnce(t, cpp, "\n                "+name+"(call);\n", "解析成功后进 guard 函数")
		assertGrpcPreservationOrder(t, grpcGuardFunction(t, cpp, "BattleNode", name),
			"[[maybe_unused]] const "+method.CppRequest()+"* request = &call->request();",
			grpcPreservationBegin("grpc."+name+".guard"),
			_config.Global.Naming.YourCodeEnd,
			"call->Proceed();")
	}
	if got := strings.Count(cpp, "\n    call->Proceed();\n}\n"); got != len(methods) {
		t.Fatalf("每个 guard 函数都须以 Proceed 收尾: got=%d want=%d", got, len(methods))
	}
	if strings.Contains(cpp, "in_loop") || strings.Contains(cpp, "post_dispatch") {
		t.Fatal("新骨架不得再生成旧的 in_loop / post_dispatch 段")
	}
	for _, name := range []string{"DestroyBattle"} {
		assertGrpcPreservedOnce(t, header, "using "+name+"Call = GrpcUnaryCall<::"+name+"Request, ::Empty>;", "Empty 响应调用类型")
		assertGrpcPreservedOnce(t, header, "static void Handle"+name+"(const ::"+name+"Request* request);", "Empty 响应委托声明")
		assertGrpcPreservedOnce(t, cpp, "[](const ::"+name+"Request* request, ::Empty* /*response*/) { Handle"+name+"(request); });", "Empty 响应委托调用")
		if strings.Contains(cpp, "Handle"+name+"(request, response)") || strings.Contains(cpp, "&Handle"+name) {
			t.Fatalf("Empty 响应不能被传给 Handle%s", name)
		}
	}
	for _, name := range []string{"QueryObserver", "QueryBattleCreation", "CreateBattle", "AddObserver", "RemoveObserver", "IssueBattleTicket"} {
		assertGrpcPreservedOnce(t, header, "using "+name+"Call = GrpcUnaryCall<::"+name+"Request, ::"+name+"Response>;", "带响应调用类型")
		assertGrpcPreservedOnce(t, cpp, name+"Call::Create(std::move(serverCall), &Handle"+name+");", "带响应委托不带类名前缀")
	}
}

// guard 段在重生成后原样保留;FirstCode、头文件三段与全部 Handle* 体同样逐字保留。
func TestGrpcServiceHandler_RegenerationPreservesBattleCustomSections(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	methods := buildBattleGrpcPreservationMethods()
	dir := t.TempDir()
	headerPath := filepath.Join(dir, "battle_node.h")
	cppPath := filepath.Join(dir, "battle_node.cpp")
	header, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
	if err != nil {
		t.Fatalf("新建头文件失败: %v", err)
	}
	cpp := GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")

	headerSections := map[string]string{
		"grpc.header.includes": "#include \"battle_admission_gate.h\"",
		"grpc.header.public":   "    bool OpenAdmission();\n    void CloseAdmission();",
		"grpc.header.private":  "    std::shared_ptr<battle_admission::AdmissionGate> admission_ = std::make_shared<battle_admission::AdmissionGate>();",
	}
	for name, body := range headerSections {
		header = fillGrpcPreservationSection(t, header, "", name, body)
	}
	globalCode := "#include \"infra/agones/agones_gameserver_lifecycle.h\"\n" +
		"#include \"logic/battle_room_manager.h\"\n" +
		"namespace {\n" +
		"constexpr char kBattleNotAllocatableMessage[] = \"battle_not_allocatable\";\n" +
		"muduo_grpc::Status NotAllocatable() { return muduo_grpc::Status(muduo_grpc::StatusCode::kUnavailable, kBattleNotAllocatableMessage); }\n" +
		"}\n" +
		"bool BattleNodeImpl::OpenAdmission() { return admission_->Open(); }\n" +
		"void BattleNodeImpl::CloseAdmission() { admission_->Close(); }"
	cpp = fillGrpcPreservationSection(t, cpp, "", "", globalCode)
	customCode := []string{globalCode}
	for _, method := range methods {
		name := method.Method()
		handleCode := "    BattleRoomManager::Instance().Handle" + name + "(*request"
		if !isVoidResponse(method) {
			handleCode += ", *response"
		}
		handleCode += ");"
		cpp = fillGrpcPreservationSection(t, cpp, "void BattleNodeImpl::Handle"+name+"(", "", handleCode)
		customCode = append(customCode, handleCode)
		if name == "CreateBattle" || name == "DestroyBattle" {
			continue
		}
		// 其余方法的 guard 段也各放不同内容，防止命名段被串到其他方法或被默认空段覆盖。
		body := "    // 保留 " + name + " 的 guard 自定义区"
		cpp = fillGrpcPreservationSection(t, cpp, "", "grpc."+name+".guard", body)
		customCode = append(customCode, body)
	}
	createGuard := "    if (admission_->Phase() != battle_admission::AdmissionPhase::kOpen)\n" +
		"    {\n" +
		"        call->Reject(NotAllocatable());\n" +
		"        return;\n" +
		"    }\n" +
		"    agones::GameServerLifecycle::Instance().AcquireAllocationPermitAsync(\n" +
		"        loop_,\n" +
		"        [call, gate = std::weak_ptr<battle_admission::AdmissionGate>(admission_)](\n" +
		"            agones::GameServerLifecycle::AllocationPermit permit)\n" +
		"        {\n" +
		"            if (!permit || !AdmissionOpen(gate))\n" +
		"            {\n" +
		"                call->Reject(NotAllocatable());\n" +
		"                return;\n" +
		"            }\n" +
		"            call->Proceed();\n" +
		"        });\n" +
		"    return;"
	destroyGuard := "    if(!BattleRoomManager::Instance().AcceptsPlacementOperation(request->battle_id(),request->placement_operation_id(),request->target_instance_id()))\n" +
		"    {\n" +
		"        call->Reject(muduo_grpc::Status(muduo_grpc::StatusCode::kFailedPrecondition,\"placement_operation_mismatch\"));\n" +
		"        return;\n" +
		"    }"
	cpp = fillGrpcPreservationSection(t, cpp, "", "grpc.CreateBattle.guard", createGuard)
	cpp = fillGrpcPreservationSection(t, cpp, "", "grpc.DestroyBattle.guard", destroyGuard)
	customCode = append(customCode, createGuard, destroyGuard)
	writeGrpcPreservationFile(t, headerPath, header)
	writeGrpcPreservationFile(t, cppPath, cpp)

	var previousHeader, previousCpp string
	for pass := 0; pass < 3; pass++ {
		generatedHeader, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
		if err != nil {
			t.Fatalf("第 %d 次头文件重生成失败: %v", pass+1, err)
		}
		generatedCpp := GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")
		for _, body := range headerSections {
			assertGrpcPreservedOnce(t, generatedHeader, body, "头文件自定义代码")
		}
		for _, body := range customCode {
			assertGrpcPreservedOnce(t, generatedCpp, body, "CPP 自定义代码")
		}
		assertGrpcPreservationOrder(t, grpcGuardFunction(t, generatedCpp, "BattleNode", "CreateBattle"),
			"[[maybe_unused]] const ::CreateBattleRequest* request = &call->request();",
			grpcPreservationBegin("grpc.CreateBattle.guard"),
			createGuard,
			_config.Global.Naming.YourCodeEnd,
			"call->Proceed();")
		assertGrpcPreservationOrder(t, grpcGuardFunction(t, generatedCpp, "BattleNode", "DestroyBattle"),
			grpcPreservationBegin("grpc.DestroyBattle.guard"), destroyGuard, "call->Proceed();")
		assertGrpcPreservationOrder(t, generatedCpp,
			"void BattleNodeImpl::RegisterMethods(muduo_grpc::MethodTable& table)",
			"table.AddUnary(std::string(kCreateBattlePath),",
			"CreateBattleCall::Create(std::move(serverCall), &HandleCreateBattle);",
			"if (call->ParseRequest(requestBytes))",
			"CreateBattle(call);")
		assertNoOfficialGrpcServerTokens(t, generatedHeader, "头文件")
		if pass > 0 && (generatedHeader != previousHeader || generatedCpp != previousCpp) {
			t.Fatalf("第 %d 次重生成发生字节漂移", pass+1)
		}
		previousHeader, previousCpp = generatedHeader, generatedCpp
		writeGrpcPreservationFile(t, headerPath, generatedHeader)
		writeGrpcPreservationFile(t, cppPath, generatedCpp)
	}
}

// 回归(上一轮评审遗留):守护段里出现 Handle* 查找键的字面量(注释、带类名限定的调用、段内顶格的行)不得把 codeParser 拉回那个方法。
// 修复前 grpc.<M>.guard 里写一句「与 SceneNodeGrpcImpl::HandleCreateScene( 同口径」,重生成后 HandleCreateScene 的手写体
// 就被覆盖成只剩结束标记(连开始标记都没有),且全程不报错。
func TestGrpcServiceHandler_LookupKeyLiteralsInSectionsKeepHandleBodies(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	methods := buildGrpcHandlerTestMethods()
	dir := t.TempDir()
	headerPath := filepath.Join(dir, "scene_node_service.h")
	cppPath := filepath.Join(dir, "scene_node_service.cpp")
	createKey := GenerateGrpcHandlerNameWrapper(methods[0], "")  // CreateScene
	destroyKey := GenerateGrpcHandlerNameWrapper(methods[1], "") // DestroyScene

	firstCode := "#include <memory>\n// 入口见 " + createKey + "request, response)"
	createBody := "    create_scene_impl(*request, *response); // 与 " + destroyKey + "request) 同一套校验"
	destroyBody := "    destroy_scene_impl(*request);"
	createGuard := "    // 准入口径与 " + createKey + "request, response) 一致\n" +
		"    " + destroyKey + "request);\n" +
		createKey + "nullptr, nullptr); // 段内顶格的行同样不认"
	releaseGuard := "    // 释放前复用 " + destroyKey + "request) 的校验"
	cpp := GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")
	cpp = fillGrpcPreservationSection(t, cpp, "", "", firstCode)
	cpp = fillGrpcPreservationSection(t, cpp, "void "+createKey, "", createBody)
	cpp = fillGrpcPreservationSection(t, cpp, "void "+destroyKey, "", destroyBody)
	cpp = fillGrpcPreservationSection(t, cpp, "", "grpc.CreateScene.guard", createGuard)
	cpp = fillGrpcPreservationSection(t, cpp, "", "grpc.ReleasePlayer.guard", releaseGuard)
	writeGrpcPreservationFile(t, cppPath, cpp)

	var previous string
	for pass := 0; pass < 3; pass++ {
		if _, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods); err != nil {
			t.Fatalf("第 %d 次头文件预检不应拒绝: %v", pass+1, err)
		}
		generated := GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")
		for _, body := range []string{firstCode, createBody, destroyBody, createGuard, releaseGuard} {
			assertGrpcPreservedOnce(t, generated, body, "含查找键字面量的自定义代码")
		}
		for _, key := range []string{createKey, destroyKey} {
			assertGrpcPreservedOnce(t, generated, "void "+key, "Handle* 定义行")
		}
		assertGrpcPreservationOrder(t, generated, "void "+createKey, _config.Global.Naming.YourCodeBegin+"\n"+createBody)
		if pass > 0 && generated != previous {
			t.Fatalf("第 %d 次重生成发生字节漂移", pass+1)
		}
		previous = generated
		writeGrpcPreservationFile(t, cppPath, generated)
	}
}

// 旧模板文件里新骨架没有位置的段全部为空时照常迁入新骨架:FirstCode 与当前方法的 Handle* 逐字保留,旧包装函数与空旧段丢弃。
// 同时覆盖不依赖当前 proto 方法表的几种形状:包装函数缺无名段(更早的旧文件)、包装函数签名折行、
// proto 已删除的方法 Create(与 CreateScene 同名前缀;它的 Handle* 体、包装函数无名段与两个命名段都为空)。
func TestGrpcServiceHandler_LegacyEmptySectionsMigrateToNewSkeleton(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	methods := buildGrpcHandlerTestMethods()
	fileMethods := withGrpcTestMethod(methods, "Create", ".CreateRequest", ".CreateResponse")
	dir := t.TempDir()
	headerPath := filepath.Join(dir, "scene_node_service.h")
	cppPath := filepath.Join(dir, "scene_node_service.cpp")
	firstCode := grpcLegacyPair("#include \"infra/agones/agones_gameserver_lifecycle.h\"")
	createBody := grpcLegacyPair("    create_scene_impl(*request, *response);")
	releaseBody := grpcLegacyPair("    release_player_impl(*request);")
	legacy := foldLegacyGrpcWrapperSignatures(t, legacyGrpcHandlerCpp(fileMethods, firstCode, map[string]string{
		"handle:CreateScene":    createBody,
		"handle:ReleasePlayer":  releaseBody,
		"omit_pre:DestroyScene": "",
	}), "SceneNodeGrpc", "CreateScene", "PrepareBattle", "Create")
	writeGrpcPreservationFile(t, cppPath, legacy)
	writeGrpcPreservationFile(t, headerPath, grpcPreservationBegin("grpc.header.includes")+"\n"+_config.Global.Naming.YourCodeEnd+"\n")

	header, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
	if err != nil {
		t.Fatalf("旧段全空时头文件预检不应拒绝: %v", err)
	}
	generated := GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")
	for _, body := range []string{firstCode, createBody, releaseBody} {
		assertGrpcPreservedOnce(t, generated, body, "旧 scene 无名守护段")
	}
	assertNoOfficialGrpcServerTokens(t, header, "迁移后的头文件")
	assertNoOfficialGrpcServerTokens(t, generated, "迁移后的 cpp")
	if strings.Contains(generated, "in_loop") || strings.Contains(generated, "post_dispatch") {
		t.Fatal("空旧段迁移后应丢弃")
	}
	for _, method := range methods {
		assertGrpcPreservedOnce(t, generated, grpcPreservationBegin(grpcGuardSectionName(method)), "迁移后补默认 guard 段")
	}
	for _, removed := range []string{"SceneNodeGrpcImpl::HandleCreate(", "grpc.Create.", "kCreatePath", "CreateCall"} {
		if strings.Contains(generated, removed) || strings.Contains(header, removed) {
			t.Fatalf("proto 已删除的方法(各段全空)迁移后应整体丢弃,仍出现 %q", removed)
		}
	}
	writeGrpcPreservationFile(t, cppPath, generated)
	if next := GetGrpcServiceHandlerCppStr(cppPath, methods, "", ""); next != generated {
		t.Fatal("旧 scene 文件迁入新骨架后再次生成发生字节漂移")
	}
}

// 前三例的固件取自 2026-10-16 工作区 cpp/nodes/{scene,battle}/handler/grpc/*.cpp 的真实非空旧段(scene 1 段、battle 4 段,
// 逐字照抄)。后三例钉住检测不依赖当前 proto 的方法表:proto 已删除的方法(fileMethods 比 methods 多一个)与折行签名。
// 生成器必须拒绝,并恰好列出这几段;.h 预检先于渲染,两份文件都不改。
func TestGrpcServiceHandler_LegacyNonEmptySectionsRefuseGeneration(t *testing.T) {
	setGrpcPreservationTestConfig(t) // 表里的 grpcLegacyPair 在子测试之前求值,需要先有标记配置
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	sceneWithDeletedCreate := withGrpcTestMethod(buildGrpcHandlerTestMethods(), "Create", ".CreateRequest", ".CreateResponse")
	for _, tc := range []struct {
		name        string
		methods     RPCMethods // 当前 proto 的方法表
		fileMethods RPCMethods // 拼旧文件用的方法表,为空时同 methods
		fold        []string   // 签名改成折行形状的旧包装函数(foldLegacyGrpcWrapperSignatures)
		base        string
		sections    map[string]string
		want        []string
		notWant     []string
	}{
		{
			name:     "scene",
			methods:  buildGrpcHandlerTestMethods(),
			base:     "scene_node_service",
			sections: map[string]string{"pre:CreateScene": workspaceSceneCreateScenePreDispatch},
			want:     []string{"CreateScene:" + legacyGrpcPreDispatchSectionName},
			notWant:  []string{"CreateScene:grpc.CreateScene.in_loop", "DestroyScene:"},
		},
		{
			name:    "battle",
			methods: buildBattleGrpcPreservationMethods(),
			base:    "battle_node",
			sections: map[string]string{
				"pre:CreateBattle":           workspaceBattleCreateBattlePreDispatch,
				"in_loop:CreateBattle":       workspaceBattleCreateBattleInLoop,
				"post_dispatch:CreateBattle": workspaceBattleCreateBattlePostDispatch,
				"in_loop:DestroyBattle":      workspaceBattleDestroyBattleInLoop,
			},
			want: []string{
				"CreateBattle:" + legacyGrpcPreDispatchSectionName,
				"CreateBattle:grpc.CreateBattle.in_loop",
				"CreateBattle:grpc.CreateBattle.post_dispatch",
				"DestroyBattle:grpc.DestroyBattle.in_loop",
			},
			notWant: []string{"DestroyBattle:" + legacyGrpcPreDispatchSectionName, "DestroyBattle:grpc.DestroyBattle.post_dispatch", "QueryBattleCreation:"},
		},
		{
			// 包装函数缺无名段(更早的旧文件):命名段只按名字列一次,不凭空记成无名投递前段。
			name:     "battle 包装函数缺无名段",
			methods:  buildBattleGrpcPreservationMethods(),
			base:     "battle_node",
			sections: map[string]string{"omit_pre:DestroyBattle": "", "in_loop:DestroyBattle": workspaceBattleDestroyBattleInLoop},
			want:     []string{"DestroyBattle:grpc.DestroyBattle.in_loop"},
			notWant:  []string{"DestroyBattle:" + legacyGrpcPreDispatchSectionName},
		},
		{
			// 已删方法 Create 与当前方法 CreateScene 同名前缀:段按签名精确归属,不串到 CreateScene。
			name:        "proto 已删除方法的旧段",
			methods:     buildGrpcHandlerTestMethods(),
			fileMethods: sceneWithDeletedCreate,
			base:        "scene_node_service",
			sections: map[string]string{
				"pre:Create":           grpcLegacyPair("    auto permit = acquire_create_permit();"),
				"in_loop:Create":       "        " + begin + ": grpc.Create.in_loop\n        dispatchStatus = reject_create();\n        " + end,
				"post_dispatch:Create": "    " + begin + ": grpc.Create.post_dispatch\n    // 迁移前的说明\n    " + end,
			},
			want: []string{
				"Create:" + legacyGrpcPreDispatchSectionName,
				"Create:grpc.Create.in_loop",
				"Create:grpc.Create.post_dispatch",
			},
			notWant: []string{"CreateScene:", "Create:" + orphanGrpcHandleSectionName("Create")},
		},
		{
			name:        "proto 已删除方法的 Handle* 体",
			methods:     buildGrpcHandlerTestMethods(),
			fileMethods: sceneWithDeletedCreate,
			base:        "scene_node_service",
			sections:    map[string]string{"handle:Create": grpcLegacyPair("    create_impl(*request, *response);")},
			want:        []string{"Create:" + orphanGrpcHandleSectionName("Create")},
			notWant:     []string{"Create:" + legacyGrpcPreDispatchSectionName, "CreateScene:"},
		},
		{
			// 按 "<Svc>Impl::<M>(grpc::ServerContext" 逐行查找(§15.16 第 5 条修复前的写法)会漏掉这三种折行签名,其中的无名段被静默丢弃。
			name:    "折行签名的旧包装函数",
			methods: buildGrpcHandlerTestMethods(),
			fold:    []string{"CreateScene", "DestroyScene", "PrepareBattle"},
			base:    "scene_node_service",
			sections: map[string]string{
				"pre:CreateScene":   workspaceSceneCreateScenePreDispatch,
				"pre:DestroyScene":  grpcLegacyPair("    // 迁移前的说明"),
				"pre:PrepareBattle": grpcLegacyPair("    check_prepare(*request);"),
			},
			want: []string{
				"CreateScene:" + legacyGrpcPreDispatchSectionName,
				"DestroyScene:" + legacyGrpcPreDispatchSectionName,
				"PrepareBattle:" + legacyGrpcPreDispatchSectionName,
			},
			notWant: []string{"ReleasePlayer:", "CreateScene:grpc.CreateScene.in_loop"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setGrpcPreservationTestConfig(t)
			dir := t.TempDir()
			headerPath := filepath.Join(dir, tc.base+".h")
			cppPath := filepath.Join(dir, tc.base+".cpp")
			fileMethods := tc.fileMethods
			if fileMethods == nil {
				fileMethods = tc.methods
			}
			legacyCpp := legacyGrpcHandlerCpp(fileMethods, _config.Global.Naming.YourCodePair, tc.sections)
			if len(tc.fold) > 0 {
				legacyCpp = foldLegacyGrpcWrapperSignatures(t, legacyCpp, tc.methods[0].Service(), tc.fold...)
			}
			legacyHeader := grpcPreservationBegin("grpc.header.includes") + "\n" + _config.Global.Naming.YourCodeEnd + "\n"
			writeGrpcPreservationFile(t, cppPath, legacyCpp)
			writeGrpcPreservationFile(t, headerPath, legacyHeader)

			header, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, tc.methods)
			if err == nil {
				t.Fatalf("旧段非空时头文件预检必须拒绝生成:\n%s", header)
			}
			assertLegacyGrpcSectionListing(t, err.Error(), cppPath, tc.want, tc.notWant)
			assertLegacyGrpcSectionListing(t, recoverGrpcGenerationPanic(t, func() {
				GetGrpcServiceHandlerCppStr(cppPath, tc.methods, "", "")
			}), cppPath, tc.want, tc.notWant)
			assertGrpcFileContent(t, cppPath, legacyCpp)
			assertGrpcFileContent(t, headerPath, legacyHeader)
		})
	}
}

// 新骨架文件里新生成带不走的非空无名段同样拒绝生成(checkLegacyGrpcSections 不只看旧模板):
// proto 已删除方法的 Handle* 体(先于该方法遗留的 grpc.<M>.guard「未识别」报出)、签名里找不到查找键的 Handle*、
// 查找键只在签名续行里的 Handle*(codeParser 只在签名起始行认键)、Handle* 里第一个段之后的无名段(两段时只列一行、按段计数)、
// <M>() 里的无名段、FirstCode 之后插在函数之间的无名段(codeParser 只把文件第一个段当 FirstCode)。
func TestGrpcServiceHandler_UnconsumedUnnamedSectionsRefuseGeneration(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	methods := buildGrpcHandlerTestMethods()
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	for _, tc := range []struct {
		name        string
		fileMethods RPCMethods // 生成初始骨架用的方法表,为空时同 methods
		edit        func(t *testing.T, cpp string) string
		want        string
		lost        int // 报错开头的非空段数,0 表示 1
	}{
		{
			name:        "proto 已删除方法的 Handle* 体",
			fileMethods: withGrpcTestMethod(methods, "Create", ".CreateRequest", ".CreateResponse"),
			edit: func(t *testing.T, cpp string) string {
				return fillGrpcPreservationSection(t, cpp, "void SceneNodeGrpcImpl::HandleCreate(", "", "    create_impl(*request, *response);")
			},
			want: "Create:" + orphanGrpcHandleSectionName("Create"),
		},
		{
			name: "Handle* 签名里找不到查找键",
			edit: func(t *testing.T, cpp string) string {
				cpp = fillGrpcPreservationSection(t, cpp, "void SceneNodeGrpcImpl::HandleDestroyScene(", "", "    destroy_impl(*request);")
				return replaceGrpcFixtureOnce(t, cpp, "void SceneNodeGrpcImpl::HandleDestroyScene(", "void SceneNodeGrpcImpl::HandleDestroyScene (")
			},
			want: "DestroyScene:" + orphanGrpcHandleSectionName("DestroyScene"),
		},
		{
			// 起始行认出的是别的限定名,查找键只出现在缩进的续行里:codeParser 不在续行上认键、带不走这个段,必须报出。
			name: "查找键只在签名续行里",
			edit: func(t *testing.T, cpp string) string {
				cpp = fillGrpcPreservationSection(t, cpp, "void SceneNodeGrpcImpl::HandleDestroyScene(", "", "    destroy_impl(*request);")
				return replaceGrpcFixtureOnce(t, cpp, "void SceneNodeGrpcImpl::HandleDestroyScene(",
					"void SceneNodeGrpcImpl::Wrapper(int unused,\n    SceneNodeGrpcImpl::HandleDestroyScene(")
			},
			want: "DestroyScene:" + orphanGrpcHandleSectionName("DestroyScene"),
		},
		{
			name: "Handle* 里第一个段之后的无名段",
			edit: func(t *testing.T, cpp string) string {
				cpp = fillGrpcPreservationSection(t, cpp, "void SceneNodeGrpcImpl::HandleCreateScene(", "", "    create_impl();")
				return replaceGrpcFixtureOnce(t, cpp, "    create_impl();\n"+end+"\n",
					"    create_impl();\n"+end+"\n"+begin+"\n    extra_impl();\n"+end+"\n")
			},
			want: "CreateScene:" + orphanGrpcHandleSectionName("CreateScene"),
		},
		{
			name: "Handle* 里第一个段之后的两个无名段",
			edit: func(t *testing.T, cpp string) string {
				cpp = fillGrpcPreservationSection(t, cpp, "void SceneNodeGrpcImpl::HandleCreateScene(", "", "    create_impl();")
				return replaceGrpcFixtureOnce(t, cpp, "    create_impl();\n"+end+"\n",
					"    create_impl();\n"+end+"\n"+begin+"\n    extra_impl();\n"+end+"\n"+begin+"\n    // 第二段\n"+end+"\n")
			},
			want: "CreateScene:" + orphanGrpcHandleSectionName("CreateScene") + "(2 段)",
			lost: 2,
		},
		{
			name: "<M>() 里的无名段",
			edit: func(t *testing.T, cpp string) string {
				anchor := "    [[maybe_unused]] const ::PrepareBattleRequest* request = &call->request();\n"
				return replaceGrpcFixtureOnce(t, cpp, anchor, anchor+"    "+begin+"\n    reject_early();\n    "+end+"\n")
			},
			want: "PrepareBattle:" + strayGrpcUnnamedSectionName,
		},
		{
			name: "FirstCode 之后的函数外无名段",
			edit: func(t *testing.T, cpp string) string {
				anchor := "void SceneNodeGrpcImpl::RegisterMethods("
				return replaceGrpcFixtureOnce(t, cpp, anchor, begin+"\n// 手写的辅助说明\n"+end+"\n\n"+anchor)
			},
			want: outsideGrpcFunctionLabel + ":" + strayGrpcUnnamedSectionName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setGrpcPreservationTestConfig(t)
			dir := t.TempDir()
			headerPath := filepath.Join(dir, "scene_node_service.h")
			cppPath := filepath.Join(dir, "scene_node_service.cpp")
			fileMethods := tc.fileMethods
			if fileMethods == nil {
				fileMethods = methods
			}
			cpp := tc.edit(t, GetGrpcServiceHandlerCppStr(cppPath, fileMethods, "", ""))
			writeGrpcPreservationFile(t, cppPath, cpp)

			header, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
			if err == nil {
				t.Fatalf("新生成带不走的非空无名段必须让头文件预检拒绝生成:\n%s", header)
			}
			lost := tc.lost
			if lost == 0 {
				lost = 1
			}
			head := fmt.Sprintf("%d 个守护段非空", lost)
			for _, message := range []string{err.Error(), recoverGrpcGenerationPanic(t, func() {
				GetGrpcServiceHandlerCppStr(cppPath, methods, "", "")
			})} {
				assertLegacyGrpcSectionListing(t, message, cppPath, []string{tc.want}, nil)
				if !strings.HasPrefix(message, head) {
					t.Fatalf("报错开头应按段计数 %q\n%s", head, message)
				}
			}
			assertGrpcFileContent(t, cppPath, cpp)
		})
	}
}

// 当前工作区 4 个已迁移的 handler 文件(只读,不写回):更严的扫描不得误报;两份文件照常生成,
// 文件里的每个守护段(FirstCode、Handle* 体、grpc.<M>.guard、头文件三段)都逐字出现在生成结果里,且不多不少。
// 方法表是照抄 proto 的夹具(buildGrpcHandlerTestMethods / buildBattleGrpcPreservationMethods):proto 增删方法后须同步夹具。
func TestGrpcServiceHandler_WorkspaceHandlersRegenerateWithoutFalsePositive(t *testing.T) {
	root := grpcWorkspaceRoot(t)
	for _, tc := range []struct {
		name    string
		dir     string
		base    string
		methods RPCMethods
	}{
		{"scene", "cpp/nodes/scene/handler/grpc", "scene_node_service", buildGrpcHandlerTestMethods()},
		{"battle", "cpp/nodes/battle/handler/grpc", "battle_node", buildBattleGrpcPreservationMethods()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setGrpcPreservationTestConfig(t)
			headerPath := filepath.Join(root, filepath.FromSlash(tc.dir), tc.base+".h")
			cppPath := filepath.Join(root, filepath.FromSlash(tc.dir), tc.base+".cpp")
			if err := checkLegacyGrpcSections(cppPath, tc.methods); err != nil {
				t.Fatalf("已迁移的工作区文件不应报旧段(若 proto 增删了方法,先同步方法表夹具): %v", err)
			}
			header, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, tc.methods)
			if err != nil {
				t.Fatalf("工作区头文件应照常生成: %v", err)
			}
			cpp := GetGrpcServiceHandlerCppStr(cppPath, tc.methods, "", "")
			for path, generated := range map[string]string{headerPath: header, cppPath: cpp} {
				source := readGrpcFileLF(t, path)
				blocks := grpcGuardedBlocks(source)
				if len(blocks) == 0 {
					t.Fatalf("%s 里没有守护段,夹具路径可能已变", path)
				}
				if got := len(grpcGuardedBlocks(generated)); got != len(blocks) {
					t.Fatalf("%s 重生成后守护段数量变了: got=%d want=%d", path, got, len(blocks))
				}
				for _, block := range blocks {
					if got, want := strings.Count(generated, block), strings.Count(source, block); got != want {
						t.Fatalf("%s 的守护段重生成后未逐字保留: got=%d want=%d\n%s", path, got, want, block)
					}
				}
			}
		})
	}
}

func TestGrpcServiceHandler_HeaderRejectsMalformedNamedSections(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	begin := grpcPreservationBegin("grpc.header.includes")
	end := _config.Global.Naming.YourCodeEnd
	for _, tc := range []struct {
		name string
		code string
	}{
		{"未知头文件段", grpcPreservationBegin("grpc.header.priavte") + "\n    battle_admission::AdmissionGate admission_;\n" + end},
		{"同名重复", begin + "\n#include \"a.h\"\n" + end + "\n" + begin + "\n#include \"b.h\"\n" + end},
		{"未闭合", begin + "\n#include \"a.h\"\n"},
		{"嵌套", begin + "\n" + grpcPreservationBegin("grpc.header.private") + "\n" + end + "\n" + end},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "battle_node.h")
			writeGrpcPreservationFile(t, dst, tc.code)
			if _, err := GetGrpcServiceHandlerHeadFromFileStr(dst, buildBattleGrpcPreservationMethods()); err == nil {
				t.Fatal("坏守护段必须报错，不能静默丢失或覆盖代码")
			}
		})
	}
}

// 头文件里的无名段与 cpp 同口径 fail-closed:骨架只渲染三个命名段,非空无名段在新生成里没有位置 ——
// 拒绝生成并逐行列出「文件:开始标记所在行」,空的不列;全部为空时照常生成并丢弃(与首次生成的骨架逐字相同)。
func TestGrpcServiceHandler_HeaderUnnamedSections(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	methods := buildBattleGrpcPreservationMethods()
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	pair := _config.Global.Naming.YourCodePair
	dir := t.TempDir()
	headerPath := filepath.Join(dir, "battle_node.h")
	fresh, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
	if err != nil {
		t.Fatalf("新建头文件失败: %v", err)
	}

	t.Run("非空无名段拒绝生成", func(t *testing.T) {
		// 类外一个非空、一个空,类内一个非空(缩进)。
		header := replaceGrpcFixtureOnce(t, fresh, "class BattleNodeImpl final",
			begin+"\n#include \"lost.h\"\n"+end+"\n"+pair+"\n\nclass BattleNodeImpl final")
		header = replaceGrpcFixtureOnce(t, header, "    muduo::net::EventLoop& loop_;\n",
			"    muduo::net::EventLoop& loop_;\n    "+begin+"\n    int lost_ = 0;\n    "+end+"\n")
		var unnamedLines []int
		for i, line := range strings.Split(header, "\n") {
			if strings.TrimSpace(line) == begin {
				unnamedLines = append(unnamedLines, i+1)
			}
		}
		if len(unnamedLines) != 3 {
			t.Fatalf("夹具应有 3 个无名段: %v", unnamedLines)
		}
		writeGrpcPreservationFile(t, headerPath, header)

		generated, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
		if err == nil {
			t.Fatalf("头文件非空无名段必须拒绝生成,不能静默丢弃:\n%s", generated)
		}
		message := err.Error()
		if !strings.HasPrefix(message, "头文件有 2 个无名守护段非空") {
			t.Fatalf("报错开头应按非空段计数:\n%s", message)
		}
		for i, line := range unnamedLines {
			location := fmt.Sprintf("%s:%d", headerPath, line)
			listed := strings.Contains(message, location+"\n") || strings.HasSuffix(message, location)
			if wantListed := i != 1; listed != wantListed {
				t.Fatalf("第 %d 个无名段(第 %d 行)是否列出不对: listed=%v want=%v\n%s", i+1, line, listed, wantListed, message)
			}
		}
		assertGrpcFileContent(t, headerPath, header)
	})

	t.Run("空无名段丢弃", func(t *testing.T) {
		header := replaceGrpcFixtureOnce(t, fresh, "class BattleNodeImpl final", pair+"\n\nclass BattleNodeImpl final")
		writeGrpcPreservationFile(t, headerPath, header)
		generated, err := GetGrpcServiceHandlerHeadFromFileStr(headerPath, methods)
		if err != nil {
			t.Fatalf("全空的无名段应照常生成: %v", err)
		}
		if generated != fresh {
			t.Fatalf("空无名段应被丢弃,结果应与首次生成的骨架逐字相同:\n%s", generated)
		}
	})
}

// .h 预检对同名 .cpp 做与 .cpp 生成相同的检查:cpp 里的坏段或未识别段也让头文件拒绝生成。
func TestGrpcServiceHandler_HeaderPrecheckRejectsBrokenCpp(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	end := _config.Global.Naming.YourCodeEnd
	for _, tc := range []struct {
		name string
		code string
	}{
		{"guard 段名拼错", grpcPreservationBegin("grpc.CreateBattel.guard") + "\n    call->Reject(NotAllocatable());\n" + end},
		{"旧 in_loop 段非空", grpcPreservationBegin("grpc.DestroyBattle.in_loop") + "\n    // 迁移前的说明\n" + end},
		{"cpp 守护段未闭合", grpcPreservationBegin("grpc.CreateBattle.guard") + "\n    return;\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGrpcPreservationFile(t, filepath.Join(dir, "battle_node.cpp"), tc.code)
			if _, err := GetGrpcServiceHandlerHeadFromFileStr(filepath.Join(dir, "battle_node.h"), buildBattleGrpcPreservationMethods()); err == nil {
				t.Fatal("同名 cpp 的守护段有问题时头文件必须拒绝生成")
			}
		})
	}
	// 推不出同名 cpp(头文件扩展名不符)时宁可拒绝,也不跳过预检。
	if _, err := GetGrpcServiceHandlerHeadFromFileStr(filepath.Join(t.TempDir(), "battle_node.hpp"), buildBattleGrpcPreservationMethods()); err == nil {
		t.Fatal("推不出同名 cpp 时必须拒绝生成")
	}
}

func TestGrpcServiceHandler_CppRejectsUnconsumedOrMalformedSections(t *testing.T) {
	setGrpcPreservationTestConfig(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	namedBegin := grpcPreservationBegin("grpc.CreateBattle.guard")
	for _, tc := range []struct {
		name string
		code string
	}{
		{"RPC 名称拼错", grpcPreservationBegin("grpc.CreateBattel.guard") + "\n    call->Reject(NotAllocatable());\n" + end},
		{"未知阶段", grpcPreservationBegin("grpc.CreateBattle.pre_dispatch") + "\n    return;\n" + end},
		{"旧 post_dispatch 段非空", grpcPreservationBegin("grpc.CreateBattle.post_dispatch") + "\n    LOG_WARN << \"rejected\";\n" + end},
		{"无名外段未闭合", begin + "\n#include \"battle_admission_gate.h\"\n"},
		{"无名段内嵌套命名段", begin + "\n" + namedBegin + "\n    call->Reject(NotAllocatable());\n" + end + "\n" + end},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "battle_node.cpp")
			writeGrpcPreservationFile(t, dst, tc.code)
			recoverGrpcGenerationPanic(t, func() {
				GetGrpcServiceHandlerCppStr(dst, buildBattleGrpcPreservationMethods(), "", "")
			})
		})
	}
}

// 真实工作区内容,逐字照抄(见 TestGrpcServiceHandler_LegacyNonEmptySectionsRefuseGeneration)。
// scene_node_service.cpp CreateScene 包装函数的无名投递前段。
const workspaceSceneCreateScenePreDispatch = `///<<< BEGIN WRITING YOUR CODE
    // Agones 高密度模式:进程必须先被 Allocate,才能创建第一个场景。
    //
    // 刻意放在 runInLoop 之前(gRPC 线程上):POST /allocate 是网络调用,
    // 放进事件循环会卡住整个逻辑帧;这里阻塞的只是一个 gRPC 池线程,
    // 且等待有上限(LifecycleOptions::allocateWaitTimeout)。
    //
    // 失败一律 fail-closed:不建实体,返回非 OK,让 SceneManager 回滚重试,
    // 而不是留下一个 Agones 不知道的房间。createPermit 必须活到本函数返回
    // (覆盖 future.get()),在途创建才能阻止提前回到 Ready。
    //
    // 2026-09-15 恢复:原块在 commit 6c4021ae5 被 proto 重生成吞掉(当时位于守护段外)。
    auto createPermit = agones::GameServerLifecycle::Instance().AcquireAllocationPermitBlocking();
    if (!createPermit)
    {
        LOG_ERROR << "[gRPC] CreateScene rejected: Agones allocate not confirmed, scene_id="
                  << request->scene_id() << " state="
                  << agones::ToString(agones::GameServerLifecycle::Instance().State());
        return grpc::Status(grpc::StatusCode::UNAVAILABLE, "agones allocate not confirmed");
    }
///<<< END WRITING YOUR CODE`

// battle_node.cpp CreateBattle 包装函数的无名投递前段。
const workspaceBattleCreateBattlePreDispatch = `    ///<<< BEGIN WRITING YOUR CODE
    // 节点级准入分三道,全部在任何副作用之前;任一道不过即回 UNAVAILABLE battle_not_allocatable,
    // match 据此跳过 DestroyBattle、换节点重试一次。
    // 顺序:准入闸(本线程)→ 许可(本线程)→ 准入闸复核(loop 内)→ 预签票据(loop 内,BuildAssignment)
    // → 插表(EmplaceRoom)。
    // NodeInfo 由 Node 持有、只在 loop 线程写;跨线程读写不同步,这里不能读取它或调用 ClientFacing。

    // 1) 建房准入闸(battle_admission_gate.h):启动完成之前(lifecycle 还没启动,Disabled 默认值会
    //    不经 allocate 放行许可)与停机开始之后都不接新房间。闸没开就连许可都不取。
    if (const auto phase = admission_.Phase(); phase != battle_admission::AdmissionPhase::kOpen)
    {
        LOG_WARN << "CreateBattle rejected: this node is not accepting new rooms, battle_id=" << request->battle_id()
                 << " admission=" << battle_admission::ToString(phase)
                 << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
        return NotAllocatable();
    }

    // 2) 分配许可(集群外入口 D82):必须在 gRPC 线程上、runInLoop 之前取 ——
    //    AcquireAllocationPermitBlocking 会有界阻塞调用线程等 Agones allocate(上限
    //    LifecycleOptions.allocateWaitTimeout),绝不能进 EventLoop。
    //    拿不到(排空标签在、allocate 未确认、lifecycle 已停 / 仍在 Starting)即拒绝:
    //    此刻还没投递 loop,房间、定时器、推送、Kafka 事件一概没有发生。
    //    非 Agones 环境 lifecycle 为 Disabled,许可恒放行(启动 / 停机窗口由第 1、3 道兜住)。
    auto &lifecycle = agones::GameServerLifecycle::Instance();
    auto permit = lifecycle.AcquireAllocationPermitBlocking();
    if (!permit)
    {
        LOG_WARN << "CreateBattle rejected: this instance is not allocatable, battle_id=" << request->battle_id()
                 << " agones_state=" << agones::ToString(lifecycle.State())
                 << " draining=" << (lifecycle.IsDraining() ? 1 : 0)
                 << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
        return NotAllocatable();
    }
    // permit 活到本函数返回(覆盖 future.get()):在途创建计入 lifecycle,
    // 防止"最后一个旧房间刚移除"时 worker 抢先回 Ready;析构时若零房间(拒绝 / 幂等命中)自动收口回 Ready。

    bool closedInLoop = false;

    // 捕获的全是本栈帧与本对象的引用:本线程阻塞在 future.get() 直到任务跑完,寿命覆盖任务
    // (与其余 RPC 的 &promise 同理);任务若因 loop 退出而永不执行,也就不会解引用。
    auto &admission = admission_;
    ///<<< END WRITING YOUR CODE`

// battle_node.cpp grpc.CreateBattle.in_loop。
const workspaceBattleCreateBattleInLoop = `        ///<<< BEGIN WRITING YOUR CODE: grpc.CreateBattle.in_loop
        // 3) loop 内复核准入闸:第 1 道之后、任务排队期间,SetBeforeShutdown 可能已经关闸并作废全部
        //    房间(关闸与 AbortAllRooms 在同一个 loop 任务里,排在它后面的任务必然看到 kClosed)。
        //    此刻还没进 HandleCreateBattle,没有任何副作用;放行的话房间会在进程退出时无声丢失。
        if (admission.Phase() != battle_admission::AdmissionPhase::kOpen)
        {
            closedInLoop = true;
            dispatchStatus = NotAllocatable();
        }
        ///<<< END WRITING YOUR CODE`

// battle_node.cpp grpc.CreateBattle.post_dispatch。
const workspaceBattleCreateBattlePostDispatch = `    ///<<< BEGIN WRITING YOUR CODE: grpc.CreateBattle.post_dispatch
    if (closedInLoop)
    {
        LOG_WARN << "CreateBattle rejected: shutdown started during dispatch(rechecked in loop), battle_id=" << request->battle_id()
                 << ",returning UNAVAILABLE " << kBattleNotAllocatableMessage;
    }
    ///<<< END WRITING YOUR CODE`

// battle_node.cpp grpc.DestroyBattle.in_loop(另一会话的放置校验)。
const workspaceBattleDestroyBattleInLoop = `        ///<<< BEGIN WRITING YOUR CODE: grpc.DestroyBattle.in_loop
        if(!BattleRoomManager::Instance().AcceptsPlacementOperation(request->battle_id(),request->placement_operation_id(),request->target_instance_id()))
            dispatchStatus=grpc::Status(grpc::StatusCode::FAILED_PRECONDITION,"placement_operation_mismatch");
        ///<<< END WRITING YOUR CODE`

// legacyGrpcHandlerCpp 按第 4 批之前的旧模板(官方 grpc::Server + runInLoop/promise)拼出 handler cpp,
// 作为迁移与拒绝用例的输入。sections 的键:"handle:<M>"、"pre:<M>"(无名投递前段)、"in_loop:<M>"、
// "post_dispatch:<M>",值含首尾标记,缺省为空段;键 "omit_pre:<M>" 存在时不写无名投递前段(更早的旧文件)。
func legacyGrpcHandlerCpp(methods RPCMethods, firstCode string, sections map[string]string) string {
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	pair := _config.Global.Naming.YourCodePair
	section := func(key, fallback string) string {
		if code, exists := sections[key]; exists {
			return code
		}
		return fallback
	}
	service := methods[0].Service()
	var b strings.Builder
	b.WriteString("#include \"" + methods[0].FileBaseNameNoEx() + ".h\"\n#include <future>\n")
	b.WriteString(firstCode + "\n\n")
	b.WriteString(service + "Impl::" + service + "Impl(muduo::net::EventLoop& loop)\n    : loop_(loop)\n{\n}\n")
	for _, method := range methods {
		name := method.Method()
		b.WriteString("\nvoid " + service + "Impl::Handle" + name + "(const " + method.CppRequest() + "* request")
		if !isVoidResponse(method) {
			b.WriteString(",\n    " + method.CppResponse() + "* response")
		}
		b.WriteString(")\n{\n" + section("handle:"+name, pair) + "\n}\n")
	}
	for _, method := range methods {
		name := method.Method()
		responseParam, handleArgs := " response", "request, response"
		if isVoidResponse(method) {
			responseParam, handleArgs = " /*response*/", "request"
		}
		b.WriteString("\ngrpc::Status " + service + "Impl::" + name + "(grpc::ServerContext* /*context*/,\n" +
			"    const " + method.CppRequest() + "* request,\n" +
			"    " + method.CppResponse() + "*" + responseParam + ")\n{\n")
		if _, omitted := sections["omit_pre:"+name]; !omitted {
			b.WriteString(section("pre:"+name, pair) + "\n")
		}
		b.WriteString("    std::promise<grpc::Status> promise;\n" +
			"    auto future = promise.get_future();\n\n" +
			"    loop_.runInLoop([&]\n" +
			"                    {\n" +
			"        grpc::Status dispatchStatus = grpc::Status::OK;\n" +
			section("in_loop:"+name, "        "+begin+": grpc."+name+".in_loop\n        "+end) + "\n" +
			"        if (dispatchStatus.ok())\n" +
			"        {\n" +
			"            Handle" + name + "(" + handleArgs + ");\n" +
			"        }\n" +
			"        promise.set_value(dispatchStatus); });\n\n" +
			"    auto status = future.get();\n" +
			section("post_dispatch:"+name, "    "+begin+": grpc."+name+".post_dispatch\n    "+end) + "\n" +
			"    return status;\n" +
			"}\n")
	}
	return b.String()
}

// foldLegacyGrpcWrapperSignatures 把 legacyGrpcHandlerCpp 产出的旧包装函数签名改成折行形状,按 names 的顺序轮流用三种写法:
// "(" 后换行再写 grpc::ServerContext;返回类型独占一行且 "(" 前有空格;"(" 后换行并写成 ::grpc::ServerContext。
func foldLegacyGrpcWrapperSignatures(t *testing.T, code, service string, names ...string) string {
	t.Helper()
	for i, name := range names {
		qualified := service + "Impl::" + name
		folded := [...]string{
			"grpc::Status " + qualified + "(\n    grpc::ServerContext*",
			"grpc::Status\n" + qualified + " (grpc::ServerContext*",
			"grpc::Status " + qualified + "(\n    ::grpc::ServerContext*",
		}[i%3]
		code = replaceGrpcFixtureOnce(t, code, "grpc::Status "+qualified+"(grpc::ServerContext*", folded)
	}
	return code
}

// replaceGrpcFixtureOnce 改写夹具;from 必须恰好出现一次,防止用例空转。
func replaceGrpcFixtureOnce(t *testing.T, code, from, to string) string {
	t.Helper()
	if got := strings.Count(code, from); got != 1 {
		t.Fatalf("夹具里 %q 应恰好出现一次: got=%d", from, got)
	}
	return strings.Replace(code, from, to, 1)
}

// withGrpcTestMethod 在 methods 之后追加一个同服务的 unary 方法,返回新表(不改 methods):模拟旧文件里有、proto 里已删除的方法。
func withGrpcTestMethod(methods RPCMethods, name, request, response string) RPCMethods {
	extended := append(RPCMethods{}, methods...)
	return append(extended, &MethodInfo{
		ProtoFileInfo: methods[0].ProtoFileInfo,
		MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{
			Name: proto.String(name), InputType: proto.String(request), OutputType: proto.String(response),
		},
	})
}

// grpcGuardedBlocks 按标记行切出 code 里的每个守护段(含首尾标记行,LF 换行),规则同 readNamedGrpcCodeSections。
func grpcGuardedBlocks(code string) []string {
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	var blocks, current []string
	for _, line := range strings.Split(code, "\n") {
		trimmed := strings.TrimSpace(line)
		if current == nil {
			if trimmed == begin || strings.HasPrefix(trimmed, begin+":") {
				current = []string{line}
			}
			continue
		}
		current = append(current, line)
		if trimmed == end {
			blocks = append(blocks, strings.Join(current, "\n"))
			current = nil
		}
	}
	return blocks
}

// readGrpcFileLF 读文件并把 CRLF 统一成 LF:工作区按 core.autocrlf 可能检出成 CRLF,生成器逐行读取,产物本就不带 CR。
func readGrpcFileLF(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// grpcWorkspaceRoot 自包目录向上找仓库根(以 scene 的 gRPC handler 文件为准);生成器脱离仓库单独运行测试时跳过。
func grpcWorkspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	marker := filepath.Join("cpp", "nodes", "scene", "handler", "grpc", "scene_node_service.cpp")
	for {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("未找到仓库里的 cpp/nodes/*/handler/grpc,跳过工作区真实文件回归")
		}
		dir = parent
	}
}

func buildBattleGrpcPreservationMethods() RPCMethods {
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("proto/battle/battle_node.proto")}
	svc := &descriptorpb.ServiceDescriptorProto{Name: proto.String("BattleNode")}
	names := []string{"QueryObserver", "QueryBattleCreation", "CreateBattle", "DestroyBattle", "AddObserver", "RemoveObserver", "IssueBattleTicket"}
	methods := make(RPCMethods, 0, len(names))
	for _, name := range names {
		response := "." + name + "Response"
		if name == "DestroyBattle" {
			response = ".Empty"
		}
		methods = append(methods, &MethodInfo{
			ProtoFileInfo: ProtoFileInfo{Fd: fd, ServiceDescriptorProto: svc},
			MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{
				Name: proto.String(name), InputType: proto.String("." + name + "Request"), OutputType: proto.String(response),
			},
		})
	}
	return methods
}

func setGrpcPreservationTestConfig(t *testing.T) {
	t.Helper()
	setParserTestNaming(t)
	oldExt := _config.Global.FileExtensions
	_config.Global.FileExtensions.Proto = ".proto"
	_config.Global.FileExtensions.PbH = ".pb.h"
	_config.Global.FileExtensions.GrpcHandlerH = ".h"
	_config.Global.FileExtensions.GrpcHandlerCpp = ".cpp"
	_config.Global.Naming.IncludeBegin = "#include \""
	t.Cleanup(func() { _config.Global.FileExtensions = oldExt })
}

func grpcPreservationBegin(name string) string {
	return _config.Global.Naming.YourCodeBegin + ": " + name
}

// grpcLegacyPair 把 body 包进无名守护段标记。
func grpcLegacyPair(body string) string {
	return _config.Global.Naming.YourCodeBegin + "\n" + body + "\n" + _config.Global.Naming.YourCodeEnd
}

func fillGrpcPreservationSection(t *testing.T, code, signature, name, body string) string {
	t.Helper()
	from := 0
	if signature != "" {
		from = strings.Index(code, signature)
		if from < 0 {
			t.Fatalf("夹具缺少函数签名 %q", signature)
		}
	}
	begin := _config.Global.Naming.YourCodeBegin
	if name != "" {
		begin = grpcPreservationBegin(name)
	}
	start := strings.Index(code[from:], begin+"\n")
	if start < 0 {
		t.Fatalf("夹具缺少守护段 %q", begin)
	}
	bodyStart := from + start + len(begin) + 1
	end := strings.Index(code[bodyStart:], _config.Global.Naming.YourCodeEnd)
	if end < 0 {
		t.Fatalf("夹具守护段 %q 没有结束标记", begin)
	}
	bodyEnd := bodyStart + end
	if strings.TrimSpace(code[bodyStart:bodyEnd]) != "" {
		t.Fatalf("夹具守护段 %q 应为空段", begin)
	}
	return code[:bodyStart] + body + "\n" + code[bodyEnd:]
}

func assertGrpcPreservedOnce(t *testing.T, code, want, label string) {
	t.Helper()
	if got := strings.Count(code, want); got != 1 {
		t.Fatalf("%s 应逐字保留一次: got=%d want=%q\n%s", label, got, want, code)
	}
}

func assertGrpcPreservationOrder(t *testing.T, code string, parts ...string) {
	t.Helper()
	from := 0
	for _, part := range parts {
		at := strings.Index(code[from:], part)
		if at < 0 {
			t.Fatalf("执行顺序错误，在前一步之后未找到 %q\n%s", part, code)
		}
		from += at + len(part)
	}
}

// grpcGuardFunction 截出生成的 <M>() 函数体(到顶格的结束括号为止)。
func grpcGuardFunction(t *testing.T, code, service, method string) string {
	t.Helper()
	signature := "void " + service + "Impl::" + method + "(const std::shared_ptr<" + method + "Call>& call)\n"
	start := strings.Index(code, signature)
	if start < 0 {
		t.Fatalf("未生成 %s.%s 的 guard 函数", service, method)
	}
	end := strings.Index(code[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s.%s 的 guard 函数没有结束括号", service, method)
	}
	return code[start : start+end+3]
}

// 生成物里不得出现官方服务端的痕迹(§15.8.2)。grpc:: 按词首匹配,不误伤 muduo_grpc::。
var officialGrpcServerTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)(^|[^A-Za-z0-9_])grpc::`),
	regexp.MustCompile(`grpcpp`),
	regexp.MustCompile(`<future>`),
	regexp.MustCompile(`promise`),
	regexp.MustCompile(`\.grpc\.pb\.h`),
	regexp.MustCompile(`runInLoop`),
}

func assertNoOfficialGrpcServerTokens(t *testing.T, code, label string) {
	t.Helper()
	for _, pattern := range officialGrpcServerTokenPatterns {
		if location := pattern.FindStringIndex(code); location != nil {
			t.Fatalf("%s 不得出现 %q(命中 %q)\n%s", label, pattern.String(), code[location[0]:location[1]], code)
		}
	}
}

func assertLegacyGrpcSectionListing(t *testing.T, message, cppPath string, want, notWant []string) {
	t.Helper()
	if got := strings.Count(message, cppPath+":"); got != len(want) {
		t.Fatalf("报错应恰好列出 %d 段旧段: got=%d\n%s", len(want), got, message)
	}
	for _, entry := range want {
		if !strings.Contains(message, cppPath+":"+entry+"\n") && !strings.HasSuffix(message, cppPath+":"+entry) {
			t.Fatalf("报错缺少 %q\n%s", entry, message)
		}
	}
	for _, entry := range notWant {
		if strings.Contains(message, cppPath+":"+entry) {
			t.Fatalf("空旧段不应被列出: %q\n%s", entry, message)
		}
	}
	for _, hint := range []string{"§15.8.4", "grpc.<M>.guard"} {
		if !strings.Contains(message, hint) {
			t.Fatalf("报错应提示迁移去向 %q\n%s", hint, message)
		}
	}
}

func recoverGrpcGenerationPanic(t *testing.T, generate func()) (message string) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("未知、损坏或未迁移的守护段必须中止生成，不能静默丢弃代码")
		}
		message = fmt.Sprint(recovered)
	}()
	generate()
	return ""
}

func assertGrpcFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("拒绝生成时不得改动 %s", path)
	}
}

func writeGrpcPreservationFile(t *testing.T, dst, code string) {
	t.Helper()
	if err := os.WriteFile(dst, []byte(code), 0o600); err != nil {
		t.Fatalf("写入临时生成文件失败: %v", err)
	}
}

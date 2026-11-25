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

func TestReadCodeSections_MethodBlocksNotMisclassifiedAsGlobal(t *testing.T) {
	oldNaming := _config.Global.Naming
	_config.Global.Naming.YourCodeBegin = "///<<< BEGIN WRITING YOUR CODE"
	_config.Global.Naming.YourCodeEnd = "///<<< END WRITING YOUR CODE"
	_config.Global.Naming.YourCodePair = _config.Global.Naming.YourCodeBegin + "\n" + _config.Global.Naming.YourCodeEnd
	_config.Global.Naming.HandlerFile = "Handler"
	t.Cleanup(func() {
		_config.Global.Naming = oldNaming
	})

	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene", "EnterScene")
	cpp := strings.Join([]string{
		"void ScenePlayerSceneHandler::LeaveScene(int a)",
		"{",
		"///<<< BEGIN WRITING YOUR CODE",
		"\tleave_impl();",
		"///<<< END WRITING YOUR CODE",
		"}",
		"",
		"void ScenePlayerSceneHandler::EnterScene(int a)",
		"{",
		"///<<< BEGIN WRITING YOUR CODE",
		"\tenter_impl();",
		"///<<< END WRITING YOUR CODE",
		"}",
		"",
	}, "\n")

	filePath := writeParserTempFile(t, cpp)
	codeMap, firstCode, err := ReadCodeSectionsFromFile(filePath, &methods, GenerateMethodHandlerNameWrapper, "")
	if err != nil {
		t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
	}

	if firstCode != _config.Global.Naming.YourCodePair {
		t.Fatalf("unexpected firstCode: got=%q want=%q", firstCode, _config.Global.Naming.YourCodePair)
	}

	leaveKey := "ScenePlayerSceneHandler::LeaveScene("
	enterKey := "ScenePlayerSceneHandler::EnterScene("

	if !strings.Contains(codeMap[leaveKey], "leave_impl") {
		t.Fatalf("leave method code not captured correctly: %q", codeMap[leaveKey])
	}
	if !strings.Contains(codeMap[enterKey], "enter_impl") {
		t.Fatalf("enter method code not captured correctly: %q", codeMap[enterKey])
	}
	if strings.Count(codeMap[leaveKey], _config.Global.Naming.YourCodeEnd) != 1 {
		t.Fatalf("leave method end marker count mismatch: %q", codeMap[leaveKey])
	}
}

// legacyGrpcWrapperKey 是旧模板 gRPC 包装函数形状的查找键,只在本文件里回归 codeParser 的取段边界:
// 带上 "(grpc::ServerContext" 不与 Handle* 键、以及同名前缀方法(如 Create / CreateScene)互相命中。
// 生成器的旧段检查已不用它,改为按签名形状扫描(checkLegacyGrpcSections / legacyGrpcWrapperPattern)。
func legacyGrpcWrapperKey(info *MethodInfo, _ string) string {
	return info.Service() + "Impl::" + info.Method() + "(grpc::ServerContext"
}

// 旧模板 gRPC 包装函数的"投递前"守护段(codeParser 取段边界回归):按旧包装函数键解析只取包装函数里的段,不串到 Handle* 的段,
// 同名前缀方法(Create / CreateScene)不互相命中,没有守护段的包装函数回落默认空段。
func TestReadCodeSections_GrpcWrapperPreDispatchSections(t *testing.T) {
	setParserTestNaming(t)

	methods := buildParserTestMethods("SceneNodeGrpc", "Create", "CreateScene", "DestroyScene")
	cpp := strings.Join([]string{
		"#include \"scene_node_service.h\"",
		"///<<< BEGIN WRITING YOUR CODE",
		"#include \"agones/agones_scene_lifecycle.h\"",
		"///<<< END WRITING YOUR CODE",
		"",
		"void SceneNodeGrpcImpl::HandleCreateScene(const ::CreateSceneRequest* request,",
		"    ::CreateSceneResponse* response)",
		"{",
		"///<<< BEGIN WRITING YOUR CODE",
		"    handle_create_scene_impl();",
		"///<<< END WRITING YOUR CODE",
		"}",
		"",
		"grpc::Status SceneNodeGrpcImpl::Create(grpc::ServerContext* /*context*/,",
		"    const ::CreateRequest* request,",
		"    ::CreateResponse* response)",
		"{",
		"    std::promise<void> promise;",
		"    return grpc::Status::OK;",
		"}",
		"",
		"grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* /*context*/,",
		"    const ::CreateSceneRequest* request,",
		"    ::CreateSceneResponse* response)",
		"{",
		"///<<< BEGIN WRITING YOUR CODE",
		"    auto createPermit = acquire_permit();",
		"///<<< END WRITING YOUR CODE",
		"    std::promise<void> promise;",
		"    return grpc::Status::OK;",
		"}",
		"",
	}, "\n")

	filePath := writeParserTempFile(t, cpp)
	codeMap, _, err := ReadCodeSectionsFromFile(filePath, &methods, legacyGrpcWrapperKey, "")
	if err != nil {
		t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
	}

	createSceneKey := "SceneNodeGrpcImpl::CreateScene(grpc::ServerContext"
	createKey := "SceneNodeGrpcImpl::Create(grpc::ServerContext"
	destroyKey := "SceneNodeGrpcImpl::DestroyScene(grpc::ServerContext"

	if !strings.Contains(codeMap[createSceneKey], "acquire_permit") {
		t.Fatalf("CreateScene pre-dispatch code not captured: %q", codeMap[createSceneKey])
	}
	for key, code := range codeMap {
		if strings.Contains(code, "handle_create_scene_impl") {
			t.Fatalf("Handle* section leaked into wrapper key %q: %q", key, code)
		}
	}
	if codeMap[createKey] != _config.Global.Naming.YourCodePair {
		t.Fatalf("Create wrapper without section should fall back to default pair, got %q", codeMap[createKey])
	}
	if codeMap[destroyKey] != _config.Global.Naming.YourCodePair {
		t.Fatalf("missing DestroyScene wrapper should fall back to default pair, got %q", codeMap[destroyKey])
	}
}

// 旧格式文件(包装函数里没有任何守护段)升级时:全部回落默认空段,Handle* 段不被误收。
func TestReadCodeSections_GrpcWrapperLegacyFileHasNoPreDispatchSections(t *testing.T) {
	setParserTestNaming(t)

	methods := buildParserTestMethods("SceneNodeGrpc", "CreateScene", "DestroyScene")
	cpp := strings.Join([]string{
		"#include \"scene_node_service.h\"",
		"///<<< BEGIN WRITING YOUR CODE",
		"///<<< END WRITING YOUR CODE",
		"",
		"void SceneNodeGrpcImpl::HandleCreateScene(const ::CreateSceneRequest* request,",
		"    ::CreateSceneResponse* response)",
		"{",
		"///<<< BEGIN WRITING YOUR CODE",
		"    handle_create_scene_impl();",
		"///<<< END WRITING YOUR CODE",
		"}",
		"",
		"grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* /*context*/,",
		"    const ::CreateSceneRequest* request,",
		"    ::CreateSceneResponse* response)",
		"{",
		"    std::promise<void> promise;",
		"    return grpc::Status::OK;",
		"}",
		"",
		"grpc::Status SceneNodeGrpcImpl::DestroyScene(grpc::ServerContext* /*context*/,",
		"    const ::DestroySceneRequest* request,",
		"    ::Empty* response)",
		"{",
		"    std::promise<void> promise;",
		"    return grpc::Status::OK;",
		"}",
		"",
	}, "\n")

	filePath := writeParserTempFile(t, cpp)
	codeMap, _, err := ReadCodeSectionsFromFile(filePath, &methods, legacyGrpcWrapperKey, "")
	if err != nil {
		t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
	}
	for _, key := range []string{
		"SceneNodeGrpcImpl::CreateScene(grpc::ServerContext",
		"SceneNodeGrpcImpl::DestroyScene(grpc::ServerContext",
	} {
		if codeMap[key] != _config.Global.Naming.YourCodePair {
			t.Fatalf("legacy wrapper %q should fall back to default pair, got %q", key, codeMap[key])
		}
	}
}

// 无守护段的旧包装函数结束后,不能把后续 Handle* 的守护段归给它。
func TestReadCodeSections_GrpcWrapperWithoutSectionDoesNotCaptureNextHandler(t *testing.T) {
	setParserTestNaming(t)
	methods := buildParserTestMethods("SceneNodeGrpc", "CreateScene", "DestroyScene")
	cpp := strings.Join([]string{
		_config.Global.Naming.YourCodePair,
		"grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* context)",
		"{",
		"    loop_.runInLoop([] {",
		"        do_work();",
		"    });",
		"    return grpc::Status::OK;",
		"}",
		"void SceneNodeGrpcImpl::HandleDestroyScene(const ::DestroySceneRequest* request)",
		"{",
		_config.Global.Naming.YourCodeBegin,
		"    handle_destroy_scene_impl();",
		_config.Global.Naming.YourCodeEnd,
		"}",
	}, "\n")

	codeMap, _, err := ReadCodeSectionsFromFile(writeParserTempFile(t, cpp), &methods, legacyGrpcWrapperKey, "")
	if err != nil {
		t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
	}
	for _, method := range methods {
		key := legacyGrpcWrapperKey(method, "")
		if codeMap[key] != _config.Global.Naming.YourCodePair {
			t.Fatalf("wrapper %q without a section captured unrelated code: %q", key, codeMap[key])
		}
	}
}

// 回归(上一轮评审遗留):守护段内出现某方法的查找键字面量 —— 注释、带类名限定的调用、段内顶格的行 —— 不得把解析拉回那个方法。
// 修复前 currentMethod 为空时段内各行照样匹配查找键:grpc.<M>.guard 里提到 "SceneNodeGrpcImpl::HandleCreateScene(" 就会重新进入
// CreateScene,随后的结束标记把 codeMap 里已取到的 Handle 体覆盖成只剩结束标记,手写体被静默丢掉。
func TestReadCodeSections_LookupKeyInsideSectionDoesNotReenterMethod(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	methods := buildParserTestMethods("SceneNodeGrpc", "CreateScene", "DestroyScene")
	createKey := GenerateGrpcHandlerNameWrapper(methods[0], "")
	destroyKey := GenerateGrpcHandlerNameWrapper(methods[1], "")

	firstCode := begin + "\n// 入口见 " + destroyKey + "request)\n" + end
	createBody := "    " + begin + "\n    create_impl(*request, *response); // 与 " + destroyKey + "request) 同一套校验\n    " + end
	destroyBody := "    " + begin + "\n    destroy_impl(*request);\n    " + end
	cpp := strings.Join([]string{
		"#include \"scene_node_service.h\"",
		firstCode,
		"",
		"SceneNodeGrpcImpl::SceneNodeGrpcImpl(muduo::net::EventLoop& loop)",
		"    : loop_(loop)",
		"{",
		"}",
		"",
		"void " + createKey + "const ::CreateSceneRequest* request,",
		"    ::CreateSceneResponse* response)",
		"{",
		createBody,
		"}",
		"",
		"void " + destroyKey + "const ::DestroySceneRequest* request)",
		"{",
		destroyBody,
		"}",
		"",
		"void SceneNodeGrpcImpl::CreateScene(const std::shared_ptr<CreateSceneCall>& call)",
		"{",
		"    [[maybe_unused]] const ::CreateSceneRequest* request = &call->Request();",
		"    " + begin + ": grpc.CreateScene.guard",
		"    // 准入口径与 " + createKey + "request, response) 一致",
		"    " + destroyKey + "request);",
		createKey + "nullptr, nullptr); // 段内顶格的行同样不认",
		"    " + end,
		"    call->Proceed();",
		"}",
		"",
		"void SceneNodeGrpcImpl::DestroyScene(const std::shared_ptr<DestroySceneCall>& call)",
		"{",
		"    [[maybe_unused]] const ::DestroySceneRequest* request = &call->Request();",
		"    " + begin + ": grpc.DestroyScene.guard",
		"    " + destroyKey + "request);",
		"    " + end,
		"    call->Proceed();",
		"}",
		"",
		"void SceneNodeGrpcImpl::RegisterMethods(muduo_grpc::MethodTable& table)",
		"{",
		"}",
		"",
	}, "\n")

	codeMap, gotFirstCode, err := ReadCodeSectionsFromFile(writeParserTempFile(t, cpp), &methods, GenerateGrpcHandlerNameWrapper, "")
	if err != nil {
		t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
	}
	if gotFirstCode != firstCode+"\n" {
		t.Fatalf("FirstCode 应逐字取到: got=%q want=%q", gotFirstCode, firstCode+"\n")
	}
	for key, want := range map[string]string{createKey: createBody + "\n", destroyKey: destroyBody + "\n"} {
		if codeMap[key] != want {
			t.Fatalf("守护段里的查找键字面量把 %q 的手写体顶替了: got=%q want=%q", key, codeMap[key], want)
		}
	}
	if len(codeMap) != len(methods) {
		t.Fatalf("codeMap 只应有当前方法的键: %q", codeMap)
	}
}

// 查找键只在守护段之外的函数签名行(isCppSignatureLine:顶格、不是预处理指令或行注释)上生效。
// 表里每一行往典型 handler 文件里插一段带别的方法查找键的「噪声」;修复前每一种都会让某个方法的手写体被别的段顶替,
// 或被覆盖成只剩结束标记。before 插在 FirstCode 与第一个签名之间,after 插在两个方法段都取到之后。
func TestReadCodeSections_LookupKeyOnlyOnSignatureLinesOutsideSections(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene", "EnterScene")
	leaveKey := GenerateMethodHandlerNameWrapper(methods[0], "")
	enterKey := GenerateMethodHandlerNameWrapper(methods[1], "")
	firstCode := begin + "\n#include \"scene/system/scene_system.h\"\n" + end
	leaveBody := begin + "\n\tleave_impl();\n" + end
	enterBody := begin + "\n\tenter_impl();\n" + end

	for _, tc := range []struct {
		name   string
		before string
		after  string
	}{
		{name: "段外顶格的行注释", before: "// 转发到 " + enterKey + "int a)"},
		{name: "段外的预处理指令", before: "#define FORWARD_ENTER " + enterKey + "a)"},
		{name: "段外缩进的语句", before: "    " + enterKey + "0); // 手写在函数外"},
		{name: "方法段之后手工加的无名段", after: begin + "\n// 见 " + leaveKey + "int a)\n" + enterKey + "0);\n" + end},
		{name: "其他生成器的命名段", after: "    " + begin + ": other.section\n    " + leaveKey + "0);\n    " + end},
		{
			name:  "非方法函数体里缩进的调用与段",
			after: "void ScenePlayerSceneHandler::Helper()\n{\n    " + enterKey + "0);\n" + begin + "\n    helper_impl();\n" + end + "\n}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{"#include \"scene_player_scene_handler.h\"", firstCode, ""}
			if tc.before != "" {
				lines = append(lines, tc.before)
			}
			lines = append(lines,
				"void "+leaveKey+"int a)", "{", leaveBody, "}", "",
				"void "+enterKey+"int a)", "{", enterBody, "}", "")
			if tc.after != "" {
				lines = append(lines, tc.after, "")
			}
			codeMap, gotFirstCode, err := ReadCodeSectionsFromFile(writeParserTempFile(t, strings.Join(lines, "\n")),
				&methods, GenerateMethodHandlerNameWrapper, "")
			if err != nil {
				t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
			}
			if gotFirstCode != firstCode+"\n" {
				t.Fatalf("FirstCode 应逐字取到: got=%q", gotFirstCode)
			}
			for key, want := range map[string]string{leaveKey: leaveBody + "\n", enterKey: enterBody + "\n"} {
				if codeMap[key] != want {
					t.Fatalf("%q 的手写体被噪声顶替: got=%q want=%q", key, codeMap[key], want)
				}
			}
			if len(codeMap) != len(methods) {
				t.Fatalf("codeMap 只应有当前方法的键: %q", codeMap)
			}
		})
	}
}

// 结构损坏的文件:方法之间留下一个没有结束标记的孤立开始标记(手工编辑误删了结束标记)。handler / 玩家 handler 生成器取段前
// 没有结构预检;孤立标记不得一路跳到下一个方法段的结束标记、把其间方法的签名连同守护段吞掉 —— 修复前段外照常认查找键,
// 这种文件不丢代码,跳过段若不回放就成了新的静默丢弃。下一个整行开始标记出现时撤销跳过、按段外规则回放;
// 回放后紧跟的完整手工段照常整段跳过,段内的查找键仍不把解析拉回已取到代码的方法。
func TestReadCodeSections_StrayBeginMarkerDoesNotSwallowNextMethod(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene", "EnterScene", "ChangeScene")
	keys := make([]string, 0, len(methods))
	for _, method := range methods {
		keys = append(keys, GenerateMethodHandlerNameWrapper(method, ""))
	}
	firstCode := begin + "\n#include \"scene/system/scene_system.h\"\n" + end
	bodies := []string{
		begin + "\n\tleave_impl();\n" + end,
		begin + "\n\tenter_impl();\n" + end,
		begin + "\n\tchange_impl();\n" + end,
	}
	methodLines := func(i int) []string {
		return []string{"void " + keys[i] + "int a)", "{", bodies[i], "}", ""}
	}

	for _, tc := range []struct {
		name  string
		stray []string // 插在第一个方法之后
	}{
		{name: "孤立的无名开始标记", stray: []string{begin, "// 结束标记被误删的手工段", ""}},
		{name: "孤立的命名开始标记", stray: []string{"    " + begin + ": other.section", "    // 结束标记被误删", ""}},
		{
			name:  "孤立标记后紧跟完整的手工段",
			stray: []string{begin, "", begin, keys[0] + "0); // 段内顶格的行", end, ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{"#include \"scene_player_scene_handler.h\"", firstCode, ""}
			lines = append(lines, methodLines(0)...)
			lines = append(lines, tc.stray...)
			lines = append(lines, methodLines(1)...)
			lines = append(lines, methodLines(2)...)
			codeMap, gotFirstCode, err := ReadCodeSectionsFromFile(writeParserTempFile(t, strings.Join(lines, "\n")),
				&methods, GenerateMethodHandlerNameWrapper, "")
			if err != nil {
				t.Fatalf("ReadCodeSectionsFromFile failed: %v", err)
			}
			if gotFirstCode != firstCode+"\n" {
				t.Fatalf("FirstCode 应逐字取到: got=%q", gotFirstCode)
			}
			for i, key := range keys {
				if codeMap[key] != bodies[i]+"\n" {
					t.Fatalf("孤立开始标记吞掉了 %q 的手写体: got=%q want=%q", key, codeMap[key], bodies[i]+"\n")
				}
			}
			if len(codeMap) != len(methods) {
				t.Fatalf("codeMap 只应有当前方法的键: %q", codeMap)
			}
		})
	}
}

// 函数边界与 grpc 扫描器同一套签名累积规则(§15.17):以 "{" 结尾才进入函数体、函数体内不认查找键,以 ";" 结尾的声明放弃,
// 新起始行取代未完成的签名。表里每一种在修复前都会把某个方法的段劫持给别的方法(或把 FirstCode 吞掉),最后一种只锁定规则。
func TestReadCodeSections_FunctionBodiesAndDeclarationsDoNotHijack(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene", "EnterScene")
	leaveKey := GenerateMethodHandlerNameWrapper(methods[0], "")
	enterKey := GenerateMethodHandlerNameWrapper(methods[1], "")
	firstCode := begin + "\n#include \"scene/system/scene_system.h\"\n" + end
	leaveBody := begin + "\n\tleave_impl();\n" + end
	enterBody := begin + "\n\tenter_impl();\n" + end
	leaveLines := []string{"void " + leaveKey + "int a)", "{", leaveBody, "}", ""}
	enterLines := []string{"void " + enterKey + "int a)", "{", enterBody, "}", ""}
	join := func(parts ...[]string) string {
		var lines []string
		for _, part := range parts {
			lines = append(lines, part...)
		}
		return strings.Join(lines, "\n")
	}
	header := []string{"#include \"scene_player_scene_handler.h\""}

	for _, tc := range []struct {
		name string
		cpp  string
		// wantProblems:Problems 应恰好含这些子串(各一条);为空表示没有问题。
		wantProblems []string
	}{
		{
			// 修复前:LeaveScene 的段闭合后,函数体里顶格的折行转调被当成 EnterScene 的签名,其后手工补的第二段顶替了
			// 先定义的 EnterScene 的手写体。现在函数体内不认查找键,第二段整段跳过并记进 Problems。
			name: "函数体内顶格的查找键调用",
			cpp: join(header, []string{firstCode, ""}, enterLines,
				[]string{"void " + leaveKey + "int a)", "{", leaveBody, enterKey + "0,", "    1);",
					begin, "\t// 手工补在方法段之后的第二段", end, "}", ""}),
			wantProblems: []string{"ScenePlayerSceneHandler::LeaveScene 体里第一个守护段之后的非空守护段没有去处"},
		},
		{
			// 修复前:前置声明命中 EnterScene,其后的 FirstCode 被当成 EnterScene 的段,FirstCode 退回默认空段。
			name: "FirstCode 之前以 ; 结尾的前置声明",
			cpp:  join(header, []string{"void " + enterKey + "int a);", firstCode, ""}, leaveLines, enterLines),
		},
		{
			// 修复前:前置声明命中 EnterScene 后,LeaveScene 的签名不再重新匹配,LeaveScene 的段被归给 EnterScene,LeaveScene 退回默认空段。
			name: "方法之间以 ; 结尾的前置声明",
			cpp:  join(header, []string{firstCode, "", "void " + enterKey + "int a);", ""}, leaveLines, enterLines),
		},
		{
			name: "折行的前置声明",
			cpp:  join(header, []string{firstCode, "", "void " + enterKey, "    int a);", ""}, leaveLines, enterLines),
		},
		{
			name: "新起始行取代未完成的签名",
			cpp:  join(header, []string{firstCode, "", "void ScenePlayerSceneHandler::Helper(int a,"}, leaveLines, enterLines),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sections, err := ReadCodeSectionsByKeys(writeParserTempFile(t, tc.cpp), []string{leaveKey, enterKey})
			if err != nil {
				t.Fatalf("ReadCodeSectionsByKeys failed: %v", err)
			}
			if sections.FirstCode != firstCode+"\n" {
				t.Fatalf("FirstCode 应逐字取到: got=%q", sections.FirstCode)
			}
			for key, want := range map[string]string{leaveKey: leaveBody + "\n", enterKey: enterBody + "\n"} {
				if sections.CodeMap[key] != want {
					t.Fatalf("%q 的手写体被劫持: got=%q want=%q", key, sections.CodeMap[key], want)
				}
			}
			if len(sections.CodeMap) != len(methods) {
				t.Fatalf("codeMap 只应有当前方法的键: %q", sections.CodeMap)
			}
			assertCodeSectionProblems(t, sections.Problems, tc.wantProblems)
		})
	}
}

// Problems 列出结构损坏与读到却带不走的非空段(handler / 玩家 handler / 事件 handler 生成器据此拒绝生成),带开始标记所在行号;
// 空段与结构完好的文件不报。修复前这些段在重生成时被静默丢弃,结构损坏的文件也照常覆盖。
func TestReadCodeSectionsByKeys_ProblemsListBrokenAndLostSections(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	pair := _config.Global.Naming.YourCodePair
	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene", "EnterScene")
	leaveKey := GenerateMethodHandlerNameWrapper(methods[0], "")
	enterKey := GenerateMethodHandlerNameWrapper(methods[1], "")
	firstCode := begin + "\n#include \"scene/system/scene_system.h\"\n" + end
	method := func(key, body string, extra ...string) string {
		lines := append([]string{"void " + key + "int a)", "{", body}, extra...)
		return strings.Join(append(lines, "}", ""), "\n")
	}
	leave := method(leaveKey, begin+"\n\tleave_impl();\n"+end)
	enter := method(enterKey, begin+"\n\tenter_impl();\n"+end)
	file := func(parts ...string) string {
		return strings.Join(append([]string{"#include \"scene_player_scene_handler.h\"", firstCode, ""}, parts...), "\n")
	}

	for _, tc := range []struct {
		name string
		cpp  string
		// anchor:定位行号的唯一文本,期望的行号是它所在行减 lineOffset(通常指向开始标记)。为空则不校验行号。
		anchor     string
		lineOffset int
		want       []string
		kind       CodeSectionProblemKind // 第一条问题的类别(want 非空时校验)
		// sectionName:第一条问题的 SectionName(want 非空时校验;无名段与非跳过段为空)
		sectionName string
	}{
		{
			name: "结构完好:空的多余段、空的手写函数段与函数外空段都不报",
			cpp: file(method(leaveKey, begin+"\n\tleave_impl();\n"+end, pair), pair, "",
				method("ScenePlayerSceneHandler::Helper(", pair), enter),
		},
		{
			name:   "方法体里第一个段之后的非空段",
			cpp:    file(method(leaveKey, begin+"\n\tleave_impl();\n"+end, begin, "\textra_impl();", end), enter),
			anchor: "extra_impl", lineOffset: 1,
			want: []string{"ScenePlayerSceneHandler::LeaveScene 体里第一个守护段之后的非空守护段没有去处"},
			kind: CodeSectionUnplaced,
		},
		{
			name:   "签名不对应当前方法的函数里的非空段(proto 已删除的方法)",
			cpp:    file(leave, method("ScenePlayerSceneHandler::ChangeScene(", begin+"\n\tchange_impl();\n"+end), enter),
			anchor: "change_impl", lineOffset: 1,
			want: []string{"ScenePlayerSceneHandler::ChangeScene 的签名不对应当前任何方法"},
			kind: CodeSectionUnplaced,
		},
		{
			name:   "FirstCode 之后函数外的非空段",
			cpp:    file(leave, begin, "// 手工插在函数之间", end, "", enter),
			anchor: "手工插在函数之间", lineOffset: 1,
			want: []string{"函数外的非空守护段"},
			kind: CodeSectionUnplaced,
		},
		{
			name:   "最后一个方法段没有结束标记",
			cpp:    file(leave, "void "+enterKey+"int a)", "{", begin, "\tenter_impl();", "}"),
			anchor: "enter_impl", lineOffset: 1,
			want: []string{"守护段尚未闭合"},
			kind: CodeSectionBroken,
		},
		{
			name: "方法段缺结束标记、吞进下一个方法",
			cpp:  file("void "+leaveKey+"int a)", "{", begin, "\tleave_impl();", "}", "", enter),
			want: []string{"守护段内不能嵌套守护段"},
			kind: CodeSectionBroken,
		},
		{
			name:   "孤立的结束标记",
			cpp:    file(leave, "// 孤立结束标记在下一行", end, enter),
			anchor: "孤立结束标记在下一行", lineOffset: -1,
			want: []string{"守护段结束标记没有对应开始标记"},
			kind: CodeSectionBroken,
		},
		{
			// 段内某行夹带结束标记文本:按行内含结束标记提前闭合,段的后半截落到段外,真正的结束标记成了孤立标记。
			name: "段内某行夹带结束标记文本",
			cpp:  file(method(leaveKey, begin+"\n\t// 段尾用 "+end+" 收口\n\tleave_impl();\n"+end), enter),
			want: []string{"守护段结束标记没有对应开始标记"},
			kind: CodeSectionBroken,
		},
		{
			name: "孤立的开始标记(跳过段回放)",
			cpp:  file(leave, begin, "// 结束标记被误删", "", enter),
			want: []string{"守护段内不能嵌套守护段"},
			kind: CodeSectionBroken,
		},
		{
			name: "同一方法定义两次",
			cpp:  file(leave, method(leaveKey, begin+"\n\tleave_again();\n"+end), enter),
			want: []string{leaveKey + " 再次取到守护段"},
			kind: CodeSectionOverwritten,
		},
		{
			// 跳过的命名段带上名字(gRPC 生成器据此只放过当前方法的 grpc.<M>.guard)。
			name:   "跳过的命名段",
			cpp:    file(leave, method("ScenePlayerSceneHandler::Helper(", "    "+begin+": other.section\n    helper_impl();\n    "+end), enter),
			anchor: "helper_impl", lineOffset: 1,
			want:        []string{"其中的非空守护段(other.section)没有去处"},
			kind:        CodeSectionUnplaced,
			sectionName: "other.section",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sections, err := ReadCodeSectionsByKeys(writeParserTempFile(t, tc.cpp), []string{leaveKey, enterKey})
			if err != nil {
				t.Fatalf("ReadCodeSectionsByKeys failed: %v", err)
			}
			assertCodeSectionProblems(t, sections.Problems, tc.want)
			if len(tc.want) > 0 && (sections.Problems[0].Kind != tc.kind || sections.Problems[0].SectionName != tc.sectionName) {
				t.Fatalf("问题类别或段名不对: got=(%d,%q) want=(%d,%q) (%s)", sections.Problems[0].Kind,
					sections.Problems[0].SectionName, tc.kind, tc.sectionName, sections.Problems[0].Message)
			}
			if tc.anchor != "" {
				line := lineOfParserFixture(t, tc.cpp, tc.anchor) - tc.lineOffset
				if want := fmt.Sprintf("第 %d 行：", line); !strings.HasPrefix(sections.Problems[0].Message, want) ||
					sections.Problems[0].Line != line {
					t.Fatalf("问题应定位到 %q: got=%v", want, sections.Problems[0])
				}
			}
			refusal := RefuseLostCodeSections("handler.cpp", sections)
			if (refusal != nil) != (len(tc.want) > 0) {
				t.Fatalf("有问题才拒绝生成: problems=%v err=%v", sections.Problems, refusal)
			}
		})
	}
}

// 缺 FirstCode 的文件里,第一个段落在签名不对应当前方法的函数体内(已删方法 / 手写辅助函数):宽松解析照旧把它当 FirstCode
// (尽力而为的结果不变),但重生成会把它搬到文件开头、函数本身随之消失 —— 非空时记进 Problems(生成器据此拒绝),空段不报。
// 修复前不记任何问题,代码被悄悄挪了位置。
func TestReadCodeSectionsByKeys_FirstSectionInsideUnrecognizedFunctionIsReported(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	pair := _config.Global.Naming.YourCodePair
	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene")
	leaveKey := GenerateMethodHandlerNameWrapper(methods[0], "")
	leaveBody := begin + "\n\tleave_impl();\n" + end

	for _, tc := range []struct {
		name        string
		helperBody  string
		wantProblem bool
	}{
		{name: "非空段", helperBody: begin + "\n\thelper_impl();\n" + end, wantProblem: true},
		{name: "空段", helperBody: pair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpp := strings.Join([]string{
				"#include \"scene_player_scene_handler.h\"",
				"",
				"void ScenePlayerSceneHandler::Helper(int a)",
				"{",
				tc.helperBody, // 第 5 行起
				"}",
				"",
				"void " + leaveKey + "int a)",
				"{",
				leaveBody,
				"}",
				"",
			}, "\n")
			sections, err := ReadCodeSectionsByKeys(writeParserTempFile(t, cpp), []string{leaveKey})
			if err != nil {
				t.Fatalf("ReadCodeSectionsByKeys failed: %v", err)
			}
			if sections.FirstCode != tc.helperBody+"\n" || sections.CodeMap[leaveKey] != leaveBody+"\n" {
				t.Fatalf("宽松解析的结果应不变: FirstCode=%q leave=%q", sections.FirstCode, sections.CodeMap[leaveKey])
			}
			if !tc.wantProblem {
				assertCodeSectionProblems(t, sections.Problems, nil)
				return
			}
			assertCodeSectionProblems(t, sections.Problems,
				[]string{"ScenePlayerSceneHandler::Helper 里的守护段是文件的第一个段,会被当成 FirstCode 搬到文件开头"})
			if problem := sections.Problems[0]; problem.Kind != CodeSectionUnplaced || problem.Line != 5 {
				t.Fatalf("应为第 5 行的 CodeSectionUnplaced: got=%v", problem)
			}
		})
	}
}

// 函数体在顶格的 "}" 或 "};" 处结束,行尾空白不计(与 scanGrpcCppSections 同口径,见 TestScanGrpcCppSections_FunctionBodyEndsOnTopLevelBraceVariants);
// 缩进的 "}" 是嵌套块的结束,不结束函数体。修复前只认整行恰为 "}":"} " 或 "};" 之后的签名都落在「函数体内」不被认出,
// 其后方法的段被整段跳过、退回默认空段(生成器随之拒绝)。
func TestReadCodeSections_FunctionBodyEndsOnTopLevelBraceVariants(t *testing.T) {
	setParserTestNaming(t)
	begin := _config.Global.Naming.YourCodeBegin
	end := _config.Global.Naming.YourCodeEnd
	methods := buildParserTestMethods("ScenePlayerScene", "LeaveScene", "EnterScene", "ChangeScene")
	keys := make([]string, 0, len(methods))
	bodies := make([]string, 0, len(methods))
	for _, method := range methods {
		keys = append(keys, GenerateMethodHandlerNameWrapper(method, ""))
		bodies = append(bodies, begin+"\n\t"+strings.ToLower(method.Method())+"_impl();\n"+end)
	}
	firstCode := begin + "\n#include \"scene/system/scene_system.h\"\n" + end
	cpp := strings.Join([]string{
		"#include \"scene_player_scene_handler.h\"",
		firstCode,
		"",
		"void " + keys[0] + "int a)",
		"{",
		"    if (a > 0)",
		"    {", // 段之前的嵌套块:缩进的 "}" 不结束函数体,LeaveScene 照旧等到它的段
		"    }",
		bodies[0],
		"} ", // 行尾空白
		"void " + keys[1] + "int a)",
		"{",
		bodies[1],
		"};",
		"void " + keys[2] + "int a)",
		"{",
		bodies[2],
		"}\t",
		"",
	}, "\n")
	sections, err := ReadCodeSectionsByKeys(writeParserTempFile(t, cpp), keys)
	if err != nil {
		t.Fatalf("ReadCodeSectionsByKeys failed: %v", err)
	}
	if sections.FirstCode != firstCode+"\n" {
		t.Fatalf("FirstCode 应逐字取到: got=%q", sections.FirstCode)
	}
	for i, key := range keys {
		if sections.CodeMap[key] != bodies[i]+"\n" {
			t.Fatalf("%q 的段没有取到: got=%q want=%q", key, sections.CodeMap[key], bodies[i]+"\n")
		}
	}
	assertCodeSectionProblems(t, sections.Problems, nil)
}

// handler / 玩家 handler 生成器在取段前预检(与 gRPC 同口径):proto 删掉一个方法后,旧文件里该方法的非空手写体在新生成里没有位置,
// .cpp 生成拒绝(panic,调用方不写文件),同名 .h 的生成同样拒绝(不留下「新 .h 配旧 .cpp」);删掉的方法体为空时照常生成。
func TestHandlerGenerators_RefuseToDropSectionsOfRemovedMethods(t *testing.T) {
	setParserTestNaming(t)
	previousExtensions := _config.Global.FileExtensions
	t.Cleanup(func() { _config.Global.FileExtensions = previousExtensions })
	_config.Global.FileExtensions.Proto = ".proto"
	_config.Global.FileExtensions.HandlerH = "_handler.h"
	_config.Global.FileExtensions.HandlerCpp = "_handler.cpp"
	_config.Global.Naming.IncludeBegin = "#include \""
	_config.Global.Naming.IncludeEndLine = "\"\n"
	_config.Global.Naming.GoogleMethodController = "::google::protobuf::RpcController* controller, "
	_config.Global.Naming.PlayerMethodController = "entt::entity player,"

	const className = "ScenePlayerHandler"
	for _, tc := range []struct {
		name         string
		generateCpp  func(dst string, methods RPCMethods) string
		generateHead func(dst string, methods RPCMethods) (string, error)
		plainHead    func(methods RPCMethods) (string, error)
		key          func(info *MethodInfo) string
	}{
		{
			name:         "handler_gen(节点 RPC handler)",
			generateCpp:  func(dst string, methods RPCMethods) string { return GetServiceHandlerCppStr(dst, methods, "", "") },
			generateHead: GetServiceHandlerHeadFromFileStr,
			plainHead:    GetServiceHandlerHeadStr,
			key:          func(info *MethodInfo) string { return GenerateMethodHandlerNameWrapper(info, "") },
		},
		{
			name: "player_service_gen(玩家 handler)",
			generateCpp: func(dst string, methods RPCMethods) string {
				return GetPlayerServiceHandlerCppStr(dst, methods, className, "#include \"player_lifecycle_handler.h\"")
			},
			generateHead: GetPlayerServiceHeadFromFileStr,
			plainHead:    GetPlayerServiceHeadStr,
			key:          func(info *MethodInfo) string { return GenerateMethodHandlerNameWithClassPrefixWrapper(info, className) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := buildParserTestMethods("ScenePlayer", "GateLoginNotify", "ExitGame", "EnterScene")
			for _, method := range all {
				method.MethodDescriptorProto.InputType = proto.String("." + method.Method() + "Request")
				method.MethodDescriptorProto.OutputType = proto.String(".google.protobuf.Empty")
			}
			remaining := all[:2] // proto 删掉了 EnterScene
			dir := t.TempDir()
			dst := filepath.Join(dir, "player_lifecycle_handler.cpp")
			headerPath := filepath.Join(dir, "player_lifecycle_handler.h")

			// 删掉的方法体为空:照常生成,.h 与不读文件的版本逐字一致。
			writeGrpcPreservationFile(t, dst, tc.generateCpp(dst, all))
			if generated := tc.generateCpp(dst, remaining); strings.Contains(generated, tc.key(all[2])) {
				t.Fatalf("删掉的方法不应再生成:\n%s", generated)
			}
			wantHead, err := tc.plainHead(remaining)
			if err != nil {
				t.Fatalf("生成头文件失败: %v", err)
			}
			if head, err := tc.generateHead(headerPath, remaining); err != nil || head != wantHead {
				t.Fatalf("预检通过时头文件应与不读文件的版本一致: err=%v\n%s", err, head)
			}

			// 删掉的方法体非空:.cpp 与 .h 都拒绝,报错点名文件与函数。
			code := fillGrpcPreservationSection(t, tc.generateCpp(dst, all), "void "+tc.key(all[2]), "", "\tenter_scene_impl();")
			writeGrpcPreservationFile(t, dst, code)
			message := recoverGrpcGenerationPanic(t, func() { tc.generateCpp(dst, remaining) })
			for _, want := range []string{dst, className + "::EnterScene 的签名不对应当前任何方法", "拒绝生成"} {
				if !strings.Contains(message, want) {
					t.Fatalf(".cpp 拒绝信息缺少 %q:\n%s", want, message)
				}
			}
			if _, err := tc.generateHead(headerPath, remaining); err == nil || !strings.Contains(err.Error(), dst) {
				t.Fatalf(".h 生成必须先预检同名 .cpp 并拒绝: err=%v", err)
			}
			assertGrpcFileContent(t, dst, code)

			// proto 里方法还在时同一份文件照常重生成,手写体逐字保留。
			assertGrpcPreservedOnce(t, tc.generateCpp(dst, all), "\tenter_scene_impl();", "自定义代码")
		})
	}
}

// 修复只收紧「守护段内与段外非签名行不认查找键」,两个非 gRPC 生成器的典型文件解析结果不变:
// FirstCode + 每个方法一个段,段内提到别的方法的查找键;重生成后逐字保留,连续重生成不漂移。
func TestReadCodeSections_HandlerGeneratorsRegenerateUnchanged(t *testing.T) {
	setParserTestNaming(t)
	previousExtensions := _config.Global.FileExtensions
	t.Cleanup(func() { _config.Global.FileExtensions = previousExtensions })
	_config.Global.FileExtensions.Proto = ".proto"
	_config.Global.FileExtensions.HandlerH = "_handler.h"
	_config.Global.Naming.IncludeBegin = "#include \""
	_config.Global.Naming.IncludeEndLine = "\"\n"
	_config.Global.Naming.GoogleMethodController = "::google::protobuf::RpcController* controller, "
	_config.Global.Naming.PlayerMethodController = "entt::entity player,"

	const className = "ScenePlayerHandler"
	for _, tc := range []struct {
		name     string
		generate func(dst string, methods RPCMethods) string
		key      func(info *MethodInfo) string
	}{
		{
			name:     "handler_gen(节点 RPC handler)",
			generate: func(dst string, methods RPCMethods) string { return GetServiceHandlerCppStr(dst, methods, "", "") },
			key:      func(info *MethodInfo) string { return GenerateMethodHandlerNameWrapper(info, "") },
		},
		{
			name: "player_service_gen(玩家 handler)",
			generate: func(dst string, methods RPCMethods) string {
				return GetPlayerServiceHandlerCppStr(dst, methods, className, "#include \"player_lifecycle_handler.h\"")
			},
			key: func(info *MethodInfo) string { return GenerateMethodHandlerNameWithClassPrefixWrapper(info, className) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			methods := buildParserTestMethods("ScenePlayer", "GateLoginNotify", "ExitGame", "EnterScene")
			for _, method := range methods {
				method.MethodDescriptorProto.InputType = proto.String("." + method.Method() + "Request")
				method.MethodDescriptorProto.OutputType = proto.String(".google.protobuf.Empty")
			}
			dst := filepath.Join(t.TempDir(), "player_lifecycle_handler.cpp")
			code := tc.generate(dst, methods)
			customCode := []string{"#include \"player/system/player_lifecycle.h\"\n// 入口见 " + tc.key(methods[2]) + "player, request, response)"}
			code = fillGrpcPreservationSection(t, code, "", "", customCode[0])
			for i, method := range methods {
				// 每个方法段都提到下一个方法的查找键(带类名限定的调用与注释);不能写成 "void <键>",否则会与签名行混淆。
				next := tc.key(methods[(i+1)%len(methods)])
				body := "\t// 口径同 " + next + "player, request, response)\n\t" + next + "player, nullptr, nullptr);\n\t" + strings.ToLower(method.Method()) + "_impl();"
				code = fillGrpcPreservationSection(t, code, "void "+tc.key(method), "", body)
				customCode = append(customCode, body)
			}
			writeGrpcPreservationFile(t, dst, code)

			var previous string
			for pass := 0; pass < 3; pass++ {
				generated := tc.generate(dst, methods)
				for _, body := range customCode {
					assertGrpcPreservedOnce(t, generated, body, "自定义代码")
				}
				for _, method := range methods {
					assertGrpcPreservedOnce(t, generated, "void "+tc.key(method), "方法签名")
				}
				if pass > 0 && generated != previous {
					t.Fatalf("第 %d 次重生成发生字节漂移", pass+1)
				}
				previous = generated
				writeGrpcPreservationFile(t, dst, generated)
			}
		})
	}
}

// 工作区里全部由 codeParser 读取的 handler cpp(只读,不写回):方法表按文件里守护段外的签名行反推,期望值逐行独立求出 ——
// 第一个段是 FirstCode,每个签名之后的第一个段归该方法,其余段(grpc.<M>.guard 等)不归任何方法。
// 「守护段内与段外非签名行不认查找键」与「函数边界按签名累积规则跟踪」(§15.17)的修复对既有生成物必须逐字不变;
// 非 gRPC 的 handler 与事件 handler 生成器取段前的预检(readCheckedCodeSections)对它们也不得误报,结果与宽松入口逐字相同。
// 事件 handler(event.go 经 ReadCodeSectionsByKeys 取段)的查找键是整条签名,这里用 "<类>::<名>(" 作键:两者在签名起始行上同样命中。
func TestReadCodeSections_WorkspaceHandlersParseUnchanged(t *testing.T) {
	root := grpcWorkspaceRoot(t)
	setParserTestNaming(t)
	// 第 1 组是查找键里 "::" 之前的类名(gRPC 为服务名,类名再加 Impl),第 2 组是方法名。
	plainSignature := regexp.MustCompile(`^void (\w+)::(\w+)\(`)
	grpcSignature := regexp.MustCompile(`^void (\w+)Impl::Handle(\w+)\(`)
	for _, tc := range []struct {
		glob string
		grpc bool
	}{
		{"cpp/nodes/scene/handler/rpc/*.cpp", false},
		{"cpp/nodes/scene/handler/rpc/player/*.cpp", false},
		{"cpp/nodes/gate/handler/rpc/*.cpp", false},
		{"cpp/nodes/scene/handler/event/*_event_handler.cpp", false},
		{"cpp/nodes/gate/handler/event/*_event_handler.cpp", false},
		{"cpp/nodes/scene/handler/grpc/*.cpp", true},
		{"cpp/nodes/battle/handler/grpc/*.cpp", true},
	} {
		paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(tc.glob)))
		if err != nil {
			t.Fatalf("匹配 %s 失败: %v", tc.glob, err)
		}
		checked := 0
		for _, path := range paths {
			source := readGrpcFileLF(t, path)
			if !strings.Contains(source, _config.Global.Naming.YourCodeBegin) {
				continue // 手写文件,不经生成器
			}
			checked++
			signature := plainSignature
			if tc.grpc {
				signature = grpcSignature
			}
			wantFirstCode, wantSections, owner, names := expectedWorkspaceCodeSections(t, path, source, signature)
			methods := buildParserTestMethods(owner, names...)
			keyFunc, funcParam := GenerateMethodHandlerNameWithClassPrefixWrapper, owner
			if tc.grpc {
				keyFunc, funcParam = GenerateGrpcHandlerNameWrapper, ""
			}
			codeMap, firstCode, err := ReadCodeSectionsFromFile(path, &methods, keyFunc, funcParam)
			if err != nil {
				t.Fatalf("%s 解析失败: %v", path, err)
			}
			if firstCode != wantFirstCode {
				t.Fatalf("%s 的 FirstCode 变了:\n got=%q\nwant=%q", path, firstCode, wantFirstCode)
			}
			for _, method := range methods {
				key := keyFunc(method, funcParam)
				if codeMap[key] != wantSections[method.Method()] {
					t.Fatalf("%s 的 %s 解析结果变了:\n got=%q\nwant=%q", path, method.Method(), codeMap[key], wantSections[method.Method()])
				}
			}
			if tc.grpc {
				// gRPC 生成器的预检(validateGrpcHandlerCppSections)在 codeParser 的 Problems 里只放过 grpc.<M>.guard:既有生成物同样不得误报。
				if _, err := validateGrpcHandlerCppSections(path, methods); err != nil {
					t.Fatalf("%s 是既有生成物,gRPC 预检不得误报: %v", path, err)
				}
				continue
			}
			checkedMap, checkedFirstCode, err := readCheckedCodeSections(path, &methods, keyFunc, funcParam)
			if err != nil {
				t.Fatalf("%s 是既有生成物,取段预检不得误报: %v", path, err)
			}
			if checkedFirstCode != firstCode || len(checkedMap) != len(codeMap) {
				t.Fatalf("%s 的预检入口与宽松入口结果不一致", path)
			}
			for key, code := range codeMap {
				if checkedMap[key] != code {
					t.Fatalf("%s 的预检入口与宽松入口在 %q 上不一致", path, key)
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s 下没有找到生成的 handler 文件,夹具路径可能已变", tc.glob)
		}
	}
}

// expectedWorkspaceCodeSections 逐行切出 source(LF 换行)里 codeParser 应得的结果,不复用 codeParser 的状态机:
// 开始标记整行识别、行内含结束标记即闭合(与 codeParser 同口径;scene_admin_handler.cpp 里有一行旧模板留下的
// "///<<< END WRITING YOUR CODE}");守护段外命中 signature 的行是一个方法的签名,其后第一个段归它;
// 文件里第一个段是 FirstCode,且必须先于第一个签名。
// 返回的段文本与 codeParser 同格式(含首尾标记,每行带 "\n");owner 是签名里 "::" 前的类名(全文件唯一),names 按出现顺序。
func expectedWorkspaceCodeSections(t *testing.T, path, source string, signature *regexp.Regexp) (string, map[string]string, string, []string) {
	t.Helper()
	end := _config.Global.Naming.YourCodeEnd
	var firstCode, owner, pending string
	var names []string
	sections := make(map[string]string)
	inSection, sectionCount := false, 0
	target := ""
	var code strings.Builder
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inSection:
			code.WriteString(line + "\n")
			if strings.Contains(line, end) {
				if target == "" && sectionCount == 1 {
					firstCode = code.String()
				} else if target != "" {
					sections[target] = code.String()
				}
				inSection = false
				code.Reset()
			}
		case isCodeSectionBeginMarker(trimmed):
			inSection = true
			sectionCount++
			target, pending = pending, ""
			if sectionCount == 1 && target != "" {
				t.Fatalf("%s 的第一个段在签名之后,不是 FirstCode,夹具假设不成立", path)
			}
			code.WriteString(line + "\n")
		case line == "}":
			pending = ""
		default:
			match := signature.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if owner != "" && match[1] != owner {
				t.Fatalf("%s 里有两个类名 %q / %q,夹具假设不成立", path, owner, match[1])
			}
			owner, pending = match[1], match[2]
			names = append(names, match[2])
		}
	}
	if len(names) == 0 || firstCode == "" {
		t.Fatalf("%s 里没有找到签名或 FirstCode,夹具假设不成立", path)
	}
	for _, name := range names {
		if _, exists := sections[name]; !exists {
			t.Fatalf("%s 的 %s 签名之后没有守护段,夹具假设不成立", path, name)
		}
	}
	return firstCode, sections, owner, names
}

// assertCodeSectionProblems:problems 恰好 len(want) 条,第 i 条的 Message 含 want[i]。
func assertCodeSectionProblems(t *testing.T, problems []CodeSectionProblem, want []string) {
	t.Helper()
	if len(problems) != len(want) {
		t.Fatalf("Problems 应恰好 %d 条: got=%v", len(want), problems)
	}
	for i, substring := range want {
		if !strings.Contains(problems[i].Message, substring) {
			t.Fatalf("第 %d 条问题应含 %q: got=%q", i+1, substring, problems[i].Message)
		}
	}
}

// lineOfParserFixture 返回 anchor 在 content 里第一次出现的行号(从 1 计)。
func lineOfParserFixture(t *testing.T, content, anchor string) int {
	t.Helper()
	at := strings.Index(content, anchor)
	if at < 0 {
		t.Fatalf("夹具里没有 %q", anchor)
	}
	return strings.Count(content[:at], "\n") + 1
}

func setParserTestNaming(t *testing.T) {
	t.Helper()
	oldNaming := _config.Global.Naming
	_config.Global.Naming.YourCodeBegin = "///<<< BEGIN WRITING YOUR CODE"
	_config.Global.Naming.YourCodeEnd = "///<<< END WRITING YOUR CODE"
	_config.Global.Naming.YourCodePair = _config.Global.Naming.YourCodeBegin + "\n" + _config.Global.Naming.YourCodeEnd
	_config.Global.Naming.HandlerFile = "Handler"
	t.Cleanup(func() {
		_config.Global.Naming = oldNaming
	})
}

func buildParserTestMethods(serviceName string, methodNames ...string) RPCMethods {
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("proto/scene/player_scene.proto")}
	svc := &descriptorpb.ServiceDescriptorProto{Name: proto.String(serviceName)}

	methods := make(RPCMethods, 0, len(methodNames))
	for _, m := range methodNames {
		methods = append(methods, &MethodInfo{
			ProtoFileInfo: ProtoFileInfo{
				Fd:                     fd,
				ServiceDescriptorProto: svc,
			},
			MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: proto.String(m)},
		})
	}

	return methods
}

func writeParserTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "handler.cpp")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file failed: %v", err)
	}
	return filePath
}

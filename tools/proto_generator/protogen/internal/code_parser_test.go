package internal

import (
	"os"
	"path/filepath"
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

// gRPC 包装函数的"投递前"守护段:第二遍解析只取包装函数里的段,不串到 Handle* 的段,
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
	codeMap, _, err := ReadCodeSectionsFromFile(filePath, &methods, GenerateGrpcWrapperNameWrapper, "")
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
	codeMap, _, err := ReadCodeSectionsFromFile(filePath, &methods, GenerateGrpcWrapperNameWrapper, "")
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

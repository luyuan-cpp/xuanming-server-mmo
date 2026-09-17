package internal

import (
	"os"
	"strings"
	"testing"

	_config "protogen/internal/config"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 回归:commit 6c4021ae5 的重生成吞掉了写在 gRPC 包装函数里、守护段外的 Agones 建场景许可块。
// 这里钉住三件事:① 包装函数"投递前"守护段在重生成后原样保留且位于 promise 之前;
// ② 连续重生成结果完全一致(换行不漂移);③ 新文件为每个包装函数生成默认空守护段。
func TestGrpcServiceHandlerCpp_PreservesPreDispatchSection(t *testing.T) {
	setParserTestNaming(t)
	oldExt := _config.Global.FileExtensions
	_config.Global.FileExtensions.Proto = ".proto"
	_config.Global.FileExtensions.GrpcHandlerH = ".h"
	t.Cleanup(func() {
		_config.Global.FileExtensions = oldExt
	})

	methods := buildGrpcHandlerTestMethods()

	fresh := GetGrpcServiceHandlerCppStr(writeParserTempFile(t, "")+".missing", methods, "", "")
	if got, want := strings.Count(fresh, _config.Global.Naming.YourCodeBegin), 1+2*len(methods); got != want {
		t.Fatalf("fresh file section count mismatch: got=%d want=%d\n%s", got, want, fresh)
	}

	existing := strings.Replace(fresh,
		"grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* /*context*/,\n"+
			"    const ::CreateSceneRequest* request,\n"+
			"    ::CreateSceneResponse* response)\n"+
			"{\n"+
			_config.Global.Naming.YourCodePair+"\n",
		"grpc::Status SceneNodeGrpcImpl::CreateScene(grpc::ServerContext* /*context*/,\n"+
			"    const ::CreateSceneRequest* request,\n"+
			"    ::CreateSceneResponse* response)\n"+
			"{\n"+
			_config.Global.Naming.YourCodeBegin+"\n"+
			"    auto createPermit = acquire_permit();\n"+
			"    if (!createPermit) { return grpc::Status(grpc::StatusCode::UNAVAILABLE, \"x\"); }\n"+
			_config.Global.Naming.YourCodeEnd+"\n",
		1)
	if existing == fresh {
		t.Fatalf("test setup failed: CreateScene wrapper pattern not found in fresh output\n%s", fresh)
	}

	dst := writeParserTempFile(t, existing)
	first := GetGrpcServiceHandlerCppStr(dst, methods, "", "")
	if strings.Count(first, "acquire_permit") != 1 {
		t.Fatalf("pre-dispatch code should appear exactly once:\n%s", first)
	}
	wrapperStart := strings.Index(first, "grpc::Status SceneNodeGrpcImpl::CreateScene(")
	permitAt := strings.Index(first, "acquire_permit")
	promiseAt := strings.Index(first[wrapperStart:], "std::promise<void> promise;")
	if wrapperStart < 0 || permitAt < wrapperStart || promiseAt < 0 || permitAt > wrapperStart+promiseAt {
		t.Fatalf("pre-dispatch code must sit inside CreateScene wrapper before dispatch:\n%s", first)
	}

	if err := os.WriteFile(dst, []byte(first), 0o600); err != nil {
		t.Fatalf("rewrite dst failed: %v", err)
	}
	second := GetGrpcServiceHandlerCppStr(dst, methods, "", "")
	if second != first {
		t.Fatalf("regeneration is not idempotent\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func buildGrpcHandlerTestMethods() RPCMethods {
	fd := &descriptorpb.FileDescriptorProto{Name: proto.String("proto/scene/scene_node_service.proto")}
	svc := &descriptorpb.ServiceDescriptorProto{Name: proto.String("SceneNodeGrpc")}
	newMethod := func(name, in, out string) *MethodInfo {
		return &MethodInfo{
			ProtoFileInfo: ProtoFileInfo{Fd: fd, ServiceDescriptorProto: svc},
			MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{
				Name:       proto.String(name),
				InputType:  proto.String(in),
				OutputType: proto.String(out),
			},
		}
	}
	return RPCMethods{
		newMethod("CreateScene", ".CreateSceneRequest", ".CreateSceneResponse"),
		newMethod("DestroyScene", ".DestroySceneRequest", ".Empty"),
	}
}

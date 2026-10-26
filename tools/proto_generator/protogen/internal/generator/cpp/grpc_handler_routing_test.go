package cpp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"protogen/internal"
	_config "protogen/internal/config"
	"protogen/logger"

	messageoption "github.com/luyuancpp/protooption"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

func setupGrpcHandlerRouting(t *testing.T) string {
	t.Helper()
	originalConfig, originalLogger := _config.Global, logger.Global
	t.Cleanup(func() {
		_config.Global, logger.Global = originalConfig, originalLogger
	})
	_config.Global = _config.Config{}
	logger.Global = zap.NewNop()
	root := t.TempDir()
	_config.Global.Paths.OutputRoot = root
	_config.Global.DirectoryNames.ProtoDirName = "proto/"
	_config.Global.FileExtensions.Proto = ".proto"
	_config.Global.FileExtensions.PbH = ".pb.h"
	_config.Global.FileExtensions.GrpcHandlerH = ".h"
	_config.Global.FileExtensions.GrpcHandlerCpp = ".cpp"
	_config.Global.Naming.IncludeBegin = "#include \""
	_config.Global.Naming.YourCodeBegin = "///<<< BEGIN WRITING YOUR CODE"
	_config.Global.Naming.YourCodeEnd = "///<<< END WRITING YOUR CODE"
	_config.Global.Naming.YourCodePair = _config.Global.Naming.YourCodeBegin + "\n" + _config.Global.Naming.YourCodeEnd
	_config.Global.DomainMeta = map[string]_config.DomainMeta{
		"battle": {Source: filepath.Join(root, "proto", "battle"), Rpc: _config.RpcMeta{Type: "grpc"}},
	}
	data, err := yaml.Marshal(map[string]string{"battle_node_grpc": filepath.ToSlash(filepath.Join(root, "battle-grpc")) + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &_config.Global.PathLists.MethodHandlerDirectories); err != nil {
		t.Fatal(err)
	}
	return root
}

func battleGrpcMethods(serviceName string, clientProtocol bool) internal.RPCMethods {
	method := buildMethod("proto/battle/battle_node.proto", messageoption.NodeType_NODE_UNSPECIFIED, false, false, clientProtocol)
	method.ServiceDescriptorProto.Name = proto.String(serviceName)
	method.MethodDescriptorProto = &descriptorpb.MethodDescriptorProto{
		Name: proto.String("CreateBattle"), InputType: proto.String(".CreateBattleRequest"), OutputType: proto.String(".CreateBattleResponse"),
	}
	return asMethods(method)
}

func TestBattleGrpcHandlerRoutingWritesOnlyControlPlane(t *testing.T) {
	root := setupGrpcHandlerRouting(t)
	var wg sync.WaitGroup
	ProcessAllHandlers(&wg, battleGrpcMethods("BattleNode", false))
	wg.Wait()
	for _, extension := range []string{".h", ".cpp"} {
		path := filepath.Join(root, "battle-grpc", "battle_node"+extension)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("未在 battle 的配置目录生成控制面文件 %s: %v", path, err)
		}
		if !strings.Contains(string(content), "BattleNodeImpl") || !strings.Contains(string(content), "CreateBattle") {
			t.Fatalf("控制面文件缺少服务或方法: %s", content)
		}
	}
}

func TestBattleGrpcHandlerRoutingDoesNotRestoreClientProtocol(t *testing.T) {
	root := setupGrpcHandlerRouting(t)
	for _, methods := range []internal.RPCMethods{
		battleGrpcMethods("BattleClientPlayer", true),
		battleGrpcMethods("BattleNode", true),
		battleGrpcMethods("BattleAdmin", false),
	} {
		var wg sync.WaitGroup
		ProcessAllHandlers(&wg, methods)
		wg.Wait()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("非控制面服务写出了 handler: %v", entries)
	}
}

func TestBattleGrpcHandlerRoutingPreservesHeaderState(t *testing.T) {
	root := setupGrpcHandlerRouting(t)
	headerPath := filepath.Join(root, "battle-grpc", "battle_node.h")
	if err := os.MkdirAll(filepath.Dir(headerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	input := "///<<< BEGIN WRITING YOUR CODE: grpc.header.includes\n#include \"battle_admission_gate.h\"\n///<<< END WRITING YOUR CODE\n" +
		"///<<< BEGIN WRITING YOUR CODE: grpc.header.public\n    bool OpenAdmission();\n    void CloseAdmission();\n///<<< END WRITING YOUR CODE\n" +
		"///<<< BEGIN WRITING YOUR CODE: grpc.header.private\n    battle_admission::AdmissionGate admission_;\n///<<< END WRITING YOUR CODE\n"
	if err := os.WriteFile(headerPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	var first []byte
	for generation := 0; generation < 2; generation++ {
		var wg sync.WaitGroup
		ProcessAllHandlers(&wg, battleGrpcMethods("BattleNode", false))
		wg.Wait()
		content, err := os.ReadFile(headerPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range []string{"#include \"battle_admission_gate.h\"", "bool OpenAdmission();", "void CloseAdmission();", "battle_admission::AdmissionGate admission_;"} {
			if strings.Count(string(content), declaration) != 1 {
				t.Fatalf("重生成未完整保留头文件自定义声明 %q: %s", declaration, content)
			}
		}
		if generation == 0 {
			first = content
		} else if string(content) != string(first) {
			t.Fatal("头文件连续重生成不幂等")
		}
	}
}

func TestDefaultConfigProvidesBattleGrpcHandlerDirectory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "etc", "proto_gen.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config _config.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if got := config.PathLists.MethodHandlerDirectories.BattleNodeGrpc; got != "{{node_cpp_dir}}nodes/battle/handler/grpc/" {
		t.Fatalf("默认配置缺少 battle 控制面输出路径: %q", got)
	}
}

func TestTargetGrpcHandlerDescriptorDoesNotGenerateImports(t *testing.T) {
	root := setupGrpcHandlerRouting(t)
	methods := battleGrpcMethods("BattleNode", false)
	methods[0].Fd.Service = []*descriptorpb.ServiceDescriptorProto{methods[0].ServiceDescriptorProto}
	methods[0].ServiceDescriptorProto.Method = []*descriptorpb.MethodDescriptorProto{methods[0].MethodDescriptorProto}
	importedMethods := battleGrpcMethods("BattleClientPlayer", true)
	importedMethods[0].Fd.Name = proto.String("proto/battle/player_battle.proto")
	importedMethods[0].Fd.Service = []*descriptorpb.ServiceDescriptorProto{importedMethods[0].ServiceDescriptorProto}
	importedMethods[0].ServiceDescriptorProto.Method = []*descriptorpb.MethodDescriptorProto{importedMethods[0].MethodDescriptorProto}
	data, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{importedMethods[0].Fd, methods[0].Fd}})
	if err != nil {
		t.Fatal(err)
	}
	if err := generateGrpcHandlerFromDescriptor("proto/battle/battle_node.proto", data); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "battle-grpc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "battle_node.cpp" || entries[1].Name() != "battle_node.h" {
		t.Fatalf("定向生成写出了目标之外的文件: %v", entries)
	}
	if err := generateGrpcHandlerFromDescriptor("proto/battle/player_battle.proto", data); err == nil {
		t.Fatal("定向入口未拒绝已删除的客户端 gRPC handler")
	}
}

// 旧模板留下的非空手写段(§15.8.4)必须先手工迁移:定向生成在写任何文件之前失败,
// .h 预检对同名 .cpp 做旧段检查,两份文件都保持原样(宁可拒绝生成,也不吞代码)。
func TestTargetGrpcHandlerRefusesLegacySectionsBeforeWriting(t *testing.T) {
	root := setupGrpcHandlerRouting(t)
	methods := battleGrpcMethods("BattleNode", false)
	methods[0].Fd.Service = []*descriptorpb.ServiceDescriptorProto{methods[0].ServiceDescriptorProto}
	methods[0].ServiceDescriptorProto.Method = []*descriptorpb.MethodDescriptorProto{methods[0].MethodDescriptorProto}
	data, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{methods[0].Fd}})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "battle-grpc")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	headerPath := filepath.Join(directory, "battle_node.h")
	cppPath := filepath.Join(directory, "battle_node.cpp")
	header := "///<<< BEGIN WRITING YOUR CODE: grpc.header.includes\n#include \"battle_admission_gate.h\"\n///<<< END WRITING YOUR CODE\n"
	cpp := strings.Join([]string{
		"#include \"battle_node.h\"",
		"#include <future>",
		"///<<< BEGIN WRITING YOUR CODE",
		"///<<< END WRITING YOUR CODE",
		"grpc::Status BattleNodeImpl::CreateBattle(grpc::ServerContext* /*context*/,",
		"    const ::CreateBattleRequest* request,",
		"    ::CreateBattleResponse* response)",
		"{",
		"    ///<<< BEGIN WRITING YOUR CODE",
		"    auto permit = agones::GameServerLifecycle::Instance().AcquireAllocationPermitBlocking();",
		"    ///<<< END WRITING YOUR CODE",
		"    return grpc::Status::OK;",
		"}",
		"",
	}, "\n")
	for path, content := range map[string]string{headerPath: header, cppPath: cpp} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = generateGrpcHandlerFromDescriptor("proto/battle/battle_node.proto", data)
	if err == nil || !strings.Contains(err.Error(), "CreateBattle:无名投递前段") || !strings.Contains(err.Error(), "§15.8.4") {
		t.Fatalf("旧段非空时定向生成必须拒绝并列出旧段: %v", err)
	}
	for path, want := range map[string]string{headerPath: header, cppPath: cpp} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("拒绝生成时不得改动 %s:\n%s", path, got)
		}
	}
}

func TestGrpcHandlerTempCleanupRefusesSourceDirectory(t *testing.T) {
	directory := t.TempDir()
	sentinel := filepath.Join(directory, "source.proto")
	if err := os.WriteFile(sentinel, []byte("syntax = \"proto3\";"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeGrpcHandlerTempDir(directory); err == nil {
		t.Fatal("临时目录清理未拒绝非生成器拥有的目录")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("错误删除了源文件: %v", err)
	}
}

func TestGrpcHandlerTempCleanupRemovesOwnedDirectory(t *testing.T) {
	directory, err := os.MkdirTemp("", "proto-gen-grpc-handler-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(directory); err == nil {
			if err := removeGrpcHandlerTempDir(directory); err != nil {
				t.Error(err)
			}
		}
	})
	if err := removeGrpcHandlerTempDir(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("生成器临时目录未清理: %v", err)
	}
}

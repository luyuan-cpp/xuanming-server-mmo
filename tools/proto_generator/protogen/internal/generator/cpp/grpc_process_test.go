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

	completionStart := strings.Index(output, "void HandleCompletedQueueMessage(entt::registry& registry)")
	initStart := strings.Index(output, "void InitGrpcNode(")
	if completionStart < 0 || initStart < 0 || completionStart >= initStart {
		t.Fatal("expected completion and init functions in rendered output")
	}
	completionOutput := output[completionStart:initStart]
	if !strings.Contains(completionOutput, "messageId == 159u || messageId == 160u") {
		t.Fatal("expected completion dispatch to aggregate methods from both services in battle_node.proto")
	}
	if !strings.Contains(completionOutput, "messageId == 162u") {
		t.Fatal("expected completion dispatch for player_battle.proto")
	}
	if !strings.Contains(completionOutput, "::HandleBattleNodeCompletedQueueMessage") ||
		!strings.Contains(completionOutput, "::HandlePlayerBattleCompletedQueueMessage") {
		t.Fatal("expected both same-node completion handlers in rendered output")
	}

	initOutput := output[initStart:]
	if strings.Contains(initOutput, "else if") {
		t.Fatal("same-node gRPC clients must use independent initialization branches")
	}
	if count := strings.Count(initOutput, "if (test::eNodeType::BattleNodeService == nodeType)"); count != 2 {
		t.Fatalf("expected two independent Battle node initialization branches, got %d", count)
	}
	if !strings.Contains(initOutput, "::InitBattleNodeGrpcNode") ||
		!strings.Contains(initOutput, "::InitPlayerBattleGrpcNode") {
		t.Fatal("expected both same-node gRPC clients to be initialized")
	}
}

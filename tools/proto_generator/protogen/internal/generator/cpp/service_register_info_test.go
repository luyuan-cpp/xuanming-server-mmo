package cpp

import (
	"math"
	"testing"

	"protogen/internal"

	"google.golang.org/protobuf/types/descriptorpb"
)

func TestInitMessageIdUpdatesRegistrySizeWhenAssigningUnusedId(t *testing.T) {
	originalServices := internal.GlobalRPCServiceList
	originalMessageIDs := internal.MessageIdMap
	originalRPCMethods := internal.RpcIdMethodMap
	originalMaxMessageID := internal.MaxMessageId
	originalMessageIDFileMaxID := internal.MessageIdFileMaxId
	originalFileMaxMessageID := internal.FileMaxMessageId
	t.Cleanup(func() {
		internal.GlobalRPCServiceList = originalServices
		internal.MessageIdMap = originalMessageIDs
		internal.RpcIdMethodMap = originalRPCMethods
		internal.MaxMessageId = originalMaxMessageID
		internal.MessageIdFileMaxId = originalMessageIDFileMaxID
		internal.FileMaxMessageId = originalFileMaxMessageID
	})

	file := &descriptorpb.FileDescriptorProto{Name: strPtr("proto/test/service.proto")}
	service := &descriptorpb.ServiceDescriptorProto{Name: strPtr("Registry")}
	protoInfo := internal.ProtoFileInfo{
		Fd:                     file,
		ServiceDescriptorProto: service,
	}
	existingMethod := &internal.MethodInfo{
		ProtoFileInfo:         protoInfo,
		Id:                    math.MaxUint64,
		MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: strPtr("Existing")},
	}
	newMethod := &internal.MethodInfo{
		ProtoFileInfo:         protoInfo,
		Id:                    math.MaxUint64,
		MethodDescriptorProto: &descriptorpb.MethodDescriptorProto{Name: strPtr("New")},
	}

	internal.GlobalRPCServiceList = internal.RPCServiceInfoList{{
		ProtoFileInfo: protoInfo,
		Methods:       internal.RPCMethods{existingMethod, newMethod},
	}}
	internal.MessageIdMap = map[string]uint64{existingMethod.KeyName(): 0}
	internal.RpcIdMethodMap = map[uint64]*internal.MethodInfo{}
	internal.MaxMessageId = 2
	internal.MessageIdFileMaxId = 0
	internal.FileMaxMessageId = 0

	InitMessageId()

	if newMethod.Id != 1 {
		t.Fatalf("expected new method to reuse ID 1, got %d", newMethod.Id)
	}
	if internal.FileMaxMessageId != 1 {
		t.Fatalf("expected generated registry max ID 1, got %d", internal.FileMaxMessageId)
	}
	if internal.MessageIdLen() != 2 {
		t.Fatalf("expected generated registry size 2, got %d", internal.MessageIdLen())
	}
}

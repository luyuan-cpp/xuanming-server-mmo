package prototools

import (
	"testing"

	messageoption "github.com/luyuancpp/protooption"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	attributeSyncOptionName   = protoreflect.FullName("OptionAttributeSync")
	attributeSyncOptionNumber = protoreflect.FieldNumber(700000)
)

func TestReadBoolExtensionRegistered(t *testing.T) {
	opts := &descriptorpb.MessageOptions{}
	proto.SetExtension(opts, messageoption.E_OptionAttributeSync, true)

	value, found, err := ReadBoolExtension(
		opts.ProtoReflect(),
		attributeSyncOptionName,
		attributeSyncOptionNumber,
	)
	if err != nil {
		t.Fatalf("读取已注册扩展失败: %v", err)
	}
	if !found || !value {
		t.Fatalf("读取结果错误: found=%v value=%v", found, value)
	}
}

func TestReadBoolExtensionUnknown(t *testing.T) {
	opts := &descriptorpb.MessageOptions{}
	raw := protowire.AppendTag(nil, protowire.Number(attributeSyncOptionNumber), protowire.VarintType)
	raw = protowire.AppendVarint(raw, 1)
	opts.ProtoReflect().SetUnknown(raw)

	value, found, err := ReadBoolExtension(
		opts.ProtoReflect(),
		attributeSyncOptionName,
		attributeSyncOptionNumber,
	)
	if err != nil {
		t.Fatalf("读取未注册扩展失败: %v", err)
	}
	if !found || !value {
		t.Fatalf("读取结果错误: found=%v value=%v", found, value)
	}
}

func TestReadBoolExtensionExplicitFalse(t *testing.T) {
	opts := &descriptorpb.MessageOptions{}
	raw := protowire.AppendTag(nil, protowire.Number(attributeSyncOptionNumber), protowire.VarintType)
	raw = protowire.AppendVarint(raw, 0)
	opts.ProtoReflect().SetUnknown(raw)

	value, found, err := ReadBoolExtension(
		opts.ProtoReflect(),
		attributeSyncOptionName,
		attributeSyncOptionNumber,
	)
	if err != nil {
		t.Fatalf("读取显式 false 扩展失败: %v", err)
	}
	if !found || value {
		t.Fatalf("读取结果错误: found=%v value=%v", found, value)
	}
}

func TestReadBoolExtensionAbsent(t *testing.T) {
	opts := &descriptorpb.MessageOptions{}

	value, found, err := ReadBoolExtension(
		opts.ProtoReflect(),
		attributeSyncOptionName,
		attributeSyncOptionNumber,
	)
	if err != nil {
		t.Fatalf("读取缺失扩展失败: %v", err)
	}
	if found || value {
		t.Fatalf("缺失扩展不应命中: found=%v value=%v", found, value)
	}
}

package prototools

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ReadBoolExtension 按完整名和编号读取 bool custom option。
//
// 已注册的扩展从反射字段读取；未注册的下游扩展从 unknown fields 读取。
// 这样属性同步生成器不再依赖旧 protooption 模块中的具体生成类型。
func ReadBoolExtension(
	message protoreflect.Message,
	expectedName protoreflect.FullName,
	expectedNumber protoreflect.FieldNumber,
) (value bool, found bool, err error) {
	if !message.IsValid() {
		return false, false, nil
	}

	message.Range(func(field protoreflect.FieldDescriptor, fieldValue protoreflect.Value) bool {
		if !field.IsExtension() || field.Number() != expectedNumber {
			return true
		}
		if field.FullName() != expectedName {
			err = fmt.Errorf(
				"扩展编号 %d 已被 %s 占用，期望 %s",
				expectedNumber,
				field.FullName(),
				expectedName,
			)
			return false
		}
		if field.Kind() != protoreflect.BoolKind {
			err = fmt.Errorf(
				"扩展 %s 类型为 %s，期望 bool",
				expectedName,
				field.Kind(),
			)
			return false
		}
		value = fieldValue.Bool()
		found = true
		return false
	})
	if err != nil || found {
		return value, found, err
	}

	unknown := message.GetUnknown()
	for len(unknown) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(unknown)
		if tagLength < 0 {
			return false, false, protowire.ParseError(tagLength)
		}
		unknown = unknown[tagLength:]

		if protoreflect.FieldNumber(number) == expectedNumber {
			if wireType != protowire.VarintType {
				return false, false, fmt.Errorf(
					"扩展 %s 的 wire type 为 %d，期望 varint",
					expectedName,
					wireType,
				)
			}
			rawValue, valueLength := protowire.ConsumeVarint(unknown)
			if valueLength < 0 {
				return false, false, protowire.ParseError(valueLength)
			}
			return rawValue != 0, true, nil
		}

		fieldLength := protowire.ConsumeFieldValue(number, wireType, unknown)
		if fieldLength < 0 {
			return false, false, protowire.ParseError(fieldLength)
		}
		unknown = unknown[fieldLength:]
	}

	return false, false, nil
}

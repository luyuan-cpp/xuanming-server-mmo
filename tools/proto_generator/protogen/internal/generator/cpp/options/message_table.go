package options

import (
	"fmt"
	"os"
	_config "protogen/internal/config"
	prototools "protogen/internal/prototools/option"
	"protogen/internal/utils"
	"protogen/logger"
	"strings"
	"text/template"

	"github.com/iancoleman/strcase"
	"go.uber.org/zap"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	attributeSyncOptionName   = protoreflect.FullName("OptionAttributeSync")
	attributeSyncOptionNumber = protoreflect.FieldNumber(700000)
)

// BuildOption registers callbacks for message options and extension fields.
func BuildOption() {
	if logger.Global == nil {
		tempLogger, _ := zap.NewProduction()
		defer tempLogger.Sync()
		tempLogger.Warn("Global logger not initialized, using temporary logger")

		registerCallbacks(tempLogger)
		return
	}

	registerCallbacks(logger.Global)
}

// registerCallbacks extracts callback registration logic for reuse.
func registerCallbacks(log *zap.Logger) {
	prototools.RegisterCallback(prototools.OptionTypeMessage,
		func(desc interface{}, opts interface{}, context *prototools.OptionContext) error {
			type AttributeField struct {
				FieldName      string // Original field name (e.g. entity_id)
				CamelFieldName string // Upper camel case field name (e.g. EntityId)
				Number         int32  // Field number
				FieldType      string // Field type name (e.g. uint64, Transform, CombatStateFlagsPbComponent)
			}

			type AttributeSyncMessage struct {
				MessageName     string           // Message name (e.g. ActorBaseAttributesS2C)
				CppClass        string           // C++ class name (same as message name)
				ProtoHeaderFile string           // Header file corresponding to the proto file
				ProtoFileName   string           // Original proto file name
				Fields          []AttributeField // All fields in the message
				BitsetSize      int32            // std::bitset size = max field number + 1
			}

			msgDesc, ok := desc.(*descriptorpb.DescriptorProto)
			if !ok {
				return fmt.Errorf("desc type assertion failed: expected *descriptorpb.DescriptorProto, got %T", desc)
			}

			if context == nil || context.File == nil {
				return fmt.Errorf("failed to get file context for message [%s]", msgDesc.GetName())
			}
			fileDesc := context.File

			protoFileName := fileDesc.GetName()
			if protoFileName == "" {
				return fmt.Errorf("proto file name is empty for message [%s]", msgDesc.GetName())
			}
			protoHeaderFile := strings.TrimSuffix(protoFileName, ".proto") + ".pb.h"

			messageOptions, ok := opts.(*descriptorpb.MessageOptions)
			if !ok {
				return fmt.Errorf("属性同步 option 类型错误: 期望 *descriptorpb.MessageOptions，实际为 %T", opts)
			}
			enabled, found, err := prototools.ReadBoolExtension(
				messageOptions.ProtoReflect(),
				attributeSyncOptionName,
				attributeSyncOptionNumber,
			)
			if err != nil {
				return fmt.Errorf("消息 [%s] 的属性同步 option 无效: %w", msgDesc.GetName(), err)
			}
			if !found {
				logger.Global.Debug("Message has no attribute sync option, skipping generation",
					zap.String("message_name", msgDesc.GetName()),
					zap.String("proto_file", protoFileName),
				)
				return nil
			}
			if !enabled {
				logger.Global.Debug("Attribute sync option is false, skipping generation",
					zap.String("message_name", msgDesc.GetName()),
					zap.String("proto_file", protoFileName),
				)
				return nil
			}

			asm := AttributeSyncMessage{
				MessageName:     msgDesc.GetName(),
				CppClass:        msgDesc.GetName(),
				ProtoFileName:   protoFileName,
				ProtoHeaderFile: protoHeaderFile,
				Fields:          make([]AttributeField, 0, len(msgDesc.GetField())),
			}

			for _, field := range msgDesc.GetField() {
				fieldType := getShortFieldTypeName(fileDesc, msgDesc, field)

				asm.Fields = append(asm.Fields, AttributeField{
					FieldName:      field.GetName(),
					CamelFieldName: strcase.ToCamel(field.GetName()),
					Number:         field.GetNumber(),
					FieldType:      fieldType, // Now resolves short names like CombatStateFlagsPbComponent
				})
			}

			// Compute std::bitset size = max field number + 1
			var maxFieldNumber int32
			for _, f := range asm.Fields {
				if f.Number > maxFieldNumber {
					maxFieldNumber = f.Number
				}
			}
			asm.BitsetSize = maxFieldNumber + 1
			if asm.BitsetSize < 1 {
				asm.BitsetSize = 1
			}

			logger.Global.Info("Generating attribute sync code",
				zap.String("message_name", asm.MessageName),
				zap.String("proto_file", asm.ProtoFileName),
				zap.String("proto_header", asm.ProtoHeaderFile),
				zap.Int("field_count", len(asm.Fields)),
			)

			base := strings.ToLower(msgDesc.GetName())
			outputDir := _config.Global.Paths.SceneAttributeSyncDir
			if err := os.MkdirAll(outputDir, 0755); err != nil {
				return fmt.Errorf("failed to create output directory [%s]: %w", outputDir, err)
			}

			funcMap := template.FuncMap{
				"ToLower": strings.ToLower,
				"ToCamel": strcase.ToCamel,
			}
			cppFile, err := _config.Global.GetOutputPath("attribute_sync_cpp", outputDir, base)
			if err != nil {
				return fmt.Errorf("failed to get cpp file path: %w", err)
			}
			cppTemplatePath, err := _config.Global.GetTemplatePath("attribute_sync_cpp")
			if err != nil {
				return fmt.Errorf("failed to get cpp template path: %w", err)
			}
			if err := utils.RenderTemplateToFileWithFuncs(cppTemplatePath, cppFile, asm, funcMap); err != nil {
				return fmt.Errorf("failed to generate cpp file [%s]: %w", cppFile, err)
			}
			hFile, err := _config.Global.GetOutputPath("attribute_sync_h", outputDir, base)
			if err != nil {
				return fmt.Errorf("failed to get header file path: %w", err)
			}
			hTemplatePath, err := _config.Global.GetTemplatePath("attribute_sync_h")
			if err != nil {
				return fmt.Errorf("failed to get header template path: %w", err)
			}
			if err := utils.RenderTemplateToFileWithFuncs(hTemplatePath, hFile, asm, funcMap); err != nil {
				return fmt.Errorf("failed to generate header file [%s]: %w", hFile, err)
			}

			logger.Global.Info("Attribute sync code generation completed",
				zap.String("message_name", asm.MessageName),
				zap.String("proto_header", asm.ProtoHeaderFile),
				zap.String("cpp_file", cppFile),
				zap.String("h_file", hFile),
			)

			return nil
		})
}

// getShortFieldTypeName resolves the short type name for a protobuf field.
// - Primitive types: uint64, string, bool
// - Custom messages: Transform, Velocity, CombatStateFlagsPbComponent
// - Well-known types: Timestamp (not google.protobuf.Timestamp)
func getShortFieldTypeName(fileDesc *descriptorpb.FileDescriptorProto, msgDesc *descriptorpb.DescriptorProto, field *descriptorpb.FieldDescriptorProto) string {
	fieldType := field.GetType()
	if fieldType != descriptorpb.FieldDescriptorProto_TYPE_MESSAGE &&
		fieldType != descriptorpb.FieldDescriptorProto_TYPE_ENUM {
		return strings.ToLower(fieldType.String()[len("TYPE_"):])
	}

	typeName := strings.TrimPrefix(field.GetTypeName(), ".")
	parts := strings.Split(typeName, ".")
	shortName := parts[len(parts)-1]

	// (Optional: ensure 100% accuracy of custom type names)
	return shortName
}

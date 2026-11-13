package _go

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/luyuancpp/proto2mysql"
	messageoption "github.com/luyuancpp/protooption"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"protogen/internal"
	_config "protogen/internal/config"
	utils2 "protogen/internal/utils"
	"protogen/logger"
)

// MessageListConfig defines the message name list structure.
type MessageListConfig struct {
	Messages []string `json:"messages"`
}

// extractMessageNamesFromProto extracts fully qualified message names (adapted for v1.36.6).
func extractMessageNamesFromProto(protoFile string) ([]string, error) {
	var messageNames []string

	for _, fileDesc := range internal.FdSet.GetFile() {
		fileName := filepath.Base(fileDesc.GetName())
		if fileName != protoFile {
			continue
		}

		pkgName := fileDesc.GetPackage()
		for _, msgDesc := range fileDesc.GetMessageType() {
			// 只有声明了表名的消息才建表:player_database 是存盘的内存形状,不是表。
			if tableName, _ := proto.GetExtension(msgDesc.GetOptions(), messageoption.E_OptionTableName).(string); tableName == "" {
				continue
			}
			var fullName string
			if pkgName == "" {
				fullName = msgDesc.GetName() // no package, use message name directly
			} else {
				fullName = pkgName + "." + msgDesc.GetName()
			}
			messageNames = append(messageNames, fullName)
			logger.Global.Info("Extracted fully qualified message name",
				zap.String("proto_file", fileDesc.GetName()),
				zap.String("message_full_name", fullName),
				zap.String("target_proto_file", protoFile),
			)
		}
	}

	if len(messageNames) == 0 {
		logger.Global.Warn("No messages extracted",
			zap.String("target_proto_file", protoFile),
			zap.Int("total_files_scanned", len(internal.FdSet.GetFile())),
		)
	}
	return messageNames, nil
}

// LoadAllDescriptors activates descriptors (v1.36.6 compatible, no protoregistry.NewFiles() needed).
// LoadAllDescriptors adapts to your protoregistry version: uses Files management + protodesc activation.
func LoadAllDescriptors(wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()

	logger.Global.Info("Starting descriptor activation",
		zap.Int("total_files", len(internal.FdSet.GetFile())),
	)

	fileReg := &protoregistry.Files{}

	for _, rawFile := range internal.FdSet.GetFile() {
		activeFileDesc, err := protodesc.NewFile(rawFile, fileReg)
		if err != nil {
			logger.Global.Warn("Failed to activate file, skipping",
				zap.String("file_name", rawFile.GetName()),
				zap.Error(err),
			)
			continue
		}

		if err := fileReg.RegisterFile(activeFileDesc); err != nil {
			logger.Global.Warn("Failed to register file in registry, skipping",
				zap.String("file_path", activeFileDesc.Path()),
				zap.Error(err),
			)
			continue
		}

		internal.FileDescCache[rawFile.GetName()] = activeFileDesc
		logger.Global.Debug("Activated and registered file",
			zap.String("file_path", activeFileDesc.Path()),
			zap.String("package", string(activeFileDesc.Package())),
			zap.Int("message_count", activeFileDesc.Messages().Len()),
		)
	}

	internal.ActiveMsgDescCache = make(map[protoreflect.FullName]protoreflect.MessageDescriptor) // clear old cache
	fileReg.RangeFiles(func(activeFileDesc protoreflect.FileDescriptor) bool {
		messages := activeFileDesc.Messages()
		for i := 0; i < messages.Len(); i++ {
			activeMsgDesc := messages.Get(i)
			var fullNameStr string
			if pkg := string(activeFileDesc.Package()); pkg != "" {
				fullNameStr = pkg + "." + string(activeMsgDesc.Name())
			} else {
				fullNameStr = string(activeMsgDesc.Name())
			}
			fullName := protoreflect.FullName(fullNameStr)

			internal.ActiveMsgDescCache[fullName] = activeMsgDesc
			logger.Global.Debug("Cached message descriptor",
				zap.String("message_full_name", fullNameStr),
				zap.Int("field_count", activeMsgDesc.Fields().Len()),
				zap.String("file_path", string(activeFileDesc.Path())),
			)
		}
		return true
	})

	logger.Global.Info("Descriptor activation completed",
		zap.Int("cached_message_count", len(internal.ActiveMsgDescCache)),
		zap.Int("processed_file_count", len(internal.FileDescCache)),
	)
}

func GenerateMergedTableSQL(messageNames []string) error {
	sqlGenerator := proto2mysql.NewDB()
	var mergedSQL strings.Builder

	for _, msgFullNameStr := range messageNames {
		msgFullName := protoreflect.FullName(msgFullNameStr)

		activeMsgDesc, exists := internal.ActiveMsgDescCache[msgFullName]
		if !exists {
			logger.Global.Warn("Activated message descriptor not found, skipping",
				zap.String("message_full_name", msgFullNameStr),
			)
			continue
		}

		msgInstance := dynamicpb.NewMessage(activeMsgDesc)
		if msgInstance == nil {
			logger.Global.Warn("Failed to create message instance, skipping",
				zap.String("message_full_name", msgFullNameStr),
			)
			continue
		}
		logger.Global.Debug("Message instance created",
			zap.String("message_full_name", msgFullNameStr),
		)

		logMessageFields(msgFullNameStr, msgInstance)

		sqlGenerator.RegisterTable(msgInstance)
		tableSQL := sqlGenerator.GetCreateTableSQL(msgInstance)
		mergedSQL.WriteString(tableSQL)
		mergedSQL.WriteString("\n\n")
	}

	if mergedSQL.Len() == 0 {
		logger.Global.Info("No SQL generated",
			zap.String("reason", "no valid message instances"),
			zap.Int("input_message_count", len(messageNames)),
		)
		return nil
	}

	for _, meta := range _config.Global.DomainMeta {
		if !utils2.HasGrpcService(meta.Source) {
			continue
		}

		sqlDir := utils2.BuildModelGoPath(meta.Source)
		if err := os.MkdirAll(sqlDir, 0755); err != nil {
			logger.Global.Error("Failed to create SQL directory",
				zap.String("sql_dir", sqlDir),
				zap.String("proto_dir", meta.Source),
				zap.Error(err),
			)
			return err
		}

		sqlFileName := _config.Global.FileExtensions.ModelSqlExtension
		sqlPath := filepath.Join(sqlDir, sqlFileName)

		if err := utils2.WriteFileIfChanged(sqlPath, []byte(mergedSQL.String())); err != nil {
			logger.Global.Error("Failed to write SQL file",
				zap.String("sql_path", sqlPath),
				zap.Error(err),
			)
			return err
		}
		logger.Global.Info("SQL file generated",
			zap.String("sql_path", sqlPath),
			zap.String("proto_dir", meta.Source),
			zap.Int("sql_length", mergedSQL.Len()),
		)
	}

	return nil
}

func logMessageFields(msgName string, msg proto.Message) {
	msgReflect := msg.ProtoReflect()
	fieldCount := 0

	fields := msgReflect.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		fieldCount++
		logger.Global.Debug("Message descriptor field",
			zap.String("message_name", msgName),
			zap.String("field_name", string(fd.Name())),
			zap.String("field_kind", fd.Kind().String()),
			zap.Int32("field_number", int32(fd.Number())),
		)
	}

	if fieldCount == 0 {
		logger.Global.Warn("Message descriptor declares no fields",
			zap.String("message_name", msgName),
			zap.String("reason", "empty field list"),
		)
	} else {
		logger.Global.Info("Message descriptor fields recorded",
			zap.String("message_name", msgName),
			zap.Int("field_count", fieldCount),
		)
	}
}

// tableListOutputPaths 是建表清单的全部落点,一次生成写出,字节相同。go/db 按部署方式读其中一份:
//   - generated/data:本机。go/db/etc/db.yaml 的 JsonPath 指向它,merge_zone 也读它。
//   - go/db/data:容器。deploy/k8s/Dockerfile.go-svc 把它拷到 /app/data/,
//     k8s_deploy.ps1 与 prepare_docker_debug.py 生成的 JsonPath 指向那里。
//
// 两种部署注册的表必须相同:data_service 的 HasHistoricalPlayers 要读每个落点库的
// player_centre_database、user_accounts,这些表都由 go/db 的迁移按这份清单建出来。
func tableListOutputPaths() []string {
	name := _config.Global.Naming.DbTableListJson
	return []string{
		filepath.Join(_config.Global.Paths.TableGeneratorDir, name),
		filepath.Join(_config.Global.Paths.NodeGoDir, "db", "data", name),
	}
}

func writeMessageNamesToJSON(messages []string) error {
	// go/db 启动时拒绝空清单。这里写出去只会盖掉上一份好的,把报错推迟到部署之后。
	if len(messages) == 0 {
		return errors.New("table list is empty; refusing to overwrite the existing files")
	}
	data, err := json.MarshalIndent(&MessageListConfig{Messages: messages}, "", "  ")
	if err != nil {
		logger.Global.Error("JSON serialization failed",
			zap.Error(err),
			zap.Int("message_count", len(messages)),
		)
		return err
	}

	for _, outputPath := range tableListOutputPaths() {
		if err := utils2.WriteFileIfChanged(outputPath, data); err != nil {
			logger.Global.Error("Failed to write JSON config file",
				zap.String("output_path", outputPath),
				zap.Error(err),
			)
			return err
		}

		logger.Global.Info("Config file generated",
			zap.String("output_path", outputPath),
			zap.Int("message_count", len(messages)),
		)
	}
	return nil
}

func getFullMessageName(pkgName, msgName string) string {
	if pkgName == "" {
		return msgName
	}
	return pkgName + "." + msgName
}

func GenerateDBResource(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		protoFile := _config.Global.Naming.DbTableFile
		logger.Global.Info("Processing target proto file",
			zap.String("proto_file", protoFile),
		)

		messageNames, err := extractMessageNamesFromProto(protoFile)
		if err != nil {
			logger.Global.Fatal("Failed to extract message names",
				zap.String("proto_file", protoFile),
				zap.Error(err),
			)
		}
		if len(messageNames) == 0 {
			logger.Global.Info("No messages extracted",
				zap.String("proto_file", protoFile),
			)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeMessageNamesToJSON(messageNames); err != nil {
				logger.Global.Fatal("Failed to generate JSON config",
					zap.Error(err),
					zap.Int("message_count", len(messageNames)),
				)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := GenerateMergedTableSQL(messageNames); err != nil {
				logger.Global.Fatal("Failed to generate SQL",
					zap.Error(err),
					zap.Int("message_count", len(messageNames)),
				)
			}
		}()
	}()
}

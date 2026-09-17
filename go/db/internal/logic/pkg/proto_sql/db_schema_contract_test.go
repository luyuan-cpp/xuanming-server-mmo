package proto_sql

import (
	"path/filepath"
	"strings"
	"testing"

	"db/internal/config"
	dbopts "proto/db"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// 覆盖正式清单中的全部表和生产注册入口;不打开数据库,不执行任何 SQL。
// 存量账号键必须继续使用有界完整列,不能随依赖替换退化成 TEXT 前缀主键。
func TestRegisteredTableSchemaContract(t *testing.T) {
	previousDB := DB
	previousJSONPath := config.AppConfig.ServerConfig.JsonPath
	t.Cleanup(func() {
		DB = previousDB
		config.AppConfig.ServerConfig.JsonPath = previousJSONPath
	})
	config.AppConfig.ServerConfig.JsonPath = filepath.Join("..", "..", "..", "..", "..", "..", "generated", "data", "mysql_database_table_list.json")
	DB = &GameDB{SqlModel: proto2mysql.NewDB()}
	tables, err := RegisterTables()
	if err != nil {
		t.Fatalf("注册正式表清单: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("正式表清单不能为空")
	}
	for _, table := range tables {
		md := table.ProtoReflect().Descriptor()
		name, ok := proto2mysql.TableNameFromDescriptor(md)
		if !ok || name == "" {
			t.Fatalf("表 %s 缺少 OptionTableName", md.FullName())
		}
		t.Run(name, func(t *testing.T) {
			if got := proto2mysql.GetTableName(table); got != name {
				t.Fatalf("运行期表名 %q 与声明 %q 不一致", got, name)
			}
			ddl := DB.SqlModel.GetCreateTableSQL(table)
			primary, _ := proto.GetExtension(md.Options(), dbopts.E_OptionPrimaryKey).(string)
			if primary == "" {
				t.Fatal("正式表缺少声明的主键")
			}
			var quoted []string
			for _, key := range strings.Split(primary, ",") {
				key = strings.TrimSpace(key)
				field := md.Fields().ByName(protoreflect.Name(key))
				if field == nil {
					t.Fatalf("主键字段 %q 不存在", key)
				}
				quoted = append(quoted, "`"+key+"`")
				var requiredType string
				switch field.Kind() {
				case protoreflect.StringKind:
					requiredType = "VARCHAR(191)"
				case protoreflect.BytesKind:
					requiredType = "VARBINARY(191)"
				}
				if requiredType != "" && !strings.Contains(ddl, "`"+key+"` "+requiredType) {
					t.Errorf("主键 %s 必须保留 %s 完整列语义:\n%s", key, requiredType, ddl)
				}
			}
			if want := "PRIMARY KEY (" + strings.Join(quoted, ",") + ")"; !strings.Contains(ddl, want) {
				t.Errorf("主键不能使用前缀或缺列,要求 %q:\n%s", want, ddl)
			}
			if nonclustered, _ := proto.GetExtension(md.Options(), dbopts.E_OptionTiDBNonclusteredPK).(bool); nonclustered {
				for _, option := range []string{"/*T![clustered_index] NONCLUSTERED */", "SHARD_ROW_ID_BITS=4", "PRE_SPLIT_REGIONS=4"} {
					if !strings.Contains(ddl, option) {
						t.Errorf("声明的 TiDB 选项 %q 丢失:\n%s", option, ddl)
					}
				}
			}
		})
	}
}

// 正式清单暂时没有 bytes 主键,用最小描述符守住同一修复的二进制键分支。
func TestBinaryPrimaryKeyUsesBoundedFullColumn(t *testing.T) {
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:   proto.String("db_binary_key_contract.proto"),
		Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("BinaryKeyContract"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("binary_key"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()},
				{Name: proto.String("payload"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()},
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	message := dynamicpb.NewMessage(fd.Messages().Get(0))
	model := proto2mysql.NewDB()
	model.RegisterTable(message, proto2mysql.WithPrimaryKey("binary_key"))
	ddl := model.GetCreateTableSQL(message)
	for _, want := range []string{"`binary_key` VARBINARY(191)", "PRIMARY KEY (`binary_key`)", "`payload` MEDIUMBLOB"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("二进制键或非键存储契约丢失 %q:\n%s", want, ddl)
		}
	}
}

package _go

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	messageoption "github.com/luyuancpp/protooption"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protogen/internal"
	_config "protogen/internal/config"
	"protogen/logger"
)

// useTableListConfig 让配置指向临时仓库,返回 generated/data 与 go/db/data 两份清单的路径。
func useTableListConfig(t *testing.T) (generatedList, goDBList string) {
	t.Helper()
	previousConfig := _config.Global
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() {
		_config.Global = previousConfig
		logger.Global = previousLogger
	})
	root := t.TempDir()
	separator := string(os.PathSeparator)
	const name = "mysql_database_table_list.json"
	_config.Global.Paths.TableGeneratorDir = filepath.Join(root, "generated", "data") + separator
	_config.Global.Paths.NodeGoDir = filepath.Join(root, "go") + separator
	_config.Global.Naming.DbTableListJson = name
	return filepath.Join(root, "generated", "data", name), filepath.Join(root, "go", "db", "data", name)
}

func TestTableListReachesBothReadersByteForByte(t *testing.T) {
	generatedList, goDBList := useTableListConfig(t)
	// go/db/data 这份原来没人写,一直停在旧表名上。
	if err := os.MkdirAll(filepath.Dir(goDBList), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goDBList, []byte(`{"messages":["player_database","player_database_1"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	messages := []string{"user", "player_centre_database", "player_store"}
	if err := writeMessageNamesToJSON(messages); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(generatedList)
	if err != nil {
		t.Fatal(err)
	}
	goDB, err := os.ReadFile(goDBList)
	if err != nil {
		t.Fatal(err)
	}
	if string(generated) != string(goDB) {
		t.Fatalf("两份清单必须字节相同:\n%s\n---\n%s", generated, goDB)
	}
	var decoded MessageListConfig
	if err := json.Unmarshal(goDB, &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.Messages, messages) {
		t.Fatalf("清单内容不对: %v", decoded.Messages)
	}

	// 重复运行不改写:内容相同时文件时间不变。
	stamp := time.Unix(1000, 0)
	for _, list := range []string{generatedList, goDBList} {
		if err := os.Chtimes(list, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMessageNamesToJSON(messages); err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{generatedList, goDBList} {
		info, err := os.Stat(list)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(stamp) {
			t.Errorf("内容没变却重写了 %s", list)
		}
	}
}

func TestEmptyTableListKeepsExistingFiles(t *testing.T) {
	generatedList, goDBList := useTableListConfig(t)
	messages := []string{"player_store"}
	if err := writeMessageNamesToJSON(messages); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(goDBList)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMessageNamesToJSON(nil); err == nil {
		t.Fatal("空清单必须报错,不能写出去")
	}
	for _, list := range []string{generatedList, goDBList} {
		after, err := os.ReadFile(list)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("%s 被空清单盖掉: %s", list, after)
		}
	}
}

// 清单只列声明了表名的消息:player_database / player_database_1 是存盘的内存形状,不建表。
func TestTableListComesOnlyFromMessagesWithTableName(t *testing.T) {
	previousLogger := logger.Global
	previousSet := internal.FdSet
	logger.Global = zap.NewNop()
	t.Cleanup(func() {
		logger.Global = previousLogger
		internal.FdSet = previousSet
	})
	table := func(name string) *descriptorpb.MessageOptions {
		options := &descriptorpb.MessageOptions{}
		proto.SetExtension(options, messageoption.E_OptionTableName, name)
		return options
	}
	internal.FdSet = &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		{
			Name: proto.String("proto/common/database/mysql_database_table.proto"),
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("user"), Options: table("user")},
				{Name: proto.String("player_database")},
				{Name: proto.String("player_store"), Options: table("player_store")},
				{Name: proto.String("player_database_1")},
			},
		},
		{
			Name:        proto.String("proto/common/database/rollback_database_table.proto"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("player_snapshot"), Options: table("player_snapshot")}},
		},
	}}
	got, err := extractMessageNamesFromProto("mysql_database_table.proto")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"user", "player_store"}; !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

package schemamigrate

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// 测试用表用 descriptorpb 现场构造,不依赖 go/proto(本 module 不许 import proto)。
//
// 表选项按字段号写进 MessageOptions 的 unknown fields。proto2mysql 按字段号读选项并补扫
// unknown fields,字段号与 proto/db/proto_option.proto 相同,效果等同于在 .proto 里写
// option(OptionTableName) = "..."。
const (
	optTableName           protowire.Number = 500001
	optPrimaryKey          protowire.Number = 500002
	optIndex               protowire.Number = 500011
	optUniqueKey           protowire.Number = 500012
	optTiDBNonclusteredPK  protowire.Number = 500021
	optTiDBShardRowIDBits  protowire.Number = 500022
	optTiDBPreSplitRegions protowire.Number = 500023
)

const (
	tUint64 = descriptorpb.FieldDescriptorProto_TYPE_UINT64
	tUint32 = descriptorpb.FieldDescriptorProto_TYPE_UINT32
	tString = descriptorpb.FieldDescriptorProto_TYPE_STRING
	tEnum   = descriptorpb.FieldDescriptorProto_TYPE_ENUM
	tSint64 = descriptorpb.FieldDescriptorProto_TYPE_SINT64
)

type testField struct {
	name   string
	number int32
	typ    descriptorpb.FieldDescriptorProto_Type
}

// testTable 描述一张测试表。table 为空表示不声明 OptionTableName。
type testTable struct {
	message string
	table   string
	pk      string
	index   string
	unique  string
	tidb    bool // 带上与 trade 表相同的 TiDB 选项
	fields  []testField
}

var testFileSeq atomic.Int64

// build 构造带表选项的动态 message。每次调用都是独立的 FileDescriptor,
// 所以同名 message 的不同「版本」(加字段、改表名)可以在同一个测试里并存。
func (tt testTable) build(t testing.TB) proto.Message {
	t.Helper()
	var raw []byte
	appendString := func(num protowire.Number, v string) {
		if v == "" {
			return
		}
		raw = protowire.AppendTag(raw, num, protowire.BytesType)
		raw = protowire.AppendString(raw, v)
	}
	appendVarint := func(num protowire.Number, v uint64) {
		raw = protowire.AppendTag(raw, num, protowire.VarintType)
		raw = protowire.AppendVarint(raw, v)
	}
	appendString(optTableName, tt.table)
	appendString(optPrimaryKey, tt.pk)
	appendString(optIndex, tt.index)
	appendString(optUniqueKey, tt.unique)
	if tt.tidb {
		appendVarint(optTiDBNonclusteredPK, 1)
		appendVarint(optTiDBShardRowIDBits, 4)
		appendVarint(optTiDBPreSplitRegions, 4)
	}
	opts := &descriptorpb.MessageOptions{}
	opts.ProtoReflect().SetUnknown(raw)

	msg := &descriptorpb.DescriptorProto{Name: proto.String(tt.message), Options: opts}
	for _, f := range tt.fields {
		fdp := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(f.name),
			Number: proto.Int32(f.number),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   f.typ.Enum(),
		}
		if f.typ == tEnum {
			fdp.TypeName = proto.String(".schemamigratetest.TestKind")
		}
		msg.Field = append(msg.Field, fdp)
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:        proto.String(fmt.Sprintf("schemamigratetest/t%d.proto", testFileSeq.Add(1))),
		Package:     proto.String("schemamigratetest"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{msg},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("TestKind"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("TEST_KIND_UNSPECIFIED"), Number: proto.Int32(0)},
			},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("构造测试表 %s 的描述符失败: %v", tt.message, err)
	}
	return dynamicpb.NewMessage(fd.Messages().Get(0))
}

// withField 返回追加了一个字段的副本(模拟 proto 加字段)。
func (tt testTable) withField(f testField) testTable {
	tt.fields = append(append([]testField(nil), tt.fields...), f)
	return tt
}

// listingTable 形状对齐 proto/trade/trade_table.proto 的 trade_listing(整数主键 + 联合索引 + TiDB 选项)。
func listingTable() testTable {
	return testTable{
		message: "Listing",
		table:   "trade_listing",
		pk:      "listing_id",
		index:   "market_zone,status,category;seller_player_id,listing_id",
		tidb:    true,
		fields: []testField{
			{"listing_id", 1, tUint64},
			{"seller_player_id", 2, tUint64},
			{"market_zone", 3, tUint32},
			{"category", 4, tEnum},
			{"title", 5, tString},
			{"price_fen", 6, tUint64},
			{"status", 7, tEnum},
		},
	}
}

// favoriteTable 形状对齐 trade_favorite(复合整数主键)。
func favoriteTable() testTable {
	return testTable{
		message: "Favorite",
		table:   "trade_favorite",
		pk:      "player_id,listing_id",
		index:   "listing_id",
		tidb:    true,
		fields: []testField{
			{"player_id", 1, tUint64},
			{"listing_id", 2, tUint64},
			{"created_ms", 3, tUint64},
		},
	}
}

// receiptTable 带唯一键(string 列,按 191 前缀建)。
func receiptTable() testTable {
	return testTable{
		message: "Receipt",
		table:   "trade_receipt",
		pk:      "receipt_id",
		unique:  "order_no",
		fields: []testField{
			{"receipt_id", 1, tUint64},
			{"order_no", 2, tString},
			{"amount_fen", 3, tUint64},
		},
	}
}

func mustSchema(t testing.TB, messages ...proto.Message) *schema {
	t.Helper()
	sch, err := buildSchema(messages)
	if err != nil {
		t.Fatalf("buildSchema: %v", err)
	}
	return sch
}

// liveMatching 按 schema 造一份与 proto 完全一致的真实结构,测试再在上面做减法。
func liveMatching(sch *schema) liveSchema {
	live := liveSchema{}
	for _, tbl := range sch.tables {
		for _, c := range tbl.columns {
			live.addColumn(tbl.name, c.name, liveColumnType(c.definition))
		}
		for _, pk := range tbl.primaryKey {
			live.addIndexColumn(tbl.name, "PRIMARY", pk)
		}
		for _, idx := range tbl.indexes {
			live.addIndexColumn(tbl.name, idx, "x")
		}
		if tbl.uniqueKey != "" {
			live.addIndexColumn(tbl.name, tbl.uniqueKey, "x")
		}
	}
	return live
}

// liveColumnType 模拟 information_schema.COLUMNS.COLUMN_TYPE:只留类型与 unsigned。
func liveColumnType(definition string) string {
	s := strings.ToLower(strings.TrimSpace(definition))
	for _, cut := range []string{" not null", " default ", " comment "} {
		if i := strings.Index(s, cut); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(strings.TrimSuffix(s, ","))
}

// dropLiveColumn 从真实结构里删掉一列。
func dropLiveColumn(lt *liveTable, column string) {
	delete(lt.columns, column)
	order := lt.columnOrder[:0]
	for _, c := range lt.columnOrder {
		if c != column {
			order = append(order, c)
		}
	}
	lt.columnOrder = order
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

package persistschema

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	messageoption "github.com/luyuancpp/protooption"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"protogen/logger"
)

func TestMain(m *testing.M) {
	if err := logger.Init(); err != nil {
		panic(err)
	}
	defer logger.Sync()
	os.Exit(m.Run())
}

// ---------- 描述符构造 ----------

type fdp = descriptorpb.FieldDescriptorProto

func unknownBools(nums ...protowire.Number) []byte {
	var raw []byte
	for _, n := range nums {
		raw = protowire.AppendTag(raw, n, protowire.VarintType)
		raw = protowire.AppendVarint(raw, 1)
	}
	return raw
}

func msgOpts(nums ...protowire.Number) *descriptorpb.MessageOptions {
	o := &descriptorpb.MessageOptions{}
	o.ProtoReflect().SetUnknown(unknownBools(nums...))
	return o
}

func uncarriedOpts() *descriptorpb.FieldOptions {
	o := &descriptorpb.FieldOptions{}
	o.ProtoReflect().SetUnknown(unknownBools(optionPersistUncarried))
	return o
}

func scalar(name string, num int32, t descriptorpb.FieldDescriptorProto_Type) *fdp {
	return &fdp{Name: proto.String(name), Number: proto.Int32(num), Type: t.Enum(),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
}

func repeatedScalar(name string, num int32, t descriptorpb.FieldDescriptorProto_Type) *fdp {
	f := scalar(name, num, t)
	f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	return f
}

func msgField(name string, num int32, typeName string, repeated bool) *fdp {
	f := &fdp{Name: proto.String(name), Number: proto.Int32(num),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String("." + typeName),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
	if repeated {
		f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	}
	return f
}

func uncarried(f *fdp) *fdp {
	f.Options = uncarriedOpts()
	return f
}

const (
	u32 = descriptorpb.FieldDescriptorProto_TYPE_UINT32
	u64 = descriptorpb.FieldDescriptorProto_TYPE_UINT64
	str = descriptorpb.FieldDescriptorProto_TYPE_STRING
)

// fixture 仿照真实形状:玩家表 → 货币(可携带)、背包(冻结层,实例可携带)、压测探针(未携带),外加一个不可达类型。
// 每次调用返回新的一份,测试可以随意改。
func fixture() *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{
		Name: proto.String("proto/test/persist.proto"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:    proto.String("player_database"),
				Options: msgOpts(optionIsPlayerDatabase),
				Field: []*fdp{
					scalar("player_id", 1, u64),
					msgField("currency", 2, "CurrencyComp", false),
					msgField("bag", 3, "BagAllData", false),
					uncarried(msgField("probe", 4, "Probe", false)),
					scalar("persist_schema_gen", 5, u32),
				},
			},
			{
				Name: proto.String("CurrencyComp"),
				Field: []*fdp{
					repeatedScalar("values", 1, u64),
					msgField("debts", 2, "CurrencyDebtEntry", true),
				},
			},
			{
				Name:  proto.String("CurrencyDebtEntry"),
				Field: []*fdp{scalar("currency_type", 1, u32), scalar("owed", 2, u64)},
			},
			{
				Name:    proto.String("BagAllData"),
				Options: msgOpts(optionPersistFrozen),
				Field: []*fdp{
					msgField("items", 1, "ItemEntry", true),
					msgField("dynamic_bags", 2, "BagAllData.DynamicBagData", true),
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("DynamicBagData"),
					Options: msgOpts(optionPersistFrozen),
					Field:   []*fdp{scalar("bag_id", 1, u64), msgField("items", 2, "ItemEntry", true)},
				}},
			},
			{
				Name:    proto.String("ItemEntry"),
				Options: msgOpts(optionPersistFrozen),
				Field: []*fdp{
					scalar("item_uuid", 1, u64),
					uncarried(msgField("equip", 2, "EquipData", false)),
					msgField("instance", 3, "ItemComp", false),
				},
			},
			{
				Name: proto.String("ItemComp"),
				Field: []*fdp{
					scalar("item_id", 1, u64),
					msgField("equip", 2, "EquipData", false),
					msgField("attrs", 3, "ItemComp.AttrsEntry", true),
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("AttrsEntry"),
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
					Field:   []*fdp{scalar("key", 1, u32), msgField("value", 2, "Attr", false)},
				}},
			},
			{Name: proto.String("EquipData"), Field: []*fdp{scalar("level", 1, u32)}},
			{Name: proto.String("Attr"), Field: []*fdp{scalar("value", 1, u64)}},
			{Name: proto.String("Probe"), Field: []*fdp{scalar("stamped_at", 1, u64)}},
			{Name: proto.String("Orphan"), Field: []*fdp{scalar("x", 1, u32)}},
		},
	}
}

func findMsg(t *testing.T, file *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	t.Helper()
	for _, m := range file.GetMessageType() {
		if m.GetName() == name {
			return m
		}
	}
	t.Fatalf("fixture 没有消息 %s", name)
	return nil
}

func files(f *descriptorpb.FileDescriptorProto) []*descriptorpb.FileDescriptorProto {
	return []*descriptorpb.FileDescriptorProto{f}
}

// baselineLedger 返回对 fixture 建基线后的台账(state 由调用方指定)。
func baselineLedger(t *testing.T, state string) Ledger {
	t.Helper()
	r := Apply(Ledger{State: StateBootstrap}, CollectSchema(files(fixture())))
	if len(r.Errors) > 0 {
		t.Fatalf("建基线不应报错: %v", r.Errors)
	}
	l := r.Ledger
	l.State = state
	return l
}

func newEntries(old, cur Ledger) []Entry {
	return cur.Entries[len(old.Entries):]
}

func hasError(r Result, substr string) bool {
	for _, e := range r.Errors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// ---------- 收集 ----------

func TestCollectSchema(t *testing.T) {
	s := CollectSchema(files(fixture()))

	if !reflect.DeepEqual(s.PlayerTables, []string{"player_database"}) {
		t.Fatalf("PlayerTables = %v", s.PlayerTables)
	}
	for _, name := range []string{"player_database", "CurrencyComp", "CurrencyDebtEntry", "BagAllData", "BagAllData.DynamicBagData",
		"ItemEntry", "ItemComp", "EquipData", "Attr", "Probe"} {
		if !s.Reachable[name] {
			t.Errorf("%s 应可达", name)
		}
	}
	if s.Reachable["Orphan"] || s.Reachable["ItemComp.AttrsEntry"] {
		t.Errorf("不可达类型与 map entry 不应记入: %v", s.Reachable)
	}
	if s.Carried["Probe"] {
		t.Errorf("只经未携带字段可达的 Probe 不应算可携带")
	}
	if !s.Carried["EquipData"] {
		t.Errorf("EquipData 经 ItemComp.equip 可携带,不能因 ItemEntry.equip 投影而排除")
	}
	wantFrozen := map[string]bool{"BagAllData": true, "BagAllData.DynamicBagData": true, "ItemEntry": true}
	if !reflect.DeepEqual(s.Frozen, wantFrozen) {
		t.Errorf("Frozen = %v, want %v", s.Frozen, wantFrozen)
	}
	wantUncarried := map[string]string{"player_database#4": "player_database.probe", "ItemEntry#2": "ItemEntry.equip"}
	if !reflect.DeepEqual(s.Uncarried, wantUncarried) {
		t.Errorf("Uncarried = %v, want %v", s.Uncarried, wantUncarried)
	}
	attrs := s.Fields["ItemComp#3"]
	if attrs.Label != LabelMap || attrs.Type != "map<uint32,Attr>" {
		t.Errorf("map 字段记录为 %+v", attrs)
	}
	debts := s.Fields["CurrencyComp#2"]
	if debts.Label != LabelRepeated || debts.Type != "CurrencyDebtEntry" {
		t.Errorf("repeated message 字段记录为 %+v", debts)
	}
	if s.Fields["CurrencyComp#1"].Type != "uint64" {
		t.Errorf("标量类型名应为小写 uint64: %+v", s.Fields["CurrencyComp#1"])
	}
}

func TestBoolOptionReadsKnownExtension(t *testing.T) {
	// 生产里 OptionIsPlayerDatabase 在 protooption 绑定里,解析描述符后是已知扩展而不是未知字段。
	opts := &descriptorpb.MessageOptions{}
	proto.SetExtension(opts, messageoption.E_OptionIsPlayerDatabase, true)
	data, err := proto.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	parsed := &descriptorpb.MessageOptions{}
	if err := proto.Unmarshal(data, parsed); err != nil {
		t.Fatal(err)
	}
	if !boolOption(parsed, optionIsPlayerDatabase) {
		t.Fatal("已知扩展形式的 OptionIsPlayerDatabase 没读到")
	}
	if boolOption(parsed, optionPersistFrozen) {
		t.Fatal("没设的选项不应读成 true")
	}
	if !boolOption(msgOpts(optionPersistFrozen), optionPersistFrozen) {
		t.Fatal("未知字段形式的 OptionPersistFrozen 没读到")
	}
	if boolOption(nil, optionPersistFrozen) {
		t.Fatal("nil 选项应为 false")
	}
}

// ---------- 发号与规则 ----------

func TestApplyBaselineAllGenOne(t *testing.T) {
	s := CollectSchema(files(fixture()))
	r := Apply(Ledger{State: StateBootstrap}, s)
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %v", r.Errors)
	}
	if !r.Changed || r.Gen != 1 || r.Ledger.State != StateBootstrap {
		t.Fatalf("Changed=%v Gen=%d State=%s", r.Changed, r.Gen, r.Ledger.State)
	}
	if len(r.Ledger.Entries) != len(s.Fields) {
		t.Fatalf("基线应记录全部 %d 个字段,实际 %d", len(s.Fields), len(r.Ledger.Entries))
	}
	for _, e := range r.Ledger.Entries {
		if e.Gen != 1 {
			t.Fatalf("基线字段应为世代 1: %+v", e)
		}
	}
	// 冻结类型在建基线时照常记录,不报 R1。
	if _, ok := s.Fields["ItemEntry#3"]; !ok {
		t.Fatal("冻结类型的字段也要进台账")
	}
}

func TestApplyNoChangeIsStable(t *testing.T) {
	old := baselineLedger(t, StateActive)
	r := Apply(old, CollectSchema(files(fixture())))
	if len(r.Errors) > 0 || r.Changed || r.Gen != 1 {
		t.Fatalf("无改动时不应写回台账: Changed=%v Gen=%d Errors=%v", r.Changed, r.Gen, r.Errors)
	}
}

func TestApplyBootstrapAddsAtGenOne(t *testing.T) {
	old := baselineLedger(t, StateBootstrap)
	f := fixture()
	debt := findMsg(t, f, "CurrencyDebtEntry")
	debt.Field = append(debt.Field, scalar("interest", 3, u64))
	r := Apply(old, CollectSchema(files(f)))
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %v", r.Errors)
	}
	added := newEntries(old, r.Ledger)
	if len(added) != 1 || added[0].Gen != 1 || added[0].Field.Key() != "CurrencyDebtEntry#3" {
		t.Fatalf("引导期新增字段应并入世代 1: %+v", added)
	}
	if r.Gen != 1 {
		t.Fatalf("引导期世代应保持 1,实际 %d", r.Gen)
	}
}

func TestApplyActiveBumpsOneGenPerRun(t *testing.T) {
	old := baselineLedger(t, StateActive)
	f := fixture()
	debt := findMsg(t, f, "CurrencyDebtEntry")
	debt.Field = append(debt.Field, scalar("interest", 3, u64))
	item := findMsg(t, f, "ItemComp")
	item.Field = append(item.Field, scalar("bound", 4, u32))
	r := Apply(old, CollectSchema(files(f)))
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %v", r.Errors)
	}
	added := newEntries(old, r.Ledger)
	if len(added) != 2 || added[0].Gen != 2 || added[1].Gen != 2 || r.Gen != 2 {
		t.Fatalf("同一次生成的新增字段共用新世代 2: %+v Gen=%d", added, r.Gen)
	}
	// 追加行按 (消息, 字段号) 排序,输出可重复。
	if added[0].Field.Message != "CurrencyDebtEntry" || added[1].Field.Message != "ItemComp" {
		t.Fatalf("追加顺序不稳定: %+v", added)
	}
}

func TestApplyRejectsFieldOnFrozenType(t *testing.T) {
	old := baselineLedger(t, StateActive)
	f := fixture()
	entry := findMsg(t, f, "ItemEntry")
	entry.Field = append(entry.Field, scalar("bound_state", 4, u32))
	r := Apply(old, CollectSchema(files(f)))
	if !hasError(r, "R1") || !hasError(r, "ItemEntry#4") {
		t.Fatalf("给冻结类型加字段应报 R1: %v", r.Errors)
	}
	if r.Changed {
		t.Fatal("报错时不得要求写回台账")
	}
}

func TestApplyAllowsNewFrozenTypeAndUncarriedOnlyGrowth(t *testing.T) {
	old := baselineLedger(t, StateActive)
	f := fixture()
	// 新增一个冻结类型并从玩家表引用:类型本身是新的,它的字段按新增发号,不报 R1。
	f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
		Name: proto.String("NewLayout"), Options: msgOpts(optionPersistFrozen),
		Field: []*fdp{scalar("slot", 1, u32)},
	})
	table := findMsg(t, f, "player_database")
	table.Field = append(table.Field, msgField("layout", 6, "NewLayout", false))
	// 只经未携带字段可达的 Probe 加字段:不受冻结规则约束。
	probe := findMsg(t, f, "Probe")
	probe.Field = append(probe.Field, scalar("round", 2, u32))
	r := Apply(old, CollectSchema(files(f)))
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %v", r.Errors)
	}
	if got := len(newEntries(old, r.Ledger)); got != 3 {
		t.Fatalf("应新增 3 行(layout、NewLayout.slot、Probe.round),实际 %d", got)
	}
}

func TestApplyRejectsTypeChange(t *testing.T) {
	old := baselineLedger(t, StateActive)
	f := fixture()
	debt := findMsg(t, f, "CurrencyDebtEntry")
	debt.Field[1].Type = u32.Enum()
	r := Apply(old, CollectSchema(files(f)))
	if !hasError(r, "R3") || !hasError(r, "CurrencyDebtEntry#2") {
		t.Fatalf("改类型应报 R3: %v", r.Errors)
	}
}

func TestApplyRejectsNewMemberOfRecordedOneof(t *testing.T) {
	f := fixture()
	debt := findMsg(t, f, "CurrencyDebtEntry")
	debt.OneofDecl = []*descriptorpb.OneofDescriptorProto{{Name: proto.String("source")}}
	gm := scalar("gm_operator", 3, str)
	gm.OneofIndex = proto.Int32(0)
	debt.Field = append(debt.Field, gm)
	base := Apply(Ledger{State: StateBootstrap}, CollectSchema(files(f)))
	if len(base.Errors) > 0 {
		t.Fatalf("errors: %v", base.Errors)
	}
	old := base.Ledger
	old.State = StateActive

	inbox := scalar("inbox_seq", 4, u64)
	inbox.OneofIndex = proto.Int32(0)
	debt.Field = append(debt.Field, inbox)
	r := Apply(old, CollectSchema(files(f)))
	if !hasError(r, "R2") {
		t.Fatalf("给已记录的 oneof 加成员应报 R2: %v", r.Errors)
	}

	// proto3 optional 的合成 oneof 不算真实 oneof。
	debt.Field = debt.Field[:len(debt.Field)-1]
	debt.OneofDecl = append(debt.OneofDecl, &descriptorpb.OneofDescriptorProto{Name: proto.String("_note")})
	note := scalar("note", 5, str)
	note.OneofIndex = proto.Int32(1)
	note.Proto3Optional = proto.Bool(true)
	debt.Field = append(debt.Field, note)
	r = Apply(old, CollectSchema(files(f)))
	if len(r.Errors) > 0 {
		t.Fatalf("proto3 optional 不应被当成 oneof 成员: %v", r.Errors)
	}
}

func TestApplyRemovalNeedsReservedAndNumberNeverReused(t *testing.T) {
	old := baselineLedger(t, StateActive)

	f := fixture()
	debt := findMsg(t, f, "CurrencyDebtEntry")
	debt.Field = debt.Field[:1] // 删掉 owed(2),不写 reserved
	r := Apply(old, CollectSchema(files(f)))
	if !hasError(r, "reserved") {
		t.Fatalf("删字段不写 reserved 应报错: %v", r.Errors)
	}

	debt.ReservedRange = []*descriptorpb.DescriptorProto_ReservedRange{{Start: proto.Int32(2), End: proto.Int32(3)}}
	r = Apply(old, CollectSchema(files(f)))
	if len(r.Errors) > 0 {
		t.Fatalf("写了 reserved 应允许删除: %v", r.Errors)
	}
	if !r.Changed || len(r.Ledger.Retired) != 1 || r.Ledger.Retired[0] != "CurrencyDebtEntry#2" {
		t.Fatalf("应追加退役行: %+v", r.Ledger.Retired)
	}

	// 退役之后复用这个字段号(换了名字和类型)必须失败。
	retiredLedger := r.Ledger
	f2 := fixture()
	debt2 := findMsg(t, f2, "CurrencyDebtEntry")
	debt2.Field[1] = scalar("paid", 2, u32)
	r = Apply(retiredLedger, CollectSchema(files(f2)))
	if !hasError(r, "复用") {
		t.Fatalf("复用已退役字段号应报错: %v", r.Errors)
	}
}

func TestApplyRenameIsNoteOnly(t *testing.T) {
	old := baselineLedger(t, StateActive)
	f := fixture()
	debt := findMsg(t, f, "CurrencyDebtEntry")
	debt.Field[1].Name = proto.String("owed_amount")
	r := Apply(old, CollectSchema(files(f)))
	if len(r.Errors) > 0 || r.Changed {
		t.Fatalf("改名只提示,不报错、不改台账: Errors=%v Changed=%v", r.Errors, r.Changed)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "owed_amount") {
		t.Fatalf("改名应有提示: %v", r.Notes)
	}
}

func TestApplyRejectsBrokenLedgerStates(t *testing.T) {
	s := CollectSchema(files(fixture()))
	if r := Apply(Ledger{State: StateActive}, s); !hasError(r, "state=bootstrap") {
		t.Fatalf("active 状态的空台账应报错: %v", r.Errors)
	}
	high := baselineLedger(t, StateBootstrap)
	high.Entries[0].Gen = 3
	if r := Apply(high, s); !hasError(r, "改回 bootstrap") {
		t.Fatalf("bootstrap 状态下出现世代 3 应报错: %v", r.Errors)
	}
}

// ---------- 台账文本 ----------

func TestLedgerFormatParseRoundTrip(t *testing.T) {
	l := baselineLedger(t, StateActive)
	l.Entries = append(l.Entries, Entry{Gen: 2, Field: Field{Message: "M", Number: 9, Label: LabelOptional, Type: "uint32", Name: "x", Oneof: "kind"}})
	l.Retired = []string{"M#9"}
	parsed, err := ParseLedger(l.Format())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, l) {
		t.Fatalf("往返不一致:\n got %+v\nwant %+v", parsed, l)
	}
}

// 仓库里的台账必须能解析,且已是生成器写出的规范形态:否则下一次生成会改写出与发号无关的 diff。
func TestRepositoryLedgerIsCanonical(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..", "proto", "persist_schema_gen.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取仓库台账 %s: %v", path, err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	l, err := ParseLedger([]byte(text))
	if err != nil {
		t.Fatalf("仓库台账解析失败: %v", err)
	}
	if string(l.Format()) != text {
		t.Fatal("仓库台账不是规范形态;不要手改,用 proto 生成器写回")
	}
}

func TestParseLedgerRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"缺 state":     "1=M#1 optional uint32 a\n",
		"重复 state":    "state=bootstrap\nstate=active\n",
		"未知 state":    "state=done\n",
		"世代为 0":       "state=active\n0=M#1 optional uint32 a\n",
		"重复字段":        "state=active\n1=M#1 optional uint32 a\n2=M#1 optional uint32 b\n",
		"未知标签":        "state=active\n1=M#1 required uint32 a\n",
		"段数不对":        "state=active\n1=M#1 optional uint32\n",
		"退役行无字段":      "state=active\nretired=M#1\n",
		"字段身份坏":       "state=active\n1=M optional uint32 a\n",
		"oneof 段格式坏": "state=active\n1=M#1 optional uint32 a kind\n",
	}
	for name, text := range cases {
		if _, err := ParseLedger([]byte(text)); err == nil {
			t.Errorf("%s: 应解析失败", name)
		}
	}
}

// ---------- 渲染与入口 ----------

func TestRenderOutputs(t *testing.T) {
	s := CollectSchema(files(fixture()))
	retired, err := RetiredFieldsOf([]string{"ItemComp#7", "CurrencyDebtEntry#9", "ItemComp#4"})
	if err != nil {
		t.Fatal(err)
	}
	cpp := string(RenderCppHeader(3, s, retired))
	for _, want := range []string{
		"inline constexpr uint32_t kPersistSchemaGen = 3;",
		"std::array<std::string_view, 3> kFrozenMessageTypes",
		`"BagAllData.DynamicBagData",`,
		"std::array<std::string_view, 2> kUncarriedFields",
		`"ItemEntry.equip",`,
		`"player_database.probe",`,
		// 退役清单按消息全名、字段号升序,与台账里的书写顺序无关。
		"inline constexpr std::array<RetiredField, 3> kRetiredFields = {{\n" +
			"    {\"CurrencyDebtEntry\", 9},\n    {\"ItemComp\", 4},\n    {\"ItemComp\", 7},\n}};",
		"[[nodiscard]] constexpr bool IsRetiredField(std::string_view message, int number)",
	} {
		if !strings.Contains(cpp, want) {
			t.Errorf("C++ 头文件缺少 %q:\n%s", want, cpp)
		}
	}
	if string(RenderCppHeader(3, s, retired)) != cpp {
		t.Error("C++ 输出不稳定")
	}
	empty := string(RenderCppHeader(1, Schema{}, nil))
	if !strings.Contains(empty, "std::array<std::string_view, 0> kFrozenMessageTypes = {};") {
		t.Errorf("空清单应输出 {}: %s", empty)
	}
	if !strings.Contains(empty, "inline constexpr std::array<RetiredField, 0> kRetiredFields = {};") ||
		!strings.Contains(empty, "constexpr bool IsRetiredField(") {
		t.Errorf("没有退役字段时也要输出空清单与查询函数(C++ 调用方不分情况): %s", empty)
	}
	if _, err := RetiredFieldsOf([]string{"ItemComp"}); err == nil {
		t.Error("不合法的退役身份应报错")
	}
	goSrc := string(RenderGo(3))
	if !strings.Contains(goSrc, "package persistschema") || !strings.Contains(goSrc, "const Gen uint32 = 3") ||
		!strings.HasPrefix(goSrc, "// Code generated by protogen") {
		t.Errorf("Go 输出不对:\n%s", goSrc)
	}
}

func TestRunWritesLedgerAndOutputs(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "persist_schema_gen.txt")
	cppPath := filepath.Join(dir, "cpp", "persist_schema_gen.h")
	goPath := filepath.Join(dir, "go", "persistschema.go")

	if _, err := run(ledgerPath, cppPath, goPath, files(fixture())); err == nil || !strings.Contains(err.Error(), "不会自动新建") {
		t.Fatalf("台账缺失应拒绝: %v", err)
	}
	if err := os.WriteFile(ledgerPath, Ledger{State: StateBootstrap}.Format(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ledgerPath, cppPath, goPath, nil); err == nil || !strings.Contains(err.Error(), "OptionIsPlayerDatabase") {
		t.Fatalf("描述符里没有玩家表时应拒绝改台账: %v", err)
	}
	gen, err := run(ledgerPath, cppPath, goPath, files(fixture()))
	if err != nil || gen != 1 {
		t.Fatalf("gen=%d err=%v", gen, err)
	}
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "1=CurrencyDebtEntry#2 optional uint64 owed") {
		t.Fatalf("基线没有写回台账:\n%s", data)
	}
	for _, p := range []string{cppPath, goPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("产物 %s 没写出: %v", p, err)
		}
	}

	// 违规时不写任何文件。
	before, _ := os.ReadFile(ledgerPath)
	f := fixture()
	entry := findMsg(t, f, "ItemEntry")
	entry.Field = append(entry.Field, scalar("bound_state", 4, u32))
	activeLedger, _ := ParseLedger(before)
	activeLedger.State = StateActive
	if err := os.WriteFile(ledgerPath, activeLedger.Format(), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(ledgerPath)
	if _, err := run(ledgerPath, cppPath, goPath, files(f)); err == nil || !strings.Contains(err.Error(), "R1") {
		t.Fatalf("违规应失败: %v", err)
	}
	after, _ := os.ReadFile(ledgerPath)
	if string(after) != string(before) {
		t.Fatal("违规时台账不得改动")
	}
}

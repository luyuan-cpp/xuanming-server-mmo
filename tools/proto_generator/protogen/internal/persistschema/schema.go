package persistschema

import (
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// 自定义选项的字段号,定义见 proto/db/proto_option.proto。按字段号读,不依赖外部 protooption 模块的编译绑定:
// 新增的两个选项不在已发布的绑定里,解出来是选项消息的未知字段;OptionIsPlayerDatabase 在绑定里,解出来是已知扩展。
const (
	optionIsPlayerDatabase protowire.Number = 500013
	optionPersistFrozen    protowire.Number = 500040
	optionPersistUncarried protowire.Number = 600040
	// 玩家表顶层字段的存储分组(玩家存储按分组拆成多行,docs/design/player-save-direct-commit.md §1)。
	optionPersistGroup protowire.Number = 600041
)

// Schema 是本次生成从描述符集合里收集到的持久字段视图。
type Schema struct {
	// PlayerTables 是标了 OptionIsPlayerDatabase 的玩家表消息全名(排序)。为空说明描述符没加载或选项读错了。
	PlayerTables []string
	// Fields 是从持久根可达的全部字段,按 Field.Key 索引。
	Fields map[string]Field
	// Reachable 是从持久根可达的全部消息类型(含冻结与只经未携带字段可达的)。
	Reachable map[string]bool
	// Carried 是不经过任何未携带字段、从持久根可达的消息类型;往返一致性测试要覆盖的就是其中的非冻结类型。
	Carried map[string]bool
	// Frozen 是显式标了 OptionPersistFrozen 的可达消息类型(R1 只约束它们)。
	Frozen map[string]bool
	// Uncarried 是标了 OptionPersistUncarried 的字段,值为 "<消息全名>.<字段名>",按 Field.Key 索引。
	Uncarried map[string]string
	// reserved 记每个可达消息保留的字段号区间,判断「删字段是否写了 reserved」。
	reserved map[string][]reservedRange
}

type reservedRange struct{ start, end int32 } // [start, end),与描述符一致

func (s Schema) isReserved(message string, number int32) bool {
	for _, r := range s.reserved[message] {
		if number >= r.start && number < r.end {
			return true
		}
	}
	return false
}

type messageInfo struct {
	fullName string
	desc     *descriptorpb.DescriptorProto
}

// CollectSchema 从描述符集合收集持久字段。持久根是标了 OptionIsPlayerDatabase 的表消息,
// 以及显式冻结的消息(例如 PlayerAllData:它不经 SQL,但顶层加字段同样会丢,需要受 R1 约束)。
func CollectSchema(files []*descriptorpb.FileDescriptorProto) Schema {
	messages := indexMessages(files)
	s := Schema{
		Fields:    map[string]Field{},
		Reachable: map[string]bool{},
		Carried:   map[string]bool{},
		Frozen:    map[string]bool{},
		Uncarried: map[string]string{},
		reserved:  map[string][]reservedRange{},
	}

	tables := map[string]bool{}
	var roots []string
	for name, info := range messages {
		isTable := boolOption(info.desc.GetOptions(), optionIsPlayerDatabase)
		if isTable {
			s.PlayerTables = append(s.PlayerTables, name)
			tables[name] = true
		}
		if isTable || boolOption(info.desc.GetOptions(), optionPersistFrozen) {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	sort.Strings(s.PlayerTables)

	walk(messages, roots, false, func(info *messageInfo) {
		s.Reachable[info.fullName] = true
		if boolOption(info.desc.GetOptions(), optionPersistFrozen) {
			s.Frozen[info.fullName] = true
		}
		for _, r := range info.desc.GetReservedRange() {
			s.reserved[info.fullName] = append(s.reserved[info.fullName], reservedRange{start: r.GetStart(), end: r.GetEnd()})
		}
		for _, fd := range info.desc.GetField() {
			f := describeField(messages, info, fd)
			if tables[info.fullName] {
				f.Group, f.HasGroup = uint32Option(fd.GetOptions(), optionPersistGroup)
			}
			s.Fields[f.Key()] = f
			if boolOption(fd.GetOptions(), optionPersistUncarried) {
				s.Uncarried[f.Key()] = info.fullName + "." + fd.GetName()
			}
		}
	})
	// 可携带集合只从「真正的顶层容器」出发:玩家表,以及玩家表到达不了的冻结根(如 PlayerAllData)。
	// 深处的冻结类型虽然也是 roots(为了它们在不可达时也受 R1 约束),但不能当成可携带的起点:
	// 只经未携带字段可达的冻结类型,不该被往返测试要求覆盖。
	fromTables := map[string]bool{}
	walk(messages, s.PlayerTables, false, func(info *messageInfo) { fromTables[info.fullName] = true })
	carriedRoots := append([]string(nil), s.PlayerTables...)
	for _, name := range roots {
		if !fromTables[name] {
			carriedRoots = append(carriedRoots, name)
		}
	}
	walk(messages, carriedRoots, true, func(info *messageInfo) {
		s.Carried[info.fullName] = true
	})
	return s
}

// walk 从根出发广度遍历消息类型。skipUncarried 为真时不穿过标了 OptionPersistUncarried 的字段。
// map 字段穿过合成的 entry 类型、直达值类型,entry 类型本身不访问(它的键值两个字段由 map 语义固定)。
func walk(messages map[string]*messageInfo, roots []string, skipUncarried bool, visit func(*messageInfo)) {
	visited := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		info, ok := messages[name]
		if !ok || visited[name] {
			continue
		}
		visited[name] = true
		visit(info)
		for _, fd := range info.desc.GetField() {
			if fd.GetType() != descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
				continue
			}
			if skipUncarried && boolOption(fd.GetOptions(), optionPersistUncarried) {
				continue
			}
			target := trimDot(fd.GetTypeName())
			if entry, ok := messages[target]; ok && entry.desc.GetOptions().GetMapEntry() {
				if value := mapValueField(entry.desc); value != nil && value.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
					queue = append(queue, trimDot(value.GetTypeName()))
				}
				continue
			}
			queue = append(queue, target)
		}
	}
}

func describeField(messages map[string]*messageInfo, owner *messageInfo, fd *descriptorpb.FieldDescriptorProto) Field {
	f := Field{Message: owner.fullName, Number: fd.GetNumber(), Name: fd.GetName(), Label: LabelOptional}
	if fd.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		f.Label = LabelRepeated
	}
	f.Type = typeName(fd)
	if fd.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		if entry, ok := messages[trimDot(fd.GetTypeName())]; ok && entry.desc.GetOptions().GetMapEntry() {
			f.Label = LabelMap
			f.Type = "map<" + typeName(mapKeyField(entry.desc)) + "," + typeName(mapValueField(entry.desc)) + ">"
		}
	}
	// proto3 optional 的合成 oneof 不算真实 oneof:它只表达 presence,不会与别的成员互斥。
	if fd.OneofIndex != nil && !fd.GetProto3Optional() {
		if idx := int(fd.GetOneofIndex()); idx >= 0 && idx < len(owner.desc.GetOneofDecl()) {
			f.Oneof = owner.desc.GetOneofDecl()[idx].GetName()
		}
	}
	return f
}

func typeName(fd *descriptorpb.FieldDescriptorProto) string {
	if fd == nil {
		return "unknown"
	}
	switch fd.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
		return trimDot(fd.GetTypeName())
	case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
		return "enum:" + trimDot(fd.GetTypeName())
	case descriptorpb.FieldDescriptorProto_TYPE_GROUP:
		return "group:" + trimDot(fd.GetTypeName())
	default:
		return strings.ToLower(strings.TrimPrefix(fd.GetType().String(), "TYPE_"))
	}
}

func mapKeyField(entry *descriptorpb.DescriptorProto) *descriptorpb.FieldDescriptorProto {
	return fieldByNumber(entry, 1)
}

func mapValueField(entry *descriptorpb.DescriptorProto) *descriptorpb.FieldDescriptorProto {
	return fieldByNumber(entry, 2)
}

func fieldByNumber(desc *descriptorpb.DescriptorProto, number int32) *descriptorpb.FieldDescriptorProto {
	for _, fd := range desc.GetField() {
		if fd.GetNumber() == number {
			return fd
		}
	}
	return nil
}

// indexMessages 按全名(不带前导点)索引所有消息,含嵌套类型。
func indexMessages(files []*descriptorpb.FileDescriptorProto) map[string]*messageInfo {
	out := map[string]*messageInfo{}
	var add func(prefix string, desc *descriptorpb.DescriptorProto)
	add = func(prefix string, desc *descriptorpb.DescriptorProto) {
		name := desc.GetName()
		if prefix != "" {
			name = prefix + "." + name
		}
		out[name] = &messageInfo{fullName: name, desc: desc}
		for _, nested := range desc.GetNestedType() {
			add(name, nested)
		}
	}
	for _, file := range files {
		for _, desc := range file.GetMessageType() {
			add(file.GetPackage(), desc)
		}
	}
	return out
}

func trimDot(name string) string { return strings.TrimPrefix(name, ".") }

// boolOption 按字段号读布尔选项。选项可能已被解析成已知扩展(绑定里有),也可能留在未知字段里(绑定里没有);
// 两处都看,同一字段出现多次时按 protobuf 语义取最后一次。
func boolOption(opts proto.Message, number protowire.Number) bool {
	if opts == nil {
		return false
	}
	m := opts.ProtoReflect()
	if !m.IsValid() {
		return false
	}
	found, value := false, false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Number() == number && fd.Kind() == protoreflect.BoolKind && !fd.IsList() {
			found, value = true, v.Bool()
			return false
		}
		return true
	})
	if found {
		return value
	}
	raw := m.GetUnknown()
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return false
		}
		raw = raw[n:]
		if num == number && typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(raw)
			if vn < 0 {
				return false
			}
			value = v != 0
			raw = raw[vn:]
			continue
		}
		skip := protowire.ConsumeFieldValue(num, typ, raw)
		if skip < 0 {
			return false
		}
		raw = raw[skip:]
	}
	return value
}

// uint32Option 按字段号读 uint32 选项,口径同 boolOption:已知扩展与未知字段两处都看,出现多次取最后一次。
// 第二个返回值表示选项是否出现过(0 是合法的分组值:元数据字段不进任何分组)。
func uint32Option(opts proto.Message, number protowire.Number) (uint32, bool) {
	if opts == nil {
		return 0, false
	}
	m := opts.ProtoReflect()
	if !m.IsValid() {
		return 0, false
	}
	var value uint32
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Number() == number && fd.Kind() == protoreflect.Uint32Kind && !fd.IsList() {
			found, value = true, uint32(v.Uint())
			return false
		}
		return true
	})
	if found {
		return value, true
	}
	raw := m.GetUnknown()
	for len(raw) > 0 {
		num, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return value, found
		}
		raw = raw[n:]
		if num == number && typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(raw)
			if vn < 0 {
				return value, found
			}
			value, found = uint32(v), true
			raw = raw[vn:]
			continue
		}
		skip := protowire.ConsumeFieldValue(num, typ, raw)
		if skip < 0 {
			return value, found
		}
		raw = raw[skip:]
	}
	return value, found
}

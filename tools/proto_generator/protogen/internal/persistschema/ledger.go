// Package persistschema 维护玩家持久 schema 世代台账 proto/persist_schema_gen.txt,并据此产出
// C++ / Go 的世代常量与往返一致性测试用清单。
//
// 设计见 docs/design/mixed-version-persist-field-protection.md §3:每张玩家表消息带覆盖世代
// persist_schema_gen,世代号在这里按「两张玩家表消息可达的字段集合」单调发放;不能被旧写方安全携带的
// 改动(给冻结类型加字段、给已有 oneof 加成员、改类型、删字段不写 reserved、复用字段号)在生成期失败。
//
// 台账与 proto/message_id.txt 同一套路:由生成器维护、提交进仓库,PR 里能看到每次发号。
// 唯一允许的手工修改是负责人在引导期结束时把 state 从 bootstrap 改为 active(§8.1)。
//
// 封存(§3.1「封存」):发布前用 dev_tools.ps1 -Command persist-schema-seal 追加一行 sealed=,记下当时最大世代
// 及其以下字段行的摘要。此后生成器每次运行都核对:已封存世代的字段集合不得再增删。要守的不变量是「同一世代号,
// 所有二进制认识的字段集合相同」—— 只比世代号的 C1 / C1b / C2 / C3 / C4 都靠它成立。
package persistschema

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 台账状态。bootstrap:机制上线、全网换版期间,新增字段一律并入世代 1(不升世代);
// active:每次出现新增字段的生成运行追加一个新世代。
const (
	StateBootstrap = "bootstrap"
	StateActive    = "active"
)

// 字段标签。map 单列出来:map 与 repeated message 的线格式虽然相同,语义(按键合并)不同,改动要被看见。
const (
	LabelOptional = "optional"
	LabelRepeated = "repeated"
	LabelMap      = "map"
)

// Field 是台账里的一条持久字段。身份是 (Message, Number);Name 只为可读,改名不算改动。
type Field struct {
	Message string // 消息全名,不带前导点,例如 "player_database"、"BagAllData.DynamicBagData"
	Number  int32
	Label   string // LabelOptional / LabelRepeated / LabelMap
	Type    string // 标量小写类型名、消息全名、"enum:<全名>" 或 "map<键类型,值类型>"
	Name    string
	Oneof   string // 所属的真实 oneof 名;不在 oneof 里或 proto3 optional 的合成 oneof 为空
	// Group 是玩家表顶层字段的存储分组(OptionPersistGroup);HasGroup 为假表示没有这个选项(嵌套消息的字段、
	// player_database_1 的字段),或台账里还没记录。分组不进 Key,也不进封存摘要:它管的是「存在哪一行」,
	// 不是「这个世代认识哪些字段」;换组另由 Apply 拒绝(审计 N1)。
	Group    uint32
	HasGroup bool
}

// Key 返回字段身份 "<消息全名>#<字段号>"。
func (f Field) Key() string {
	return f.Message + "#" + strconv.FormatInt(int64(f.Number), 10)
}

// Entry 是台账里的一行字段记录:字段在哪个世代引入。
type Entry struct {
	Gen   uint32
	Field Field
}

// Seal 是一条封存记录:发布时世代 ≤ Gen 的字段行共 Rows 行,摘要为 Digest(SealDigest 的口径)。
type Seal struct {
	Gen    uint32
	Rows   int
	Digest string // 64 位小写十六进制 SHA-256
}

// Ledger 是台账的内存形态。Entries 与 Retired 保持文件顺序,新增只追加,不重排旧行。
type Ledger struct {
	State   string
	Entries []Entry
	Retired []string // 已退役的字段身份(Field.Key),字段号永不复用
	Seals   []Seal   // 封存记录,按世代严格递增;只由 SealLedger 追加
}

const ledgerHeader = `# 玩家持久 schema 世代台账。由 proto 生成器维护,不要手改。
# 设计:docs/design/mixed-version-persist-field-protection.md §3。
# state=bootstrap:引导期,新增字段并入世代 1;引导期结束时由负责人改为 state=active,此后每次出现新增字段的
#   生成运行追加一个新世代。只在主干发号:发布分支需要新持久字段时,先合主干拿号,再原样 cherry-pick 本文件的行。
# 字段行:<世代>=<消息全名>#<字段号> <标签> <类型> <字段名>[ oneof=<oneof 名>][ group=<存储分组>]
#   group= 只出现在玩家表顶层字段上,记 OptionPersistGroup;记下之后不得再改(换组按 R4 走 expand / contract)。
# 退役行:retired=<消息全名>#<字段号>(字段已删除并写了 reserved;字段号永不复用)
# 封存行:sealed=<世代>:<行数>:<sha256>,发布前由 dev_tools.ps1 -Command persist-schema-seal 追加。世代 ≤ 它的
#   字段行此后不得增删:引导期还在加字段、合并带进同一世代的行,都会让生成失败(设计 §3.1「封存」)。
`

// ParseLedger 解析台账文本。格式不认识、身份重复都报错并带行号:台账是发号的唯一依据,不能猜。
func ParseLedger(data []byte) (Ledger, error) {
	var l Ledger
	seen := map[string]bool{}
	retired := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Ledger{}, fmt.Errorf("台账第 %d 行缺少 '=': %q", lineNo, line)
		}
		switch {
		case key == "state":
			if value != StateBootstrap && value != StateActive {
				return Ledger{}, fmt.Errorf("台账第 %d 行 state 只能是 %s 或 %s: %q", lineNo, StateBootstrap, StateActive, value)
			}
			if l.State != "" {
				return Ledger{}, fmt.Errorf("台账第 %d 行重复声明 state", lineNo)
			}
			l.State = value
		case key == "retired":
			if _, _, err := splitFieldKey(value); err != nil {
				return Ledger{}, fmt.Errorf("台账第 %d 行退役身份不合法: %w", lineNo, err)
			}
			if retired[value] {
				return Ledger{}, fmt.Errorf("台账第 %d 行重复退役 %s", lineNo, value)
			}
			retired[value] = true
			l.Retired = append(l.Retired, value)
		case key == "sealed":
			seal, err := parseSealValue(value)
			if err != nil {
				return Ledger{}, fmt.Errorf("台账第 %d 行封存行不合法: %w", lineNo, err)
			}
			if n := len(l.Seals); n > 0 && seal.Gen <= l.Seals[n-1].Gen {
				return Ledger{}, fmt.Errorf("台账第 %d 行封存世代 %d 不大于上一条封存世代 %d:封存行只能按世代递增追加",
					lineNo, seal.Gen, l.Seals[n-1].Gen)
			}
			l.Seals = append(l.Seals, seal)
		default:
			gen, err := strconv.ParseUint(key, 10, 32)
			if err != nil || gen == 0 {
				return Ledger{}, fmt.Errorf("台账第 %d 行世代号必须是正整数: %q", lineNo, key)
			}
			field, err := parseFieldValue(value)
			if err != nil {
				return Ledger{}, fmt.Errorf("台账第 %d 行: %w", lineNo, err)
			}
			if seen[field.Key()] {
				return Ledger{}, fmt.Errorf("台账第 %d 行重复记录 %s", lineNo, field.Key())
			}
			seen[field.Key()] = true
			l.Entries = append(l.Entries, Entry{Gen: uint32(gen), Field: field})
		}
	}
	if err := scanner.Err(); err != nil {
		return Ledger{}, fmt.Errorf("读取台账失败: %w", err)
	}
	if l.State == "" {
		return Ledger{}, fmt.Errorf("台账缺少 state 行(state=%s 或 state=%s)", StateBootstrap, StateActive)
	}
	// 退役字段的字段行保留(记着它在哪个世代引入);退役行指向台账里不存在的字段,说明台账被手工改坏了。
	for _, key := range l.Retired {
		if !seen[key] {
			return Ledger{}, fmt.Errorf("退役行 %s 在台账里没有对应的字段行", key)
		}
	}
	if n := len(l.Seals); n > 0 && l.Seals[n-1].Gen > l.MaxGen() {
		return Ledger{}, fmt.Errorf("封存世代 %d 比台账最大世代 %d 还大:封存行被手工改坏了", l.Seals[n-1].Gen, l.MaxGen())
	}
	return l, nil
}

// parseSealValue 解析封存行等号右侧:"<世代>:<行数>:<sha256>"。
func parseSealValue(value string) (Seal, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return Seal{}, fmt.Errorf("应为 <世代>:<行数>:<sha256>: %q", value)
	}
	gen, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || gen == 0 {
		return Seal{}, fmt.Errorf("世代号必须是正整数: %q", value)
	}
	rows, err := strconv.Atoi(parts[1])
	if err != nil || rows <= 0 {
		return Seal{}, fmt.Errorf("行数必须是正整数: %q", value)
	}
	digest := parts[2]
	if _, err := hex.DecodeString(digest); err != nil || len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return Seal{}, fmt.Errorf("摘要必须是 64 位小写十六进制: %q", value)
	}
	return Seal{Gen: uint32(gen), Rows: rows, Digest: digest}, nil
}

// parseFieldValue 解析字段行等号右侧:"<消息全名>#<字段号> <标签> <类型> <字段名>[ oneof=<名>][ group=<分组>]"。
func parseFieldValue(value string) (Field, error) {
	parts := strings.Fields(value)
	if len(parts) < 4 || len(parts) > 6 {
		return Field{}, fmt.Errorf("字段行应为 4 到 6 段,实际 %d 段: %q", len(parts), value)
	}
	message, number, err := splitFieldKey(parts[0])
	if err != nil {
		return Field{}, err
	}
	switch parts[1] {
	case LabelOptional, LabelRepeated, LabelMap:
	default:
		return Field{}, fmt.Errorf("未知标签 %q", parts[1])
	}
	f := Field{Message: message, Number: number, Label: parts[1], Type: parts[2], Name: parts[3]}
	// 可选段的顺序固定为 oneof= 在前、group= 在后,各至多一次:与 Format 的写法一致,台账才有唯一的规范形态。
	for _, extra := range parts[4:] {
		if oneof, ok := strings.CutPrefix(extra, "oneof="); ok {
			if oneof == "" || f.Oneof != "" || f.HasGroup {
				return Field{}, fmt.Errorf("oneof=<名> 只能出现一次,且在 group= 之前: %q", value)
			}
			f.Oneof = oneof
			continue
		}
		if groupText, ok := strings.CutPrefix(extra, "group="); ok {
			group, err := strconv.ParseUint(groupText, 10, 32)
			if err != nil || f.HasGroup {
				return Field{}, fmt.Errorf("group=<存储分组> 只能出现一次,且是非负整数: %q", value)
			}
			f.Group, f.HasGroup = uint32(group), true
			continue
		}
		return Field{}, fmt.Errorf("第 5、6 段只能是 oneof=<名> 或 group=<存储分组>: %q", extra)
	}
	return f, nil
}

// splitFieldKey 拆分 "<消息全名>#<字段号>"。
func splitFieldKey(key string) (string, int32, error) {
	message, numberText, ok := strings.Cut(key, "#")
	if !ok || message == "" {
		return "", 0, fmt.Errorf("字段身份应为 <消息全名>#<字段号>: %q", key)
	}
	number, err := strconv.ParseInt(numberText, 10, 32)
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("字段号必须是正整数: %q", key)
	}
	return message, int32(number), nil
}

// Format 把台账写回文本。旧行顺序不变,新增行在 Apply 里已追加到末尾,所以 diff 只有追加。
func (l Ledger) Format() []byte {
	var b strings.Builder
	b.WriteString(ledgerHeader)
	b.WriteString("state=")
	b.WriteString(l.State)
	b.WriteString("\n")
	if len(l.Entries) > 0 {
		b.WriteString("\n")
	}
	for _, e := range l.Entries {
		f := e.Field
		fmt.Fprintf(&b, "%d=%s %s %s %s", e.Gen, f.Key(), f.Label, f.Type, f.Name)
		if f.Oneof != "" {
			b.WriteString(" oneof=")
			b.WriteString(f.Oneof)
		}
		if f.HasGroup {
			fmt.Fprintf(&b, " group=%d", f.Group)
		}
		b.WriteString("\n")
	}
	if len(l.Retired) > 0 {
		b.WriteString("\n")
	}
	for _, key := range l.Retired {
		b.WriteString("retired=")
		b.WriteString(key)
		b.WriteString("\n")
	}
	if len(l.Seals) > 0 {
		b.WriteString("\n")
	}
	for _, seal := range l.Seals {
		fmt.Fprintf(&b, "sealed=%d:%d:%s\n", seal.Gen, seal.Rows, seal.Digest)
	}
	return []byte(b.String())
}

// MaxGen 返回台账里最大的世代号;台账为空时为 0。
func (l Ledger) MaxGen() uint32 {
	var maxGen uint32
	for _, e := range l.Entries {
		if e.Gen > maxGen {
			maxGen = e.Gen
		}
	}
	return maxGen
}

// SealDigest 返回世代 ≤ gen 的字段行数与摘要。口径:按 (消息全名, 字段号) 排序,每行写
// "<世代>=<字段身份> <标签> <类型>[ oneof=<名>]" 加换行后求 SHA-256。
// 字段名不进摘要:改名不改线格式,生成器允许;标签、类型、oneof 归属一旦改动,Apply 本来就拒绝(R3)。
// 退役行不进摘要:退役不改变「这个世代认识哪些字段」。
func SealDigest(entries []Entry, gen uint32) (int, string) {
	var rows []Entry
	for _, e := range entries {
		if e.Gen <= gen {
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].Field, rows[j].Field
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		return a.Number < b.Number
	})
	h := sha256.New()
	for _, e := range rows {
		f := e.Field
		fmt.Fprintf(h, "%d=%s %s %s", e.Gen, f.Key(), f.Label, f.Type)
		if f.Oneof != "" {
			fmt.Fprintf(h, " oneof=%s", f.Oneof)
		}
		h.Write([]byte("\n"))
	}
	return len(rows), hex.EncodeToString(h.Sum(nil))
}

// SealViolations 核对 entries 是否仍满足每条封存记录;返回不满足的描述,全部满足时为空。
func SealViolations(entries []Entry, seals []Seal) []string {
	var violations []string
	for _, seal := range seals {
		rows, digest := SealDigest(entries, seal.Gen)
		if rows != seal.Rows || digest != seal.Digest {
			violations = append(violations, fmt.Sprintf("世代 ≤ %d 的字段集合与封存记录不一致(封存时 %d 行,现在 %d 行)",
				seal.Gen, seal.Rows, rows))
		}
	}
	return violations
}

// SealLedger 把台账封存到当前最大世代:追加 sealed=<最大世代>:<行数>:<摘要>。已封存到最大世代时原样返回,changed 为 false。
// 已有的封存记录被违反时拒绝封存:那说明已发布的世代被改过,要先按 Apply 的提示修台账,不能用新封存把问题盖住。
func SealLedger(l Ledger) (sealed Ledger, seal Seal, changed bool, err error) {
	if violations := SealViolations(l.Entries, l.Seals); len(violations) > 0 {
		return l, Seal{}, false, fmt.Errorf("已有封存记录被违反,不能再封存:%s", strings.Join(violations, ";"))
	}
	maxGen := l.MaxGen()
	if maxGen == 0 {
		return l, Seal{}, false, fmt.Errorf("台账没有任何字段行,没有可封存的世代")
	}
	rows, digest := SealDigest(l.Entries, maxGen)
	seal = Seal{Gen: maxGen, Rows: rows, Digest: digest}
	if n := len(l.Seals); n > 0 && l.Seals[n-1] == seal {
		return l, seal, false, nil
	}
	// 走到这里,最后一条封存的世代一定小于 maxGen:同世代的封存若摘要不同,上面的核对已经报错。
	l.Seals = append(append([]Seal(nil), l.Seals...), seal)
	return l, seal, true, nil
}

package persistschema

import (
	"fmt"
	"sort"
	"strings"
)

// Result 是一次比对的结果。Errors 非空时调用方必须让生成失败,且不得写回台账与产物。
type Result struct {
	Ledger  Ledger   // 比对后的台账(只追加)
	Changed bool     // 台账是否需要写回
	Gen     uint32   // 本二进制的世代:台账里最大的世代号,台账为空时为 1
	Errors  []string // 违反可携带性规则(§3.2 R1–R3)或台账与 proto 不一致
	Notes   []string // 不阻断生成、但要让人看见的事项(改名、类型不再可达、疑似字段迁移)
}

// Apply 用当前 proto 的持久字段比对台账,按 §3.1 / §3.2 发号并检查规则。纯函数,不读写文件。
func Apply(old Ledger, s Schema) Result {
	r := Result{Ledger: Ledger{State: old.State}}
	r.Ledger.Entries = append(r.Ledger.Entries, old.Entries...)
	r.Ledger.Retired = append(r.Ledger.Retired, old.Retired...)
	r.Ledger.Seals = append(r.Ledger.Seals, old.Seals...)

	recorded := map[string]Entry{}
	recordedAt := map[string]int{} // 字段身份 → 在 r.Ledger.Entries 里的下标,补记存储分组时原地写
	messagesInLedger := map[string]bool{}
	for i, e := range old.Entries {
		recorded[e.Field.Key()] = e
		recordedAt[e.Field.Key()] = i
		messagesInLedger[e.Field.Message] = true
	}
	groupBackfill := map[string]uint32{}
	retired := map[string]bool{}
	for _, key := range old.Retired {
		retired[key] = true
	}

	baseline := len(old.Entries) == 0
	if baseline && old.State != StateBootstrap {
		r.Errors = append(r.Errors, "台账没有任何字段行,却不是 state=bootstrap:台账可能被误删或清空;从版本库恢复,不要重新建基线")
		return r
	}
	if old.State == StateBootstrap && old.MaxGen() > 1 {
		r.Errors = append(r.Errors, fmt.Sprintf("台账是 state=bootstrap,却已有世代 %d 的字段:引导期只有世代 1,state 不能从 active 改回 bootstrap", old.MaxGen()))
		return r
	}
	newGen := old.MaxGen() + 1
	if old.State == StateBootstrap {
		newGen = 1
	}

	keys := make([]string, 0, len(s.Fields))
	for key := range s.Fields {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.Fields[keys[i]], s.Fields[keys[j]]
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		return a.Number < b.Number
	})

	addedIn := map[string]bool{}
	var added []Entry
	for _, key := range keys {
		cur := s.Fields[key]
		if retired[key] {
			r.Errors = append(r.Errors, fmt.Sprintf("%s(%s)复用了已退役的字段号;上线后字段号不得复用(AGENTS §4.3),换一个新号", key, cur.Name))
			continue
		}
		if prev, ok := recorded[key]; ok {
			if prev.Field.Label != cur.Label || prev.Field.Type != cur.Type || prev.Field.Oneof != cur.Oneof {
				r.Errors = append(r.Errors, fmt.Sprintf("%s(%s)的定义由「%s」改成了「%s」;持久字段不得改标签、类型或 oneof 归属(R3),"+
					"请写 reserved 后用新字段号承载新定义", key, cur.Name, describe(prev.Field), describe(cur)))
			}
			if prev.Field.Name != cur.Name {
				r.Notes = append(r.Notes, fmt.Sprintf("%s 改名 %s → %s;线格式不变,台账保留原名", key, prev.Field.Name, cur.Name))
			}
			// 存储分组:记下之后不得再改。玩家存储按分组拆成多行,加载时只认字段的当前分组(以防两行各有一份时
			// 合并翻倍),所以换组后第一次载入会把旧分组那一行里的这个字段清掉,新分组那一行里还没有,
			// 第一次存盘就把它永久写丢(审计 N1)。换存储位置就是 R4 的字段迁移,只能走 expand / contract。
			switch {
			case prev.Field.HasGroup && !cur.HasGroup:
				r.Errors = append(r.Errors, fmt.Sprintf("%s(%s)的存储分组选项 OptionPersistGroup 被删掉了(台账记的是分组 %d);"+
					"玩家表顶层字段必须标分组,标过的分组不得改(审计 N1)", key, cur.Name, prev.Field.Group))
			case prev.Field.HasGroup && prev.Field.Group != cur.Group:
				r.Errors = append(r.Errors, fmt.Sprintf("%s(%s)的存储分组由 %d 改成了 %d;换组后第一次载入会把旧分组那一行里的这个字段清掉,"+
					"新分组那一行里还没有,第一次存盘就永久写丢(审计 N1)。换存储位置按 R4 走 expand / contract:在新分组加一个新字段,"+
					"新旧双写一个完整版本周期后再切读方,旧字段写 reserved 退役", key, cur.Name, prev.Field.Group, cur.Group))
			case !prev.Field.HasGroup && cur.HasGroup:
				groupBackfill[key] = cur.Group
			}
			continue
		}
		if !baseline {
			if s.Frozen[cur.Message] && messagesInLedger[cur.Message] {
				r.Errors = append(r.Errors, fmt.Sprintf("%s(%s)加在冻结类型 %s 上;冻结类型由手写代码重建、不携带未知字段,旧写方会把它抹掉(R1)。"+
					"把数据放进会被整份携带的类型(物品数据进 ItemComp,任务进度进 MissionListComp),"+
					"或先为该类型实现运行时携带并纳入往返一致性测试再解冻", key, cur.Name, cur.Message))
				continue
			}
			if cur.Oneof != "" && oneofRecorded(old.Entries, cur.Message, cur.Oneof) {
				r.Errors = append(r.Errors, fmt.Sprintf("%s(%s)给已有的持久 oneof %s.%s 加了新成员;旧写方设置别的成员时会与携带回来的新成员冲突(R2)。"+
					"把字段放到 oneof 之外,或走破坏性变更流程", key, cur.Name, cur.Message, cur.Oneof))
				continue
			}
		}
		added = append(added, Entry{Gen: newGen, Field: cur})
		addedIn[cur.Message] = true
	}

	retiredIn := map[string]bool{}
	for _, e := range old.Entries {
		key := e.Field.Key()
		if retired[key] {
			continue
		}
		if _, ok := s.Fields[key]; ok {
			continue
		}
		if !s.Reachable[e.Field.Message] {
			r.Notes = append(r.Notes, fmt.Sprintf("%s(%s)所在类型已不再从玩家表可达,台账行保留不动", key, e.Field.Name))
			continue
		}
		if !s.isReserved(e.Field.Message, e.Field.Number) {
			r.Errors = append(r.Errors, fmt.Sprintf("持久字段 %s(%s)被删除但没有写 reserved %d;上线后字段号不得复用,删字段必须写 reserved(AGENTS §4.3)",
				key, e.Field.Name, e.Field.Number))
			continue
		}
		r.Ledger.Retired = append(r.Ledger.Retired, key)
		retiredIn[e.Field.Message] = true
		r.Changed = true
	}

	// 已封存(已发布)的世代不能再增删字段:同一世代号的两个二进制认识的字段集合必须相同,否则只比世代号的校验
	// 全部失效,旧写方会静默丢掉新列(审计 r2-ps#1、r2-ps#10,设计 §3.1「封存」)。两种成因分开报:
	//   - 台账本身(生成之前)已经违反封存:只能是合并带进来同世代的行,或台账被手改,与本次 proto 改动无关;
	//   - 台账完好、加上本次新增行才违反:新行在 active 下拿的是比任何封存都大的新世代,所以只可能是
	//     引导期还在把新字段并进世代 1。
	// 同一根因会违反它之上的每一条封存,只报世代最低的那条。
	if violations := SealViolations(r.Ledger.Entries, old.Seals); len(violations) > 0 {
		r.Errors = append(r.Errors, sealViolationSummary(violations)+";台账本身已违反封存,多半是合并带进来一行同世代的字段(或台账被手改):"+
			"丢弃本分支在台账里新增的行,在合入后的主干上重新生成(新字段拿新世代);不要改封存行")
	} else if violations := SealViolations(append(append([]Entry(nil), r.Ledger.Entries...), added...), old.Seals); len(violations) > 0 {
		r.Errors = append(r.Errors, sealViolationSummary(violations)+";台账已经封存过(已发布),引导期不能再加字段:"+
			"按 docs/ops/release-checklist.md §6.2 关闭引导期、把 state 改为 active 之后重新生成,新字段会拿到新世代")
	}

	if len(r.Errors) > 0 {
		return r
	}
	for message := range addedIn {
		if retiredIn[message] {
			r.Notes = append(r.Notes, fmt.Sprintf("%s 在同一次生成里既删字段又加字段:如果是字段迁移(提升为列、拆分、换存储位置),"+
				"必须走 expand / contract(R4),不能直接搬", message))
		}
	}
	sort.Strings(r.Notes)
	// 台账里还没记存储分组的旧行(分组选项出现之前写下的)原地补记一次;此后它们和新行一样受换组检查约束。
	if len(groupBackfill) > 0 {
		keys := make([]string, 0, len(groupBackfill))
		for key, group := range groupBackfill {
			entry := &r.Ledger.Entries[recordedAt[key]]
			entry.Field.Group, entry.Field.HasGroup = group, true
			keys = append(keys, key)
		}
		sort.Strings(keys)
		r.Notes = append(r.Notes, fmt.Sprintf("首次记录 %d 个玩家表顶层字段的存储分组(此后不得再改):%s", len(keys), strings.Join(keys, ", ")))
		r.Changed = true
	}
	if len(added) > 0 {
		r.Ledger.Entries = append(r.Ledger.Entries, added...)
		r.Changed = true
	}
	r.Gen = r.Ledger.MaxGen()
	if r.Gen == 0 {
		r.Gen = 1
	}
	return r
}

func oneofRecorded(entries []Entry, message, oneof string) bool {
	for _, e := range entries {
		if e.Field.Message == message && e.Field.Oneof == oneof {
			return true
		}
	}
	return false
}

func describe(f Field) string {
	s := f.Label + " " + f.Type
	if f.Oneof != "" {
		s += " oneof=" + f.Oneof
	}
	return s
}

// sealViolationSummary 只保留世代最低的那条违反(SealViolations 按封存顺序、也就是世代递增返回),其余计数附在后面。
func sealViolationSummary(violations []string) string {
	if len(violations) == 1 {
		return violations[0]
	}
	return fmt.Sprintf("%s(另有 %d 条更高世代的封存记录因同一原因不成立)", violations[0], len(violations)-1)
}

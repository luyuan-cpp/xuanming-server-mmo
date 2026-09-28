package activity

// 帮会活动配表的启动期校验与运行期取行(06 §6.2.4;90 X-16:GuildRule 的 S6 三列只在这里校验)。
//
// 两条纪律沿用 logic/economy_config.go:
//
//  1. **启动期拒启而不是运行期兜底**:活动发的是帮贡、帮会资金与物品,表错了在运行期表现为
//     "同一档期资金发两次""物品被 scene 永久拒收""团圆 0 人在线也能领"这类不可逆的静默错误;
//     启动期拒绝的代价只是一次部署回滚。guild.go 在 svc.NewServiceContext 之后调用 ValidateTables,
//     出错 logx.Must。
//  2. **配表只存 id、用时现查**:table 包是 atomic 快照,热更整批换指针。运行期查不到行
//     = 配表被错误替换,调用方一律 fail-closed(写 RPC 回未开放或返回错误,不猜默认值)。

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"guild/internal/constants"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

// RuleRowID 是 GuildRule 的唯一规则行 id(与 logic 的 guildRuleRowID 同值;本包不能 import logic,
// 否则 logic → activity → logic 成环)。
const RuleRowID uint32 = 1

// 配表取值的合法区间。写成常量而不是字面量:错误文案与判据用同一组数,改区间时不会出现
// "判据改了、文案还说旧范围"(与 economy_config.go 同一口径)。
const (
	// activity_join_min_hours:上限 30 天。再大等于把老成员以外的人永久挡在活动外,多半是把分钟当小时填了。
	maxActivityJoinMinHours uint32 = 720
	// trial_invite_ttl_seconds:太短来不及点同意,太长被邀请人会被占在开不了战的房间里(B6b 读)。
	minTrialInviteTTLSeconds uint32 = 10
	maxTrialInviteTTLSeconds uint32 = 120
	// trial_invite_cooldown_seconds:同一发起人两次建房的最小间隔(B6b 读);0 = 不限。
	maxTrialInviteCooldownSeconds uint32 = 600
	// 历练队伍人数(含发起人):至少 2 人才算"组队";上限 5 与回合制战斗一方的站位数一致。
	minTrialTeamSize uint32 = 2
	maxTrialTeamSize uint32 = 5
	// personal_contribution:防误填超大值(多写几个 0 会让帮会商店形同虚设)。
	// 远离 uint64 溢出,事务里的 contribution_total <= MaxUint64 − delta 守卫因此永远不会误伤正常值。
	maxPersonalContribution uint64 = 1_000_000_000
	// guild_funds:与 GuildLevel.upgrade_cost_funds 的上限同量级(economy_config.go maxUpgradeCostFunds)。
	maxActivityGuildFunds uint64 = 1_000_000_000_000
)

// activityTables 是 validateRows 的全部输入。纯数据 + 查询函数,便于单测逐条造坏样例,
// 不必去动 table 包的全局快照。
type activityTables struct {
	rows []*tablepb.GuildActivityTable
	rule *tablepb.GuildRuleTable
	// levelExists:GuildLevel 是否有该 id(min_guild_level 是 GuildLevel.id)。
	levelExists func(id uint32) bool
	// rewardOf:按 id 取 Reward 行。
	rewardOf func(id uint32) (*tablepb.RewardTable, bool)
	// itemExists:Item 表是否有该物品。scene 发物前会查 Item 表,不存在就永久拒收。
	itemExists func(id uint32) bool
	// dungeonExists:Dungeon 表是否有该副本(历练开战用)。
	dungeonExists func(id uint32) bool
}

// ValidateTables 校验 GuildActivity 全表与 GuildRule 的活动列,失败即拒绝启动。
// 错误信息一律带表名、行 id、列名与实际值,策划看到启动日志要能直接定位到格子。
func ValidateTables() error {
	rule, _ := table.GuildRuleTableManagerInstance.FindById(RuleRowID)
	return validateRows(activityTables{
		rows:          table.GuildActivityTableManagerInstance.FindAll(),
		rule:          rule,
		levelExists:   table.GuildLevelTableManagerInstance.Exists,
		rewardOf:      table.RewardTableManagerInstance.FindById,
		itemExists:    table.ItemTableManagerInstance.Exists,
		dungeonExists: table.DungeonTableManagerInstance.Exists,
	})
}

// validateRows 是 ValidateTables 的纯函数部分。检查顺序:规则行 → 逐行 → 同类型时间窗不重叠。
func validateRows(t activityTables) error {
	if t.levelExists == nil || t.rewardOf == nil || t.itemExists == nil || t.dungeonExists == nil {
		// 只可能是接线错误;缺了哪张表都判不了对应的引用,不能放行。
		return errors.New("GuildActivity 校验缺少 GuildLevel / Reward / Item / Dungeon 表查询")
	}
	if err := validateRule(t.rule); err != nil {
		return err
	}
	seen := make(map[uint32]struct{}, len(t.rows))
	for i, row := range t.rows {
		if row == nil {
			return fmt.Errorf("GuildActivity 表第 %d 行为空", i+1)
		}
		id := row.GetId()
		// 0 是请求里 activity_id 的 proto 缺省值:客户端漏填字段时不能恰好命中一个真实活动。
		if id == 0 {
			return fmt.Errorf("GuildActivity 第 %d 行 id=0:0 是请求缺省值,不能对应真实活动", i+1)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("GuildActivity.id=%d 重复", id)
		}
		seen[id] = struct{}{}
		if err := validateRow(row, t); err != nil {
			return err
		}
	}
	return validateNoOverlap(t.rows)
}

// validateRule:06 §6.2.4 第 1 条 + GuildRule 第 7 列(团圆兜底阈值,B6a 读取,B2 未设区间)。
func validateRule(rule *tablepb.GuildRuleTable) error {
	if rule == nil {
		return fmt.Errorf("GuildRule 表缺少 id=%d 的规则行", RuleRowID)
	}
	if v := rule.GetActivityJoinMinHours(); v > maxActivityJoinMinHours {
		return fmt.Errorf("GuildRule[%d].activity_join_min_hours=%d 越界,应在 [0,%d]",
			RuleRowID, v, maxActivityJoinMinHours)
	}
	if v := rule.GetTrialInviteTtlSeconds(); v < minTrialInviteTTLSeconds || v > maxTrialInviteTTLSeconds {
		return fmt.Errorf("GuildRule[%d].trial_invite_ttl_seconds=%d 越界,应在 [%d,%d]",
			RuleRowID, v, minTrialInviteTTLSeconds, maxTrialInviteTTLSeconds)
	}
	if v := rule.GetTrialInviteCooldownSeconds(); v > maxTrialInviteCooldownSeconds {
		return fmt.Errorf("GuildRule[%d].trial_invite_cooldown_seconds=%d 越界,应在 [0,%d]",
			RuleRowID, v, maxTrialInviteCooldownSeconds)
	}
	// 超过成员上限的在线阈值永远凑不齐:团圆整期领不了,且没有任何报错。
	if v := rule.GetReunionMinOnlineMembers(); v > constants.MaxGuildMembersCap {
		return fmt.Errorf("GuildRule[%d].reunion_min_online_members=%d 越界,应在 [0,%d](帮会成员上限)",
			RuleRowID, v, constants.MaxGuildMembersCap)
	}
	return nil
}

// validateRow:06 §6.2.4 第 2–6 条。
func validateRow(row *tablepb.GuildActivityTable, t activityTables) error {
	id := row.GetId()
	if strings.TrimSpace(row.GetName()) == "" {
		return fmt.Errorf("GuildActivity[%d].name 为空:界面没有可显示的活动名", id)
	}
	typ := row.GetType()
	if typ != TypeLantern && typ != TypeReunion && typ != TypeTrial {
		return fmt.Errorf("GuildActivity[%d].type=%d 非法,只允许 %d(元宵灯会)/ %d(中秋团圆)/ %d(同道历练)",
			id, typ, TypeLantern, TypeReunion, TypeTrial)
	}
	if row.GetDailyLimit() < 1 {
		return fmt.Errorf("GuildActivity[%d].daily_limit=0:每人每游戏日至少可参与 1 次", id)
	}
	if err := validateWindow(row); err != nil {
		return err
	}
	if v := row.GetMinGuildLevel(); v < 1 || !t.levelExists(v) {
		return fmt.Errorf("GuildActivity[%d].min_guild_level=%d 非法:必须 >= 1 且是 GuildLevel 表里存在的等级", id, v)
	}
	if v := row.GetPersonalContribution(); v > maxPersonalContribution {
		return fmt.Errorf("GuildActivity[%d].personal_contribution=%d 超过上限 %d(防误填超大值)", id, v, maxPersonalContribution)
	}
	if v := row.GetGuildFunds(); v > maxActivityGuildFunds {
		return fmt.Errorf("GuildActivity[%d].guild_funds=%d 超过上限 %d(防误填超大值)", id, v, maxActivityGuildFunds)
	}
	if err := validateReward(row, t); err != nil {
		return err
	}

	switch typ {
	case TypeLantern, TypeReunion:
		// 副本与人数只对历练有意义;灯会 / 团圆填了非 0 多半是把历练那一行复制过来没清干净。
		if row.GetDungeonId() != 0 || row.GetTeamSizeMin() != 0 || row.GetTeamSizeMax() != 0 {
			return fmt.Errorf("GuildActivity[%d]:type=%d 的 dungeon_id / team_size_min / team_size_max 必须为 0(实际 %d / %d / %d)",
				id, typ, row.GetDungeonId(), row.GetTeamSizeMin(), row.GetTeamSizeMax())
		}
	}
	switch typ {
	case TypeLantern:
		// 阈值 0 = 第 0 个人就达标,资金在无人点灯时也"已达成",等于白送。
		if row.GetGuildThreshold() < 1 {
			return fmt.Errorf("GuildActivity[%d].guild_threshold=0:灯会点灯人次阈值至少为 1", id)
		}
	case TypeReunion:
		if row.GetGuildThreshold() < 1 && t.rule.GetReunionMinOnlineMembers() < 1 {
			return fmt.Errorf("GuildActivity[%d].guild_threshold 与 GuildRule[%d].reunion_min_online_members 不能同时为 0:"+
				"团圆会变成 0 人在线也能领", id, RuleRowID)
		}
		if v := row.GetGuildThreshold(); v > constants.MaxGuildMembersCap {
			return fmt.Errorf("GuildActivity[%d].guild_threshold=%d 超过帮会成员上限 %d:团圆在线人数永远凑不齐",
				id, v, constants.MaxGuildMembersCap)
		}
	case TypeTrial:
		if d := row.GetDungeonId(); d == 0 || !t.dungeonExists(d) {
			return fmt.Errorf("GuildActivity[%d].dungeon_id=%d 非法:历练必须填 Dungeon 表里存在的副本", id, d)
		}
		lo, hi := row.GetTeamSizeMin(), row.GetTeamSizeMax()
		if lo < minTrialTeamSize || lo > hi || hi > maxTrialTeamSize {
			return fmt.Errorf("GuildActivity[%d]:team_size_min=%d / team_size_max=%d 非法,须满足 %d <= min <= max <= %d",
				id, lo, hi, minTrialTeamSize, maxTrialTeamSize)
		}
		if row.GetGuildThreshold() < 1 {
			return fmt.Errorf("GuildActivity[%d].guild_threshold=0:历练每游戏日计资金胜场上限至少为 1", id)
		}
	}
	return nil
}

// validateWindow:档期必须是 0/0(常开,仅开发)或 0 < start < end,且都装得进 int64。
//
// 比 06 §6.2.4 第 2 条("同为 0 或 end > start")多拒一种:start = 0 而 end > 0。
// 那样的行会以 1970-01-01 为档期键、从"远古"一直开到 end,几乎只可能是漏填了开始时间;
// 放行它,帮会进度会跨多个本应独立的档期累计。
// int64 上限:GuildPeriodKey 要把 start 转成 time.UnixMilli(int64),超过会变成负数。
func validateWindow(row *tablepb.GuildActivityTable) error {
	id := row.GetId()
	start, end := row.GetStartAtMs(), row.GetEndAtMs()
	if start == 0 && end == 0 {
		return nil
	}
	if start > math.MaxInt64 || end > math.MaxInt64 {
		return fmt.Errorf("GuildActivity[%d]:start_at_ms=%d / end_at_ms=%d 超过 int64 上限", id, start, end)
	}
	if start == 0 || end <= start {
		return fmt.Errorf("GuildActivity[%d]:start_at_ms=%d / end_at_ms=%d 非法,须同为 0(常开)或 0 < start < end",
			id, start, end)
	}
	return nil
}

// validateReward:06 §6.2.4 第 3 条。reward_id = 0 表示不发物品,不查。
func validateReward(row *tablepb.GuildActivityTable, t activityTables) error {
	id, rewardID := row.GetId(), row.GetRewardId()
	if rewardID == 0 {
		return nil
	}
	bundle, err := buildRewardBundle(rewardID, t.rewardOf)
	if err != nil {
		return fmt.Errorf("GuildActivity[%d].reward_id=%d 非法:%w", id, rewardID, err)
	}
	// 引用了奖励却一件都不发:玩家看到奖励栏是空的,策划本意多半不是这样;不发物品请填 0。
	if bundle == nil {
		return fmt.Errorf("GuildActivity[%d].reward_id=%d 的奖励包为空(各槽数量全为 0);不发物品请填 0", id, rewardID)
	}
	items := bundle.GetItems()
	if len(items) > maxRewardItemKinds {
		return fmt.Errorf("GuildActivity[%d].reward_id=%d 合并后有 %d 种物品,超过资产通道单次发放上限 %d",
			id, rewardID, len(items), maxRewardItemKinds)
	}
	for _, it := range items {
		// buildRewardBundle 已跳过数量 0 的槽;仍显式判一次,防它的规则日后改动后悄悄放进 0 数量项
		// (scene 会把整包判成非法、永久拒收)。
		if it.GetCount() < 1 {
			return fmt.Errorf("GuildActivity[%d].reward_id=%d 物品 %d 数量为 0", id, rewardID, it.GetConfigId())
		}
		if !t.itemExists(it.GetConfigId()) {
			return fmt.Errorf("GuildActivity[%d].reward_id=%d 引用的物品 %d 在 Item 表里不存在(scene 会永久拒收)",
				id, rewardID, it.GetConfigId())
		}
	}
	return nil
}

// validateNoOverlap:06 §6.2.4 第 7 条 —— 同 type 且 enabled 的行,时间窗 [start,end) 两两不重叠;
// 0/0 视为全时段,与同类型任何其它启用行都算重叠。
//
// 为什么必须拒:同一时刻若有两行同类型都在开放,玩家可以在两行上各领一次个人奖,
// 帮会进度也分成两份各自达标、资金发两次。PickForWrite 只接受选中行是第二道闸,这里是第一道。
// 未启用的行不参与:策划可以先把下一期配好、enabled=false 放着。
func validateNoOverlap(rows []*tablepb.GuildActivityTable) error {
	byType := make(map[uint32][]*tablepb.GuildActivityTable, len(allTypes))
	for _, row := range rows {
		if row.GetEnabled() {
			byType[row.GetType()] = append(byType[row.GetType()], row)
		}
	}
	for _, typ := range allTypes {
		group := byType[typ]
		// 按 id 排序只为报错信息稳定(小 id 在前),不随 FindAll 的顺序漂。
		sort.Slice(group, func(i, j int) bool { return group[i].GetId() < group[j].GetId() })
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				if windowsOverlap(group[i], group[j]) {
					return fmt.Errorf("GuildActivity[%d] 与 GuildActivity[%d](type=%d)同时启用且时间窗重叠"+
						"(0/0 常开行与同类型任何启用行都算重叠):同一时刻两行都能计进度,资金可能发两次",
						group[i].GetId(), group[j].GetId(), typ)
				}
			}
		}
	}
	return nil
}

// window 把 0/0 常开行展开成全时段,其余行原样返回 [start, end)。
func window(row *tablepb.GuildActivityTable) (start, end uint64) {
	start, end = row.GetStartAtMs(), row.GetEndAtMs()
	if start == 0 && end == 0 {
		return 0, math.MaxUint64
	}
	return start, end
}

// windowsOverlap:两个左闭右开区间是否有交集。首尾相接([a,b) 与 [b,c))不算重叠。
func windowsOverlap(a, b *tablepb.GuildActivityTable) bool {
	as, ae := window(a)
	bs, be := window(b)
	return as < be && bs < ae
}

// Row 按 id 取活动行;不存在返回 nil(调用方按"配置缺失"处理,例如历练结算记 CONFIG_MISSING)。
// 返回的行属于当时的配表快照,调用方只在本次请求内使用、不要长期持有(热更契约)。
func Row(id uint32) *tablepb.GuildActivityTable {
	row, ok := table.GuildActivityTableManagerInstance.FindById(id)
	if !ok {
		return nil
	}
	return row
}

// Rows 返回全部活动行,供 SelectVisible / PickForWrite 使用。
// 返回的是快照内部切片:只读,不得原地排序或修改(本包的函数都不改入参切片)。
func Rows() []*tablepb.GuildActivityTable {
	return table.GuildActivityTableManagerInstance.FindAll()
}

// Rule 取 GuildRule 规则行。ok=false = 配表缺行,调用方按故障处理(启动期 ValidateTables 已保证存在)。
func Rule() (*tablepb.GuildRuleTable, bool) {
	return table.GuildRuleTableManagerInstance.FindById(RuleRowID)
}

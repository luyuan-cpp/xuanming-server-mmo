// Package catalog 把三张配置表编译成匹配服务用的不可变目录
// （docs/design/global-match-hierarchical-pools.md §3.1、§7）：
//
//   - PlayMode：匹配入口白名单，一行 = 一个 (模式, 怪物战斗模板) 组合可以排队或开战；
//   - Dungeon：怪物战斗模板本身（怪物组、人数上限、战斗时长），只描述"打什么"；
//   - ZoneGroup：zone 号段 → 大区，匹配范围树的中间层。
//
// 目录在进程启动时建一次，之后只读；校验失败直接拒绝启动（fail-closed），
// 不带着坏表开放任何入口。入队校验、按池撮合与提交点复核都只读这一份目录。
package catalog

import (
	"errors"
	"fmt"
	"sort"

	"match/internal/config"
	"match/internal/scope"

	battlepb "proto/battle"
	matchpb "proto/match"
	"shared/generated/table"

	"google.golang.org/protobuf/proto"
)

// DefaultBattleSeconds 是模板没有填 time_limit 时的战斗时长。
// 与提交点的备战预算（authority.WorkLeaseMs，120 秒）相加正好是旧的固定期限 300 秒。
const DefaultBattleSeconds = 180

// MaxBattleSeconds 是单局战斗时长上限（负责人 2026-10-24 拍板 D6）。
const MaxBattleSeconds = 3600

// 回合制引擎按模板时限定回合上限的换算，镜像 cpp/libs/services/battle/constants/turn_battle_constants.h
// （TurnBattleEngine::Initialize）：maxRounds = max(1, ceil(time_limit × 1000 / kRoundDurationMs))，time_limit 为 0
// 或模板不在表里取 kDefaultMaxRounds。两边改动必须同步，TestRoundLimitMirrorsEngine 钉住这几个值。
const (
	EngineRoundDurationMs   = 6000 // turnbattle::kRoundDurationMs
	EngineDefaultRoundLimit = 300  // turnbattle::kDefaultMaxRounds
)

// DungeonRow 是 Dungeon 表里匹配用得到的部分。
type DungeonRow struct {
	Id               uint32
	MaxTeamSize      uint32
	TimeLimitSeconds uint32
	RoundLimit       uint32
	MonsterCount     int
	BossTrialKind    uint32
	Difficulty       uint32
	GuardMonsterId   uint32
}

// TrialJournalEntryRow 是 TrialJournalEntry 表里匹配建房冻结 trial_context 用得到的部分。
type TrialJournalEntryRow struct {
	Id          uint32
	SourceKind  uint32
	Difficulty  uint32
	VariantKey  string
	RuleVersion uint32
	SeasonId    uint32
	Enabled     bool
}

// PlayModeRow 是 PlayMode 表的一行。
type PlayModeRow struct {
	Id               uint32
	Mode             matchpb.MatchMode
	ConfigId         uint32
	Enabled          bool
	TeamSizeMax      uint32
	AllowedTeamSizes []uint32
}

// Rows 是建目录的全部输入。生产由 Load 从导表产物读出，单测直接构造。
type Rows struct {
	Dungeons     []DungeonRow
	PlayModes    []PlayModeRow
	ZoneGroups   []scope.Range
	TrialEntries []TrialJournalEntryRow
}

// Pool 是一个匹配池：同一池里的票才可能进同一局。TeamSize 与票据 team_size 同口径：
// PVE_SOLO 为 1，PVE_TEAM 为同场人数，1V1 为 2，5V5 为 10，PVP_TEAM 为每侧人数。
type Pool struct {
	Mode     matchpb.MatchMode
	ConfigId uint32
	TeamSize uint32
}

// Players 返回这个池一局的总人数。
func (p Pool) Players() int {
	if p.Mode == matchpb.MatchMode_MATCH_MODE_PVP_TEAM {
		return int(p.TeamSize) * 2
	}
	return int(p.TeamSize)
}

type templateKey struct {
	mode     matchpb.MatchMode
	configId uint32
}

// Catalog 是编译后的只读目录。nil 目录只出现在手工构造 ServiceContext 的单测里：
// 此时没有白名单与大区，期限取默认值；生产启动时目录缺失会直接拒绝启动。
type Catalog struct {
	maxSize        map[templateKey]uint32          // 只含 enabled 的入口
	allowedSizes   map[templateKey]map[uint32]bool // 非空时仅开放列明的人数档（冷启动挡位开关）
	dungeons       map[uint32]DungeonRow
	trialContexts  map[uint32]*battlepb.TrialBattleContext // Dungeon.id → 冻结试炼志上下文（卷一/卷二/护卫救援/物法易势）
	rankedTrialCtx *battlepb.TrialBattleContext            // 正式人宠会武（PVP_TEAM）赛季试炼上下文
	zones          *scope.Directory
	pools          []Pool
}

// Load 从导表产物目录读匹配目录依赖的配置表并建目录。
func Load(tableDir string) (*Catalog, error) {
	if err := table.DungeonTableManagerInstance.Load(tableDir, false); err != nil {
		return nil, fmt.Errorf("catalog: 读取 Dungeon 表失败: %w", err)
	}
	if err := table.PlayModeTableManagerInstance.Load(tableDir, false); err != nil {
		return nil, fmt.Errorf("catalog: 读取 PlayMode 表失败: %w", err)
	}
	if err := table.ZoneGroupTableManagerInstance.Load(tableDir, false); err != nil {
		return nil, fmt.Errorf("catalog: 读取 ZoneGroup 表失败: %w", err)
	}
	if err := table.MonsterTableManagerInstance.Load(tableDir, false); err != nil {
		return nil, fmt.Errorf("catalog: 读取 Monster 表失败: %w", err)
	}
	if err := table.TrialJournalEntryTableManagerInstance.Load(tableDir, false); err != nil {
		return nil, fmt.Errorf("catalog: 读取 TrialJournalEntry 表失败: %w", err)
	}
	return buildFromManagers(
		table.DungeonTableManagerInstance,
		table.PlayModeTableManagerInstance,
		table.ZoneGroupTableManagerInstance,
		table.MonsterTableManagerInstance,
		table.TrialJournalEntryTableManagerInstance,
	)
}

// BuildFromTableSet 从候选 TableSet 编译不可变目录,不触碰全局表单例(两阶段热更与 --validate-tables 专用)。
func BuildFromTableSet(ts *table.TableSet) (*Catalog, error) {
	if ts == nil || ts.Dungeon == nil || ts.PlayMode == nil || ts.ZoneGroup == nil {
		return nil, errors.New("catalog: TableSet missing Dungeon/PlayMode/ZoneGroup tables")
	}
	return buildFromManagers(ts.Dungeon, ts.PlayMode, ts.ZoneGroup, ts.Monster, ts.TrialJournalEntry)
}

func buildFromManagers(
	dungeons *table.DungeonTableManager,
	playModes *table.PlayModeTableManager,
	zoneGroups *table.ZoneGroupTableManager,
	monsters *table.MonsterTableManager,
	trialEntries *table.TrialJournalEntryTableManager,
) (*Catalog, error) {
	var rows Rows
	for _, row := range dungeons.FindAll() {
		monsterCount := 0
		var guardMonsterId uint32
		for _, monsterId := range row.GetMonster() {
			if monsterId == 0 {
				continue
			}
			monsterCount++
			if guardMonsterId == 0 && monsters != nil {
				if m, ok := monsters.FindById(monsterId); ok && m.GetIsBoss() == 0 {
					guardMonsterId = monsterId
				}
			}
		}
		rows.Dungeons = append(rows.Dungeons, DungeonRow{
			Id:               row.GetId(),
			MaxTeamSize:      row.GetMaxTeamSize(),
			TimeLimitSeconds: row.GetTimeLimit(),
			RoundLimit:       row.GetRoundLimit(),
			MonsterCount:     monsterCount,
			BossTrialKind:    row.GetBossTrialKind(),
			Difficulty:       row.GetDifficulty(),
			GuardMonsterId:   guardMonsterId,
		})
	}
	for _, row := range playModes.FindAll() {
		var allowed []uint32
		for _, size := range row.GetAllowedTeamSizes() {
			if size != 0 {
				allowed = append(allowed, size)
			}
		}
		rows.PlayModes = append(rows.PlayModes, PlayModeRow{
			Id:               row.GetId(),
			Mode:             matchpb.MatchMode(row.GetMatchMode()),
			ConfigId:         row.GetBattleConfigId(),
			Enabled:          row.GetEnabled(),
			TeamSizeMax:      row.GetTeamSizeMax(),
			AllowedTeamSizes: allowed,
		})
	}
	for _, row := range zoneGroups.FindAll() {
		rows.ZoneGroups = append(rows.ZoneGroups, scope.Range{GroupID: row.GetGroupId(), ZoneBegin: row.GetZoneBegin(), ZoneEnd: row.GetZoneEnd()})
	}
	if trialEntries != nil {
		for _, row := range trialEntries.FindAll() {
			rows.TrialEntries = append(rows.TrialEntries, TrialJournalEntryRow{
				Id:          row.GetId(),
				SourceKind:  row.GetSourceKind(),
				Difficulty:  row.GetDifficulty(),
				VariantKey:  row.GetVariantKey(),
				RuleVersion: row.GetRuleVersion(),
				SeasonId:    row.GetSeasonId(),
				Enabled:     row.GetEnabled(),
			})
		}
	}
	return New(rows)
}

// New 校验并编译目录。任何一行不合法都整体拒绝，不部分开放。
func New(rows Rows) (*Catalog, error) {
	c := &Catalog{
		maxSize:       map[templateKey]uint32{},
		allowedSizes:  map[templateKey]map[uint32]bool{},
		dungeons:      map[uint32]DungeonRow{},
		trialContexts: map[uint32]*battlepb.TrialBattleContext{},
	}
	for _, d := range rows.Dungeons {
		if _, dup := c.dungeons[d.Id]; dup || d.Id == 0 {
			return nil, fmt.Errorf("catalog: Dungeon 行 id=%d 重复或为 0", d.Id)
		}
		c.dungeons[d.Id] = d
	}
	trialContexts, rankedTrialCtx, err := buildTrialContexts(c.dungeons, rows.TrialEntries)
	if err != nil {
		return nil, err
	}
	c.trialContexts = trialContexts
	c.rankedTrialCtx = rankedTrialCtx
	seen := map[templateKey]uint32{}
	for _, row := range rows.PlayModes {
		key := templateKey{mode: row.Mode, configId: row.ConfigId}
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("catalog: PlayMode 行 %d 与 %d 的 (模式, 模板) 重复", prev, row.Id)
		}
		seen[key] = row.Id
		if err := c.validate(row); err != nil {
			return nil, fmt.Errorf("catalog: PlayMode 行 %d: %w", row.Id, err)
		}
		if row.Enabled {
			c.maxSize[key] = row.TeamSizeMax
			if len(row.AllowedTeamSizes) > 0 {
				allowed := make(map[uint32]bool, len(row.AllowedTeamSizes))
				for _, size := range row.AllowedTeamSizes {
					allowed[size] = true
				}
				c.allowedSizes[key] = allowed
			}
		}
	}
	zones, err := scope.NewDirectory(rows.ZoneGroups)
	if err != nil {
		return nil, err
	}
	c.zones = zones
	c.pools = buildPools(c.maxSize, c.allowedSizes)
	return c, nil
}

func (c *Catalog) validate(row PlayModeRow) error {
	dungeon, hasDungeon := c.dungeons[row.ConfigId]
	if row.ConfigId != 0 && !hasDungeon {
		return fmt.Errorf("模板 %d 不在 Dungeon 表", row.ConfigId)
	}
	// 瑞兽迎春(boss_trial_kind=7)仅允许经 scene -> MatchInternal.StartActivityBattle 准入，禁止进普通 PlayMode 队列。
	if hasDungeon && dungeon.BossTrialKind == 7 {
		return fmt.Errorf("瑞兽迎春模板 %d 禁止加入普通 PlayMode 白名单", row.ConfigId)
	}
	switch row.Mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO, matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		if !hasDungeon || dungeon.MonsterCount == 0 {
			return errors.New("PVE 入口必须引用有怪物组的 Dungeon 行")
		}
		if row.Mode == matchpb.MatchMode_MATCH_MODE_PVE_SOLO {
			if row.TeamSizeMax != 1 {
				return errors.New("PVE_SOLO 的 team_size_max 必须为 1")
			}
			if len(row.AllowedTeamSizes) > 0 {
				return errors.New("PVE_SOLO 不允许配置 allowed_team_sizes")
			}
		}
		if row.Mode == matchpb.MatchMode_MATCH_MODE_PVE_TEAM && (row.TeamSizeMax == 0 || row.TeamSizeMax > min(dungeon.MaxTeamSize, config.TeamCapacity)) {
			return fmt.Errorf("PVE_TEAM 人数上限 %d 须在 1..min(Dungeon.max_team_size=%d, %d)", row.TeamSizeMax, dungeon.MaxTeamSize, config.TeamCapacity)
		}
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		if row.TeamSizeMax == 0 || row.TeamSizeMax > config.TeamCapacity {
			return fmt.Errorf("PVP_TEAM 每侧人数上限 %d 须在 1..%d", row.TeamSizeMax, config.TeamCapacity)
		}
	case matchpb.MatchMode_MATCH_MODE_1V1, matchpb.MatchMode_MATCH_MODE_5V5, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, matchpb.MatchMode_MATCH_MODE_POLARITY_5V5:
		if row.TeamSizeMax != 0 {
			return errors.New("1V1 / 5V5 / 切磋 / 物法易势的人数由模式决定，team_size_max 必须为 0")
		}
		if len(row.AllowedTeamSizes) > 0 {
			return errors.New("固定人数模式不允许配置 allowed_team_sizes")
		}
	default:
		return fmt.Errorf("不支持的匹配模式 %d", uint32(row.Mode))
	}
	if len(row.AllowedTeamSizes) > 0 {
		seenSizes := make(map[uint32]bool, len(row.AllowedTeamSizes))
		for _, size := range row.AllowedTeamSizes {
			if size == 0 || size > row.TeamSizeMax {
				return fmt.Errorf("allowed_team_sizes 项 %d 超出 1..team_size_max(%d)", size, row.TeamSizeMax)
			}
			if seenSizes[size] {
				return fmt.Errorf("allowed_team_sizes 含重复人数档 %d", size)
			}
			seenSizes[size] = true
		}
	}
	return nil
}

// buildPools 枚举全部开放入口的匹配池，按 (模式, 模板, 人数) 排序，结果确定。
func buildPools(maxSize map[templateKey]uint32, allowedSizes map[templateKey]map[uint32]bool) []Pool {
	var pools []Pool
	for key, limit := range maxSize {
		switch key.mode {
		case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
			pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: 1})
		case matchpb.MatchMode_MATCH_MODE_PVE_TEAM, matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
			allowed := allowedSizes[key]
			for size := uint32(1); size <= limit; size++ {
				if len(allowed) > 0 && !allowed[size] {
					continue
				}
				pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: size})
			}
		case matchpb.MatchMode_MATCH_MODE_1V1:
			pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: 2})
		case matchpb.MatchMode_MATCH_MODE_5V5, matchpb.MatchMode_MATCH_MODE_POLARITY_5V5:
			pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: uint32(config.TeamCapacity * 2)})
		}
		// 切磋点名成局，不排队，没有池。
	}
	sort.Slice(pools, func(i, j int) bool {
		a, b := pools[i], pools[j]
		if a.Mode != b.Mode {
			return a.Mode < b.Mode
		}
		if a.ConfigId != b.ConfigId {
			return a.ConfigId < b.ConfigId
		}
		return a.TeamSize < b.TeamSize
	})
	return pools
}

// Open 报告 (模式, 模板) 是否是开放的入口。nil 目录（单测）视为全部开放。
func (c *Catalog) Open(mode matchpb.MatchMode, configId uint32) bool {
	if c == nil {
		return true
	}
	_, ok := c.maxSize[templateKey{mode: mode, configId: configId}]
	return ok
}

// MaxTeamSize 返回入口的人数上限：PVE_SOLO 为 1，PVE_TEAM 为同场人数上限，PVP_TEAM 为每侧人数上限，
// 1V1 / 5V5 / 切磋 / 物法易势为 0（人数由模式决定）。未开放或 nil 目录返回 0。
func (c *Catalog) MaxTeamSize(mode matchpb.MatchMode, configId uint32) uint32 {
	if c == nil {
		return 0
	}
	return c.maxSize[templateKey{mode: mode, configId: configId}]
}

// AllowedTeamSize 报告 (模式, 模板) 是否开放指定的 teamSize 人数档。
// 未配白名单时退回 1..team_size_max 全开；配置了白名单时只允许列明的档位（如冷启动期仅开放 1V1 与 3V3）。
// nil 目录（单测）视为放行。
func (c *Catalog) AllowedTeamSize(mode matchpb.MatchMode, configId, teamSize uint32) bool {
	if c == nil {
		return true
	}
	key := templateKey{mode: mode, configId: configId}
	limit, ok := c.maxSize[key]
	if !ok || teamSize == 0 || teamSize > limit {
		return false
	}
	if allowed := c.allowedSizes[key]; len(allowed) > 0 {
		return allowed[teamSize]
	}
	return true
}

// AllowedTeamSizes 返回 (模式, 模板) 当前全部开放的每侧/同场人数档（升序）。
// 配置了 allowed_team_sizes 白名单时仅返回白名单，未配置时返回 1..team_size_max。
func (c *Catalog) AllowedTeamSizes(mode matchpb.MatchMode, configId uint32) []uint32 {
	if c == nil {
		return nil
	}
	key := templateKey{mode: mode, configId: configId}
	limit, ok := c.maxSize[key]
	if !ok || limit == 0 {
		return nil
	}
	allowed := c.allowedSizes[key]
	out := make([]uint32, 0, limit)
	for size := uint32(1); size <= limit; size++ {
		if len(allowed) > 0 && !allowed[size] {
			continue
		}
		out = append(out, size)
	}
	return out
}

// BossTrialKind 返回 Dungeon 表中该模板的 boss_trial_kind（0=常规，1..4/8=首领试炼，5=护卫救援，6=物法易势，7=瑞兽迎春）。
func (c *Catalog) BossTrialKind(configId uint32) uint32 {
	if c == nil {
		return 0
	}
	return c.dungeons[configId].BossTrialKind
}

// Pools 返回全部开放入口的匹配池（只读，调用方不得修改）。
func (c *Catalog) Pools() []Pool {
	if c == nil {
		return nil
	}
	return c.pools
}

// ZoneNode 返回 home zone 在范围树上的位置；nil 目录下所有 zone 都未分大区。
// 调用方传解析合服之后的最终区（票上存的是原始区）。
func (c *Catalog) ZoneNode(zone uint32) scope.Node {
	if c == nil {
		return scope.Node{Zone: zone}
	}
	return c.zones.NodeOf(zone)
}

// BattleSeconds 返回模板的战斗时长：Dungeon.time_limit，未填或模板不在表里取 DefaultBattleSeconds，
// 不超过 MaxBattleSeconds。成局期限 = 提交时刻 + 备战预算 + 本值。
func (c *Catalog) BattleSeconds(configId uint32) uint32 {
	if c == nil {
		return DefaultBattleSeconds
	}
	dungeon, ok := c.dungeons[configId]
	if !ok || dungeon.TimeLimitSeconds == 0 {
		return DefaultBattleSeconds
	}
	return min(dungeon.TimeLimitSeconds, MaxBattleSeconds)
}

// RoundLimit 返回引擎给这个模板定的回合上限，与引擎同一换算（见 EngineRoundDurationMs）。回合打满时引擎判
// 进攻方（0 队）负，计分局把它改判平局（logic.ratedScoreA）。
// 它和 BattleSeconds 是两个量：BattleSeconds 是 match 给的整场期限（缺省 180 秒、截断到 MaxBattleSeconds），
// 期限打满由 battle 直接判平；回合上限只看表里的 time_limit，不截断、缺省 300 回合。
// nil 目录（单测）与不在表里的模板（含切磋的模板 0）取引擎缺省。
func (c *Catalog) RoundLimit(configId uint32) uint32 {
	if c == nil {
		return EngineDefaultRoundLimit
	}
	dungeon, ok := c.dungeons[configId]
	if !ok {
		return EngineDefaultRoundLimit
	}
	if dungeon.RoundLimit > 0 {
		return dungeon.RoundLimit
	}
	if dungeon.TimeLimitSeconds == 0 {
		return EngineDefaultRoundLimit
	}
	// time_limit ≥ 1 秒时向上取整至少是 1 回合，引擎里的 max(1, …) 在这里自然成立。
	return uint32((uint64(dungeon.TimeLimitSeconds)*1000 + EngineRoundDurationMs - 1) / EngineRoundDurationMs)
}

type trialVariantKey struct {
	sourceKind uint32
	difficulty uint32
	variantKey string
}

// bossTrialVariant 将 Dungeon.boss_trial_kind 映射到 TrialJournalEntry.(source_kind, variant_key)。
func bossTrialVariant(bossTrialKind uint32) (uint32, string, bool) {
	switch bossTrialKind {
	case 1:
		return 1, "rejuvenation", true
	case 2:
		return 1, "bulwark", true
	case 3:
		return 1, "barrier", true
	case 4:
		return 1, "venom", true
	case 5:
		return 2, "rescue", true
	case 6:
		return 3, "polarity_5v5", true
	case 8:
		return 1, "dual_guard", true
	default:
		return 0, "", false
	}
}

func buildTrialContexts(dungeons map[uint32]DungeonRow, entries []TrialJournalEntryRow) (map[uint32]*battlepb.TrialBattleContext, *battlepb.TrialBattleContext, error) {
	byVariant := make(map[trialVariantKey]TrialJournalEntryRow, len(entries))
	for _, entry := range entries {
		if !entry.Enabled {
			continue
		}
		if entry.Id == 0 || entry.RuleVersion == 0 {
			return nil, nil, fmt.Errorf("catalog: TrialJournalEntry 行 id=%d 缺少有效 id 或 rule_version", entry.Id)
		}
		key := trialVariantKey{sourceKind: entry.SourceKind, difficulty: entry.Difficulty, variantKey: entry.VariantKey}
		if prev, dup := byVariant[key]; dup {
			return nil, nil, fmt.Errorf("catalog: TrialJournalEntry 行 %d 与 %d 的 (source_kind=%d, difficulty=%d, variant_key=%q) 重复",
				prev.Id, entry.Id, entry.SourceKind, entry.Difficulty, entry.VariantKey)
		}
		byVariant[key] = entry
	}
	out := make(map[uint32]*battlepb.TrialBattleContext)
	for _, d := range dungeons {
		sourceKind, variantKey, ok := bossTrialVariant(d.BossTrialKind)
		if !ok {
			continue
		}
		entry, matched := byVariant[trialVariantKey{sourceKind: sourceKind, difficulty: d.Difficulty, variantKey: variantKey}]
		if !matched {
			continue
		}
		out[d.Id] = &battlepb.TrialBattleContext{
			JournalEntryId:      entry.Id,
			TrialRuleVersion:    entry.RuleVersion,
			SeasonId:            entry.SeasonId,
			GuardMonsterTableId: d.GuardMonsterId,
		}
	}
	var rankedCtx *battlepb.TrialBattleContext
	if entry, ok := byVariant[trialVariantKey{sourceKind: 4, difficulty: 0, variantKey: "ranked_pvp"}]; ok {
		rankedCtx = &battlepb.TrialBattleContext{
			JournalEntryId:   entry.Id,
			TrialRuleVersion: entry.RuleVersion,
			SeasonId:         entry.SeasonId,
		}
	}
	return out, rankedCtx, nil
}

// TrialContextFor 返回本局应冻结进 CreateBattleRequest.trial_context 的试炼志上下文。
// 覆盖：卷一/卷二首领试炼与卷三护卫救援（PVE_SOLO/PVE_TEAM）、卷三物法易势（POLARITY_5V5）与卷三人宠会武（PVP_TEAM）。
// 帮会活动战、切磋与未配试炼志格子的普通副本一律返回 nil。
func (c *Catalog) TrialContextFor(
	mode matchpb.MatchMode,
	configId uint32,
	activityContext *battlepb.BattleActivityContext,
	rankedCtx *battlepb.RankedMatchContext,
) *battlepb.TrialBattleContext {
	if c == nil || activityContext != nil {
		return nil
	}
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, matchpb.MatchMode_MATCH_MODE_POLARITY_5V5:
		// 仅排斥带正式排位赛季的上下文；免费预设娱乐场（SeasonId==0, UseFreePreset==true）须保留卷三护卫救援(301)/物法易势(302)试炼志上下文。
		if rankedCtx != nil && rankedCtx.GetSeasonId() != 0 {
			return nil
		}
		ctx, ok := c.trialContexts[configId]
		if !ok || ctx == nil {
			return nil
		}
		return proto.Clone(ctx).(*battlepb.TrialBattleContext)
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		if c.rankedTrialCtx == nil {
			return nil
		}
		if rankedCtx != nil && rankedCtx.GetSeasonId() != 0 &&
			c.rankedTrialCtx.GetSeasonId() != 0 && c.rankedTrialCtx.GetSeasonId() != rankedCtx.GetSeasonId() {
			return nil
		}
		return proto.Clone(c.rankedTrialCtx).(*battlepb.TrialBattleContext)
	default:
		return nil
	}
}

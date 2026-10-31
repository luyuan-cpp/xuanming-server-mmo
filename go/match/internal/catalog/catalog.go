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

	matchpb "proto/match"
	"shared/generated/table"
)

// DefaultBattleSeconds 是模板没有填 time_limit 时的战斗时长。
// 与提交点的备战预算（authority.WorkLeaseMs，120 秒）相加正好是旧的固定期限 300 秒。
const DefaultBattleSeconds = 180

// MaxBattleSeconds 是单局战斗时长上限（负责人 2026-10-24 拍板 D6）。
const MaxBattleSeconds = 3600

// DungeonRow 是 Dungeon 表里匹配用得到的部分。
type DungeonRow struct {
	Id               uint32
	MaxTeamSize      uint32
	TimeLimitSeconds uint32
	MonsterCount     int
}

// PlayModeRow 是 PlayMode 表的一行。
type PlayModeRow struct {
	Id          uint32
	Mode        matchpb.MatchMode
	ConfigId    uint32
	Enabled     bool
	TeamSizeMax uint32
}

// Rows 是建目录的全部输入。生产由 Load 从导表产物读出，单测直接构造。
type Rows struct {
	Dungeons   []DungeonRow
	PlayModes  []PlayModeRow
	ZoneGroups []scope.Range
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

// OwnerKey 是池归属的哈希键：同一模板的各人数档落在同一个写者上，
// 以后做跨档合池不用跨副本协调。
func (p Pool) OwnerKey() string {
	return OwnerKey(p.Mode, p.ConfigId)
}

// OwnerKey 是 (模式, 模板) 的归属键，Pending 恢复按同一键分片。
func OwnerKey(mode matchpb.MatchMode, configId uint32) string {
	return fmt.Sprintf("%d:%d", uint32(mode), configId)
}

type templateKey struct {
	mode     matchpb.MatchMode
	configId uint32
}

// Catalog 是编译后的只读目录。nil 目录只出现在手工构造 ServiceContext 的单测里：
// 此时没有白名单与大区，期限取默认值；生产启动时目录缺失会直接拒绝启动。
type Catalog struct {
	maxSize  map[templateKey]uint32 // 只含 enabled 的入口
	dungeons map[uint32]DungeonRow
	zones    *scope.Directory
	pools    []Pool
}

// Load 从导表产物目录读三张表并建目录。只加载这三张，不连带加载与匹配无关的表。
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
	var rows Rows
	for _, row := range table.DungeonTableManagerInstance.FindAll() {
		monsters := 0
		for _, monster := range row.GetMonster() {
			if monster != 0 {
				monsters++
			}
		}
		rows.Dungeons = append(rows.Dungeons, DungeonRow{Id: row.GetId(), MaxTeamSize: row.GetMaxTeamSize(), TimeLimitSeconds: row.GetTimeLimit(), MonsterCount: monsters})
	}
	for _, row := range table.PlayModeTableManagerInstance.FindAll() {
		rows.PlayModes = append(rows.PlayModes, PlayModeRow{Id: row.GetId(), Mode: matchpb.MatchMode(row.GetMatchMode()), ConfigId: row.GetBattleConfigId(), Enabled: row.GetEnabled(), TeamSizeMax: row.GetTeamSizeMax()})
	}
	for _, row := range table.ZoneGroupTableManagerInstance.FindAll() {
		rows.ZoneGroups = append(rows.ZoneGroups, scope.Range{GroupID: row.GetGroupId(), ZoneBegin: row.GetZoneBegin(), ZoneEnd: row.GetZoneEnd()})
	}
	return New(rows)
}

// New 校验并编译目录。任何一行不合法都整体拒绝，不部分开放。
func New(rows Rows) (*Catalog, error) {
	c := &Catalog{maxSize: map[templateKey]uint32{}, dungeons: map[uint32]DungeonRow{}}
	for _, d := range rows.Dungeons {
		if _, dup := c.dungeons[d.Id]; dup || d.Id == 0 {
			return nil, fmt.Errorf("catalog: Dungeon 行 id=%d 重复或为 0", d.Id)
		}
		c.dungeons[d.Id] = d
	}
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
		}
	}
	zones, err := scope.NewDirectory(rows.ZoneGroups)
	if err != nil {
		return nil, err
	}
	c.zones = zones
	c.pools = buildPools(c.maxSize)
	return c, nil
}

func (c *Catalog) validate(row PlayModeRow) error {
	dungeon, hasDungeon := c.dungeons[row.ConfigId]
	if row.ConfigId != 0 && !hasDungeon {
		return fmt.Errorf("模板 %d 不在 Dungeon 表", row.ConfigId)
	}
	switch row.Mode {
	case matchpb.MatchMode_MATCH_MODE_PVE_SOLO, matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		if !hasDungeon || dungeon.MonsterCount == 0 {
			return errors.New("PVE 入口必须引用有怪物组的 Dungeon 行")
		}
		if row.Mode == matchpb.MatchMode_MATCH_MODE_PVE_SOLO && row.TeamSizeMax != 1 {
			return errors.New("PVE_SOLO 的 team_size_max 必须为 1")
		}
		if row.Mode == matchpb.MatchMode_MATCH_MODE_PVE_TEAM && (row.TeamSizeMax == 0 || row.TeamSizeMax > min(dungeon.MaxTeamSize, config.TeamCapacity)) {
			return fmt.Errorf("PVE_TEAM 人数上限 %d 须在 1..min(Dungeon.max_team_size=%d, %d)", row.TeamSizeMax, dungeon.MaxTeamSize, config.TeamCapacity)
		}
	case matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
		if row.TeamSizeMax == 0 || row.TeamSizeMax > config.TeamCapacity {
			return fmt.Errorf("PVP_TEAM 每侧人数上限 %d 须在 1..%d", row.TeamSizeMax, config.TeamCapacity)
		}
	case matchpb.MatchMode_MATCH_MODE_1V1, matchpb.MatchMode_MATCH_MODE_5V5, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE:
		if row.TeamSizeMax != 0 {
			return errors.New("1V1 / 5V5 / 切磋的人数由模式决定，team_size_max 必须为 0")
		}
	default:
		return fmt.Errorf("不支持的匹配模式 %d", uint32(row.Mode))
	}
	return nil
}

// buildPools 枚举全部开放入口的匹配池，按 (模式, 模板, 人数) 排序，结果确定。
func buildPools(maxSize map[templateKey]uint32) []Pool {
	var pools []Pool
	for key, limit := range maxSize {
		switch key.mode {
		case matchpb.MatchMode_MATCH_MODE_PVE_SOLO:
			pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: 1})
		case matchpb.MatchMode_MATCH_MODE_PVE_TEAM, matchpb.MatchMode_MATCH_MODE_PVP_TEAM:
			for size := uint32(1); size <= limit; size++ {
				pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: size})
			}
		case matchpb.MatchMode_MATCH_MODE_1V1:
			pools = append(pools, Pool{Mode: key.mode, ConfigId: key.configId, TeamSize: 2})
		case matchpb.MatchMode_MATCH_MODE_5V5:
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
// 1V1 / 5V5 / 切磋为 0（人数由模式决定）。未开放或 nil 目录返回 0。
func (c *Catalog) MaxTeamSize(mode matchpb.MatchMode, configId uint32) uint32 {
	if c == nil {
		return 0
	}
	return c.maxSize[templateKey{mode: mode, configId: configId}]
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

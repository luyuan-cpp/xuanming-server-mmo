package battlescript

import (
	_ "embed"
	"fmt"
	"math"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"
	"proto/battle"
	matchpb "proto/match"
)

// 官方剧本:策划写的 BattleScriptConfig(protobuf JSON),随 match 构建内置,改了要重新发布 match。
// 字段名、类型、枚举名都由 protojson 按 proto 定义检查,未知字段直接拒绝;之后再按配表整份校验。
//
//go:embed official_scripts.json
var officialConfigJSON []byte

// 官方配置的条数上限
const (
	MaxOfficialScripts = 256
	MaxBindings        = 1024
)

type bindingKey struct {
	matchMode      uint32
	battleConfigID uint32
	side           battle.EBattleScriptSide
}

// Catalog 是官方剧本目录:整份配置校验通过才生成,之后只读,可并发读
type Catalog struct {
	scripts  map[uint32]*battle.BattleScript // 官方剧本 id → 规范化后的正文
	ids      []uint32                        // 官方剧本 id 升序,列表接口按它出
	bindings map[bindingKey]*battle.BattleScript
}

// LoadOfficial 解析并校验内置的官方剧本;返回错误时 match 必须拒绝启动(坏配置不能带病上线)
func LoadOfficial(tables Tables) (*Catalog, error) {
	return ParseCatalog(officialConfigJSON, tables)
}

// ParseCatalog 解析一份官方剧本配置(protobuf JSON)并校验
func ParseCatalog(raw []byte, tables Tables) (*Catalog, error) {
	var config battle.BattleScriptConfig
	if err := (protojson.UnmarshalOptions{}).Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("battle script config is not valid protobuf JSON: %w", err)
	}
	return BuildCatalog(&config, tables)
}

// BuildCatalog 整份校验官方配置:剧本 id 非 0 且唯一、每份剧本过 Normalize;
// 绑定引用的剧本存在、模式与副本存在、怪物方只绑 PVE、剧本适用于绑定的那一方、绑定键不重复
func BuildCatalog(config *battle.BattleScriptConfig, tables Tables) (*Catalog, error) {
	if len(config.GetScripts()) > MaxOfficialScripts || len(config.GetBindings()) > MaxBindings {
		return nil, fmt.Errorf("battle script config exceeds %d scripts or %d bindings", MaxOfficialScripts, MaxBindings)
	}
	catalog := &Catalog{
		scripts:  make(map[uint32]*battle.BattleScript, len(config.GetScripts())),
		bindings: make(map[bindingKey]*battle.BattleScript, len(config.GetBindings())),
	}
	for _, script := range config.GetScripts() {
		id := script.GetScriptId()
		if id == 0 || catalog.scripts[id] != nil {
			return nil, fmt.Errorf("script %d: script_id must be non-zero and unique", id)
		}
		normalized, err := Normalize(script, tables)
		if err != nil {
			return nil, err
		}
		catalog.scripts[id] = normalized
		catalog.ids = append(catalog.ids, id)
	}
	sort.Slice(catalog.ids, func(i, j int) bool { return catalog.ids[i] < catalog.ids[j] })

	for _, binding := range config.GetBindings() {
		where := fmt.Sprintf("binding (match_mode %d, battle_config_id %d, side %d)",
			binding.GetMatchMode(), binding.GetBattleConfigId(), binding.GetSide())
		script := catalog.scripts[binding.GetScriptId()]
		if script == nil {
			return nil, fmt.Errorf("%s: unknown script %d", where, binding.GetScriptId())
		}
		if !IsKnownMatchMode(binding.GetMatchMode()) {
			return nil, fmt.Errorf("%s: unknown match_mode", where)
		}
		if binding.GetBattleConfigId() != 0 && !tables.HasDungeon(binding.GetBattleConfigId()) {
			return nil, fmt.Errorf("%s: unknown dungeon", where)
		}
		// 只有 PVE 有怪物方;绑到 PVP 模式的怪物方剧本永远取不到,按配置错误拒绝
		if binding.GetSide() == battle.EBattleScriptSide_BATTLE_SCRIPT_SIDE_MONSTER && binding.GetMatchMode() != 0 &&
			!IsPveMatchMode(binding.GetMatchMode()) {
			return nil, fmt.Errorf("%s: the monster side only exists in PVE modes", where)
		}
		if err := CheckSide(script, binding.GetSide(), tables); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		key := bindingKey{binding.GetMatchMode(), binding.GetBattleConfigId(), binding.GetSide()}
		if catalog.bindings[key] != nil {
			return nil, fmt.Errorf("%s: duplicate binding", where)
		}
		catalog.bindings[key] = script
	}
	return catalog, nil
}

// IsKnownMatchMode 报告 MatchMode 数值是否认识;0 在绑定与选用里表示「任意模式」
func IsKnownMatchMode(matchMode uint32) bool {
	if matchMode == 0 {
		return true
	}
	if matchMode > math.MaxInt32 {
		return false
	}
	_, ok := matchpb.MatchMode_name[int32(matchMode)]
	return ok
}

// Official 返回官方剧本(规范化后的只读正文,调用方不得修改)
func (c *Catalog) Official(id uint32) (*battle.BattleScript, bool) {
	script, ok := c.scripts[id]
	return script, ok
}

// OfficialScripts 按 id 升序返回全部官方剧本(只读)
func (c *Catalog) OfficialScripts() []*battle.BattleScript {
	out := make([]*battle.BattleScript, 0, len(c.ids))
	for _, id := range c.ids {
		out = append(out, c.scripts[id])
	}
	return out
}

// Bound 返回这一局这一方的官方绑定剧本:按 (模式, 副本) → (模式, 任意) → (任意, 副本) → (任意, 任意) 取第一份,没有返回 nil
func (c *Catalog) Bound(matchMode, battleConfigID uint32, side battle.EBattleScriptSide) *battle.BattleScript {
	for _, key := range LookupKeys(matchMode, battleConfigID) {
		if script := c.bindings[bindingKey{key.MatchMode, key.BattleConfigID, side}]; script != nil {
			return script
		}
	}
	return nil
}

// ModeKey 是「哪种对局」的键:match_mode + battle_config_id,0 表示任意
type ModeKey struct {
	MatchMode      uint32
	BattleConfigID uint32
}

// LookupKeys 给出一局对局从具体到通用的查找顺序(官方绑定与玩家选用同一口径)
func LookupKeys(matchMode, battleConfigID uint32) []ModeKey {
	return []ModeKey{
		{matchMode, battleConfigID},
		{matchMode, 0},
		{0, battleConfigID},
		{0, 0},
	}
}

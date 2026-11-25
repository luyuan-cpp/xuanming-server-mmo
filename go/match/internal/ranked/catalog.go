// Package ranked 提供论剑排位首发 V1 的配置目录编译、合法性门禁、段位映射与确定性指纹计算
// (docs/design/ranked-play-season-ui-candidate.md)。
//
// 目录在进程启动与表包热更两阶段预检时由 TableSet 编译,任何字段缺失或跨表不合法均整批拒绝(fail-closed);
// 编译完成后完全只读,可无锁并发访问。
package ranked

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"match/internal/config"
	"match/internal/ratingrule"

	matchpb "proto/match"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

const (
	RoleKindCharacter uint32 = 1
	RoleKindPet       uint32 = 2

	casterMaskPlayer uint32 = 1 << 0
	casterMaskPet    uint32 = 1 << 1

	weightTotal           uint32 = 5
	defaultPetEntryBudget uint32 = 20
	turnBattleModel       uint32 = 1
)

// Catalog 是编译并校验通过的论剑排位不可变目录。
type Catalog struct {
	rule        *tablepb.RankedRuleTable
	presetsByID map[uint32]*tablepb.RankedPresetTable
	sorted      []*tablepb.RankedPresetTable
	views       []*matchpb.RankedPresetView
	curve       ratingrule.RankedCurve
	fingerprint string
}

// BuildFromTableSet 从完整 TableSet 编译并严格校验论剑排位目录(启动校验、热更两阶段预检与 --validate-tables 共用)。
func BuildFromTableSet(ts *table.TableSet) (*Catalog, error) {
	if ts == nil || ts.RankedRule == nil || ts.RankedPreset == nil || ts.Dungeon == nil {
		return nil, errors.New("ranked: TableSet missing RankedRule/RankedPreset/Dungeon tables")
	}
	rule, ok := ts.RankedRule.FindById(1)
	if !ok || rule == nil {
		return nil, errors.New("ranked: RankedRule table missing id=1 row")
	}
	if err := validateRule(rule); err != nil {
		return nil, err
	}
	dungeon, ok := ts.Dungeon.FindById(rule.GetBattleConfigId())
	if !ok || dungeon == nil {
		return nil, fmt.Errorf("ranked: Dungeon missing battle_config_id=%d", rule.GetBattleConfigId())
	}
	for _, m := range dungeon.GetMonster() {
		if m != 0 {
			return nil, fmt.Errorf("ranked: Dungeon id=%d must not spawn monsters for PVP ranked", dungeon.GetId())
		}
	}
	if dungeon.GetMaxTeamSize() != uint32(config.TeamCapacity) || dungeon.GetTimeLimit() == 0 || dungeon.GetAllowPetCapture() != 0 {
		return nil, fmt.Errorf("ranked: Dungeon id=%d invalid ranked limits (max_team_size=%d time_limit=%d allow_pet_capture=%d)",
			dungeon.GetId(), dungeon.GetMaxTeamSize(), dungeon.GetTimeLimit(), dungeon.GetAllowPetCapture())
	}
	if ts.Class != nil {
		if _, ok := ts.Class.FindById(rule.GetReferenceClassId()); !ok {
			return nil, fmt.Errorf("ranked: Class missing reference_class_id=%d", rule.GetReferenceClassId())
		}
	}
	entryBudget := defaultPetEntryBudget
	if ts.PetRule != nil {
		if pr, ok := ts.PetRule.FindById(1); ok && pr != nil && pr.GetEntryTotalPoints() > 0 {
			entryBudget = pr.GetEntryTotalPoints()
		}
	}
	all := ts.RankedPreset.FindAll()
	return buildCatalog(rule, all, entryBudget, ts.Skill, ts.Pet, ts.PetEntry)
}

// NewFromRows 供单测直接构造不可变排位目录,跳过与单测无关的外表查找,但保持全部结构与权重/指纹校验。
func NewFromRows(rule *tablepb.RankedRuleTable, presets []*tablepb.RankedPresetTable) (*Catalog, error) {
	if rule == nil {
		return nil, errors.New("ranked: rule is nil")
	}
	if err := validateRule(rule); err != nil {
		return nil, err
	}
	return buildCatalog(rule, presets, defaultPetEntryBudget, nil, nil, nil)
}

// DefaultFallback 供未加载配表的极简单元测试回退使用(生产启动始终经 BuildFromTableSet 强制校验)。
func DefaultFallback() *Catalog {
	rule := &tablepb.RankedRuleTable{
		Id:                       1,
		RuleVersion:              1,
		BattleConfigId:           5,
		ReferenceClassId:         1,
		CompetitiveLevel:         30,
		DefaultCharacterPresetId: 101,
		DefaultPetPresetId:       203,
		SeasonDurationDays:       56,
		WaitPromptSeconds:        120,
		SpreadTier1Seconds:       30,
		SpreadTier1Points:        100,
		SpreadTier2Seconds:       90,
		SpreadTier2Points:        200,
		SpreadTier3Points:        300,
		MilestoneAvatarMatches:   4,
		MilestoneCardMatches:     8,
		MilestoneTalismanMatches: 12,
		TierQingfengCenti:        140000,
		TierYunzhangCenti:        155000,
		TierTianshuCenti:         170000,
		TierWushuangCenti:        185000,
	}
	presets := []*tablepb.RankedPresetTable{
		{Id: 101, RoleKind: RoleKindCharacter, Code: "pozhen", Name: "破阵", RoleTag: "物攻", WeightCon: 1, WeightSpi: 0, WeightStr: 3, WeightAgi: 1, SkillIds: []uint32{10002, 10039, 10025}, Sort: 1},
		{Id: 102, RoleKind: RoleKindCharacter, Code: "kongchang", Name: "控场", RoleTag: "控制", WeightCon: 1, WeightSpi: 2, WeightStr: 0, WeightAgi: 2, SkillIds: []uint32{10006, 10021, 10034}, Sort: 2},
		{Id: 103, RoleKind: RoleKindCharacter, Code: "shouxin", Name: "守心", RoleTag: "治疗", WeightCon: 2, WeightSpi: 2, WeightStr: 0, WeightAgi: 1, SkillIds: []uint32{10010, 10033, 10040}, Sort: 3},
		{Id: 201, RoleKind: RoleKindPet, Code: "pojia", Name: "破甲", RoleTag: "破甲", PetTableId: 3, WeightCon: 1, WeightSpi: 0, WeightStr: 3, WeightAgi: 1, SkillIds: []uint32{10002, 10018, 13001}, PetEntryIds: []uint32{501}, PetEntryPoints: []uint32{20}, Sort: 1},
		{Id: 202, RoleKind: RoleKindPet, Code: "lingzhou", Name: "灵咒", RoleTag: "法控", PetTableId: 1, WeightCon: 1, WeightSpi: 2, WeightStr: 0, WeightAgi: 2, SkillIds: []uint32{10006, 10019, 10034}, PetEntryIds: []uint32{502, 531}, PetEntryPoints: []uint32{10, 10}, Sort: 2},
		{Id: 203, RoleKind: RoleKindPet, Code: "huzhu", Name: "护主", RoleTag: "护盾", PetTableId: 2, WeightCon: 2, WeightSpi: 2, WeightStr: 0, WeightAgi: 1, SkillIds: []uint32{10014, 10010, 11006, 12004}, PetEntryIds: []uint32{503, 504}, PetEntryPoints: []uint32{10, 10}, Sort: 3},
	}
	cat, err := NewFromRows(rule, presets)
	if err != nil {
		panic(err)
	}
	return cat
}

func validateRule(r *tablepb.RankedRuleTable) error {
	if r.GetRuleVersion() == 0 || r.GetBattleConfigId() == 0 || r.GetReferenceClassId() == 0 || r.GetCompetitiveLevel() == 0 {
		return errors.New("ranked: RankedRule core parameters must be positive")
	}
	if r.GetSeasonDurationDays() == 0 || r.GetWaitPromptSeconds() == 0 {
		return errors.New("ranked: RankedRule season duration and wait prompt must be positive")
	}
	if r.GetSpreadTier1Seconds() == 0 || r.GetSpreadTier2Seconds() <= r.GetSpreadTier1Seconds() ||
		r.GetSpreadTier1Points() == 0 || r.GetSpreadTier2Points() < r.GetSpreadTier1Points() ||
		r.GetSpreadTier3Points() < r.GetSpreadTier2Points() {
		return errors.New("ranked: RankedRule spread curve parameters invalid")
	}
	if r.GetMilestoneAvatarMatches() == 0 ||
		r.GetMilestoneCardMatches() <= r.GetMilestoneAvatarMatches() ||
		r.GetMilestoneTalismanMatches() <= r.GetMilestoneCardMatches() {
		return errors.New("ranked: RankedRule milestone match counts must strictly increase")
	}
	if r.GetTierQingfengCenti() == 0 ||
		r.GetTierYunzhangCenti() <= r.GetTierQingfengCenti() ||
		r.GetTierTianshuCenti() <= r.GetTierYunzhangCenti() ||
		r.GetTierWushuangCenti() <= r.GetTierTianshuCenti() {
		return errors.New("ranked: RankedRule tier thresholds must strictly increase")
	}
	return nil
}

func buildCatalog(
	rule *tablepb.RankedRuleTable,
	rows []*tablepb.RankedPresetTable,
	entryBudget uint32,
	skills *table.SkillTableManager,
	pets *table.PetTableManager,
	petEntries *table.PetEntryTableManager,
) (*Catalog, error) {
	sorted := make([]*tablepb.RankedPresetTable, 0, len(rows))
	byID := make(map[uint32]*tablepb.RankedPresetTable, len(rows))
	charCount, petCount := 0, 0
	for _, row := range rows {
		if row == nil || row.GetId() == 0 {
			return nil, errors.New("ranked: preset row nil or id=0")
		}
		if _, dup := byID[row.GetId()]; dup {
			return nil, fmt.Errorf("ranked: duplicate preset id=%d", row.GetId())
		}
		if row.GetRoleKind() != RoleKindCharacter && row.GetRoleKind() != RoleKindPet {
			return nil, fmt.Errorf("ranked: preset id=%d invalid role_kind=%d", row.GetId(), row.GetRoleKind())
		}
		if row.GetWeightCon()+row.GetWeightSpi()+row.GetWeightStr()+row.GetWeightAgi() != weightTotal {
			return nil, fmt.Errorf("ranked: preset id=%d attribute weights must sum to %d", row.GetId(), weightTotal)
		}
		if len(row.GetSkillIds()) == 0 {
			return nil, fmt.Errorf("ranked: preset id=%d must specify at least one skill", row.GetId())
		}
		if skills != nil {
			wantMask := casterMaskPlayer
			if row.GetRoleKind() == RoleKindPet {
				wantMask = casterMaskPet
			}
			for _, sid := range row.GetSkillIds() {
				sk, ok := skills.FindById(sid)
				if !ok || sk == nil {
					return nil, fmt.Errorf("ranked: preset id=%d references missing skill %d", row.GetId(), sid)
				}
				if sk.GetBattleModel() != turnBattleModel {
					return nil, fmt.Errorf("ranked: preset id=%d skill %d battle_model=%d is not turn battle", row.GetId(), sid, sk.GetBattleModel())
				}
				if sk.GetCasterMask() != 0 && (sk.GetCasterMask()&wantMask) == 0 {
					return nil, fmt.Errorf("ranked: preset id=%d skill %d caster_mask=%d incompatible with role_kind=%d", row.GetId(), sid, sk.GetCasterMask(), row.GetRoleKind())
				}
			}
		}
		if row.GetRoleKind() == RoleKindCharacter {
			charCount++
			if row.GetPetTableId() != 0 || len(row.GetPetEntryIds()) != 0 || len(row.GetPetEntryPoints()) != 0 {
				return nil, fmt.Errorf("ranked: character preset id=%d must not carry pet fields", row.GetId())
			}
		} else {
			petCount++
			if row.GetPetTableId() == 0 {
				return nil, fmt.Errorf("ranked: pet preset id=%d missing pet_table_id", row.GetId())
			}
			if pets != nil {
				if _, ok := pets.FindById(row.GetPetTableId()); !ok {
					return nil, fmt.Errorf("ranked: pet preset id=%d references missing Pet %d", row.GetId(), row.GetPetTableId())
				}
			}
			if len(row.GetPetEntryIds()) != len(row.GetPetEntryPoints()) {
				return nil, fmt.Errorf("ranked: pet preset id=%d pet_entry_ids/points length mismatch", row.GetId())
			}
			var sumPts uint32
			for i, eid := range row.GetPetEntryIds() {
				if petEntries != nil {
					if _, ok := petEntries.FindById(eid); !ok {
						return nil, fmt.Errorf("ranked: pet preset id=%d references missing PetEntry %d", row.GetId(), eid)
					}
				}
				pts := row.GetPetEntryPoints()[i]
				if pts == 0 {
					return nil, fmt.Errorf("ranked: pet preset id=%d entry %d has zero points", row.GetId(), eid)
				}
				sumPts += pts
			}
			if sumPts != entryBudget {
				return nil, fmt.Errorf("ranked: pet preset id=%d entry points sum %d != budget %d", row.GetId(), sumPts, entryBudget)
			}
		}
		byID[row.GetId()] = row
		sorted = append(sorted, row)
	}
	if charCount == 0 || petCount == 0 {
		return nil, errors.New("ranked: RankedPreset must contain both character and pet presets")
	}
	defChar, okChar := byID[rule.GetDefaultCharacterPresetId()]
	defPet, okPet := byID[rule.GetDefaultPetPresetId()]
	if !okChar || defChar.GetRoleKind() != RoleKindCharacter || !okPet || defPet.GetRoleKind() != RoleKindPet {
		return nil, errors.New("ranked: RankedRule default character/pet presets invalid")
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].GetRoleKind() != sorted[j].GetRoleKind() {
			return sorted[i].GetRoleKind() < sorted[j].GetRoleKind()
		}
		if sorted[i].GetSort() != sorted[j].GetSort() {
			return sorted[i].GetSort() < sorted[j].GetSort()
		}
		return sorted[i].GetId() < sorted[j].GetId()
	})
	views := make([]*matchpb.RankedPresetView, 0, len(sorted))
	for _, row := range sorted {
		views = append(views, toPresetView(row))
	}
	return &Catalog{
		rule:        rule,
		presetsByID: byID,
		sorted:      sorted,
		views:       views,
		curve: ratingrule.RankedCurve{
			Tier1Seconds: rule.GetSpreadTier1Seconds(),
			Tier1Points:  rule.GetSpreadTier1Points(),
			Tier2Seconds: rule.GetSpreadTier2Seconds(),
			Tier2Points:  rule.GetSpreadTier2Points(),
			Tier3Points:  rule.GetSpreadTier3Points(),
		},
		fingerprint: ComputeConfigFingerprint(rule, sorted),
	}, nil
}

// ComputeConfigFingerprint 生成 Go / C++ / Java 三端逐字一致的排位配置确定性指纹(SHA-256 小写十六进制)。
func ComputeConfigFingerprint(rule *tablepb.RankedRuleTable, presets []*tablepb.RankedPresetTable) string {
	byID := append([]*tablepb.RankedPresetTable(nil), presets...)
	sort.Slice(byID, func(i, j int) bool { return byID[i].GetId() < byID[j].GetId() })
	var b strings.Builder
	b.WriteString("ranked-v1\n")
	fmt.Fprintf(&b, "rule:id=%d,ver=%d,cfg=%d,cls=%d,lvl=%d,defc=%d,defp=%d\n",
		rule.GetId(), rule.GetRuleVersion(), rule.GetBattleConfigId(),
		rule.GetReferenceClassId(), rule.GetCompetitiveLevel(),
		rule.GetDefaultCharacterPresetId(), rule.GetDefaultPetPresetId())
	for _, p := range byID {
		fmt.Fprintf(&b, "preset:%d,kind=%d,pet=%d,w=%d/%d/%d/%d,sk=",
			p.GetId(), p.GetRoleKind(), p.GetPetTableId(),
			p.GetWeightCon(), p.GetWeightSpi(), p.GetWeightStr(), p.GetWeightAgi())
		for i, sid := range p.GetSkillIds() {
			if i > 0 {
				b.WriteByte('|')
			}
			b.WriteString(strconv.FormatUint(uint64(sid), 10))
		}
		b.WriteString(",pe=")
		for i, eid := range p.GetPetEntryIds() {
			if i > 0 {
				b.WriteByte('|')
			}
			pts := uint32(0)
			if i < len(p.GetPetEntryPoints()) {
				pts = p.GetPetEntryPoints()[i]
			}
			b.WriteString(strconv.FormatUint(uint64(eid), 10))
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(uint64(pts), 10))
		}
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (c *Catalog) effective() *Catalog {
	if c == nil {
		return DefaultFallback()
	}
	return c
}

func (c *Catalog) Rule() *tablepb.RankedRuleTable { return c.effective().rule }
func (c *Catalog) RuleVersion() uint32            { return c.effective().rule.GetRuleVersion() }
func (c *Catalog) BattleConfigId() uint32         { return c.effective().rule.GetBattleConfigId() }
func (c *Catalog) Fingerprint() string            { return c.effective().fingerprint }
func (c *Catalog) RankedCurve() ratingrule.RankedCurve {
	return c.effective().curve
}
func (c *Catalog) WaitPromptSeconds() uint32 { return c.effective().rule.GetWaitPromptSeconds() }

func (c *Catalog) DefaultPresets() (uint32, uint32) {
	r := c.effective().rule
	return r.GetDefaultCharacterPresetId(), r.GetDefaultPetPresetId()
}

func (c *Catalog) IsValidPair(characterPresetID, petPresetID uint32) bool {
	cat := c.effective()
	ch, okC := cat.presetsByID[characterPresetID]
	pet, okP := cat.presetsByID[petPresetID]
	return okC && okP && ch.GetRoleKind() == RoleKindCharacter && pet.GetRoleKind() == RoleKindPet
}

func (c *Catalog) PresetViews() []*matchpb.RankedPresetView {
	return c.effective().views
}

// TierFor 按服务端原精度评分(centi)映射五段位;未完成首场有效参赛时返回 UNSPECIFIED。
func (c *Catalog) TierFor(ratingCenti int64, qualified bool) matchpb.RankedTier {
	if !qualified {
		return matchpb.RankedTier_RANKED_TIER_UNSPECIFIED
	}
	r := c.effective().rule
	switch {
	case ratingCenti >= int64(r.GetTierWushuangCenti()):
		return matchpb.RankedTier_RANKED_TIER_WUSHUANG
	case ratingCenti >= int64(r.GetTierTianshuCenti()):
		return matchpb.RankedTier_RANKED_TIER_TIANSHU
	case ratingCenti >= int64(r.GetTierYunzhangCenti()):
		return matchpb.RankedTier_RANKED_TIER_YUNZHANG
	case ratingCenti >= int64(r.GetTierQingfengCenti()):
		return matchpb.RankedTier_RANKED_TIER_QINGFENG
	default:
		return matchpb.RankedTier_RANKED_TIER_CHUSHI
	}
}

func toPresetView(row *tablepb.RankedPresetTable) *matchpb.RankedPresetView {
	return &matchpb.RankedPresetView{
		PresetId:       row.GetId(),
		RoleKind:       row.GetRoleKind(),
		Code:           row.GetCode(),
		Name:           row.GetName(),
		RoleTag:        row.GetRoleTag(),
		PetTableId:     row.GetPetTableId(),
		Weights:        []uint32{row.GetWeightCon(), row.GetWeightSpi(), row.GetWeightStr(), row.GetWeightAgi()},
		SkillIds:       append([]uint32(nil), row.GetSkillIds()...),
		PetEntryIds:    append([]uint32(nil), row.GetPetEntryIds()...),
		PetEntryPoints: append([]uint32(nil), row.GetPetEntryPoints()...),
		Sort:           row.GetSort(),
		Desc:           row.GetDesc(),
	}
}

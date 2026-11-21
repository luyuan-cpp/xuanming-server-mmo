
package table

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// SchemaDigest 是整批服务端配置表的数据布局摘要(16 位小写十六进制,docs/design/config-table-hot-reload.md §5.1)。
const SchemaDigest = "de4be939691ab0e3"

var hotReloadableTables = map[string]bool{
	"ActivitySchedule": true,
	"ActivityVitalityReward": true,
	"ActivityVitalityRule": true,
	"Buff": true,
	"Cooldown": true,
	"GuildDonate": true,
	"GuildShop": true,
	"MessageLimiter": true,
	"Monster": true,
	"MysteryMerchantGoods": true,
	"MysteryMerchantRule": true,
	"Skill": true,
	"SkillPermission": true,
}

var allTableNames = []string{
	"ActivitySchedule",
	"ActivityVitalityReward",
	"ActivityVitalityRule",
	"ActorActionCombatState",
	"ActorActionState",
	"AfkPassRule",
	"AttributeAllocRatio",
	"AttributeAutoPlan",
	"AttributeDimension",
	"AttributePool",
	"AttributeRule",
	"BaseScene",
	"Buff",
	"Class",
	"Condition",
	"Cooldown",
	"Dungeon",
	"EquipAffixPool",
	"EquipAffixRule",
	"EquipArtifact",
	"EquipAttribute",
	"EquipAttributeCap",
	"EquipEnhance",
	"EquipGem",
	"EquipGemSocket",
	"EquipSlot",
	"EquipV1Affix",
	"GlobalVariable",
	"GuildActivity",
	"GuildDonate",
	"GuildLevel",
	"GuildRule",
	"GuildShop",
	"Item",
	"MessageLimiter",
	"Mirror",
	"Mission",
	"Monster",
	"MysteryMerchantGoods",
	"MysteryMerchantRule",
	"MysteryMerchantSpot",
	"Pet",
	"PetEntry",
	"PetRule",
	"PetTier",
	"PlayMode",
	"Reward",
	"RoleNameRule",
	"Skill",
	"SkillPermission",
	"Test",
	"TestMultiKey",
	"World",
	"ZoneGroup",
}

// IsHotReloadable 按本二进制编译期名单判定该表是否允许运行期热更(cfg_hot_reload)。
func IsHotReloadable(tableName string) bool {
	return hotReloadableTables[tableName]
}

// TableNames 返回本二进制包含的全部表名副本(按字典序)。
func TableNames() []string {
	out := make([]string, len(allTableNames))
	copy(out, allTableNames)
	return out
}

// BundleIdentity 记录一套 TableSet 的来源与摘要身份。
type BundleIdentity struct {
	SchemaDigest  string
	ContentDigest string
	Version       uint64
	LoadSeq       uint64
	Source        string // "image" 或 "bundle"
}

// TableSet 持有整批不可变表快照:需要跨多张表读同一版本的业务先取 ts := table.Current() 再经 ts 读取。
type TableSet struct {
	Identity BundleIdentity
	ActivitySchedule *ActivityScheduleTableManager
	ActivityVitalityReward *ActivityVitalityRewardTableManager
	ActivityVitalityRule *ActivityVitalityRuleTableManager
	ActorActionCombatState *ActorActionCombatStateTableManager
	ActorActionState *ActorActionStateTableManager
	AfkPassRule *AfkPassRuleTableManager
	AttributeAllocRatio *AttributeAllocRatioTableManager
	AttributeAutoPlan *AttributeAutoPlanTableManager
	AttributeDimension *AttributeDimensionTableManager
	AttributePool *AttributePoolTableManager
	AttributeRule *AttributeRuleTableManager
	BaseScene *BaseSceneTableManager
	Buff *BuffTableManager
	Class *ClassTableManager
	Condition *ConditionTableManager
	Cooldown *CooldownTableManager
	Dungeon *DungeonTableManager
	EquipAffixPool *EquipAffixPoolTableManager
	EquipAffixRule *EquipAffixRuleTableManager
	EquipArtifact *EquipArtifactTableManager
	EquipAttribute *EquipAttributeTableManager
	EquipAttributeCap *EquipAttributeCapTableManager
	EquipEnhance *EquipEnhanceTableManager
	EquipGem *EquipGemTableManager
	EquipGemSocket *EquipGemSocketTableManager
	EquipSlot *EquipSlotTableManager
	EquipV1Affix *EquipV1AffixTableManager
	GlobalVariable *GlobalVariableTableManager
	GuildActivity *GuildActivityTableManager
	GuildDonate *GuildDonateTableManager
	GuildLevel *GuildLevelTableManager
	GuildRule *GuildRuleTableManager
	GuildShop *GuildShopTableManager
	Item *ItemTableManager
	MessageLimiter *MessageLimiterTableManager
	Mirror *MirrorTableManager
	Mission *MissionTableManager
	Monster *MonsterTableManager
	MysteryMerchantGoods *MysteryMerchantGoodsTableManager
	MysteryMerchantRule *MysteryMerchantRuleTableManager
	MysteryMerchantSpot *MysteryMerchantSpotTableManager
	Pet *PetTableManager
	PetEntry *PetEntryTableManager
	PetRule *PetRuleTableManager
	PetTier *PetTierTableManager
	PlayMode *PlayModeTableManager
	Reward *RewardTableManager
	RoleNameRule *RoleNameRuleTableManager
	Skill *SkillTableManager
	SkillPermission *SkillPermissionTableManager
	Test *TestTableManager
	TestMultiKey *TestMultiKeyTableManager
	World *WorldTableManager
	ZoneGroup *ZoneGroupTableManager
}

var (
	loadSuccessCallback func()
	currentTableSet     atomic.Pointer[TableSet]
	commitSeq           atomic.Uint64
	commitCallbacksMu   sync.Mutex
	commitCallbacks     []func(*TableSet)
)

// Current 返回当前已提交的整套不可变 TableSet;尚未 LoadTables / Commit 前返回 nil。
func Current() *TableSet {
	return currentTableSet.Load()
}

// Prepare 从 configDir 解析全部配置表到一套全新的独立 TableSet,不修改任何现役单例。
// 任意一张表失败立即返回错误。
func Prepare(configDir string, useBinary bool) (*TableSet, error) {
	ts := &TableSet{
		Identity: BundleIdentity{
			SchemaDigest: SchemaDigest,
		},
		ActivitySchedule: NewActivityScheduleTableManager(),
		ActivityVitalityReward: NewActivityVitalityRewardTableManager(),
		ActivityVitalityRule: NewActivityVitalityRuleTableManager(),
		ActorActionCombatState: NewActorActionCombatStateTableManager(),
		ActorActionState: NewActorActionStateTableManager(),
		AfkPassRule: NewAfkPassRuleTableManager(),
		AttributeAllocRatio: NewAttributeAllocRatioTableManager(),
		AttributeAutoPlan: NewAttributeAutoPlanTableManager(),
		AttributeDimension: NewAttributeDimensionTableManager(),
		AttributePool: NewAttributePoolTableManager(),
		AttributeRule: NewAttributeRuleTableManager(),
		BaseScene: NewBaseSceneTableManager(),
		Buff: NewBuffTableManager(),
		Class: NewClassTableManager(),
		Condition: NewConditionTableManager(),
		Cooldown: NewCooldownTableManager(),
		Dungeon: NewDungeonTableManager(),
		EquipAffixPool: NewEquipAffixPoolTableManager(),
		EquipAffixRule: NewEquipAffixRuleTableManager(),
		EquipArtifact: NewEquipArtifactTableManager(),
		EquipAttribute: NewEquipAttributeTableManager(),
		EquipAttributeCap: NewEquipAttributeCapTableManager(),
		EquipEnhance: NewEquipEnhanceTableManager(),
		EquipGem: NewEquipGemTableManager(),
		EquipGemSocket: NewEquipGemSocketTableManager(),
		EquipSlot: NewEquipSlotTableManager(),
		EquipV1Affix: NewEquipV1AffixTableManager(),
		GlobalVariable: NewGlobalVariableTableManager(),
		GuildActivity: NewGuildActivityTableManager(),
		GuildDonate: NewGuildDonateTableManager(),
		GuildLevel: NewGuildLevelTableManager(),
		GuildRule: NewGuildRuleTableManager(),
		GuildShop: NewGuildShopTableManager(),
		Item: NewItemTableManager(),
		MessageLimiter: NewMessageLimiterTableManager(),
		Mirror: NewMirrorTableManager(),
		Mission: NewMissionTableManager(),
		Monster: NewMonsterTableManager(),
		MysteryMerchantGoods: NewMysteryMerchantGoodsTableManager(),
		MysteryMerchantRule: NewMysteryMerchantRuleTableManager(),
		MysteryMerchantSpot: NewMysteryMerchantSpotTableManager(),
		Pet: NewPetTableManager(),
		PetEntry: NewPetEntryTableManager(),
		PetRule: NewPetRuleTableManager(),
		PetTier: NewPetTierTableManager(),
		PlayMode: NewPlayModeTableManager(),
		Reward: NewRewardTableManager(),
		RoleNameRule: NewRoleNameRuleTableManager(),
		Skill: NewSkillTableManager(),
		SkillPermission: NewSkillPermissionTableManager(),
		Test: NewTestTableManager(),
		TestMultiKey: NewTestMultiKeyTableManager(),
		World: NewWorldTableManager(),
		ZoneGroup: NewZoneGroupTableManager(),
	}
	if err := ts.ActivitySchedule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare ActivitySchedule table failed: %w", err)
	}
	if err := ts.ActivityVitalityReward.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare ActivityVitalityReward table failed: %w", err)
	}
	if err := ts.ActivityVitalityRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare ActivityVitalityRule table failed: %w", err)
	}
	if err := ts.ActorActionCombatState.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare ActorActionCombatState table failed: %w", err)
	}
	if err := ts.ActorActionState.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare ActorActionState table failed: %w", err)
	}
	if err := ts.AfkPassRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare AfkPassRule table failed: %w", err)
	}
	if err := ts.AttributeAllocRatio.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare AttributeAllocRatio table failed: %w", err)
	}
	if err := ts.AttributeAutoPlan.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare AttributeAutoPlan table failed: %w", err)
	}
	if err := ts.AttributeDimension.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare AttributeDimension table failed: %w", err)
	}
	if err := ts.AttributePool.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare AttributePool table failed: %w", err)
	}
	if err := ts.AttributeRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare AttributeRule table failed: %w", err)
	}
	if err := ts.BaseScene.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare BaseScene table failed: %w", err)
	}
	if err := ts.Buff.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Buff table failed: %w", err)
	}
	if err := ts.Class.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Class table failed: %w", err)
	}
	if err := ts.Condition.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Condition table failed: %w", err)
	}
	if err := ts.Cooldown.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Cooldown table failed: %w", err)
	}
	if err := ts.Dungeon.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Dungeon table failed: %w", err)
	}
	if err := ts.EquipAffixPool.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipAffixPool table failed: %w", err)
	}
	if err := ts.EquipAffixRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipAffixRule table failed: %w", err)
	}
	if err := ts.EquipArtifact.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipArtifact table failed: %w", err)
	}
	if err := ts.EquipAttribute.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipAttribute table failed: %w", err)
	}
	if err := ts.EquipAttributeCap.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipAttributeCap table failed: %w", err)
	}
	if err := ts.EquipEnhance.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipEnhance table failed: %w", err)
	}
	if err := ts.EquipGem.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipGem table failed: %w", err)
	}
	if err := ts.EquipGemSocket.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipGemSocket table failed: %w", err)
	}
	if err := ts.EquipSlot.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipSlot table failed: %w", err)
	}
	if err := ts.EquipV1Affix.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare EquipV1Affix table failed: %w", err)
	}
	if err := ts.GlobalVariable.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare GlobalVariable table failed: %w", err)
	}
	if err := ts.GuildActivity.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare GuildActivity table failed: %w", err)
	}
	if err := ts.GuildDonate.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare GuildDonate table failed: %w", err)
	}
	if err := ts.GuildLevel.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare GuildLevel table failed: %w", err)
	}
	if err := ts.GuildRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare GuildRule table failed: %w", err)
	}
	if err := ts.GuildShop.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare GuildShop table failed: %w", err)
	}
	if err := ts.Item.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Item table failed: %w", err)
	}
	if err := ts.MessageLimiter.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare MessageLimiter table failed: %w", err)
	}
	if err := ts.Mirror.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Mirror table failed: %w", err)
	}
	if err := ts.Mission.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Mission table failed: %w", err)
	}
	if err := ts.Monster.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Monster table failed: %w", err)
	}
	if err := ts.MysteryMerchantGoods.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare MysteryMerchantGoods table failed: %w", err)
	}
	if err := ts.MysteryMerchantRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare MysteryMerchantRule table failed: %w", err)
	}
	if err := ts.MysteryMerchantSpot.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare MysteryMerchantSpot table failed: %w", err)
	}
	if err := ts.Pet.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Pet table failed: %w", err)
	}
	if err := ts.PetEntry.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare PetEntry table failed: %w", err)
	}
	if err := ts.PetRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare PetRule table failed: %w", err)
	}
	if err := ts.PetTier.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare PetTier table failed: %w", err)
	}
	if err := ts.PlayMode.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare PlayMode table failed: %w", err)
	}
	if err := ts.Reward.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Reward table failed: %w", err)
	}
	if err := ts.RoleNameRule.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare RoleNameRule table failed: %w", err)
	}
	if err := ts.Skill.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Skill table failed: %w", err)
	}
	if err := ts.SkillPermission.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare SkillPermission table failed: %w", err)
	}
	if err := ts.Test.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare Test table failed: %w", err)
	}
	if err := ts.TestMultiKey.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare TestMultiKey table failed: %w", err)
	}
	if err := ts.World.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare World table failed: %w", err)
	}
	if err := ts.ZoneGroup.Load(configDir, useBinary); err != nil {
		return nil, fmt.Errorf("prepare ZoneGroup table failed: %w", err)
	}
	return ts, nil
}

// Commit 把 Prepare 产出的整套 TableSet 提交生效:
// 先替换全部包级单例的原子快照指针,再原子发布 Current(),最后按序回调 OnTablesCommit 监听器。
func Commit(ts *TableSet) {
	if ts == nil {
		return
	}
	ts.Identity.LoadSeq = commitSeq.Add(1)
	if ts.ActivitySchedule != nil {
		ActivityScheduleTableManagerInstance.swapSnapshot(ts.ActivitySchedule.snap.Load())
	}
	if ts.ActivityVitalityReward != nil {
		ActivityVitalityRewardTableManagerInstance.swapSnapshot(ts.ActivityVitalityReward.snap.Load())
	}
	if ts.ActivityVitalityRule != nil {
		ActivityVitalityRuleTableManagerInstance.swapSnapshot(ts.ActivityVitalityRule.snap.Load())
	}
	if ts.ActorActionCombatState != nil {
		ActorActionCombatStateTableManagerInstance.swapSnapshot(ts.ActorActionCombatState.snap.Load())
	}
	if ts.ActorActionState != nil {
		ActorActionStateTableManagerInstance.swapSnapshot(ts.ActorActionState.snap.Load())
	}
	if ts.AfkPassRule != nil {
		AfkPassRuleTableManagerInstance.swapSnapshot(ts.AfkPassRule.snap.Load())
	}
	if ts.AttributeAllocRatio != nil {
		AttributeAllocRatioTableManagerInstance.swapSnapshot(ts.AttributeAllocRatio.snap.Load())
	}
	if ts.AttributeAutoPlan != nil {
		AttributeAutoPlanTableManagerInstance.swapSnapshot(ts.AttributeAutoPlan.snap.Load())
	}
	if ts.AttributeDimension != nil {
		AttributeDimensionTableManagerInstance.swapSnapshot(ts.AttributeDimension.snap.Load())
	}
	if ts.AttributePool != nil {
		AttributePoolTableManagerInstance.swapSnapshot(ts.AttributePool.snap.Load())
	}
	if ts.AttributeRule != nil {
		AttributeRuleTableManagerInstance.swapSnapshot(ts.AttributeRule.snap.Load())
	}
	if ts.BaseScene != nil {
		BaseSceneTableManagerInstance.swapSnapshot(ts.BaseScene.snap.Load())
	}
	if ts.Buff != nil {
		BuffTableManagerInstance.swapSnapshot(ts.Buff.snap.Load())
	}
	if ts.Class != nil {
		ClassTableManagerInstance.swapSnapshot(ts.Class.snap.Load())
	}
	if ts.Condition != nil {
		ConditionTableManagerInstance.swapSnapshot(ts.Condition.snap.Load())
	}
	if ts.Cooldown != nil {
		CooldownTableManagerInstance.swapSnapshot(ts.Cooldown.snap.Load())
	}
	if ts.Dungeon != nil {
		DungeonTableManagerInstance.swapSnapshot(ts.Dungeon.snap.Load())
	}
	if ts.EquipAffixPool != nil {
		EquipAffixPoolTableManagerInstance.swapSnapshot(ts.EquipAffixPool.snap.Load())
	}
	if ts.EquipAffixRule != nil {
		EquipAffixRuleTableManagerInstance.swapSnapshot(ts.EquipAffixRule.snap.Load())
	}
	if ts.EquipArtifact != nil {
		EquipArtifactTableManagerInstance.swapSnapshot(ts.EquipArtifact.snap.Load())
	}
	if ts.EquipAttribute != nil {
		EquipAttributeTableManagerInstance.swapSnapshot(ts.EquipAttribute.snap.Load())
	}
	if ts.EquipAttributeCap != nil {
		EquipAttributeCapTableManagerInstance.swapSnapshot(ts.EquipAttributeCap.snap.Load())
	}
	if ts.EquipEnhance != nil {
		EquipEnhanceTableManagerInstance.swapSnapshot(ts.EquipEnhance.snap.Load())
	}
	if ts.EquipGem != nil {
		EquipGemTableManagerInstance.swapSnapshot(ts.EquipGem.snap.Load())
	}
	if ts.EquipGemSocket != nil {
		EquipGemSocketTableManagerInstance.swapSnapshot(ts.EquipGemSocket.snap.Load())
	}
	if ts.EquipSlot != nil {
		EquipSlotTableManagerInstance.swapSnapshot(ts.EquipSlot.snap.Load())
	}
	if ts.EquipV1Affix != nil {
		EquipV1AffixTableManagerInstance.swapSnapshot(ts.EquipV1Affix.snap.Load())
	}
	if ts.GlobalVariable != nil {
		GlobalVariableTableManagerInstance.swapSnapshot(ts.GlobalVariable.snap.Load())
	}
	if ts.GuildActivity != nil {
		GuildActivityTableManagerInstance.swapSnapshot(ts.GuildActivity.snap.Load())
	}
	if ts.GuildDonate != nil {
		GuildDonateTableManagerInstance.swapSnapshot(ts.GuildDonate.snap.Load())
	}
	if ts.GuildLevel != nil {
		GuildLevelTableManagerInstance.swapSnapshot(ts.GuildLevel.snap.Load())
	}
	if ts.GuildRule != nil {
		GuildRuleTableManagerInstance.swapSnapshot(ts.GuildRule.snap.Load())
	}
	if ts.GuildShop != nil {
		GuildShopTableManagerInstance.swapSnapshot(ts.GuildShop.snap.Load())
	}
	if ts.Item != nil {
		ItemTableManagerInstance.swapSnapshot(ts.Item.snap.Load())
	}
	if ts.MessageLimiter != nil {
		MessageLimiterTableManagerInstance.swapSnapshot(ts.MessageLimiter.snap.Load())
	}
	if ts.Mirror != nil {
		MirrorTableManagerInstance.swapSnapshot(ts.Mirror.snap.Load())
	}
	if ts.Mission != nil {
		MissionTableManagerInstance.swapSnapshot(ts.Mission.snap.Load())
	}
	if ts.Monster != nil {
		MonsterTableManagerInstance.swapSnapshot(ts.Monster.snap.Load())
	}
	if ts.MysteryMerchantGoods != nil {
		MysteryMerchantGoodsTableManagerInstance.swapSnapshot(ts.MysteryMerchantGoods.snap.Load())
	}
	if ts.MysteryMerchantRule != nil {
		MysteryMerchantRuleTableManagerInstance.swapSnapshot(ts.MysteryMerchantRule.snap.Load())
	}
	if ts.MysteryMerchantSpot != nil {
		MysteryMerchantSpotTableManagerInstance.swapSnapshot(ts.MysteryMerchantSpot.snap.Load())
	}
	if ts.Pet != nil {
		PetTableManagerInstance.swapSnapshot(ts.Pet.snap.Load())
	}
	if ts.PetEntry != nil {
		PetEntryTableManagerInstance.swapSnapshot(ts.PetEntry.snap.Load())
	}
	if ts.PetRule != nil {
		PetRuleTableManagerInstance.swapSnapshot(ts.PetRule.snap.Load())
	}
	if ts.PetTier != nil {
		PetTierTableManagerInstance.swapSnapshot(ts.PetTier.snap.Load())
	}
	if ts.PlayMode != nil {
		PlayModeTableManagerInstance.swapSnapshot(ts.PlayMode.snap.Load())
	}
	if ts.Reward != nil {
		RewardTableManagerInstance.swapSnapshot(ts.Reward.snap.Load())
	}
	if ts.RoleNameRule != nil {
		RoleNameRuleTableManagerInstance.swapSnapshot(ts.RoleNameRule.snap.Load())
	}
	if ts.Skill != nil {
		SkillTableManagerInstance.swapSnapshot(ts.Skill.snap.Load())
	}
	if ts.SkillPermission != nil {
		SkillPermissionTableManagerInstance.swapSnapshot(ts.SkillPermission.snap.Load())
	}
	if ts.Test != nil {
		TestTableManagerInstance.swapSnapshot(ts.Test.snap.Load())
	}
	if ts.TestMultiKey != nil {
		TestMultiKeyTableManagerInstance.swapSnapshot(ts.TestMultiKey.snap.Load())
	}
	if ts.World != nil {
		WorldTableManagerInstance.swapSnapshot(ts.World.snap.Load())
	}
	if ts.ZoneGroup != nil {
		ZoneGroupTableManagerInstance.swapSnapshot(ts.ZoneGroup.snap.Load())
	}
	currentTableSet.Store(ts)

	commitCallbacksMu.Lock()
	cbs := make([]func(*TableSet), len(commitCallbacks))
	copy(cbs, commitCallbacks)
	commitCallbacksMu.Unlock()
	for _, cb := range cbs {
		cb(ts)
	}
}

// OnTablesCommit 注册每次整批 Commit 生效后的回调(启动与热更均会触发)。
func OnTablesCommit(cb func(*TableSet)) {
	if cb == nil {
		return
	}
	commitCallbacksMu.Lock()
	commitCallbacks = append(commitCallbacks, cb)
	commitCallbacksMu.Unlock()
}

// LoadTables loads all config tables synchronously.
// useBinary: true loads .pb (proto binary), false loads .json.
func LoadTables(configDir string, useBinary bool) {
	ts, err := Prepare(configDir, useBinary)
	if err != nil {
		log.Fatalf("failed to load tables from %s: %v", configDir, err)
	}
	ts.Identity.Source = "image"
	Commit(ts)

	if loadSuccessCallback != nil {
		loadSuccessCallback()
	}
}

// LoadTablesAsync loads all config tables using goroutines.
// useBinary: true loads .pb (proto binary), false loads .json.
func LoadTablesAsync(configDir string, useBinary bool) {
	ts := &TableSet{
		Identity: BundleIdentity{
			SchemaDigest: SchemaDigest,
			Source:       "image",
		},
		ActivitySchedule: NewActivityScheduleTableManager(),
		ActivityVitalityReward: NewActivityVitalityRewardTableManager(),
		ActivityVitalityRule: NewActivityVitalityRuleTableManager(),
		ActorActionCombatState: NewActorActionCombatStateTableManager(),
		ActorActionState: NewActorActionStateTableManager(),
		AfkPassRule: NewAfkPassRuleTableManager(),
		AttributeAllocRatio: NewAttributeAllocRatioTableManager(),
		AttributeAutoPlan: NewAttributeAutoPlanTableManager(),
		AttributeDimension: NewAttributeDimensionTableManager(),
		AttributePool: NewAttributePoolTableManager(),
		AttributeRule: NewAttributeRuleTableManager(),
		BaseScene: NewBaseSceneTableManager(),
		Buff: NewBuffTableManager(),
		Class: NewClassTableManager(),
		Condition: NewConditionTableManager(),
		Cooldown: NewCooldownTableManager(),
		Dungeon: NewDungeonTableManager(),
		EquipAffixPool: NewEquipAffixPoolTableManager(),
		EquipAffixRule: NewEquipAffixRuleTableManager(),
		EquipArtifact: NewEquipArtifactTableManager(),
		EquipAttribute: NewEquipAttributeTableManager(),
		EquipAttributeCap: NewEquipAttributeCapTableManager(),
		EquipEnhance: NewEquipEnhanceTableManager(),
		EquipGem: NewEquipGemTableManager(),
		EquipGemSocket: NewEquipGemSocketTableManager(),
		EquipSlot: NewEquipSlotTableManager(),
		EquipV1Affix: NewEquipV1AffixTableManager(),
		GlobalVariable: NewGlobalVariableTableManager(),
		GuildActivity: NewGuildActivityTableManager(),
		GuildDonate: NewGuildDonateTableManager(),
		GuildLevel: NewGuildLevelTableManager(),
		GuildRule: NewGuildRuleTableManager(),
		GuildShop: NewGuildShopTableManager(),
		Item: NewItemTableManager(),
		MessageLimiter: NewMessageLimiterTableManager(),
		Mirror: NewMirrorTableManager(),
		Mission: NewMissionTableManager(),
		Monster: NewMonsterTableManager(),
		MysteryMerchantGoods: NewMysteryMerchantGoodsTableManager(),
		MysteryMerchantRule: NewMysteryMerchantRuleTableManager(),
		MysteryMerchantSpot: NewMysteryMerchantSpotTableManager(),
		Pet: NewPetTableManager(),
		PetEntry: NewPetEntryTableManager(),
		PetRule: NewPetRuleTableManager(),
		PetTier: NewPetTierTableManager(),
		PlayMode: NewPlayModeTableManager(),
		Reward: NewRewardTableManager(),
		RoleNameRule: NewRoleNameRuleTableManager(),
		Skill: NewSkillTableManager(),
		SkillPermission: NewSkillPermissionTableManager(),
		Test: NewTestTableManager(),
		TestMultiKey: NewTestMultiKeyTableManager(),
		World: NewWorldTableManager(),
		ZoneGroup: NewZoneGroupTableManager(),
	}
	var wg sync.WaitGroup
	wg.Add(54)
	go func() {
		defer wg.Done()
		if err := ts.ActivitySchedule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load ActivitySchedule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.ActivityVitalityReward.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load ActivityVitalityReward table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.ActivityVitalityRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load ActivityVitalityRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.ActorActionCombatState.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load ActorActionCombatState table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.ActorActionState.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load ActorActionState table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.AfkPassRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load AfkPassRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.AttributeAllocRatio.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load AttributeAllocRatio table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.AttributeAutoPlan.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load AttributeAutoPlan table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.AttributeDimension.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load AttributeDimension table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.AttributePool.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load AttributePool table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.AttributeRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load AttributeRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.BaseScene.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load BaseScene table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Buff.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Buff table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Class.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Class table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Condition.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Condition table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Cooldown.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Cooldown table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Dungeon.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Dungeon table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipAffixPool.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipAffixPool table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipAffixRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipAffixRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipArtifact.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipArtifact table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipAttribute.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipAttribute table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipAttributeCap.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipAttributeCap table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipEnhance.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipEnhance table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipGem.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipGem table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipGemSocket.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipGemSocket table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipSlot.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipSlot table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.EquipV1Affix.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load EquipV1Affix table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.GlobalVariable.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load GlobalVariable table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.GuildActivity.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load GuildActivity table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.GuildDonate.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load GuildDonate table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.GuildLevel.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load GuildLevel table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.GuildRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load GuildRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.GuildShop.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load GuildShop table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Item.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Item table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.MessageLimiter.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load MessageLimiter table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Mirror.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Mirror table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Mission.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Mission table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Monster.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Monster table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.MysteryMerchantGoods.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load MysteryMerchantGoods table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.MysteryMerchantRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load MysteryMerchantRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.MysteryMerchantSpot.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load MysteryMerchantSpot table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Pet.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Pet table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.PetEntry.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load PetEntry table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.PetRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load PetRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.PetTier.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load PetTier table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.PlayMode.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load PlayMode table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Reward.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Reward table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.RoleNameRule.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load RoleNameRule table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Skill.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Skill table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.SkillPermission.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load SkillPermission table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.Test.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load Test table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.TestMultiKey.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load TestMultiKey table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.World.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load World table: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ts.ZoneGroup.Load(configDir, useBinary); err != nil {
			log.Fatalf("failed to load ZoneGroup table: %v", err)
		}
	}()

	wg.Wait()
	Commit(ts)

	if loadSuccessCallback != nil {
		loadSuccessCallback()
	}
}

// OnTablesLoadSuccess registers a callback invoked after initial table load completes.
func OnTablesLoadSuccess(cb func()) {
	loadSuccessCallback = cb
}
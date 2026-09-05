
package table

import (
    "fmt"
    "log"
    "sync"
)

var loadSuccessCallback func()

// LoadTables loads all config tables synchronously.
// useBinary: true loads .pb (proto binary), false loads .json.
func LoadTables(configDir string, useBinary bool) {
    if err := ActorActionCombatStateTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load ActorActionCombatState table: %v", err)
    }
    if err := ActorActionStateTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load ActorActionState table: %v", err)
    }
    if err := AttributeAutoPlanTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load AttributeAutoPlan table: %v", err)
    }
    if err := AttributeDimensionTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load AttributeDimension table: %v", err)
    }
    if err := AttributePoolTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load AttributePool table: %v", err)
    }
    if err := AttributeRuleTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load AttributeRule table: %v", err)
    }
    if err := BaseSceneTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load BaseScene table: %v", err)
    }
    if err := BuffTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Buff table: %v", err)
    }
    if err := ClassTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Class table: %v", err)
    }
    if err := ConditionTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Condition table: %v", err)
    }
    if err := CooldownTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Cooldown table: %v", err)
    }
    if err := DungeonTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Dungeon table: %v", err)
    }
    if err := EquipSlotTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load EquipSlot table: %v", err)
    }
    if err := GlobalVariableTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load GlobalVariable table: %v", err)
    }
    if err := ItemTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Item table: %v", err)
    }
    if err := MessageLimiterTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load MessageLimiter table: %v", err)
    }
    if err := MirrorTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Mirror table: %v", err)
    }
    if err := MissionTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Mission table: %v", err)
    }
    if err := MonsterTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Monster table: %v", err)
    }
    if err := RewardTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Reward table: %v", err)
    }
    if err := SkillTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Skill table: %v", err)
    }
    if err := SkillPermissionTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load SkillPermission table: %v", err)
    }
    if err := TestTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load Test table: %v", err)
    }
    if err := TestMultiKeyTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load TestMultiKey table: %v", err)
    }
    if err := WorldTableManagerInstance.Load(configDir, useBinary); err != nil {
        log.Fatalf("failed to load World table: %v", err)
    }

    if loadSuccessCallback != nil {
        loadSuccessCallback()
    }
}

// LoadTablesAsync loads all config tables using goroutines.
// useBinary: true loads .pb (proto binary), false loads .json.
func LoadTablesAsync(configDir string, useBinary bool) {
    var wg sync.WaitGroup
    wg.Add(25)
    go func() {
        defer wg.Done()
        if err := ActorActionCombatStateTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load ActorActionCombatState table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := ActorActionStateTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load ActorActionState table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := AttributeAutoPlanTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load AttributeAutoPlan table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := AttributeDimensionTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load AttributeDimension table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := AttributePoolTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load AttributePool table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := AttributeRuleTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load AttributeRule table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := BaseSceneTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load BaseScene table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := BuffTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Buff table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := ClassTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Class table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := ConditionTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Condition table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := CooldownTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Cooldown table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := DungeonTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Dungeon table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := EquipSlotTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load EquipSlot table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := GlobalVariableTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load GlobalVariable table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := ItemTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Item table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := MessageLimiterTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load MessageLimiter table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := MirrorTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Mirror table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := MissionTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Mission table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := MonsterTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Monster table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := RewardTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Reward table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := SkillTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Skill table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := SkillPermissionTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load SkillPermission table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := TestTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load Test table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := TestMultiKeyTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load TestMultiKey table: %v", err)
        }
    }()
    go func() {
        defer wg.Done()
        if err := WorldTableManagerInstance.Load(configDir, useBinary); err != nil {
            log.Fatalf("failed to load World table: %v", err)
        }
    }()

    wg.Wait()

    if loadSuccessCallback != nil {
        loadSuccessCallback()
    }
}

// OnTablesLoadSuccess registers a callback invoked after all tables load.
func OnTablesLoadSuccess(cb func()) {
    loadSuccessCallback = cb
}

// ReloadTables re-creates all table managers and loads fresh data.
// Safe for hot-reload: replaces the global instances atomically.
func ReloadTables(configDir string, useBinary bool) error {
    newActorActionCombatState := NewActorActionCombatStateTableManager()
    if err := newActorActionCombatState.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload ActorActionCombatState failed: %w", err)
    }
    newActorActionState := NewActorActionStateTableManager()
    if err := newActorActionState.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload ActorActionState failed: %w", err)
    }
    newAttributeAutoPlan := NewAttributeAutoPlanTableManager()
    if err := newAttributeAutoPlan.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload AttributeAutoPlan failed: %w", err)
    }
    newAttributeDimension := NewAttributeDimensionTableManager()
    if err := newAttributeDimension.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload AttributeDimension failed: %w", err)
    }
    newAttributePool := NewAttributePoolTableManager()
    if err := newAttributePool.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload AttributePool failed: %w", err)
    }
    newAttributeRule := NewAttributeRuleTableManager()
    if err := newAttributeRule.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload AttributeRule failed: %w", err)
    }
    newBaseScene := NewBaseSceneTableManager()
    if err := newBaseScene.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload BaseScene failed: %w", err)
    }
    newBuff := NewBuffTableManager()
    if err := newBuff.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Buff failed: %w", err)
    }
    newClass := NewClassTableManager()
    if err := newClass.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Class failed: %w", err)
    }
    newCondition := NewConditionTableManager()
    if err := newCondition.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Condition failed: %w", err)
    }
    newCooldown := NewCooldownTableManager()
    if err := newCooldown.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Cooldown failed: %w", err)
    }
    newDungeon := NewDungeonTableManager()
    if err := newDungeon.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Dungeon failed: %w", err)
    }
    newEquipSlot := NewEquipSlotTableManager()
    if err := newEquipSlot.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload EquipSlot failed: %w", err)
    }
    newGlobalVariable := NewGlobalVariableTableManager()
    if err := newGlobalVariable.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload GlobalVariable failed: %w", err)
    }
    newItem := NewItemTableManager()
    if err := newItem.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Item failed: %w", err)
    }
    newMessageLimiter := NewMessageLimiterTableManager()
    if err := newMessageLimiter.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload MessageLimiter failed: %w", err)
    }
    newMirror := NewMirrorTableManager()
    if err := newMirror.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Mirror failed: %w", err)
    }
    newMission := NewMissionTableManager()
    if err := newMission.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Mission failed: %w", err)
    }
    newMonster := NewMonsterTableManager()
    if err := newMonster.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Monster failed: %w", err)
    }
    newReward := NewRewardTableManager()
    if err := newReward.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Reward failed: %w", err)
    }
    newSkill := NewSkillTableManager()
    if err := newSkill.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Skill failed: %w", err)
    }
    newSkillPermission := NewSkillPermissionTableManager()
    if err := newSkillPermission.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload SkillPermission failed: %w", err)
    }
    newTest := NewTestTableManager()
    if err := newTest.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload Test failed: %w", err)
    }
    newTestMultiKey := NewTestMultiKeyTableManager()
    if err := newTestMultiKey.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload TestMultiKey failed: %w", err)
    }
    newWorld := NewWorldTableManager()
    if err := newWorld.Load(configDir, useBinary); err != nil {
        return fmt.Errorf("reload World failed: %w", err)
    }

    // Swap all instances at once after all loads succeed.
    ActorActionCombatStateTableManagerInstance = newActorActionCombatState
    ActorActionStateTableManagerInstance = newActorActionState
    AttributeAutoPlanTableManagerInstance = newAttributeAutoPlan
    AttributeDimensionTableManagerInstance = newAttributeDimension
    AttributePoolTableManagerInstance = newAttributePool
    AttributeRuleTableManagerInstance = newAttributeRule
    BaseSceneTableManagerInstance = newBaseScene
    BuffTableManagerInstance = newBuff
    ClassTableManagerInstance = newClass
    ConditionTableManagerInstance = newCondition
    CooldownTableManagerInstance = newCooldown
    DungeonTableManagerInstance = newDungeon
    EquipSlotTableManagerInstance = newEquipSlot
    GlobalVariableTableManagerInstance = newGlobalVariable
    ItemTableManagerInstance = newItem
    MessageLimiterTableManagerInstance = newMessageLimiter
    MirrorTableManagerInstance = newMirror
    MissionTableManagerInstance = newMission
    MonsterTableManagerInstance = newMonster
    RewardTableManagerInstance = newReward
    SkillTableManagerInstance = newSkill
    SkillPermissionTableManagerInstance = newSkillPermission
    TestTableManagerInstance = newTest
    TestMultiKeyTableManagerInstance = newTestMultiKey
    WorldTableManagerInstance = newWorld

    if loadSuccessCallback != nil {
        loadSuccessCallback()
    }
    return nil
}
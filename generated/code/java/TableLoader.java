
package com.game.table;

import java.util.Set;
import java.util.concurrent.CountDownLatch;

/**
 * Auto-generated aggregated table loader.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TableLoader {

    public static final String SCHEMA_DIGEST = "88750c2d961d8710";

    private static final Set<String> HOT_RELOADABLE_TABLES = Set.of(
            "ActivitySchedule",
            "ActivityVitalityReward",
            "ActivityVitalityRule",
            "BattleProgressionReward",
            "BattleProgressionRule",
            "Buff",
            "Cooldown",
            "EquipAffixQuality",
            "FestivalBeastActivity",
            "FestivalBeastTier",
            "GuildDonate",
            "GuildShop",
            "GuildWarMilestone",
            "GuildWarRule",
            "JianghuRule",
            "MessageLimiter",
            "Monster",
            "MysteryMerchantGoods",
            "MysteryMerchantRule",
            "PlayerLevelExp",
            "RewardedAdSlot",
            "SeasonRule",
            "SeasonSeal",
            "Skill",
            "SkillPermission",
            "SkillScrollCraft",
            "SpiritShopGoods",
            "SpiritShopLimitGroup",
            "TravelPassSeason",
            "TravelPassTier",
            "TrialJournalEntry",
            "TrialJournalMilestone",
            "TrialJournalSeal"
    );

    public static boolean isHotReloadable(String tableName) {
        return tableName != null && HOT_RELOADABLE_TABLES.contains(tableName);
    }

    private static Runnable loadSuccessCallback;

    /**
     * Load all config tables synchronously.
     * @param useBinary true to load .pb (proto binary), false to load .json.
     */
    public static void loadTables(String configDir, boolean useBinary) throws Exception {

        ActivityScheduleTableManager.getInstance().load(configDir, useBinary);

        ActivityVitalityRewardTableManager.getInstance().load(configDir, useBinary);

        ActivityVitalityRuleTableManager.getInstance().load(configDir, useBinary);

        ActorActionCombatStateTableManager.getInstance().load(configDir, useBinary);

        ActorActionStateTableManager.getInstance().load(configDir, useBinary);

        AfkPassRuleTableManager.getInstance().load(configDir, useBinary);

        AppearanceRuleTableManager.getInstance().load(configDir, useBinary);

        AppearanceSeriesTableManager.getInstance().load(configDir, useBinary);

        AttributeAllocRatioTableManager.getInstance().load(configDir, useBinary);

        AttributeAutoPlanTableManager.getInstance().load(configDir, useBinary);

        AttributeDimensionTableManager.getInstance().load(configDir, useBinary);

        AttributePoolTableManager.getInstance().load(configDir, useBinary);

        AttributeRuleTableManager.getInstance().load(configDir, useBinary);

        BaseSceneTableManager.getInstance().load(configDir, useBinary);

        BattleProgressionRewardTableManager.getInstance().load(configDir, useBinary);

        BattleProgressionRuleTableManager.getInstance().load(configDir, useBinary);

        BuffTableManager.getInstance().load(configDir, useBinary);

        CharacterAppearanceTableManager.getInstance().load(configDir, useBinary);

        ClassTableManager.getInstance().load(configDir, useBinary);

        ConditionTableManager.getInstance().load(configDir, useBinary);

        CooldownTableManager.getInstance().load(configDir, useBinary);

        DungeonTableManager.getInstance().load(configDir, useBinary);

        EquipAffixPoolTableManager.getInstance().load(configDir, useBinary);

        EquipAffixQualityTableManager.getInstance().load(configDir, useBinary);

        EquipAffixRuleTableManager.getInstance().load(configDir, useBinary);

        EquipArtifactTableManager.getInstance().load(configDir, useBinary);

        EquipAttributeTableManager.getInstance().load(configDir, useBinary);

        EquipAttributeCapTableManager.getInstance().load(configDir, useBinary);

        EquipEnhanceTableManager.getInstance().load(configDir, useBinary);

        EquipGemTableManager.getInstance().load(configDir, useBinary);

        EquipGemSocketTableManager.getInstance().load(configDir, useBinary);

        EquipSlotTableManager.getInstance().load(configDir, useBinary);

        EquipV1AffixTableManager.getInstance().load(configDir, useBinary);

        FestivalBeastActivityTableManager.getInstance().load(configDir, useBinary);

        FestivalBeastTierTableManager.getInstance().load(configDir, useBinary);

        GlobalVariableTableManager.getInstance().load(configDir, useBinary);

        GuildActivityTableManager.getInstance().load(configDir, useBinary);

        GuildDonateTableManager.getInstance().load(configDir, useBinary);

        GuildLevelTableManager.getInstance().load(configDir, useBinary);

        GuildRuleTableManager.getInstance().load(configDir, useBinary);

        GuildShopTableManager.getInstance().load(configDir, useBinary);

        GuildWarMilestoneTableManager.getInstance().load(configDir, useBinary);

        GuildWarRuleTableManager.getInstance().load(configDir, useBinary);

        ItemTableManager.getInstance().load(configDir, useBinary);

        JianghuRuleTableManager.getInstance().load(configDir, useBinary);

        MessageLimiterTableManager.getInstance().load(configDir, useBinary);

        MirrorTableManager.getInstance().load(configDir, useBinary);

        MissionTableManager.getInstance().load(configDir, useBinary);

        MonsterTableManager.getInstance().load(configDir, useBinary);

        MysteryMerchantGoodsTableManager.getInstance().load(configDir, useBinary);

        MysteryMerchantRuleTableManager.getInstance().load(configDir, useBinary);

        MysteryMerchantSpotTableManager.getInstance().load(configDir, useBinary);

        PetTableManager.getInstance().load(configDir, useBinary);

        PetCosmeticTableManager.getInstance().load(configDir, useBinary);

        PetEntryTableManager.getInstance().load(configDir, useBinary);

        PetRuleTableManager.getInstance().load(configDir, useBinary);

        PetTierTableManager.getInstance().load(configDir, useBinary);

        PlayModeTableManager.getInstance().load(configDir, useBinary);

        PlayerLevelExpTableManager.getInstance().load(configDir, useBinary);

        RankedPresetTableManager.getInstance().load(configDir, useBinary);

        RankedRuleTableManager.getInstance().load(configDir, useBinary);

        RewardTableManager.getInstance().load(configDir, useBinary);

        RewardedAdSlotTableManager.getInstance().load(configDir, useBinary);

        RoleNameRuleTableManager.getInstance().load(configDir, useBinary);

        SeasonRuleTableManager.getInstance().load(configDir, useBinary);

        SeasonSealTableManager.getInstance().load(configDir, useBinary);

        SkillTableManager.getInstance().load(configDir, useBinary);

        SkillPermissionTableManager.getInstance().load(configDir, useBinary);

        SkillScrollCraftTableManager.getInstance().load(configDir, useBinary);

        SkillVfxCosmeticTableManager.getInstance().load(configDir, useBinary);

        SpiritShopGoodsTableManager.getInstance().load(configDir, useBinary);

        SpiritShopLimitGroupTableManager.getInstance().load(configDir, useBinary);

        TestTableManager.getInstance().load(configDir, useBinary);

        TestMultiKeyTableManager.getInstance().load(configDir, useBinary);

        TournamentRewardTableManager.getInstance().load(configDir, useBinary);

        TournamentRuleTableManager.getInstance().load(configDir, useBinary);

        TravelPassSeasonTableManager.getInstance().load(configDir, useBinary);

        TravelPassTierTableManager.getInstance().load(configDir, useBinary);

        TrialJournalEntryTableManager.getInstance().load(configDir, useBinary);

        TrialJournalMilestoneTableManager.getInstance().load(configDir, useBinary);

        TrialJournalSealTableManager.getInstance().load(configDir, useBinary);

        WorldTableManager.getInstance().load(configDir, useBinary);

        ZoneGroupTableManager.getInstance().load(configDir, useBinary);

        if (loadSuccessCallback != null) {
            loadSuccessCallback.run();
        }
    }

    /** Backward-compatible overload — defaults to JSON. */
    public static void loadTables(String configDir) throws Exception {
        loadTables(configDir, false);
    }

    /**
     * Load all config tables asynchronously using threads.
     * @param useBinary true to load .pb (proto binary), false to load .json.
     */
    public static void loadTablesAsync(String configDir, boolean useBinary) throws Exception {
        CountDownLatch latch = new CountDownLatch(83);

        new Thread(() -> {
            try {
                ActivityScheduleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load ActivitySchedule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ActivityVitalityRewardTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load ActivityVitalityReward table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ActivityVitalityRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load ActivityVitalityRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ActorActionCombatStateTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load ActorActionCombatState table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ActorActionStateTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load ActorActionState table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AfkPassRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AfkPassRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AppearanceRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AppearanceRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AppearanceSeriesTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AppearanceSeries table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AttributeAllocRatioTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AttributeAllocRatio table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AttributeAutoPlanTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AttributeAutoPlan table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AttributeDimensionTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AttributeDimension table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AttributePoolTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AttributePool table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                AttributeRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load AttributeRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                BaseSceneTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load BaseScene table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                BattleProgressionRewardTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load BattleProgressionReward table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                BattleProgressionRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load BattleProgressionRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                BuffTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Buff table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                CharacterAppearanceTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load CharacterAppearance table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ClassTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Class table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ConditionTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Condition table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                CooldownTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Cooldown table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                DungeonTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Dungeon table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipAffixPoolTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipAffixPool table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipAffixQualityTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipAffixQuality table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipAffixRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipAffixRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipArtifactTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipArtifact table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipAttributeTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipAttribute table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipAttributeCapTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipAttributeCap table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipEnhanceTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipEnhance table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipGemTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipGem table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipGemSocketTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipGemSocket table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipSlotTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipSlot table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                EquipV1AffixTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load EquipV1Affix table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                FestivalBeastActivityTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load FestivalBeastActivity table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                FestivalBeastTierTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load FestivalBeastTier table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GlobalVariableTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GlobalVariable table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildActivityTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildActivity table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildDonateTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildDonate table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildLevelTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildLevel table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildShopTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildShop table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildWarMilestoneTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildWarMilestone table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                GuildWarRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load GuildWarRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ItemTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Item table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                JianghuRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load JianghuRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MessageLimiterTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load MessageLimiter table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MirrorTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Mirror table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MissionTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Mission table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MonsterTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Monster table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MysteryMerchantGoodsTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load MysteryMerchantGoods table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MysteryMerchantRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load MysteryMerchantRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                MysteryMerchantSpotTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load MysteryMerchantSpot table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PetTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Pet table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PetCosmeticTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load PetCosmetic table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PetEntryTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load PetEntry table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PetRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load PetRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PetTierTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load PetTier table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PlayModeTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load PlayMode table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                PlayerLevelExpTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load PlayerLevelExp table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                RankedPresetTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load RankedPreset table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                RankedRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load RankedRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                RewardTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Reward table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                RewardedAdSlotTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load RewardedAdSlot table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                RoleNameRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load RoleNameRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SeasonRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SeasonRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SeasonSealTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SeasonSeal table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SkillTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Skill table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SkillPermissionTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SkillPermission table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SkillScrollCraftTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SkillScrollCraft table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SkillVfxCosmeticTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SkillVfxCosmetic table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SpiritShopGoodsTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SpiritShopGoods table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                SpiritShopLimitGroupTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load SpiritShopLimitGroup table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TestTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load Test table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TestMultiKeyTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TestMultiKey table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TournamentRewardTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TournamentReward table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TournamentRuleTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TournamentRule table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TravelPassSeasonTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TravelPassSeason table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TravelPassTierTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TravelPassTier table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TrialJournalEntryTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TrialJournalEntry table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TrialJournalMilestoneTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TrialJournalMilestone table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                TrialJournalSealTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load TrialJournalSeal table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                WorldTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load World table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        new Thread(() -> {
            try {
                ZoneGroupTableManager.getInstance().load(configDir, useBinary);
            } catch (Exception e) {
                throw new RuntimeException("Failed to load ZoneGroup table", e);
            } finally {
                latch.countDown();
            }
        }).start();

        latch.await();

        if (loadSuccessCallback != null) {
            loadSuccessCallback.run();
        }
    }

    /**
     * Register a callback invoked after all tables load.
     */
    public static void onTablesLoadSuccess(Runnable callback) {
        loadSuccessCallback = callback;
    }

    /**
     * Reload all config tables (hot-reload safe).
     * @param useBinary true to load .pb (proto binary), false to load .json.
     */
    public static void reloadTables(String configDir, boolean useBinary) throws Exception {

        ActivityScheduleTableManager.getInstance().load(configDir, useBinary);

        ActivityVitalityRewardTableManager.getInstance().load(configDir, useBinary);

        ActivityVitalityRuleTableManager.getInstance().load(configDir, useBinary);

        ActorActionCombatStateTableManager.getInstance().load(configDir, useBinary);

        ActorActionStateTableManager.getInstance().load(configDir, useBinary);

        AfkPassRuleTableManager.getInstance().load(configDir, useBinary);

        AppearanceRuleTableManager.getInstance().load(configDir, useBinary);

        AppearanceSeriesTableManager.getInstance().load(configDir, useBinary);

        AttributeAllocRatioTableManager.getInstance().load(configDir, useBinary);

        AttributeAutoPlanTableManager.getInstance().load(configDir, useBinary);

        AttributeDimensionTableManager.getInstance().load(configDir, useBinary);

        AttributePoolTableManager.getInstance().load(configDir, useBinary);

        AttributeRuleTableManager.getInstance().load(configDir, useBinary);

        BaseSceneTableManager.getInstance().load(configDir, useBinary);

        BattleProgressionRewardTableManager.getInstance().load(configDir, useBinary);

        BattleProgressionRuleTableManager.getInstance().load(configDir, useBinary);

        BuffTableManager.getInstance().load(configDir, useBinary);

        CharacterAppearanceTableManager.getInstance().load(configDir, useBinary);

        ClassTableManager.getInstance().load(configDir, useBinary);

        ConditionTableManager.getInstance().load(configDir, useBinary);

        CooldownTableManager.getInstance().load(configDir, useBinary);

        DungeonTableManager.getInstance().load(configDir, useBinary);

        EquipAffixPoolTableManager.getInstance().load(configDir, useBinary);

        EquipAffixQualityTableManager.getInstance().load(configDir, useBinary);

        EquipAffixRuleTableManager.getInstance().load(configDir, useBinary);

        EquipArtifactTableManager.getInstance().load(configDir, useBinary);

        EquipAttributeTableManager.getInstance().load(configDir, useBinary);

        EquipAttributeCapTableManager.getInstance().load(configDir, useBinary);

        EquipEnhanceTableManager.getInstance().load(configDir, useBinary);

        EquipGemTableManager.getInstance().load(configDir, useBinary);

        EquipGemSocketTableManager.getInstance().load(configDir, useBinary);

        EquipSlotTableManager.getInstance().load(configDir, useBinary);

        EquipV1AffixTableManager.getInstance().load(configDir, useBinary);

        FestivalBeastActivityTableManager.getInstance().load(configDir, useBinary);

        FestivalBeastTierTableManager.getInstance().load(configDir, useBinary);

        GlobalVariableTableManager.getInstance().load(configDir, useBinary);

        GuildActivityTableManager.getInstance().load(configDir, useBinary);

        GuildDonateTableManager.getInstance().load(configDir, useBinary);

        GuildLevelTableManager.getInstance().load(configDir, useBinary);

        GuildRuleTableManager.getInstance().load(configDir, useBinary);

        GuildShopTableManager.getInstance().load(configDir, useBinary);

        GuildWarMilestoneTableManager.getInstance().load(configDir, useBinary);

        GuildWarRuleTableManager.getInstance().load(configDir, useBinary);

        ItemTableManager.getInstance().load(configDir, useBinary);

        JianghuRuleTableManager.getInstance().load(configDir, useBinary);

        MessageLimiterTableManager.getInstance().load(configDir, useBinary);

        MirrorTableManager.getInstance().load(configDir, useBinary);

        MissionTableManager.getInstance().load(configDir, useBinary);

        MonsterTableManager.getInstance().load(configDir, useBinary);

        MysteryMerchantGoodsTableManager.getInstance().load(configDir, useBinary);

        MysteryMerchantRuleTableManager.getInstance().load(configDir, useBinary);

        MysteryMerchantSpotTableManager.getInstance().load(configDir, useBinary);

        PetTableManager.getInstance().load(configDir, useBinary);

        PetCosmeticTableManager.getInstance().load(configDir, useBinary);

        PetEntryTableManager.getInstance().load(configDir, useBinary);

        PetRuleTableManager.getInstance().load(configDir, useBinary);

        PetTierTableManager.getInstance().load(configDir, useBinary);

        PlayModeTableManager.getInstance().load(configDir, useBinary);

        PlayerLevelExpTableManager.getInstance().load(configDir, useBinary);

        RankedPresetTableManager.getInstance().load(configDir, useBinary);

        RankedRuleTableManager.getInstance().load(configDir, useBinary);

        RewardTableManager.getInstance().load(configDir, useBinary);

        RewardedAdSlotTableManager.getInstance().load(configDir, useBinary);

        RoleNameRuleTableManager.getInstance().load(configDir, useBinary);

        SeasonRuleTableManager.getInstance().load(configDir, useBinary);

        SeasonSealTableManager.getInstance().load(configDir, useBinary);

        SkillTableManager.getInstance().load(configDir, useBinary);

        SkillPermissionTableManager.getInstance().load(configDir, useBinary);

        SkillScrollCraftTableManager.getInstance().load(configDir, useBinary);

        SkillVfxCosmeticTableManager.getInstance().load(configDir, useBinary);

        SpiritShopGoodsTableManager.getInstance().load(configDir, useBinary);

        SpiritShopLimitGroupTableManager.getInstance().load(configDir, useBinary);

        TestTableManager.getInstance().load(configDir, useBinary);

        TestMultiKeyTableManager.getInstance().load(configDir, useBinary);

        TournamentRewardTableManager.getInstance().load(configDir, useBinary);

        TournamentRuleTableManager.getInstance().load(configDir, useBinary);

        TravelPassSeasonTableManager.getInstance().load(configDir, useBinary);

        TravelPassTierTableManager.getInstance().load(configDir, useBinary);

        TrialJournalEntryTableManager.getInstance().load(configDir, useBinary);

        TrialJournalMilestoneTableManager.getInstance().load(configDir, useBinary);

        TrialJournalSealTableManager.getInstance().load(configDir, useBinary);

        WorldTableManager.getInstance().load(configDir, useBinary);

        ZoneGroupTableManager.getInstance().load(configDir, useBinary);

        if (loadSuccessCallback != null) {
            loadSuccessCallback.run();
        }
    }

    /** Backward-compatible overload — defaults to JSON. */
    public static void reloadTables(String configDir) throws Exception {
        reloadTables(configDir, false);
    }
}

#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <string_view>

struct ActivityScheduleTableSnapshot;
struct ActivityVitalityRewardTableSnapshot;
struct ActivityVitalityRuleTableSnapshot;
struct ActorActionCombatStateTableSnapshot;
struct ActorActionStateTableSnapshot;
struct AfkPassRuleTableSnapshot;
struct AppearanceRuleTableSnapshot;
struct AppearanceSeriesTableSnapshot;
struct AttributeAllocRatioTableSnapshot;
struct AttributeAutoPlanTableSnapshot;
struct AttributeDimensionTableSnapshot;
struct AttributePoolTableSnapshot;
struct AttributeRuleTableSnapshot;
struct BaseSceneTableSnapshot;
struct BattleProgressionRewardTableSnapshot;
struct BattleProgressionRuleTableSnapshot;
struct BuffTableSnapshot;
struct CharacterAppearanceTableSnapshot;
struct ClassTableSnapshot;
struct ConditionTableSnapshot;
struct CooldownTableSnapshot;
struct DungeonTableSnapshot;
struct EquipAffixPoolTableSnapshot;
struct EquipAffixQualityTableSnapshot;
struct EquipAffixRuleTableSnapshot;
struct EquipArtifactTableSnapshot;
struct EquipAttributeTableSnapshot;
struct EquipAttributeCapTableSnapshot;
struct EquipEnhanceTableSnapshot;
struct EquipGemTableSnapshot;
struct EquipGemSocketTableSnapshot;
struct EquipSlotTableSnapshot;
struct EquipV1AffixTableSnapshot;
struct FestivalBeastActivityTableSnapshot;
struct FestivalBeastTierTableSnapshot;
struct GlobalVariableTableSnapshot;
struct GuildActivityTableSnapshot;
struct GuildDonateTableSnapshot;
struct GuildLevelTableSnapshot;
struct GuildRuleTableSnapshot;
struct GuildShopTableSnapshot;
struct GuildWarMilestoneTableSnapshot;
struct GuildWarRuleTableSnapshot;
struct ItemTableSnapshot;
struct MessageLimiterTableSnapshot;
struct MirrorTableSnapshot;
struct MissionTableSnapshot;
struct MonsterTableSnapshot;
struct MysteryMerchantGoodsTableSnapshot;
struct MysteryMerchantRuleTableSnapshot;
struct MysteryMerchantSpotTableSnapshot;
struct PetTableSnapshot;
struct PetCosmeticTableSnapshot;
struct PetEntryTableSnapshot;
struct PetRuleTableSnapshot;
struct PetTierTableSnapshot;
struct PlayModeTableSnapshot;
struct PlayerLevelExpTableSnapshot;
struct RankedPresetTableSnapshot;
struct RankedRuleTableSnapshot;
struct RewardTableSnapshot;
struct RewardedAdSlotTableSnapshot;
struct RoleNameRuleTableSnapshot;
struct SeasonRuleTableSnapshot;
struct SeasonSealTableSnapshot;
struct SkillTableSnapshot;
struct SkillPermissionTableSnapshot;
struct SkillScrollCraftTableSnapshot;
struct SkillVfxCosmeticTableSnapshot;
struct SpiritShopGoodsTableSnapshot;
struct SpiritShopLimitGroupTableSnapshot;
struct TestTableSnapshot;
struct TestMultiKeyTableSnapshot;
struct TournamentRewardTableSnapshot;
struct TournamentRuleTableSnapshot;
struct TravelPassSeasonTableSnapshot;
struct TravelPassTierTableSnapshot;
struct TrialJournalEntryTableSnapshot;
struct TrialJournalMilestoneTableSnapshot;
struct TrialJournalSealTableSnapshot;
struct WorldTableSnapshot;
struct ZoneGroupTableSnapshot;


using LoadSuccessCallback = std::function<void()>;

// 整批服务端配置表结构摘要(16 位小写十六进制,docs/design/config-table-hot-reload.md §5.1)。
// 运行期只加载 schema_digest 与本常量一致的表包。
inline constexpr std::string_view kTableSchemaDigest = "8a563f7a854d5bc4";

// 一次加载或热更表包的身份元数据。
struct TableBundleIdentity {
    std::string schemaDigest;
    std::string contentDigest;
    uint64_t version = 0;
    uint64_t loadSeq = 0;
    std::string source; // "image" 或 "bundle"
};

// 一整套不可变配置表快照集合:提交时整体替换现役管理器,战斗房间可显式持有保活至终局。
struct TableSet {
    TableBundleIdentity identity;

    std::shared_ptr<const ActivityScheduleTableSnapshot> activityScheduleTable;

    std::shared_ptr<const ActivityVitalityRewardTableSnapshot> activityVitalityRewardTable;

    std::shared_ptr<const ActivityVitalityRuleTableSnapshot> activityVitalityRuleTable;

    std::shared_ptr<const ActorActionCombatStateTableSnapshot> actorActionCombatStateTable;

    std::shared_ptr<const ActorActionStateTableSnapshot> actorActionStateTable;

    std::shared_ptr<const AfkPassRuleTableSnapshot> afkPassRuleTable;

    std::shared_ptr<const AppearanceRuleTableSnapshot> appearanceRuleTable;

    std::shared_ptr<const AppearanceSeriesTableSnapshot> appearanceSeriesTable;

    std::shared_ptr<const AttributeAllocRatioTableSnapshot> attributeAllocRatioTable;

    std::shared_ptr<const AttributeAutoPlanTableSnapshot> attributeAutoPlanTable;

    std::shared_ptr<const AttributeDimensionTableSnapshot> attributeDimensionTable;

    std::shared_ptr<const AttributePoolTableSnapshot> attributePoolTable;

    std::shared_ptr<const AttributeRuleTableSnapshot> attributeRuleTable;

    std::shared_ptr<const BaseSceneTableSnapshot> baseSceneTable;

    std::shared_ptr<const BattleProgressionRewardTableSnapshot> battleProgressionRewardTable;

    std::shared_ptr<const BattleProgressionRuleTableSnapshot> battleProgressionRuleTable;

    std::shared_ptr<const BuffTableSnapshot> buffTable;

    std::shared_ptr<const CharacterAppearanceTableSnapshot> characterAppearanceTable;

    std::shared_ptr<const ClassTableSnapshot> classTable;

    std::shared_ptr<const ConditionTableSnapshot> conditionTable;

    std::shared_ptr<const CooldownTableSnapshot> cooldownTable;

    std::shared_ptr<const DungeonTableSnapshot> dungeonTable;

    std::shared_ptr<const EquipAffixPoolTableSnapshot> equipAffixPoolTable;

    std::shared_ptr<const EquipAffixQualityTableSnapshot> equipAffixQualityTable;

    std::shared_ptr<const EquipAffixRuleTableSnapshot> equipAffixRuleTable;

    std::shared_ptr<const EquipArtifactTableSnapshot> equipArtifactTable;

    std::shared_ptr<const EquipAttributeTableSnapshot> equipAttributeTable;

    std::shared_ptr<const EquipAttributeCapTableSnapshot> equipAttributeCapTable;

    std::shared_ptr<const EquipEnhanceTableSnapshot> equipEnhanceTable;

    std::shared_ptr<const EquipGemTableSnapshot> equipGemTable;

    std::shared_ptr<const EquipGemSocketTableSnapshot> equipGemSocketTable;

    std::shared_ptr<const EquipSlotTableSnapshot> equipSlotTable;

    std::shared_ptr<const EquipV1AffixTableSnapshot> equipV1AffixTable;

    std::shared_ptr<const FestivalBeastActivityTableSnapshot> festivalBeastActivityTable;

    std::shared_ptr<const FestivalBeastTierTableSnapshot> festivalBeastTierTable;

    std::shared_ptr<const GlobalVariableTableSnapshot> globalVariableTable;

    std::shared_ptr<const GuildActivityTableSnapshot> guildActivityTable;

    std::shared_ptr<const GuildDonateTableSnapshot> guildDonateTable;

    std::shared_ptr<const GuildLevelTableSnapshot> guildLevelTable;

    std::shared_ptr<const GuildRuleTableSnapshot> guildRuleTable;

    std::shared_ptr<const GuildShopTableSnapshot> guildShopTable;

    std::shared_ptr<const GuildWarMilestoneTableSnapshot> guildWarMilestoneTable;

    std::shared_ptr<const GuildWarRuleTableSnapshot> guildWarRuleTable;

    std::shared_ptr<const ItemTableSnapshot> itemTable;

    std::shared_ptr<const MessageLimiterTableSnapshot> messageLimiterTable;

    std::shared_ptr<const MirrorTableSnapshot> mirrorTable;

    std::shared_ptr<const MissionTableSnapshot> missionTable;

    std::shared_ptr<const MonsterTableSnapshot> monsterTable;

    std::shared_ptr<const MysteryMerchantGoodsTableSnapshot> mysteryMerchantGoodsTable;

    std::shared_ptr<const MysteryMerchantRuleTableSnapshot> mysteryMerchantRuleTable;

    std::shared_ptr<const MysteryMerchantSpotTableSnapshot> mysteryMerchantSpotTable;

    std::shared_ptr<const PetTableSnapshot> petTable;

    std::shared_ptr<const PetCosmeticTableSnapshot> petCosmeticTable;

    std::shared_ptr<const PetEntryTableSnapshot> petEntryTable;

    std::shared_ptr<const PetRuleTableSnapshot> petRuleTable;

    std::shared_ptr<const PetTierTableSnapshot> petTierTable;

    std::shared_ptr<const PlayModeTableSnapshot> playModeTable;

    std::shared_ptr<const PlayerLevelExpTableSnapshot> playerLevelExpTable;

    std::shared_ptr<const RankedPresetTableSnapshot> rankedPresetTable;

    std::shared_ptr<const RankedRuleTableSnapshot> rankedRuleTable;

    std::shared_ptr<const RewardTableSnapshot> rewardTable;

    std::shared_ptr<const RewardedAdSlotTableSnapshot> rewardedAdSlotTable;

    std::shared_ptr<const RoleNameRuleTableSnapshot> roleNameRuleTable;

    std::shared_ptr<const SeasonRuleTableSnapshot> seasonRuleTable;

    std::shared_ptr<const SeasonSealTableSnapshot> seasonSealTable;

    std::shared_ptr<const SkillTableSnapshot> skillTable;

    std::shared_ptr<const SkillPermissionTableSnapshot> skillPermissionTable;

    std::shared_ptr<const SkillScrollCraftTableSnapshot> skillScrollCraftTable;

    std::shared_ptr<const SkillVfxCosmeticTableSnapshot> skillVfxCosmeticTable;

    std::shared_ptr<const SpiritShopGoodsTableSnapshot> spiritShopGoodsTable;

    std::shared_ptr<const SpiritShopLimitGroupTableSnapshot> spiritShopLimitGroupTable;

    std::shared_ptr<const TestTableSnapshot> testTable;

    std::shared_ptr<const TestMultiKeyTableSnapshot> testMultiKeyTable;

    std::shared_ptr<const TournamentRewardTableSnapshot> tournamentRewardTable;

    std::shared_ptr<const TournamentRuleTableSnapshot> tournamentRuleTable;

    std::shared_ptr<const TravelPassSeasonTableSnapshot> travelPassSeasonTable;

    std::shared_ptr<const TravelPassTierTableSnapshot> travelPassTierTable;

    std::shared_ptr<const TrialJournalEntryTableSnapshot> trialJournalEntryTable;

    std::shared_ptr<const TrialJournalMilestoneTableSnapshot> trialJournalMilestoneTable;

    std::shared_ptr<const TrialJournalSealTableSnapshot> trialJournalSealTable;

    std::shared_ptr<const WorldTableSnapshot> worldTable;

    std::shared_ptr<const ZoneGroupTableSnapshot> zoneGroupTable;


    TableSet();
    ~TableSet();
    TableSet(const TableSet&);
    TableSet& operator=(const TableSet&);
    TableSet(TableSet&&) noexcept;
    TableSet& operator=(TableSet&&) noexcept;
};

// 主循环单线程校验作用域:在构造期间把 staged 快照临时装入各表管理器以便复用现有校验函数,
// 析构时无条件还原先前的现役快照。
class ScopedTableSetOverride {
public:
    explicit ScopedTableSetOverride(const std::shared_ptr<const TableSet>& staged);
    ~ScopedTableSetOverride();

    ScopedTableSetOverride(const ScopedTableSetOverride&) = delete;
    ScopedTableSetOverride& operator=(const ScopedTableSetOverride&) = delete;
    ScopedTableSetOverride(ScopedTableSetOverride&&) = delete;
    ScopedTableSetOverride& operator=(ScopedTableSetOverride&&) = delete;

private:
    std::shared_ptr<const TableSet> previous;
};

void LoadTables();

/// 并行加载全部表:工作线程数不超过 CPU 核数,阻塞到全部加载完、LoadSuccess 与回调都跑完才返回,
/// 返回时工作线程已全部退出。名字里的 Async 指并行,不是「返回后还在后台加载」。
/// 可以重复调用;加载会整批换掉各表的快照,调用期间不能有别的线程读表。
void LoadTablesAsync();

void OnTablesLoadSuccess(const LoadSuccessCallback& callback);

/// 显式触发首次表加载完成后的全部单表 LoadSuccess 与全局 OnTablesLoadSuccess 回调(供表包启动路径调用)。
void InvokeTablesLoadSuccess();

/// 返回按本二进制编译期名单判定该表是否允许运行期热更(cfg_hot_reload)。
bool IsTableHotReloadable(std::string_view tableName);

std::size_t GetTableCount();
std::string_view GetTableNameAt(std::size_t index);

/// 分片准备接口:解析第 index 张表写入 target,不触碰现役表也不 LOG_FATAL。
bool LoadTableIntoSetAt(std::size_t index,
                        const std::string& configDir,
                        bool useBinary,
                        TableSet& target,
                        std::string& err);

/// 同步准备一整套 TableSet(供 --validate-tables 与单测使用)。
bool PrepareTableSetSync(const std::string& configDir,
                         bool useBinary,
                         std::shared_ptr<TableSet>& out,
                         std::string& err);

/// 获取当前现役的整套不可变 TableSet。
std::shared_ptr<const TableSet> CurrentTableSet();

/// 整批提交新 TableSet,返回先前现役的 TableSet 供调用方放入延迟退役列表。
std::shared_ptr<const TableSet> CommitTableSet(std::shared_ptr<TableSet> next);

/// 更新当前 TableSet 身份元数据(供启动期解析完 manifest 后补齐摘要/版本/来源)。
void SetCurrentTableBundleIdentity(const TableBundleIdentity& identity);

/// Reload all tables and bump the version counter.
/// WARNING: after reload, any string_view/span components built from old
///          proto data are INVALID.  Rebuild all components immediately.
void ReloadTables();

/// Monotonically-increasing version; incremented by CommitTableSet() / ReloadTables().
/// Use to detect stale cached components.
uint64_t GetTableLoadVersion();
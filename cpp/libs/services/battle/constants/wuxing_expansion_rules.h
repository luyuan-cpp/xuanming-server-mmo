#pragma once

#include <algorithm>
#include <array>
#include <cstdint>
#include <vector>

namespace turnbattle {
namespace wuxing {

// 五行枚举：1=金, 2=木, 3=水, 4=火, 5=土（与 TournamentLeylineElement 顺序逐项一致）。
inline constexpr uint32_t kElementNone  = 0u;
inline constexpr uint32_t kElementMetal = 1u;
inline constexpr uint32_t kElementWood  = 2u;
inline constexpr uint32_t kElementWater = 3u;
inline constexpr uint32_t kElementFire  = 4u;
inline constexpr uint32_t kElementEarth = 5u;

inline constexpr uint32_t kMaxFormationId = 5u;
inline constexpr uint32_t kTowerMaxFloors = 10u;
inline constexpr uint32_t kRealmV2Version = 2u;
inline constexpr uint32_t kRealmV2MaxNodes = 6u;
inline constexpr uint32_t kRealmV2ResonanceThreshold = 3u;
inline constexpr uint32_t kTowerOfferCount = 3u;
inline constexpr uint32_t kMaxSavedBuildBookmarks = 12u;
inline constexpr uint32_t kSynergyIntervalRounds = 3u;
inline constexpr uint32_t kSynergyBonusPct = 10u;
inline constexpr uint32_t kFormationCounterBonusPct = 5u;
inline constexpr uint64_t kFireCritBurnCapPerHit = 1500ULL;

// 五行幻境 V2 节点类型与非战斗事件枚举（与 player_season_comp.proto / player_season.proto 严格对齐）。
inline constexpr uint32_t kRealmNodeNeedBranch = 0u; // 第 2 / 4 关待选分岔
inline constexpr uint32_t kRealmNodeNormal     = 1u; // 普通战
inline constexpr uint32_t kRealmNodeElite      = 2u; // 精英强敌（必出灵/仙奇策）
inline constexpr uint32_t kRealmNodeBoss       = 3u; // 第 6 关终局首领
inline constexpr uint32_t kRealmNodeEvent      = 4u; // 第 2 关右路：奇遇事件
inline constexpr uint32_t kRealmNodeRest       = 5u; // 第 4 关右路：秘境休整

inline constexpr uint32_t kRealmEventNone      = 0u;
inline constexpr uint32_t kRealmEventEncounter = 1u;
inline constexpr uint32_t kRealmEventRest      = 2u;

inline constexpr uint32_t kRealmBranchElite    = 1u; // 左路：精英强敌
inline constexpr uint32_t kRealmBranchSpecial  = 2u; // 右路：奇遇事件 / 秘境休整

// 五行幻境 V2 15 枚机制改造型五行奇策 ID（5 行 × 3 档，号段 601..643，与 V1 101..503 物理隔离）。
inline constexpr uint32_t kBoonMetalBreakRefund   = 601u; // 金·凡：破盾返还 50% 法力并追加 60% 斩击
inline constexpr uint32_t kBoonMetalCritSplash    = 602u; // 金·灵：暴击向随机另一存活敌方溅射 40% 实伤
inline constexpr uint32_t kBoonMetalFirstPierce   = 603u; // 金·仙：每回合己方首次直击无视 35% 防御（+35% 穿透增伤）
inline constexpr uint32_t kBoonWoodDotSpread      = 611u; // 木·凡：敌方持续伤害跳字向其余存活敌方传染 50%
inline constexpr uint32_t kBoonWoodHealEmpower    = 612u; // 木·灵：获得治疗或护盾后下次直击 +20% 伤害
inline constexpr uint32_t kBoonWoodPetFollowUp    = 613u; // 木·仙：主角普攻/单体技能后出战灵宠免费追击 70% 普攻（每回合 1 次）
inline constexpr uint32_t kBoonWaterFreeFirstCast = 621u; // 水·凡：每回合己方首招主动技能法力消耗降至 0
inline constexpr uint32_t kBoonWaterExtendControl = 622u; // 水·灵：硬控/封印成功持续 +1 回合；免疫则附加 2 回合 18% 易伤
inline constexpr uint32_t kBoonWaterSkillEcho     = 623u; // 水·仙：主角每第 3 次施放主动进攻技能额外以 55% 威力连发 1 次
inline constexpr uint32_t kBoonFireShieldBlast    = 631u; // 火·凡：任意护盾碎裂时对敌方全体造成护盾上限 30% 真伤（封顶 1800）
inline constexpr uint32_t kBoonFireLowHpFury      = 632u; // 火·灵：气血低于 60% 时按已损气血比例获至高 +35% 直伤与 +20% 暴击
inline constexpr uint32_t kBoonFireRoundEndBurn   = 633u; // 火·仙：回合末对最高气血敌人叠 1 层灼伤（至多 3 层，每层 4% 上限 1500）
inline constexpr uint32_t kBoonEarthDoubleReflect = 641u; // 土·凡：反震伤害翻倍（×200%）且附 2 回合破甲（降防 15%）
inline constexpr uint32_t kBoonEarthClutchShield  = 642u; // 土·灵：每战首次气血跌破 35% 自动获得 25% 最大气血护盾
inline constexpr uint32_t kBoonEarthShieldStrike  = 643u; // 土·仙：开局获 18% 护盾，持盾期间攻击附加当前护盾 25% 伤害（封顶 1200）

inline constexpr uint64_t kBoonShieldShatterDamageCap = 1800ULL;
inline constexpr uint64_t kBoonShieldAttackBonusCap   = 1200ULL;
inline constexpr uint64_t kBoonRoundEndBurnCap        = 1500ULL;
inline constexpr uint32_t kBoonMaxProcDepth           = 1u;

// 公平 3x3 试炼预设白名单（与 RankedPreset 101..103 / 201..203 严格一致）。
inline constexpr uint32_t kDefaultFairCharPresetId = 101u;
inline constexpr uint32_t kDefaultFairPetPresetId  = 201u;
constexpr bool IsValidFairCharPresetId(uint32_t id) { return id >= 101u && id <= 103u; }
constexpr bool IsValidFairPetPresetId(uint32_t id)  { return id >= 201u && id <= 203u; }

constexpr uint32_t ElementOfClass(uint32_t classId) {
    return classId == 0u ? kElementNone : ((classId - 1u) % 5u) + 1u;
}

constexpr uint32_t ElementOfPet(uint32_t petTableId) {
    return petTableId == 0u ? kElementNone : ((petTableId - 1u) % 5u) + 1u;
}

// 五行相生链：金(1)生水(3) → 水(3)生木(2) → 木(2)生火(4) → 火(4)生土(5) → 土(5)生金(1)。
constexpr bool ElementGenerates(uint32_t producer, uint32_t receiver) {
    return (producer == kElementMetal && receiver == kElementWater) ||
           (producer == kElementWater && receiver == kElementWood) ||
           (producer == kElementWood && receiver == kElementFire) ||
           (producer == kElementFire && receiver == kElementEarth) ||
           (producer == kElementEarth && receiver == kElementMetal);
}

constexpr bool AreElementsMutuallyGenerating(uint32_t first, uint32_t second) {
    return ElementGenerates(first, second) || ElementGenerates(second, first);
}

constexpr bool IsSynergyRound(uint32_t roundIndex) {
    return roundIndex > 0u && (roundIndex % kSynergyIntervalRounds) == 0u;
}

// ---- 五套免费道家阵法（零数值付费，全员免费切换） ----
struct WuxingFormationDef {
    uint32_t id = 0u;
    uint32_t element = kElementNone;
    uint32_t countersId = 0u;
    uint32_t damageBonusPct = 0u;
    uint32_t damageTakenReducePct = 0u;
    uint32_t speedBonusPct = 0u;
    uint32_t healShieldBonusPct = 0u;
    uint32_t controlResistPct = 0u;
    uint32_t roundManaRegenPct = 0u;
    uint32_t critBonusPct = 0u;
    uint32_t petDamageBonusPct = 0u;
    uint32_t openingShieldPct = 0u;
};

inline constexpr std::array<WuxingFormationDef, 5> kFormations = {{
    // 1 天覆阵（金）：直伤 +5%、速度 +2%，克地载阵(2)
    {1u, kElementMetal, 2u, 5u, 0u, 2u, 0u, 0u, 0u, 0u, 0u, 0u},
    // 2 地载阵（土）：受击减伤 +6%、开局 4% 护盾，克风扬阵(3)
    {2u, kElementEarth, 3u, 0u, 6u, 0u, 0u, 0u, 0u, 0u, 0u, 4u},
    // 3 风扬阵（木）：速度 +6%、治疗与护盾 +4%，克云垂阵(4)
    {3u, kElementWood,  4u, 0u, 0u, 6u, 4u, 0u, 0u, 0u, 0u, 0u},
    // 4 云垂阵（水）：控制抗性 +8%、回合初回复 3% 法力，克虎翼阵(5)
    {4u, kElementWater, 5u, 0u, 0u, 0u, 0u, 8u, 3u, 0u, 0u, 0u},
    // 5 虎翼阵（火）：暴击率 +4%、出战灵宠伤害 +8%，克天覆阵(1)
    {5u, kElementFire,  1u, 0u, 0u, 0u, 0u, 0u, 0u, 4u, 8u, 0u},
}};

constexpr bool IsValidFormationId(uint32_t formationId) {
    return formationId <= kMaxFormationId;
}

constexpr WuxingFormationDef FormationOf(uint32_t formationId) {
    return (formationId >= 1u && formationId <= kMaxFormationId)
        ? kFormations[formationId - 1u]
        : WuxingFormationDef{};
}

constexpr uint32_t FormationCounterBonusPct(uint32_t attackerFormationId, uint32_t defenderFormationId) {
    if (attackerFormationId < 1u || attackerFormationId > kMaxFormationId ||
        defenderFormationId < 1u || defenderFormationId > kMaxFormationId) {
        return 0u;
    }
    return kFormations[attackerFormationId - 1u].countersId == defenderFormationId
        ? kFormationCounterBonusPct
        : 0u;
}

// ---- 五行幻境·15 枚局内五行符印（仅本轮爬塔生效，出塔清空） ----
struct WuxingTowerSigilDef {
    uint32_t id = 0u;
    uint32_t element = kElementNone;
    uint32_t rarity = 1u; // 1=凡品, 2=灵品, 3=仙品
    uint32_t damagePct = 0u;
    uint32_t critPct = 0u;
    uint32_t critDamagePct = 0u;
    uint32_t shieldBreakBonusPct = 0u;
    uint32_t healShieldPct = 0u;
    uint32_t petDamagePct = 0u;
    uint32_t roundHpRegenPct = 0u;
    uint32_t roundManaRegenPct = 0u;
    uint32_t controlResistPct = 0u;
    uint32_t onHitManaRegenPct = 0u;
    uint32_t damageTakenReducePct = 0u;
    uint32_t first3RoundsDamagePct = 0u;
    uint32_t singleTargetDamagePct = 0u;
    uint32_t critBurnMaxHpPct = 0u;
    uint32_t openingShieldPct = 0u;
    uint32_t reflectPct = 0u;
};

inline constexpr std::array<WuxingTowerSigilDef, 15> kTowerSigils = {{
    // 金系（101..103）
    {101u, kElementMetal, 1u,  8u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    {102u, kElementMetal, 2u,  0u, 12u,  0u, 25u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    {103u, kElementMetal, 3u, 14u,  0u, 20u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    // 木系（201..203）
    {201u, kElementWood,  1u,  0u,  0u,  0u,  0u, 12u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    {202u, kElementWood,  2u,  0u,  0u,  0u,  0u,  0u, 18u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    {203u, kElementWood,  3u,  0u,  0u,  0u,  0u,  0u, 22u, 5u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    // 水系（301..303）
    {301u, kElementWater, 1u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  8u,  0u, 0u,  0u,  0u,  0u, 0u,  0u,  0u},
    {302u, kElementWater, 2u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  0u, 15u, 4u,  0u,  0u,  0u, 0u,  0u,  0u},
    {303u, kElementWater, 3u,  0u,  0u,  0u,  0u,  0u,  0u, 0u, 10u, 20u, 0u,  8u,  0u,  0u, 0u,  0u,  0u},
    // 火系（401..403）
    {401u, kElementFire,  1u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u, 14u,  0u, 0u,  0u,  0u},
    {402u, kElementFire,  2u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u, 12u, 0u,  0u,  0u},
    {403u, kElementFire,  3u, 16u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 3u,  0u,  0u},
    // 土系（501..503）
    {501u, kElementEarth, 1u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u, 12u,  0u},
    {502u, kElementEarth, 2u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u,  0u,  0u,  0u, 0u,  0u, 12u},
    {503u, kElementEarth, 3u,  0u,  0u,  0u,  0u,  0u,  0u, 0u,  0u,  0u, 0u, 12u,  0u,  0u, 0u, 20u, 15u},
}};

constexpr WuxingTowerSigilDef FindTowerSigil(uint32_t sigilId) {
    for (const auto& def : kTowerSigils) {
        if (def.id == sigilId) return def;
    }
    return WuxingTowerSigilDef{};
}

constexpr bool IsValidTowerSigilId(uint32_t sigilId) {
    return FindTowerSigil(sigilId).id != 0u;
}

// 层数映射到既有稳定首领试炼普通副本（零改 Excel 表，复用已验证怪物与预告机制）。
constexpr uint32_t TowerFloorDungeonId(uint32_t floor) {
    switch (floor) {
    case 1u: return 101u;
    case 2u: return 103u;
    case 3u: return 105u;
    case 4u: return 107u;
    case 5u: return 103u;
    case 6u: return 101u;
    case 7u: return 105u;
    case 8u: return 107u;
    case 9u: return 103u;
    case 10u: return 107u;
    default: return 101u;
    }
}

constexpr uint32_t TowerFloorLeylineElement(uint32_t weekKey, uint32_t floor) {
    const uint32_t clamped = (floor >= 1u && floor <= kTowerMaxFloors) ? floor : 1u;
    return ((weekKey + clamped - 1u) % 5u) + 1u;
}

constexpr uint32_t TowerMonsterHealthPct(uint32_t floor) {
    const uint32_t f = (floor >= 1u && floor <= kTowerMaxFloors) ? floor : 1u;
    uint32_t pct = 85u + (f - 1u) * 8u;
    if (f == 5u) pct += 10u;
    if (f == 10u) pct += 20u;
    return pct;
}

constexpr uint32_t TowerMonsterAttackPct(uint32_t floor) {
    const uint32_t f = (floor >= 1u && floor <= kTowerMaxFloors) ? floor : 1u;
    uint32_t pct = 85u + (f - 1u) * 5u;
    if (f == 5u) pct += 6u;
    if (f == 10u) pct += 12u;
    return pct;
}

inline uint64_t MixSeed(uint64_t x) {
    x += 0x9E3779B97F4A7C15ULL;
    x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9ULL;
    x = (x ^ (x >> 27)) * 0x94D049BB133111EBULL;
    return x ^ (x >> 31);
}

// 确定性抽取 3 枚不重复、未持有的局内五行符印；层数越高越容易出现灵品/仙品。
inline std::vector<uint32_t> RollTowerSigilOffer(uint32_t weekKey, uint64_t runId, uint32_t clearedFloor,
                                                 const std::vector<uint32_t>& ownedSigils) {
    std::vector<uint32_t> pool;
    pool.reserve(kTowerSigils.size());
    for (const auto& def : kTowerSigils) {
        if (std::find(ownedSigils.begin(), ownedSigils.end(), def.id) != ownedSigils.end()) {
            continue;
        }
        // 前 3 层只抽凡/灵品保持构筑曲线平滑；第 4 层起开放仙品。
        if (clearedFloor <= 3u && def.rarity >= 3u) {
            continue;
        }
        pool.push_back(def.id);
    }
    if (pool.size() < kTowerOfferCount) {
        pool.clear();
        for (const auto& def : kTowerSigils) {
            if (std::find(ownedSigils.begin(), ownedSigils.end(), def.id) == ownedSigils.end()) {
                pool.push_back(def.id);
            }
        }
    }
    std::vector<uint32_t> offers;
    uint64_t state = MixSeed((static_cast<uint64_t>(weekKey) << 32) ^ MixSeed(runId) ^ (static_cast<uint64_t>(clearedFloor) * 0x517CC1B727220A95ULL));
    while (!pool.empty() && offers.size() < kTowerOfferCount) {
        state = MixSeed(state);
        const std::size_t idx = static_cast<std::size_t>(state % pool.size());
        offers.push_back(pool[idx]);
        pool.erase(pool.begin() + static_cast<std::ptrdiff_t>(idx));
    }
    return offers;
}

// 自定义切磋回合上限白名单：0=默认；允许 8/12/20 回合，其余拒绝防畸形房间。
constexpr bool IsValidCustomDuelRoundLimit(uint32_t roundLimitOverride) {
    return roundLimitOverride == 0u || roundLimitOverride == 8u ||
           roundLimitOverride == 12u || roundLimitOverride == 20u;
}

// ---- 五行幻境 V2：15 枚机制改造型五行奇策与 6 关分岔探索纯规则 ----
struct WuxingRealmBoonDef {
    uint32_t id = 0u;
    uint32_t element = kElementNone;
    uint32_t rarity = 1u; // 1=凡, 2=灵, 3=仙
    uint32_t bitIndex = 0u;
};

inline constexpr std::array<WuxingRealmBoonDef, 15> kRealmBoons = {{
    {kBoonMetalBreakRefund,   kElementMetal, 1u,  0u},
    {kBoonMetalCritSplash,    kElementMetal, 2u,  1u},
    {kBoonMetalFirstPierce,   kElementMetal, 3u,  2u},
    {kBoonWoodDotSpread,      kElementWood,  1u,  3u},
    {kBoonWoodHealEmpower,    kElementWood,  2u,  4u},
    {kBoonWoodPetFollowUp,    kElementWood,  3u,  5u},
    {kBoonWaterFreeFirstCast, kElementWater, 1u,  6u},
    {kBoonWaterExtendControl, kElementWater, 2u,  7u},
    {kBoonWaterSkillEcho,     kElementWater, 3u,  8u},
    {kBoonFireShieldBlast,    kElementFire,  1u,  9u},
    {kBoonFireLowHpFury,      kElementFire,  2u, 10u},
    {kBoonFireRoundEndBurn,   kElementFire,  3u, 11u},
    {kBoonEarthDoubleReflect, kElementEarth, 1u, 12u},
    {kBoonEarthClutchShield,  kElementEarth, 2u, 13u},
    {kBoonEarthShieldStrike,  kElementEarth, 3u, 14u},
}};

constexpr WuxingRealmBoonDef FindRealmBoon(uint32_t boonId) {
    for (const auto& def : kRealmBoons) {
        if (def.id == boonId) return def;
    }
    return WuxingRealmBoonDef{};
}

constexpr bool IsValidRealmBoonId(uint32_t boonId) {
    return FindRealmBoon(boonId).id != 0u;
}

constexpr bool IsValidTowerOrBoonId(uint32_t id) {
    return IsValidTowerSigilId(id) || IsValidRealmBoonId(id);
}

constexpr uint32_t RealmBoonBit(uint32_t boonId) {
    const auto def = FindRealmBoon(boonId);
    return def.id != 0u ? (1u << def.bitIndex) : 0u;
}

template <typename Range>
inline uint32_t ComputeRealmBoonMask(const Range& ids) {
    uint32_t mask = 0u;
    for (const uint32_t id : ids) {
        mask |= RealmBoonBit(id);
    }
    return mask;
}

// 统计同系奇策数量：任意五行集齐 >= 3 枚即点亮对应五行奇策共鸣（按 1..5 稳定取最小元素）。
template <typename Range>
inline uint32_t ComputeActiveResonanceElement(const Range& ids) {
    std::array<uint32_t, 6> counts{};
    for (const uint32_t id : ids) {
        const auto def = FindRealmBoon(id);
        if (def.id != 0u && def.element >= 1u && def.element <= 5u) {
            counts[def.element] += 1u;
        }
    }
    for (uint32_t elem = 1u; elem <= 5u; ++elem) {
        if (counts[elem] >= kRealmV2ResonanceThreshold) {
            return elem;
        }
    }
    return kElementNone;
}

// 返回当前持有奇策最多的五行（供第 4 关秘境休整「同源寻策」凑齐 3 同系共鸣使用）。
template <typename Range>
inline uint32_t DominantBoonElement(const Range& ids) {
    std::array<uint32_t, 6> counts{};
    for (const uint32_t id : ids) {
        const auto def = FindRealmBoon(id);
        if (def.id != 0u && def.element >= 1u && def.element <= 5u) {
            counts[def.element] += 1u;
        }
    }
    uint32_t bestElem = kElementNone;
    uint32_t bestCount = 0u;
    for (uint32_t elem = 1u; elem <= 5u; ++elem) {
        if (counts[elem] > bestCount && counts[elem] < kRealmV2ResonanceThreshold) {
            bestCount = counts[elem];
            bestElem = elem;
        }
    }
    if (bestElem == kElementNone) {
        for (uint32_t elem = 1u; elem <= 5u; ++elem) {
            if (counts[elem] > bestCount) {
                bestCount = counts[elem];
                bestElem = elem;
            }
        }
    }
    return bestElem;
}

// 淬炼升品：将一枚已持有的凡/灵品奇策升级为同五行更高一档未持有奇策（失败返回 0）。
inline uint32_t NextUpgradeBoonId(uint32_t currentBoonId, const std::vector<uint32_t>& owned) {
    const auto cur = FindRealmBoon(currentBoonId);
    if (cur.id == 0u || cur.rarity >= 3u) {
        return 0u;
    }
    for (uint32_t targetRarity = cur.rarity + 1u; targetRarity <= 3u; ++targetRarity) {
        for (const auto& def : kRealmBoons) {
            if (def.element == cur.element && def.rarity == targetRarity &&
                std::find(owned.begin(), owned.end(), def.id) == owned.end()) {
                return def.id;
            }
        }
    }
    return 0u;
}

constexpr uint32_t RealmNodeDefaultKind(uint32_t nodeIndex) {
    switch (nodeIndex) {
    case 1u: return kRealmNodeNormal;
    case 2u: return kRealmNodeNeedBranch;
    case 3u: return kRealmNodeNormal;
    case 4u: return kRealmNodeNeedBranch;
    case 5u: return kRealmNodeElite;
    case 6u: return kRealmNodeBoss;
    default: return kRealmNodeNormal;
    }
}

constexpr bool IsRealmCombatNodeKind(uint32_t nodeKind) {
    return nodeKind == kRealmNodeNormal || nodeKind == kRealmNodeElite || nodeKind == kRealmNodeBoss;
}

constexpr uint32_t RealmNodeDungeonId(uint32_t nodeIndex, uint32_t nodeKind) {
    if (nodeKind == kRealmNodeBoss || nodeIndex >= kRealmV2MaxNodes) {
        return 107u;
    }
    if (nodeKind == kRealmNodeElite) {
        return (nodeIndex <= 2u) ? 103u : 105u;
    }
    switch (nodeIndex) {
    case 1u: return 101u;
    case 3u: return 105u;
    default: return 103u;
    }
}

constexpr uint32_t RealmMonsterHealthPct(uint32_t nodeIndex, uint32_t nodeKind) {
    const uint32_t n = (nodeIndex >= 1u && nodeIndex <= kRealmV2MaxNodes) ? nodeIndex : 1u;
    uint32_t pct = 82u + (n - 1u) * 10u;
    if (nodeKind == kRealmNodeElite) pct += 16u;
    if (nodeKind == kRealmNodeBoss || n == kRealmV2MaxNodes) pct += 16u;
    return pct;
}

constexpr uint32_t RealmMonsterAttackPct(uint32_t nodeIndex, uint32_t nodeKind) {
    const uint32_t n = (nodeIndex >= 1u && nodeIndex <= kRealmV2MaxNodes) ? nodeIndex : 1u;
    uint32_t pct = 82u + (n - 1u) * 7u;
    if (nodeKind == kRealmNodeElite) pct += 10u;
    if (nodeKind == kRealmNodeBoss || n == kRealmV2MaxNodes) pct += 8u;
    return pct;
}

// 确定性抽取 3 枚未持有的 V2 五行奇策：
// - 精英战保底至少 1 枚灵/仙品；
// - preferredElement 非 0 时（休整「同源寻策」），保底优先塞入至多 2 枚该五行未持有奇策助玩家凑 3 同系共鸣。
inline std::vector<uint32_t> RollRealmBoonOffer(uint32_t weekKey, uint64_t runId, uint32_t nodeIndex,
                                                uint32_t nodeKind, uint32_t rerollCount,
                                                const std::vector<uint32_t>& owned,
                                                uint32_t preferredElement = kElementNone) {
    auto isOwned = [&](uint32_t id) {
        return std::find(owned.begin(), owned.end(), id) != owned.end();
    };
    std::vector<uint32_t> offers;
    std::vector<uint32_t> pool;
    uint64_t state = MixSeed(
        (static_cast<uint64_t>(weekKey) << 32) ^ MixSeed(runId) ^
        (static_cast<uint64_t>(nodeIndex) * 0x517CC1B727220A95ULL) ^
        (static_cast<uint64_t>(nodeKind) * 0x94D049BB133111EBULL) ^
        (static_cast<uint64_t>(rerollCount + 1u) * 0xBF58476D1CE4E5B9ULL));

    auto pickFrom = [&](std::vector<uint32_t>& candidates, std::size_t maxAdd) {
        std::size_t added = 0u;
        while (!candidates.empty() && offers.size() < kTowerOfferCount && added < maxAdd) {
            state = MixSeed(state);
            const std::size_t idx = static_cast<std::size_t>(state % candidates.size());
            const uint32_t chosen = candidates[idx];
            candidates.erase(candidates.begin() + static_cast<std::ptrdiff_t>(idx));
            if (std::find(offers.begin(), offers.end(), chosen) == offers.end()) {
                offers.push_back(chosen);
                ++added;
            }
        }
    };

    if (preferredElement >= 1u && preferredElement <= 5u) {
        std::vector<uint32_t> elemPool;
        for (const auto& def : kRealmBoons) {
            if (def.element == preferredElement && !isOwned(def.id)) {
                elemPool.push_back(def.id);
            }
        }
        pickFrom(elemPool, 2u);
    }
    if (nodeKind == kRealmNodeElite && offers.size() < kTowerOfferCount) {
        std::vector<uint32_t> highPool;
        for (const auto& def : kRealmBoons) {
            if (def.rarity >= 2u && !isOwned(def.id)) {
                highPool.push_back(def.id);
            }
        }
        pickFrom(highPool, 2u);
    }
    for (const auto& def : kRealmBoons) {
        if (isOwned(def.id)) {
            continue;
        }
        if (nodeIndex <= 1u && nodeKind == kRealmNodeNormal && def.rarity >= 3u) {
            continue;
        }
        pool.push_back(def.id);
    }
    pickFrom(pool, kTowerOfferCount);
    if (offers.size() < kTowerOfferCount) {
        std::vector<uint32_t> fallback;
        for (const auto& def : kRealmBoons) {
            if (!isOwned(def.id)) {
                fallback.push_back(def.id);
            }
        }
        pickFrom(fallback, kTowerOfferCount);
    }
    return offers;
}

// 第 2 关奇遇「道缘寻策」：确定性发放 1 枚未持有灵品奇策（无未持有灵品时回退任意未持有奇策）。
inline uint32_t RollRealmEncounterBoon(uint32_t weekKey, uint64_t runId, const std::vector<uint32_t>& owned) {
    auto isOwned = [&](uint32_t id) {
        return std::find(owned.begin(), owned.end(), id) != owned.end();
    };
    std::vector<uint32_t> pool;
    for (const auto& def : kRealmBoons) {
        if (def.rarity == 2u && !isOwned(def.id)) {
            pool.push_back(def.id);
        }
    }
    if (pool.empty()) {
        for (const auto& def : kRealmBoons) {
            if (!isOwned(def.id)) {
                pool.push_back(def.id);
            }
        }
    }
    if (pool.empty()) {
        return 0u;
    }
    const uint64_t h = MixSeed((static_cast<uint64_t>(weekKey) << 32) ^ MixSeed(runId) ^ 0xA0761D6478BD642FULL);
    return pool[static_cast<std::size_t>(h % pool.size())];
}

} // namespace wuxing
} // namespace turnbattle

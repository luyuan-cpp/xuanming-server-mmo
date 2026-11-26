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
inline constexpr uint32_t kTowerOfferCount = 3u;
inline constexpr uint32_t kMaxSavedBuildBookmarks = 12u;
inline constexpr uint32_t kSynergyIntervalRounds = 3u;
inline constexpr uint32_t kSynergyBonusPct = 10u;
inline constexpr uint32_t kFormationCounterBonusPct = 5u;
inline constexpr uint64_t kFireCritBurnCapPerHit = 1500ULL;

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

} // namespace wuxing
} // namespace turnbattle

#pragma once

// 宝宝(宠物)纯规则(零 ECS / 零表 / 零 proto 依赖,可单测),设计文档
// docs/design/player-pet.md §3。
//
// 点数总量与"目标已分配"校验完全复用 attributerules(角色那套),这里只回答宝宝独有的三个问题:
//   1) 一只宝宝当前是几级(跟随主人,受种类上限夹);
//   2) 一个维度的面板值怎么算(自然成长 × 等级 + 已分配);
//   3) 六项二级属性怎么由维度值 × 系数 × 资质 汇总出来。
//
// 资质(aptitude)是万分比:10000 = 100%,只放大该维度对二级属性的贡献,不改面板上的维度值
// —— 玩家看到的"力量 50"是实打实的 50 点,资质高低体现在这 50 点换来多少物伤。

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <vector>

namespace petrules {

// 资质基准:万分比,10000 = 100%(缺配/老存档按基准处理,等价于没有资质概念)
inline constexpr uint32_t kAptitudeBase = 10000;

// 一个维度的系数矩阵(AttributeDimension 的六列 + 每级自然成长)
struct DimensionCoefficients {
    uint32_t basePerLevel = 0;
    double maxHealth = 0.0;
    double maxMana = 0.0;
    double physicalAttack = 0.0;
    double magicAttack = 0.0;
    double speed = 0.0;
    double defense = 0.0;
};

// 一个维度在这只宝宝身上的完整输入
struct DimensionInput {
    uint32_t dimensionId = 0;
    DimensionCoefficients coefficients;
    uint32_t allocated = 0;                 // 已分配点
    uint32_t aptitude = kAptitudeBase;      // 资质(万分比)
};

// 二级属性(与 DerivedAttributesComp 同六项)
struct DerivedAttributes {
    uint64_t maxHealth = 0;
    uint64_t maxMana = 0;
    uint64_t physicalAttack = 0;
    uint64_t magicAttack = 0;
    uint64_t speed = 0;
    uint64_t defense = 0;
};

// PetTable 的三项初值(等价角色的 ClassTable 初值)
struct BaseValues {
    uint64_t maxHealth = 0;
    uint64_t maxMana = 0;
    uint64_t speed = 0;
};

// 宝宝等级 = min(主人等级, 种类上限);上限缺配(0)时不夹。等级至少 1。
inline uint32_t EffectiveLevel(uint32_t ownerLevel, uint32_t levelCap) {
    const uint32_t level = ownerLevel > 0 ? ownerLevel : 1;
    if (levelCap == 0) {
        return level;
    }
    return std::min(level, levelCap);
}

// 面板维度值 = 每级自然成长 × 等级 + 已分配点(资质不参与,见文件头)
inline uint64_t DimensionValue(const DimensionInput& input, uint32_t level) {
    return static_cast<uint64_t>(input.coefficients.basePerLevel) * level + input.allocated;
}

// 二级属性 = 初值 + Σ(维度值 × 系数 × 资质 / 10000);向下取整,气血上限至少 1
inline DerivedAttributes ComputeDerived(const BaseValues& base,
                                        const std::vector<DimensionInput>& dimensions,
                                        uint32_t level) {
    double maxHealth = static_cast<double>(base.maxHealth);
    double maxMana = static_cast<double>(base.maxMana);
    double physicalAttack = 0.0;
    double magicAttack = 0.0;
    double speed = static_cast<double>(base.speed);
    double defense = 0.0;

    for (const auto& input : dimensions) {
        const auto value = static_cast<double>(DimensionValue(input, level));
        if (value == 0.0) {
            continue;
        }
        const double aptitude =
            static_cast<double>(input.aptitude > 0 ? input.aptitude : kAptitudeBase) /
            static_cast<double>(kAptitudeBase);
        const double weighted = value * aptitude;
        maxHealth += input.coefficients.maxHealth * weighted;
        maxMana += input.coefficients.maxMana * weighted;
        physicalAttack += input.coefficients.physicalAttack * weighted;
        magicAttack += input.coefficients.magicAttack * weighted;
        speed += input.coefficients.speed * weighted;
        defense += input.coefficients.defense * weighted;
    }

    auto toU64 = [](double v) -> uint64_t {
        if (v <= 0.0) {
            return 0;
        }
        return static_cast<uint64_t>(std::floor(v));
    };

    DerivedAttributes derived;
    derived.maxHealth = std::max<uint64_t>(toU64(maxHealth), 1);
    derived.maxMana = toU64(maxMana);
    derived.physicalAttack = toU64(physicalAttack);
    derived.magicAttack = toU64(magicAttack);
    derived.speed = toU64(speed);
    derived.defense = toU64(defense);
    return derived;
}

// 展示用"成长率" = 各维度资质的算术平均(万分比)。派生量,不落库:
// 存第二份就会和 aptitude 分叉,而它只是给玩家一眼看的概括数。
inline uint32_t GrowthPermyriad(const std::vector<DimensionInput>& dimensions) {
    if (dimensions.empty()) {
        return kAptitudeBase;
    }
    uint64_t sum = 0;
    for (const auto& input : dimensions) {
        sum += input.aptitude > 0 ? input.aptitude : kAptitudeBase;
    }
    return static_cast<uint32_t>(sum / dimensions.size());
}

}  // namespace petrules

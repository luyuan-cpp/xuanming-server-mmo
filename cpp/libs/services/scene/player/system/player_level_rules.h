#pragma once

// 角色等级的合法范围与生效口径(纯规则,零 ECS / 零表依赖,可单测),设计文档
// docs/design/player-attribute-allocation.md §2.2、docs/design/mixed-version-persist-field-protection.md §12.1。
//
// 上限是策划定的硬约束(2026-09-10 由 200 改为 85)。存储等级与生效等级分开:
//   - 存储等级 = LevelComp.level() 的原值。加载原样携带、存盘原样写出,本版本不按自己的上限改写它
//     (新版本提高上限后回滚 scene、或混版本跨 zone 迁移,压回的等级随下一次存盘落库,升回新版本也恢复不了);
//   - 生效等级 = CappedLevel(存储等级)。所有玩法(总点数、面板、技能解锁与生效重数、战斗快照、伤害与回血公式、
//     任务等级条件、宝宝跟随等级、装备穿戴等级……)一律读它。
// LevelComp.level() 的直读只允许出现在四处:加载器(超上限只告警)、存盘(原样写出)、GmSetLevel(写前取旧值)、
// 管理端 dump(原样展示)。其余读点一律经 CappedLevel。
//
// 写入口只有:新号初始化写 1(InitPlayerFromAllData)、GmSetPlayerLevel(IsValidLevel 拒绝越界)与将来的经验系统。
// 经验系统的升级判断必须用存储等级(存储等级 ≥ kMaxLevel 就不再升级),绝不能写回 CappedLevel + 1 ——
// 那会把携带的超上限等级降下来,正是这里要防的删数据。
// 下调上限不再自动收敛:超上限的存档按上限生效、存储值原样留着;真要把存量压回新上限,另做显式迁移。
// 按等级的门槛与总量必须随等级单调不减:超上限玩家按上限生效,单调时恰好等于上限玩家的结果,不会因多出来的等级
// 变强或变弱;新增非单调的按等级规则(只在某个等级以下生效的福利之类),要单独评估超上限玩家拿到的是哪一档。
//
// 单测:cpp/tests/turn_battle_engine_test/attribute_allocation_rules_test.cpp(PlayerLevelRulesTest);
// 超上限存储等级的加载 / 存盘往返:cpp/tests/bag_test/persist_round_trip_test.cpp。

#include <cstdint>

namespace playerlevel {

inline constexpr uint32_t kMaxLevel = 85;

// 可写入的等级:1..kMaxLevel(0 不是合法等级)
inline bool IsValidLevel(uint32_t level) {
    return level >= 1 && level <= kMaxLevel;
}

// 存储等级 → 玩法生效等级:0 按 1(新号加载期间 InitPlayerFromAllData 写 1 之前就是 0),超过上限按上限,其余原样。
// 只做换算,不写回 LevelComp(见文件头)。
constexpr uint32_t CappedLevel(uint32_t storedLevel) noexcept {
    if (storedLevel == 0) {
        return 1;
    }
    return storedLevel > kMaxLevel ? kMaxLevel : storedLevel;
}

static_assert(CappedLevel(0) == 1 && CappedLevel(1) == 1, "0 级按 1 级生效");
static_assert(CappedLevel(kMaxLevel) == kMaxLevel && CappedLevel(kMaxLevel + 1) == kMaxLevel,
              "超上限的存储等级按上限生效");

}  // namespace playerlevel

#pragma once

#include <cstddef>
#include <cstdint>
#include <functional>
#include <unordered_map>

#include "bag_system.h"

// ─────────────────────────────────────────────────────────────────────────
// BagProfileRegistry —— 动态包(节日 / 活动 / 宠物包)的 profile 注册表。
//
// 四个固定包由 `BagType` 指认,构造函数直接装 profile,不经这里。动态包不一样:
// 它们运行时来去、由玩法创建,而**跨服 / 存档还原时必须能重新装回同一套规则**。
//
// 这就是注册表存在的全部理由 —— 它把
//     "这个包是哪套规则"(一个 uint32,**属于玩家数据,必须持久化**)
//   与
//     "那套规则具体是什么"(布局 / 准入 / 淘汰三个对象,**属于代码+配置,绝不进快照**)
// 分开。设计文档 §7 红线 5:策略对象不许序列化进快照,快照只存 profile_id。
//
// 不这么做的后果是具体的:`BagAllData.DynamicBagData` 早先没有 profile_id,
// 于是节日包跨服回来会退化成默认的自由格 + 什么都收 —— 规则静默消失,节日包
// 变成普通包,玩家能往里塞任何东西。
//
// ── 为什么是"注册"而不是"查表" ──────────────────────────────────────────
// 设计文档原本规划的是一张 `CfgBagProfile` 表(profile_id -> 布局/容量/接受哪些
// tag/淘汰策略)。那仍然是终局,但今天落不了地:配置表的源是二进制 xlsx,加表
// 加列要跑导表工具。而注册式今天就完整可用 —— 活动系统本来就知道自己发哪些
// 道具、包多大,启动时注册一条即可。
//
// 等 `CfgBagProfile` 真的落地,改动只在**这个文件内部**:`Make()` 改成先查表、
// 查不到再回退到已注册的工厂。注册表的调用方(bag_marshal / 玩法)一行都不用动。
// ─────────────────────────────────────────────────────────────────────────

// 给定容量,造出一整套规则。容量由调用方给(还原时来自快照,创建时来自玩法),
// 因为容量是**状态**不是规则 —— 它会被 ExpandCapacity / SetCapacityForRestore 改。
using BagProfileFactory = std::function<BagProfile(std::size_t capacity)>;

class BagProfileRegistry
{
public:
    static BagProfileRegistry &Instance();

    // 注册一套规则。重复注册同一个 id 会覆盖并打 WARN —— 覆盖本身是合法的
    // (热更 / 活动改版),但静默覆盖会让"为什么这个包的规则变了"无从查起。
    // id == kBagProfileUnspecified(0)被拒绝:0 是"未指定"的哨兵。
    void Register(uint32_t profileId, BagProfileFactory factory);

    [[nodiscard]] bool Contains(uint32_t profileId) const;

    // 按 id 造一套规则。
    //
    // **查不到时返回默认自由格 profile,并打 ERROR,绝不返回空。**
    // 这是还原路径上的 fail-open,与 `InsertItemForRestore` 同一条口径:
    // 活动下线了、profile 还没注册、脏数据 —— 无论哪种,都不能因此让玩家的
    // 东西没地方放。**位置可以变,规则可以退化,物品不能丢。**
    // 退化出来的 profile 的 id 仍然保留传入值,于是下次存盘不会把 id 抹掉:
    // 活动重新上线后,包会自己变回去。
    [[nodiscard]] BagProfile Make(uint32_t profileId, std::size_t capacity) const;

    // 只给用例用:清空注册表,免得用例之间互相污染。
    void ClearForTest();

private:
    BagProfileRegistry() = default;

    std::unordered_map<uint32_t, BagProfileFactory> factories_;
};

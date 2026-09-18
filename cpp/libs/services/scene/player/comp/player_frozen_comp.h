#pragma once

#include <cstdint>

// PlayerFrozenComp —— 归属交接在途的"冻结输入"标记。
//
// 与 PlayerTravelHandoffComp(player_ownership_comp.h)成对挂、成对摘,只由
// PlayerLifecycleSystem::StartTravelHandoff 挂上。覆盖两种交接:跨 zone 传送,以及同 zone
// 跨节点换图(被 scene_manager 以 18 暂拒之后的"先存盘、出示标记、再请求")。
//
// 为什么要冻结:交接的前提是"盘上就是最新状态"。从挂上它到交接出结果(放行 → 销毁本地实体;
// 未成 → 解冻)之间,本节点手里这份内存态不得再变,否则目标节点从盘上读到的就不是玩家最后的样子。
//
// 挂着它期间的约束(业务系统各自按 any_of<PlayerFrozenComp> / IsCrossZoneFrozen 判):
//   * AOI / 战斗 / 移动 tick / 技能 / buff / 挂机 等业务系统必须跳过该实体;
//   * 货币 / 背包 / 任务 等写路径必须拒绝 —— 对写而言这个实体等同于"已经不在本节点";
//   * gate 会话保留:交接未成时原地解冻即可继续玩,不需要客户端重连。
//
// 摘掉的时机(全部在 player_lifecycle.cpp):
//   * 放行        DestroyDeposedPlayer(随实体一起销毁,不存盘);
//   * 未成        AbortTravelHandoff(解冻,按情形回 tip);
//   * 玩家途中退出 HandlePlayerAsyncSaved / FinishExitAfterPersist 的"退出优先"分支。
// 应答丢失由 ArmTravelReplyWatchdog + ResolveTravelOutcome 兜底,不会永久冻结。
//
// 历史:它原本是 player_migrate 搬数据链(Kafka 搬 PlayerAllData + ACK + CrossZoneReaper 重发)的
// 在途标记。那条链按 cross-zone-scene-travel.md CZ-1 已下线(全仓从无触发点,且与"数据不搬家"
// 的决策冲突),只保留了这个组件和业务侧的拦写闸。
//
// 纯运行时标记:不入 PlayerAllData、不序列化、不跨进程,所以是普通 C++ struct 而不是 proto。
struct PlayerFrozenComp
{
    // 冻结开始的墙钟毫秒。两个用途:
    //   * 排障:看"冻结了多久";
    //   * 存盘阶段看门狗(PlayerLifecycleSystem::ArmTravelSaveWatchdog)的代际 —— 那一段
    //     PlayerTravelHandoffComp.requestedAtMs 还是 0,分不出是哪一次交接,只能靠它认。
    // EnterScene 发出之后的应答超时判定用的是 requestedAtMs,不是它。
    // 挂上之后不得改写:改了等于换代际,在途的存盘阶段看门狗会认不出这次交接、到期不解冻。
    int64_t frozenAtMs{0};

    // 交接的目标 zone(同 zone 跨节点换图时等于本 zone)。仅供日志 / 指标。
    uint32_t toZoneId{0};

    // 老搬数据链的重发计数,业务代码已无人读写。字段保留只因为 cpp/tests/cross_zone_test 还在
    // 直接给它赋值,而本轮不能编译验证;下次能编译时连同那处测试一起删。
    uint32_t migrateAttempts{1};
};

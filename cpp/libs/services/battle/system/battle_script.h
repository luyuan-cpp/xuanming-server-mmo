#pragma once
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>

#include "data/battle_data_provider.h"
#include "proto/battle/battle_script.pb.h"

// 战斗剧本的开局复核与规范化(docs/design/battle-script.md §4)。
//
// 剧本由 match 解析、按值冻结进 CreateBattleRequest.team_scripts;match 侧(Go,go/shared/battlescript)已按同一规则校验过。
// 这里是 battle 自己的第二道闸:按本节点的配表再校验一遍(两边配表版本不一致、或编排层出错时兜住),
// 通过才交给引擎。规则与 Go 版逐条对应,改一处必须同改另一处(对拍用例见设计 §8)。
//
// 纯逻辑:只读数据源的表行,不打日志、不碰引擎状态。失败时 error 填英文原因(服务日志用英文,AGENTS §2),由节点记日志。

namespace turnbattle {

// ---- 上限:每回合求值量 ≤ 角色位 × 单位 × 规则 × 候选 × 筛选,都是整数比较,不分配大块内存(设计 §6) ----
inline constexpr int kMaxScriptRoles = 8;
inline constexpr int kMaxScriptRulesPerRole = 16;
inline constexpr int kMaxScriptConditionsPerRule = 2;
inline constexpr int kMaxScriptFiltersPerRule = 3;
inline constexpr uint32_t kMaxScriptRoleMembers = 10;  // 一队最多 5 人 + 5 宝宝,或 10 只怪
inline constexpr uint32_t kMaxScriptRound = 1000;      // 回合类参数上限;副本 time_limit 换算的回合上限可以超过 300
inline constexpr uint32_t kMaxScriptUnitCount = 20;    // 「存活敌方 >= N」的 N 上限:一局最多约 20 个单位
inline constexpr uint32_t kMaxScriptClassId = 1000;    // 职业号只做区间校验:数据源不提供 Class 表
inline constexpr uint32_t kPermyriadFull = 10000;
inline constexpr std::size_t kMaxScriptNameBytes = 64;
inline constexpr std::size_t kMaxScriptDescriptionBytes = 512;
inline constexpr std::size_t kMaxScriptNoteBytes = 256;
inline constexpr uint32_t kBattleScriptSchemaVersion = 1;

// value / max < permyriad / 10000,纯整数判定(不经浮点,C++ / Java 两版逐位一致)。
// max == 0 视为满(没有上限的单位不算残血);乘法饱和,不会回绕
bool RatioBelowPermyriad(uint64_t value, uint64_t max, uint32_t permyriad);

// lhsValue / lhsMax < rhsValue / rhsMax,交叉相乘的整数判定(饱和);max == 0 视为满
bool RatioLess(uint64_t lhsValue, uint64_t lhsMax, uint64_t rhsValue, uint64_t rhsMax);

// HAS_BUFF_TYPE 只认引擎回合内实际生效的持续状态类型(turn_battle_constants.h);驱散是瞬时的,不收
bool IsScriptStatusBuffType(uint32_t buffType);

// 校验一份剧本里与「绑到哪一方」无关的部分(结构、上限、引用、条件 / 筛选 / 行动 × 目标组合),
// 成功返回角色位按 order、规则按 priority 排好的副本;失败返回空并填 error
std::optional<BattleScript> NormalizeBattleScript(const BattleScript& script, const BattleDataProvider& data,
                                                  std::string& error);

// 这份剧本能不能用在这一方:怪物方只许认领怪物、只放零耗蓝的非职业技能(怪物没有职业,Monster 表没有法力列);
// 玩家方不许认领怪物。剧本须已经过 NormalizeBattleScript
bool CheckBattleScriptSide(const BattleScript& script, eBattleScriptSide side, const BattleDataProvider& data,
                           std::string& error);

}  // namespace turnbattle

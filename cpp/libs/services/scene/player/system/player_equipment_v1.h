#pragma once

#include <cstdint>
#include <entt/src/entt/entity/entity.hpp>

class ItemComp;
class BagItemInfo;
class CraftMasteryInfo;
class EquipmentEffects;
class GetEquipmentCraftRulesResponse;
class PreviewEquipmentCraftRequest;
class PreviewEquipmentCraftResponse;
class CommitEquipmentCraftRequest;
class CommitEquipmentCraftResponse;

// V1 固定词条、品相打磨、无损重铸、法宝与定向打造的权威入口。只在scene逻辑线程调用。
// 读路径不创建组件；写路径同步完成候选、严格扣料、实例修改和防重水位。
// 成功回执的durable只来自确认提交的玩家快照，重放不重新选号或更新预览。
class PlayerEquipmentV1System {
public:
    static void FillDisplay(const ItemComp& item, BagItemInfo& info);
    static void CollectEffects(entt::entity player, EquipmentEffects& effects);
    static uint32_t ValidateQualityTable();
    static void FillForgeMastery(entt::entity player, CraftMasteryInfo& out);
    static uint32_t GetRules(entt::entity player, GetEquipmentCraftRulesResponse& response);
    static uint32_t Preview(entt::entity player, const PreviewEquipmentCraftRequest& request,
                            PreviewEquipmentCraftResponse& response);
    static void Commit(entt::entity player, const CommitEquipmentCraftRequest& request,
                       CommitEquipmentCraftResponse& response);

    using PersistFn = bool (*)(entt::entity);
    using QualityRollFn = uint32_t (*)(uint32_t bound);
    // 单测以同一存盘接缝安装确定性替身，nullptr恢复正式存盘。
    static void SetPersistForTest(PersistFn fn);
    // 单测以同一随机接缝安装确定性掷骰（返回 [0, bound)），nullptr恢复正式随机源。
    static void SetQualityRollForTest(QualityRollFn fn);
    // 单测原地 const_cast 修改内存表行时显式失效规则缓存；正式热更由快照版本自动失效。
    static void InvalidateRulesCacheForTest();
};

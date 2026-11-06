#pragma once

#include <cstdint>
#include <entt/src/entt/entity/entity.hpp>

class BagInfo;
class GetMissionListResponse;
class GetActivityListResponse;

// 这些入口只在 scene EventLoop 上调用，读取已有组件，不在开窗时创建玩家状态。
// 返回既有 tip 表的错误码；成功才发布完整快照，客户端整体覆盖。
class PlayerBagSystem {
public:
    static uint32_t BuildSnapshot(entt::entity player, uint32_t bagType, BagInfo& out);
    static uint32_t Sort(entt::entity player, uint32_t bagType, BagInfo& out, bool& changed);

    // 从临时格取回到人物背包(SceneBagClientPlayer.RetrieveTemporaryItems;docs/design/bag-rule-policy-layering.md §6.6)。
    // itemId != 0 取回这一件;takeAll 取回临时格里的全部(按入包先后)。两者恰好给一个。
    // 全或无:人物背包放不下这一批就整批拒绝,两个包都不动;成功后由 handler 回两个包的全量。不改装备栏、不重算属性。
    // 真搬了东西之后调 PlayerInboxSystem::OnBagSpaceFreed:因两包都放不下而延后的结算下一个 Tick 就重试,不用等退避。
    // 校验顺序(全部在任何修改之前)与返回的 tip:
    //   实体无效                                       kThisEntityIsInvalid
    //   itemId 与 takeAll 都给或都不给                 kInvalidParameter(纯参数错误先于状态判定)
    //   没有 PlayerBagsComp                            kServiceUnavailable
    //   人物背包或临时格两层不一致                     kInvalidTableData
    //   跨节点交接冻结中(PlayerFrozenComp)              kBagTemporaryRetrieveBusy
    //   (战斗中放行:整笔延后的那一局要靠取回腾位才补得上,InBattleComp 要等结算应用之后才摘;理由见实现)
    //   itemId 不在临时格里(含「已经取回」的重发)      kBagTemporaryItemNotFound
    //   takeAll 且临时格是空的                         kSuccess(什么都不动)
    //   人物背包放不下                                 kBagTemporaryRetrieveBagFull
    //   其余                                           原样透传 BagService::RetrieveFromTemporary(表数据异常等)
    static uint32_t RetrieveTemporary(entt::entity player, uint64_t itemId, bool takeAll);
};

class PlayerMissionReadSystem {
public:
    // 读取目录和运行态；操作能力复用正式写入口的只读资格校验。
    static uint32_t BuildList(entt::entity player, GetMissionListResponse& out);
};

class PlayerActivityReadSystem {
public:
    // 活动目录来自现有任务表；无运营排期时保持未开放。时间由调用者显式传入。
    static uint32_t BuildList(entt::entity player, uint64_t serverTimeMs, GetActivityListResponse& out);
};

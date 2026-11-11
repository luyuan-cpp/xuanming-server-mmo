#pragma once

#include <cstdint>
#include <unordered_set>

#include "entt/src/entt/entity/entity.hpp"
#include "proto/common/rollback/transaction_log.pb.h"

// 获得封禁的判定与状态入口,各获得入口在变更玩家资产之前检查。两层,互相独立:
//
//   * 全服封禁:thread_local 集合,对当前 scene 线程的全部玩家生效;
//     多线程/多节点的规则传播由既有 GM 配置与热更新接线负责,本类不跨线程读写。
//   * 单玩家封禁:
//       - 货币:CurrencyComp.blocked_types,由 CurrencySystem 判定(AddCurrency 内)。
//       - 物品:PlayerItemGainBlockComp(玩家存档字段 player_database.item_gain_block,持久数据),
//         由下面的 BlockPlayerItemGain / UnblockPlayerItemGain 改,唯一写入方是收件箱 GM 域执行器
//         (docs/design/player-inbox-service.md §2.2)。
//
// **物品获得只认 IsItemGainBlocked 一个判定**(全服 + 本人)。BagService 的三个入包入口与
// ConsumeInventoryAndGrant、资产转入(含完整实例直接入包)、商店的扣款前预检、战斗结算剔除被封掉落
// (player_battle.cpp 的 DropBlockedGains)都调它;新增物品获得入口也必须调它,
// 不要只调 IsGloballyBlocked(kItem, …),那样会漏掉单人封禁。
// 存档还原与同一所有者内部搬运(整理、仓库互转、装备穿脱)不是新增获得,不调它。
// 带交易类型的获得入口(AddCurrency、BagService::AddItems、资产转入)先问 CountsAsGain:交易退回本人托管的资产
// 也不是新增获得。
class GainBlockService
{
public:
    // 这次转入算不算新增获得。交易把玩家自己托管的资产退回(TX_AUCTION_RETURN:撤单、过期、整单退款、竞价退回)
    // 是同一所有者的资产回到名下,与上面的内部搬运同类,不受获取封禁约束。不豁免的话,被封禁的卖家取不回自己托管的东西,
    // 买家的退款也进不了账,合同只能转人工(数据覆写审计 r2-trade#1)。成交所得(TX_AUCTION_BUY)照常判封禁。
    // 只看交易类型:TX_AUCTION_RETURN 只由交易服务给退回腿使用(go/trade/internal/commerce/leg_tx_type.go),
    // 资产通道按流绑定调用方,别的服务发不出这条流。
    static constexpr bool CountsAsGain(TransactionType txType) { return txType != TX_AUCTION_RETURN; }

    enum class GainType : uint32_t
    {
        kCurrency = 0,
        kItem     = 1,
    };

    // ── Server-wide (global) blocks ──────────────────────────────────────

    // Block a specific ID globally for all players on this thread.
    static void BlockGlobal(GainType type, uint32_t id);

    // Unblock a previously globally blocked ID.
    static void UnblockGlobal(GainType type, uint32_t id);

    // Check whether an ID is globally blocked.
    static bool IsGloballyBlocked(GainType type, uint32_t id);

    // Return all globally blocked IDs of a given type (for GM query).
    static const std::unordered_set<uint32_t>& GlobalBlockedIds(GainType type);

    // Clear all global blocks (for server restart or GM reset).
    static void ClearAllGlobalBlocks();

    // ── 物品获得的唯一判定(全服 + 本人)──────────────────────────────────

    // 全服封禁了 configId,或 player 被 GM 封禁获得 configId,返回 true。只读,不创建任何组件。
    //
    // player 的口径:
    //   * 无效句柄(entt::null、已销毁):没有玩家,只判全服。BagService 的容器级调用(单测里的裸 Bag)走这里;
    //     生产调用方都先校验了玩家实体。
    //   * 有效玩家缺 PlayerItemGainBlockComp:该组件是持久数据,唯一创建点是加载
    //     (PlayerDatabaseMessageFieldsUnmarshal),缺了说明加载不变量已被破坏。这里按「没有单人封禁」回答,
    //     不补建组件:同一实体的存盘会因缺组件被整份拒绝(PlayerDatabaseMessageFieldsMarshal 打 ERROR,
    //     missing=PlayerItemGainBlockComp),资产通道与收件箱只在落盘之后才终结 / 确认,所以这种状态下发出的物品
    //     不会成为盘上事实,封禁也绕不过去(偏离持久数据「读缺失即 fail-closed」的理由与代价见设计 §2.2)。
    static bool IsItemGainBlocked(entt::entity player, uint32_t configId);

    // ── 单玩家物品获得封禁(GM 处置的效果状态,落库)────────────────────────

    // 把 configId 加进 player 的封禁表,保持升序、无重复。已封禁时直接成功(天然幂等)。
    // configId 为 0 或 player 缺 PlayerItemGainBlockComp 时回 kInvalidParameter 且不改任何状态
    // (调用方在此之前已判过,这里是最后一道防线)。不要求 configId 在本节点物品表里存在:
    // 表里没有的配置本节点发不出来,封禁照样记下,换到认识它的版本 / 节点时已经生效。
    static uint32_t BlockPlayerItemGain(entt::entity player, uint32_t configId);

    // 把 configId 从 player 的封禁表里删掉(连同可能存在的重复项)。本来没封禁时直接成功(天然幂等)。
    // 错误口径同 BlockPlayerItemGain。
    static uint32_t UnblockPlayerItemGain(entt::entity player, uint32_t configId);

    // 只看单人封禁表,不看全服。缺组件 / 无效句柄回 false(口径同 IsItemGainBlocked)。
    static bool IsPlayerItemGainBlocked(entt::entity player, uint32_t configId);
};

#pragma once

#include <functional>
#include <string>
#include <vector>

#include "engine/core/type_define/type_define.h"

#include "bag_system.h"
#include "proto/common/rollback/transaction_log.pb.h"

// Orchestration layer for bag operations.
//
// Handles cross-cutting concerns that do NOT belong in the Bag container:
//   - Global (server-wide) item block check  (GainBlockService)
//   - Transaction logging                     (TransactionLogSystem)
//   - Anomaly detection                       (AnomalyDetector)
//
// 2026-09-01 起还多一件:**为腾位而被挤掉的实例要落 LogItemDestroy**。
// 临时格(kTemporary)配的是先进先出淘汰策略,满了会销毁最早进包的实例;
// `item_uuid` 是外挂回收的关联键,不留痕追溯链就在这一步断掉。Bag 是纯容器
// 不写流水,所以它把退役清单**回执**上来,由这里落 —— 形状与 MergeAndCompact
// 那条路完全一致。批量入包走 Bag::AddItems 的候选提交；CheckSpaceFor 只做无淘汰预测。
//
// Bag itself is a pure container (grid / slot / stack management only).
class BagService
{
public:
	// 安装「新铸实例初始化回调」(契约见 item_system.h 的 ItemInstanceInitializer;
	// scene 侧的安装方是 PlayerEquipSystem::InstallItemInitializer,装的是装备掷属性)。
	//
	// **进程级**,不是每线程一份:启动时装一次,此后只读。传空 function = 卸载(单测用)。
	// 刻意不做成 thread_local —— 安装点与入包点一旦不在同一线程,回调会静默为空,
	// 装备从此不掷属性且零报错。代价是它**不带同步**:只许在还没有任何入包流量时安装
	// (启动期 / 单测的 SetUp、TearDown),运行中换回调不在支持范围内。
	//
	// 装上之后,下面三个 AddItem / AddItems 入口新铸的**每一个不可叠加实例**,在进
	// ItemStore 之前各调它一次,条件是该实例 `!has_equip()` —— 带着装备数据来的既有实例
	// (邮件回流、穿脱搬运后再经这里入包的)绝不重掷;`count = N` 的批量每件各调一次。
	// 可叠加物品不调;还原(Bag::InsertItemForRestore)与搬运原语(Bag::TakeInstance /
	// PutInstance)根本不经过本类,自然也不调。
	// Bag 自己不持有这份全局状态:回调由本类在每次调用 Bag::AddItem 时作为参数传下去。
	static void SetItemInstanceInitializer(ItemInstanceInitializer initializer);

	// Orchestrated AddItem:
	//   block check → Bag::AddItem → transaction log → anomaly detection
	static uint32_t AddItem(
		entt::entity playerEntity,
		Bag &bag,
		const InitItemParam &param);

    // 批量入包：冻结/全服禁发预检 → Bag::AddItems 候选提交 → 每项流水与异常检测。
    // 容器先完成最终空间规划和全部发号，任何拒绝都不修改原包、布局或回执，不发资产流水。
    // 发号允许烧掉尚未入包的 ID；这不是跨 Kafka 或持久化存储的事务。
    // correlationId / extra 继续关联获取流水（战斗掉落传 battle_id 与来源）。
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const ItemCountMap &itemsToAdd,
		TransactionType txType = TX_SYSTEM_GRANT,
		uint64_t correlationId = 0,
		const std::string &extra = {});

    // 同步整笔：主包按实际持有扣消耗，再把全批新增放主包或全批转临时格。先在store/layout候选中
    // 完成全部空间、发号、初始化与禁发检查；任何失败不改玩家包、不记流水。允许烧掉未提交的ID。
    // beforeCommit只给同帧外层的另一份零失败后置效果（当前为货币）：失败必须零副作用；不得yield、
    // 抛异常或修改背包。它返回成功之后，只做不抛出的两层swap及流水发布，不再有业务失败分支。
    // 策略只在本同步调用内借用原只读对象；返回时不保留任何连接、实体或策略引用。
    static uint32_t ConsumeInventoryAndGrant(entt::entity playerEntity,
        const ItemCountMap& consumed, const ItemCountMap& gained, uint64_t correlationId,
        const std::string& extra, const std::function<uint32_t()>& beforeCommit = {});

	// 上面这个按数量入包的重载,一次调用最多新铸多少个实例身份(item guid)。
	// 调用方在不可重复的副作用之前,拿它与 tlsGuidSegmentRegistry.Get(GuidKind::kItem).Available()
	// 比较,预检号段余量。号段发不出号时 AddItems 会整批拒绝,而失败已经晚了。
	//   * 口径:每个 config 按 ItemStore::StacksNeededFor(count, max_stack_size) 计。能并进已有的
	//     未满堆只会更少,所以这是上界,只会偏严。
	//   * Bag::AddItems 先做空间规划,放不下时一个号都不铸。所以「主背包放不下 → 整批转投临时格」
	//     这种两段式发放,合计最多也只铸这么多,一份余量就够。
	//   * 查不到配置行、max_stack_size 为 0、数量为 0 的项按 0 计:这类批次会因参数或配置错误整批被拒,
	//     一个号都不铸,与号段余量无关。
	//   * 只读配置表,不读写任何玩家或号段状态。
	[[nodiscard]] static uint64_t MaxNewInstancesFor(const ItemCountMap &itemsToAdd);

	// Orchestrated batch AddItems carrying full ItemComp per piece
	//   (mail attachments mixing equipment + stackable items): preserves
	//   each piece's preassigned guid / attributes.
    //   先逐项禁发/身份预检，再统一候选提交，最后按输入顺序记每项流水；原子性口径同上。
	//
	//   ⚠ 信任边界(docs/design/equipment-attributes.md §8):入参里已带 equip 段的实例**原样入库**
	//   —— has_equip() 即视为「已初始化」,不重掷,也**不校验**它的随机属性(条数、同档是否重复、
	//   attr_id 是否属于该装备的属性池;消费侧只把每条的数值夹到表上限)。容器层不认识属性,
	//   这道校验不该长在这里。本重载今天没有生产调用方(只有单测);接上任何「外来的 equip 段」
	//   之前(邮件附件、寄售 / 交易回流、Go 侧携带实例),调用方必须先在玩法层把实例校验 / 规范化,
	//   否则这里就是属性注入口(一件帽子带 50 条「所有属性」,每条都按上限计)。
	//   二期起同一段里还有强化等级与已镶宝石,同样原样入库、不校验(消费侧的夹取 / 现查口径见
	//   docs/design/equipment-enhance-inlay.md §4.3)。
	static uint32_t AddItems(
		entt::entity playerEntity,
		Bag &bag,
		const std::vector<InitItemParam> &itemsToAdd,
		TransactionType txType = TX_SYSTEM_GRANT);

	// Orchestrated RemoveItem:
	//   capture item info → Bag::RemoveItem → transaction log
	static uint32_t RemoveItem(
		entt::entity playerEntity,
		Bag &bag,
		Guid guid);

	// Orchestrated 按数量扣除(config_id → count),**按实际持有夹紧**:
	//   frozen check → Bag::RemoveItemsClamped → 逐条回执落 TX_ITEM_DESTROY。
	//
	// 用在"账已经在别处记好、这里只负责把数量落到背包"的场景 —— 今天唯一的
	// 生产调用方是战斗结算的道具消耗:引擎在快照副本上记的账本可能比玩家此刻
	// 真实持有的多(中途丢了/被扣了),契约是"不足按 0 处理并记日志,防刷"
	// (docs/design/turn-based-battle-server.md §5.3),所以不能用全或无的语义。
	//
	// correlationId / extra 落进流水(战斗传 battle_id 与来源 JSON),否则事后
	// 查不出这瓶药是哪一场战斗喝掉的,反刷追溯链断在这里。
	// drainedOut(可为 nullptr)按 config_id 升序、同 config 内槽位号升序
	// 给出被扣的 (guid, config, 数量),同槽位按实例序打破平局;
	// 调用方把它的总量与请求量一比,就知道夹紧了多少、要不要告警。
	//
	// 返回:跨 zone 冻结期返回 kInvalidParameter(一件不扣);其余恒 kSuccess。
	static uint32_t RemoveItemsClamped(
		entt::entity playerEntity,
		Bag &bag,
		const ItemCountMap &itemsToRemove,
		std::vector<DrainedInstance> *drainedOut = nullptr,
		uint64_t correlationId = 0,
		const std::string &extra = {});

	// Orchestrated 按数量扣除(config_id → count),**全或无**:
	//   参数检查 → frozen check → Bag::RemoveItems → 逐条回执落销毁流水(类型由调用方给)。
	//
	// 与上面的 RemoveItemsClamped 是一对,语义相反,不要互相替代。那条是"账已经在别处
	// 记好、不足按 0 处理"(战斗结算);这条是"玩家此刻要付出这么多,付不起就什么都别动":
	// 任何一种持有量不够就一件不扣,返回 kBagInsufficientItems。装备强化扣材料、宝石镶嵌
	// 扣宝石走这条(docs/design/equipment-enhance-inlay.md §4.1)—— 材料只扣到一半的强化
	// 没有正确的含义,不是白送就是白扣。
	//
	// 调用方必须知道的约束:
	//   * itemsToRemove 为空,或任一条目 count == 0 -> 打 ERROR,返回 kInvalidParameter,
	//     一件不扣、不落流水。这是编程错误:底层对 0 数量的结果取决于玩家是否**恰好持有**
	//     该物品(持有就放行,一个都没有就报不足),同一个调用在不同玩家身上结果不同,
	//     所以在入口判掉;调用方自己跳过不需要扣的项。
	//     这一步排在冻结闸之前:写错的调用不该因为玩家恰好在过图,就只留下一条"冻结"的
	//     WARN 而看不出是调用方的错。
	//   * 跨 zone 冻结期返回 kInvalidParameter(一件不扣),理由同 RemoveItem。
	//   * **不判战斗**(D48 红线):局中拦一切背包写的闸只许放在玩法入口层(scene 是
	//     PlayerEquipSystem 的写入口),不许下沉到这里。
    //   * 被扣光的实例与槽位立即成对释放，其余实例不挪位；调用方不得跨写持有实例指针。
	//   * 常规的"不够"请调用方先用静默的 Bag::GetTotalItemCount 预检。本函数内部的不足
	//     分支会打 ERROR 与调用栈 —— 那是给"预检过了却还是不够"这种不该发生的情形留的,
	//     不该被玩家随手点出来。
	//   * 按 config 抽数量**不看是不是装备**(不查表,也不看 has_equip()):调用方必须保证
	//     被扣的 config 不是装备。否则被抽成 0 的可能是一件带属性的装备实例(甚至就是正在
	//     被强化的那一件),它会立即退役。
	//   * 逐条流水按 config_id 升序、同 config 内槽位号升序,同槽位按实例序。
	//     按数量扣除不保证是玩家点的那一堆。
	//
	// txType 进流水,**刻意没有默认值**:加这条入口就是因为"销毁的原因"要在流水里分得开
	// (强化传 TX_EQUIP_ENHANCE、镶嵌传 TX_EQUIP_INLAY),默认成 TX_ITEM_DESTROY 等于白加。
	// correlationId / extra 同 RemoveItemsClamped(强化 / 镶嵌传装备的 item guid 与来源 JSON)。
	// 一条回执(一个被抽到的实例)落一条流水,理由同 RemoveItemsClamped。
	//
	// 返回:
	//   * 空表 / 含 0 数量的条目    -> kInvalidParameter(一件不扣)
	//   * 跨 zone 冻结期            -> kInvalidParameter(一件不扣)
	//   * 任何一种持有量不够        -> kBagInsufficientItems(一件不扣)
	//   * 其余                      -> kSuccess(每种恰好扣掉请求的数量)
	static uint32_t RemoveItemsExact(
		entt::entity playerEntity,
		Bag &bag,
		const ItemCountMap &itemsToRemove,
		TransactionType txType,
		uint64_t correlationId = 0,
		const std::string &extra = {});

	// Orchestrated 按 guid 全或无扣出(聚宝斋 P2 托管):
	//   frozen check → Bag::ReserveForBatchRemove(纯预检)→ 逐个 Bag::RemoveItem
	//   (**销毁实例**)→ 逐条落流水。
	//
	// **与 RemoveItemsClamped 语义相反,不要复用它。** 那条是按 config 夹紧、恒成功
	// (战斗结算消耗);这条是按 guid 全或无,少一件就整批拒。托管的是玩家挂出去卖的
	// 那一件具体装备,夹紧等于凭空少卖一件。
	//
	// **扣出必须销毁实例,不能留 size=0 的僵尸堆。** 托管之后权威转到交易库的快照上
	// (docs/design/jubaozhai-market.md §6.1 第 1 条),玩家 blob 里若还留着同 guid 的
	// 空壳,整理时会把它回收、跨 zone 快照会把它带走,而交易库里那份仍然存在 ——
	// 同一个 item_uuid 两处都在,过户回来就是复制。所以走 Bag::RemoveItem(销毁 +
	// 释放槽位),不走 Drain 那条"只扣数量、留着实例"的路。
	//
	// **只支持 `max_stack_size == 1` 的不可叠加物品**(由 ReserveForBatchRemove 把关):
	// 可叠加物品的预设 guid 在合堆时会被重新铸造,发回来对不上。
	//
	// **不拦战斗**(D48 红线):局中拦一切背包写的闸只许放在资产 RPC 入口层。下沉到
	// 这里,战斗结算自己的扣物就会被自己拦住,结算永久卡死。
	//
	// txType 进流水(托管扣出传 TX_AUCTION_SELL),correlationId 传 listing_id。
	// removedOut(可为 nullptr)按入参顺序给出销毁前抓拍的 (guid, config, size)。
	//
	// mutated(可为 nullptr)说明 RemoveItemsByGuid 这次失败时，
	// 包到底动过没有。函数开头一律先置 false。预检拒与冻结拒都是一件不扣
	// (mutated 保持 false,调用方按 RETRY / REJECTED 记账,不记资产变更);
	// commit 段中途失败则已有实例被销毁并落了流水,置 true —— 调用方**必须**按
	// APPLIED + partial 记账转人工补偿,不许当成零改动重投(04-asset-channel.md
	// §4.33:已改动却不记账 = 重复发放或永久少发)。
	//
	// 返回:
	//   * 跨 zone 冻结期            -> kAssetFrozen(RETRY 类,一件不扣,mutated=false)
	//   * 预检不过                  -> kAssetInvalidBundle(REJECTED 类,一件不扣,mutated=false)
	//   * commit 段中途失败         -> Bag 层原始错误码(kBagDeleteItemFindGuid 等),
	//                                 **批次已半截且无回滚**,mutated=true;预检已排除
	//                                 全部数据可触发的原因,走到这里即两层状态不自洽的
	//                                 编程错误,函数会打 ERROR。不要把它当"一件不扣"。
	//   * 其余                      -> kSuccess
	static uint32_t RemoveItemsByGuid(
		entt::entity playerEntity,
		Bag &bag,
		const std::vector<Guid> &guids,
		TransactionType txType = TX_AUCTION_SELL,
		uint64_t correlationId = 0,
		std::vector<DestroyedInstance> *removedOut = nullptr,
		bool *mutated = nullptr);

	// Orchestrated MergeAndCompact (背包整理):
	//   frozen check → Bag::MergeAndCompact → transaction log per retired instance
	//
	// **生产代码整理背包一律走这个入口,不要直接调 Bag::MergeAndCompact。**
	// 整理会退役一批 item_uuid(被合并空的堆),而
	// item_uuid 正是 transaction_log 做外挂回收关联的键 —— 直接调纯容器那层,
	// 这些实例就无声消失了,追溯链在整理这一步断掉。
	//
	// **policy 刻意没有默认值。** "开背包要不要自动重排"是一条会和"跨服还原
	// 保持原位"直接冲突的产品取舍,拆分前从来没人做过这个取舍(只有重排一种
	// 行为)。现在把它变成调用点必须填的参数 —— 你没法在不表态的情况下整理背包:
	//   * 自动触发(开背包、入包后顺手整理)-> kMergeOnly,绝不挪位置
	//   * 玩家显式点"整理"                  -> kMergeAndReorder
	//
	// changedOut(可为 nullptr)透传 Bag::MergeAndCompact 的返回值:
	// 背包是否真的动过(已最优时会早退,返回 false)。
	static uint32_t MergeAndCompact(
		entt::entity playerEntity,
		Bag &bag,
		CompactPolicy policy,
		bool *changedOut = nullptr);

	// 玩家点击"整理"。
	//
	// **2026-08-27 拍板:整理是玩家的显式动作,不是开背包的副作用。**
	// 所以这是目前唯一会重排位置的入口 —— 背包 UI 上那个"整理"按钮接到这里。
	//
	// 自动触发的路径(开背包、入包后顺手收拾)应当走
	// MergeAndCompact(..., CompactPolicy::kMergeOnly):只合并堆叠、回收空实例,
	// 一格不挪。这样跨服回来"东西还在你离开时摆的地方"才成立。
	//
	// 以后若要做成"打开背包自动整理"的玩家设置,不需要改这两层中的任何一层:
	// 读设置,然后决定调这个还是调 kMergeOnly 那个。CompactPolicy 就是为这一天
	// 留的口子。
	static uint32_t SortByPlayerRequest(
		entt::entity playerEntity,
		Bag &bag,
		bool *changedOut = nullptr);
};

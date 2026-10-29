#pragma once

#include <cstdint>
#include <map>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

class PetListInfo;
class PetInstance;
class BattlePetSnapshot;
class BattlePetSettlementData;

// 宝宝(宠物)系统,设计文档 docs/design/player-pet.md。
//
// 唯一入口不变量:PlayerPetComp 的任何写入只经本类静态函数;每个写操作成功后都
// Recalculate(同步等级 + 按新上限处理当前 HP/MP)。
// 返回值:kSuccess 以外一律为 tip id(table/proto/tip/pet_error_tip.pb.h),handler 直接搬进
// TipInfoMessage。
//
// 与角色属性加点的关系:点数总量、"目标已分配"校验、按比例保持 HP/MP 全部复用
// attributerules;维度与池取表中 owner_type=1 的行。宝宝的二级属性**不落库、不缓存**,
// 需要时由 ComputeDerived 现算,全仓只有"已分配点 + 资质 + 主人等级"三个输入是真相。
//
// 跨 zone 冻结(PlayerFrozenComp)与战斗在途(InBattleComp)期间所有写操作拒绝:
// 快照已出,改宝宝会让局内单位与面板分叉(与 PlayerAttributeSystem::CheckWritable 同口径)。
//
// 线程模型:全部方法都在 scene 节点 loop 线程调用。
//
// 组件归属:PlayerPetComp 由加载(PlayerDatabaseMessageFieldsUnmarshal)挂上,本类所有入口只取不建。
// 缺组件是服务端故障:返回错误码的入口(含 BuildList)一律返回 kServiceUnavailable(写入口在任何扣费 /
// 改动之前返回),BuildBattleSnapshot 答 false，调用方须先校验 PlayerPetComp 存在(见 PlayerBattleSystem::BuildBattleSnapshot)；
// 其余重算入口跳过，PushList 不推；都记 ERROR。
// 错误码口径与 player_mission.cpp / skill.cpp 一致:实体无效 → kEntityIsNull,实体有效但缺必需组件 → kServiceUnavailable。
// 使用主人等级的初始化修补、重算、召唤、加点、洗点、发放还要求已加载 LevelComp;
// 缺失时在清理存档、扣费或铸号之前拒绝 / 跳过,不拿默认 1 级覆盖宝宝。BuildList 读取已存宝宝等级,
// 交易恢复保留快照等级,这两处不额外依赖主人 LevelComp。
class PetSystem {
public:
	// 从 DB 加载 / 首登后调用:清理已失效引用、同步等级、按上限夹当前 HP/MP。
	// 已分配点原样携带,不清理、不返还:本版本宝宝池之外的键是未知键(不生效、计入已用),超额的加点整池按 0 生效
	// (加点统一规则,attribute_allocation_rules.h;docs/design/mixed-version-persist-field-protection.md §12.1 D3 / D4)。
	static void InitializeOnLoad(entt::entity player);

	// 重算的触发原因(与 PlayerAttributeSystem::RecalcReason 同语义):
	//   kLevelChanged 宝宝升级(主人升级带动):上限抬高按绝对增量补当前值,降级只夹;
	//   其它:按比例保持,一升一降往返不净得(不能把加点/洗点变成宝宝的治疗手段;
	//         极低血量因"至少留 1"有上界的回升例外,见 attributerules::RescaleCurrent)。
	enum class RecalcReason : uint8_t { kLoad, kLevelChanged, kAllocate, kReset };

	// 重算全部宝宝(同步等级 + 处理当前 HP/MP);不落库,不改写已分配点。
	// 只有 kLevelChanged 会因主人等级低于出战宝宝而取消出战;kLoad 不写 active_pet_id
	// (召唤与战斗快照本来就按主人等级拦截)。
	static void RecalculateAll(entt::entity player, RecalcReason reason = RecalcReason::kLoad);

	// 面板全量(客户端零配表:维度名/说明/上限/剩余点/资质/成长率都在这里)。
	// 返回值:kSuccess;实体无效 kEntityIsNull;缺 PlayerPetComp(加载链路故障)kServiceUnavailable。
	// 非 kSuccess 时 list 是清空的 —— 调用方必须回错误码,不能把空列表当成功发给客户端。只读,不改变组件集合。
	static uint32_t BuildList(entt::entity player, PetListInfo& list);

	// 出战 / 收回。出战同时只能有一只(PetRule.max_pets 管的是携带量,不是出战量)。
	static uint32_t Summon(entt::entity player, uint64_t petId);
	static uint32_t Recall(entt::entity player);

	// 确认加点:target 为该宝宝"目标已分配"(全量、幂等;缺省维度视为不变)。目标只能是宝宝池已知维度;
	// 只写真正变化的键。剩余点扣掉未知键,加点不生效时剩余为 0(回「点数不足」)。
	static uint32_t Allocate(entt::entity player, uint64_t petId,
							 const std::map<uint32_t, uint32_t>& target);

	// 洗点:清空该宝宝在宝宝池已知维度上的分配,未知键原样保留;按 AttributePool 表扣金币。
	// 已知维度一点没加(含只剩未知键)→ kPetNothingToChange 且不扣费;加点不生效 → 免费并打 WARN
	// (metric=allocation_reset_free_ineffective)。费用与列表 reset_cost_gold 同一口径。
	static uint32_t Reset(entt::entity player, uint64_t petId);

	// 自动加点:按 AttributeAutoPlan(宝宝池)算建议"目标已分配",只算不落
	static uint32_t AutoAllocate(entt::entity player, uint64_t petId,
								 std::map<uint32_t, uint32_t>& suggested);

	static uint32_t Rename(entt::entity player, uint64_t petId, const std::string& name);

	// GM / 未来的捕捉与购买共用的发放入口:铸 pet_id、掷资质、落 PlayerPetComp。
	// petIdOut 仅在返回 kSuccess 时有效。
	static uint32_t GrantPet(entt::entity player, uint32_t petTableId, uint64_t& petIdOut);

	// —— 交易资产原语(聚宝斋 P2;通用资产通道 docs/design/guild-phase2/04-asset-channel.md §4.9)——
	//
	// 这两个函数是**纯原语**:只判"结构性"条件(实体在不在、宝宝在不在、出战没出战、槽位满没满),
	// 一律**不判**跨 zone 冻结 / 传送在途 / 战斗中。那三条是"暂时条件",必须由资产 RPC 入口层
	// (PlayerAssetOpSystem::Process)统一判成 RETRY 并且**不记账**;下沉到这一层就会被调用方
	// 当成终局 REJECTED 记进账本,战斗中的一次交付会被永久判死(D48 红线,与 BagService /
	// CurrencySystem 同一条纪律)。所以这里不复用 CheckWritable。
	//
	// 两者都不推列表、不写流水:推送与 transaction_log 由资产入口层在同一次处理里统一做,
	// 否则"取出成功但回包丢了"的重投会推两次、记两条。
	//
	// 返回值给调用方的映射建议(入口层负责):kPetSlotFull → RETRY(买家腾位后可成),其余非
	// kSuccess → REJECTED(终局)。例外:kServiceUnavailable 是「宝宝组件缺失」—— 加载链路故障而非
	// 交易本身不成立,应与入口层对账本组件缺失的处置同口径(RETRY、不记账),不能判成终局。

	// 取出用于交易:把 petId 这只宝宝整条移出 PlayerPetComp,移出前的完整实例写进 snapshotOut。
	// 快照就是托管期间的权威副本(聚宝斋 §6.1 第 1 条),调用方须原样持久化。
	// 出战中的宝宝拒绝取出(kPetAlreadyActive),提示玩家先收回:出战态是战斗快照的输入,
	// 让它在局中消失等于战斗单位凭空没了。
	// 失败时 snapshotOut 被清空,组件不发生任何改动。
	static uint32_t RemovePetForTrade(entt::entity player, uint64_t petId, PetInstance& snapshotOut);

	// 还原:把 RemovePetForTrade 取到的快照**整条**放回(买家交付,或卖家下架回退)。
	// 保留 pet_id / 资质 / 已分配点 / created_at / 当前 HP MP —— 刻意**不复用 GrantPet**:
	// 它会重掷资质、按新主人等级重定级、回满血蓝、盖新 created_at,那样"买到的"就不是"卖出的"。
	// pet_id 撞号拒绝(同一玩家名下已有该 id)。
	//
	// 快照来自交易库(托管期间不在 scene 的玩家存档里),所以这是唯一一个输入不受
	// scene 自己把关的宠物写入口。实例整条原样放回、什么都不夹:level、资质、已分配点、当前 HP/MP 与未知字段
	// 都按快照保留 —— 这是已有实例的移动,本版本调低的 level_cap 不能把托管的合法资产改写成另一只。
	// 低级主人用不了高级宝宝,由召唤与战斗快照入口按主人等级复核;身份、配置存在、字节上限等不可信输入
	// 由资产通道入口(PlayerAssetOpSystem)在任何副作用之前校验。
	static uint32_t RestorePetFromSnapshot(entt::entity player, const PetInstance& snapshot);

	// —— 战斗接缝(scene/battle/system/player_battle.cpp 调用)——

	// 出战宝宝的战斗快照。没有出战宝宝返回 false；调用前须确认 PlayerPetComp 存在。
	// 缺组件同样返回 false，不能据此当作正常开战。
	static bool BuildBattleSnapshot(entt::entity player, ::BattlePetSnapshot& snapshot);

	// 结算回写:HP/MP 终值按现算上限夹后落实例;阵亡宝宝回满(与玩家同口径,
	// 否则 0 血宝宝会一直被快照进新局、开局即倒)。未知 pet_id 忽略并告警。
	static void ApplyBattleSettlement(entt::entity player, const ::BattlePetSettlementData& settlement);

	// 主动推列表(主人升级带动宝宝升级、结算回写等);BuildList 失败时不推。
	static void PushList(entt::entity player);

	// 宝宝属性点池的 owner_type 值(AttributePool.owner_type)
	static constexpr uint32_t kPoolOwnerPet = 1;
};

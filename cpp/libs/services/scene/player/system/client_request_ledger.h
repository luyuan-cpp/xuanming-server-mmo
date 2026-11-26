#pragma once

// 客户端请求幂等键:scene 侧的去重账本(docs/design/client-request-idempotency.md)。
//
// 要防的事:玩家点了强化 / 购买 / 新建方案,回包还没到又点了一次;或者客户端等不到回包(超时、断线、
// gate 回 1003),把同一个请求重发了一次。两条请求都会到 scene,没有去重时第二条会再扣一次钱、再发一次货。
//
// 契约(与 Stripe Idempotency-Key、AWS ClientToken 同一形状):
//   - 客户端每发起一次操作生成一个非 0 的 client_request_id。这一次操作拿到明确结果(成功,或带 tip 的拒绝)
//     之前的所有重发都带同一个键,其余字段原样不变。0 = 旧客户端,不去重,行为与加字段之前完全相同。
//   - scene 按 (玩家, 键) 去重。键第一次出现时正常执行,**执行成功**才记下「键 -> 种类 + 参数指纹 + 首次结果」。
//     之后同键、同种类、同参数的请求不再执行,回首次结果(背包、货架这类状态取当前值),并在应答里标 replayed:
//     第二遍不会执行(防重),客户端也能知道第一次成了(幂等)。
//   - 同一个键换了参数或换了请求种类 = 客户端把旧键用在了新操作上(客户端缺陷):回 kInvalidParameter,不执行。
//   - 执行失败(业务拒绝)不记:那一次没有副作用,同一个键重发会重新判定。
//
// 存储(负责人 2026-11-06 定,取代 11-05 的「只放内存」):账本是玩家存档的一个字段
// (player_database.client_request_ledger = 25,proto 在 client_request_ledger_comp.proto),与它保护的
// 货币、背包、限购记录同一次存盘提交(不变量 I3):效果和记录要么一起在盘上,要么一起没有。所以崩溃回档、
// 下线重登、跨节点迁移之后,带原键的重发照样认得出来。
//   - 组件类别是「持久数据」(ecs-component-access-rules.md):唯一创建点是加载(PlayerDatabaseMessageFieldsUnmarshal),
//     任何路径都不懒建;存盘缺它整份拒存。
//   - 加载校验不过:挂 ClientRequestLedgerInvalidComp,带键的请求一律回 kServiceUnavailable、不执行(fail-closed),
//     不带键的请求照常;原数据不改写。
//
// 线程模型:只在 scene loop 线程读写,没有锁。同一名玩家的请求在这条线程上逐条同步执行,首次执行完之前
// 重发不可能开始,所以不需要「处理中」状态。将来哪个接入方改成等下游再回包(RpcDoneCallback::Adopt),
// 必须先给账本补上「处理中」状态,否则首次还在等下游时,重发会被当成新操作。

#include <cstddef>
#include <cstdint>
#include <iterator>
#include <string>
#include <string_view>

#include "entt/src/entt/entity/entity.hpp"
#include "proto/common/component/client_request_ledger_comp.pb.h"

namespace google::protobuf
{
class MessageLite;
}

// 接入了幂等键的请求种类。值随存档落盘(ClientRequestRecord.kind):**不可重排、不可复用**,删掉的种类留空号。
// 同一个键只属于一种:跨种类复用按「换了参数」拒绝。0 不是合法种类。
// 最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2)。
enum class ClientRequestKind : uint8_t
{
	kUnspecified = 0,
	kEnhanceEquip = 1,            // SceneBagClientPlayer.EnhanceEquip
	kBuyMysteryMerchantGoods = 2, // SceneMysteryMerchantClientPlayer.BuyMysteryMerchantGoods
	kCreateAttributeScheme = 3,   // SceneAttributeClientPlayer.CreateAttributeScheme
	kEquipmentCraft = 4,         // 打造与首次安装词条；另有forge_revision防止回执淘汰后重做。
	kEquipmentMaterialReward = 5, // 材料自选领取，额度水位防止回执淘汰后再领。
	kBuyAfkPass = 6,              // SceneAfkPassClientPlayer.BuyAfkPass(docs/design/afk-monthly-card.md §4.1);首次结果取当前月卡状态
	kSwitchAppearance = 7,        // SceneAppearanceClientPlayer.SwitchAppearance:消费角色形象卡永久切换形象
	kStartAppearanceTrial = 8,    // SceneAppearanceClientPlayer.StartAppearanceTrial:开启某形象 1 小时试用(位图记账)
	kCatchupSeasonSeal = 9,       // SceneSeasonClientPlayer.CatchupSeasonSeal:小额灵石快捷追赶赛季印进度
	kClaimTrialJournalMilestone = 10, // SceneTrialJournalClientPlayer.ClaimTrialJournalMilestone:领取试炼志战法印里程碑奖励
	kRedeemBoundArtifactVoucher = 11, // SceneTrialJournalClientPlayer.RedeemBoundArtifactVoucher:使用自用法宝拓印兑换绑定法宝
	kSkillScrollCraft = 12,           // SceneSkillClientPlayer.CraftSkillBook:分卷宗拓印常规/稀世技能书
	kLearnSkillBook = 13,             // SceneSkillClientPlayer.LearnSkillBook:研习未学技能书永久解锁
	kDismantleSkillBook = 14,         // SceneSkillClientPlayer.DismantleSkillBook:拆解未学常规书返还残页
	kBuySpiritShopGoods = 15,         // SceneSpiritShopClientPlayer.BuySpiritShopGoods:官方灵石百宝阁购买
	kUnlockTravelPassPremium = 16,    // SceneActivityClientPlayer.UnlockTravelPassPremium:解锁游历手记典藏轨
	kClaimTravelPassReward = 17,      // SceneActivityClientPlayer.ClaimTravelPassReward:领取游历手记奖励
	kEquipPetCosmetic = 18,           // SceneAppearanceClientPlayer.EquipPetCosmetic:耗券永久解锁并穿戴灵宠幻化
	kEquipSkillVfx = 19,              // SceneAppearanceClientPlayer.EquipSkillVfx:耗券永久解锁并启用技能特效
	kEquipLoadoutUnlock = 20,         // SceneBagClientPlayer.UnlockEquipLoadout:付费或耗扩容符解锁装备方案位
	kClaimRewardedAd = 21,            // SceneRewardedAdClientPlayer.ClaimRewardedAd:激励视频完播领绑定银两
	kCount
};

// 查账结论。
enum class ClientRequestVerdict : uint8_t
{
	kNew,         // 没见过这个键(或它已被挤出账本):按正常流程执行
	kReplay,      // 见过,种类与参数都相同:不执行,按首次结果回包
	kMismatch,    // 见过,但种类或参数不同:旧键被用在了新操作上,拒绝(kInvalidParameter)且不执行
	kUnavailable, // 这名玩家的账本缺失或加载校验不过:带键的请求不执行,回 kServiceUnavailable(fail-closed)
	kCount
};

// 名字表:日志用。下标即枚举值,static_assert 防脱节。
inline constexpr const char *kClientRequestKindNames[] = {"unspecified", "enhance_equip", "buy_mystery_merchant_goods",
														  "create_attribute_scheme", "equipment_craft", "equipment_material_reward",
														  "buy_afk_pass", "switch_appearance", "start_appearance_trial",
														  "catchup_season_seal", "claim_trial_journal_milestone",
														  "redeem_bound_artifact_voucher", "skill_scroll_craft",
														  "learn_skill_book", "dismantle_skill_book",
														  "buy_spirit_shop_goods",
														  "unlock_travel_pass_premium", "claim_travel_pass_reward",
														  "equip_pet_cosmetic", "equip_skill_vfx",
														  "equip_loadout_unlock", "claim_rewarded_ad"};
static_assert(std::size(kClientRequestKindNames) == static_cast<std::size_t>(ClientRequestKind::kCount),
			  "kClientRequestKindNames is out of sync with ClientRequestKind");

inline constexpr const char *kClientRequestVerdictNames[] = {"new", "replay", "mismatch", "unavailable"};
static_assert(std::size(kClientRequestVerdictNames) == static_cast<std::size_t>(ClientRequestVerdict::kCount),
			  "kClientRequestVerdictNames is out of sync with ClientRequestVerdict");

// 越界回 "?"。返回 const char*:muduo 的 LogStream 没有 string_view 重载。
inline const char *ClientRequestKindName(ClientRequestKind kind)
{
	const auto index = static_cast<std::size_t>(kind);
	return index < std::size(kClientRequestKindNames) ? kClientRequestKindNames[index] : "?";
}

inline const char *ClientRequestVerdictName(ClientRequestVerdict verdict)
{
	const auto index = static_cast<std::size_t>(verdict);
	return index < std::size(kClientRequestVerdictNames) ? kClientRequestVerdictNames[index] : "?";
}

// 首次执行结果里,重放要回、却推不出来的那部分(背包、货架、面板这类状态,重放时取当前值)。
// 每个字段只对注释里的那个种类有意义,其余种类保持默认值。落盘时对应 ClientRequestRecord 的 5~7 号字段。
struct ClientRequestResult
{
	bool enhanceSuccess{false};  // kEnhanceEquip:那一次掷骰成没成
	uint32_t enhanceLevel{0};    // kEnhanceEquip:那一次强化之后的等级(后来又强化过的话,当前等级会不同)
	uint32_t enhancePityCount{0}; // kEnhanceEquip:那一次强化之后的保底累计失败次数（成功归 0）
	uint32_t createdSchemeId{0}; // kCreateAttributeScheme:那一次建出来的方案号
	uint64_t equipmentItemId{0};
	uint64_t forgeRevision{0};
	uint64_t materialRewardRevision{0};
	uint32_t craftingMaterialCount{0};
	uint32_t enhancementStoneCount{0};
	uint32_t craftingTradableCount{0};
	uint32_t enhancementTradableCount{0};
	uint32_t seasonCatchupPoints{0};
	uint64_t seasonCatchupSpent{0};
	uint32_t trialMilestoneId{0};
	uint32_t trialMilestoneSandCount{0};
	uint32_t trialMilestoneVoucherCount{0};
	uint64_t redeemedArtifactItemId{0};
	uint32_t redeemedArtifactConfigId{0};
	uint32_t equipmentQualityTier{0};
	uint32_t equipmentPolishPityCount{0};
	bool equipmentGuaranteedByPity{false};
	uint64_t craftedSkillBookId{0};
	uint32_t craftedSkillTableId{0};
	uint64_t scrollCraftRevision{0};
	uint32_t scrollBonusFragments{0};
	uint32_t travelPassClaimedCount{0};
	uint32_t equipLoadoutUnlockedCount{0};
	uint32_t equipLoadoutUsedItemId{0};
	uint64_t equipLoadoutSpentBindDiamond{0};
	uint64_t equipLoadoutSpentDiamond{0};
	uint32_t rewardedAdCurrencyType{0};
	uint64_t rewardedAdAmount{0};
};

// 参数指纹 = 请求清掉 client_request_id 之后做确定性序列化,取 SHA-256 的前 16 字节。
// 指纹随存档落盘,算法是契约,**不能改**:改了之后,老存档里的记录与新算出的指纹永远对不上,同键重发会被判成换了参数。
// 请求以后加了字段会自动算进指纹,不用记得回来改这里。
inline constexpr std::size_t kClientRequestFingerprintBytes = 16;
std::string ClientRequestFingerprintDigest(const google::protobuf::MessageLite &requestWithoutKey);

// 按值传入:清键改的是副本,不动调用方手里的请求。Request 必须带 client_request_id 字段。
template <typename Request>
std::string ClientRequestFingerprint(Request request)
{
	request.clear_client_request_id();
	return ClientRequestFingerprintDigest(request);
}

// 对落盘账本的纯函数:不碰 ECS、不取时间、不打日志,可以单测(照 asset_op_ledger)。
namespace client_request_ledger
{
// 每名玩家最多记 32 条,满了淘汰 seq 最小的那条。回包没到的窗口是十几秒(客户端 15s 超时),这期间一名玩家
// 成功执行的带键操作远少于 32 次;被挤掉的键再来,只会被当成一次新操作,退化成没有去重时的行为,不会凭空多发东西。
inline constexpr int kCapacity = 32;
// 加载校验能容忍的条数上限,比 kCapacity 宽:将来下调容量时,旧存档不会因此被判坏(多出来的在下一次记账时淘汰)。
inline constexpr int kMaxRecordsOnLoad = 256;

// key 为 0(不去重)一律答 kNew。只有 kReplay 会写 result,其余结论不动它。不会答 kUnavailable(那是接入层的判断)。
ClientRequestVerdict Check(const ClientRequestLedgerComp &ledger, uint64_t key, ClientRequestKind kind,
						   std::string_view fingerprint, ClientRequestResult &result);

// 记下一次**已经成功**的请求:先淘汰到不足 kCapacity 条,再追加,seq 取 next_seq。
// key 为 0,或这个键已在账本里时什么都不做,返回 false(调用方按「先 Check、再执行、成功后 Remember」走,正常碰不到)。
bool Remember(ClientRequestLedgerComp &ledger, uint64_t key, ClientRequestKind kind, std::string fingerprint,
			  const ClientRequestResult &result);

// 加载校验。返回空串 = 自洽;否则是原因(进日志)。判坏的条件:条数超过 kMaxRecordsOnLoad;键为 0 或重复;
// 指纹不是 16 字节;seq 为 0、重复、或不小于 next_seq(next_seq 为 0 时按 1)。
// 不校验 kind:新版本写进来的种类本版本不认识,原样携带即可 —— 同一个键再来时按「种类不同」拒绝。
std::string Validate(const ClientRequestLedgerComp &ledger);
} // namespace client_request_ledger

// 运行时标签(不入库、不序列化):加载时账本校验不过就挂上,带键的请求据此一律 fail-closed。
// 照 PlayerInboxLedgerInvalidComp:不阻断登录、不改写原数据、实体在本节点存活期间一直挂着。
struct ClientRequestLedgerInvalidComp
{
};

// 接入方(handler 或系统入口)用的两个函数。都只在 scene loop 线程调用。
//
// 接入顺序(每个接入点都一样):
//   1. key = request.client_request_id();key == 0 时跳过 2、4,走原流程。
//   2. Check 放在一切依赖状态的校验**之前**(战斗中、冻结、余额、物品在不在、轮次对不对、乐观比较……):
//      首次已经成功,状态已经变了,重放要是先过这些校验,就会被拒掉,客户端会以为第一次也失败了。
//      kReplay -> 按首次结果与当前状态组应答,标 replayed,直接返回;kMismatch -> kInvalidParameter,直接返回;
//      kUnavailable -> kServiceUnavailable,直接返回。
//   3. 原流程。
//   4. 原流程返回成功之后、组应答之前 Remember(与生效在同一次同步调用里,中间不让出 EventLoop)。
//      应答组装失败(快照建不出来)也照样记:那时操作已经生效了。
namespace client_request_dedup
{
// 只读:不创建组件,陈旧句柄也安全(try_get 按版本判归属)。kReplay / kMismatch / kUnavailable 各打一行带 metric= 的日志。
ClientRequestVerdict Check(entt::entity player, uint64_t key, ClientRequestKind kind, std::string_view fingerprint,
						   ClientRequestResult &result);

// key 为 0 时什么都不做。账本缺失或已判坏时打 ERROR、什么都不做(正常走不到:第 2 步已经按 kUnavailable 返回)。
void Remember(entt::entity player, uint64_t key, ClientRequestKind kind, std::string fingerprint,
			  const ClientRequestResult &result);
} // namespace client_request_dedup

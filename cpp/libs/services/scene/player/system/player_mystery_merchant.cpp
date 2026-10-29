#include "player_mystery_merchant.h"

#include <limits>
#include <string>

#include <hiredis/hiredis.h>
#include <muduo/base/Logging.h>
#include <muduo/net/EventLoop.h>

#include "core/utils/registry/actor_log.h"
#include "engine/core/time/system/time.h"
#include "network/node_utils.h"
#include "network/client_message_sender.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"

#include "battle/system/player_battle.h"
#include "modules/bag/bag_service.h"
#include "modules/bag/comp/player_bags_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "modules/gain_block/gain_block_service.h"
#include <modules/scene/comp/scene_comp.h>
#include "player/system/mystery_merchant_rules.h"
#include "player/system/player_feature_snapshot.h"
#include "player/system/player_lifecycle.h"

#include "rpc/service_metadata/player_mystery_merchant_service_metadata.h"
#include "table/code/item_table.h"
#include "table/code/mysterymerchantgoods_table.h"
#include "table/code/mysterymerchantrule_table.h"
#include "table/code/mysterymerchantspot_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/mystery_merchant_error_tip.pb.h"

#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_mystery_merchant_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h" // PlayerSessionSnapshotComp(公告只发给有会话的玩家)
#include "proto/common/rollback/transaction_log.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
#include "proto/scene/scene_info.pb.h"

namespace
{

// 记录所在的 Redis 键(HASH)与 field。两个都是**跨语言契约**:写入方是
// go/scene_manager/internal/logic/mystery_merchant.go 的 MysteryMerchantKeyFmt / MysteryMerchantStateField
// (那边的占位符是 Go 的 %d,这边是 C printf 的 %u;契约是渲染出来的键逐字节相同:
// mystery_merchant:zone:{zone 十进制})。改一边必须改另一边。
constexpr char kMysteryMerchantKeyFmt[] = "mystery_merchant:zone:%u";
constexpr char kMysteryMerchantStateField[] = "state";

// 多久从 Redis 读一次记录。它决定:整点之后本节点最晚几秒知道商人来了(并发出公告),
// 以及商人换线之后旧线最晚几秒停止营业。每个 scene 节点每 5 秒一次 HGET,开销可以忽略。
// 购买的时间窗按记录自带的离开时刻判,不受这个周期影响 —— 缓存再旧也不会卖过点。
constexpr double kRefreshIntervalSec = 5.0;

// 规则表缺行时的购买距离兜底(与 data/MysteryMerchantRule.xlsx 的初值相同)。
constexpr uint32_t kDefaultInteractRange = 8;
constexpr uint32_t kMaxInteractRange = 100;

// 本节点缓存的那份记录。只属于 scene 的 EventLoop 线程:与 tlsEcs / tlsRedis 同一个所有权口径,用 thread_local 持有。
// 它不是组件 —— 不挂在任何实体上,也就不受实体销毁、registry 清理的影响。
struct MysteryMerchantCache
{
	bool hasState = false;
	::MysteryMerchantState state;
	// 已经向本节点玩家公告过的最新版本(merchantrules::StateVersion)。0 = 还没公告过任何一份。
	uint64_t announcedVersion = 0;
};

thread_local MysteryMerchantCache tlsMerchantCache;

MysteryMerchantCache &Cache()
{
	return tlsMerchantCache;
}

// 刷新定时器(单定时器形态:整个进程只有一个,Start / Stop 成对;与 PlayerBattleSystem 的 reaper 同一写法)。
muduo::net::TimerId gRefreshTimerId;
bool gRefreshActive = false;
muduo::net::EventLoop *gRefreshLoop = nullptr;

// 玩家当前所在场景的实例 id;不在任何场景时返回 0。
uint64_t CurrentSceneId(entt::entity player)
{
	const auto *sceneEntityComp = tlsEcs.actorRegistry.try_get<SceneEntityComp>(player);
	if (sceneEntityComp == nullptr || sceneEntityComp->sceneEntity == entt::null)
	{
		return 0;
	}
	// SceneEntityComp::sceneEntity 是 sceneRegistry 的句柄,SceneInfoComp 只挂在 sceneRegistry 上。
	const auto *sceneInfo = tlsEcs.sceneRegistry.try_get<SceneInfoComp>(sceneEntityComp->sceneEntity);
	return sceneInfo != nullptr ? sceneInfo->scene_id() : 0;
}

// 购买距离:规则表第一行的 interact_range,钳到 [1, kMaxInteractRange];表里没有行时用兜底值。
// 行指针现查现用,不出函数(热更后旧快照当场析构,data/AGENTS.md)。
uint32_t InteractRange()
{
	const auto &rows = MysteryMerchantRuleTableManager::Instance().FindAll().data();
	if (rows.empty() || rows.begin()->interact_range() == 0)
	{
		return kDefaultInteractRange;
	}
	const uint32_t range = rows.begin()->interact_range();
	return range > kMaxInteractRange ? kMaxInteractRange : range;
}

// 货架上一件商品「此刻能不能卖、卖多少钱」的完整解析结果。全部按值保存,不留表行指针。
struct ResolvedGoods
{
	uint32_t goodsId = 0;
	uint32_t itemConfigId = 0;
	uint32_t itemCount = 0;
	CurrencyType currencyType = kCurrencyGold;
	uint64_t originalPrice = 0;
	uint64_t price = 0;
	uint32_t discountPercent = 0;
	uint32_t limitCount = 0;
	std::string name;
	std::string description;
	std::string iconKey;
};

// 把一条货架记录解析成可售商品。任何一项对不上都返回 false,reason 指向一句说明(字面量,不必释放):
// 记录是 scene_manager 按它当时的表写的,这里按本节点此刻的表再核一遍 —— 两边的表版本不一致
// (只滚动更新了一半)或热更改了行时,宁可这件商品暂时买不了,也不按半截数据成交。
// 下发货架与结算走的是同一个函数,所以「玩家看到的价格」与「实际扣的价格」不可能分叉。
bool ResolveGoods(const ::MysteryMerchantShelfEntry &entry, ResolvedGoods &out, const char *&reason)
{
	const auto [goodsRow, goodsErr] = MysteryMerchantGoodsTableManager::Instance().FindByIdSilent(entry.goods_id());
	if (goodsRow == nullptr)
	{
		reason = "goods row not found in MysteryMerchantGoods";
		return false;
	}
	// weight == 0 = 已下架。货架是 scene_manager 排这一轮时定的,这里再认一次,是为了给运营留一个**立即停售**的手段:
	// 把这一行的 weight 改成 0 并热更 scene 的表,正在卖的这一轮里它马上买不了(不必等到下一轮)。
	if (goodsRow->weight() == 0)
	{
		reason = "goods delisted (weight is 0)";
		return false;
	}
	if (!merchantrules::IsValidDiscountPercent(entry.discount_percent()))
	{
		reason = "discount percent not in 1..100";
		return false;
	}
	if (goodsRow->price() == 0 || goodsRow->price() > merchantrules::kMaxGoodsPrice)
	{
		reason = "original price not in 1..1000000000";
		return false;
	}
	if (goodsRow->item_count() == 0)
	{
		reason = "item count per bundle is 0";
		return false;
	}
	if (goodsRow->limit_count() == 0 || goodsRow->limit_count() > merchantrules::kMaxBuyCountPerRequest)
	{
		reason = "limit count not in 1..99";
		return false;
	}
	if (!IsKnownCurrencyType(goodsRow->currency_type()))
	{
		reason = "unknown currency type";
		return false;
	}
	const auto [itemRow, itemErr] = ItemTableManager::Instance().FindByIdSilent(goodsRow->item_id());
	if (itemRow == nullptr)
	{
		reason = "item not found in Item table";
		return false;
	}
	if (goodsRow->item_count() > itemRow->max_stack_size())
	{
		reason = "item count per bundle exceeds the item max stack size";
		return false;
	}

	out.goodsId = goodsRow->id();
	out.itemConfigId = goodsRow->item_id();
	out.itemCount = goodsRow->item_count();
	out.currencyType = static_cast<CurrencyType>(goodsRow->currency_type());
	out.originalPrice = goodsRow->price();
	out.discountPercent = entry.discount_percent();
	out.price = merchantrules::DiscountedPrice(goodsRow->price(), entry.discount_percent());
	out.limitCount = goodsRow->limit_count();
	// 展示名:商品表有就用商品表的(与帮会商店同一个习惯),否则用物品表的;都没有则留空,客户端不许编造。
	out.name = goodsRow->name().empty() ? itemRow->name() : goodsRow->name();
	out.description = itemRow->description();
	out.iconKey = itemRow->icon_key();
	return true;
}

// 填「所有人看到的都一样」的那部分:在不在、在哪条线、站在哪、什么时候走。
// 调用方保证 state 此刻在场。here 与 goods 因人而异,不在这里填。
void FillPublicInfo(const ::MysteryMerchantState &state, uint64_t nowMs, ::MysteryMerchantInfo &out)
{
	out.set_active(true);
	out.set_round_id(state.round_id());
	out.set_scene_config_id(state.scene_config_id());
	out.set_scene_id(state.scene_id());
	out.set_channel_no(state.channel_no());
	out.mutable_position()->CopyFrom(state.position());
	out.set_appear_at_ms(state.appear_at_ms());
	out.set_expire_at_ms(state.expire_at_ms());
	out.set_next_appear_at_ms(merchantrules::NextAppearAtMs(state, nowMs));
	out.set_server_time_ms(nowMs);
	out.set_interact_range(InteractRange());
}

// 向本节点的全部在线玩家发一次出现公告。
//
// 每个 scene 节点只管自己身上的玩家:全 zone 的玩家分布在各个节点上,各节点各发各的,合起来恰好
// 每人一条。刻意不用 BroadcastToAllClientsOnKnownGates(发给本节点连着的每个 gate 上的全部会话):那样每个节点
// 都会让全服收到一条,玩家会收到「节点数」条重复公告。
void Announce(const ::MysteryMerchantState &state, uint64_t nowMs)
{
	// 只收「有会话快照、且会话号有效」的玩家:带着 Player 标签却没有可用会话的实体(进场 / 离场途中),
	// 广播层会为每一个打一条 ERROR,每轮公告一批。他们进场时会自己查询,不靠这条公告。
	// 这只是挡掉一眼看得出发不到的;会话号看着有效而 gate 已经不认的,仍由广播层自己处理。
	EntityVector players;
	for (const auto entity : tlsEcs.actorRegistry.view<Player>())
	{
		const auto *session = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(entity);
		if (session == nullptr || session->gate_session_id() == kInvalidSessionId)
		{
			continue;
		}
		players.push_back(entity);
	}
	if (players.empty())
	{
		return;
	}
	::MysteryMerchantNotify notify;
	FillPublicInfo(state, nowMs, *notify.mutable_info());
	BroadcastToPlayerClients(SceneMysteryMerchantClientPlayerNotifyMysteryMerchantMessageId, notify, players);
	LOG_INFO << "[MysteryMerchant] Appearance announcement sent: round_id=" << state.round_id() << " revision=" << state.revision()
			 << " scene_config_id=" << state.scene_config_id() << " channel_no=" << state.channel_no()
			 << " scene_id=" << state.scene_id() << " players=" << players.size();
}

// 用一份新读到的记录更新缓存,并判定要不要公告。incoming 为 nullptr = Redis 里已经没有记录。
void ApplyState(const ::MysteryMerchantState *incoming, uint64_t nowMs)
{
	auto &cache = Cache();
	if (incoming == nullptr)
	{
		if (cache.hasState)
		{
			LOG_INFO << "[MysteryMerchant] Record disappeared from Redis (expired or feature disabled); clearing cache: round_id="
					 << cache.state.round_id();
		}
		cache.hasState = false;
		cache.state.Clear();
		return;
	}

	const uint64_t version = merchantrules::StateVersion(*incoming);
	if (cache.hasState && merchantrules::ShouldDiscardAsStale(*incoming, cache.state, nowMs))
	{
		// 手里那一轮还没结束,却读到版本更旧的:只可能是 Redis 主从切换丢了最近的写。
		// 保留手里较新的那份:它已经公告过、可能已经卖过东西,退回去会让同一轮出现两条线。
		// 手里那一轮结束之后这道保护自动解除(见 ShouldDiscardAsStale),不会永久拒收。
		LOG_WARN << "[MysteryMerchant] Read a record older than the cache; not adopting it before the cached round ends: incoming_version=" << version
				 << " cached_version=" << merchantrules::StateVersion(cache.state);
		return;
	}
	cache.state.CopyFrom(*incoming);
	cache.hasState = true;

	// 同一份记录每 5 秒会被读到一次:公告过的不再发。相等之外还挡「更小」:键被清掉(过期 / 手工删除)之后
	// 读回一份更早的记录时,不把早就公告过的那一轮再播一遍。
	// 例外是 scene_manager 强制覆盖出的、版本更小的**新**一轮(改了出现间隔):它的出现时刻早于已公告的那一轮,
	// 这里不补公告,玩家靠角标与进场查询得知 —— 改间隔是运维操作,不值得为它放宽去重。
	if (version <= cache.announcedVersion)
	{
		return;
	}
	if (nowMs >= cache.state.expire_at_ms())
	{
		// 这一轮已经结束(节点刚启动 / Redis 断连了很久才恢复):不补发过期的公告,只记下「见过了」。
		cache.announcedVersion = version;
		return;
	}
	if (nowMs < cache.state.appear_at_ms())
	{
		// 本机时钟比 scene_manager 慢了一点,出现时刻还没到:先不记,下一次刷新(5 秒后)再判。
		// 这里要是记成「已公告」,这个节点上的玩家就永远收不到这一轮的公告了。
		return;
	}
	cache.announcedVersion = version;
	Announce(cache.state, nowMs);
}

// "HGET 记录" 的应答处理。只有「应答是字符串、能解析、且确实是本 zone 的记录」才更新缓存;
// 其余情况一律保留手里的旧缓存(购买的时间窗按记录自带的离开时刻判,旧缓存不会卖过点)。
// 日志分级的依据与分线目录相同(player_scene.cpp ParseChannelDirectoryReply):
// 键不存在是正常状态;读失败是 WARN;应答形状 / 字节 / zone 对不上说明两侧契约被破坏,是 ERROR。
void OnStateReply(const redisReply *reply, uint64_t nowMs)
{
	if (reply == nullptr)
	{
		LOG_WARN << "[MysteryMerchant] No reply when reading the record (Redis disconnected); keeping the cached record";
		return;
	}
	if (reply->type == REDIS_REPLY_NIL)
	{
		ApplyState(nullptr, nowMs);
		return;
	}
	if (reply->type == REDIS_REPLY_ERROR)
	{
		LOG_WARN << "[MysteryMerchant] Redis rejected the record read; keeping the cached record: err=" << (reply->str != nullptr ? reply->str : "");
		return;
	}
	// 先判 type 再读 str / len;长度保护是因为 ParseFromArray 只收 int。
	if (reply->type != REDIS_REPLY_STRING || reply->str == nullptr ||
		reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
	{
		LOG_ERROR << "[MysteryMerchant] Record reply has an invalid shape; keeping the cached record: reply_type=" << reply->type;
		return;
	}
	::MysteryMerchantState parsed;
	if (!parsed.ParseFromArray(reply->str, static_cast<int>(reply->len)))
	{
		LOG_ERROR << "[MysteryMerchant] Failed to parse the record; keeping the cached record: bytes=" << reply->len;
		return;
	}
	if (parsed.zone_id() != GetZoneId())
	{
		LOG_ERROR << "[MysteryMerchant] Record belongs to another zone; keeping the cached record: record_zone_id=" << parsed.zone_id()
				  << " zone_id=" << GetZoneId();
		return;
	}
	ApplyState(&parsed, nowMs);
}

// 发起一次异步读。Redis 没连上时直接跳过(保留旧缓存),下一拍再试。
void RequestRefresh()
{
	auto &redis = tlsRedis.GetZoneRedis();
	if (!redis || !redis->connected())
	{
		return;
	}
	// 回调不捕获任何东西:它只动全局缓存,不引用任何可能先于它销毁的对象。
	const int ret = redis->command(
		[](hiredis::Hiredis *, redisReply *reply) { OnStateReply(reply, TimeSystem::NowMillisecondsUTC()); },
		(std::string("HGET ") + kMysteryMerchantKeyFmt + " " + kMysteryMerchantStateField).c_str(), GetZoneId());
	if (ret != REDIS_OK)
	{
		LOG_WARN << "[MysteryMerchant] Record read command was not sent; keeping the cached record: ret=" << ret;
	}
}

// 这次购买的来源说明,落进物品流水的 extra(JSON)。全是数字,不需要转义。
std::string PurchaseExtra(const ::MysteryMerchantState &state, const ResolvedGoods &goods, uint32_t count)
{
	return std::string("{\"source\":\"mystery_merchant\",\"round_id\":") + std::to_string(state.round_id()) +
		   ",\"goods_id\":" + std::to_string(goods.goodsId) + ",\"count\":" + std::to_string(count) +
		   ",\"unit_price\":" + std::to_string(goods.price) + ",\"discount_percent\":" +
		   std::to_string(goods.discountPercent) + "}";
}

} // namespace

void PlayerMysteryMerchantSystem::Start(muduo::net::EventLoop *loop)
{
	if (loop == nullptr)
	{
		LOG_ERROR << "[MysteryMerchant] Start: loop is null";
		return;
	}
	if (gRefreshActive)
	{
		// 幂等:先取消再重挂,避免重复定时器
		loop->cancel(gRefreshTimerId);
		gRefreshActive = false;
	}
	gRefreshLoop = loop;
	gRefreshTimerId = loop->runEvery(kRefreshIntervalSec, [] { RequestRefresh(); });
	gRefreshActive = true;
	// 立即先读一次:不等第一个 5 秒,节点一启动就知道商人在不在。
	RequestRefresh();
	LOG_INFO << "[MysteryMerchant] Record refresh started, interval=" << kRefreshIntervalSec << "s";
}

void PlayerMysteryMerchantSystem::Stop()
{
	if (!gRefreshActive || gRefreshLoop == nullptr)
	{
		return;
	}
	gRefreshLoop->cancel(gRefreshTimerId);
	gRefreshActive = false;
	LOG_INFO << "[MysteryMerchant] Record refresh stopped";
}

void PlayerMysteryMerchantSystem::ValidateTables()
{
	const auto &ruleRows = MysteryMerchantRuleTableManager::Instance().FindAll().data();
	if (ruleRows.empty())
	{
		LOG_ERROR << "[MysteryMerchant] MysteryMerchantRule table is empty; purchase range falls back to " << kDefaultInteractRange;
	}
	else if (ruleRows.begin()->interact_range() == 0 || ruleRows.begin()->interact_range() > kMaxInteractRange)
	{
		LOG_ERROR << "[MysteryMerchant] MysteryMerchantRule.interact_range=" << ruleRows.begin()->interact_range()
				  << " is not in 1.." << kMaxInteractRange << ", effective value " << InteractRange();
	}

	uint32_t sellable = 0;
	for (const auto &row : MysteryMerchantGoodsTableManager::Instance().FindAll().data())
	{
		if (row.weight() == 0)
		{
			continue; // 0 = 暂不上架,是合法配置
		}
		// 借 ResolveGoods 核一遍:它就是下发与结算时的那道判定,这里说「能卖」与运行时说「能卖」是同一个口径。
		::MysteryMerchantShelfEntry probe;
		probe.set_goods_id(row.id());
		probe.set_discount_percent(merchantrules::kMaxDiscountPercent);
		ResolvedGoods resolved;
		const char *reason = "";
		if (!ResolveGoods(probe, resolved, reason))
		{
			LOG_ERROR << "[MysteryMerchant] MysteryMerchantGoods row is not sellable; players will not be able to buy it once shelved: id=" << row.id()
					  << " reason=" << reason;
			continue;
		}
		++sellable;
	}
	LOG_INFO << "[MysteryMerchant] Table check finished: sellable_goods=" << sellable << " spots="
			 << MysteryMerchantSpotTableManager::Instance().Count() << " interact_range=" << InteractRange();
}

void PlayerMysteryMerchantSystem::FillInfo(entt::entity player, uint64_t nowMs, ::MysteryMerchantInfo &out)
{
	out.Clear();
	out.set_server_time_ms(nowMs);

	const auto &cache = Cache();
	if (!cache.hasState)
	{
		return; // 还没有任何一轮的记录:不在场,下一次出现时刻未知(0)
	}
	out.set_next_appear_at_ms(merchantrules::NextAppearAtMs(cache.state, nowMs));
	if (!merchantrules::IsPresent(cache.state, nowMs))
	{
		return;
	}
	FillPublicInfo(cache.state, nowMs, out);

	if (!tlsEcs.actorRegistry.valid(player))
	{
		return;
	}
	const uint64_t playerSceneId = CurrentSceneId(player);
	if (playerSceneId == 0 || playerSceneId != cache.state.scene_id())
	{
		return; // 不在同一条线:只知道「在几线」,看不到货架
	}
	out.set_here(true);

	// try_get:只读路径,绝不在这里造组件。组件缺失(不该发生)时已购数按 0 显示,真正的拦截在 Buy。
	const auto *record = tlsEcs.actorRegistry.try_get<PlayerMysteryMerchantComp>(player);
	for (const auto &entry : cache.state.shelf())
	{
		ResolvedGoods goods;
		const char *reason = "";
		if (!ResolveGoods(entry, goods, reason))
		{
			// 这件此刻卖不了(原因已在配表加载时打过日志,结算路径也会再打):不上架,免得玩家点了才被拒。
			continue;
		}
		auto *info = out.add_goods();
		info->set_goods_id(goods.goodsId);
		info->set_item_config_id(goods.itemConfigId);
		info->set_item_count(goods.itemCount);
		info->set_name(goods.name);
		info->set_description(goods.description);
		info->set_icon_key(goods.iconKey);
		info->set_currency_type(static_cast<uint32_t>(goods.currencyType));
		info->set_original_price(goods.originalPrice);
		info->set_price(goods.price);
		info->set_discount_percent(goods.discountPercent);
		info->set_limit_count(goods.limitCount);
		info->set_bought_count(
			record != nullptr ? merchantrules::BoughtCount(*record, cache.state.round_id(), goods.goodsId) : 0);
	}
}

uint32_t PlayerMysteryMerchantSystem::Buy(entt::entity player, const ::BuyMysteryMerchantGoodsRequest &request,
										  uint64_t nowMs, ::BuyMysteryMerchantGoodsResponse &response)
{
	response.clear_info();
	response.clear_bag();

	// ── 第一段:只读预检。这一段里任何一处 return 都保证「什么都没改」 ──────────────────

	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	// 冻结 / 战斗两道闸与装备穿脱同口径(player_equip.cpp CheckWritable);冻结用的判定函数与
	// BagService / CurrencySystem 内部用的是同一个,所以这里放行的请求不会在后面被它们以「冻结」拒绝。
	if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
	{
		return kMysteryMerchantFrozen;
	}
	if (PlayerBattleSystem::IsInBattle(player))
	{
		return kMysteryMerchantInBattle;
	}
	if (request.goods_id() == 0 || request.count() == 0 || request.count() > merchantrules::kMaxBuyCountPerRequest)
	{
		return kMysteryMerchantInvalidCount;
	}

	const auto &cache = Cache();
	if (!cache.hasState || !merchantrules::IsPresent(cache.state, nowMs))
	{
		return kMysteryMerchantNotPresent;
	}
	const auto &state = cache.state;
	if (request.round_id() != state.round_id())
	{
		return kMysteryMerchantShelfChanged; // 客户端手里还是上一轮的货架
	}
	const uint64_t playerSceneId = CurrentSceneId(player);
	if (playerSceneId == 0 || playerSceneId != state.scene_id())
	{
		return kMysteryMerchantNotHere;
	}
	const auto *transform = tlsEcs.actorRegistry.try_get<Transform>(player);
	if (transform == nullptr)
	{
		LOG_ERROR << "[MysteryMerchant] Purchase failed: player has no Transform component " << ActorLog(player);
		return kMysteryMerchantInternalError;
	}
	if (!merchantrules::WithinInteractRange(transform->location().x(), transform->location().y(),
											state.position().x(), state.position().y(), InteractRange()))
	{
		return kMysteryMerchantTooFar;
	}

	const auto *entry = merchantrules::FindShelfEntry(state, request.goods_id());
	if (entry == nullptr)
	{
		return kMysteryMerchantShelfChanged; // 这件不在本轮货架上
	}
	ResolvedGoods goods;
	const char *reason = "";
	if (!ResolveGoods(*entry, goods, reason))
	{
		LOG_ERROR << "[MysteryMerchant] Purchase failed: shelved goods is not sellable right now " << ActorLog(player)
				  << " round_id=" << state.round_id() << " goods_id=" << request.goods_id() << " reason=" << reason;
		return kMysteryMerchantInternalError;
	}
	// 报价防呆:价格、货币、物品、每份数量,任何一项与玩家看到的不同(配表热更)都拒绝,让客户端刷新后再买。
	if (request.expected_price() != goods.price ||
		request.expected_currency_type() != static_cast<uint32_t>(goods.currencyType) ||
		request.expected_item_config_id() != goods.itemConfigId || request.expected_item_count() != goods.itemCount)
	{
		return kMysteryMerchantShelfChanged;
	}

	// try_get + fail-closed:限购记录缺失时不许现造一份空的来放行(那等于名额清零)。
	auto *record = tlsEcs.actorRegistry.try_get<PlayerMysteryMerchantComp>(player);
	if (record == nullptr)
	{
		LOG_ERROR << "[MysteryMerchant] Purchase failed: player has no purchase record component " << ActorLog(player);
		return kMysteryMerchantInternalError;
	}
	const uint32_t bought = merchantrules::BoughtCount(*record, state.round_id(), goods.goodsId);
	if (merchantrules::WouldExceedLimit(bought, request.count(), goods.limitCount))
	{
		return kMysteryMerchantLimitReached;
	}

	int64_t totalCost = 0;
	const uint64_t totalItems = static_cast<uint64_t>(goods.itemCount) * request.count();
	if (!merchantrules::TotalCost(goods.price, request.count(), totalCost) ||
		totalItems > std::numeric_limits<uint32_t>::max())
	{
		LOG_ERROR << "[MysteryMerchant] Purchase failed: total cost or total item count overflows " << ActorLog(player)
				  << " goods_id=" << goods.goodsId << " unit_price=" << goods.price << " item_count=" << goods.itemCount
				  << " count=" << request.count();
		return kMysteryMerchantInternalError;
	}

	auto *bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
	if (bags == nullptr)
	{
		LOG_ERROR << "[MysteryMerchant] Purchase failed: player has no bag component " << ActorLog(player);
		return kMysteryMerchantInternalError;
	}
	auto &inventory = bags->bags[kInventory];
	// 封禁与余额先判,是为了给玩家一个看得懂的原因;发物自己还会再判一遍封禁与空间(见第二段)。
	if (GainBlockService::IsGainBlocked(GainBlockService::GainType::kItem, goods.itemConfigId))
	{
		return kMysteryMerchantItemBlocked;
	}
	// 扣钱会失败的全部条件(金额非正、币种未知、冻结、缺货币组件、余额不足)到这里都已排除:
	// 金额与币种在上面校验过,冻结在入口判过,缺组件时 CanAfford 按余额 0 返回 false。
	if (!CurrencySystem::CanAfford(player, goods.currencyType, totalCost))
	{
		return kMysteryMerchantCurrencyNotEnough;
	}
	ItemCountMap items;
	items[goods.itemConfigId] = static_cast<uint32_t>(totalItems);
	if (inventory.CheckSpaceFor(items) != kSuccess)
	{
		return kMysteryMerchantBagFull;
	}

	// ── 第二段:修改。顺序是 发物 → 扣钱 → 记名额,同一次调用里同步完成 ──────────────────
	//
	// 为什么先发物、后扣钱(与宝宝洗点「先扣费再改状态」相反):那里的「改状态」不会失败,这里的发物会 ——
	// 封禁、背包空间之外,还有物品号段不可用(data_service 故障时的既定 fail-closed)、表数据异常等上面预检不到的原因。
	//   - 先发物:BagService::AddItems 是候选提交,「被拒 = 原包一件没动、不发流水」(它的接口契约)。
	//     所以发物失败时钱还没动,直接拒绝即可,**不需要退款路径**;
	//   - 后扣钱:它会失败的条件上面已经全部排除,而且从那里到这里是同一次同步调用,余额不可能变。
	// 反过来(先扣钱)的话,发物失败就得把钱退回去,而仓里唯一的加钱入口 AddCurrency 会先抵扣补缴欠款、
	// 遇到币种封禁还会拒绝 —— 有欠款或被封禁币种的玩家会「钱扣了、货没到、退款也没拿到」。

	// 两条流水(发物、扣钱)用同一个 correlation_id 串起来。它在「这名玩家的这一轮」内唯一即可定位一笔购买:
	// 流水本身带 player_id 与时间戳,发物那条的 extra 里有商品与份数。
	const uint64_t correlationId = state.round_id();
	const auto grantErr = BagService::AddItems(player, inventory, items, TX_SHOP_BUY, correlationId,
											   PurchaseExtra(state, goods, request.count()));
	if (grantErr != kSuccess)
	{
		// 一件没发出去,钱也还没动:干净的拒绝。空间与封禁上面判过,走到这里多半是号段不可用。
		LOG_ERROR << "[MysteryMerchant] Purchase failed: item grant rejected after precheck (nothing charged) " << ActorLog(player)
				  << " round_id=" << state.round_id() << " goods_id=" << goods.goodsId
				  << " item_config_id=" << goods.itemConfigId << " item_total=" << totalItems << " err=" << grantErr;
		return kMysteryMerchantInternalError;
	}

	// 走到这里,货已经整批进包。之后无论发生什么都照价扣钱、照记名额:少扣等于白送,不记名额等于还能再买一遍。
	const auto deductErr =
		CurrencySystem::DeductCurrency(player, goods.currencyType, totalCost, TX_SHOP_BUY, correlationId);
	merchantrules::RecordPurchase(*record, state.round_id(), goods.goodsId, request.count());
	if (deductErr != kSuccess)
	{
		// 预检刚过就扣不动:不该发生(编程错误)。货已经给了,名额也记了 —— 损失以这件商品的限购份数为上限。
		// 固定标签 [UNPAID_GRANT] 便于告警规则匹配,转人工追缴。
		LOG_ERROR << "[MysteryMerchant][UNPAID_GRANT] Items granted but the charge was rejected; manual recovery required: " << ActorLog(player)
				  << " round_id=" << state.round_id() << " goods_id=" << goods.goodsId
				  << " item_config_id=" << goods.itemConfigId << " item_total=" << totalItems << " cost=" << totalCost
				  << " currency_type=" << static_cast<uint32_t>(goods.currencyType) << " err=" << deductErr;
		return kMysteryMerchantInternalError;
	}

	LOG_INFO << "[MysteryMerchant] Purchase succeeded: " << ActorLog(player) << " round_id=" << state.round_id()
			 << " goods_id=" << goods.goodsId << " count=" << request.count() << " unit_price=" << goods.price
			 << " discount_percent=" << goods.discountPercent << " cost=" << totalCost
			 << " currency_type=" << static_cast<uint32_t>(goods.currencyType) << " item_config_id=" << goods.itemConfigId
			 << " item_total=" << totalItems;

	// ── 第三段:组应答。购买已经生效,这里出任何问题都不回滚 ──────────────────────────
	FillInfo(player, nowMs, *response.mutable_info());
	if (const auto snapshotErr = PlayerBagSystem::BuildSnapshot(player, kInventory, *response.mutable_bag());
		snapshotErr != kSuccess)
	{
		// 背包快照建不出来不改变「买到了」这个事实:应答里不带背包,客户端发现缺失会自己重新拉一次。
		response.clear_bag();
		LOG_ERROR << "[MysteryMerchant] Purchase succeeded but the bag snapshot failed; replying without the bag: " << ActorLog(player)
				  << " err=" << snapshotErr;
	}
	return kSuccess;
}

void PlayerMysteryMerchantSystem::ApplyStateForTest(const ::MysteryMerchantState *state, uint64_t nowMs)
{
	ApplyState(state, nowMs);
}

void PlayerMysteryMerchantSystem::ResetForTest()
{
	auto &cache = Cache();
	cache.hasState = false;
	cache.state.Clear();
	cache.announcedVersion = 0;
}

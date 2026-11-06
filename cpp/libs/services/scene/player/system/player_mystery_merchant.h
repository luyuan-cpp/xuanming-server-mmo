#pragma once

// 神秘商人在 scene 节点上的全部职责(docs/design/mystery-merchant.md §5):
//   1. 周期性地从 zone Redis 读「这一轮商人在哪、卖什么」的记录(由 scene_manager 写入)并缓存;
//   2. 发现一份没见过的记录时,向本节点的在线玩家发一次出现公告;
//   3. 回答客户端的查询(GetMysteryMerchantInfo);
//   4. 校验并执行购买(BuyMysteryMerchantGoods):同线、够近、在时间窗内、报价对得上、没超限购 → 发物 → 扣钱 → 记名额。
//
// 服务端没有神秘商人的实体:它在这里只是「一份记录 + 购买校验」,形象由客户端自己画。
//
// 线程模型:全部状态(记录缓存、定时器)属于 scene 的 EventLoop 线程,所有入口都必须在那条线程上调用
// (RPC handler、定时器回调、hiredis 回调本来就都在那条线程)。没有锁。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

namespace muduo::net
{
class EventLoop;
}

class MysteryMerchantState;
class MysteryMerchantInfo;
class BuyMysteryMerchantGoodsRequest;
class BuyMysteryMerchantGoodsResponse;

class PlayerMysteryMerchantSystem
{
public:
	// 起周期刷新(每 5 秒读一次记录,启动后立即先读一次)。幂等:重复调用只保留一个定时器。
	// 在节点 SetAfterStart 里调;与 Stop 成对。
	static void Start(muduo::net::EventLoop *loop);
	// 停掉周期刷新。停机入口调;之后缓存不再更新(已缓存的那一轮仍按自己的离开时刻失效)。
	static void Stop();

	// 配表加载完成后核对神秘商人用到的三张表,把问题打进日志(不阻断启动:坏行只会让对应的商品买不了)。
	// 在 TableLoadHandler::OnLoaded 里调。
	static void ValidateTables();

	// 把神秘商人此刻对这名玩家可见的状态写进 out(out 会先被清空)。纯读取,不改任何状态。
	// nowMs 由调用方传入(RPC 入口取墙钟),便于单测。
	static void FillInfo(entt::entity player, uint64_t nowMs, ::MysteryMerchantInfo &out);

	// 购买。返回 kSuccess 或一个 tip 码(mystery_merchant_error / common_error)。
	//
	// 原子性:先做只读预检;之后依次是 发物 → 扣钱 → 记名额,三步在同一次调用里同步完成,中间不让出 EventLoop。
	// 发物被拒时钱还没动(直接拒绝,没有退款路径);发物成功后扣钱不会失败(失败条件在预检里全部排除)。
	// 顺序的理由见 .cpp 里第二段开头的注释。
	//
	// 成功时 response.info 是最新货架(已购数已更新),response.bag 是人物背包全量;
	// 失败时 response 的这两项保持为空。
	//
	// 幂等(docs/design/client-request-idempotency.md):request.client_request_id 非 0 时,同一名玩家的同一个键
	// 至多成交一次。同键同参数的重发不再执行,回 kSuccess + 当前货架与背包 + replayed = true;
	// 同键换了参数回 kInvalidParameter,不执行;这名玩家的账本缺失或已判坏回 kServiceUnavailable,不执行。
	// 账本随玩家存档落盘(player_database.client_request_ledger),与钱、货、限购记录同一次提交。
	static uint32_t Buy(entt::entity player, const ::BuyMysteryMerchantGoodsRequest &request, uint64_t nowMs,
						::BuyMysteryMerchantGoodsResponse &response);

	// ── 单测入口 ──
	// 直接替换缓存的记录(nullptr = 没有记录),等价于「刚从 Redis 读到这一份」,包括公告判定。
	static void ApplyStateForTest(const ::MysteryMerchantState *state, uint64_t nowMs);
	// 清空缓存与「已公告到哪个版本」的记忆,回到进程刚启动的状态。
	static void ResetForTest();
};

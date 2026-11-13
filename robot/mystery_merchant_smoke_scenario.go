package main

// merchant-smoke 场景:单机器人对「神秘商人」做端到端冒烟
// (docs/design/mystery-merchant.md §2 协议 / §5 购买流程 / §9 验证清单):
//
//	M1 登录进场,记下当前 scene_id / scene_config_id;
//	M2 查询:GetMysteryMerchantInfo。商人不在场 → MERCHANT_SMOKE_SKIP(前置条件不满足,退出码 0);
//	   在场则校验形状:scene_id / channel_no / interact_range 非 0、离开时刻晚于服务器当前时刻、带站位;
//	M3 到商人那条线:不在同一条线就 EnterScene 切过去(与 channel-smoke 同一条路),再查一次,
//	   这次必须 here == true 且货架非空、每件商品的折后价 == ceil(原价 × 折扣 / 100);
//	M4 走到商人旁边:MoveStop 上报站位旁 1 个单位的点(服务端按导航网格校验直线可达);
//	M5 负例(都不该动钱):份数 0 → kMysteryMerchantInvalidCount;错误的轮次号,以及报价四项(单价 / 货币 / 物品 /
//	   每份数量)任何一项与货架不符 → kMysteryMerchantShelfChanged;
//	M6 GM 加银两(够买两次),买 1 份:应答无错;货架上这件的 bought_count +1;背包全量里的银两 == 加钱后的余额 − 单价;
//	   背包里出现这件物品;
//	   随后同一个请求对象原样重发一次(步骤名 replay,docs/design/client-request-idempotency.md):应答无错且标 replayed,
//	   银两没有再扣、物品没有再发、bought_count 不变;
//	M7 买到限购上限(剩余名额一次买完),再买 1 份 → kMysteryMerchantLimitReached;
//	M8 再查一次:服务端记的 bought_count == limit_count;
//	M9 正常下线、重新登录进场,再原样发一次 M6 的请求(步骤名 replay-after-relogin):幂等键账本随存档落盘,
//	   仍须应答无错、标 replayed、银两与物品同下线前最后一份成功应答。
//
// 每次购买(含 M5 的负例)都带一个新的幂等键 client_request_id;只有 replay / replay-after-relogin 复用 M6 那个请求。
//
// 结果约定(供外层脚本消费):
//
//	全过 → 日志一行 `MERCHANT_SMOKE_OK player_id=… zone=… round_id=… channel_no=… goods_id=… …`,退出码 0;
//	前置条件不满足 → `MERCHANT_SMOKE_SKIP reason=…`,退出码 0(商人不在场 / 商人不在机器人所在的地图 /
//	  货架上每件都已被本账号买满,即同一轮里跑了第二遍);
//	任一步失败 → `MERCHANT_SMOKE_FAIL step=… reason=…`,退出码 1。
//	每步通过另打一行 `[merchant-smoke] PASS step=…`。
//
// 环境要求(不满足会 SKIP 或在 M4 之后报「太远」,详见 etc/merchant_smoke.yaml):
//   - scene_manager 配了 MysteryMerchant.IntervalSecondsOverride == StaySecondsOverride(商人一直在场),
//     且 SpotIdOverride 钉在出生点旁的站位:机器人只会直线上报一个点,远处的站位中间隔着建筑走不到;
//   - 一轮的时长要比跑一遍冒烟(约 20 秒)长得多:轮次在 M3 与 M7 之间切换会让购买被拒(kMysteryMerchantShelfChanged),
//     本场景遇到这种情况会明确报「撞上换轮」,重跑即可。
//
// 传输层直接复用 channel-smoke 的机器人会话(channelSmokeBot:登录、按节奏发送、有同步应答的请求经 call
// 按请求号认领回包、推送按到达顺序留底、切线)。本文件只多登记两类消息号:经 call 发的请求
// (merchantSmokeIsCallMessage,由 channelSmokeIsCallMessage 转调,供 onMessage 丢弃不带请求号的同号消息),
// 以及要留底的推送(merchantSmokeIsRecorded,由 channelSmokeIsRecorded 转调)。

import (
	"errors"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/component"
	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。99xx 与其它冒烟(见 channelSmokeAccount 的注释)错开。
// 账号必须**不在任何队伍里**(理由同 channel-smoke:队员切线后会被队长拉回)。
const merchantSmokeAccount = "robot_9901"

const (
	// 机器人站到离商人站位多远的地方(服务器坐标单位)。必须小于规则表的 interact_range(初值 8)。
	merchantSmokeStandOff = 1.0
	// MoveStop 发出后等服务端落位的时间:它没有回包(返回类型 Empty),只能等一小会儿再买。
	merchantSmokeSettle = 500 * time.Millisecond
	// GM 加的银两:商品池里最贵的 4 折后约 8000,取够买两次的量。
	merchantSmokeGrantGold int64 = 100000
	// M9 下线之后等这么久再登录:让 scene 把下线存盘落到库里(与 equip-forge / attribute 冒烟的重登同值)。
	// 太快重登会被当成「重连取代登出」,玩家实体还留在内存里,验不到幂等键账本随存档落盘。
	merchantSmokeReloginWait = 2 * time.Second
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipMerchantInvalidCount = uint32(tiptable.MysteryMerchantError_kMysteryMerchantInvalidCount)
	tipMerchantShelfChanged = uint32(tiptable.MysteryMerchantError_kMysteryMerchantShelfChanged)
	tipMerchantLimitReached = uint32(tiptable.MysteryMerchantError_kMysteryMerchantLimitReached)
	tipMerchantTooFar       = uint32(tiptable.MysteryMerchantError_kMysteryMerchantTooFar)
	tipMerchantNotHere      = uint32(tiptable.MysteryMerchantError_kMysteryMerchantNotHere)
)

// merchantSmokeIsCallMessage:本场景经 call 发出、回包只按请求号认领的请求消息号(由 channelSmokeIsCallMessage 转调)。
// 三者的应答类型都不是 Empty,scene 恰好回一个带请求号的包(scene_handler.cpp ProcessClientPlayerMessage;
// scene 派发前早退时 gate 回带请求号的 1003 信封(docs/design/client-rpc-callback.md §7 R1),merchantCall 按信封拒绝报错)。
func merchantSmokeIsCallMessage(messageId uint32) bool {
	switch messageId {
	case game.SceneMysteryMerchantClientPlayerGetMysteryMerchantInfoMessageId,
		game.SceneMysteryMerchantClientPlayerBuyMysteryMerchantGoodsMessageId,
		game.SceneCurrencyClientPlayerGmAddCurrencyMessageId:
		return true
	}
	return false
}

// merchantSmokeIsRecorded:本场景额外要按到达顺序留底的推送(由 channelSmokeIsRecorded 转调)。
func merchantSmokeIsRecorded(messageId uint32) bool {
	return messageId == game.SceneMysteryMerchantClientPlayerNotifyMysteryMerchantMessageId
}

// merchantCall 按节奏发一个请求并只等它自己的同步应答(channelSmokeBot.call,按请求号认领),解到 response 里。
// gate 信封拒绝(限流 / GM 指令在非 dev 环境被拦)与等不到应答一律 error:它们说明请求没到 scene 的业务逻辑。
// 回包不经 onMessage,生成的 handler 不会跑;调用方只读这里解出的 response。
func (b *channelSmokeBot) merchantCall(messageId uint32, request, response proto.Message) error {
	envelopeTip, err := b.call(messageId, request, response)
	if errors.Is(err, errChannelSmokeRpcTimeout) {
		return fmt.Errorf("message_id=%d 发出后 %s 内没有应答(scene 没换上带神秘商人的新版?gate 的客户端消息白名单没重新生成?)",
			messageId, channelSmokeRpcTimeout)
	}
	if err != nil {
		return err
	}
	if envelopeTip != 0 {
		return fmt.Errorf("message_id=%d 被 gate 信封拒绝 tip=%d%s:请求没到 scene",
			messageId, envelopeTip, channelSmokeTipHint(envelopeTip))
	}
	return nil
}

func (b *channelSmokeBot) merchantInfo() (*scene.MysteryMerchantInfo, error) {
	var response scene.GetMysteryMerchantInfoResponse
	if err := b.merchantCall(game.SceneMysteryMerchantClientPlayerGetMysteryMerchantInfoMessageId,
		&scene.GetMysteryMerchantInfoRequest{}, &response); err != nil {
		return nil, err
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		return nil, fmt.Errorf("GetMysteryMerchantInfo 应答带 tip=%d%s,查询不该有业务拒绝", tip, channelSmokeTipHint(tip))
	}
	if response.GetInfo() == nil {
		return nil, fmt.Errorf("GetMysteryMerchantInfo 应答没有 info")
	}
	return response.GetInfo(), nil
}

// merchantQuote 是购买请求里「玩家看到的那份报价」:轮次 + 报价四项。服务端发现任何一项与它现算的不符就拒绝。
type merchantQuote struct {
	roundId      uint64
	goodsId      uint32
	price        uint64
	currencyType uint32
	itemConfigId uint32
	itemCount    uint32
}

// merchantQuoteOf 把货架上的一件商品原样抄成报价(真实客户端也是这么做的)。
func merchantQuoteOf(roundId uint64, goods *scene.MysteryMerchantGoodsInfo) merchantQuote {
	return merchantQuote{
		roundId:      roundId,
		goodsId:      goods.GetGoodsId(),
		price:        goods.GetPrice(),
		currencyType: goods.GetCurrencyType(),
		itemConfigId: goods.GetItemConfigId(),
		itemCount:    goods.GetItemCount(),
	}
}

// merchantBuyRequest 按 quote 组一份买 count 份的请求。key 是这次购买的业务幂等键(smokeClientRequestKey):
// 每次新的购买换新键;要重发同一次购买时留住这个请求对象,原样再交给 merchantBuy。
func merchantBuyRequest(quote merchantQuote, count uint32, key uint64) *scene.BuyMysteryMerchantGoodsRequest {
	return &scene.BuyMysteryMerchantGoodsRequest{
		RoundId:              quote.roundId,
		GoodsId:              quote.goodsId,
		Count:                count,
		ExpectedPrice:        quote.price,
		ExpectedCurrencyType: quote.currencyType,
		ExpectedItemConfigId: quote.itemConfigId,
		ExpectedItemCount:    quote.itemCount,
		ClientRequestId:      key,
	}
}

// merchantBuy 发一份购买请求,返回(tip, 应答)。tip == 0 = 成功,包括带原键重发拿回的首次结果(应答标 replayed)。
func (b *channelSmokeBot) merchantBuy(request *scene.BuyMysteryMerchantGoodsRequest) (uint32, *scene.BuyMysteryMerchantGoodsResponse, error) {
	var response scene.BuyMysteryMerchantGoodsResponse
	if err := b.merchantCall(game.SceneMysteryMerchantClientPlayerBuyMysteryMerchantGoodsMessageId, request, &response); err != nil {
		return 0, nil, err
	}
	return response.GetErrorMessage().GetId(), &response, nil
}

// merchantSmokeBalance 读一份背包快照里某种货币的余额。快照里没有这个槽位(货币数组比币种短,或整个 currency 缺失)时
// 第二个返回值为 false。
func merchantSmokeBalance(bag *scene.BagInfo, currencyType uint32) (uint64, bool) {
	values := bag.GetCurrency().GetValues()
	if int(currencyType) >= len(values) {
		return 0, false
	}
	return values[currencyType], true
}

// merchantSmokeItemTotal 汇总一份背包快照里某个 config 的数量(同一种物品可能分几堆)。
func merchantSmokeItemTotal(bag *scene.BagInfo, configId uint32) uint64 {
	var total uint64
	for _, item := range bag.GetItems() {
		if item.GetConfigId() == configId {
			total += uint64(item.GetCount())
		}
	}
	return total
}

// merchantSmokeCheckReplay 核对一次带原键重发的购买应答(docs/design/client-request-idempotency.md §2.2;tip 由调用方先判)。
// last 是重发之前最后一份成功的购买应答,两者之间这个账号不能再有别的扣钱发物。要求:标 replayed;
// 这种货币的余额与这件物品的总数都同 last —— 没有再扣钱、没有再发物;货架上这件看得见时(应答 here 且仍是 roundId
// 这一轮),已购数也同 last,没有再占限购名额。重登之后常常不在商人那条线,那时只核钱和物。
func merchantSmokeCheckReplay(last, replay *scene.BuyMysteryMerchantGoodsResponse, roundId uint64, quote merchantQuote) error {
	if !replay.GetReplayed() {
		return fmt.Errorf("replayed = false: the server ran the resend as a new purchase")
	}
	want, ok := merchantSmokeBalance(last.GetBag(), quote.currencyType)
	if !ok {
		return fmt.Errorf("the last successful reply carries no balance for currency type %d", quote.currencyType)
	}
	got, ok := merchantSmokeBalance(replay.GetBag(), quote.currencyType)
	if !ok {
		return fmt.Errorf("the replayed reply carries no balance for currency type %d", quote.currencyType)
	}
	if got != want {
		return fmt.Errorf("balance of currency type %d went from %d to %d: the resend was charged again", quote.currencyType, want, got)
	}
	if before, now := merchantSmokeItemTotal(last.GetBag(), quote.itemConfigId),
		merchantSmokeItemTotal(replay.GetBag(), quote.itemConfigId); now != before {
		return fmt.Errorf("config_id=%d went from x%d to x%d in the bag: the resend granted the goods again", quote.itemConfigId, before, now)
	}
	info := replay.GetInfo()
	if !info.GetHere() || info.GetRoundId() != roundId {
		return nil
	}
	before, now := merchantSmokeFindGoods(last.GetInfo(), quote.goodsId), merchantSmokeFindGoods(info, quote.goodsId)
	if before == nil || now == nil {
		return fmt.Errorf("goods_id=%d is missing from a shelf (in the last reply: %t, in the replayed reply: %t)",
			quote.goodsId, before != nil, now != nil)
	}
	if now.GetBoughtCount() != before.GetBoughtCount() {
		return fmt.Errorf("goods_id=%d bought_count went from %d to %d: the resend took another slot of the limit",
			quote.goodsId, before.GetBoughtCount(), now.GetBoughtCount())
	}
	return nil
}

func merchantSmokeFindGoods(info *scene.MysteryMerchantInfo, goodsId uint32) *scene.MysteryMerchantGoodsInfo {
	for _, goods := range info.GetGoods() {
		if goods.GetGoodsId() == goodsId {
			return goods
		}
	}
	return nil
}

// merchantSmokeCheckActive 校验「商人在场」的应答形状。
func merchantSmokeCheckActive(info *scene.MysteryMerchantInfo) error {
	switch {
	case info.GetRoundId() == 0:
		return fmt.Errorf("active 的应答 round_id 为 0")
	case info.GetRoundId()*1000 != info.GetAppearAtMs():
		// 契约:轮次号 = 这一轮出现时刻的 Unix 秒(proto/scene/player_mystery_merchant.proto)。
		return fmt.Errorf("round_id=%d 不等于出现时刻的秒数(appear_at_ms=%d)", info.GetRoundId(), info.GetAppearAtMs())
	case info.GetSceneId() == 0 || info.GetSceneConfigId() == 0:
		return fmt.Errorf("active 的应答 scene_id=%d scene_config_id=%d,两者都必须非 0", info.GetSceneId(), info.GetSceneConfigId())
	case info.GetChannelNo() == 0:
		return fmt.Errorf("active 的应答 channel_no 为 0(线号从 1 起)")
	case info.GetPosition() == nil:
		return fmt.Errorf("active 的应答没有 position")
	case info.GetInteractRange() == 0:
		return fmt.Errorf("active 的应答 interact_range 为 0")
	case info.GetServerTimeMs() < info.GetAppearAtMs() || info.GetServerTimeMs() >= info.GetExpireAtMs():
		return fmt.Errorf("active 的应答里 server_time_ms=%d 不在 [appear_at_ms=%d, expire_at_ms=%d) 内",
			info.GetServerTimeMs(), info.GetAppearAtMs(), info.GetExpireAtMs())
	case info.GetNextAppearAtMs() <= info.GetServerTimeMs():
		return fmt.Errorf("next_appear_at_ms=%d 不晚于 server_time_ms=%d", info.GetNextAppearAtMs(), info.GetServerTimeMs())
	}
	return nil
}

// merchantSmokeCheckShelf 校验货架:非空、每件的折后价与折扣自洽、限购合法、同一 goods_id 不重复。
func merchantSmokeCheckShelf(info *scene.MysteryMerchantInfo) error {
	if len(info.GetGoods()) == 0 {
		return fmt.Errorf("here == true 但货架为空(MysteryMerchantGoods 表没导出 / scene 与 scene_manager 的表版本不一致?)")
	}
	seen := map[uint32]bool{}
	for _, goods := range info.GetGoods() {
		id := goods.GetGoodsId()
		if id == 0 || seen[id] {
			return fmt.Errorf("货架上 goods_id=%d 为 0 或重复", id)
		}
		seen[id] = true
		percent := uint64(goods.GetDiscountPercent())
		if percent < 1 || percent > 100 {
			return fmt.Errorf("goods_id=%d 的 discount_percent=%d 不在 1..100", id, percent)
		}
		// 与 cpp mystery_merchant_rules.h DiscountedPrice 同一个公式:向上取整。
		if want := (goods.GetOriginalPrice()*percent + 99) / 100; goods.GetPrice() != want {
			return fmt.Errorf("goods_id=%d 的 price=%d != ceil(original_price=%d × %d%%)=%d",
				id, goods.GetPrice(), goods.GetOriginalPrice(), percent, want)
		}
		if goods.GetLimitCount() == 0 || goods.GetItemCount() == 0 || goods.GetItemConfigId() == 0 {
			return fmt.Errorf("goods_id=%d 的 limit_count=%d item_count=%d item_config_id=%d,都必须非 0",
				id, goods.GetLimitCount(), goods.GetItemCount(), goods.GetItemConfigId())
		}
		if goods.GetBoughtCount() > goods.GetLimitCount() {
			return fmt.Errorf("goods_id=%d 的 bought_count=%d 超过 limit_count=%d", id, goods.GetBoughtCount(), goods.GetLimitCount())
		}
	}
	return nil
}

// RunMerchantSmoke 是 main.go `mode: merchant-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过或因前置条件不满足而跳过则正常返回(退出码 0)。
func RunMerchantSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式(password / satoken)
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	arriveTimeout := time.Duration(cfg.MerchantSmoke.ArriveTimeoutSeconds) * time.Second

	var bot *channelSmokeBot
	cleanup := func() {
		if bot == nil {
			return
		}
		_ = leaveGame(bot.gc, stats)
		gameobject.PlayerList.Delete(bot.gc.PlayerId)
		bot.gc.Close()
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("MERCHANT_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[merchant-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}
	skipAll := func(reason, format string, args ...any) {
		zap.L().Warn(fmt.Sprintf("MERCHANT_SMOKE_SKIP reason=%s %s", reason, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
	}

	// ---- M1:登录进场 ----
	var err error
	bot, err = channelSmokeLogin(cfg, merchantSmokeAccount, stats)
	if err != nil {
		fail("M1-login", "account=%s zone=%d err=%v", merchantSmokeAccount, cfg.ZoneID, err)
	}
	currentScene := bot.player.GetSceneID()
	sceneConfigId := bot.player.GetSceneConfigID()
	if currentScene == 0 || sceneConfigId == 0 {
		fail("M1-login", "After entering scene_id=%d scene_config_id=%d; both must be nonzero", currentScene, sceneConfigId)
	}
	pass("M1-login", "player_id=%d gate=%s scene_config_id=%d scene_id=%d", bot.gc.PlayerId, bot.gate, sceneConfigId, currentScene)

	// ---- M2:查询 ----
	info, err := bot.merchantInfo()
	if err != nil {
		fail("M2-info", "%v", err)
	}
	if !info.GetActive() {
		nextIn := "unknown"
		if info.GetNextAppearAtMs() > info.GetServerTimeMs() {
			nextIn = (time.Duration(info.GetNextAppearAtMs()-info.GetServerTimeMs()) * time.Millisecond).Round(time.Second).String()
		}
		skipAll("merchant_away", "player_id=%d the merchant is not present right now (next appearance: %s). To keep him present, configure scene_manager with "+
			"MysteryMerchant.IntervalSecondsOverride == StaySecondsOverride (see etc/merchant_smoke.yaml); "+
			"next appearance \"unknown\" = scene_manager never scheduled a round (old build / not the leader / the three tables were not exported)",
			bot.gc.PlayerId, nextIn)
		return
	}
	if err := merchantSmokeCheckActive(info); err != nil {
		fail("M2-info", "%v", err)
	}
	pass("M2-info", "round_id=%d scene_config_id=%d channel_no=%d scene_id=%d here=%v position=(%.1f,%.1f) interact_range=%d remaining=%s",
		info.GetRoundId(), info.GetSceneConfigId(), info.GetChannelNo(), info.GetSceneId(), info.GetHere(),
		info.GetPosition().GetX(), info.GetPosition().GetY(), info.GetInteractRange(),
		(time.Duration(info.GetExpireAtMs()-info.GetServerTimeMs()) * time.Millisecond).Round(time.Second))

	// ---- M3:到商人那条线 ----
	if info.GetSceneConfigId() != sceneConfigId {
		skipAll("merchant_on_another_map", "player_id=%d the merchant is on map %d but the robot is on map %d; this smoke test only switches channels within one map",
			bot.gc.PlayerId, info.GetSceneConfigId(), sceneConfigId)
		return
	}
	if info.GetHere() != (info.GetSceneId() == currentScene) {
		fail("M3-here", "here=%v does not match scene_id: merchant scene_id=%d, robot scene_id=%d", info.GetHere(), info.GetSceneId(), currentScene)
	}
	if !info.GetHere() {
		if len(info.GetGoods()) != 0 {
			fail("M3-here", "not on the same channel, yet %d shelf goods were sent", len(info.GetGoods()))
		}
		rejectTip, err := bot.switchTo(sceneConfigId, info.GetSceneId(), arriveTimeout)
		if err != nil {
			fail("M3-switch", "Switching to the merchant channel %d (scene_id=%d) failed: %v", info.GetChannelNo(), info.GetSceneId(), err)
		}
		if rejectTip != 0 {
			fail("M3-switch", "Switching to the merchant channel %d was rejected tip=%d%s (that channel is full / the channel switch cooldown left by the previous run has not expired; rerun in 10 seconds)",
				info.GetChannelNo(), rejectTip, channelSmokeTipHint(rejectTip))
		}
		targetRound := info.GetRoundId()
		if info, err = bot.merchantInfo(); err != nil {
			fail("M3-info", "%v", err)
		}
		if info.GetRoundId() != targetRound {
			fail("M3-info", "A new round started during the channel switch (round_id %d -> %d): a round must last much longer than the smoke run; raise IntervalSecondsOverride and rerun",
				targetRound, info.GetRoundId())
		}
	}
	if !info.GetActive() || !info.GetHere() {
		fail("M3-info", "Arrived on the merchant channel, but the reply says active=%v here=%v", info.GetActive(), info.GetHere())
	}
	if err := merchantSmokeCheckShelf(info); err != nil {
		fail("M3-shelf", "%v", err)
	}
	pass("M3-arrive", "channel_no=%d shelf=%d goods", info.GetChannelNo(), len(info.GetGoods()))

	// 挑一件还有名额的商品(同一轮里重跑时前面的可能已经买满)。
	var target *scene.MysteryMerchantGoodsInfo
	for _, goods := range info.GetGoods() {
		if goods.GetBoughtCount() < goods.GetLimitCount() {
			target = goods
			break
		}
	}
	if target == nil {
		skipAll("all_bought", "player_id=%d round_id=%d this account already bought out all %d shelf goods (second run within the same round); rerun next round",
			bot.gc.PlayerId, info.GetRoundId(), len(info.GetGoods()))
		return
	}
	roundId, goodsId, price := info.GetRoundId(), target.GetGoodsId(), target.GetPrice()
	quote := merchantQuoteOf(roundId, target)

	// ---- M4:走到商人旁边 ----
	// MoveStop 的返回类型是 Empty,没有回包;服务端按导航网格校验「当前位置 → 上报点」直线可达,走不通会停在阻挡点。
	stand := &component.Location{X: info.GetPosition().GetX() + merchantSmokeStandOff, Y: info.GetPosition().GetY(), Z: info.GetPosition().GetZ()}
	if err := bot.send(game.SceneMovementClientPlayerMoveStopMessageId, &scene.MoveStopC2S{
		EndLocation:  stand,
		ClientTimeMs: uint64(time.Now().UnixMilli()),
		InputSeq:     1,
	}); err != nil {
		fail("M4-move", "%v", err)
	}
	time.Sleep(merchantSmokeSettle)
	pass("M4-move", "Reported a point next to the spot (%.1f,%.1f)", stand.GetX(), stand.GetY())

	// ---- M5:负例(都不该动钱、不该占名额)----
	expectReject := func(step string, wantTip uint32, tampered merchantQuote, count uint32) {
		// 每条负例都是一次新的操作,各带一个新键;业务拒绝不记键(client-request-idempotency.md §3.5)
		tip, response, err := bot.merchantBuy(merchantBuyRequest(tampered, count, smokeClientRequestKey()))
		if err != nil {
			fail(step, "%v", err)
		}
		if tip == tipMerchantTooFar {
			fail(step, "Purchase rejected: too far (tip=%d). The robot only walks straight to the spot: make sure scene_manager sets MysteryMerchant.SpotIdOverride "+
				"(pinned to the spot next to the spawn point) and nothing blocks the way between the spot and the spawn point", tip)
		}
		if tip != wantTip {
			fail(step, "expected tip=%d, got tip=%d%s", wantTip, tip, channelSmokeTipHint(tip))
		}
		if response.GetInfo() != nil || response.GetBag() != nil {
			fail(step, "a rejected reply must not carry info / bag")
		}
	}
	expectReject("M5-count-zero", tipMerchantInvalidCount, quote, 0)
	// 报价里任何一项与货架不符都要被拒:轮次、单价、货币、物品、每份数量,逐项各改一处。
	staleRound, wrongPrice, wrongCurrency, wrongItem, wrongItemCount := quote, quote, quote, quote, quote
	staleRound.roundId--
	wrongPrice.price++
	wrongCurrency.currencyType++
	wrongItem.itemConfigId++
	wrongItemCount.itemCount++
	expectReject("M5-stale-round", tipMerchantShelfChanged, staleRound, 1)
	expectReject("M5-wrong-price", tipMerchantShelfChanged, wrongPrice, 1)
	expectReject("M5-wrong-currency", tipMerchantShelfChanged, wrongCurrency, 1)
	expectReject("M5-wrong-item", tipMerchantShelfChanged, wrongItem, 1)
	expectReject("M5-wrong-item-count", tipMerchantShelfChanged, wrongItemCount, 1)
	pass("M5-rejects", "count 0 / stale round / each of the four quote terms (price, currency, item, count per bundle) wrong: all rejected")

	// ---- M6:加钱,买 1 份 ----
	var grant scene.GmAddCurrencyResponse
	if err := bot.merchantCall(game.SceneCurrencyClientPlayerGmAddCurrencyMessageId,
		&scene.GmAddCurrencyRequest{CurrencyType: target.GetCurrencyType(), Amount: merchantSmokeGrantGold}, &grant); err != nil {
		fail("M6-grant", "GM currency grant failed: %v (GM commands are only allowed in dev / test, see cpp/nodes/gate/SECURITY.md)", err)
	}
	if tip := grant.GetErrorMessage().GetId(); tip != 0 {
		fail("M6-grant", "GM currency grant rejected tip=%d%s (scene SCENE_RUN_MODE is not dev / test?)", tip, channelSmokeTipHint(tip))
	}
	balance := grant.GetBalanceAfter()
	if balance < price {
		fail("M6-grant", "balance %d after the grant still cannot cover unit price %d", balance, price)
	}

	// 请求对象留着:replay 与 M9 的 replay-after-relogin 原样再发它(同一个键、其余字段不变)
	buyRequest := merchantBuyRequest(quote, 1, smokeClientRequestKey())
	tip, bought, err := bot.merchantBuy(buyRequest)
	if err != nil {
		fail("M6-buy", "%v", err)
	}
	switch tip {
	case 0:
	case tipMerchantTooFar:
		fail("M6-buy", "Purchase rejected: too far (tip=%d). See the same note in M5: set SpotIdOverride", tip)
	case tipMerchantShelfChanged, tipMerchantNotHere:
		fail("M6-buy", "Purchase rejected tip=%d: hit a round change or the merchant moved channel. A round must last much longer than the smoke run; raise IntervalSecondsOverride and rerun", tip)
	default:
		fail("M6-buy", "Purchase rejected tip=%d%s", tip, channelSmokeTipHint(tip))
	}
	if bought.GetReplayed() {
		fail("M6-buy", "the first purchase with a fresh client_request_id=%d came back replayed = true", buyRequest.GetClientRequestId())
	}
	after := merchantSmokeFindGoods(bought.GetInfo(), goodsId)
	if after == nil {
		fail("M6-buy", "the success reply has no goods_id=%d on the shelf", goodsId)
	}
	if after.GetBoughtCount() != target.GetBoughtCount()+1 {
		fail("M6-buy", "bought_count should go from %d to %d, got %d", target.GetBoughtCount(), target.GetBoughtCount()+1, after.GetBoughtCount())
	}
	got, ok := merchantSmokeBalance(bought.GetBag(), target.GetCurrencyType())
	if !ok {
		fail("M6-buy", "the success reply bag has no balance for currency type %d (values length %d)",
			target.GetCurrencyType(), len(bought.GetBag().GetCurrency().GetValues()))
	}
	if want := balance - price; got != want {
		fail("M6-buy", "balance should be %d (%d - unit price %d), got %d", want, balance, price, got)
	}
	itemTotal := merchantSmokeItemTotal(bought.GetBag(), target.GetItemConfigId())
	if itemTotal < uint64(target.GetItemCount()) {
		fail("M6-buy", "the bag holds config_id=%d x%d, fewer than the %d this bundle grants", target.GetItemConfigId(), itemTotal, target.GetItemCount())
	}
	balance -= price
	pass("M6-buy", "goods_id=%d unit_price=%d discount=%d%% balance=%d item_total_in_bag=%d client_request_id=%d",
		goodsId, price, target.GetDiscountPercent(), balance, itemTotal, buyRequest.GetClientRequestId())

	// ---- 同一个请求原样重发(防重 + 幂等):回首次的结果、标 replayed,不再扣钱、不再发物、不再占名额 ----
	tip, again, err := bot.merchantBuy(buyRequest)
	if err != nil {
		fail("replay", "%v", err)
	}
	if tip != 0 {
		fail("replay", "resending the same purchase (client_request_id=%d) was rejected tip=%d%s; a resend with the original key must get the first result back",
			buyRequest.GetClientRequestId(), tip, channelSmokeTipHint(tip))
	}
	if err := merchantSmokeCheckReplay(bought, again, roundId, quote); err != nil {
		fail("replay", "client_request_id=%d: %v", buyRequest.GetClientRequestId(), err)
	}
	pass("replay", "client_request_id=%d replayed: balance=%d bought_count=%d unchanged", buyRequest.GetClientRequestId(), balance, after.GetBoughtCount())
	// 最后一份成功的购买应答:M9 重登之后的重放拿它对账(M7 补满名额会更新它)
	last := bought

	// ---- M7:买到上限,再买被拒 ----
	if remaining := after.GetLimitCount() - after.GetBoughtCount(); remaining > 0 {
		// 余额只够买两次单价的量时,名额多的便宜商品仍然买得起(加的钱远大于 5 份药的价钱)。
		tip, filled, err := bot.merchantBuy(merchantBuyRequest(quote, remaining, smokeClientRequestKey()))
		if err != nil {
			fail("M7-fill", "%v", err)
		}
		if tip != 0 {
			fail("M7-fill", "buying the remaining %d bundles in one request was rejected tip=%d%s", remaining, tip, channelSmokeTipHint(tip))
		}
		last = filled
	}
	if tip, _, err := bot.merchantBuy(merchantBuyRequest(quote, 1, smokeClientRequestKey())); err != nil {
		fail("M7-limit", "%v", err)
	} else if tip != tipMerchantLimitReached {
		fail("M7-limit", "buying after reaching the limit: expected tip=%d (limit reached), got tip=%d%s", tipMerchantLimitReached, tip, channelSmokeTipHint(tip))
	}
	pass("M7-limit", "goods_id=%d rejected after buying all %d bundles", goodsId, after.GetLimitCount())

	// ---- M8:再查一次,服务端记的已购数 == 限购 ----
	final, err := bot.merchantInfo()
	if err != nil {
		fail("M8-info", "%v", err)
	}
	if final.GetRoundId() == roundId {
		recorded := merchantSmokeFindGoods(final, goodsId)
		if recorded == nil || recorded.GetBoughtCount() != recorded.GetLimitCount() {
			fail("M8-info", "query after buying out: goods_id=%d bought_count / limit_count differ: %v", goodsId, recorded)
		}
	}
	pass("M8-info", "round_id=%d (round at query time %d)", roundId, final.GetRoundId())

	// ---- M9:正常下线、重新登录进场,再原样发一次 M6 的请求 ----
	// 幂等键账本是玩家存档的字段(client-request-idempotency.md §3.1),和它保护的银两、背包、限购同一次存盘:
	// 重登之后同一个键仍要认得出。查键排在购买的一切预检之前,所以重登后不在商人那条线、离商人远、名额已满、
	// 甚至换了轮,结论都应是重放;没认出来的话会被这些预检拒掉(tip 非 0),或者真的又买了一份。
	cleanup()
	bot = nil
	time.Sleep(merchantSmokeReloginWait)
	if bot, err = channelSmokeLogin(cfg, merchantSmokeAccount, stats); err != nil {
		fail("replay-after-relogin", "relogin account=%s: %v", merchantSmokeAccount, err)
	}
	tip, again, err = bot.merchantBuy(buyRequest)
	if err != nil {
		fail("replay-after-relogin", "%v", err)
	}
	if tip != 0 {
		fail("replay-after-relogin", "resending the M6 purchase (client_request_id=%d) after relogin was rejected tip=%d%s; "+
			"the ledger did not survive logout, or the save did not land before relogin",
			buyRequest.GetClientRequestId(), tip, channelSmokeTipHint(tip))
	}
	if err := merchantSmokeCheckReplay(last, again, roundId, quote); err != nil {
		fail("replay-after-relogin", "client_request_id=%d: %v", buyRequest.GetClientRequestId(), err)
	}
	pass("replay-after-relogin", "player_id=%d client_request_id=%d replayed after relogin: here=%v round_id=%d, no second charge or grant",
		bot.gc.PlayerId, buyRequest.GetClientRequestId(), again.GetInfo().GetHere(), again.GetInfo().GetRoundId())

	zap.L().Info(fmt.Sprintf("MERCHANT_SMOKE_OK player_id=%d zone=%d round_id=%d scene_config_id=%d channel_no=%d goods_id=%d "+
		"price=%d discount_percent=%d limit_count=%d",
		bot.gc.PlayerId, cfg.ZoneID, roundId, info.GetSceneConfigId(), info.GetChannelNo(), goodsId, price,
		target.GetDiscountPercent(), target.GetLimitCount()))
	cleanup()
	_ = zap.L().Sync()
}

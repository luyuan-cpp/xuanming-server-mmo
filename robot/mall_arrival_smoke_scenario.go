package main

// mall-arrival-smoke 场景:单机器人对「官方商品到账」做端到端冒烟(D-17 首批官方 SKU 的真实 Go↔Java↔C++ 闭环)。
//
// 调用链:robot →(客户端协议)gate → client_rpc_router → trade ClientPlayerJubaozhai → java/mall_game 下单;
// 付款由本进程请求商城本地mock适配器，金额取原订单，提供方原件先独立提交；
// 之后 trade 每秒 poll 商城的待发货意图 → 资产通道 AssetCredit 到 C++ scene → 等玩家 SQL 提交证据 →
// 回执商城 DELIVERED。本场景只从玩家能看到的两处取证:订单状态(GetCommerceOrder)与背包(GetBag)。
//
//	A1 登录进场;
//	A2 清理遗留:翻完本账号在商城的订单,同一 SKU 还没付款的单逐个取消(商城每人最多 5 张未付单);
//	   已付款还在发货中的单等它发完再继续,否则它的到账会混进本轮的背包增量;
//	A3 记下背包里 item_config_id 的数量;
//	A4 ListOfficialSkus 查到 sku_id:标题非空、单价 > 0、可售 ≥ 份数;
//	A5 CreateOfficialOrder:待付款、官方单、买家是自己、金额 = 单价 × 份数、币种与商品一致;
//	   同一 client_request_id 再发一次,必须拿回同一张单(下单幂等);
//	A6 GetCommerceOrder:仍是待付款;
//	A7 带金额覆盖的mock命令必须400且原单仍待付款；随后仅原订单+稳定请求号付款；
//	   同两字段原样重发200，核原买家/金额/币种/SKU/份数，状态待发货或已发货；
//	A8 轮询 GetCommerceOrder 直到 DELIVERED(退款中 / 人工 / 已取消都是失败,原因写进 FAIL);
//	A9 背包里的数量正好 = A3 + count_per_unit × quantity;
//	A10 再等三拍(trade 每秒 poll 一次):背包数量不变、订单仍是已发货 —— 没有重复发货;
//	A11 下线重登:背包数量仍然是 A9 的值(已发货回执要求玩家 SQL 已提交,重登读的是落盘后的档)。
//
// 结果约定(供外层脚本消费):
//
//	全过 → `MALL_ARRIVAL_SMOKE_OK player_id=… order_id=… sku_id=… …`,退出码 0;
//	任一步失败 → `MALL_ARRIVAL_SMOKE_FAIL step=… reason=…`,退出码 1。每步通过另打一行 `[mall-arrival-smoke] PASS step=…`。
//
// 前置条件(商城 / trade / go/db 的配置与启动顺序)写在 etc/mall_arrival_smoke.yaml 文件头。
// 会话与读背包复用 equip-smoke 的 equipSmokeSession;聚宝斋请求走 smokeCaller(按请求号认领回包)。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	tradepb "proto/trade"
	"robot/config"
	"robot/generated/pb/game"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧 DevPasswordAuth。99xx 里 9901-9906 / 9911-9912 已占,
// 9921-9922 是摆摊冒烟的摊主 / 买家(docs/design/stall-system.md),9931-9934 留给人民币寄售冒烟,本场景用 9925。
// 账号每跑一遍多 count_per_unit × quantity 件物品。
const mallArrivalSmokeAccount = "robot_9925"

const (
	// 单次聚宝斋 RPC 的等待预算:trade → 商城是一次本机 HTTP(客户端超时 2.5s),给 10s 覆盖首次路由建立。
	mallArrivalRpcTimeout = 10 * time.Second
	// A2 翻订单的页数上限:每页 100 张,本账号每跑一遍只多一张。超过就是数据异常,不无界翻。
	mallArrivalMaxOrderPages = 20
	// A9 读背包的上限:已发货回执晚于 scene 入包,正常第一次读就到;留几秒给 GetBag 自身的路由。
	mallArrivalBagWait = 10 * time.Second
	// A10 观察的拍数与间隔:trade 每秒 poll 一次商城,三拍内会重复发就一定看得到。
	mallArrivalSettleTicks = 3
	mallArrivalSettleGap   = 1100 * time.Millisecond
	// A11 下线后等异步存盘的时间(同 equipSmokeSession.relogin)。
	mallArrivalReloginGap = 2 * time.Second
)

var errMallArrivalTimeout = errors.New("mall-arrival-smoke 等回包超时")

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipMallFeatureDisabled = uint32(tiptable.TradeError_kTradeFeatureDisabled)
	tipMallOrderNotFound   = uint32(tiptable.TradeError_kTradeOrderNotFound)
	tipMallStateConflict   = uint32(tiptable.TradeError_kTradeOrderStateConflict)
	tipMallRequestConflict = uint32(tiptable.TradeError_kTradeRequestConflict)
	tipMallInvalid         = uint32(tiptable.CommonError_kInvalidParameter)
	tipMallUnavailable     = uint32(tiptable.CommonError_kServiceUnavailable)
	tipMallRateLimited     = uint32(tiptable.CommonError_kRateLimitExceeded)
)

// mallArrivalTipHint 给常见拒绝码配一句排障提示。
func mallArrivalTipHint(tip uint32) string {
	switch tip {
	case tipMallFeatureDisabled:
		return "(kTradeFeatureDisabled:trade 没装配商城 —— AssetOp.Enabled / Mall.URL 两项缺一,或商城密钥环境变量没注入)"
	case tipMallUnavailable:
		return "(kServiceUnavailable:trade 连不上商城,或商城回了 trade 未映射的状态码,例如未付单超过上限的 429;看 trade 日志里的「商城请求失败 HTTP …」)"
	case tipMallOrderNotFound:
		return "(kTradeOrderNotFound:商城 404,SKU 或订单不存在 —— 商城库没跑 etc/mall_arrival_smoke_seed.sql?)"
	case tipMallStateConflict:
		return "(kTradeOrderStateConflict:商城 409,商品未上架 / 库存不足 / 订单状态不允许)"
	case tipMallRequestConflict:
		return "(kTradeRequestConflict:同一请求号换了参数)"
	case tipMallInvalid:
		return "(kInvalidParameter:trade 或商城拒绝了参数 / 商城应答形状不对)"
	case tipMallRateLimited:
		return "(kRateLimitExceeded:gate 限流,聚宝斋商城几个消息号还没进 MessageLimiter 表,吃默认 3 次/秒)"
	}
	return ""
}

// mallArrivalRequestID 生成本轮的下单请求号:trade 要求 8..80 字节,商城要求 [A-Za-z0-9_-]{8,128}。
// 毫秒时刻 + 随机后缀:同账号多次运行不撞号(撞号会被商城当成同一张单)。
func mallArrivalRequestID(nowMs int64, randomHex string) string {
	return "mallarrival-" + strconv.FormatInt(nowMs, 10) + "-" + randomHex
}

// 同原订单在未知提交、重登或驱动重启后仍使用同一mock请求号。
func mallArrivalPaymentRequestID(orderID string) string {
	return "mallarrival-pay-" + strings.ReplaceAll(orderID, "-", "")
}

// mallArrivalCheckCreated 核对下单应答:只看玩家能看到、且服务端承诺的字段。
func mallArrivalCheckCreated(order *tradepb.CommerceOrderView, playerID uint64, sku *tradepb.OfficialSkuView, quantity uint32) error {
	switch {
	case order == nil:
		return fmt.Errorf("应答没有 order")
	case !commerceOrderIDPattern.MatchString(order.GetOrderId()):
		return fmt.Errorf("order_id=%q 不是商城的小写 UUID", order.GetOrderId())
	case order.GetKind() != tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_OFFICIAL:
		return fmt.Errorf("kind=%v,期望官方单", order.GetKind())
	case order.GetState() != tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT:
		return fmt.Errorf("state=%v,期望待付款", order.GetState())
	case order.GetBuyerPlayerId() != playerID:
		return fmt.Errorf("buyer_player_id=%d,不是本角色 %d", order.GetBuyerPlayerId(), playerID)
	case order.GetAmount() != sku.GetPriceMinor()*uint64(quantity):
		return fmt.Errorf("amount=%d != 单价 %d × 份数 %d", order.GetAmount(), sku.GetPriceMinor(), quantity)
	case order.GetCurrency() != sku.GetCurrency():
		return fmt.Errorf("currency=%q 与商品的 %q 不一致", order.GetCurrency(), sku.GetCurrency())
	case order.GetRelatedId() != strconv.FormatUint(sku.GetSkuId(), 10):
		return fmt.Errorf("related_id=%q 不是 sku_id %d", order.GetRelatedId(), sku.GetSkuId())
	}
	return nil
}

// mallArrivalDeliveryVerdict 判断付款之后的订单状态:done = 已发货;err != nil = 不会再变好的终态或不该出现的状态。
func mallArrivalDeliveryVerdict(state tradepb.CommerceOrderState) (done bool, err error) {
	switch state {
	case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED:
		return true, nil
	case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERING:
		return false, nil
	case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_REFUND_PENDING:
		return false, fmt.Errorf("订单转入退款中:trade 的发货腿被 scene 拒绝(背包满 / 物品配置不存在 / 冻结),看 trade 日志与 trade_asset_op 表")
	case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_MANUAL:
		return false, fmt.Errorf("订单转入人工处理:发货结果未知且无法自动收敛")
	case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_CANCELLED, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_REFUNDED:
		return false, fmt.Errorf("已付款的订单变成了 %v", state)
	case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT:
		return false, fmt.Errorf("商城已记账付款,订单却仍显示待付款")
	}
	return false, fmt.Errorf("官方单不该出现的状态 %v", state)
}

// mallArrivalLeftovers 从订单列表里挑出本 SKU 遗留的官方单:还没付款的(要取消)与已付款还在发货的(要等)。
// 只认商城订单号(UUID);trade 自己的合同记录(数字号)不归本场景管。
func mallArrivalLeftovers(orders []*tradepb.CommerceOrderView, skuID uint64) (unpaid, delivering []*tradepb.CommerceOrderView) {
	related := strconv.FormatUint(skuID, 10)
	for _, order := range orders {
		if order.GetKind() != tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_OFFICIAL || order.GetRelatedId() != related ||
			!commerceOrderIDPattern.MatchString(order.GetOrderId()) {
			continue
		}
		switch order.GetState() {
		case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT:
			unpaid = append(unpaid, order)
		case tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERING:
			delivering = append(delivering, order)
		}
	}
	return unpaid, delivering
}

// mallArrivalBot 是一个已登录进场的机器人:session 负责登录 / 读背包 / 下线,caller 负责聚宝斋请求。
type mallArrivalBot struct {
	session *equipSmokeSession
	caller  *smokeCaller
}

func openMallArrivalBot(cfg *config.Config, stats *metrics.Stats) (*mallArrivalBot, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("bad gate port %q: %w", portStr, err)
	}
	gc, player, err := prepareBehaviorClient(host, port, mallArrivalSmokeAccount, cfg.Password, stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("account=%s: %w", mallArrivalSmokeAccount, err)
	}
	session := &equipSmokeSession{gc: gc, player: player, stats: stats, lastSent: make(map[uint32]time.Time)}
	return &mallArrivalBot{
		session: session,
		caller:  newSmokeCaller(gc, stats, mallArrivalSmokeAccount, mallArrivalRpcTimeout, errMallArrivalTimeout),
	}, nil
}

func (b *mallArrivalBot) close() {
	if b != nil {
		b.session.close()
	}
}

// jubaozhai 发一个聚宝斋请求并等它自己的回包。同号请求按 equipSmokeSameIdGap 节流(这几个消息号还没进
// MessageLimiter 表,吃 gate 默认 3 次/秒)。gate / 路由服的信封拒绝一律 error:请求没到 trade。
func (b *mallArrivalBot) jubaozhai(messageId uint32, request, response proto.Message) error {
	b.session.throttle(messageId)
	envelopeTip, err := b.caller.call(messageId, request, response)
	if err != nil {
		return err
	}
	if envelopeTip != 0 {
		return fmt.Errorf("message_id=%d 被 gate / 路由服信封拒绝 tip=%d%s:请求没到 trade(路由服模式 GATE_CLIENT_RPC_ROUTER=1?gate 是用新消息号重编的?)",
			messageId, envelopeTip, mallArrivalTipHint(envelopeTip))
	}
	return nil
}

// order 查一张单;业务拒绝(应答体 tip)也是 error。
func (b *mallArrivalBot) order(orderID string) (*tradepb.CommerceOrderView, error) {
	var response tradepb.CommerceOrderResponse
	if err := b.jubaozhai(game.ClientPlayerJubaozhaiGetCommerceOrderMessageId, &tradepb.GetCommerceOrderRequest{OrderId: orderID}, &response); err != nil {
		return nil, err
	}
	if tip := response.GetErrorMessage().GetId(); tip != 0 {
		return nil, fmt.Errorf("GetCommerceOrder(%s) 被拒 tip=%d%s", orderID, tip, mallArrivalTipHint(tip))
	}
	if response.GetOrder().GetOrderId() != orderID {
		return nil, fmt.Errorf("GetCommerceOrder(%s) 回的是另一张单 %q", orderID, response.GetOrder().GetOrderId())
	}
	return response.GetOrder(), nil
}

// allOrders 翻完本账号的订单列表(trade 先翻商城的单,翻完再接 trade 自己的合同记录;两者都收,由调用方挑)。
func (b *mallArrivalBot) allOrders() ([]*tradepb.CommerceOrderView, error) {
	var all []*tradepb.CommerceOrderView
	cursor := ""
	for page := 0; page < mallArrivalMaxOrderPages; page++ {
		var response tradepb.ListCommerceOrdersResponse
		if err := b.jubaozhai(game.ClientPlayerJubaozhaiListCommerceOrdersMessageId,
			&tradepb.ListCommerceOrdersRequest{Cursor: cursor, Limit: 100}, &response); err != nil {
			return nil, err
		}
		if tip := response.GetErrorMessage().GetId(); tip != 0 {
			return nil, fmt.Errorf("ListCommerceOrders 被拒 tip=%d%s", tip, mallArrivalTipHint(tip))
		}
		all = append(all, response.GetOrders()...)
		// 只有商城分页(mall: 前缀)才继续翻;商城翻完时 trade 已经把它自己的记录接在同一页里返回。
		if len(response.GetOrders()) == 0 || !strings.HasPrefix(response.GetNextCursor(), "mall:") {
			return all, nil
		}
		cursor = response.GetNextCursor()
	}
	return nil, fmt.Errorf("订单超过 %d 页仍没翻完:本账号订单数异常,换账号或清商城库", mallArrivalMaxOrderPages)
}

// awaitDelivered 轮询到已发货;返回最后看到的状态与耗时。
func (b *mallArrivalBot) awaitDelivered(orderID string, timeout, interval time.Duration, onChange func(state tradepb.CommerceOrderState, elapsed time.Duration)) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	last := tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_UNSPECIFIED
	for {
		order, err := b.order(orderID)
		if err != nil {
			return time.Since(start), err
		}
		if state := order.GetState(); state != last {
			last = state
			if onChange != nil {
				onChange(state, time.Since(start))
			}
		}
		done, err := mallArrivalDeliveryVerdict(last)
		if err != nil {
			return time.Since(start), err
		}
		if done {
			return time.Since(start), nil
		}
		if !time.Now().Add(interval).Before(deadline) {
			return time.Since(start), fmt.Errorf("%s 内没有发货,最后状态 %v(trade 没在 poll 商城?trade 日志「商城交付恢复失败」;"+
				"或 go/db 没切 Fencing.Mode=dual,资产腿拿不到 SQL 提交证据而一直待定)", timeout, last)
		}
		time.Sleep(interval)
	}
}

func (b *mallArrivalBot) inventoryCount(configID uint32) (uint64, error) {
	bag, err := b.session.getBag(equipSmokeBagInventory)
	if err != nil {
		return 0, err
	}
	return equipForgeConfigCount(bag, configID), nil
}

// RunMallArrivalSmoke 是 main.go `mode: mall-arrival-smoke` 的入口。
// 任一步失败直接以退出码 1 结束进程;全部通过则正常返回(退出码 0)。
func RunMallArrivalSmoke(cfg *config.Config) {
	// loginAndEnterScenario 从这个包级变量读认证方式
	loginTestCfg = cfg
	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	sc := cfg.MallArrivalSmoke
	deliverTimeout := time.Duration(sc.DeliverTimeoutSeconds) * time.Second
	pollInterval := time.Duration(sc.PollIntervalMs) * time.Millisecond
	expectedGrant := uint64(sc.CountPerUnit) * uint64(sc.Quantity)

	var bot *mallArrivalBot
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("MALL_ARRIVAL_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		bot.close()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	pass := func(step, format string, args ...any) {
		zap.L().Info(fmt.Sprintf("[mall-arrival-smoke] PASS step=%s %s", step, fmt.Sprintf(format, args...)))
	}

	// ---- A0:付款器(在任何网络操作之前;密钥只从环境变量读)----
	payer, err := newCommerceMockPayer(sc.MallURL, sc.PaymentSubject, os.Getenv(commercePaymentSecretEnv))
	if err != nil {
		fail("A0-payer", "%v", err)
	}

	// ---- A1:登录进场 ----
	if bot, err = openMallArrivalBot(cfg, stats); err != nil {
		fail("A1-login", "zone=%d err=%v", cfg.ZoneID, err)
	}
	playerID := bot.session.gc.PlayerId
	pass("A1-login", "account=%s player_id=%d", mallArrivalSmokeAccount, playerID)

	// ---- A2:清理遗留 ----
	orders, err := bot.allOrders()
	if err != nil {
		fail("A2-leftovers", "%v", err)
	}
	unpaid, delivering := mallArrivalLeftovers(orders, sc.SkuID)
	for _, leftover := range unpaid {
		var response tradepb.CommerceOrderResponse
		if err := bot.jubaozhai(game.ClientPlayerJubaozhaiCancelCommerceOrderMessageId,
			&tradepb.CancelCommerceOrderRequest{OrderId: leftover.GetOrderId()}, &response); err != nil {
			fail("A2-leftovers", "取消遗留未付单 %s: %v", leftover.GetOrderId(), err)
		}
		if tip := response.GetErrorMessage().GetId(); tip != 0 ||
			response.GetOrder().GetState() != tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_CANCELLED {
			fail("A2-leftovers", "取消遗留未付单 %s: tip=%d%s state=%v", leftover.GetOrderId(), tip, mallArrivalTipHint(tip), response.GetOrder().GetState())
		}
	}
	for _, leftover := range delivering {
		if _, err := bot.awaitDelivered(leftover.GetOrderId(), deliverTimeout, pollInterval, nil); err != nil {
			fail("A2-leftovers", "上一轮已付款的单 %s 没有发完: %v", leftover.GetOrderId(), err)
		}
	}
	pass("A2-leftovers", "orders=%d cancelled_unpaid=%d awaited_delivering=%d", len(orders), len(unpaid), len(delivering))

	// ---- A3:基线 ----
	before, err := bot.inventoryCount(sc.ItemConfigID)
	if err != nil {
		fail("A3-baseline", "%v", err)
	}
	pass("A3-baseline", "config_id=%d count=%d", sc.ItemConfigID, before)

	// ---- A4:查商品 ----
	var skus tradepb.ListOfficialSkusResponse
	if err := bot.jubaozhai(game.ClientPlayerJubaozhaiListOfficialSkusMessageId,
		&tradepb.ListOfficialSkusRequest{AfterSkuId: sc.SkuID - 1, Limit: 1}, &skus); err != nil {
		fail("A4-sku", "%v", err)
	}
	if tip := skus.GetErrorMessage().GetId(); tip != 0 {
		fail("A4-sku", "ListOfficialSkus 被拒 tip=%d%s", tip, mallArrivalTipHint(tip))
	}
	var sku *tradepb.OfficialSkuView
	for _, candidate := range skus.GetSkus() {
		if candidate.GetSkuId() == sc.SkuID {
			sku = candidate
		}
	}
	switch {
	case sku == nil:
		fail("A4-sku", "商城没有在售的 sku_id=%d(没跑 etc/mall_arrival_smoke_seed.sql / 已下架 / 库存卖光)", sc.SkuID)
	case sku.GetTitle() == "" || sku.GetPriceMinor() == 0 || sku.GetCurrency() == "":
		fail("A4-sku", "商品视图不完整: %v", sku)
	case sku.GetAvailable() < uint64(sc.Quantity):
		fail("A4-sku", "可售 %d < 份数 %d:按种子脚本补库存", sku.GetAvailable(), sc.Quantity)
	}
	pass("A4-sku", "sku_id=%d title=%q price_minor=%d currency=%s available=%d", sku.GetSkuId(), sku.GetTitle(), sku.GetPriceMinor(), sku.GetCurrency(), sku.GetAvailable())

	// ---- A5:下单 + 同号重发 ----
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		fail("A5-create", "生成请求号随机后缀失败: %v", err)
	}
	createRequest := &tradepb.CreateOfficialOrderRequest{
		ClientRequestId: mallArrivalRequestID(time.Now().UnixMilli(), hex.EncodeToString(suffix)),
		SkuId:           sc.SkuID,
		Quantity:        sc.Quantity,
	}
	create := func() *tradepb.CommerceOrderView {
		var response tradepb.CommerceOrderResponse
		if err := bot.jubaozhai(game.ClientPlayerJubaozhaiCreateOfficialOrderMessageId, createRequest, &response); err != nil {
			fail("A5-create", "%v", err)
		}
		if tip := response.GetErrorMessage().GetId(); tip != 0 {
			fail("A5-create", "CreateOfficialOrder 被拒 tip=%d%s", tip, mallArrivalTipHint(tip))
		}
		if err := mallArrivalCheckCreated(response.GetOrder(), playerID, sku, sc.Quantity); err != nil {
			fail("A5-create", "%v", err)
		}
		return response.GetOrder()
	}
	created := create()
	orderID := created.GetOrderId()
	if replay := create(); replay.GetOrderId() != orderID {
		fail("A5-create", "同一 client_request_id 重发得到另一张单 %s(第一次是 %s):下单不幂等", replay.GetOrderId(), orderID)
	}
	pass("A5-create", "order_id=%s amount=%d %s request_id=%s(同号重发拿回同一张单)", orderID, created.GetAmount(), created.GetCurrency(), createRequest.GetClientRequestId())

	// ---- A6:查单 ----
	if order, err := bot.order(orderID); err != nil {
		fail("A6-get", "%v", err)
	} else if order.GetState() != tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT || order.GetAmount() != created.GetAmount() {
		fail("A6-get", "state=%v amount=%d,期望待付款、amount=%d", order.GetState(), order.GetAmount(), created.GetAmount())
	}
	pass("A6-get", "order_id=%s 待付款", orderID)

	// ---- A7:拒绝金额覆盖，再按原订单付款并原样重放 ----
	ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
	defer cancel()
	payment := commerceMockPay{OrderID: orderID, RequestID: mallArrivalPaymentRequestID(orderID)}
	var rejected *commerceHTTPError
	if err := payer.ProbeAmountOverride(ctx, payment, created.GetAmount()+1); !errors.As(err, &rejected) || rejected.Status != 400 {
		fail("A7-pay-wrong-amount", "mock付款不得接受金额覆盖，应当400拒绝，实际err=%v", err)
	}
	if order, err := bot.order(orderID); err != nil {
		fail("A7-pay-wrong-amount", "%v", err)
	} else if order.GetState() != tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT || order.GetAmount() != created.GetAmount() || order.GetBuyerPlayerId() != playerID {
		fail("A7-pay-wrong-amount", "被拒的付款改变了订单状态: %v", order.GetState())
	}
	pass("A7-pay-wrong-amount", "商城400「%s」，原买家/金额不变且仍待付款", rejected.Message)

	paid, err := payer.RecordPayment(ctx, payment)
	if err != nil {
		fail("A7-pay", "%v(付款器的商城地址 / subject / 密钥要与商城 COMMERCE_* 环境变量一致;401 = 签名或密钥不符)", err)
	}
	if (paid.State != "DELIVERY_PENDING" && paid.State != "DELIVERED") || paid.BuyerID != strconv.FormatUint(playerID, 10) || paid.AmountMinor != created.GetAmount() || paid.Currency != created.GetCurrency() || paid.SkuID != sc.SkuID || paid.Quantity != sc.Quantity {
		fail("A7-pay", "商城付款应答 state=%s buyer=%s amount=%d,期望 DELIVERY_PENDING / %d / %d",
			paid.State, paid.BuyerID, paid.AmountMinor, playerID, created.GetAmount())
	}
	paidAt := time.Now()
	replayed, err := payer.RecordPayment(ctx, payment)
	if err != nil {
		fail("A7-pay-replay", "同一笔付款原样重发应当幂等 200,实际 %v", err)
	}
	if replayed.State != "DELIVERY_PENDING" && replayed.State != "DELIVERED" {
		fail("A7-pay-replay", "重发后商城状态 %s,期望待发货或已发货", replayed.State)
	}
	if replayed.BuyerID != paid.BuyerID || replayed.AmountMinor != paid.AmountMinor || replayed.Currency != paid.Currency || replayed.SkuID != paid.SkuID || replayed.Quantity != paid.Quantity {
		fail("A7-pay-replay", "重放改变了原订单买家、金额、币种、SKU或份数")
	}
	pass("A7-pay", "本地mock request_id=%s；原样重发幂等(state=%s)", payment.RequestID, replayed.State)

	// ---- A8:等已发货 ----
	waited, err := bot.awaitDelivered(orderID, deliverTimeout, pollInterval, func(state tradepb.CommerceOrderState, elapsed time.Duration) {
		zap.L().Info(fmt.Sprintf("[mall-arrival-smoke] order_id=%s state=%v after=%s", orderID, state, elapsed.Round(time.Millisecond)))
	})
	if err != nil {
		fail("A8-delivered", "%v", err)
	}
	pass("A8-delivered", "order_id=%s 已发货,付款后 %s", orderID, time.Since(paidAt).Round(time.Millisecond))

	// ---- A9:背包增量 ----
	want := before + expectedGrant
	var after uint64
	bagDeadline := time.Now().Add(mallArrivalBagWait)
	for {
		if after, err = bot.inventoryCount(sc.ItemConfigID); err != nil {
			fail("A9-bag", "%v", err)
		}
		if after >= want || !time.Now().Before(bagDeadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if after != want {
		fail("A9-bag", "config_id=%d 数量 %d → %d,期望正好 +%d(count_per_unit=%d × quantity=%d)。少了 = 已发货却没进包;"+
			"多了 = 重复发货,或 yaml 的 item_config_id / count_per_unit 与种子脚本不一致",
			sc.ItemConfigID, before, after, expectedGrant, sc.CountPerUnit, sc.Quantity)
	}
	pass("A9-bag", "config_id=%d %d → %d", sc.ItemConfigID, before, after)

	// ---- A10:不重复发货 ----
	for tick := 0; tick < mallArrivalSettleTicks; tick++ {
		time.Sleep(mallArrivalSettleGap)
	}
	if again, err := bot.inventoryCount(sc.ItemConfigID); err != nil {
		fail("A10-no-double", "%v", err)
	} else if again != want {
		fail("A10-no-double", "已发货之后数量又从 %d 变成 %d:重复发货", want, again)
	}
	if order, err := bot.order(orderID); err != nil {
		fail("A10-no-double", "%v", err)
	} else if order.GetState() != tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED {
		fail("A10-no-double", "已发货的订单又变成了 %v", order.GetState())
	}
	pass("A10-no-double", "再等 %d 拍:数量仍是 %d,订单仍已发货", mallArrivalSettleTicks, want)

	// ---- A11:重登后仍在 ----
	bot.close()
	time.Sleep(mallArrivalReloginGap)
	if bot, err = openMallArrivalBot(cfg, stats); err != nil {
		fail("A11-relogin", "%v", err)
	}
	if bot.session.gc.PlayerId != playerID {
		fail("A11-relogin", "重登进的是另一个角色 %d(原 %d)", bot.session.gc.PlayerId, playerID)
	}
	if persisted, err := bot.inventoryCount(sc.ItemConfigID); err != nil {
		fail("A11-relogin", "%v", err)
	} else if persisted != want {
		fail("A11-relogin", "重登后数量 %d,期望 %d:已发货回执之前玩家档并没有真正落盘", persisted, want)
	}
	pass("A11-relogin", "重登后 config_id=%d 数量仍是 %d", sc.ItemConfigID, want)

	zap.L().Info(fmt.Sprintf("MALL_ARRIVAL_SMOKE_OK player_id=%d zone=%d order_id=%s sku_id=%d quantity=%d amount=%d currency=%s "+
		"item_config_id=%d count_before=%d count_after=%d deliver_wait_ms=%d",
		playerID, cfg.ZoneID, orderID, sc.SkuID, sc.Quantity, created.GetAmount(), created.GetCurrency(),
		sc.ItemConfigID, before, want, waited.Milliseconds()))
	bot.close()
	_ = zap.L().Sync()
}

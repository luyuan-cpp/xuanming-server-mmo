//go:build commerceacceptance

package main

// 本文件只在显式 commerceacceptance tag 下编译。所有经济写操作均走正式 Gate 客户端
// RPC；HTTP 只读付款方订单视图或调用本地 mock 付款/退款对账/出款入口，绝不提交资产回执。
// 外层仍须按专用 GoList 闭包、当前包与原句柄监管计划执行；默认单测不会运行这些场景。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"proto/scene"
	tradepb "proto/trade"
	"robot/config"
	"robot/generated/pb/game"
	"robot/metrics"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type currentCommercePin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type currentCommerceInput struct {
	Schema             string             `json:"schema"`
	RunID              string             `json:"run_id"`
	ConfigFile         currentCommercePin `json:"config_file"`
	EvidenceParent     string             `json:"evidence_parent"`
	MallURL            string             `json:"mall_url"`
	PaymentSubject     string             `json:"payment_subject"`
	Zone               uint32             `json:"zone"`
	Accounts           []string           `json:"accounts"` // 第0名卖家，第1..25名买家；全部须在归属收据中明确列出。
	AccountZones       []uint32           `json:"account_zones"`
	SKU                uint64             `json:"sku"`
	ItemConfigID       uint32             `json:"item_config_id"`
	CountPerUnit       uint64             `json:"count_per_unit"`
	UnitPriceMinor     uint64             `json:"unit_price_minor"`
	OfflineHoldSeconds int                `json:"offline_hold_seconds"`
	MallJar            currentCommercePin `json:"mall_jar"`
	GatewayJar         currentCommercePin `json:"gateway_jar"`
	OwnedStackReceipt  currentCommercePin `json:"owned_stack_receipt"`
}

type currentCommerceStack struct {
	Schema                string               `json:"schema"`
	RootReviewed          bool                 `json:"root_reviewed"`
	LiveOwnedProcesses    bool                 `json:"live_owned_processes"`
	StrongTiDB            bool                 `json:"strong_tidb"`
	AssetSQLCommit        bool                 `json:"asset_sql_commit"`
	OriginalSessionActors bool                 `json:"original_session_actors"`
	RealChannelsDisabled  bool                 `json:"real_channels_disabled"`
	GlobalMarket          bool                 `json:"global_market"`
	Zone                  uint32               `json:"zone"`
	Zones                 []uint32             `json:"zones"`
	Accounts              []string             `json:"accounts"`
	AccountZones          []uint32             `json:"account_zones"`
	MallJarSHA256         string               `json:"mall_jar_sha256"`
	GatewayJarSHA256      string               `json:"gateway_jar_sha256"`
	NoticeMS              uint64               `json:"notice_ms"`
	CoolingMS             uint64               `json:"cooling_ms"`
	FeeBPS                uint32               `json:"fee_bps"`
	RuntimeProofs         []currentCommercePin `json:"runtime_proofs"`
}

type currentCommerceRun struct {
	t        *testing.T
	in       currentCommerceInput
	cfg      *config.Config
	payer    *commerceMockPayer
	dir      string
	sequence int
	bots     map[string]*mallArrivalBot
	heads    []currentCommerceOfflineOrder
}

type currentCommerceOfflineOrder struct {
	Account string
	Before  uint64
	Order   *tradepb.CommerceOrderView
}

func currentCommerceRead(t *testing.T, pin currentCommercePin, target any) {
	t.Helper()
	require.True(t, filepath.IsAbs(pin.Path), "只接受已归属原件的绝对路径")
	require.Regexp(t, `^[A-Fa-f0-9]{64}$`, pin.SHA256)
	raw, err := os.ReadFile(pin.Path)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, strings.ToLower(pin.SHA256), hex.EncodeToString(sum[:]), "原件SHA不符")
	if target != nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		require.NoError(t, decoder.Decode(target))
		require.ErrorIs(t, decoder.Decode(new(any)), io.EOF, "原件不能带尾随JSON")
	}
}

func TestCommerceCurrentRealStack(t *testing.T) {
	if os.Getenv("COMMERCE_ACCEPTANCE_REQUIRE") != "1" {
		t.Skip("只供root显式准入的真实Gate/Trade/Scene与TiDB验收；未运行不代表通过")
	}
	var input currentCommerceInput
	currentCommerceRead(t, currentCommercePin{os.Getenv("COMMERCE_ACCEPTANCE_INPUT"), os.Getenv("COMMERCE_ACCEPTANCE_INPUT_SHA256")}, &input)
	require.Equal(t, "commerce-current-real-stack-input-v1", input.Schema)
	require.Regexp(t, `^[0-9a-f]{32}$`, input.RunID)
	require.Len(t, input.Accounts, 26)
	require.Len(t, input.AccountZones, 26)
	require.NotZero(t, input.Zone)
	require.NotZero(t, input.SKU)
	require.NotZero(t, input.ItemConfigID)
	require.Equal(t, uint64(2), input.CountPerUnit, "本验收SKU固定每份2件，卖家准备和尾页净增按原条款核对")
	require.Equal(t, uint64(600), input.UnitPriceMinor, "须与本轮admin正式创建的原SKU条款一致")
	require.GreaterOrEqual(t, input.OfflineHoldSeconds, 30)
	require.LessOrEqual(t, input.OfflineHoldSeconds, 120)
	seen := map[string]bool{}
	for _, account := range input.Accounts {
		require.Regexp(t, `^robot_[A-Za-z0-9_]{1,40}$`, account)
		require.False(t, seen[account], "验收账号不能复用")
		seen[account] = true
	}
	var owned currentCommerceStack
	currentCommerceRead(t, input.OwnedStackReceipt, &owned)
	require.Equal(t, "commerce-current-owned-stack-v1", owned.Schema)
	require.True(t, owned.RootReviewed && owned.LiveOwnedProcesses && owned.StrongTiDB && owned.AssetSQLCommit && owned.OriginalSessionActors && owned.RealChannelsDisabled && owned.GlobalMarket)
	require.Equal(t, input.Zone, owned.Zone)
	require.Equal(t, input.Accounts, owned.Accounts)
	require.Len(t, owned.Zones, 2)
	require.NotZero(t, owned.Zones[0])
	require.NotZero(t, owned.Zones[1])
	require.NotEqual(t, owned.Zones[0], owned.Zones[1])
	require.Equal(t, input.Zone, owned.Zones[0])
	require.Equal(t, input.AccountZones, owned.AccountZones)
	for i, zone := range input.AccountZones {
		require.Equal(t, owned.Zones[i%2], zone, "卖家在A，买家须交替在B/A，不能用双区进程冒同区验收")
	}
	require.Equal(t, uint64(0), owned.NoticeMS, "仅本次独立验收配置缩短公示；不等48小时也不写SQL跳过")
	require.LessOrEqual(t, owned.CoolingMS, uint64(1000))
	require.Equal(t, uint32(500), owned.FeeBPS)
	require.NotEmpty(t, owned.RuntimeProofs, "必须绑定本轮真实全栈包/启动/归属证明")
	for _, receipt := range owned.RuntimeProofs {
		currentCommerceRead(t, receipt, nil)
	}
	currentCommerceRead(t, input.MallJar, nil)
	currentCommerceRead(t, input.GatewayJar, nil)
	require.Equal(t, strings.ToLower(input.MallJar.SHA256), strings.ToLower(owned.MallJarSHA256))
	require.Equal(t, strings.ToLower(input.GatewayJar.SHA256), strings.ToLower(owned.GatewayJarSHA256))
	currentCommerceRead(t, input.ConfigFile, nil)
	cfg, err := config.Load(input.ConfigFile.Path)
	require.NoError(t, err)
	require.Equal(t, input.Zone, cfg.ZoneID)
	require.NoError(t, validateCommerceMockMallURL(cfg.GatewayAddr))
	require.NoError(t, validateCommerceMockMallURL(input.MallURL))
	cfg.Password = os.Getenv("COMMERCE_ACCEPTANCE_PASSWORD")
	require.NotEmpty(t, cfg.Password, "口令只从当前进程环境注入")
	cfg.LoadTables()
	payer, err := newCommerceMockPayer(input.MallURL, input.PaymentSubject, os.Getenv(commercePaymentSecretEnv))
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(input.EvidenceParent))
	info, err := os.Lstat(input.EvidenceParent)
	require.NoError(t, err)
	require.True(t, info.IsDir() && info.Mode()&os.ModeSymlink == 0)
	dir := filepath.Join(input.EvidenceParent, "commerce-"+input.RunID)
	require.NoError(t, os.Mkdir(dir, 0700), "每轮证据目录必须新建，已有原件不覆盖/不自动重跑")
	r := &currentCommerceRun{t: t, in: input, cfg: cfg, payer: payer, dir: dir, bots: map[string]*mallArrivalBot{}}
	t.Cleanup(func() {
		for _, bot := range r.bots {
			bot.close()
		}
		payer.client.CloseIdleConnections()
	})
	r.event("admitted", map[string]any{"run_id": input.RunID, "zone": input.Zone, "accounts": len(input.Accounts), "input_sha256": os.Getenv("COMMERCE_ACCEPTANCE_INPUT_SHA256"), "sql_mutations_by_driver": false})
	if !t.Run("official_前二十离线仍交付尾五_原身份_重登不重发", r.officialPaging) {
		return
	}
	if !t.Run("rmb_前二十离线仍交付尾五_卖家收益提现_托管撤回", r.rmbPagingAndPayout) {
		return
	}
	if !t.Run("rmb_买家撤销后迟到付款_原款退款且不发货", r.rmbLateRefund) {
		return
	}
	if !t.Run("恢复前二十原买家_原请求继续交付_重登无重复", r.restoreOfflineHeads) {
		return
	}
	r.t = t
	r.event("all-passed", map[string]any{"actual_gate_trade_scene": true, "mock_only": true, "official_tail": 5, "rmb_tail": 5, "first_twenty_kept_offline": true, "relogin_checked": true})
}

func (r *currentCommerceRun) event(stage string, value any) {
	r.t.Helper()
	r.sequence++
	raw, err := json.MarshalIndent(map[string]any{"stage": stage, "value": value}, "", "  ")
	require.NoError(r.t, err)
	file := filepath.Join(r.dir, fmt.Sprintf("%03d-%s.json", r.sequence, stage))
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	require.NoError(r.t, err)
	_, err = f.Write(append(raw, '\n'))
	closeErr := f.Close()
	require.NoError(r.t, err)
	require.NoError(r.t, closeErr)
}

func (r *currentCommerceRun) id(suffix string) string { return "ca-" + r.in.RunID + "-" + suffix }

func (r *currentCommerceRun) open(account string) *mallArrivalBot {
	r.t.Helper()
	if bot := r.bots[account]; bot != nil && bot.session.gc != nil {
		return bot
	}
	index := r.accountIndex(account)
	require.GreaterOrEqual(r.t, index, 0)
	zone := r.in.AccountZones[index]
	assignment, err := assignAssetArrivalGate(r.cfg.GatewayAddr, &httpAssignGateRequest{ZoneID: zone}, 5*time.Second)
	require.NoError(r.t, err)
	require.NoError(r.t, validateAssetArrivalAssignment(assignment))
	stats := &metrics.Stats{}
	gc, player, err := prepareBehaviorClient(assignment.GateIP, assignment.GatePort, account, r.cfg.Password, stats, assignment.TokenPayload, assignment.TokenSignature)
	require.NoError(r.t, err)
	bot := &mallArrivalBot{session: &equipSmokeSession{gc: gc, player: player, stats: stats, lastSent: map[uint32]time.Time{}}, caller: newSmokeCaller(gc, stats, account, mallArrivalRpcTimeout, errMallArrivalTimeout)}
	r.bots[account] = bot
	_, before := player.GetSceneInfoPush()
	require.NoError(r.t, gc.SendRequest(game.SceneSceneClientPlayerSceneInfoC2SMessageId, &scene.SceneInfoRequest{WithChannelDirectory: true}))
	require.True(r.t, waitUntil(8*time.Second, func() bool {
		info, sequence := player.GetSceneInfoPush()
		if sequence <= before || info.GetChannelDirectory().GetZoneId() != zone {
			return false
		}
		for _, channel := range info.GetChannelDirectory().GetChannels() {
			if channel.GetSceneId() == player.GetSceneID() {
				return true
			}
		}
		return false
	}), "正式scene目录必须证明本角色实际落在原分配zone，不只核配置/票据")
	r.event("login", map[string]any{"account_index": index, "player_id": gc.PlayerId, "actual_scene_zone": zone, "actual_scene_id": player.GetSceneID()})
	return bot
}

func (r *currentCommerceRun) accountIndex(account string) int {
	for i, a := range r.in.Accounts {
		if a == account {
			return i
		}
	}
	return -1
}

func (r *currentCommerceRun) rpc(bot *mallArrivalBot, method uint32, request, response proto.Message) {
	r.t.Helper()
	require.NoError(r.t, bot.jubaozhai(method, request, response))
	// 各正式响应均有error_message；用PB反射取正式字段，避免平行错误结构或手写tip码。
	field := response.ProtoReflect().Descriptor().Fields().ByName("error_message")
	require.NotNil(r.t, field)
	message := response.ProtoReflect().Get(field).Message()
	if message.IsValid() {
		id := message.Descriptor().Fields().ByName("id")
		require.NotNil(r.t, id)
		require.Zero(r.t, message.Get(id).Uint(), "正式RPC业务拒绝，方法=%d", method)
	}
}

func (r *currentCommerceRun) count(bot *mallArrivalBot) uint64 {
	r.t.Helper()
	count, err := bot.inventoryCount(r.in.ItemConfigID)
	require.NoError(r.t, err)
	return count
}

func (r *currentCommerceRun) createOfficial(bot *mallArrivalBot, label string, quantity uint32) *tradepb.CommerceOrderView {
	r.t.Helper()
	request := &tradepb.CreateOfficialOrderRequest{ClientRequestId: r.id(label), SkuId: r.in.SKU, Quantity: quantity}
	r.event("official-request", request)
	var first, replay tradepb.CommerceOrderResponse
	r.rpc(bot, game.ClientPlayerJubaozhaiCreateOfficialOrderMessageId, request, &first)
	r.rpc(bot, game.ClientPlayerJubaozhaiCreateOfficialOrderMessageId, request, &replay)
	require.Equal(r.t, first.GetOrder().GetOrderId(), replay.GetOrder().GetOrderId())
	require.Equal(r.t, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT, first.GetOrder().GetState())
	require.Equal(r.t, bot.session.gc.PlayerId, first.GetOrder().GetBuyerPlayerId())
	require.Equal(r.t, tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_OFFICIAL, first.GetOrder().GetKind())
	require.NotZero(r.t, first.GetOrder().GetAmount())
	require.Equal(r.t, r.in.UnitPriceMinor*uint64(quantity), first.GetOrder().GetAmount())
	require.Equal(r.t, "CNY", first.GetOrder().GetCurrency())
	r.event("official-order", first.GetOrder())
	return first.GetOrder()
}

func (r *currentCommerceRun) pay(order *tradepb.CommerceOrderView, refund bool) {
	r.t.Helper()
	input := commerceMockPay{OrderID: order.GetOrderId(), RequestID: r.id("pay-" + strings.ReplaceAll(order.GetOrderId(), "-", ""))}
	r.event("payment-request", input)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := r.payer.RecordPayment(ctx, input)
	require.NoError(r.t, err)
	require.Equal(r.t, order.GetOrderId(), first.OrderID)
	require.Equal(r.t, strconv.FormatUint(order.GetBuyerPlayerId(), 10), first.BuyerID)
	require.Equal(r.t, order.GetAmount(), first.AmountMinor)
	require.Equal(r.t, order.GetCurrency(), first.Currency)
	if refund {
		require.Contains(r.t, []string{"REFUND_PENDING", "REFUNDED"}, first.State)
	} else {
		require.Contains(r.t, []string{"DELIVERY_PENDING", "DELIVERED"}, first.State)
	}
	replayed, err := r.payer.RecordPayment(ctx, input)
	require.NoError(r.t, err)
	require.Equal(r.t, first.OrderID, replayed.OrderID)
	require.Equal(r.t, first.AmountMinor, replayed.AmountMinor)
	r.event("payment-result", first)
}

func (r *currentCommerceRun) http(path string, input, output any) {
	r.t.Helper()
	allowed := map[string]bool{"/internal/payments/order-view": true, "/internal/mock/reconcile": true, "/internal/mock/withdraw": true, "/internal/mock/withdrawals/reconcile": true}
	require.True(r.t, allowed[path], "付款方验收不允许直接下单/托管或提交资产回执")
	body, err := json.Marshal(input)
	require.NoError(r.t, err)
	nonce, err := commerceRandomNonce()
	require.NoError(r.t, err)
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.payer.baseURL+path, bytes.NewReader(body))
	require.NoError(r.t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Commerce-Subject", r.payer.subject)
	request.Header.Set("X-Commerce-Timestamp", timestamp)
	request.Header.Set("X-Commerce-Nonce", nonce)
	request.Header.Set("X-Commerce-Signature", commerceSign(r.payer.secret, "payment", r.payer.subject, timestamp, nonce, http.MethodPost, path, body))
	response, err := r.payer.client.Do(request)
	require.NoError(r.t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, commerceMaxResponseBytes+1))
	require.NoError(r.t, err)
	require.LessOrEqual(r.t, len(raw), commerceMaxResponseBytes)
	require.Equal(r.t, http.StatusOK, response.StatusCode, "原HTTP动作失败，不换请求号或伪造回执")
	if output != nil {
		require.NoError(r.t, json.Unmarshal(raw, output))
	}
}

func (r *currentCommerceRun) mallOrder(id string) commerceMallOrder {
	r.t.Helper()
	var order commerceMallOrder
	r.http("/internal/payments/order-view", map[string]string{"orderId": id}, &order)
	require.Equal(r.t, id, order.OrderID)
	return order
}

func (r *currentCommerceRun) await(bot *mallArrivalBot, id string, rmb bool, terminal tradepb.CommerceOrderState) {
	r.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		var answer tradepb.CommerceOrderResponse
		method := uint32(game.ClientPlayerJubaozhaiGetCommerceOrderMessageId)
		if rmb {
			method = game.ClientPlayerJubaozhaiGetRmbOrderMessageId
		}
		r.rpc(bot, method, &tradepb.GetCommerceOrderRequest{OrderId: id}, &answer)
		require.Equal(r.t, id, answer.GetOrder().GetOrderId())
		state := answer.GetOrder().GetState()
		if state == terminal {
			r.event("terminal-order", answer.GetOrder())
			return
		}
		require.NotEqual(r.t, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_MANUAL, state)
		if terminal == tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED {
			require.Equal(r.t, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERING, state)
		}
		require.True(r.t, time.Now().Add(1500*time.Millisecond).Before(deadline), "订单未在原预算内收敛")
		time.Sleep(1500 * time.Millisecond)
	}
}

func (r *currentCommerceRun) assertOfflineHead(orders []*tradepb.CommerceOrderView) {
	r.t.Helper()
	require.Len(r.t, orders, 20)
	for _, order := range orders {
		actual := r.mallOrder(order.GetOrderId())
		require.Equal(r.t, "DELIVERY_PENDING", actual.State, "离线前20单不得凭空终局")
		require.Equal(r.t, order.GetAmount(), actual.AmountMinor)
	}
	r.event("offline-head-still-pending", map[string]any{"count": 20, "order_ids": func() []string {
		var ids []string
		for _, o := range orders {
			ids = append(ids, o.GetOrderId())
		}
		return ids
	}()})
}

func (r *currentCommerceRun) reloginCheck(account string, expected uint64) *mallArrivalBot {
	r.t.Helper()
	r.bots[account].close()
	time.Sleep(2 * time.Second)
	bot := r.open(account)
	require.Equal(r.t, expected, r.count(bot), "重登读取原角色已落盘资产，不能靠发货回执冒持久化")
	return bot
}

func (r *currentCommerceRun) officialPaging(t *testing.T) {
	r.t = t
	var head []*tradepb.CommerceOrderView
	for i := 1; i <= 25; i++ {
		account := r.in.Accounts[i]
		bot := r.open(account)
		before := r.count(bot)
		order := r.createOfficial(bot, fmt.Sprintf("official-%02d", i), 1)
		if i <= 20 {
			bot.close()
			time.Sleep(2 * time.Second)
			r.pay(order, false)
			head = append(head, order)
			continue
		}
		r.pay(order, false)
		r.await(bot, order.GetOrderId(), false, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED)
		require.Equal(t, before+r.in.CountPerUnit, r.count(bot))
		time.Sleep(mallArrivalSettleGap * mallArrivalSettleTicks)
		require.Equal(t, before+r.in.CountPerUnit, r.count(bot), "重投不得重复入包")
		r.reloginCheck(account, before+r.in.CountPerUnit)
	}
	time.Sleep(time.Duration(r.in.OfflineHoldSeconds) * time.Second)
	r.assertOfflineHead(head)
	r.event("official-tail-real-delivery", map[string]any{"tail_count": 5, "head_pending": 20, "sql_commit_inferred_from_production_receipt_and_relogin": true})
}

func (r *currentCommerceRun) createListing(seller *mallArrivalBot, label string) uint64 {
	r.t.Helper()
	input := &tradepb.CreateRmbConsignmentRequest{ClientRequestId: r.id(label), Assets: &tradepb.TradeAssetSelection{Items: map[uint32]uint32{r.in.ItemConfigID: 1}}, PriceFen: 101, Title: "本轮真实托管验收"}
	r.event("rmb-create-request", input)
	var answer, replay tradepb.RmbConsignmentResponse
	r.rpc(seller, game.ClientPlayerJubaozhaiCreateRmbConsignmentMessageId, input, &answer)
	id := answer.GetListing().GetListingId()
	require.NotZero(r.t, id)
	r.rpc(seller, game.ClientPlayerJubaozhaiCreateRmbConsignmentMessageId, input, &replay)
	require.Equal(r.t, id, replay.GetListing().GetListingId())
	deadline := time.Now().Add(90 * time.Second)
	for {
		var current tradepb.RmbConsignmentResponse
		r.rpc(seller, game.ClientPlayerJubaozhaiGetRmbConsignmentMessageId, &tradepb.GetRmbConsignmentRequest{ListingId: id}, &current)
		view := current.GetListing()
		require.Equal(r.t, id, view.GetListingId())
		if view.GetPhase() == "ON_SALE" {
			require.Equal(r.t, seller.session.gc.PlayerId, view.GetSellerPlayerId())
			require.Equal(r.t, r.in.Zone, view.GetMarketZone())
			require.Equal(r.t, uint64(101), view.GetPriceFen())
			require.Equal(r.t, uint32(500), view.GetFeeBps())
			r.event("rmb-registered", view)
			return id
		}
		require.NotContains(r.t, []string{"MANUAL", "RETURNED", "EXPIRED"}, view.GetState())
		require.True(r.t, time.Now().Add(1500*time.Millisecond).Before(deadline), "托管未登记；不直接向Mall register补原件")
		time.Sleep(1500 * time.Millisecond)
	}
}

func (r *currentCommerceRun) buy(bot *mallArrivalBot, listing uint64, label string) *tradepb.CommerceOrderView {
	r.t.Helper()
	input := &tradepb.BuyRmbConsignmentRequest{ListingId: listing, ClientRequestId: r.id(label)}
	r.event("rmb-buy-request", input)
	var answer, replay tradepb.CommerceOrderResponse
	r.rpc(bot, game.ClientPlayerJubaozhaiBuyRmbConsignmentMessageId, input, &answer)
	r.rpc(bot, game.ClientPlayerJubaozhaiBuyRmbConsignmentMessageId, input, &replay)
	require.Equal(r.t, answer.GetOrder().GetOrderId(), replay.GetOrder().GetOrderId())
	require.Equal(r.t, bot.session.gc.PlayerId, answer.GetOrder().GetBuyerPlayerId())
	require.Equal(r.t, tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_RMB_CONSIGNMENT, answer.GetOrder().GetKind())
	require.Equal(r.t, uint64(101), answer.GetOrder().GetAmount())
	require.Equal(r.t, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_AWAITING_PAYMENT, answer.GetOrder().GetState())
	r.event("rmb-buyer-order", answer.GetOrder())
	return answer.GetOrder()
}

func (r *currentCommerceRun) rmbPagingAndPayout(t *testing.T) {
	r.t = t
	seller := r.open(r.in.Accounts[0])
	original := r.count(seller)
	seed := r.createOfficial(seller, "seller-stock", 20)
	r.pay(seed, false)
	r.await(seller, seed.GetOrderId(), false, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED)
	require.Equal(t, original+40, r.count(seller))
	seller = r.reloginCheck(r.in.Accounts[0], original+40)
	var head []*tradepb.CommerceOrderView
	var tailIDs []string
	for i := 1; i <= 25; i++ {
		listing := r.createListing(seller, fmt.Sprintf("rmb-%02d", i))
		account := r.in.Accounts[i]
		buyer := r.open(account)
		// 首轮官方离线单在本次重新登录后可正常发货；基线须等它落盘，不能混入RMB增量。
		if i <= 20 {
			orders, err := buyer.allOrders()
			require.NoError(t, err)
			for _, o := range orders {
				if o.GetKind() == tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_OFFICIAL && o.GetState() == tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERING {
					r.await(buyer, o.GetOrderId(), false, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED)
				}
			}
		}
		before := r.count(buyer)
		order := r.buy(buyer, listing, fmt.Sprintf("buy-%02d", i))
		if i <= 20 {
			buyer.close()
			time.Sleep(2 * time.Second)
			r.pay(order, false)
			head = append(head, order)
			r.heads = append(r.heads, currentCommerceOfflineOrder{Account: account, Before: before, Order: order})
			continue
		}
		r.pay(order, false)
		r.await(buyer, order.GetOrderId(), true, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED)
		require.Equal(t, before+1, r.count(buyer))
		time.Sleep(mallArrivalSettleGap * mallArrivalSettleTicks)
		require.Equal(t, before+1, r.count(buyer))
		r.reloginCheck(account, before+1)
		tailIDs = append(tailIDs, order.GetOrderId())
	}
	time.Sleep(time.Duration(r.in.OfflineHoldSeconds) * time.Second)
	r.assertOfflineHead(head)
	// 对尾五逐单核5%向上取分：101-6=95；只保留本轮真实已交付的原订单收益。
	deadline := time.Now().Add(30 * time.Second)
	for {
		var incomes tradepb.ListSellerIncomesResponse
		r.rpc(seller, game.ClientPlayerJubaozhaiListSellerIncomesMessageId, &tradepb.ListSellerIncomesRequest{Limit: 100}, &incomes)
		ready := 0
		for _, id := range tailIDs {
			for _, income := range incomes.GetIncomes() {
				if income.GetOrderId() == id {
					require.Equal(t, uint64(101), income.GetGrossFen())
					require.Equal(t, uint64(6), income.GetFeeFen())
					require.Equal(t, uint64(95), income.GetNetFen())
					require.Zero(t, income.GetPaidFen())
					require.Zero(t, income.GetReservedFen())
					if income.GetAvailableMs() <= incomes.GetServerNowMs() {
						ready++
					}
				}
			}
		}
		if ready == 5 {
			r.event("seller-tail-incomes", incomes.GetIncomes())
			break
		}
		require.True(t, time.Now().Before(deadline), "尾五未结算为原卖家可提现收入")
		time.Sleep(1500 * time.Millisecond)
	}
	request := &tradepb.CreateWithdrawalRequest{ClientRequestId: r.id("withdraw-five"), AmountFen: 475}
	r.event("withdrawal-request", request)
	var first, replay tradepb.WithdrawalResponse
	r.rpc(seller, game.ClientPlayerJubaozhaiCreateWithdrawalMessageId, request, &first)
	r.rpc(seller, game.ClientPlayerJubaozhaiCreateWithdrawalMessageId, request, &replay)
	id := first.GetWithdrawal().GetWithdrawalId()
	require.NotEmpty(t, id)
	require.Equal(t, id, replay.GetWithdrawal().GetWithdrawalId())
	require.Equal(t, uint64(475), first.GetWithdrawal().GetAmountFen())
	pay := map[string]string{"withdrawalId": id, "requestId": r.id("payout-five")}
	r.event("payout-request", pay)
	var paid, repaid map[string]any
	r.http("/internal/mock/withdraw", pay, &paid)
	r.http("/internal/mock/withdraw", pay, &repaid)
	require.Equal(t, paid, repaid)
	require.Equal(t, "PAID", paid["state"])
	require.Equal(t, "mock", paid["channel"])
	require.NotEmpty(t, paid["transactionId"])
	r.http("/internal/mock/withdrawals/reconcile", map[string]any{}, nil)
	var done tradepb.WithdrawalResponse
	r.rpc(seller, game.ClientPlayerJubaozhaiGetWithdrawalMessageId, &tradepb.GetWithdrawalRequest{WithdrawalId: id}, &done)
	require.Equal(t, "PAID", done.GetWithdrawal().GetState())
	require.Equal(t, "mock", done.GetWithdrawal().GetChannel())
	require.Equal(t, uint64(475), done.GetWithdrawal().GetAmountFen())
	require.Equal(t, paid["transactionId"], done.GetWithdrawal().GetTransactionId())
	r.event("withdrawal-paid", done.GetWithdrawal())
	// 撤回未售托管必须走原卖家RPC→return worker→Scene→SQL，精确回到原角色。
	beforeCancel := r.count(seller)
	cancelID := r.createListing(seller, "return-one")
	require.Equal(t, beforeCancel-1, r.count(seller))
	var cancelled tradepb.RmbConsignmentResponse
	r.rpc(seller, game.ClientPlayerJubaozhaiCancelRmbConsignmentMessageId, &tradepb.CancelRmbConsignmentRequest{ListingId: cancelID}, &cancelled)
	end := time.Now().Add(90 * time.Second)
	for r.count(seller) != beforeCancel {
		require.True(t, time.Now().Before(end), "原卖家托管回退未SQL到账")
		time.Sleep(1500 * time.Millisecond)
	}
	r.reloginCheck(r.in.Accounts[0], beforeCancel)
	r.event("rmb-tail-real-delivery-payout-and-return", map[string]any{"tail_count": 5, "head_pending": 20, "withdrawal_id": id, "amount_fen": 475, "return_listing": cancelID})
}

func (r *currentCommerceRun) rmbLateRefund(t *testing.T) {
	r.t = t
	seller := r.open(r.in.Accounts[0])
	buyer := r.open(r.in.Accounts[25])
	before := r.count(buyer)
	listing := r.createListing(seller, "late-refund")
	order := r.buy(buyer, listing, "late-refund-buy")
	var cancelled tradepb.CommerceOrderResponse
	r.rpc(buyer, game.ClientPlayerJubaozhaiCancelRmbOrderMessageId, &tradepb.CancelCommerceOrderRequest{OrderId: order.GetOrderId()}, &cancelled)
	require.Equal(t, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_CANCELLED, cancelled.GetOrder().GetState())
	r.pay(order, true)
	deadline := time.Now().Add(30 * time.Second)
	for {
		r.http("/internal/mock/reconcile", map[string]any{}, nil)
		state := r.mallOrder(order.GetOrderId())
		if state.State == "REFUNDED" {
			break
		}
		require.Equal(t, "REFUND_PENDING", state.State)
		require.True(t, time.Now().Before(deadline))
		time.Sleep(1500 * time.Millisecond)
	}
	r.await(buyer, order.GetOrderId(), true, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_REFUNDED)
	time.Sleep(mallArrivalSettleGap * mallArrivalSettleTicks)
	require.Equal(t, before, r.count(buyer))
	r.reloginCheck(r.in.Accounts[25], before)
	var incomes tradepb.ListSellerIncomesResponse
	r.rpc(seller, game.ClientPlayerJubaozhaiListSellerIncomesMessageId, &tradepb.ListSellerIncomesRequest{Limit: 100}, &incomes)
	for _, income := range incomes.GetIncomes() {
		require.NotEqual(t, order.GetOrderId(), income.GetOrderId(), "退款原单不得生成卖家收入")
	}
	// 已撤销买单不销毁listing；收尾仍由原卖家正常撤回托管，不直接修改SQL状态。
	beforeReturn := r.count(seller)
	var cancelListing tradepb.RmbConsignmentResponse
	r.rpc(seller, game.ClientPlayerJubaozhaiCancelRmbConsignmentMessageId, &tradepb.CancelRmbConsignmentRequest{ListingId: listing}, &cancelListing)
	end := time.Now().Add(90 * time.Second)
	for r.count(seller) != beforeReturn+1 {
		require.True(t, time.Now().Before(end), "迟到退款后的原托管未回到原卖家")
		time.Sleep(1500 * time.Millisecond)
	}
	r.reloginCheck(r.in.Accounts[0], beforeReturn+1)
	r.event("late-payment-refunded-without-assets", map[string]any{"order_id": order.GetOrderId(), "buyer_count_before_after": before, "seller_income_absent": true, "listing_return_completed": listing})
}

func (r *currentCommerceRun) restoreOfflineHeads(t *testing.T) {
	r.t = t
	require.Len(t, r.heads, 20)
	for _, head := range r.heads {
		buyer := r.open(head.Account)
		r.await(buyer, head.Order.GetOrderId(), true, tradepb.CommerceOrderState_COMMERCE_ORDER_STATE_DELIVERED)
		require.Equal(t, head.Before+1, r.count(buyer), "保持离线期间的原合同只能交给原买家一次")
		time.Sleep(mallArrivalSettleGap * mallArrivalSettleTicks)
		require.Equal(t, head.Before+1, r.count(buyer))
		r.reloginCheck(head.Account, head.Before+1)
	}
	r.event("offline-original-heads-restored", map[string]any{"count": 20, "all_original_contracts_delivered": true, "relogin_checked": true, "driver_sql_writes": false})
}

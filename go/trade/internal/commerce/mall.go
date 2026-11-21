package commerce

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"io"
	"math"
	"net/http"
	"net/url"
	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"regexp"
	"shared/accountdirectory"
	"shared/currencytype"
	"shared/roleidentity"
	"shared/tracing"
	"strconv"
	"strings"
	"time"
	"trade/internal/data"
)

// Mall只管官方库存/真实支付；本适配器保持HTTP契约隔离，不拥有第二份钱包。
type Mall struct {
	Base                          string
	GatewaySecret, DeliverySecret string
	HTTP                          *http.Client
	Engine                        *Engine
	// 仅由驱动 Tick 的单个 worker 持有；重启可从头扫描，订单唯一键继续守住原合同与 seq。
	pollCursor string
	// 扣回待办(付款被渠道修订的官方订单)的扫描游标,同样只归驱动 Tick 的那一个 worker。
	reversalCursor string
}
type mallIntent struct {
	OrderID            string `json:"orderId"`
	BuyerID            string `json:"buyerId"`
	BuyerAccount       string `json:"buyerAccount"`
	BuyerOwnerRevision string `json:"buyerOwnerRevision"`
	SKU                uint64 `json:"skuId"`
	Quantity           uint32 `json:"quantity"`
	Config             uint32 `json:"itemConfigId"`
	Count              uint32 `json:"itemCount"`
	Title              string `json:"title"`
	Amount             uint64 `json:"amountMinor"`
	Currency           string `json:"currency"`
	// 发放物二选一:物品(Config × Count)或货币(CurrencyType × CurrencyAmount,如充值灵石包),另一种全为 0。
	// 货币种类 0 是银两,有没有货币发放看 CurrencyAmount。
	CurrencyType   uint32 `json:"currencyType"`
	CurrencyAmount uint64 `json:"currencyAmount"`
}
type mallPollEntry struct {
	Intent *mallIntent `json:"intent"`
	Cursor *string     `json:"cursor"`
	// 订单绑定付款的最新修订号(付款被渠道撤销或撤销又被撤回过),没有为 0;见 official_reversal.go。
	PaymentRevision uint32 `json:"paymentRevision"`
}

// mallReversalEntry 是扣回待办的一项:商城要求把这张订单的发放收敛到 Revision(Entitled = 该不该持有,偶数为真)。
type mallReversalEntry struct {
	Intent   *mallIntent `json:"intent"`
	Revision uint32      `json:"revision"`
	Entitled bool        `json:"entitled"`
	Cursor   *string     `json:"cursor"`
}
type mallPollPage struct {
	Entries []mallPollEntry `json:"entries"`
}
type mallScanCursor struct {
	afterMs, upperMs int64
	afterID, upperID string
}

func parseMallCursor(raw string) (mallScanCursor, error) {
	var out mallScanCursor
	fields := strings.Split(raw, ":")
	if len(raw) > 256 || len(fields) != 5 || fields[0] != "v1" || !mallOrderID.MatchString(fields[2]) || !mallOrderID.MatchString(fields[4]) {
		return out, errors.New("商城交付游标非法")
	}
	after, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || after < 0 || strconv.FormatInt(after, 10) != fields[1] {
		return out, errors.New("商城交付游标时间非法")
	}
	upper, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || upper < 0 || strconv.FormatInt(upper, 10) != fields[3] || after > upper || after == upper && fields[2] > fields[4] {
		return out, errors.New("商城交付游标上界非法")
	}
	return mallScanCursor{afterMs: after, afterID: fields[2], upperMs: upper, upperID: fields[4]}, nil
}

// 先核整页包装，再处理任何业务或前移游标。未知/畸形响应不能变成第一页或跳过未尝试项。
func validateMallPage(page mallPollPage, requested string) error {
	if page.Entries == nil || len(page.Entries) > 20 {
		return errors.New("商城交付页非法")
	}
	var previous *mallScanCursor
	if requested != "" {
		cursor, err := parseMallCursor(requested)
		if err != nil {
			return err
		}
		previous = &cursor
	}
	for i, entry := range page.Entries {
		if entry.Intent == nil || entry.Cursor == nil || !mallOrderID.MatchString(entry.Intent.OrderID) {
			return errors.New("商城交付页缺少原意图或游标")
		}
		if *entry.Cursor == "" {
			if i != len(page.Entries)-1 {
				return errors.New("商城交付页提前结束")
			}
			continue
		}
		cursor, err := parseMallCursor(*entry.Cursor)
		if err != nil {
			return err
		}
		if cursor.afterID != entry.Intent.OrderID {
			return errors.New("商城交付游标与订单不符")
		}
		if previous != nil && (cursor.upperMs != previous.upperMs || cursor.upperID != previous.upperID || cursor.afterMs < previous.afterMs || cursor.afterMs == previous.afterMs && cursor.afterID <= previous.afterID) {
			return errors.New("商城交付页游标倒退或扫描上界改变")
		}
		previous = &cursor
	}
	return nil
}

type MallOrder struct {
	OrderID            string `json:"orderId"`
	BuyerID            string `json:"buyerId"`
	BuyerAccount       string `json:"buyerAccount"`
	BuyerOwnerRevision string `json:"buyerOwnerRevision"`
	SKU                uint64 `json:"skuId"`
	Quantity           uint32 `json:"quantity"`
	Amount             uint64 `json:"amountMinor"`
	Currency           string `json:"currency"`
	State              string `json:"state"`
	Deadline           uint64 `json:"paymentDeadlineMs"`
	PaymentRevision    uint32 `json:"paymentRevision"`
}

var mallOrderID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// errRolePreparing 表示买家角色还在投票,这一轮判不了退不退款。不是拒绝:商城的待办还在,下一轮绕回来再判。
var errRolePreparing = errors.New("买家角色还在冻结投票,下一轮再判")

type mallSKU struct {
	ID        uint64 `json:"skuId"`
	Title     string `json:"title"`
	Price     uint64 `json:"priceMinor"`
	Currency  string `json:"currency"`
	Available uint64 `json:"available"`
}

func NewMall(base, gateway, delivery string, e *Engine) (*Mall, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || len(gateway) < 32 || len(delivery) < 32 || gateway == delivery {
		return nil, errors.New("商城地址或独立调用密钥配置非法")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	return &Mall{Base: strings.TrimRight(base, "/"), GatewaySecret: gateway, DeliverySecret: delivery,
		HTTP: &http.Client{Timeout: 2500 * time.Millisecond, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Engine: e}, nil
}

// commerceSignature 是商城 X-Commerce-Signature 的 v1 算法：正文 SHA-256 小写 hex，与其余七项按 \n 拼成规范串，
// HMAC-SHA256 后取小写 hex。必须与 java/mall_game HmacAuthentication.sign、robot commerceSign 逐字一致；
// 三边用同一组输入核对同一个期望值（mall_signature_test.go），改任一边都要同批改三边，否则线上表现是全部 401。
// path 是商城看到的原始 URI（含部署前缀），body 是实际发出的字节。
func commerceSignature(secret, role, subject, timestamp, nonce, method, path string, body []byte) string {
	hash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.Join([]string{"v1", role, subject, timestamp, nonce, method, path, hex.EncodeToString(hash[:])}, "\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *Mall) call(ctx context.Context, role, subject, method, path string, input, output any) error {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, m.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nonceRaw := make([]byte, 16)
	if _, err = rand.Read(nonceRaw); err != nil {
		return err
	}
	nonce := hex.EncodeToString(nonceRaw)
	secret := m.GatewaySecret
	if role == "delivery" {
		secret = m.DeliverySecret
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Commerce-Subject", subject)
	request.Header.Set("X-Commerce-Timestamp", stamp)
	request.Header.Set("X-Commerce-Nonce", nonce)
	request.Header.Set("X-Commerce-Signature", commerceSignature(secret, role, subject, stamp, nonce, method, path, body))
	// 带上 traceparent 调商城(distributed-tracing.md §6.2);签名只覆盖上面几项,多一个传播头不影响验签。
	response, err := tracing.DoHTTP(m.HTTP, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil {
		return err
	}
	if len(raw) > 256*1024 {
		return errors.New("商城应答越界")
	}
	if response.StatusCode != 200 {
		if response.StatusCode == 400 {
			return ErrInvalid
		}
		if response.StatusCode == 404 {
			return ErrMissing
		}
		if response.StatusCode == 409 {
			return ErrConflict
		}
		return fmt.Errorf("商城请求失败 HTTP %d", response.StatusCode)
	}
	if output != nil {
		return json.Unmarshal(raw, output)
	}
	return nil
}
func (m *Mall) Skus(ctx context.Context, player, after uint64, limit uint32) (*tradepb.ListOfficialSkusResponse, error) {
	var raw struct {
		Skus []mallSKU `json:"skus"`
	}
	if err := m.call(ctx, "gateway", strconv.FormatUint(player, 10), "POST", "/api/skus/list", map[string]any{"afterSkuId": after, "limit": min(max(limit, 1), 100)}, &raw); err != nil {
		return nil, err
	}
	out := &tradepb.ListOfficialSkusResponse{}
	for _, s := range raw.Skus {
		if s.ID <= after {
			continue
		}
		out.Skus = append(out.Skus, &tradepb.OfficialSkuView{SkuId: s.ID, Title: s.Title, PriceMinor: s.Price, Currency: s.Currency, Available: s.Available})
		out.NextSkuId = s.ID
		if len(out.Skus) >= int(min(max(limit, 1), 100)) {
			break
		}
	}
	return out, nil
}
func (m *Mall) Create(ctx context.Context, player uint64, in *tradepb.CreateOfficialOrderRequest) (*tradepb.CommerceOrderResponse, error) {
	if player == 0 || in.GetSkuId() == 0 || in.GetQuantity() == 0 || in.GetQuantity() > 4096 || len(in.GetClientRequestId()) < 8 || len(in.GetClientRequestId()) > 80 {
		return nil, ErrInvalid
	}
	var out MallOrder
	owned, ok := roleidentity.Original(ctx)
	if !ok || owned.Role.GetPlayerId() != player {
		return nil, ErrInvalid
	}
	err := m.call(ctx, "gateway", strconv.FormatUint(player, 10), "POST", "/api/orders", map[string]any{"requestId": in.GetClientRequestId(), "skuId": in.GetSkuId(), "quantity": in.GetQuantity(), "buyerAccount": owned.Account, "buyerOwnerRevision": strconv.FormatUint(owned.OwnerRevision, 10)}, &out)
	if err == nil && out.BuyerID != strconv.FormatUint(player, 10) {
		return nil, errors.New("商城订单主体不匹配")
	}
	if err != nil {
		return nil, err
	}
	return mallAnswer(out)
}
func (m *Mall) List(ctx context.Context, player uint64, after string, limit uint32) ([]*tradepb.CommerceOrderView, error) {
	var orders []MallOrder
	owned, ok := roleidentity.Original(ctx)
	if !ok || owned.Role.GetPlayerId() != player {
		return nil, ErrInvalid
	}
	err := m.call(ctx, "gateway", strconv.FormatUint(player, 10), "POST", "/api/orders/list", map[string]any{"afterOrderId": after, "limit": min(max(limit, 1), 100), "buyerAccount": owned.Account, "buyerOwnerRevision": strconv.FormatUint(owned.OwnerRevision, 10)}, &orders)
	if err != nil {
		return nil, err
	}
	out := make([]*tradepb.CommerceOrderView, 0, len(orders))
	for _, r := range orders {
		if r.BuyerID != strconv.FormatUint(player, 10) {
			return nil, errors.New("商城订单主体不匹配")
		}
		answer, err := mallAnswer(r)
		if err != nil {
			return nil, err
		}
		out = append(out, answer.Order)
	}
	return out, nil
}
func (m *Mall) Order(ctx context.Context, player uint64, id string, cancel bool) (*tradepb.CommerceOrderResponse, error) {
	if !mallOrderID.MatchString(id) {
		return nil, ErrInvalid
	}
	var out MallOrder
	owned, ok := roleidentity.Original(ctx)
	if !ok || owned.Role.GetPlayerId() != player {
		return nil, ErrInvalid
	}
	method, path := "POST", "/api/orders/query"
	if cancel {
		path = "/api/orders/cancel"
	}
	err := m.call(ctx, "gateway", strconv.FormatUint(player, 10), method, path, map[string]any{"orderId": id, "buyerAccount": owned.Account, "buyerOwnerRevision": strconv.FormatUint(owned.OwnerRevision, 10)}, &out)
	if err == nil && out.BuyerID != strconv.FormatUint(player, 10) {
		return nil, errors.New("商城订单主体不匹配")
	}
	if err != nil {
		return nil, err
	}
	return mallAnswer(out)
}
func mallAnswer(o MallOrder) (*tradepb.CommerceOrderResponse, error) {
	states := map[string]tradepb.CommerceOrderState{"AWAITING_PAYMENT": 2, "DELIVERY_PENDING": 4, "DELIVERED": 5, "CANCELLED": 7, "REFUND_PENDING": 8, "REFUNDED": 9, "MANUAL": 10, "REVERSING": 12, "REVERSED": 13}
	// 版本号按 (paymentRevision << 4) | stateRank 单调递增:同一修订号内按生命周期排秩,修订号每次渠道翻转加 1,
	// 避免 REVERSING(12)->DELIVERED(5) / MANUAL(10) 或 REVERSED(13)->REVERSING(12) 时客户端缓存因版本号倒退被冻结。
	ranks := map[string]uint64{
		"AWAITING_PAYMENT": 1,
		"DELIVERY_PENDING": 2,
		"CANCELLED":        3,
		"REVERSING":        3,
		"REFUND_PENDING":   4,
		"DELIVERED":        5,
		"REVERSED":         5,
		"REFUNDED":         5,
		"MANUAL":           6,
	}
	state, known := states[o.State]
	buyer, err := strconv.ParseUint(o.BuyerID, 10, 64)
	if !known || err != nil || buyer == 0 || !mallOrderID.MatchString(o.OrderID) {
		return nil, errors.New("商城订单应答无效")
	}
	version := (uint64(o.PaymentRevision) << 4) | ranks[o.State]
	return &tradepb.CommerceOrderResponse{Order: &tradepb.CommerceOrderView{OrderId: o.OrderID, Kind: 1, State: state, Version: version, BuyerPlayerId: buyer, Amount: o.Amount, Currency: o.Currency, RelatedId: strconv.FormatUint(o.SKU, 10)}, ServerNowMs: uint64(time.Now().UnixMilli())}, nil
}

// officialBundle 把意图里的发放物转成资产包:物品或货币二选一,两种都有、都没有或越界都拒绝。
func officialBundle(in mallIntent) (*assetpb.AssetBundle, bool) {
	switch {
	case in.Config != 0 && in.Count != 0 && in.Count <= 4096 && in.CurrencyType == 0 && in.CurrencyAmount == 0:
		return &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: in.Config, Count: in.Count}}}, true
	case in.Config == 0 && in.Count == 0 && in.CurrencyAmount != 0 && in.CurrencyAmount <= math.MaxInt64 && currencytype.IsKnown(in.CurrencyType):
		return &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: in.CurrencyType, Amount: in.CurrencyAmount}}}, true
	}
	return nil, false
}

func (m *Mall) accept(ctx context.Context, in mallIntent) (*tradepb.TradeCommerceRecord, error) {
	return m.acceptRevised(ctx, in, 0)
}

// acceptRevised 找回或新建这张订单的交付合同。paymentRevision 是商城报来的付款修订号(没有为 0),只在新建时用:
// 已有的合同原样返回,修订号由调用方另行记上(Engine.applyPaymentRevision)。
func (m *Mall) acceptRevised(ctx context.Context, in mallIntent, paymentRevision uint32) (*tradepb.TradeCommerceRecord, error) {
	player, err := strconv.ParseUint(in.BuyerID, 10, 64)
	revision, revisionErr := canonicalID(in.BuyerOwnerRevision)
	bundle, bundleOK := officialBundle(in)
	if err != nil || player == 0 || revisionErr != nil || !accountdirectory.ValidAccount(in.BuyerAccount) || !mallOrderID.MatchString(in.OrderID) || !bundleOK || in.Quantity == 0 {
		return nil, ErrInvalid
	}
	key := RequestKey("official", player, in.OrderID)
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	if old, err := m.Engine.Repo.ByRequest(ctx, key); err == nil {
		if !bytes.Equal(hash[:], old.RequestDigest) {
			return nil, ErrReplay
		}
		return old, nil
	}
	id, err := m.Engine.IDs.Next(ctx)
	if err != nil {
		return nil, noWrite(err)
	}
	now := m.Engine.now()
	w := &tradepb.TradeCommerceWorkflow{ExternalOrderSn: in.OrderID, SkuId: in.SKU, SkuQuantity: in.Quantity, ExternalAmount: in.Amount, ExternalCurrency: in.Currency, OfficialBundle: bundle,
		PaymentRevision: paymentRevision, PaymentRevoked: paymentRevision%2 == 1}
	w.BuyerIdentity = &tradepb.TradeRoleIdentity{Account: in.BuyerAccount, OwnerRevision: revision}
	r := &tradepb.TradeCommerceRecord{CommerceId: id, RequestKey: key, RequestDigest: hash[:], Kind: 1, BuyerPlayerId: player, CreatedMs: now, UpdatedMs: now, Version: 1}
	if m.Engine.Roles == nil {
		return nil, errors.New("官方交付角色提交互锁未配置")
	}
	// 退款结局不可逆,不能凭可能落后的副本下判断:冻结态或修订号落后时(解冻或过户的推送丢了)先读权威状态,第二遍才按它判。
	// 也不能凭还在投票的准备令牌下判断:撤销了角色就回来了。这时回可重试,待办还在商城,下一轮再判。
	attempt := 0
	err = rolledBack(m.Engine.Roles.RunTx(ctx, func() error {
		attempt++
		healed := attempt > 1
		return m.Engine.Repo.Tx(ctx, func(tx *sql.Tx) error {
			// 事务体可能重跑:每次从头判定,不沿用上一次的结论。
			w.Stage, w.Quarantine, r.Status = stageOfficialDeliver, nil, Delivering
			if w.PaymentRevoked {
				// 付款已被渠道撤销:不发放,也就不看角色归属,直接记下「不持有」的终局。
				w.Stage, r.Status = stageOfficialReversed, Cancelled
			} else {
				owned, err := m.Engine.Roles.Ownership(ctx, tx, player)
				if err != nil {
					return err
				}
				if !healed && (owned.TradeListingId != 0 || owned.PrepareToken != 0 || owned.OwnerRevision < revision) {
					return staleRole(player)
				}
				switch {
				case owned.Account == in.BuyerAccount && owned.OwnerRevision == revision && owned.TradeListingId == 0:
					if owned.PrepareToken != 0 {
						return errRolePreparing
					}
				case paymentRevision == 0:
					// 尚无任何资产op：先持久原意图的退款结局，再回报商城，另一副本不能改发给当前新主。
					w.Stage, r.Status = "official_refund", 8
				default:
					// 修订过的付款不走退款(订单状态已归扣回流程管):转人工。
					markManual(r, w, manualReasonOwnerChanged, "官方订单的买家角色已换主,付款修订号 "+strconv.FormatUint(uint64(paymentRevision), 10), now)
				}
			}
			payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(w)
			if err != nil {
				return err
			}
			r.WorkflowProto = payload
			return m.Engine.Repo.Insert(ctx, tx, r)
		})
	}))
	if err != nil {
		old, read := m.Engine.Repo.ByRequest(ctx, key)
		if read == nil && bytes.Equal(hash[:], old.RequestDigest) {
			return old, nil
		}
		return nil, err
	}
	return r, nil
}

// 领取无需破坏性出队：多个副本会得到同一意图，同库唯一request_key保证只有一条资产seq。
// 单条意图失败不挡同批其余意图(2026-10-20,与 Engine.Tick 的单行隔离同一条修复):各条错误汇总后一并返回,
// 本轮预算用完才提前结束。
//
// 拉取本身只读,拉不到、页面非法、预算用完都是确定没写,不打排空 Hold,见 OutcomeKnown。
func (m *Mall) Tick(ctx context.Context) error {
	return errors.Join(m.tickDeliveries(ctx), m.tickReversals(ctx))
}

func (m *Mall) tickDeliveries(ctx context.Context) error {
	var page mallPollPage
	if err := m.call(ctx, "delivery", "trade", "POST", "/internal/deliveries/poll", map[string]any{"limit": 20, "cursor": m.pollCursor}, &page); err != nil {
		return noWrite(err)
	}
	if err := validateMallPage(page, m.pollCursor); err != nil {
		return noWrite(err)
	}
	if len(page.Entries) == 0 {
		m.pollCursor = ""
		return nil
	}
	var errs []error
	for _, entry := range page.Entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, noWrite(err))
			break
		}
		if err := m.deliverIntent(ctx, *entry.Intent, entry.PaymentRevision); err != nil {
			errs = append(errs, fmt.Errorf("商城交付意图 %q: %w", entry.Intent.OrderID, err))
		}
		// 这项无论成功或依赖失败都已尝试；待办仍在商城，下轮绕回按原订单重试，不用新 seq。
		m.pollCursor = *entry.Cursor
	}
	return errors.Join(errs...)
}

// tickReversals 处理一页扣回待办:订单绑定的付款被渠道撤销或撤销又被撤回,把发放收敛到最新修订号并回执。
// 翻页、单项隔离与交付待办相同;拉取只读,拉不到或页面非法都是确定没写。
func (m *Mall) tickReversals(ctx context.Context) error {
	var page struct {
		Entries []mallReversalEntry `json:"entries"`
	}
	if err := m.call(ctx, "delivery", "trade", "POST", "/internal/reversals/poll", map[string]any{"limit": 20, "cursor": m.reversalCursor}, &page); err != nil {
		return noWrite(err)
	}
	check := mallPollPage{Entries: make([]mallPollEntry, 0, len(page.Entries))}
	for _, entry := range page.Entries {
		if entry.Revision == 0 || entry.Entitled != (entry.Revision%2 == 0) {
			return noWrite(errors.New("商城扣回页的修订号非法"))
		}
		check.Entries = append(check.Entries, mallPollEntry{Intent: entry.Intent, Cursor: entry.Cursor})
	}
	if err := validateMallPage(check, m.reversalCursor); err != nil {
		return noWrite(err)
	}
	if len(page.Entries) == 0 {
		m.reversalCursor = ""
		return nil
	}
	var errs []error
	for _, entry := range page.Entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, noWrite(err))
			break
		}
		if err := m.reverseIntent(ctx, *entry.Intent, entry.Revision); err != nil {
			errs = append(errs, fmt.Errorf("商城扣回意图 %q: %w", entry.Intent.OrderID, err))
		}
		m.reversalCursor = *entry.Cursor
	}
	return errors.Join(errs...)
}

// contract 找回或新建订单的交付合同,并把商城报来的付款修订号记上(只增不减)。
func (m *Mall) contract(ctx context.Context, in mallIntent, paymentRevision uint32) (*tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow, error) {
	r, err := m.acceptRevised(ctx, in, paymentRevision)
	if err != nil {
		return nil, nil, err
	}
	if paymentRevision > 0 {
		if r, err = m.Engine.applyPaymentRevision(ctx, r.CommerceId, paymentRevision); err != nil {
			return nil, nil, err
		}
	}
	w, err := data.Workflow(r)
	return r, w, err
}

// receipt 把合同的结论回报商城;proof 是合同 workflow 的摘要。
func (m *Mall) receipt(ctx context.Context, path string, r *tradepb.TradeCommerceRecord, body map[string]any) error {
	sum := sha256.Sum256(r.WorkflowProto)
	body["tradeOrderId"] = strconv.FormatUint(r.CommerceId, 10)
	body["proof"] = hex.EncodeToString(sum[:])
	return m.call(ctx, "delivery", "trade", "POST", path, body, nil)
}

// deliverIntent 为一条已付款意图建(或找回)交付合同;合同已有终局时回报商城。
func (m *Mall) deliverIntent(ctx context.Context, in mallIntent, paymentRevision uint32) error {
	r, w, err := m.contract(ctx, in, paymentRevision)
	if err != nil {
		return err
	}
	state := officialDeliveryState(r, w)
	if state == "" {
		return nil
	}
	return m.receipt(ctx, "/internal/deliveries/receipt", r, map[string]any{"orderId": in.OrderID, "state": state})
}

// reverseIntent 把一张订单的发放收敛到 revision;合同已按这个修订号收敛时回报商城。
func (m *Mall) reverseIntent(ctx context.Context, in mallIntent, revision uint32) error {
	r, w, err := m.contract(ctx, in, revision)
	if err != nil {
		return err
	}
	if w.PaymentRevision != revision {
		return nil // 合同已记到更新的修订号:这条待办过时了,商城下一轮给新的
	}
	state := officialReversalState(r, w)
	if state == "" {
		return nil
	}
	return m.receipt(ctx, "/internal/reversals/receipt", r, map[string]any{"orderId": in.OrderID, "revision": revision, "state": state})
}

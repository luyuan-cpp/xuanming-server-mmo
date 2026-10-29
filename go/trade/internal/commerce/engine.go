// Package commerce 管理交易合同、托管与SQL可靠待办，不拥有任何玩家资产。
package commerce

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"math"
	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"
	"shared/assetop"
	"shared/generated/pb/table"
	"shared/roleidentity"
	"sort"
	"strconv"
	"strings"
	"time"
	"trade/internal/data"
)

const (
	Waiting       = uint32(1)
	Escrowing     = uint32(3)
	Delivering    = uint32(4)
	Delivered     = uint32(5)
	CancelPending = uint32(6)
	Cancelled     = uint32(7)
	Manual        = uint32(10)
	Settled       = uint32(11)
	BidKind       = uint32(100)
)

type Rejection uint32

func (e Rejection) Error() string { return "交易请求被拒绝" }

var (
	ErrInvalid     = Rejection(table.CommonError_kInvalidParameter)
	ErrMissing     = Rejection(table.TradeError_kTradeOrderNotFound)
	ErrConflict    = Rejection(table.TradeError_kTradeOrderStateConflict)
	ErrReplay      = Rejection(table.TradeError_kTradeRequestConflict)
	ErrSelf        = Rejection(table.TradeError_kTradeCannotTradeSelf)
	ErrNotTradable = Rejection(table.TradeError_kTradeAssetNotTradable)
	ErrBid         = Rejection(table.TradeError_kTradeBidTooLow)
	ErrEnded       = Rejection(table.TradeError_kTradeAuctionEnded)
)

type IDs interface {
	Next(context.Context) (uint64, error)
}
type Policy struct {
	Items          []uint32
	ItemTitles     map[uint32]string
	Pets           []uint32
	Currencies     []uint32
	AllowInstances bool
}

func contains(values []uint32, n uint32) bool {
	for _, v := range values {
		if v == n {
			return true
		}
	}
	return false
}
func (p Policy) Selection(v *tradepb.TradeAssetSelection) error {
	if v == nil || len(v.Items) > 16 || len(v.ItemUuids) > 8 || len(v.Items)+len(v.ItemUuids) == 0 && v.PetId == 0 && v.CurrencyAmount == 0 {
		return ErrInvalid
	}
	if v.CurrencyAmount == 0 && v.CurrencyType != 0 || v.CurrencyAmount > math.MaxInt64 {
		return ErrInvalid
	}
	if v.CurrencyAmount > 0 && (v.CurrencyType == 2 || !contains(p.Currencies, v.CurrencyType)) {
		return ErrNotTradable
	}
	var total uint64
	for id, n := range v.Items {
		if id == 0 || n == 0 || n > 4096 || !contains(p.Items, id) {
			return ErrNotTradable
		}
		total += uint64(n)
		if total > 4096 {
			return ErrInvalid
		}
	}
	seen := map[uint64]bool{}
	for _, id := range v.ItemUuids {
		if id == 0 || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	if (len(v.ItemUuids) > 0 || v.PetId != 0) && !p.AllowInstances {
		return ErrNotTradable
	}
	return nil
}
func (p Policy) Snapshot(v *assetpb.AssetSnapshot) bool {
	if v == nil {
		return true
	}
	for _, i := range v.Items {
		if !contains(p.Items, i.ConfigId) {
			return false
		}
	}
	return v.Pet == nil || contains(p.Pets, v.Pet.PetTableId)
}

type Engine struct {
	Repo    *data.CommerceRepo
	IDs     IDs
	OpIDs   IDs
	Policy  Policy
	Applier assetop.Applier
	Ledger  assetop.LedgerReader
	Now     func() time.Time
	Roles   RoleLocker
	Rmb     *RmbConsignments

	// ackAfterOpID 是收据 ACK 的翻页游标(见 ackReceipts),只归驱动 Tick 的那一个 goroutine。
	ackAfterOpID uint64
}

func (e *Engine) now() uint64 { return uint64(e.Now().UnixMilli()) }
func Bundle(v *tradepb.TradeAssetSelection) *assetpb.AssetBundle {
	b := &assetpb.AssetBundle{ItemUuids: append([]uint64(nil), v.GetItemUuids()...), PetId: v.GetPetId()}
	if v.GetCurrencyAmount() > 0 {
		b.Currencies = []*assetpb.CurrencyAmount{{CurrencyType: v.CurrencyType, Amount: v.CurrencyAmount}}
	}
	keys := make([]uint32, 0, len(v.GetItems()))
	for id := range v.GetItems() {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, id := range keys {
		b.Items = append(b.Items, &assetpb.ItemGrant{ConfigId: id, Count: v.Items[id]})
	}
	return b
}

// 返回或换主时仅补入scene提取的完整实例，不以config重新铸造装备/宝宝。
func Transfer(b *assetpb.AssetBundle, s *assetpb.AssetSnapshot) (*assetpb.AssetBundle, error) {
	if b == nil || len(b.ItemInstances) > 0 || b.PetInstance != nil {
		return nil, errors.New("托管请求包含非扣出字段")
	}
	out := proto.Clone(b).(*assetpb.AssetBundle)
	if (len(b.ItemUuids) > 0 || b.PetId > 0) && s == nil {
		return nil, errors.New("托管完整快照缺失")
	}
	if s != nil {
		out.ItemInstances = append(out.ItemInstances, s.Items...)
		out.PetInstance = s.Pet
	}
	if len(out.ItemUuids) != len(out.ItemInstances) || (out.PetId != 0) != (out.PetInstance != nil) {
		return nil, errors.New("托管快照与合同不符")
	}
	wanted := make(map[uint64]bool, len(b.ItemUuids))
	for _, id := range b.ItemUuids {
		if id == 0 || wanted[id] {
			return nil, errors.New("托管实例选择损坏")
		}
		wanted[id] = true
	}
	for _, item := range out.ItemInstances {
		if item == nil || !wanted[item.ItemUuid] {
			return nil, errors.New("托管物品身份与合同不符")
		}
		delete(wanted, item.ItemUuid)
	}
	if len(wanted) > 0 || out.PetInstance != nil && out.PetInstance.PetId != b.PetId {
		return nil, errors.New("托管实例身份与合同不符")
	}
	// 扣出用GUID选择，入账只用可信完整原件；两种资产命令不能混带字段。
	out.ItemUuids = nil
	out.PetId = 0
	return out, nil
}
func RequestKey(scope string, player uint64, id string) string {
	h := sha256.Sum256([]byte(scope + ":" + strconv.FormatUint(player, 10) + ":" + id))
	return hex.EncodeToString(h[:])
}
func digest(ctx context.Context, v proto.Message) []byte {
	raw, _ := proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if original, ok := roleidentity.Original(ctx); ok {
		identity, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&tradepb.TradeRoleIdentity{Account: original.Account, OwnerRevision: original.OwnerRevision})
		// 长度前缀隔开原客户端正文与原身份，防止拼接歧义；不含传输时钟/nonce。
		bound := binary.AppendUvarint([]byte("trade-request-v2\n"), uint64(len(raw)))
		bound = append(bound, raw...)
		raw = append(bound, identity...)
	}
	h := sha256.Sum256(raw)
	return h[:]
}

// priorRequest 只接受同一玩家、同一请求号与完整原载荷的重放；已有合同不再依赖当前拍品等易变状态。
func (e *Engine) priorRequest(ctx context.Context, player uint64, id string, input proto.Message) (*tradepb.TradeCommerceRecord, error) {
	if e == nil || e.Repo == nil || e.Repo.Ops == nil {
		return nil, errors.New("交易资产通道未配置")
	}
	if player == 0 || len(id) < 8 || len(id) > 80 {
		return nil, ErrInvalid
	}
	if old, err := e.Repo.ByRequest(ctx, RequestKey("player", player, id)); err == nil {
		if !bytes.Equal(old.RequestDigest, digest(ctx, input)) {
			return nil, ErrReplay
		}
		if err := e.verifyReplayActor(ctx, old); err != nil {
			return nil, err
		}
		return old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return nil, nil
}
func (e *Engine) Create(ctx context.Context, player uint64, id string, input proto.Message, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) (*tradepb.TradeCommerceRecord, error) {
	if old, err := e.priorRequest(ctx, player, id, input); err != nil || old != nil {
		return old, err
	}
	r.RequestKey = RequestKey("player", player, id)
	r.RequestDigest = digest(ctx, input)
	n, err := e.IDs.Next(ctx)
	if err != nil {
		return nil, err
	}
	r.CommerceId = n
	r.CreatedMs = e.now()
	r.UpdatedMs = r.CreatedMs
	r.Version = 1
	r.WorkflowProto, err = proto.MarshalOptions{Deterministic: true}.Marshal(w)
	if err != nil {
		return nil, err
	}
	err = e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		if err := e.lockRoles(ctx, tx, 0, r.SellerPlayerId, r.BuyerPlayerId); err != nil {
			return err
		}
		if r.Kind == 6 {
			if w.Rmb == nil {
				return ErrInvalid
			}
			owned, err := e.Roles.Ownership(ctx, tx, r.SellerPlayerId)
			if err != nil {
				return err
			}
			if owned.Account != w.Rmb.SellerAccount || owned.OwnerRevision != w.Rmb.SellerOwnerRevision || owned.TradeListingId != 0 {
				return ErrConflict
			}
		}
		for _, party := range []struct {
			player uint64
			target **tradepb.TradeRoleIdentity
		}{{r.SellerPlayerId, &w.SellerIdentity}, {r.BuyerPlayerId, &w.BuyerIdentity}} {
			if party.player == 0 {
				continue
			}
			owned, err := e.Roles.Ownership(ctx, tx, party.player)
			if err != nil {
				return err
			}
			*party.target = &tradepb.TradeRoleIdentity{Account: owned.Account, OwnerRevision: owned.OwnerRevision}
		}
		r.WorkflowProto, err = proto.MarshalOptions{Deterministic: true}.Marshal(w)
		if err != nil {
			return err
		}
		return e.Repo.Insert(ctx, tx, r)
	})
	if err != nil {
		if old, readErr := e.Repo.ByRequest(ctx, r.RequestKey); readErr == nil {
			if !bytes.Equal(old.RequestDigest, r.RequestDigest) {
				return nil, ErrReplay
			}
			if err := e.verifyReplayActor(ctx, old); err != nil {
				return nil, err
			}
			return old, nil
		}
		return nil, err
	}
	return r, nil
}
func (e *Engine) CreatePlayerTrade(ctx context.Context, me uint64, in *tradepb.CreatePlayerTradeRequest) (*tradepb.TradeCommerceRecord, error) {
	if in.GetBuyerPlayerId() == 0 {
		return nil, ErrInvalid
	}
	if in.BuyerPlayerId == me {
		return nil, ErrSelf
	}
	if err := e.Policy.Selection(in.SellerAssets); err != nil {
		return nil, err
	}
	if err := e.Policy.Selection(in.BuyerAssets); err != nil {
		return nil, err
	}
	return e.Create(ctx, me, in.ClientRequestId, in, &tradepb.TradeCommerceRecord{Kind: 2, Status: Escrowing, SellerPlayerId: me, BuyerPlayerId: in.BuyerPlayerId, ExpiresMs: e.now() + 15*60*1000}, &tradepb.TradeCommerceWorkflow{SellerAssets: in.SellerAssets, BuyerAssets: in.BuyerAssets, Stage: "offer_escrow"})
}
func validPrice(p uint64, c uint32, policy Policy) bool {
	return p > 0 && p <= math.MaxInt64 && c != 2 && contains(policy.Currencies, c)
}
func (e *Engine) CreateConsignment(ctx context.Context, me uint64, in *tradepb.CreateConsignmentRequest) (*tradepb.TradeCommerceRecord, error) {
	if err := e.Policy.Selection(in.GetAssets()); err != nil {
		return nil, err
	}
	if !validPrice(in.Price, in.CurrencyType, e.Policy) || in.DurationMs < 60000 || in.DurationMs > 30*24*3600000 {
		return nil, ErrInvalid
	}
	return e.Create(ctx, me, in.ClientRequestId, in, &tradepb.TradeCommerceRecord{Kind: 3, Status: Escrowing, SellerPlayerId: me, UnitPrice: in.Price, CurrencyType: in.CurrencyType, ExpiresMs: e.now() + in.DurationMs}, &tradepb.TradeCommerceWorkflow{SellerAssets: in.Assets, Stage: "escrow"})
}
func (e *Engine) CreateLot(ctx context.Context, me uint64, in *tradepb.CreateAuctionLotRequest) (*tradepb.TradeCommerceRecord, error) {
	if err := e.Policy.Selection(in.GetAssets()); err != nil {
		return nil, err
	}
	if !validPrice(in.OpeningPrice, in.CurrencyType, e.Policy) || in.MinimumIncrement == 0 || in.MinimumIncrement > math.MaxInt64 || in.DurationMs < 60000 || in.DurationMs > 7*24*3600000 {
		return nil, ErrInvalid
	}
	return e.Create(ctx, me, in.ClientRequestId, in, &tradepb.TradeCommerceRecord{Kind: 5, Status: Escrowing, SellerPlayerId: me, UnitPrice: in.OpeningPrice, CurrencyType: in.CurrencyType, ExpiresMs: e.now() + in.DurationMs}, &tradepb.TradeCommerceWorkflow{SellerAssets: in.Assets, Stage: "escrow", MinimumIncrement: in.MinimumIncrement})
}
func (e *Engine) PlaceMarket(ctx context.Context, me uint64, in *tradepb.PlaceMarketOrderRequest) (*tradepb.TradeCommerceRecord, error) {
	if in.Quantity == 0 || in.Quantity > 4096 || in.MarketId != 1 || len(e.Policy.Currencies) == 0 || in.CurrencyType != e.Policy.Currencies[0] || !contains(e.Policy.Items, in.ItemConfigId) || !validPrice(in.UnitPrice, in.CurrencyType, e.Policy) || in.UnitPrice > math.MaxInt64/uint64(in.Quantity) || (in.Side != 1 && in.Side != 2) {
		return nil, ErrInvalid
	}
	sel := &tradepb.TradeAssetSelection{}
	if in.Side == 1 {
		sel.CurrencyType = in.CurrencyType
		sel.CurrencyAmount = in.UnitPrice * uint64(in.Quantity)
	} else {
		sel.Items = map[uint32]uint32{in.ItemConfigId: in.Quantity}
	}
	return e.Create(ctx, me, in.ClientRequestId, in, &tradepb.TradeCommerceRecord{Kind: 4, Status: Escrowing, SellerPlayerId: me, MarketId: in.MarketId, ItemConfigId: in.ItemConfigId, Side: uint32(in.Side), UnitPrice: in.UnitPrice, Quantity: in.Quantity, CurrencyType: in.CurrencyType}, &tradepb.TradeCommerceWorkflow{SellerAssets: sel, Stage: "escrow"})
}
func (e *Engine) Mutate(ctx context.Context, me, id uint64, action string, requestID ...string) (*tradepb.TradeCommerceRecord, error) {
	var result *tradepb.TradeCommerceRecord
	before, err := e.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	err = e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		if err := e.lockRoles(ctx, tx, 0, me, before.SellerPlayerId, before.BuyerPlayerId); err != nil {
			return err
		}
		r, err := e.Repo.Lock(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		w, err := data.Workflow(r)
		if err != nil {
			return err
		}
		if r.SellerPlayerId != before.SellerPlayerId || r.BuyerPlayerId != before.BuyerPlayerId {
			return ErrConflict
		}
		if err := e.checkContractIdentities(ctx, tx, r, w); err != nil {
			return err
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now); err != nil {
			return err
		}
		switch action {
		case "confirm":
			if r.Kind != 2 || w.Stage != "confirm" {
				return ErrConflict
			}
			if r.ExpiresMs <= now || w.CancelRequested {
				return ErrConflict
			}
			if me == r.SellerPlayerId {
				if !w.BuyerConfirmed || !applied(leg(w, "buyer_debit")) {
					return ErrConflict
				}
				w.SellerConfirmed = true
			} else if me == r.BuyerPlayerId {
				if !applied(leg(w, "seller_debit")) {
					return ErrConflict
				}
				w.BuyerConfirmed = true
				if !applied(leg(w, "buyer_debit")) {
					w.Stage = "buyer_escrow"
					r.Status = Escrowing
				}
			} else {
				return ErrMissing
			}
			if w.SellerConfirmed && w.BuyerConfirmed {
				w.Stage = "escrow"
				r.Status = Escrowing
			}
		case "buy":
			if len(requestID) != 1 || len(requestID[0]) < 8 || len(requestID[0]) > 80 {
				return ErrInvalid
			}
			key := RequestKey("consignment_buy", me, requestID[0])
			for _, l := range w.Legs {
				if strings.HasSuffix(l.Role, "_"+key) {
					if r.BuyerPlayerId == me && (w.Stage == "pay" || w.Stage == "deliver" || w.Stage == "done") {
						result = r
						return nil
					}
					return ErrConflict
				}
			}
			if r.Kind == 3 && r.BuyerPlayerId == me && (w.Stage == "pay" || w.Stage == "deliver" || w.Stage == "done") {
				result = r
				return nil
			}
			if r.Kind != 3 || w.Stage != "open" || r.ExpiresMs <= now || r.BuyerPlayerId != 0 {
				return ErrConflict
			}
			if me == r.SellerPlayerId {
				return ErrSelf
			}
			if w.QuantityReserved >= 32 {
				return ErrConflict
			}
			r.BuyerPlayerId = me
			owned, err := e.Roles.Ownership(ctx, tx, me)
			if err != nil {
				return err
			}
			w.BuyerIdentity = &tradepb.TradeRoleIdentity{Account: owned.Account, OwnerRevision: owned.OwnerRevision}
			w.BuyerAssets = &tradepb.TradeAssetSelection{CurrencyType: r.CurrencyType, CurrencyAmount: r.UnitPrice}
			w.QuantityReserved++
			addLeg(w, "buyer_debit_"+strconv.FormatUint(w.QuantityReserved, 10)+"_"+key, me, 3, Bundle(w.BuyerAssets))
			w.Stage = "pay"
			r.Status = Escrowing
		case "cancel":
			if me != r.SellerPlayerId && me != r.BuyerPlayerId {
				return ErrMissing
			}
			if r.Status == Cancelled {
				result = r
				return nil
			}
			if r.Kind == 2 && (w.Stage == "confirm" || w.Stage == "offer_escrow" || w.Stage == "buyer_escrow") {
				w.CancelRequested = true
				r.Status = CancelPending
			} else if me == r.SellerPlayerId && (w.Stage == "open" || w.Stage == "market_open" || w.Stage == "lot_open" && w.HighestBidder == 0) {
				w.CancelRequested = true
				r.Status = CancelPending
			} else {
				return ErrConflict
			}
			// 取消不删原扣款待办；截止后由同一seq的Abort固定结局，已经扣出的仍原样退还。
			// 逐行主键点锁 + 点改(不按 ref 批量 UPDATE),锁序与终结同向,见 ExpireCommerceEscrowDebitsTx。
			if err := e.Repo.Ops.ExpireCommerceEscrowDebitsTx(ctx, tx, r.CommerceId, now); err != nil {
				return err
			}
		default:
			return ErrInvalid
		}
		if err := e.Repo.Save(ctx, tx, r, w, e.now()); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}
func (e *Engine) Get(ctx context.Context, me, id uint64) (*tradepb.TradeCommerceRecord, error) {
	r, err := e.Repo.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, err
	}
	if r.SellerPlayerId != me && r.BuyerPlayerId != me && r.Kind != 3 && r.Kind != 5 {
		return nil, ErrMissing
	}
	if _, client := roleidentity.Original(ctx); client && r.Kind != 3 && r.Kind != 5 {
		w, err := data.Workflow(r)
		if err != nil {
			return nil, err
		}
		if !OwnsRecord(ctx, r, w) {
			return nil, ErrMissing
		}
	}
	return r, nil
}

func addLeg(w *tradepb.TradeCommerceWorkflow, role string, player uint64, stream uint32, bundle *assetpb.AssetBundle) {
	for _, l := range w.Legs {
		if l.Role == role {
			return
		}
	}
	w.Legs = append(w.Legs, &tradepb.TradeCommerceLeg{Role: role, PlayerId: player, Stream: stream, Bundle: bundle})
}
func leg(w *tradepb.TradeCommerceWorkflow, role string) *tradepb.TradeCommerceLeg {
	for _, l := range w.Legs {
		if l.Role == role {
			return l
		}
	}
	return nil
}
func transferLeg(w *tradepb.TradeCommerceWorkflow, role string, to uint64, from *tradepb.TradeCommerceLeg) error {
	b, err := Transfer(from.Bundle, from.Snapshot)
	if err != nil {
		return err
	}
	addLeg(w, role, to, 4, b)
	return nil
}
func done(l *tradepb.TradeCommerceLeg) bool    { return l != nil && l.Outcome != 0 }
func applied(l *tradepb.TradeCommerceLeg) bool { return l != nil && l.Outcome == 2 }
func finalCredits(w *tradepb.TradeCommerceWorkflow) (bool, bool) {
	for _, l := range w.Legs {
		if l.Stream == 4 {
			if !done(l) {
				return false, false
			}
			if !applied(l) {
				return true, false
			}
		}
	}
	return true, true
}

// plan只决定下一批幂等腿；副作用仅为同一SQL事务写outbox，RPC在提交后由既有Loop发送。
func (e *Engine) plan(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, now uint64) error {
	if r.Kind == 6 {
		return e.planRmb(r, w, now)
	}
	if w.Stage == "official_deliver" {
		addLeg(w, "official_credit", r.BuyerPlayerId, 4, w.OfficialBundle)
		l := leg(w, "official_credit")
		if !done(l) {
			return nil
		}
		if applied(l) {
			w.Stage = "done"
			r.Status = Settled
		} else {
			w.Stage = "official_refund"
			r.Status = 8
		}
		return nil
	}
	if w.Stage == "offer_escrow" {
		addLeg(w, "seller_debit", r.SellerPlayerId, 3, Bundle(w.SellerAssets))
		a := leg(w, "seller_debit")
		if !done(a) {
			return nil
		}
		if !applied(a) {
			w.Stage = "done"
			r.Status = Cancelled
			return nil
		}
		if !e.Policy.Snapshot(a.Snapshot) {
			w.CancelRequested = true
		}
		w.Stage = "confirm"
		r.Status = Waiting
	}
	if w.Stage == "buyer_escrow" {
		addLeg(w, "buyer_debit", r.BuyerPlayerId, 3, Bundle(w.BuyerAssets))
		b := leg(w, "buyer_debit")
		if !done(b) {
			return nil
		}
		if !applied(b) || !e.Policy.Snapshot(b.Snapshot) {
			w.CancelRequested = true
		}
		w.Stage = "confirm"
		r.Status = Waiting
	}
	if r.Kind == 2 && w.Stage == "confirm" && (w.CancelRequested || r.ExpiresMs <= now) {
		w.Stage = "return"
		r.Status = CancelPending
		if applied(leg(w, "seller_debit")) {
			if err := transferLeg(w, "seller_return", r.SellerPlayerId, leg(w, "seller_debit")); err != nil {
				return err
			}
		}
		if applied(leg(w, "buyer_debit")) {
			if err := transferLeg(w, "buyer_return", r.BuyerPlayerId, leg(w, "buyer_debit")); err != nil {
				return err
			}
		}
	}
	if w.Stage == "bid_freeze" {
		addLeg(w, "bid_debit", r.SellerPlayerId, 3, Bundle(w.SellerAssets))
		b := leg(w, "bid_debit")
		if done(b) {
			if applied(b) {
				w.Stage = "bid_ready"
			} else {
				w.Stage = "done"
				r.Status = Cancelled
			}
		}
	}
	if w.Stage == "bid_refund" {
		b := leg(w, "bid_debit")
		if !applied(b) {
			return errors.New("竞价退款缺已提交扣款")
		}
		if err := transferLeg(w, "bid_return", r.SellerPlayerId, b); err != nil {
			return err
		}
		w.Stage = "return"
		r.Status = CancelPending
	}
	if w.Stage == "escrow" {
		addLeg(w, "seller_debit", r.SellerPlayerId, 3, Bundle(w.SellerAssets))
		if r.Kind == 2 {
			addLeg(w, "buyer_debit", r.BuyerPlayerId, 3, Bundle(w.BuyerAssets))
		}
		a, b := leg(w, "seller_debit"), leg(w, "buyer_debit")
		if !done(a) || r.Kind == 2 && !done(b) {
			return nil
		}
		if !applied(a) || r.Kind == 2 && !applied(b) || applied(a) && !e.Policy.Snapshot(a.Snapshot) || applied(b) && !e.Policy.Snapshot(b.Snapshot) {
			w.Stage = "return"
			r.Status = CancelPending
			if applied(a) {
				if err := transferLeg(w, "seller_return", r.SellerPlayerId, a); err != nil {
					return err
				}
			}
			if applied(b) {
				if err := transferLeg(w, "buyer_return", r.BuyerPlayerId, b); err != nil {
					return err
				}
			}
		} else if r.Kind == 2 {
			w.Stage = "deliver"
			r.Status = Delivering
			if err := transferLeg(w, "seller_receive", r.SellerPlayerId, b); err != nil {
				return err
			}
			if err := transferLeg(w, "buyer_receive", r.BuyerPlayerId, a); err != nil {
				return err
			}
		} else if r.Kind == 4 {
			w.Stage = "market_open"
		} else if r.Kind == 5 {
			w.Stage = "lot_open"
		} else {
			w.Stage = "open"
		}
	}
	if w.Stage == "pay" {
		name := "buyer_debit_" + strconv.FormatUint(w.QuantityReserved, 10)
		b := leg(w, name)
		for _, l := range w.Legs {
			if strings.HasPrefix(l.Role, name+"_") {
				b = l
				break
			}
		}
		if b == nil {
			addLeg(w, name, r.BuyerPlayerId, 3, Bundle(w.BuyerAssets))
			b = leg(w, name)
		}
		if !done(b) {
			return nil
		}
		if !applied(b) {
			w.Stage = "open"
			r.BuyerPlayerId = 0
		} else {
			w.Stage = "deliver"
			r.Status = Delivering
			if err := transferLeg(w, "buyer_receive", r.BuyerPlayerId, leg(w, "seller_debit")); err != nil {
				return err
			}
			if err := transferLeg(w, "seller_receive", r.SellerPlayerId, b); err != nil {
				return err
			}
		}
	}
	if w.Stage == "lot_open" && r.ExpiresMs <= now && w.HighestBidder != 0 {
		w.Stage = "deliver"
		r.Status = Delivering
		if err := transferLeg(w, "buyer_receive", w.HighestBidder, leg(w, "seller_debit")); err != nil {
			return err
		}
		addLeg(w, "seller_receive", r.SellerPlayerId, 4, Bundle(w.BuyerAssets))
	}
	if (w.Stage == "open" || w.Stage == "lot_open") && r.ExpiresMs <= now {
		w.CancelRequested = true
	}
	if w.CancelRequested && (w.Stage == "open" || w.Stage == "lot_open" && w.HighestBidder == 0 || w.Stage == "market_open") {
		w.Stage = "return"
		r.Status = CancelPending
		b, err := Transfer(leg(w, "seller_debit").Bundle, leg(w, "seller_debit").Snapshot)
		if err != nil {
			return err
		}
		if r.Kind == 4 {
			remaining := r.Quantity - r.FilledQuantity
			if remaining == 0 {
				b = &assetpb.AssetBundle{}
			} else if r.Side == 1 {
				b = &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: r.CurrencyType, Amount: uint64(remaining) * r.UnitPrice}}}
			} else {
				b = &assetpb.AssetBundle{Items: []*assetpb.ItemGrant{{ConfigId: r.ItemConfigId, Count: remaining}}}
			}
		}
		if len(b.Currencies)+len(b.Items)+len(b.ItemInstances) > 0 || b.PetInstance != nil {
			addLeg(w, "seller_return", r.SellerPlayerId, 4, b)
		}
	}
	if w.Stage == "deliver" || w.Stage == "return" {
		ready, ok := finalCredits(w)
		if ready {
			w.Stage = "done"
			if !ok {
				r.Status = Manual
			} else if r.Status == CancelPending {
				r.Status = Cancelled
			} else {
				r.Status = Settled
			}
		}
	}
	return nil
}

// advance 推进一条合同。合同自身的持久数据坏了(解不开、腿缺 outbox 行、腿状态非法、plan 判错)一律用 corrupt 包装:
// 那是重试只会每轮撞同一个错的确定性失败,由 Tick 隔离为 Manual(deferCommerce),不再按暂时失败无限重试。
func (e *Engine) advance(ctx context.Context, old *tradepb.TradeCommerceRecord) error {
	w, err := data.Workflow(old)
	if err != nil {
		return corrupt(err)
	}
	keys := []data.SeqKey{}
	for _, player := range []uint64{old.SellerPlayerId, old.BuyerPlayerId, w.HighestBidder} {
		if player > 0 {
			keys = append(keys, data.SeqKey{PlayerID: player, Stream: 3}, data.SeqKey{PlayerID: player, Stream: 4})
		}
	}
	if err := e.Repo.Ops.EnsureSeqRows(ctx, e.now(), keys...); err != nil {
		return err
	}
	ids := make([]uint64, 8)
	for i := range ids {
		ids[i], err = e.OpIDs.Next(ctx)
		if err != nil {
			return err
		}
	}
	return e.Repo.Tx(ctx, func(tx *sql.Tx) error {
		if err := e.lockRoles(ctx, tx, 0, old.SellerPlayerId, old.BuyerPlayerId, w.HighestBidder); err != nil {
			return err
		}
		r, err := e.Repo.Lock(ctx, tx, old.CommerceId)
		if err != nil {
			return err
		}
		w, err := data.Workflow(r)
		if err != nil {
			return corrupt(err)
		}
		if r.SellerPlayerId != old.SellerPlayerId || r.BuyerPlayerId != old.BuyerPlayerId {
			// Due 之后有人买下 / 换了参与者:角色锁是按旧参与者拿的,不能在这把锁下推进。下一轮按新行重来。
			return staleObservation()
		}
		if err := e.checkContractIdentities(ctx, tx, r, w); err != nil {
			return err
		}
		for _, l := range w.Legs {
			if l.OpId == 0 || l.Outcome != 0 {
				continue
			}
			out, err := e.Repo.LegResult(ctx, tx, l.OpId)
			if errors.Is(err, sql.ErrNoRows) {
				// 腿已发出却查不到 outbox 行:结局证据丢了,不能凭空判定。清理只删已业务消费(合同结案)的行,走到这里是数据事故。
				return corrupt(fmt.Errorf("腿 %s op_id=%d 的资产待办行不存在", l.Role, l.OpId))
			}
			if err != nil {
				return err
			}
			if out.Pending() {
				continue
			}
			if !out.Final() {
				return corrupt(fmt.Errorf("腿 %s op_id=%d 的资产待办状态非法 (%d)", l.Role, l.OpId, int32(out.Status)))
			}
			// 终局不分来源一律推进(2026-10-20,第一轮审计 §2.5 go-asset-services#1,见 data.LegOutcome.Final):
			// 自动放弃(6)与人工判定的 REJECTED / ABORTED 都是"没扣到",由 plan 走与 scene 拒绝同一条补偿路径
			// (退回另一方已托管的资产、寄售重新开放);腿的 Outcome 只记一次,补偿腿按角色名只建一次,不会退两次。
			if out.Status == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL {
				// 与 quarantineLockedTx 同理:转人工之前截止合同名下仍在途的托管扣款,不再把另一方的资产扣进人工单(2026-10-22)。
				if err := e.Repo.Ops.ExpireCommerceEscrowDebitsTx(ctx, tx, r.CommerceId, e.now()); err != nil {
					return err
				}
				r.Status = Manual
				w.Stage = "manual"
				return e.Repo.Save(ctx, tx, r, w, e.now())
			}
			l.Outcome = uint32(out.Status)
			if len(out.Snapshot) > 0 {
				l.Snapshot = &assetpb.AssetSnapshot{}
				if err := proto.Unmarshal(out.Snapshot, l.Snapshot); err != nil {
					return corrupt(fmt.Errorf("腿 %s op_id=%d 的托管快照损坏: %w", l.Role, l.OpId, err))
				}
			}
			// 已扣出的托管迟早要原样转给对方或退回(Transfer),在记下结局的这一刻就核对它转得动。
			// 典型是人工终结为 APPLIED 的实例扣款:ResolveManually 不写 escrow_snapshot,自动路径缺原件则根本不终结
			// (assetop escrowAwaitsReceipt)。若不在这里拦,寄售 / 拍品会照常上架,等买家付了款才在发货时判错,
			// 把第二个玩家的钱也一起压进人工单;在这里拦则在别人付款之前就隔离。隔离时合同名下仍在途的托管扣款
			// 一并截止(quarantineLockedTx),已经扣到的留证据、没扣到的不再扣(2026-10-22 起;此前只改合同行,在途扣款照常落地)。
			if l.Stream == 3 && applied(l) {
				if _, err := Transfer(l.Bundle, l.Snapshot); err != nil {
					return corrupt(fmt.Errorf("腿 %s op_id=%d 已扣出但托管原件不可转交(%s 终局): %w", l.Role, l.OpId, out.Source(), err))
				}
			}
			logOffbandLegOutcome(r.CommerceId, l, out)
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now); err != nil {
			return err
		}
		if err := e.plan(r, w, now); err != nil {
			// plan 是 (合同, 腿结局, 时刻) 的纯函数,它判错只可能是持久数据自相矛盾(托管快照缺失 / 与合同不符等)。
			return corrupt(err)
		}
		if err := e.marketBarrier(ctx, tx, r, w); err != nil {
			return err
		}
		pending := []*tradepb.TradeCommerceLeg{}
		for _, l := range w.Legs {
			if l.OpId == 0 {
				pending = append(pending, l)
			}
		}
		sort.Slice(pending, func(i, j int) bool {
			if pending[i].PlayerId != pending[j].PlayerId {
				return pending[i].PlayerId < pending[j].PlayerId
			}
			if pending[i].Stream != pending[j].Stream {
				return pending[i].Stream < pending[j].Stream
			}
			return pending[i].Role < pending[j].Role
		})
		for i, l := range pending {
			if i >= len(ids) {
				return corrupt(errors.New("交易单步腿数超过8"))
			}
			a, err := e.Repo.Ops.AllocateSeqsTx(ctx, tx, e.now(), data.SeqKey{PlayerID: l.PlayerId, Stream: assetpb.AssetOpStream(l.Stream)})
			if err != nil {
				return err
			}
			n := a[data.SeqKey{PlayerID: l.PlayerId, Stream: assetpb.AssetOpStream(l.Stream)}]
			raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(l.Bundle)
			if err != nil {
				return err
			}
			kind := tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_DELIVER_CREDIT
			if l.Stream == 3 {
				kind = tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT
			}
			txType := uint32(rollbackpb.TransactionType_TX_AUCTION_BUY)
			if l.Stream == 3 {
				txType = uint32(rollbackpb.TransactionType_TX_AUCTION_SELL)
			}
			l.OpId = ids[i]
			deadline := uint64(0)
			if l.Stream == 3 {
				deadline = r.ExpiresMs
				if w.CancelRequested {
					deadline = now
				}
			}
			if err := e.Repo.Ops.InsertOp(ctx, tx, &tradepb.TradeAssetOpRecord{OpId: l.OpId, PlayerId: l.PlayerId, Stream: l.Stream, StreamEpoch: n.Epoch, Seq: n.Seq, Kind: kind, Status: 1, NextAttemptMs: e.now(), DeadlineMs: deadline, TxType: txType, RefKind: 2, RefId: r.CommerceId, Payload: raw, CreatedMs: e.now(), UpdatedMs: e.now()}); err != nil {
				return err
			}
		}
		if w.Stage == "done" || w.Stage == "rmb_delivered" || w.Stage == "rmb_returned" {
			if err := e.Repo.Ops.MarkCommerceOpsConsumedTx(ctx, tx, r.CommerceId); err != nil {
				return err
			}
		}
		return e.Repo.Save(ctx, tx, r, w, e.now())
	})
}

// Tick 推进一批到期合同,再轮转发一页收据 ACK。由 svc 的单个 goroutine(trade.commerce.reconcile)每秒调用一次;
// **不得并发调用同一个 Engine**:ACK 翻页游标 ackAfterOpID 只归这一个 goroutine。
//
// 单行失败不再整轮返回(2026-10-20,第一轮审计 §2.5 go-asset-services#0;实现见 tick_isolation.go):
//   - 每行的子预算从父 ctx 截止时刻推出来(rowBudgetFor,2026-10-22),一定早于整轮截止结束:
//     卡住的行先撞上自己的子预算,父 ctx 还来得及推迟它,同一轮后面的行与收据 ACK 也各有余量;
//   - 暂时失败(存储超时、ErrTooManyPending、角色冲突等)按 tickFailureBackoff 推迟这一行的下次到期,本轮其余行照常推进;
//     乐观冲突不推迟,下一轮立即重试;
//   - 确定性坏数据(corrupt)隔离为 Manual,移出 Due,等人工处理,不再无限重试。
//
// 剩余预算只够收据 ACK 时停止开新行(计 trade_commerce_tick_truncated_total),没轮到的行仍到期,下一轮或别的副本接着推进。
// 只有 Due / AckDue 查询本身失败(存储级)、收据 ACK 依赖失败,或整轮被取消(关停)时才返回错误;取消时不推迟任何行。
func (e *Engine) Tick(ctx context.Context) error {
	rows, err := e.Repo.Due(ctx, e.now(), tickDueLimit)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		budget, ok := rowBudgetFor(ctx, time.Now())
		if !ok {
			tickTruncatedTotal.Inc()
			break
		}
		rowCtx, cancel := context.WithTimeout(ctx, budget)
		err := e.step(rowCtx, r)
		cancel()
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			// 子预算早于父截止时刻结束,走到这里只可能是整轮被取消(关停):不是这一行的错,不推迟。
			return ctx.Err()
		}
		e.deferCommerce(ctx, r, err)
	}
	return e.ackReceipts(ctx)
}

package commerce

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"trade/internal/data"

	"google.golang.org/protobuf/proto"
)

type RoleTradeAuthority interface {
	accountdirectory.RoleTrades
	Lookup(context.Context, uint64) (*dspb.AccountRoleOwnership, error)
}
type RoleSealer interface {
	Seal(context.Context, *base.RoleTradeSeal) error
}

// RmbConsignments 复用commerce/outbox。只有后台单worker持有分页水位；公共方法不写这些水位。
// 外部请求全部在SQL事务之外；未知提交只重放持久原件，不换listing/transfer tx/资产seq。
type RmbConsignments struct {
	Engine                       *Engine
	Mall                         *Mall
	Directory                    RoleTradeAuthority
	Sealer                       RoleSealer
	CurrencyTypes                []uint32
	deliveryCursor, returnCursor string
	sealAfter                    map[uint64]int
}

func (s *RmbConsignments) Create(ctx context.Context, account string, player, revision uint64, zone uint32, in *tradepb.CreateRmbConsignmentRequest) (*tradepb.TradeCommerceRecord, error) {
	if s == nil || s.Engine == nil || s.Mall == nil || s.Directory == nil || s.Sealer == nil {
		return nil, errors.New("人民币寄售依赖未装配")
	}
	if !accountdirectory.ValidAccount(account) || player == 0 || revision == 0 || zone == 0 || in == nil || in.PriceFen == 0 || in.PriceFen > 9_000_000_000_000_000 || !utf8.ValidString(in.Title) || strings.TrimSpace(in.Title) == "" || len(utf16.Encode([]rune(in.Title))) > 128 {
		return nil, ErrInvalid
	}
	kind, err := s.selection(in)
	if err != nil {
		return nil, err
	}
	terms := &tradepb.TradeRmbConsignmentWorkflow{SellerAccount: account, SellerOwnerRevision: revision, CharacterPlayerId: in.CharacterPlayerId, Title: in.Title, AssetKind: kind, MarketZone: zone, PriceFen: in.PriceFen, ClientRequestId: in.ClientRequestId}
	stage := "rmb_escrow"
	if kind == "CHARACTER" {
		stage = "rmb_freezing"
		if player != in.CharacterPlayerId {
			return nil, ErrInvalid
		}
	}
	w := &tradepb.TradeCommerceWorkflow{SellerAssets: in.Assets, Rmb: terms, Stage: stage}
	// 输入摘要包含原客户端全部选择与可信账号/修订；卖家角色易主后不能用旧requestID借新主身份重放。
	input := proto.Clone(w).(*tradepb.TradeCommerceWorkflow)
	return s.Engine.Create(ctx, player, in.ClientRequestId, input, &tradepb.TradeCommerceRecord{Kind: 6, Status: Escrowing, SellerPlayerId: player, UnitPrice: in.PriceFen, ExpiresMs: s.Engine.now() + 15*60*1000}, w)
}
func (s *RmbConsignments) selection(in *tradepb.CreateRmbConsignmentRequest) (string, error) {
	v := in.Assets
	item := len(v.GetItems())+len(v.GetItemUuids()) > 0
	pet := v.GetPetId() != 0
	currency := v.GetCurrencyAmount() != 0
	count := 0
	for _, present := range []bool{item, pet, currency} {
		if present {
			count++
		}
	}
	if in.CharacterPlayerId != 0 {
		if count != 0 || v.GetCurrencyType() != 0 {
			return "", ErrInvalid
		}
		return "CHARACTER", nil
	}
	if count != 1 {
		return "", ErrInvalid
	}
	policy := s.Engine.Policy
	policy.Currencies = s.CurrencyTypes
	if len(policy.Currencies) == 0 {
		policy.Currencies = []uint32{0}
	}
	if v.GetCurrencyType() == 2 {
		return "", ErrNotTradable
	}
	if err := policy.Selection(v); err != nil {
		return "", err
	}
	if item {
		return "ITEMS", nil
	}
	if pet {
		return "PET", nil
	}
	return "CURRENCY", nil
}
func (s *RmbConsignments) save(ctx context.Context, old *tradepb.TradeCommerceRecord, body func(*sql.Tx, *tradepb.TradeCommerceRecord, *tradepb.TradeCommerceWorkflow) error) error {
	return s.Engine.Repo.Tx(ctx, func(tx *sql.Tx) error {
		// CHARACTER本listing已经拥有交易锁；其它角色/合同不获得这份例外。
		if err := s.Engine.lockRoles(ctx, tx, old.CommerceId, old.SellerPlayerId, old.BuyerPlayerId); err != nil {
			return err
		}
		r, err := s.Engine.Repo.Lock(ctx, tx, old.CommerceId)
		if err != nil {
			return err
		}
		if r.Version != old.Version {
			return staleObservation() // 读到之后被别的写者推进过:下一轮按新行重来,不推迟
		}
		w, err := data.Workflow(r)
		if err != nil {
			return corrupt(err)
		}
		if w.Rmb == nil {
			return corrupt(errors.New("人民币合同缺少条款"))
		}
		if err := body(tx, r, w); err != nil {
			return err
		}
		return s.Engine.Repo.Save(ctx, tx, r, w, s.Engine.now())
	})
}
func (s *RmbConsignments) Cancel(ctx context.Context, account string, id uint64) error {
	r, err := s.Engine.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	w, err := data.Workflow(r)
	if err != nil {
		return err
	}
	if r.Kind != 6 || w.Rmb == nil || w.Rmb.SellerAccount != account {
		return ErrMissing
	}
	if r.Status == Settled || w.Stage == "rmb_delivery" {
		return ErrConflict
	}
	if r.Status == Cancelled {
		return nil
	}
	return s.save(ctx, r, func(tx *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
		if w.Stage == "rmb_delivery" || r.Status == Settled {
			return ErrConflict
		}
		w.CancelRequested = true
		r.Status = CancelPending
		return s.Engine.Repo.Ops.ExpireCommerceEscrowDebitsTx(ctx, tx, r.CommerceId, s.Engine.now())
	})
}
func (e *Engine) planRmb(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, now uint64) error {
	if w.Rmb == nil {
		return errors.New("人民币合同条款缺失")
	}
	switch w.Stage {
	case "rmb_escrow":
		addLeg(w, "seller_debit", r.SellerPlayerId, 3, Bundle(w.SellerAssets))
		a := leg(w, "seller_debit")
		if !done(a) {
			return nil
		}
		if !applied(a) {
			w.Stage = "rmb_returned"
			r.Status = Cancelled
			return nil
		}
		if w.CancelRequested || !e.Policy.Snapshot(a.Snapshot) {
			w.Stage = "rmb_return"
			r.Status = CancelPending
			return transferLeg(w, "seller_return", r.SellerPlayerId, a)
		}
		w.Stage = "rmb_ready"
		r.Status = Escrowing
	case "rmb_delivery":
		if w.Rmb.CharacterPlayerId != 0 {
			return nil
		}
		a := leg(w, "seller_debit")
		if !applied(a) {
			return errors.New("人民币交付缺少原扣出证据")
		}
		if err := transferLeg(w, "rmb_buyer_credit", r.BuyerPlayerId, a); err != nil {
			return err
		}
		if l := leg(w, "rmb_buyer_credit"); done(l) {
			if applied(l) {
				w.Stage = "rmb_delivered"
				r.Status = Settled
			} else {
				w.Stage = "rmb_refund"
				r.Status = 8
			}
		}
	case "rmb_return":
		if w.Rmb.CharacterPlayerId != 0 {
			return nil
		}
		if a := leg(w, "seller_debit"); applied(a) {
			if err := transferLeg(w, "seller_return", r.SellerPlayerId, a); err != nil {
				return err
			}
		}
		if returned := leg(w, "seller_return"); returned == nil || done(returned) {
			if returned == nil || applied(returned) {
				w.Stage = "rmb_returned"
				r.Status = Cancelled
			} else {
				w.Stage = "manual"
				r.Status = Manual
			}
		}
	}
	return nil
}
func (s *RmbConsignments) step(ctx context.Context, r *tradepb.TradeCommerceRecord) error {
	w, err := data.Workflow(r)
	if err != nil {
		return corrupt(err)
	}
	if w.Rmb == nil {
		return corrupt(errors.New("人民币合同缺少原件"))
	}
	t := w.Rmb
	switch w.Stage {
	case "rmb_escrow", "rmb_delivery", "rmb_return":
		if t.CharacterPlayerId == 0 {
			if err := s.checkAssetIdentity(ctx, r, w); err != nil {
				return err
			}
			return s.Engine.advance(ctx, r)
		}
		if w.Stage == "rmb_return" {
			return s.cancelRole(ctx, r, w)
		}
		if w.Stage == "rmb_delivery" {
			return s.transferRole(ctx, r, w)
		}
	case "rmb_freezing":
		// 取消先提交原 listing 墓碑；Begin 的前置条件失败也不能困住尚未冻结的合同。
		if w.CancelRequested {
			return s.cancelRole(ctx, r, w)
		}
		first, err := s.Directory.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: r.CommerceId, PlayerId: t.CharacterPlayerId, SellerAccount: t.SellerAccount, ExpectedOwnerRevision: t.SellerOwnerRevision})
		if err != nil {
			return err
		}
		if w.CancelRequested || first.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING || first.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED {
			return s.cancelRole(ctx, r, w)
		}
		if first.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED {
			return corrupt(errors.New("上架合同却已有角色过户终局"))
		}
		if err := s.seal(ctx, first); err != nil {
			return err
		}
		confirmed, err := s.Directory.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: r.CommerceId, PlayerId: t.CharacterPlayerId, SellerAccount: t.SellerAccount, ExpectedOwnerRevision: t.SellerOwnerRevision})
		if err != nil {
			return err
		}
		if confirmed.State != dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN || confirmed.FreezeEvidence == nil {
			return ErrConflict
		}
		proof, err := proto.MarshalOptions{Deterministic: true}.Marshal(confirmed)
		if err != nil {
			return err
		}
		return s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
			if w.Stage != "rmb_freezing" {
				return nil
			}
			w.Rmb.FreezeReceipt = proof
			w.Stage = "rmb_ready"
			return nil
		})
	case "rmb_ready":
		return s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
			if w.Stage != "rmb_ready" {
				return nil
			}
			if w.CancelRequested {
				w.Stage = "rmb_return"
				r.Status = CancelPending
				return nil
			}
			raw := w.Rmb.FreezeReceipt
			if t.CharacterPlayerId == 0 {
				a := leg(w, "seller_debit")
				if !applied(a) {
					return corrupt(errors.New("登记前缺少托管终局"))
				}
				var err error
				raw, err = proto.MarshalOptions{Deterministic: true}.Marshal(a)
				if err != nil {
					return err
				}
			}
			h := sha256.New()
			h.Write(r.RequestDigest)
			h.Write(raw)
			w.Rmb.EscrowProof = hex.EncodeToString(h.Sum(nil))
			w.Rmb.EscrowReference = "trade:" + strconv.FormatUint(r.CommerceId, 10)
			w.Stage = "rmb_registering"
			return nil
		})
	case "rmb_registering":
		// 即便取消已到，也先重放原register消歧，再由下一轮cancel，绝不先退资产让迟到register公示空托管。
		listing, err := s.Mall.rmbRegister(ctx, r, w)
		if err != nil {
			return err
		}
		return s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
			if w.Stage != "rmb_registering" {
				return nil
			}
			w.Rmb.RegisteredListing = listing
			w.Stage = "rmb_listed"
			r.Status = Waiting
			return nil
		})
	case "rmb_listed":
		if !w.CancelRequested {
			_, err := s.Engine.Repo.Defer(ctx, r.CommerceId, r.Version, s.Engine.now()+30000)
			return err
		}
		listing, err := s.Mall.RmbCancel(ctx, t.SellerAccount, r.CommerceId)
		if err != nil {
			return err
		}
		if listing.State != "RETURNING" && listing.State != "RETURNED" {
			return ErrConflict
		}
		return s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
			if w.Stage == "rmb_listed" {
				w.Stage = "rmb_return"
				r.Status = CancelPending
			}
			return nil
		})
	case "rmb_refund":
		_, err := s.Engine.Repo.Defer(ctx, r.CommerceId, r.Version, s.Engine.now()+30000)
		return err
	case "rmb_delivered", "rmb_returned", "manual":
		return nil
	default:
		return corrupt(errors.New("未知人民币合同阶段"))
	}
	return corrupt(errors.New("人民币合同类型与阶段不符"))
}
func (s *RmbConsignments) checkAssetIdentity(ctx context.Context, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	t := w.Rmb
	owned, err := s.Directory.Lookup(ctx, r.SellerPlayerId)
	if err != nil {
		return err
	}
	// 换主 / 被冻结是长期状态,按 role_conflict 分钟级复查(2026-10-22),与 checkContractIdentities 同一分类。
	if owned.Account != t.SellerAccount || owned.OwnerRevision != t.SellerOwnerRevision || owned.TradeListingId != 0 {
		return roleConflict()
	}
	if w.Stage == "rmb_delivery" {
		buyer, err := s.Directory.Lookup(ctx, r.BuyerPlayerId)
		if err != nil {
			return err
		}
		if buyer.Account != t.BuyerAccount || buyer.OwnerRevision != t.BuyerOwnerRevision || buyer.TradeListingId != 0 {
			return roleConflict()
		}
	}
	return nil
}
func (s *RmbConsignments) seal(ctx context.Context, receipt *dspb.AccountRoleTradeReceipt) error {
	if s.Sealer == nil {
		return errors.New("角色封存通道未装配")
	}
	if len(receipt.Holders) == 0 {
		return nil
	}
	if s.sealAfter == nil {
		s.sealAfter = map[uint64]int{}
	}
	start := s.sealAfter[receipt.ListingId] % len(receipt.Holders)
	var failures error
	for n := 0; n < min(len(receipt.Holders), 8); n++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		i := (start + n) % len(receipt.Holders)
		h := receipt.Holders[i]
		if !h.Drained {
			call, cancel := context.WithTimeout(ctx, 350*time.Millisecond)
			err := s.Sealer.Seal(call, h.Seal)
			cancel()
			if err != nil {
				failures = errors.Join(failures, err)
			}
		}
		s.sealAfter[receipt.ListingId] = (i + 1) % len(receipt.Holders)
	}
	return failures
}
func (s *RmbConsignments) cancelRole(ctx context.Context, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	t := w.Rmb
	receipt, err := s.Directory.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: r.CommerceId, PlayerId: t.CharacterPlayerId, SellerAccount: t.SellerAccount, ExpectedOwnerRevision: t.SellerOwnerRevision})
	if err != nil {
		return err
	}
	if receipt.State != dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED {
		if err := s.seal(ctx, receipt); err != nil {
			return err
		}
		return ErrConflict
	}
	delete(s.sealAfter, r.CommerceId)
	return s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
		if w.Stage == "rmb_delivery" || w.Stage == "rmb_delivered" {
			return ErrConflict
		}
		w.Stage = "rmb_returned"
		r.Status = Cancelled
		return nil
	})
}
func (s *RmbConsignments) transferRole(ctx context.Context, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	t := w.Rmb
	if t.TransferTxId == 0 || !accountdirectory.ValidAccount(t.BuyerAccount) {
		return corrupt(errors.New("角色过户缺少原tx身份"))
	}
	_, err := s.Directory.Transfer(ctx, &dspb.TransferAccountRoleRequest{TxId: t.TransferTxId, ListingId: r.CommerceId, PlayerId: t.CharacterPlayerId, SellerAccount: t.SellerAccount, BuyerAccount: t.BuyerAccount, ExpectedOwnerRevision: t.SellerOwnerRevision})
	if err != nil {
		return err
	}
	delete(s.sealAfter, r.CommerceId)
	return s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
		if w.Stage != "rmb_delivery" {
			return ErrConflict
		}
		w.Stage = "rmb_delivered"
		r.Status = Settled
		return nil
	})
}

type rmbDelivery struct {
	Order     string `json:"orderId"`
	Buyer     string `json:"buyerPlayerId"`
	Account   string `json:"buyerAccount"`
	Revision  string `json:"buyerOwnerRevision"`
	Listing   string `json:"listingId"`
	Reference string `json:"escrowReference"`
	Proof     string `json:"escrowProof"`
	Amount    uint64 `json:"amountFen"`
}
type rmbReturn struct {
	Listing   string `json:"listingId"`
	Seller    string `json:"sellerPlayerId"`
	Account   string `json:"sellerAccount"`
	Reference string `json:"escrowReference"`
	Proof     string `json:"escrowProof"`
}

func (s *RmbConsignments) deliver(ctx context.Context, in rmbDelivery) error {
	id, err := canonicalID(in.Listing)
	if err != nil {
		return err
	}
	buyer, err := canonicalID(in.Buyer)
	if err != nil {
		return err
	}
	revision, err := canonicalID(in.Revision)
	if err != nil {
		return err
	}
	if !mallOrderID.MatchString(in.Order) || !accountdirectory.ValidAccount(in.Account) {
		return ErrInvalid
	}
	r, err := s.Engine.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	w, err := data.Workflow(r)
	if err != nil {
		return err
	}
	t := w.Rmb
	if r.Kind != 6 || t == nil || in.Reference != t.EscrowReference || in.Proof != t.EscrowProof || in.Amount != t.PriceFen {
		return ErrReplay
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if len(t.DeliveryDigest) > 0 {
		if !bytes.Equal(t.DeliveryDigest, sum[:]) {
			return ErrReplay
		}
	} else {
		owned, err := s.Directory.Lookup(ctx, buyer)
		if err != nil {
			return err
		}
		if owned.Account != in.Account || owned.OwnerRevision != revision || owned.TradeListingId != 0 {
			// 本订单尚未建任何credit/Transfer原件；目标角色已换主只能退款，不能换一个账号继续发货。
			if err := s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
				if len(w.Rmb.DeliveryDigest) > 0 {
					if !bytes.Equal(w.Rmb.DeliveryDigest, sum[:]) {
						return ErrReplay
					}
					return nil
				}
				if w.Stage != "rmb_listed" {
					return ErrConflict
				}
				w.Rmb.DeliveryDigest = sum[:]
				w.Rmb.BuyerAccount = in.Account
				w.Rmb.MallOrderId = in.Order
				w.Stage = "rmb_refund"
				r.Status = 8
				return nil
			}); err != nil {
				return err
			}
			// 先提交退款决定，再调用商城；另一副本不能借目标角色恢复可用而重新建credit。
			committed, err := s.Engine.Repo.Get(ctx, id)
			if err != nil {
				return err
			}
			proof := sha256.Sum256(committed.WorkflowProto)
			return s.deliveryReceipt(ctx, committed, in.Order, "REFUND_PENDING", hex.EncodeToString(proof[:]))
		}
		txID := uint64(0)
		if t.CharacterPlayerId != 0 {
			txID, err = s.Engine.IDs.Next(ctx)
			if err != nil {
				return err
			}
		}
		err = s.Engine.Repo.Tx(ctx, func(tx *sql.Tx) error {
			if err := s.Engine.lockRoles(ctx, tx, r.CommerceId, r.SellerPlayerId, buyer); err != nil {
				return err
			}
			current, err := s.Engine.Repo.Lock(ctx, tx, id)
			if err != nil {
				return err
			}
			cw, err := data.Workflow(current)
			if err != nil {
				return err
			}
			ct := cw.Rmb
			if ct == nil {
				return ErrConflict
			}
			if len(ct.DeliveryDigest) > 0 {
				if !bytes.Equal(ct.DeliveryDigest, sum[:]) {
					return ErrReplay
				}
				return nil
			}
			if cw.Stage != "rmb_listed" || in.Account == ct.SellerAccount {
				return ErrConflict
			}
			locked, err := s.Engine.Roles.Ownership(ctx, tx, buyer)
			if err != nil {
				return err
			}
			if locked.Account != in.Account || locked.OwnerRevision != revision || locked.TradeListingId != 0 {
				return ErrConflict
			}
			ct.BuyerAccount = in.Account
			ct.BuyerOwnerRevision = locked.OwnerRevision
			cw.BuyerIdentity = &tradepb.TradeRoleIdentity{Account: in.Account, OwnerRevision: revision}
			ct.MallOrderId = in.Order
			ct.DeliveryDigest = sum[:]
			ct.TransferTxId = txID
			cw.CancelRequested = false // 可信已付款意图表示商城收款先于取消赢得裁决。
			current.BuyerPlayerId = buyer
			cw.Stage = "rmb_delivery"
			current.Status = Delivering
			return s.Engine.Repo.Save(ctx, tx, current, cw, s.Engine.now())
		})
		if err != nil {
			return err
		}
		r, err = s.Engine.Repo.Get(ctx, id)
		if err != nil {
			return err
		}
		w, err = data.Workflow(r)
		if err != nil {
			return err
		}
	}
	state := ""
	switch r.Status {
	case Settled:
		state = "DELIVERED"
	case 8:
		state = "REFUND_PENDING"
	case Manual:
		state = "MANUAL"
	}
	if state == "" {
		return nil
	}
	proof := sha256.Sum256(r.WorkflowProto)
	return s.deliveryReceipt(ctx, r, in.Order, state, hex.EncodeToString(proof[:]))
}
func (s *RmbConsignments) deliveryReceipt(ctx context.Context, r *tradepb.TradeCommerceRecord, order, state, proof string) error {
	return s.Mall.call(ctx, "delivery", "trade", "POST", "/internal/deliveries/receipt", map[string]any{"orderId": order, "tradeOrderId": strconv.FormatUint(r.CommerceId, 10), "state": state, "proof": proof}, nil)
}
func (s *RmbConsignments) returned(ctx context.Context, in rmbReturn) error {
	id, err := canonicalID(in.Listing)
	if err != nil {
		return err
	}
	r, err := s.Engine.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	w, err := data.Workflow(r)
	if err != nil {
		return err
	}
	t := w.Rmb
	if r.Kind != 6 || t == nil || in.Seller != strconv.FormatUint(r.SellerPlayerId, 10) || in.Account != t.SellerAccount || in.Reference != t.EscrowReference || in.Proof != t.EscrowProof {
		return ErrReplay
	}
	if r.Status != Cancelled {
		if w.Stage != "rmb_listed" && w.Stage != "rmb_return" && w.Stage != "rmb_refund" {
			return ErrConflict
		}
		if err := s.save(ctx, r, func(_ *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
			if w.Stage == "rmb_listed" || w.Stage == "rmb_refund" {
				w.Stage = "rmb_return"
				w.CancelRequested = true
				r.Status = CancelPending
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	}
	proof := sha256.Sum256(r.WorkflowProto)
	return s.Mall.consignmentCall(ctx, "returns/receipt", map[string]any{"listingId": in.Listing, "escrowReference": in.Reference, "escrowProof": in.Proof, "returnProof": hex.EncodeToString(proof[:])}, nil)
}
func (s *RmbConsignments) Tick(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var failures error
	// 两种扫描各有子预算，永久离线订单不会吃掉回退通道整轮时间。
	for _, returns := range []bool{false, true} {
		call, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
		var err error
		if returns {
			err = s.pollReturns(call)
		} else {
			err = s.pollDeliveries(call)
		}
		cancel()
		failures = errors.Join(failures, err)
	}
	return failures
}
func (s *RmbConsignments) pollDeliveries(ctx context.Context) error {
	var page struct {
		Entries []struct {
			Intent *rmbDelivery `json:"intent"`
			Cursor *string      `json:"cursor"`
		} `json:"entries"`
	}
	if err := s.Mall.consignmentCall(ctx, "deliveries/poll", map[string]any{"cursor": s.deliveryCursor, "limit": 20}, &page); err != nil {
		return err
	}
	if page.Entries == nil || len(page.Entries) > 20 {
		return errors.New("人民币交付页非法")
	}
	// 复用已验证的商城稳定复合游标规则；先验整页，业务意图本身不增加分页字段。
	check := mallPollPage{Entries: make([]mallPollEntry, 0, len(page.Entries))}
	for _, e := range page.Entries {
		if e.Intent == nil {
			return errors.New("人民币交付意图缺失")
		}
		check.Entries = append(check.Entries, mallPollEntry{Intent: &mallIntent{OrderID: e.Intent.Order}, Cursor: e.Cursor})
	}
	if err := validateMallPage(check, s.deliveryCursor); err != nil {
		return err
	}
	if len(page.Entries) == 0 {
		s.deliveryCursor = ""
		return nil
	}
	var failures error
	for _, entry := range page.Entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.deliver(ctx, *entry.Intent); err != nil {
			failures = errors.Join(failures, fmt.Errorf("人民币交付 %s: %w", entry.Intent.Order, err))
		}
		s.deliveryCursor = *entry.Cursor
	}
	return failures
}
func (s *RmbConsignments) pollReturns(ctx context.Context) error {
	var page struct {
		Entries []struct {
			Intent *rmbReturn `json:"intent"`
			Cursor *string    `json:"cursor"`
		} `json:"entries"`
	}
	if err := s.Mall.consignmentCall(ctx, "returns/poll", map[string]any{"cursor": s.returnCursor, "limit": 20}, &page); err != nil {
		return err
	}
	if page.Entries == nil || len(page.Entries) > 20 {
		return errors.New("人民币回退页非法")
	}
	previous, upper, err := parseReturnCursor(s.returnCursor)
	if err != nil {
		return err
	}
	for i, e := range page.Entries {
		if e.Intent == nil || e.Cursor == nil {
			return errors.New("人民币回退原件或游标缺失")
		}
		if _, err := canonicalID(e.Intent.Listing); err != nil {
			return err
		}
		if *e.Cursor == "" {
			if i != len(page.Entries)-1 {
				return errors.New("回退游标提前终止")
			}
			continue
		}
		next, bound, err := parseReturnCursor(*e.Cursor)
		if err != nil || next != e.Intent.Listing || next <= previous || upper != "" && upper != bound {
			return errors.New("人民币回退水位非法")
		}
		previous, upper = next, bound
	}
	if len(page.Entries) == 0 {
		s.returnCursor = ""
		return nil
	}
	var failures error
	for _, e := range page.Entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		failures = errors.Join(failures, s.returned(ctx, *e.Intent))
		s.returnCursor = *e.Cursor
	}
	return failures
}
func parseReturnCursor(raw string) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	p := strings.Split(raw, ":")
	if len(raw) > 100 || len(p) != 3 || p[0] != "r1" {
		return "", "", ErrInvalid
	}
	if _, err := canonicalID(p[1]); err != nil {
		return "", "", err
	}
	if _, err := canonicalID(p[2]); err != nil {
		return "", "", err
	}
	if p[1] > p[2] {
		return "", "", ErrInvalid
	}
	return p[1], p[2], nil
}

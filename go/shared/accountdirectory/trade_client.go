package accountdirectory

import (
	"context"
	"math"
	"time"

	dspb "proto/data_service"
	"shared/serviceauth"

	"google.golang.org/protobuf/proto"
)

const TradeSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE"

// RoleTrades 只修改目录交易状态，不把 Begin 成功解释为在线实体已冻结。
// Confirm 必须由服务端 SQL 证据验证；未装配验证器时拒绝，Transfer 只接受 FROZEN。
type RoleTrades interface {
	Begin(context.Context, *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error)
	Confirm(context.Context, *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error)
	Cancel(context.Context, *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error)
	Transfer(context.Context, *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error)
}

// TradeClient 与 login 目录客户端分开装配，防止 login 的读写证明扩大成角色过户权限。
type TradeClient struct {
	rpc    dspb.DataServiceClient
	secret string
}

func NewTradeClient(rpc dspb.DataServiceClient, secret string) *TradeClient {
	return &TradeClient{rpc: rpc, secret: secret}
}

// Lookup 只读当前目录，调用者还必须比较原gate登录态账号，不能把当前归属冒作会话主体。
func (c *TradeClient) Lookup(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	in := &dspb.LookupAccountRoleRequest{PlayerId: player}
	call, cancel, err := c.prepare(ctx, "LookupAccountRole", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.LookupAccountRole(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Role == nil || out.Role.PlayerId != player || !ValidAccount(out.Account) || out.OwnerRevision == 0 {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (c *TradeClient) prepare(ctx context.Context, method string, in proto.Message) (context.Context, context.CancelFunc, error) {
	call, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	if c == nil || c.rpc == nil {
		cancel()
		return nil, func() {}, ErrUnavailable
	}
	if err := serviceauth.Sign(MethodPrefix+method, in, "trade", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		cancel()
		return nil, func() {}, ErrUnavailable
	}
	return call, cancel, nil
}
func validRoleTradeReceipt(out *dspb.AccountRoleTradeReceipt, listing, player uint64, seller string, revision uint64) bool {
	if out == nil || out.ListingId != listing || out.PlayerId != player || out.SellerAccount != seller || out.SellerOwnerRevision != revision || out.State < dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING || out.State > dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING || len(out.Holders) > MaxTradeHolders {
		return false
	}
	if out.FreezeEvidence != nil {
		e := out.FreezeEvidence
		if !ValidFreezeEvidence(e) || e.PlayerId != player || e.ListingId != listing || e.OwnerRevision != revision {
			return false
		}
	}
	seen := make(map[string]bool, len(out.Holders))
	for _, holder := range out.Holders {
		if holder == nil || !ValidTradeSeal(holder.Seal) || holder.Seal.ListingId != listing || holder.Seal.PlayerId != player || holder.Seal.OwnerRevision != revision || seen[holder.Seal.SceneInstanceId] {
			return false
		}
		seen[holder.Seal.SceneInstanceId] = true
		if (out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN || out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED || out.State == dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED) && !holder.Drained {
			return false
		}
	}
	if NoActorFreezeEvidence(out.FreezeEvidence) && len(out.Holders) > 0 {
		return false
	}
	return (out.State != dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FROZEN && out.State != dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_TRANSFERRED) || out.FreezeEvidence != nil
}
func (c *TradeClient) Begin(ctx context.Context, in *dspb.BeginAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	call, cancel, err := c.prepare(ctx, "BeginAccountRoleTrade", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.BeginAccountRoleTrade(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if !validRoleTradeReceipt(out, in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (c *TradeClient) Confirm(ctx context.Context, in *dspb.ConfirmAccountRoleTradeFreezeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	call, cancel, err := c.prepare(ctx, "ConfirmAccountRoleTradeFreeze", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.ConfirmAccountRoleTradeFreeze(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if !validRoleTradeReceipt(out, in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (c *TradeClient) Cancel(ctx context.Context, in *dspb.CancelAccountRoleTradeRequest) (*dspb.AccountRoleTradeReceipt, error) {
	call, cancel, err := c.prepare(ctx, "CancelAccountRoleTrade", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.CancelAccountRoleTrade(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if !validRoleTradeReceipt(out, in.ListingId, in.PlayerId, in.SellerAccount, in.ExpectedOwnerRevision) {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (c *TradeClient) Transfer(ctx context.Context, in *dspb.TransferAccountRoleRequest) (*dspb.AccountRoleTransferReceipt, error) {
	call, cancel, err := c.prepare(ctx, "TransferAccountRole", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.TransferAccountRole(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.TxId != in.TxId || out.ListingId != in.ListingId || out.PlayerId != in.PlayerId || out.SellerAccount != in.SellerAccount || out.BuyerAccount != in.BuyerAccount || out.SellerOwnerRevision != in.ExpectedOwnerRevision || in.ExpectedOwnerRevision == math.MaxUint64 || out.BuyerOwnerRevision != in.ExpectedOwnerRevision+1 || out.Role == nil || out.Role.PlayerId != in.PlayerId || out.CreatedMs == 0 {
		return nil, ErrUnavailable
	}
	return out, nil
}

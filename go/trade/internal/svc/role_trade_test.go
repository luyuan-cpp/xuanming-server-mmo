package svc

import (
	"context"
	"testing"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"shared/callerauth/callerauthtest"
	"trade/internal/commerce"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// rmbMethod 是测试模拟的客户端入口;gate 的签名绑定方法名。
const rmbMethod = tradepb.ClientPlayerJubaozhai_CreateRmbConsignment_FullMethodName

type identityDataClient struct {
	dspb.DataServiceClient
	owned *dspb.AccountRoleOwnership
	calls int
}

func (c *identityDataClient) LookupAccountRole(context.Context, *dspb.LookupAccountRoleRequest, ...grpc.CallOption) (*dspb.AccountRoleOwnership, error) {
	c.calls++
	return proto.Clone(c.owned).(*dspb.AccountRoleOwnership), nil
}
func TestOriginalSellerSessionCannotBorrowBuyerAccountOrRevisionAfterRoleTransfer(t *testing.T) {
	rpc := &identityDataClient{owned: &dspb.AccountRoleOwnership{Account: "buyer", OwnerRevision: 2, Role: &base.AccountSimplePlayer{PlayerId: 11}}}
	a := &RmbIdentity{Directory: accountdirectory.NewTradeClient(rpc, "12345678901234567890123456789012")}
	// gate 为每次请求签当时的会话,所以 in 改动之后都重新签一次。
	in := callerauthtest.OnlineSession("seller", 11, 1)
	_, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), 0)
	require.Error(t, err)
	// ABA：账号又回到seller，修订仍必须等于gate发出时原值，不能按当前目录补成3。
	rpc.owned.Account = "seller"
	rpc.owned.OwnerRevision = 3
	_, err = a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), 0)
	require.Error(t, err)
	in.OwnerRevision = 3
	out, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), 0)
	require.NoError(t, err)
	require.EqualValues(t, 3, out.OwnerRevision)
	before := rpc.calls
	in.OwnerRevision = 0
	_, err = a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), 0)
	require.Error(t, err)
	require.Equal(t, before, rpc.calls)
}

// 原账号只认 gate 签名原件:未签名的自报身份、缺会话、缺账号的会话都在查角色目录之前拒绝,
// 按请求无效(commerce.ErrInvalid → kInvalidParameter)回,不计成服务端故障。
// Resolve 不再收业务另传的会话声明,所以没有「声明与原件不符」这条路。
func TestRmbIdentityRejectsUnsignedOrIncompleteSessionBeforeDirectoryLookup(t *testing.T) {
	rpc := &identityDataClient{owned: &dspb.AccountRoleOwnership{Account: "seller", OwnerRevision: 3, Role: &base.AccountSimplePlayer{PlayerId: 11}}}
	a := &RmbIdentity{Directory: accountdirectory.NewTradeClient(rpc, "12345678901234567890123456789012")}
	signed := callerauthtest.OnlineSession("seller", 11, 3)

	_, err := a.Resolve(callerauthtest.Unverified(t, signed), 0)
	require.ErrorIs(t, err, commerce.ErrInvalid, "没有验签原件的自报身份拿不到账号")
	_, err = a.Resolve(context.Background(), 0)
	require.ErrorIs(t, err, commerce.ErrInvalid, "没有会话的调用拿不到账号")

	noAccount := proto.Clone(signed).(*base.SessionDetails)
	noAccount.AuthenticatedAccount = ""
	_, err = a.Resolve(callerauthtest.Incoming(t, rmbMethod, noAccount), 0)
	require.ErrorIs(t, err, commerce.ErrInvalid, "gate 只在角色绑定后写账号,缺账号的会话不算在线角色")
	require.Zero(t, rpc.calls, "身份核不过就不查角色目录")

	out, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, signed), 0)
	require.NoError(t, err, "验签原件完整才放行")
	require.Equal(t, "seller", out.Account)
}

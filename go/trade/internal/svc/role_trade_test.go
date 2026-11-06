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
	_, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), in, 0)
	require.Error(t, err)
	// ABA：账号又回到seller，修订仍必须等于gate发出时原值，不能按当前目录补成3。
	rpc.owned.Account = "seller"
	rpc.owned.OwnerRevision = 3
	_, err = a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), in, 0)
	require.Error(t, err)
	in.OwnerRevision = 3
	out, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), in, 0)
	require.NoError(t, err)
	require.EqualValues(t, 3, out.OwnerRevision)
	before := rpc.calls
	in.OwnerRevision = 0
	_, err = a.Resolve(callerauthtest.Incoming(t, rmbMethod, in), in, 0)
	require.Error(t, err)
	require.Equal(t, before, rpc.calls)
}

// 原账号只认 gate 签名原件:未签名的自报身份、与原件不符的声明、缺账号的会话都在查角色目录之前拒绝,
// 按请求无效(commerce.ErrInvalid → kInvalidParameter)回,不计成服务端故障。
func TestRmbIdentityRejectsUnsignedOrAlteredSessionBeforeDirectoryLookup(t *testing.T) {
	rpc := &identityDataClient{owned: &dspb.AccountRoleOwnership{Account: "seller", OwnerRevision: 3, Role: &base.AccountSimplePlayer{PlayerId: 11}}}
	a := &RmbIdentity{Directory: accountdirectory.NewTradeClient(rpc, "12345678901234567890123456789012")}
	signed := callerauthtest.OnlineSession("seller", 11, 3)

	_, err := a.Resolve(callerauthtest.Unverified(t, signed), signed, 0)
	require.ErrorIs(t, err, commerce.ErrInvalid, "没有验签原件的自报身份拿不到账号")

	for name, mutate := range map[string]func(*base.SessionDetails){
		"新修订":      func(d *base.SessionDetails) { d.OwnerRevision++ },
		"换账号":      func(d *base.SessionDetails) { d.AuthenticatedAccount = "buyer" },
		"重启后的gate": func(d *base.SessionDetails) { d.GateInstanceId = "gate-after-restart" },
		"新连接代次":    func(d *base.SessionDetails) { d.SessionIncarnation++ },
	} {
		t.Run(name, func(t *testing.T) {
			claimed := proto.Clone(signed).(*base.SessionDetails)
			mutate(claimed)
			_, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, signed), claimed, 0)
			require.ErrorIs(t, err, commerce.ErrInvalid, "声明必须与验签原件逐字段一致")
		})
	}

	noAccount := proto.Clone(signed).(*base.SessionDetails)
	noAccount.AuthenticatedAccount = ""
	_, err = a.Resolve(callerauthtest.Incoming(t, rmbMethod, noAccount), noAccount, 0)
	require.ErrorIs(t, err, commerce.ErrInvalid, "gate 只在角色绑定后写账号,缺账号的会话不算在线角色")
	require.Zero(t, rpc.calls, "身份核不过就不查角色目录")

	out, err := a.Resolve(callerauthtest.Incoming(t, rmbMethod, signed), signed, 0)
	require.NoError(t, err, "原样声明验签原件才放行")
	require.Equal(t, "seller", out.Account)
}

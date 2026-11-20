package accountdirectory

import (
	"context"
	"errors"
	"time"

	dspb "proto/data_service"
	"shared/antiaddiction"
	"shared/serviceauth"
)

// 账号行上的实名列(docs/design/real-name-anti-addiction.md §5.1、§5.6)。

const (
	// PaymentCaller 是 go/payment 按玩家查实名时的 ServiceCallAuth.caller,只有它能调 GetAccountRealName。
	PaymentCaller = "payment"
	// PaymentSecretEnv 是这个调用方的密钥,与其它调用方互不复用。
	PaymentSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_PAYMENT"
)

// SetRealName 把国家系统给的 pi 写到账号行上,只写一次;返回写之后库里的值(与 pi 不同 = 账号早已绑定别的实名)。
func (c *Client) SetRealName(ctx context.Context, account, pi string) (string, error) {
	in := &dspb.SetAccountRealNameRequest{Account: account, RealNamePi: pi}
	call, cancel, err := c.prepare(ctx, "SetAccountRealName", in)
	if err != nil {
		return "", err
	}
	defer cancel()
	out, err := c.rpc.SetAccountRealName(call, in)
	if err != nil {
		return "", classify(err)
	}
	if out == nil || !antiaddiction.ValidPI(out.RealNamePi) {
		return "", ErrUnavailable
	}
	return out.RealNamePi, nil
}

// ResetRealName 清掉账号行上的 pi(客服重置),返回清之前有没有。幂等。
func (c *Client) ResetRealName(ctx context.Context, account string) (bool, error) {
	in := &dspb.ResetAccountRealNameRequest{Account: account}
	call, cancel, err := c.prepare(ctx, "ResetAccountRealName", in)
	if err != nil {
		return false, err
	}
	defer cancel()
	out, err := c.rpc.ResetAccountRealName(call, in)
	if err != nil {
		return false, classify(err)
	}
	return out.GetHadRealName(), nil
}

// RealNameClient 给 go/payment 按玩家查实名。与 login 的目录客户端分开装配:payment 的证明只能调这一个只读方法。
type RealNameClient struct {
	rpc    dspb.DataServiceClient
	secret string
}

// NewRealNameClient 校验密钥(同 NewRouteClient)。不合格回错误,调用方拒绝启动:限额闸读不了实名,App 渠道一单也收不了。
func NewRealNameClient(rpc dspb.DataServiceClient, secret string) (*RealNameClient, error) {
	if rpc == nil {
		return nil, errors.New("real-name lookup has no data_service client")
	}
	if _, err := serviceauth.New(map[string]string{PaymentCaller: secret}); err != nil {
		return nil, err
	}
	return &RealNameClient{rpc: rpc, secret: secret}, nil
}

// RealNameOf 回玩家所属账号的 pi。found=false 表示角色不在目录里或已删;pi 为空表示账号没由游戏方实名
// (小游戏渠道由平台实名,限额也由平台执行)。
func (c *RealNameClient) RealNameOf(ctx context.Context, player uint64) (pi string, found bool, err error) {
	if c == nil || c.rpc == nil {
		return "", false, ErrUnavailable
	}
	in := &dspb.GetAccountRealNameRequest{PlayerId: player}
	if err := serviceauth.Sign(MethodPrefix+"GetAccountRealName", in, PaymentCaller, c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return "", false, ErrUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	out, err := c.rpc.GetAccountRealName(call, in)
	if err != nil {
		return "", false, classify(err)
	}
	if out == nil || (out.RealNamePi != "" && !antiaddiction.ValidPI(out.RealNamePi)) {
		return "", false, ErrUnavailable
	}
	return out.RealNamePi, out.Found, nil
}

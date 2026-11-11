package svc

import (
	"context"
	"errors"
	"os"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	scenepb "proto/scene"
	smpb "proto/scene_manager"
	"shared/accountdirectory"
	"shared/roleidentity"
	"shared/scenenode"
	"shared/serviceauth"
	"trade/internal/commerce"
	"trade/internal/config"
)

// RmbIdentity 先取原gate连接账号再核强SQL当前角色归属；当前owner不能替代会话账号。
// 在线域原账号只认 gate 签名原件,不读 login Redis,所以这里只依赖角色目录。
type RmbIdentity struct {
	Directory *accountdirectory.TradeClient
}

// Resolve 用验签原件里的原账号、原修订与角色核角色目录;player 非 0 且不是本角色时,再核目标角色仍归同一账号。
func (a *RmbIdentity) Resolve(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	if a == nil || a.Directory == nil {
		return nil, commerce.ErrInvalid
	}
	original, err := roleidentity.SessionOwnership(ctx)
	if err != nil {
		// 未签名、缺会话或缺原账号 / 原修订是请求本身无效,不是服务端故障:
		// 回故障 tip 会被计成故障并让 RootAdmission 把排空证明打成 Hold。
		return nil, commerce.ErrInvalid
	}
	self := original.Role.GetPlayerId()
	me, err := a.Directory.Lookup(ctx, self)
	if err != nil {
		return nil, err
	}
	if me.Account != original.Account || me.OwnerRevision != original.OwnerRevision {
		return nil, commerce.ErrConflict
	}
	if player == 0 || player == self {
		return me, nil
	}
	target, err := a.Directory.Lookup(ctx, player)
	if err != nil {
		return nil, err
	}
	if target.Account != original.Account {
		return nil, commerce.ErrConflict
	}
	return target, nil
}

type nativeRoleSealer struct {
	Watcher *scenenode.Watcher
	Conns   *scenenode.ConnCache
	Secret  string
}

func (a *nativeRoleSealer) Seal(ctx context.Context, seal *base.RoleTradeSeal) error {
	if a == nil || a.Watcher == nil || a.Conns == nil || !accountdirectory.ValidTradeSeal(seal) {
		return errors.New("角色原生封存依赖不可用")
	}
	entry, err := a.Watcher.EntryOf(seal.ZoneId, seal.SceneNodeId)
	if err != nil {
		return err
	}
	if entry.NodeUuid != seal.SceneInstanceId {
		return errors.New("原角色holder进程未注册，不能替换成新实例")
	}
	conn, err := a.Conns.Dial(entry.Endpoint)
	if err != nil {
		return err
	}
	in := &scenepb.SealAccountRoleTradeRequest{Seal: seal}
	if err := serviceauth.Sign("/scene_node.SceneNodeGrpc/SealAccountRoleTrade", in, "trade", a.Secret, uint64(time.Now().UnixMilli())); err != nil {
		return err
	}
	_, err = smpb.NewSceneNodeGrpcClient(conn).SealAccountRoleTrade(ctx, in)
	return err
}

func (sc *ServiceContext) initRmb() error {
	if sc.Commerce == nil {
		return nil
	}
	if sc.Assets == nil {
		return errors.New("交易必须显式装配资产通道")
	}
	// 与配置校验同一份白名单;配置校验只在人民币寄售开启时查,这里不论开关都查。
	for _, c := range sc.Config.Rmb.CurrencyTypes {
		if !config.IsTradableCurrency(c) {
			return errors.New("人民币寄售货币只允许金币或显式开启钻石，绑定币禁售")
		}
	}
	secret := os.Getenv(accountdirectory.TradeSecretEnv)
	probe := &dspb.LookupAccountRoleRequest{PlayerId: 1}
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"LookupAccountRole", probe, "trade", secret, uint64(time.Now().UnixMilli())); err != nil {
		return errors.New("角色交易独立签名密钥未就绪")
	}
	directory := accountdirectory.NewTradeClient(sc.DataServiceClient, secret)
	sc.RmbIdentity = &RmbIdentity{Directory: directory}
	if !sc.Config.Rmb.Enabled {
		return nil
	}
	if sc.Mall == nil {
		return errors.New("人民币寄售必须装配商城")
	}
	sc.Rmb = &commerce.RmbConsignments{Engine: sc.Commerce, Mall: sc.Mall, Directory: directory, Sealer: &nativeRoleSealer{Watcher: sc.Assets.Watcher, Conns: sc.Assets.Conns, Secret: secret}, CurrencyTypes: sc.Config.Rmb.CurrencyTypes}
	sc.Commerce.Rmb = sc.Rmb
	sc.Assets.Rmb = sc.Rmb
	return nil
}

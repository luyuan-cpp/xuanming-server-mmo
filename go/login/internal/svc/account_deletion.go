package svc

import (
	"context"
	"fmt"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/accountdeletion"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/sessionmanager"
	plpb "proto/player_locator"
	"shared/accountdirectory"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// 账号注销的装配(docs/design/account-deletion.md §4、§8):目录客户端与 AccountDirectory 是同一个,另配 Apple 撤销,
// 并在每个 login 副本起一个执行者后台循环。执行者按 data_service 的租约认领,多副本、多区同时跑也只有一个在处理同一账号。

const (
	accountDeletionInterval = 30 * time.Second
	accountDeletionPoint    = "login.account_deletion_executor"
)

// initAccountDeletion 在目录客户端建好之后调。Apple 段配了撤销而私钥解析不了就拒绝启动:配了撤销却撤不了,注销时才发现就晚了。
func (s *ServiceContext) initAccountDeletion(client *accountdirectory.Client) {
	s.AccountDeletions = client
	s.AccountDeletion = &accountdeletion.Service{Dir: client, Sessions: deletionSessions{s}}
	s.initPassportAdmin()
	a := config.AppConfig.AuthProviders.Apple
	key := activeSecrets().AuthProviders.ApplePrivateKeyPEM
	if a == nil || key == "" {
		return
	}
	revoker, err := auth.NewAppleRevoker(a.ClientId, a.TeamId, a.KeyId, key, a.Endpoint, nil)
	if err != nil {
		panic(fmt.Errorf("AuthProviders.Apple revoke settings rejected; refusing to start: %w", err))
	}
	s.AccountDeletion.Apple = revoker
	logx.Infof("[account-deletion] Sign in with Apple token revocation enabled (team_id=%s key_id=%s)", a.TeamId, a.KeyId)
}

// initPassportAdmin 接上官方通行证的管理接口:注销时删通行证里的用户(docs/design/official-passport-registration.md §3.5),
// 刷新令牌前核对改密 / 封禁(§3.6,clientplayerlogin/refreshtokenlogic.go)。两处共用同一个客户端。
// 管理凭据由 ResolveSecrets 校验并解析:生产配了通行证登录就一定有;dev/test 可以没有,那时两件事都不做。
func (s *ServiceContext) initPassportAdmin() {
	p := config.AppConfig.AuthProviders.Passport
	if p == nil {
		return
	}
	secret := activeSecrets().AuthProviders.PassportAdminClientSecret
	if secret == "" {
		logx.Error("[passport] AuthProviders.Passport has no admin client: passport users are NOT deleted on account deletion, " +
			"and password changes / bans in the passport do NOT revoke game sessions (dev/test only)")
		return
	}
	admin, err := auth.NewPassportAdmin(p.Issuer, p.Endpoint, p.AdminClientId, secret, nil)
	if err != nil {
		panic(fmt.Errorf("AuthProviders.Passport admin client rejected; refusing to start: %w", err))
	}
	s.PassportAdmin = admin
	s.AccountDeletion.Passport = admin
	logx.Infof("[passport] admin API enabled for account deletion and session checks (admin_client_id=%s)", p.AdminClientId)
}

// deletionSessions 把 login 的令牌与踢线接给 accountdeletion.Sessions。
type deletionSessions struct{ s *ServiceContext }

func (d deletionSessions) RevokeAll(ctx context.Context, account string) error {
	return d.s.TokenManager.RevokeAll(ctx, account)
}

// KickPlayer 让这名角色彻底下线:先踢掉连着 gate 的设备(客户端弹 reasonTip),再按正常登出的终点收尾
// (sessionmanager.DeleteSession → player_locator.MarkOffline:删会话,LeaveScene 让持有节点按会话结束释放角色)。
// 只踢 gate 等于断线:有挂机月卡的角色会转成离线托管、留在世界里直到月卡到期(player-connection-lost-hold.md §4.1),
// 删角要等他离场(登记全部关闭),就会一直等下去。没有会话直接成功;会话在这期间变了(踢线引起的断线上报、重连)
// 这一次的登出被 locator 当迟到请求忽略,执行者下一轮按新会话再来一次。
// 代次填 0:gate 不比较代次,只要会话还绑着这名角色就踢(session_identity_fence.h ClassifyKick)。会话键不带区号、存在地域共用的
// 玩家域 Redis 上,gate 命令 topic 也是地域共用的(按 gate 实例 uuid 过滤),所以本区的 player_locator 查得到、踢得到本地域任何一个区的会话。
func (d deletionSessions) KickPlayer(ctx context.Context, player uint64, reasonTip uint32) error {
	return kickPlayerSession(ctx, d.s.PlayerLocatorClient, d.s.KickSessionWithReason, player, reasonTip)
}

type kickGateSessionFunc func(ctx context.Context, gateID string, gateInstanceID string, sessionID uint32, playerID uint64, reasonTip uint32) error

func kickPlayerSession(ctx context.Context, locator plpb.PlayerLocatorClient, kickGate kickGateSessionFunc, player uint64, reasonTip uint32) error {
	session, err := sessionmanager.GetSession(ctx, locator, player)
	if err != nil {
		return err
	}
	if session == nil || session.State == sessionmanager.StateOffline {
		return nil
	}
	if session.GateID != "" && session.GateInstanceID != "" {
		if err := kickGate(ctx, session.GateID, session.GateInstanceID, session.SessionID, player, reasonTip); err != nil {
			return err
		}
	}
	return sessionmanager.DeleteSession(ctx, locator, player, session.SessionID, session.GateInstanceID)
}

// startAccountDeletionExecutor 起执行者循环,关停时由 CloseSource 取消。
func (s *ServiceContext) startAccountDeletionExecutor() {
	if s.AccountDeletion == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.accountDeletionStop = cancel
	deletion := s.AccountDeletion
	s.background.Add(1)
	safego.Go(accountDeletionPoint, func() {
		defer s.background.Done()
		ticker := time.NewTicker(accountDeletionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				safego.Run(accountDeletionPoint, func() {
					result, err := deletion.RunOnce(ctx)
					if err != nil {
						if ctx.Err() == nil {
							logx.Errorf("[account-deletion] executor claim failed: %v", err)
						}
						return
					}
					if result.Claimed > 0 {
						logx.Infof("[account-deletion] executor round: claimed=%d finished=%d waiting=%d failed=%d",
							result.Claimed, result.Finished, result.Waiting, result.Failed)
					}
				})
			}
		}
	})
}

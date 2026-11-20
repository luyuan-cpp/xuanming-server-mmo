package svc

import (
	"context"
	"fmt"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/accountdeletion"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/sessionmanager"
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

// deletionSessions 把 login 的令牌与踢线接给 accountdeletion.Sessions。
type deletionSessions struct{ s *ServiceContext }

func (d deletionSessions) RevokeAll(ctx context.Context, account string) error {
	return d.s.TokenManager.RevokeAll(ctx, account)
}

// KickPlayer 踢掉这名角色当前绑定的在线会话。代次填 0:gate 不比较代次,只要会话还绑着这名角色就踢
// (session_identity_fence.h ClassifyKick)。没有在线会话直接成功。
func (d deletionSessions) KickPlayer(ctx context.Context, player uint64) error {
	session, err := sessionmanager.GetSession(ctx, d.s.PlayerLocatorClient, player)
	if err != nil {
		return err
	}
	if session == nil || session.GateID == "" || session.GateInstanceID == "" || session.State == sessionmanager.StateOffline {
		return nil
	}
	return d.s.KickSessionOnGate(ctx, session.GateID, session.GateInstanceID, session.SessionID, player, 0)
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

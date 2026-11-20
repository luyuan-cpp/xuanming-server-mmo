// Package accountdeletion 是 login 一侧的账号注销(docs/design/account-deletion.md §4):申请、撤销、到期执行。
//
// 状态与互斥都在 data_service 的账号行上(那边的 account_deletion.go),这里只编排 login 自己拥有的副作用:
// 令牌家族吊销、在线会话踢线、Sign in with Apple 撤销,以及执行时逐名调现有的删角(RemoveAccountRoles)。
// 每一步都幂等,失败就留给下一次:申请与撤销由玩家重试,执行由每个 login 副本的后台循环按租约认领重做。
package accountdeletion

import (
	"context"
	"errors"

	"login/internal/logic/pkg/auth"
	login_proto_common "proto/common/base"
	dspb "proto/data_service"
	login "proto/login"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// Directory 是注销用到的目录调用。*accountdirectory.Client 实现它。
type Directory interface {
	Lookup(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error)
	Remove(ctx context.Context, account string, roles []*dspb.AccountRoleVersion) error
	ReadWithLifecycle(ctx context.Context, account string, create bool) (*dspb.AccountDirectoryResponse, error)
	RequestDeletion(ctx context.Context, account string, source uint32) (*dspb.AccountDeletionResponse, error)
	CancelDeletion(ctx context.Context, account string) (*dspb.AccountDeletionResponse, error)
	ClaimDeletions(ctx context.Context, limit uint32) ([]string, error)
	FinishDeletion(ctx context.Context, account string) (*dspb.FinishAccountDeletionResponse, error)
}

// Sessions 是 login 自己的登录态。svc.ServiceContext 实现它(令牌管理器 + 经 gate 踢线)。
type Sessions interface {
	// RevokeAll 吊销账号的全部令牌家族:已签的 access / refresh 立即失效。
	RevokeAll(ctx context.Context, account string) error
	// KickPlayer 踢掉这名角色当前绑定的在线会话;没有在线会话直接成功。
	KickPlayer(ctx context.Context, player uint64) error
}

// AppleRevoker 撤销一次 Sign in with Apple 授权(*auth.AppleRevoker)。
type AppleRevoker interface {
	ExchangeCode(ctx context.Context, code string) (auth.AppleGrant, error)
	Revoke(ctx context.Context, grant auth.AppleGrant) error
}

// 申请来源,与 data_service 的 store.DeletionSourceUser / DeletionSourceAdmin 同值。
const (
	SourceUser  uint32 = 1
	SourceAdmin uint32 = 2
)

// requestTotal 按结局统计注销申请(本人与运维代办都计):ok、blocked、not_found、reauth(Apple 授权码无效)、error。
var requestTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "login",
	Subsystem: "account_deletion",
	Name:      "request_total",
	Help:      "Account deletion requests by outcome: ok, blocked, not_found, reauth, error.",
	Labels:    []string{"outcome"},
})

// Service 编排申请、撤销与执行。Apple 为 nil 表示没配撤销:Apple 账号照常注销,客户端提示玩家到 Apple 设置里手动移除。
type Service struct {
	Dir      Directory
	Sessions Sessions
	Apple    AppleRevoker
}

// Request 申请注销(§4.2)。account 已由调用方用新取的凭据认证过;isApple 时 appleCode 是这次重新授权拿到的授权码。
//
// 顺序:Apple 先换票(授权码不对就在改任何状态之前让玩家重新授权)→ 目录预检并提交冷静期 → 提交成功之后撤销 Apple 授权、
// 吊销令牌、踢掉在线设备。提交之后的三步都是尽力而为:冷静期内进不了游戏靠目录状态,不靠它们;执行时还会再吊销、踢线一遍。
func (s *Service) Request(ctx context.Context, account string, source uint32, isApple bool, appleCode string) *login.AccountDeletionResponse {
	var grant *auth.AppleGrant
	if isApple && s.Apple != nil {
		g, err := s.Apple.ExchangeCode(ctx, appleCode)
		if errors.Is(err, auth.ErrAppleCodeInvalid) {
			requestTotal.Inc("reauth")
			return tipReply(table.LoginError_kLoginAccountDeletionReauthRequired)
		}
		if err != nil {
			logx.WithContext(ctx).Errorf("[account-deletion] apple code exchange failed: %v", err)
			requestTotal.Inc("error")
			return tipReply(table.LoginError_kLoginAuthProviderUnavailable)
		}
		grant = &g
	}
	out, err := s.Dir.RequestDeletion(ctx, account, source)
	if err != nil {
		logx.WithContext(ctx).Errorf("[account-deletion] request failed: account=%s err=%v", MaskAccount(account), err)
		requestTotal.Inc("error")
		return tipReply(table.LoginError_kLoginDataSerializeFailed)
	}
	resp := Reply(out)
	requestTotal.Inc(requestOutcome(out.Outcome))
	if out.Outcome != dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK || out.GetLifecycle().GetDeletionState() == dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_NONE {
		return resp
	}
	if grant != nil {
		if err := s.Apple.Revoke(ctx, *grant); err != nil {
			// 注销已经提交,不回失败:客户端对 Apple 账号恒提示「可到 Apple 设置里移除」,这一条就是兜底。
			logx.WithContext(ctx).Errorf("[account-deletion] apple revoke failed after the deletion was scheduled: account=%s err=%v", MaskAccount(account), err)
		}
	}
	s.signOut(ctx, account)
	logx.WithContext(ctx).Infof("[account-deletion] scheduled: account=%s source=%d execute_after_ms=%d", MaskAccount(account), source, out.GetLifecycle().GetDeletionExecuteAfterMs())
	return resp
}

// Cancel 撤销注销(§4.4)。account 来自调用方验过的 access token。
func (s *Service) Cancel(ctx context.Context, account string) *login.AccountDeletionResponse {
	out, err := s.Dir.CancelDeletion(ctx, account)
	if err != nil {
		logx.WithContext(ctx).Errorf("[account-deletion] cancel failed: account=%s err=%v", MaskAccount(account), err)
		return tipReply(table.LoginError_kLoginDataSerializeFailed)
	}
	if out.Outcome == dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK {
		logx.WithContext(ctx).Infof("[account-deletion] cancelled or nothing to cancel: account=%s", MaskAccount(account))
	}
	return Reply(out)
}

// signOut 吊销令牌并踢掉这个账号还在游戏里的角色。尽力而为,失败只记日志。
func (s *Service) signOut(ctx context.Context, account string) {
	if err := s.Sessions.RevokeAll(ctx, account); err != nil {
		logx.WithContext(ctx).Errorf("[account-deletion] revoke tokens failed: account=%s err=%v", MaskAccount(account), err)
	}
	dir, err := s.Dir.ReadWithLifecycle(ctx, account, false)
	if err != nil {
		logx.WithContext(ctx).Infof("[account-deletion] read roles for kicking failed: account=%s err=%v", MaskAccount(account), err)
		return
	}
	for _, role := range dir.GetPlayers().GetPlayers() {
		if err := s.Sessions.KickPlayer(ctx, role.GetPlayerId()); err != nil {
			logx.WithContext(ctx).Infof("[account-deletion] kick failed: player=%d err=%v", role.GetPlayerId(), err)
		}
	}
}

// Reply 把目录的结论翻成客户端可见的应答(tip 码见 docs/design/account-deletion.md §5.2)。时刻从毫秒换成秒。
func Reply(out *dspb.AccountDeletionResponse) *login.AccountDeletionResponse {
	resp := &login.AccountDeletionResponse{
		State:        uint32(out.GetLifecycle().GetDeletionState()),
		ExecuteAfter: int64(out.GetLifecycle().GetDeletionExecuteAfterMs() / 1000),
	}
	switch out.GetOutcome() {
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK:
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND:
		resp.ErrorMessage = tip(table.LoginError_kLoginAccountNotFound)
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_BLOCKED:
		resp.ErrorMessage = tip(table.LoginError_kLoginAccountDeletionBlocked)
		for _, b := range out.GetBlockers() {
			resp.Blockers = append(resp.Blockers, &login.AccountDeletionBlocker{PlayerId: b.GetPlayerId(), Reason: b.GetReason()})
		}
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_TOO_LATE:
		resp.ErrorMessage = tip(table.LoginError_kLoginAccountDeletionInProgress)
	default:
		// RETRY 或认不出的结论:这一次没判成,按故障回,客户端稍后重试。
		resp.ErrorMessage = tip(table.LoginError_kLoginDataSerializeFailed)
	}
	return resp
}

func requestOutcome(o dspb.AccountDeletionOutcome) string {
	switch o {
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK:
		return "ok"
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_BLOCKED:
		return "blocked"
	case dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND:
		return "not_found"
	default:
		return "error"
	}
}

func tip(id table.LoginError) *login_proto_common.TipInfoMessage {
	return &login_proto_common.TipInfoMessage{Id: uint32(id)}
}

func tipReply(id table.LoginError) *login.AccountDeletionResponse {
	return &login.AccountDeletionResponse{ErrorMessage: tip(id)}
}

// MaskAccount 日志里只留账号的前 4 与后 2 个字符(账号是渠道身份串,如 apple_<sub>,不整段进日志)。
func MaskAccount(account string) string {
	if len(account) <= 8 {
		return "***"
	}
	return account[:4] + "***" + account[len(account)-2:]
}

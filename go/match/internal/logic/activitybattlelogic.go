package logic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/playercontract"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 活动开局 MatchInternal.StartActivityBattle(帮会同道历练,docs/design/guild-phase2/06-activities.md §6.18.2)。
//
// 名单来自 guild 邀请房间里全员已同意的成员;原 lobby_id 是 request_id,意图、全员占用与成局在同一 SQL 事务提交
// (direct_intent.go),活动上下文随成局原件持久化,建房时原样透传给 battle。
//
// 唯一合法调用方是 go/guild;客户端来源(带会话 metadata)在 match_service.go 的 sessionInterceptor
// 就被 PermissionDenied,本文件入口再防一道(拦截器链被改坏时仍 fail-closed)。

// activityTicketReadBudget 预检读一名成员票据的预算(含已结算票的按需释放),不超过调用方给的请求 ctx。
const activityTicketReadBudget = 3 * time.Second

// match_activity_battle_total 的 result 取值(metrics.activityBattleTotal)。
const (
	activityResultStarted  = "started"
	activityResultInvalid  = "invalid"
	activityResultOffline  = "offline"
	activityResultInBattle = "in_battle"
	activityResultNotReady = "not_ready"
	activityResultInternal = "internal"
)

type StartActivityBattleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

// NewStartActivityBattleLogic svcCtx 需要 Authority(意图与成局)、PlayerRedis(会话 / 战斗锁 / 位置)、BattleIDGen(发号)。
func NewStartActivityBattleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartActivityBattleLogic {
	return &StartActivityBattleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// StartActivityBattle 校验名单与活动上下文,逐人只读预检,在 SQL 提交意图与成局,然后后台推进并同步返回 battle_id
// (§6.18.2)。同一 request_id 重放返回原成局,正文不同按参数错拒绝。
//
// 返回约定:业务拒绝一律走 resp.reject(不回 gRPC 错误);gRPC 错误只有带会话调用时的 PermissionDenied。
// reject 为 MEMBER_OFFLINE / MEMBER_IN_BATTLE / MEMBER_NOT_READY 时 offender 是名单顺序里第一个不满足的人,
// 其余拒绝 offender 为 0。reject == NONE 时 battle_id 非 0;开局结局由成局原件决定,guild 巡检器读 SQL 终局记录兜底。
func (l *StartActivityBattleLogic) StartActivityBattle(in *matchpb.StartActivityBattleRequest) (*matchpb.StartActivityBattleResponse, error) {
	// 防御:带会话 = 客户端来源,一律拒绝。
	if detail, ok := ctxkeys.GetSessionDetails(l.ctx); ok && detail != nil {
		l.Errorf("[match] Rejecting client-originated MatchInternal.StartActivityBattle call session_id=%d player=%d",
			detail.GetSessionId(), detail.GetPlayerId())
		return nil, status.Error(codes.PermissionDenied, "MatchInternal is not callable by clients")
	}

	actx := in.GetActivityContext()
	kind := activityKindLabel(actx.GetKind())

	// 参数校验。
	if reason := validateStartActivityBattle(in); reason != "" {
		l.Errorf("[match] Invalid activity battle start parameters: %s config=%d members=%v ctx={%s}", reason, in.GetBattleConfigId(), in.GetMemberPlayerIds(), formatActivityContext(actx))
		return l.reject(kind, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0), nil
	}
	return l.startGlobalActivity(in)
}

// reject 组装拒绝响应并记同步出口指标。
func (l *StartActivityBattleLogic) reject(kind string, reject matchpb.ActivityBattleReject, offender uint64) *matchpb.StartActivityBattleResponse {
	metrics.ObserveActivityBattle(kind, activityRejectResult(reject))
	return &matchpb.StartActivityBattleResponse{Reject: reject, OffenderPlayerId: offender}
}

// validateStartActivityBattle 参数校验(§6.18.2 第 2 步),返回空串表示通过,否则是写进日志的原因。
// 名单:1..kMaxBattleTeamSize 人、非 0、不重复,发起人在首位;battle_config_id 非 0;
// 上下文:非空、kind 是已知的非 NONE 值,guild_id / activity_id / period_key / guild_period_key 非 0。
func validateStartActivityBattle(in *matchpb.StartActivityBattleRequest) string {
	members := in.GetMemberPlayerIds()
	if len(members) == 0 || len(members) > kMaxBattleTeamSize {
		return "人数越界"
	}
	seen := make(map[uint64]struct{}, len(members))
	for _, pid := range members {
		if pid == 0 {
			return "名单含 0"
		}
		if _, dup := seen[pid]; dup {
			return "名单重复"
		}
		seen[pid] = struct{}{}
	}
	if in.GetBattleConfigId() == 0 {
		return "battle_config_id 为 0"
	}
	actx := in.GetActivityContext()
	if actx == nil {
		return "缺少活动上下文"
	}
	if _, known := battlepb.EBattleActivityKind_name[int32(actx.GetKind())]; !known ||
		actx.GetKind() == battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_NONE {
		return "活动类型为 NONE 或未知"
	}
	if actx.GetGuildId() == 0 || actx.GetActivityId() == 0 || actx.GetPeriodKey() == 0 || actx.GetGuildPeriodKey() == 0 {
		return "活动上下文字段为 0"
	}
	if actx.GetInitiatorPlayerId() != members[0] {
		return "发起人不在名单首位"
	}
	return ""
}

// precheckMember 一名成员的只读预检(§6.18.2 第 3 步),返回 NONE 表示通过。
// 顺序固定:会话 → 战斗锁 → 位置 → 在途票据。战斗已结算的票在这里按需释放(同 JoinQueue),其余在途票拒绝。
// Redis 或 SQL 故障一律 INTERNAL(fail-closed)。提交点仍由 SQL 事务复核占用。
func (l *StartActivityBattleLogic) precheckMember(pid uint64) matchpb.ActivityBattleReject {
	session, err := playercontract.LoadSession(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] Activity battle precheck failed to read session player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL
	}
	if !playercontract.IsSessionOnline(session) {
		l.Infof("[match] Activity battle precheck: member offline player=%d", pid)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE
	}

	locked, err := playercontract.IsBattleLocked(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] Activity battle precheck failed to check battle lock player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL
	}
	if locked {
		l.Infof("[match] Activity battle precheck: member in battle player=%d", pid)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE
	}

	loc, err := playercontract.LoadLocation(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] Activity battle precheck failed to read location player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL
	}
	if loc == nil {
		l.Infof("[match] Activity battle precheck: member not in scene player=%d", pid)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY
	}

	ctx, cancel := context.WithTimeout(l.ctx, activityTicketReadBudget)
	defer cancel()
	ticket, err := l.svcCtx.Authority.Ticket(ctx, pid)
	if err != nil {
		l.Errorf("[match] Activity battle precheck failed to read ticket player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL
	}
	if ticket == nil || ticket.State == 0 || ticket.State >= 4 {
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE
	}
	released := false
	if ticket.State == 3 {
		if released, err = releaseGlobalFinished(ctx, l.svcCtx, ticket); err != nil {
			l.Errorf("[match] Activity battle precheck failed to release finished ticket player=%d: %v", pid, err)
			return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL
		}
	}
	if !released {
		l.Infof("[match] Activity battle precheck: member has an in-flight ticket player=%d ticket=%s state=%d",
			pid, ticket.Ticket, ticket.State)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY
	}
	return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE
}

// activityRejectResult 把拒绝原因映射到 match_activity_battle_total 的 result。
func activityRejectResult(reject matchpb.ActivityBattleReject) string {
	switch reject {
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT:
		return activityResultInvalid
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE:
		return activityResultOffline
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE:
		return activityResultInBattle
	case matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY:
		return activityResultNotReady
	default:
		return activityResultInternal
	}
}

// activityKindLabel 把活动类型收敛成指标 label:已知值取枚举名去掉 BATTLE_ACTIVITY_KIND_ 前缀后小写
// (guild_trial / none),未知值一律 unknown —— 调用方可传任意 int32,不能让 label 基数失控。
func activityKindLabel(kind battlepb.EBattleActivityKind) string {
	name, known := battlepb.EBattleActivityKind_name[int32(kind)]
	if !known {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "BATTLE_ACTIVITY_KIND_"))
}

// formatActivityContext 活动上下文的单行日志形式(nil 安全)。
func formatActivityContext(actx *battlepb.BattleActivityContext) string {
	if actx == nil {
		return "nil"
	}
	return fmt.Sprintf("kind=%s guild=%d activity=%d period=%d guild_period=%d initiator=%d",
		actx.GetKind(), actx.GetGuildId(), actx.GetActivityId(), actx.GetPeriodKey(),
		actx.GetGuildPeriodKey(), actx.GetInitiatorPlayerId())
}

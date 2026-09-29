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

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"shared/safego"
)

// 活动开局 MatchInternal.StartActivityBattle(帮会同道历练,docs/design/guild-phase2/06-activities.md §6.18.2)。
//
// 与整队开战(team_battle.go)的区别:名单来自 guild 邀请房间里全员已同意的成员,battle_id 在这里同步生成并
// 回给 guild(guild 要据此登记 guild_trial_battle),活动上下文原样透传给 battle。两者共用 matched 票、
// gather 管线与补偿矩阵;活动票**不设** team_id(§6.47)。
//
// 唯一合法调用方是 go/guild;客户端来源(带会话 metadata)在 match_service.go 的 sessionInterceptor
// 就被 PermissionDenied,本文件入口再防一道(拦截器链被改坏时仍 fail-closed)。

// 测试缝(照 matcher.go 的 runGatherFn、team_battle.go 的 createTeamTicketFn,生产值不可在运行期修改):
//
//	runActivityGatherFn    异步 gather 入口(真 RunActivityGather 要 gRPC 到 scene / battle);
//	createActivityTicketFn 建 matched 票,测试用它模拟"预检后被别人抢先写票""返回 err 但服务端已写入";
//	spawnActivityGatherFn  起后台 goroutine,测试换成同步执行,断言不必等墙钟。
var (
	runActivityGatherFn    = RunActivityGather
	createActivityTicketFn = createTicketIfAbsentCtx
	spawnActivityGatherFn  = safego.Go
)

// activityTicketRollbackBudget 建票失败后整批 CAS 删票的独立预算(不继承请求 ctx):至多 kMaxBattleTeamSize 条单 key Lua。
const activityTicketRollbackBudget = 3 * time.Second

// match_activity_battle_total 的 result 取值(metrics.activityBattleTotal)。
const (
	activityResultStarted      = "started"
	activityResultInvalid      = "invalid"
	activityResultOffline      = "offline"
	activityResultInBattle     = "in_battle"
	activityResultNotReady     = "not_ready"
	activityResultInternal     = "internal"
	activityResultGatherOK     = "gather_ok"
	activityResultGatherFailed = "gather_failed"
)

type StartActivityBattleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

// NewStartActivityBattleLogic svcCtx 需要 SharedRedis(会话 / 战斗锁 / 位置)、MatchRedis(票据)、
// BattleIDGen(发号);gather 另需 SceneNodes / BattleNodes(与 RunGather 相同)。
func NewStartActivityBattleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartActivityBattleLogic {
	return &StartActivityBattleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// StartActivityBattle 校验名单与活动上下文、逐人只读预检、发 battle_id、逐人建 matched 票,
// 然后异步启动 gather 并同步返回 battle_id(§6.18.2 第 1–7 步)。
//
// 返回约定:业务拒绝一律走 resp.reject(不回 gRPC 错误);gRPC 错误只有带会话调用时的 PermissionDenied。
// reject 为 MEMBER_OFFLINE / MEMBER_IN_BATTLE / MEMBER_NOT_READY 时 offender 是名单顺序里第一个不满足的人,
// 其余拒绝 offender 为 0。reject == NONE 时 battle_id 非 0,gather 已在后台启动;gather 之后失败时全员删票、
// 不回队列,该 battle_id 不会产生结果事件,由 guild 巡检器兜底。
//
// 时间预算:每人 4 次 Redis 读(自愈时多 2 次)+ 1 次建票 Lua;Redis 读写都受请求 ctx 约束(guild 给的预算),
// 只有回滚删票用独立预算。
func (l *StartActivityBattleLogic) StartActivityBattle(in *matchpb.StartActivityBattleRequest) (*matchpb.StartActivityBattleResponse, error) {
	// 1. 防御:带会话 = 客户端来源,一律拒绝。
	if detail, ok := ctxkeys.GetSessionDetails(l.ctx); ok && detail != nil {
		l.Errorf("[match] 拒绝客户端来源调用 MatchInternal.StartActivityBattle session_id=%d player=%d",
			detail.GetSessionId(), detail.GetPlayerId())
		return nil, status.Error(codes.PermissionDenied, "MatchInternal is not callable by clients")
	}

	actx := in.GetActivityContext()
	kind := activityKindLabel(actx.GetKind())
	members := in.GetMemberPlayerIds()
	configId := in.GetBattleConfigId()

	// 2. 参数校验。
	if reason := validateStartActivityBattle(in); reason != "" {
		l.Errorf("[match] 活动开局参数非法:%s config=%d members=%v ctx={%s}", reason, configId, members, formatActivityContext(actx))
		return l.reject(kind, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0), nil
	}

	// 3. 逐成员只读预检(名单顺序,第一个不满足即返回)。
	zones := make(map[uint64]uint32, len(members))
	for _, pid := range members {
		reject, zone := l.precheckMember(pid)
		if reject != matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE {
			offender := pid
			if reject == matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL {
				offender = 0
			}
			return l.reject(kind, reject, offender), nil
		}
		zones[pid] = zone
	}

	// 4. battle_id:只能由 match 节点生产(宪法 §7 SnowFlake 节点隔离);失败 fail-closed,不得用 0 顶替。
	battleID, err := l.svcCtx.BattleIDGen.Generate()
	if err != nil {
		l.Errorf("[match] 活动开局 battle_id 生成失败 members=%v ctx={%s}: %v", members, formatActivityContext(actx), err)
		return l.reject(kind, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}

	// 5. 逐人建 matched 票(失败时本函数内已回滚)。
	tickets, reject, offender := l.createMatchedTickets(configId, battleID, members, zones)
	if reject != matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE {
		return l.reject(kind, reject, offender), nil
	}

	// 5.5 调用方已放弃(guild 预算到期 / 连接断开)就不再开局:guild 拿不到 battle_id 会把房间判为失败,
	//     此时开局等于把玩家拉进一场 guild 以为没开的战斗。回滚票据后按 INTERNAL 收场。
	//     本检查之后到响应送达之间仍有极窄窗口,由 guild 结算的"补登记"兜住(§6.29)。
	if err := l.ctx.Err(); err != nil {
		l.Errorf("[match] 活动开局前调用方已放弃,回滚票据 battle=%d members=%v: %v", battleID, members, err)
		l.rollbackTickets(battleID, members, tickets)
		return l.reject(kind, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}

	// 6. 异步 gather。上下文深拷贝:请求对象归 gRPC 框架,返回后不得再被 gather 引用。
	actxCopy := proto.Clone(actx).(*battlepb.BattleActivityContext)
	svcCtx := l.svcCtx
	gatherMembers := append([]uint64(nil), members...)
	spawnActivityGatherFn("match.gather.activity", func() {
		ok := runActivityGatherFn(svcCtx, configId, gatherMembers, tickets, battleID, actxCopy)
		result := activityResultGatherOK
		if !ok {
			result = activityResultGatherFailed
		}
		metrics.ObserveActivityBattle(kind, result)
	})

	// 7. 同步返回 battle_id。
	l.Infof("[match] 活动开局已受理 battle=%d config=%d members=%v zones=%v ctx={%s}",
		battleID, configId, members, zones, formatActivityContext(actx))
	metrics.ObserveActivityBattle(kind, activityResultStarted)
	return &matchpb.StartActivityBattleResponse{BattleId: battleID}, nil
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

// precheckMember 一名成员的只读预检(§6.18.2 第 3 步),返回 (NONE, 当前 zone) 表示通过。
// 顺序固定:会话 → 战斗锁 → 位置 → 在途票据。在途票据先按 JoinQueue 同一套规则自愈(healOrphanTicket:
// 清 ready 残留、清"queued 但不在队列"的孤儿),它依赖"已确认没有战斗锁",所以排在锁检查之后。
// Redis 故障一律 INTERNAL(fail-closed)。
func (l *StartActivityBattleLogic) precheckMember(pid uint64) (matchpb.ActivityBattleReject, uint32) {
	session, err := playercontract.LoadSession(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] 活动开局预检读会话失败 player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if !playercontract.IsSessionOnline(session) {
		l.Infof("[match] 活动开局预检:成员不在线 player=%d", pid)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_OFFLINE, 0
	}

	locked, err := playercontract.IsBattleLocked(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] 活动开局预检查战斗锁失败 player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if locked {
		l.Infof("[match] 活动开局预检:成员在战斗中 player=%d", pid)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_IN_BATTLE, 0
	}

	loc, err := playercontract.LoadLocation(l.ctx, l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] 活动开局预检读位置失败 player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if loc == nil {
		l.Infof("[match] 活动开局预检:成员不在场景 player=%d", pid)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 0
	}

	// 票据读与自愈沿用既有无 ctx 的 Redis 调用(与 JoinQueue / 整队开战共用、不改其行为),先检查 ctx。
	if err := l.ctx.Err(); err != nil {
		l.Errorf("[match] 活动开局预检时请求已结束 player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	existing, err := loadTicket(l.svcCtx, pid)
	if err != nil {
		l.Errorf("[match] 活动开局预检读票据失败 player=%d: %v", pid, err)
		return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
	}
	if existing != nil {
		healed, err := healOrphanTicket(l.svcCtx, l.Logger, pid, existing)
		if err != nil {
			l.Errorf("[match] 活动开局预检校验既有票据失败 player=%d: %v", pid, err)
			return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
		}
		if !healed {
			l.Infof("[match] 活动开局预检:成员有在途票据 player=%d ticket=%s state=%s",
				pid, existing.Ticket, existing.State)
			return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, 0
		}
	}
	return matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, loc.GetZoneId()
}

// createMatchedTickets 逐人建 matched 票(§6.18.2 第 5 步):mode=PVE_TEAM、config、zone,不入队(QueueKey 空,
// 同 PVE_SOLO)、不设 team_id,TTL = matchedTicketTTLFor(人数)。每人的 ticket id 事先生成。
//
// 成功返回 (tickets, NONE, 0)。某人"已有票据"(预检之后被别人抢先)→ NOT_READY + 该成员;
// 出错 → INTERNAL + 0。两种失败都先回滚:回滚集合 = 建票返回 true 的成员 ∪ 返回 err 的成员
// (Eval 超时 / 断连时结果未知,可能已写入),逐个按本次 ticket id CAS 删;"已有票据"的成员持有的是
// 别人的票,不在集合里。
func (l *StartActivityBattleLogic) createMatchedTickets(configId uint32, battleID uint64, members []uint64,
	zones map[uint64]uint32,
) (map[uint64]string, matchpb.ActivityBattleReject, uint64) {
	ttl := matchedTicketTTLFor(l.svcCtx, uint32(len(members)))
	tickets := make(map[uint64]string, len(members))
	for _, pid := range members {
		tickets[pid] = uuid.New().String()
	}
	enqueuedAt := nowMs()
	owned := make([]uint64, 0, len(members))
	for _, pid := range members {
		created, err := createActivityTicketFn(l.ctx, l.svcCtx, pid, &queueTicket{
			Ticket:       tickets[pid],
			Mode:         int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM),
			Config:       configId,
			State:        ticketStateMatched,
			EnqueuedAtMs: enqueuedAt,
			ZoneId:       zones[pid],
		}, ttl)
		if err != nil {
			l.Errorf("[match] 活动开局建票失败(结果未知,按已写入回滚) battle=%d player=%d: %v", battleID, pid, err)
			owned = append(owned, pid)
			l.rollbackTickets(battleID, owned, tickets)
			return nil, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0
		}
		if !created {
			l.Infof("[match] 活动开局建票冲突:成员已有票据 battle=%d player=%d", battleID, pid)
			l.rollbackTickets(battleID, owned, tickets)
			return nil, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_MEMBER_NOT_READY, pid
		}
		owned = append(owned, pid)
	}
	return tickets, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, 0
}

// rollbackTickets 用独立 ctx(activityTicketRollbackBudget)逐个按本次 ticket id CAS 删票:
// 请求 ctx 已取消也照做。删不掉的票靠 matched TTL 过期自愈,期间该成员再发起会得 NOT_READY。
func (l *StartActivityBattleLogic) rollbackTickets(battleID uint64, owned []uint64, tickets map[uint64]string) {
	ctx, cancel := context.WithTimeout(context.Background(), activityTicketRollbackBudget)
	defer cancel()
	for _, pid := range owned {
		deleted, err := deleteTicketIfOwnedCtx(ctx, l.svcCtx, pid, tickets[pid])
		if err != nil {
			logx.Errorf("[match] 活动开局回滚删票失败(靠 matched TTL 自愈) battle=%d player=%d: %v", battleID, pid, err)
			continue
		}
		if !deleted {
			logx.Infof("[match] 活动开局回滚:票据不是本次写入或已不存在 battle=%d player=%d", battleID, pid)
		}
	}
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

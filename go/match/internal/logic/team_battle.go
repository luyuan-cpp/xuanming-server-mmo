package logic

import (
	"context"
	"time"

	"match/internal/svc"

	matchpb "proto/match"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// 整队开战的票据域实现(设计文档 docs/design/team-system.md §E.1 第 3、5、7、8 步)。
//
// 分工(依赖方向 team → 接口 ← logic):
//   - team 包(internal/team)持有队伍记录与编排:bindCaller 读、惰性转让、队长 / 开战锁校验、
//     roster 排序、会话 / 战斗锁 / 位置预检、开战锁 CAS(ver 钉死、整轮重来)、EndMatch 专用循环、推送;
//   - 本文件只做"票据领域"的几件事,方法签名只用 Go 内建类型,方法集满足 team.BattleStarter。
//     logic 不 import team:team 依赖 proto/team 生成物,import 它会让 match 主二进制在 regen 前
//     连带编不过;logic 也就不认识 TeamRecord,只拿 team 给的有序名单。
//
// 线程安全:TeamBattleStarter 无可变状态,可并发调用。

// teamTicketRollbackBudget 建票失败后整批 CAS 删票的独立预算(不继承请求 ctx,§E.1 第 7 步)。
// 至多 kMaxBattleTeamSize 条单 key Lua。
const teamTicketRollbackBudget = 3 * time.Second

// teamMatchLockSlackSeconds 开战锁时长在"matched 票 TTL + 补偿窗口"之上的余量(§E.1 第 6 步)。
const teamMatchLockSlackSeconds = 10

// 测试缝(照 runGatherFn 的包级变量模式,生产值不可在运行期修改):
//
//	runTeamGatherFn     整队 gather 入口(真 RunTeamGather 要 gRPC 到 scene / battle);
//	createTeamTicketFn  建票入口,测试用它模拟"返回 err 但服务端已写入"(§I.3 #29)。
var (
	runTeamGatherFn    = RunTeamGather
	createTeamTicketFn = createTicketIfAbsentCtx
)

// TeamBattleStarter 整队开战的票据域端口实现(team.BattleStarter),match_service.go 装配时注入
// team.NewService。
type TeamBattleStarter struct {
	svcCtx *svc.ServiceContext
}

// NewTeamBattleStarter svcCtx 需要 MatchRedis(票据)与 Config(PveTeamSizeByConfigId / matched TTL);
// gather 另需 BattleIDGen / SceneNodes / BattleNodes / SharedRedis(与 RunGather 相同)。
func NewTeamBattleStarter(svcCtx *svc.ServiceContext) *TeamBattleStarter {
	return &TeamBattleStarter{svcCtx: svcCtx}
}

// TeamSizeFor 副本 battleConfigId 的组队开战人数上限:与 JoinQueue / matcher 的 PVE_TEAM 凑满人数
// 同一口径(requiredPlayers:PveTeamSizeByConfigId,按 kMaxBattleTeamSize 收口);0 = 未开放组队。
func (s *TeamBattleStarter) TeamSizeFor(battleConfigId uint32) uint32 {
	return requiredPlayers(s.svcCtx, int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), battleConfigId)
}

// MatchLockTTLSeconds memberCount 人的开战锁时长(秒,§E.1 第 6 步):matched 票 TTL(gather 最坏链路)
// + 补偿窗口(逐人 CancelBattlePrepare)+ 余量。5 人 = 48 + 25 + 10 = 83s。
// 锁的绝对截止时间由 team 用 SharedRedis TIME 计算(§C.4),这里只给时长。
func (s *TeamBattleStarter) MatchLockTTLSeconds(memberCount int) int {
	return matchedTicketTTLFor(s.svcCtx, uint32(memberCount)) + compensationTicketTTLFor(memberCount) + teamMatchLockSlackSeconds
}

// TicketBlocked 一名成员的票据预检(§E.1 第 5 步最后一项):没有票据 → false;有票据先按 JoinQueue
// 同一套规则自愈(healOrphanTicket:清 ready 残留、清"queued 但不在队列"的孤儿),自愈后仍在途 → true。
// err 非 nil 时返回 true(调用方 fail-closed)。
//
// 前置条件:调用方已确认该成员没有 battle:lock(ready 分支依赖它,与 JoinQueue 同序)。
// 票据读与自愈沿用既有无 ctx 的 Redis 调用(与 JoinQueue 共用、不改其行为),入口先检查 ctx。
func (s *TeamBattleStarter) TicketBlocked(ctx context.Context, playerId uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	existing, err := loadTicket(s.svcCtx, playerId)
	if err != nil {
		return true, err
	}
	if existing == nil {
		return false, nil
	}
	healed, err := healOrphanTicket(s.svcCtx, logx.WithContext(ctx), playerId, existing)
	if err != nil {
		return true, err
	}
	return !healed, nil
}

// CreateMatchedTickets 为锁内名单逐人建 matched 票(§E.1 第 7 步):mode=PVE_TEAM、config、
// zone(预检读到的位置,缺项为 0)、team_id,不入队(QueueKey 空),TTL = matchedTicketTTLFor(len(roster))。
// 每人的 ticket id 事先生成。
//
// 成功返回 (tickets, 0)。任一人"已有票据"或出错 → 回滚后返回 (nil, 该成员):
// 回滚集合 = 建票返回 true 的成员 ∪ 返回 err 的成员(Eval 超时 / 断连 / 集群重试时结果未知,可能已写入),
// 逐个按本次 ticket id CAS 删;"已有票据"的成员持有的是别人的票,不在集合里。
// 回滚用独立 ctx(teamTicketRollbackBudget),请求 ctx 已取消也照做。
// 前置条件:roster 非空、无重复;ctx 只约束建票本身。
func (s *TeamBattleStarter) CreateMatchedTickets(ctx context.Context, battleConfigId uint32, teamId uint64,
	roster []uint64, zones map[uint64]uint32,
) (map[uint64]string, uint64) {
	log := logx.WithContext(ctx)
	ttl := matchedTicketTTLFor(s.svcCtx, uint32(len(roster)))
	tickets := make(map[uint64]string, len(roster))
	for _, pid := range roster {
		tickets[pid] = uuid.New().String()
	}
	enqueuedAt := nowMs()
	owned := make([]uint64, 0, len(roster))
	var failed uint64
	for _, pid := range roster {
		created, err := createTeamTicketFn(ctx, s.svcCtx, pid, &queueTicket{
			Ticket:       tickets[pid],
			Mode:         int32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM),
			Config:       battleConfigId,
			State:        ticketStateMatched,
			EnqueuedAtMs: enqueuedAt,
			ZoneId:       zones[pid],
			TeamId:       teamId,
		}, ttl)
		if err != nil {
			log.Errorf("[match] 整队开战建票失败(结果未知,按已写入回滚) team=%d player=%d: %v", teamId, pid, err)
			owned = append(owned, pid)
			failed = pid
			break
		}
		if !created {
			log.Infof("[match] 整队开战建票冲突:成员已有票据 team=%d player=%d", teamId, pid)
			failed = pid
			break
		}
		owned = append(owned, pid)
	}
	if failed == 0 {
		return tickets, 0
	}
	s.rollbackTickets(teamId, owned, tickets)
	return nil, failed
}

// rollbackTickets 建票失败的补偿:独立 ctx 逐个按本次 ticket id CAS 删票。
func (s *TeamBattleStarter) rollbackTickets(teamId uint64, owned []uint64, tickets map[uint64]string) {
	ctx, cancel := context.WithTimeout(context.Background(), teamTicketRollbackBudget)
	defer cancel()
	for _, pid := range owned {
		deleted, err := deleteTicketIfOwnedCtx(ctx, s.svcCtx, pid, tickets[pid])
		if err != nil {
			// 删不掉的票靠 matched TTL 过期(§E.3),期间该成员单排回 AlreadyQueued。
			logx.Errorf("[match] 整队开战回滚删票失败(靠 matched TTL 自愈) team=%d player=%d: %v", teamId, pid, err)
			continue
		}
		if !deleted {
			logx.Infof("[match] 整队开战回滚:票据不是本次写入或已不存在 team=%d player=%d", teamId, pid)
		}
	}
}

// RunGather 同步执行一次整队 gather(RunTeamGather:roster 原序、失败全员删票),返回是否开局成功。
// team 负责放进后台 goroutine,并在返回后清开战锁(EndMatch)。
func (s *TeamBattleStarter) RunGather(battleConfigId uint32, roster []uint64, tickets map[uint64]string) bool {
	return runTeamGatherFn(s.svcCtx, battleConfigId, roster, tickets)
}

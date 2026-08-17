package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// ticket 状态机(QueueState 五态映射见 getqueuestatuslogic.go):
//
//	queued  -> 在 Redis 队列里等待凑单(QUEUE_STATE_QUEUED)
//	matched -> 已被 matcher 弹出,gather 管线执行中(QUEUE_STATE_MATCHED)
//	ready   -> CreateBattle 成功,等 battle 推 BattleStartS2C(QUEUE_STATE_READY,短 TTL 自清)
//
// ticket 不存在 = QUEUE_STATE_NOT_QUEUED。ENTERING 是场景匹配(5v5/3v3)的
// 进场态,回合制战斗没有进场步骤,一期不产生该状态。
const (
	ticketStateQueued  = "queued"
	ticketStateMatched = "matched"
	ticketStateReady   = "ready"
)

// ticket hash 字段名。
const (
	ticketFieldTicket     = "ticket"
	ticketFieldMode       = "mode"
	ticketFieldConfig     = "config"
	ticketFieldState      = "state"
	ticketFieldEnqueuedAt = "enqueued_at_ms"
	ticketFieldBattleId   = "battle_id"
)

// queueTicket 是 match:ticket:{player_id} 的内存镜像。
type queueTicket struct {
	Ticket       string
	Mode         int32
	Config       uint32
	State        string
	EnqueuedAtMs uint64
}

// authoritativePlayerID 取权威 player_id:客户端直达协议(gate gRPC)时以
// x-session-detail-bin 里的身份为准,请求体里的 player_id 仅供内部调用使用
// (照 login 的 session metadata 模式)。带了 session 但与请求体不一致时记日志。
func authoritativePlayerID(ctx context.Context, reqPlayerId uint64) uint64 {
	if detail, ok := ctxkeys.GetSessionDetails(ctx); ok && detail.GetPlayerId() != 0 {
		if reqPlayerId != 0 && reqPlayerId != detail.GetPlayerId() {
			logx.Errorf("[match] 请求体 player_id=%d 与 session 权威身份 %d 不一致,以 session 为准",
				reqPlayerId, detail.GetPlayerId())
		}
		return detail.GetPlayerId()
	}
	return reqPlayerId
}

// isPlayerBattleLocked 咨询性检查 battle:lock:{player_id}(scene 写)。
// 权威判定仍在 scene 的 InBattleComp;这里只是提前挡明显不合法的请求。
// Redis 出错时按"有锁"处理(fail-closed:宁可拒绝排队,不可放进双战斗)。
func isPlayerBattleLocked(svcCtx *svc.ServiceContext, playerId uint64) (bool, error) {
	ok, err := svcCtx.Redis.Exists(battleLockKey(playerId))
	if err != nil {
		return true, fmt.Errorf("查询 battle:lock 失败: %w", err)
	}
	return ok, nil
}

// loadTicket 读取玩家 ticket;不存在返回 (nil, nil)。
func loadTicket(svcCtx *svc.ServiceContext, playerId uint64) (*queueTicket, error) {
	fields, err := svcCtx.Redis.Hgetall(matchTicketKey(playerId))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	if len(fields) == 0 {
		return nil, nil
	}
	mode, _ := strconv.ParseInt(fields[ticketFieldMode], 10, 32)
	config, _ := strconv.ParseUint(fields[ticketFieldConfig], 10, 32)
	enqueuedAt, _ := strconv.ParseUint(fields[ticketFieldEnqueuedAt], 10, 64)
	return &queueTicket{
		Ticket:       fields[ticketFieldTicket],
		Mode:         int32(mode),
		Config:       uint32(config),
		State:        fields[ticketFieldState],
		EnqueuedAtMs: enqueuedAt,
	}, nil
}

// writeTicket 写入 ticket hash 并挂兜底 TTL(正常路径由取消/开战收尾,
// TTL 只挡进程崩溃等异常残留)。
func writeTicket(svcCtx *svc.ServiceContext, playerId uint64, t *queueTicket) error {
	key := matchTicketKey(playerId)
	if err := svcCtx.Redis.Hmset(key, map[string]string{
		ticketFieldTicket:     t.Ticket,
		ticketFieldMode:       strconv.FormatInt(int64(t.Mode), 10),
		ticketFieldConfig:     strconv.FormatUint(uint64(t.Config), 10),
		ticketFieldState:      t.State,
		ticketFieldEnqueuedAt: strconv.FormatUint(t.EnqueuedAtMs, 10),
	}); err != nil {
		return err
	}
	ttl := int(svcCtx.Config.TicketTTLSeconds)
	if ttl <= 0 {
		ttl = 21600
	}
	return svcCtx.Redis.Expire(key, ttl)
}

// setTicketState 更新 ticket 状态。
func setTicketState(svcCtx *svc.ServiceContext, playerId uint64, state string) error {
	return svcCtx.Redis.Hset(matchTicketKey(playerId), ticketFieldState, state)
}

// markTicketReady 把 ticket 置为 ready 并收紧 TTL:短暂供 GetQueueStatus
// 查询后自清,战斗内状态改由 battle:lock / InBattleComp 表达。
func markTicketReady(svcCtx *svc.ServiceContext, playerId uint64, battleId uint64) {
	key := matchTicketKey(playerId)
	if err := svcCtx.Redis.Hset(key, ticketFieldState, ticketStateReady); err != nil {
		logx.Errorf("[match] 更新 ticket ready 失败 player=%d battle=%d: %v", playerId, battleId, err)
		return
	}
	if err := svcCtx.Redis.Hset(key, ticketFieldBattleId, strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[match] 写 ticket battle_id 失败 player=%d battle=%d: %v", playerId, battleId, err)
	}
	ttl := int(svcCtx.Config.ReadyTicketTTLSeconds)
	if ttl <= 0 {
		ttl = 60
	}
	if err := svcCtx.Redis.Expire(key, ttl); err != nil {
		logx.Errorf("[match] 收紧 ticket TTL 失败 player=%d: %v", playerId, err)
	}
}

// deleteTicket 删除 ticket(取消 / gather 失败的出局者)。
func deleteTicket(svcCtx *svc.ServiceContext, playerId uint64) {
	if _, err := svcCtx.Redis.Del(matchTicketKey(playerId)); err != nil {
		logx.Errorf("[match] 删除 ticket 失败 player=%d: %v", playerId, err)
	}
}

// requeueFront 把成员按原相对顺序放回队首(补偿矩阵:组队场景失败成员回队首)。
// Lpush 逐个从末尾往前推,最先弹出的成员最终仍在最前。
func requeueFront(svcCtx *svc.ServiceContext, mode int32, config uint32, members []uint64) {
	key := matchQueueKey(mode, config)
	for i := len(members) - 1; i >= 0; i-- {
		playerId := members[i]
		if _, err := svcCtx.Redis.Lpush(key, strconv.FormatUint(playerId, 10)); err != nil {
			logx.Errorf("[match] 回队首失败 player=%d queue=%s: %v", playerId, key, err)
			continue
		}
		if err := setTicketState(svcCtx, playerId, ticketStateQueued); err != nil {
			logx.Errorf("[match] 回队首后恢复 ticket 状态失败 player=%d: %v", playerId, err)
		}
	}
	logx.Infof("[match] %d 名成员已回队首 queue=%s", len(members), key)
}

// nowMs 返回当前 Unix 毫秒。
func nowMs() uint64 {
	return uint64(time.Now().UnixMilli())
}

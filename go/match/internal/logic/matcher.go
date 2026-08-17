package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"match/internal/metrics"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"shared/safego"
)

// releaseLockScript 按持有者标记释放 matcher 锁:值不匹配说明锁已过期并被
// 别的实例续拿,此时删除会误伤对方的临界区,必须放弃。
const releaseLockScript = `if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end`

// StartMatcherLoop 启动 matcher 定时循环(调用方放 goroutine)。
// 多实例安全:每个 (mode, battle_config_id) 队列的凑单临界区由
// match:matcher:lock:{mode}:{config} 的 SETNX 锁保护,拿不到锁跳过本轮
// (设计文档 §5.4)。锁只覆盖"弹出成组"这一小段;弹出后的 gather 在
// 独立 goroutine 里执行,不占锁。
func StartMatcherLoop(ctx context.Context, svcCtx *svc.ServiceContext) {
	interval := time.Duration(svcCtx.Config.MatcherIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	safego.Loop(ctx, "match.matcher", interval, func(ctx context.Context) {
		runMatcherRound(ctx, svcCtx)
	})
}

// runMatcherRound 扫描所有存在的队列 key 并逐个尝试凑单。
// 队列 key 由 JoinQueue 按需创建,SCAN 发现即可,无需预注册清单。
func runMatcherRound(ctx context.Context, svcCtx *svc.ServiceContext) {
	cursor := uint64(0)
	for {
		keys, next, err := svcCtx.Redis.Scan(cursor, "match:queue:*", 64)
		if err != nil {
			logx.Errorf("[matcher] 扫描队列 key 失败 cursor=%d: %v", cursor, err)
			return
		}
		for _, key := range keys {
			if ctx.Err() != nil {
				return
			}
			matchQueueOnce(svcCtx, key)
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

// matchQueueOnce 对单个队列执行一次凑单尝试。
func matchQueueOnce(svcCtx *svc.ServiceContext, queueKey string) {
	var mode int32
	var config uint32
	if _, err := fmt.Sscanf(queueKey, "match:queue:%d:%d", &mode, &config); err != nil {
		logx.Errorf("[matcher] 无法解析队列 key %q: %v", queueKey, err)
		return
	}

	required := requiredPlayers(svcCtx, mode, config)
	if required == 0 {
		// 配置被摘掉后残留的队列:不动数据,只告警,等运维处理。
		logx.Errorf("[matcher] 队列 %s 无凑满人数配置,跳过", queueKey)
		return
	}

	depth, err := svcCtx.Redis.Llen(queueKey)
	if err != nil {
		logx.Errorf("[matcher] Llen %s 失败: %v", queueKey, err)
		return
	}
	metrics.SetQueueDepth(matchpb.MatchMode(mode).String(), strconv.FormatUint(uint64(config), 10), depth)
	if depth < int(required) {
		return
	}

	// 凑单临界区:SETNX 锁,拿不到跳过本轮。
	lockKey := matcherLockKey(mode, config)
	lockTTL := int(svcCtx.Config.MatcherLockTTLSeconds)
	if lockTTL <= 0 {
		lockTTL = 10
	}
	acquired, err := svcCtx.Redis.SetnxEx(lockKey, svcCtx.InstanceID, lockTTL)
	if err != nil {
		logx.Errorf("[matcher] 抢锁失败 %s: %v", lockKey, err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if _, err := svcCtx.Redis.Eval(releaseLockScript, []string{lockKey}, svcCtx.InstanceID); err != nil {
			logx.Errorf("[matcher] 释放锁失败 %s: %v", lockKey, err)
		}
	}()

	// 持锁期间可以连续凑多组(队列深度允许时),摊薄扫描开销。
	for {
		members, ok := popGroup(svcCtx, queueKey, required)
		if !ok {
			return
		}
		// 弹出即凑单成功:先把票据推进 matched 态(此后取消太迟),
		// 再放 goroutine 跑 gather —— gather 是多跳 RPC,不能占凑单锁。
		for _, playerId := range members {
			if err := setTicketState(svcCtx, playerId, ticketStateMatched); err != nil {
				logx.Errorf("[matcher] 推进 ticket matched 失败 player=%d: %v", playerId, err)
			}
		}
		logx.Infof("[matcher] 凑单成功 queue=%s members=%v", queueKey, members)
		group := members
		safego.Go("match.gather.queue", func() {
			RunGather(svcCtx, matchpb.MatchMode(mode), config, group, true)
		})
	}
}

// requiredPlayers 返回该队列凑满所需人数;未知模式/未配置返回 0。
func requiredPlayers(svcCtx *svc.ServiceContext, mode int32, config uint32) uint32 {
	switch matchpb.MatchMode(mode) {
	case matchpb.MatchMode_MATCH_MODE_1V1:
		return 2
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		return svcCtx.Config.PveTeamSizeFor(config)
	default:
		// PVE_SOLO 不入队;5v5/3v3/切磋没有队列。
		return 0
	}
}

// popGroup 从队首弹出并校验,凑不满时把已弹出的有效成员按原序放回队首。
// 校验规则:ticket 必须存在且处于 queued 态(取消路径先删票再 Lrem,
// 弹出后发现票据缺失即认定已取消);battle:lock 存在的玩家出局并删票
// (排队期间通过切磋等入口进了别的战斗)。
func popGroup(svcCtx *svc.ServiceContext, queueKey string, required uint32) ([]uint64, bool) {
	var members []uint64
	for uint32(len(members)) < required {
		raw, err := svcCtx.Redis.Lpop(queueKey)
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				logx.Errorf("[matcher] Lpop %s 失败: %v", queueKey, err)
			}
			break
		}
		playerId, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			logx.Errorf("[matcher] 队列 %s 出现非法成员 %q,丢弃", queueKey, raw)
			continue
		}

		ticket, err := loadTicket(svcCtx, playerId)
		if err != nil {
			logx.Errorf("[matcher] 读 ticket 失败 player=%d,本轮放回队首: %v", playerId, err)
			// Redis 抖动:把人放回队首,下一轮重试,不做出局判定。
			if _, err := svcCtx.Redis.Lpush(queueKey, raw); err != nil {
				logx.Errorf("[matcher] 放回队首失败 player=%d: %v", playerId, err)
			}
			break
		}
		if ticket == nil || ticket.State != ticketStateQueued {
			// 已取消 / 状态异常:静默丢弃(取消路径已删票)。
			logx.Infof("[matcher] 丢弃无效队列成员 player=%d(ticket 缺失或状态异常)", playerId)
			continue
		}
		locked, err := isPlayerBattleLocked(svcCtx, playerId)
		if err != nil {
			logx.Errorf("[matcher] 查战斗锁失败 player=%d,本轮放回队首: %v", playerId, err)
			if _, err := svcCtx.Redis.Lpush(queueKey, raw); err != nil {
				logx.Errorf("[matcher] 放回队首失败 player=%d: %v", playerId, err)
			}
			break
		}
		if locked {
			logx.Infof("[matcher] 队列成员已在战斗中,出局删票 player=%d", playerId)
			deleteTicket(svcCtx, playerId)
			continue
		}
		members = append(members, playerId)
	}

	if uint32(len(members)) == required {
		return members, true
	}
	// 凑不满:有效成员按原相对顺序放回队首,等下一轮。
	if len(members) > 0 {
		for i := len(members) - 1; i >= 0; i-- {
			if _, err := svcCtx.Redis.Lpush(queueKey, strconv.FormatUint(members[i], 10)); err != nil {
				logx.Errorf("[matcher] 凑不满回队首失败 player=%d: %v", members[i], err)
			}
		}
	}
	return nil, false
}

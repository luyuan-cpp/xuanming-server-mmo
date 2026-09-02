package logic

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"match/internal/discovery"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 观战索引与互斥标记(设计文档 §10.4;spectate:* 只有 match 读写,不变量 7):
//
//	spectate:battle:{battle_id}   string  SpectateBattleRecord pb,TTL = 战斗最长期限 + 60s
//	spectate:battles:active       zset    member=battle_id 十进制,score=created_at_ms
//	spectate:watching:{player_id} string  battle_id 十进制,TTL 同上(gather 互斥清退反查用)
//
// ZSET 成员没有 TTL,靠三条路清理:AddObserver 报房间不存在时懒剔除、
// 读到过期 score 时现场剔除、matcher loop 每轮 ZREMRANGEBYSCORE 兜底。
const spectateBattlesActiveKey = "spectate:battles:active"

// battle 节点观战 RPC 超时(与 gather 的 prepare/rollback 同量级)。
const (
	addObserverTimeout    = 3 * time.Second
	removeObserverTimeout = 3 * time.Second
)

// battle 侧 AddObserver 的房间不存在信号 = table.CommonError_kEntityIsNull
// (与 battle_room_manager.cpp AddObserver 契约对齐,match 据此懒剔除索引);
// 观众满=kRateLimitExceeded、观众是参战者=kInvalidParameter 不触发剔除
// —— 那两种情况战斗仍在打,索引是好的。

func spectateBattleKey(battleId uint64) string {
	return fmt.Sprintf("spectate:battle:%d", battleId)
}

func spectateWatchingKey(playerId uint64) string {
	return fmt.Sprintf("spectate:watching:%d", playerId)
}

// spectateTTLSeconds 观战索引 TTL:战斗最长期限 + 60s 余量。余量吃掉
// deadline 强制收尾与索引清理之间的时间差,保证 TTL 到点时战斗必已收尾。
func spectateTTLSeconds(svcCtx *svc.ServiceContext) int {
	maxDuration := int(svcCtx.Config.BattleMaxDurationSeconds)
	if maxDuration <= 0 {
		maxDuration = 300
	}
	return maxDuration + 60
}

// registerSpectateBattle 把开局成功的战斗登记进观战索引(gather 成功收尾时调,
// 设计决策 D9)。失败只记日志:索引缺失只是该场不可观战,不反悔已建成的战斗。
func registerSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64, battleNodeId uint32,
	mode matchpb.MatchMode, battleConfigId uint32, playerNames []string, createdAtMs uint64,
) {
	record := &matchpb.SpectateBattleRecord{
		Summary: &matchpb.BattleWatchSummary{
			BattleId:       battleId,
			Mode:           mode,
			BattleConfigId: battleConfigId,
			PlayerNames:    playerNames,
			CreatedAtMs:    createdAtMs,
		},
		BattleNodeId: battleNodeId,
	}
	raw, err := proto.Marshal(record)
	if err != nil {
		logx.Errorf("[spectate] 序列化观战记录失败 battle=%d: %v", battleId, err)
		return
	}
	// 先写记录再入 ZSET:列表/随机挑中的成员保证记录可读(反序不变量,
	// 与 JoinQueue 先写 ticket 再入队同理)。
	if err := svcCtx.Redis.Setex(spectateBattleKey(battleId), string(raw), spectateTTLSeconds(svcCtx)); err != nil {
		logx.Errorf("[spectate] 写观战记录失败 battle=%d: %v", battleId, err)
		return
	}
	if _, err := svcCtx.Redis.Zadd(spectateBattlesActiveKey, int64(createdAtMs),
		strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] 观战索引 ZADD 失败 battle=%d: %v", battleId, err)
		return
	}
	logx.Infof("[spectate] 战斗已登记可观战 battle=%d node=%d mode=%s players=%d",
		battleId, battleNodeId, mode.String(), len(playerNames))
}

// loadSpectateBattle 读活跃战斗记录;不存在(TTL 已清/从未登记)返回 (nil, nil)。
func loadSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) (*matchpb.SpectateBattleRecord, error) {
	raw, err := svcCtx.Redis.Get(spectateBattleKey(battleId))
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	record := &matchpb.SpectateBattleRecord{}
	if err := proto.Unmarshal([]byte(raw), record); err != nil {
		return nil, fmt.Errorf("SpectateBattleRecord 反序列化失败: %w", err)
	}
	return record, nil
}

// removeSpectateBattle 懒剔除:battle 报房间不存在 / 记录缺失或损坏时清索引。
func removeSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) {
	if _, err := svcCtx.Redis.Del(spectateBattleKey(battleId)); err != nil {
		logx.Errorf("[spectate] 删观战记录失败 battle=%d: %v", battleId, err)
	}
	if _, err := svcCtx.Redis.Zrem(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] 观战索引 ZREM 失败 battle=%d: %v", battleId, err)
	}
}

// spectateStaleBeforeMs 返回过期分界:score(created_at_ms)早于该值的战斗
// 必已按 deadline 收尾,索引条目是残留。
func spectateStaleBeforeMs(svcCtx *svc.ServiceContext) uint64 {
	return nowMs() - uint64(spectateTTLSeconds(svcCtx))*1000
}

// pickRandomBattle 随机挑一场可观战战斗:ZCARD 取规模 + 随机下标 ZRANGE 单点读,
// 避免全量拉 ZSET。挑到过期/非法成员现场懒剔除后换一个,最多尝试 3 次;
// 挑不到返回 (0, nil)。随机用 math/rand 即可:选场不要求确定性
// (种子 RNG 纪律只约束 battle 引擎内部)。
func pickRandomBattle(svcCtx *svc.ServiceContext) (uint64, error) {
	staleBefore := spectateStaleBeforeMs(svcCtx)
	for attempt := 0; attempt < 3; attempt++ {
		count, err := svcCtx.Redis.Zcard(spectateBattlesActiveKey)
		if err != nil {
			return 0, fmt.Errorf("观战索引 ZCARD 失败: %w", err)
		}
		if count <= 0 {
			return 0, nil
		}
		idx := int64(rand.Intn(count))
		pairs, err := svcCtx.Redis.ZrangeWithScores(spectateBattlesActiveKey, idx, idx)
		if err != nil {
			return 0, fmt.Errorf("观战索引 ZRANGE 失败: %w", err)
		}
		if len(pairs) == 0 {
			// 并发剔除导致下标越界,重试。
			continue
		}
		battleId, err := strconv.ParseUint(pairs[0].Key, 10, 64)
		if err != nil {
			logx.Errorf("[spectate] 观战索引出现非法成员 %q,剔除", pairs[0].Key)
			if _, err := svcCtx.Redis.Zrem(spectateBattlesActiveKey, pairs[0].Key); err != nil {
				logx.Errorf("[spectate] 剔除非法成员失败: %v", err)
			}
			continue
		}
		if uint64(pairs[0].Score) < staleBefore {
			removeSpectateBattle(svcCtx, battleId)
			continue
		}
		return battleId, nil
	}
	return 0, nil
}

// stopWatchingIfAny 观战互斥清退(设计决策 D11 / 不变量 8):玩家进 gather 前
// 把他从观战中摘除,杜绝观众绑定与随后的参战 BindBattleEvent 抢 SessionInfo
// 槽位的竞态。整条路径尽力而为:任何失败只记日志不阻断开局 —— RemoveObserver
// 丢了,观众绑定也会被参战绑定覆盖,battle 侧观众推送则因防僵尸校验自然失效。
func stopWatchingIfAny(svcCtx *svc.ServiceContext, playerId uint64, reason string) {
	raw, err := svcCtx.Redis.Get(spectateWatchingKey(playerId))
	if err != nil {
		logx.Errorf("[spectate] 读观战标记失败 player=%d: %v", playerId, err)
		return
	}
	if raw == "" {
		return
	}
	battleId, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		logx.Errorf("[spectate] 观战标记值非法 player=%d value=%q,直接清除", playerId, raw)
		deleteSpectateWatching(svcCtx, playerId)
		return
	}
	record, err := loadSpectateBattle(svcCtx, battleId)
	switch {
	case err != nil:
		logx.Errorf("[spectate] 清退时读观战记录失败 player=%d battle=%d: %v", playerId, battleId, err)
	case record == nil:
		// 战斗已收尾,标记是残留,删标记即可。
	default:
		removeObserver(svcCtx, record.GetBattleNodeId(), battleId, playerId, reason)
	}
	deleteSpectateWatching(svcCtx, playerId)
	logx.Infof("[spectate] 已清退观战 player=%d battle=%d reason=%s", playerId, battleId, reason)
}

// deleteSpectateWatching 删观战互斥标记(WatchBattle 失败回滚 / 清退共用)。
func deleteSpectateWatching(svcCtx *svc.ServiceContext, playerId uint64) {
	if _, err := svcCtx.Redis.Del(spectateWatchingKey(playerId)); err != nil {
		logx.Errorf("[spectate] 删观战标记失败 player=%d: %v", playerId, err)
	}
}

// addObserver 调 battle 节点挂观众。返回 roomMissing=true 表示 battle 回
// "房间不存在"(战斗已收尾),调用方据此懒剔除索引;其余错误(节点定位失败/
// RPC 失败/满员/观众是参战者)不动索引。
func addObserver(svcCtx *svc.ServiceContext, battleNodeId uint32, battleId, observerId uint64,
	observerName string, routing *battlepb.BattleRouting,
) (roomMissing bool, err error) {
	endpoint, err := svcCtx.BattleNodes.EndpointOfNode(battleNodeId)
	if err != nil {
		return false, fmt.Errorf("定位 battle 节点失败(node=%d): %w", battleNodeId, err)
	}
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return false, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), addObserverTimeout)
	defer cancel()
	resp, err := battlepb.NewBattleNodeClient(conn).AddObserver(ctx, &battlepb.AddObserverRequest{
		BattleId:         battleId,
		ObserverPlayerId: observerId,
		Routing:          routing,
		ObserverName:     observerName,
	})
	if err != nil {
		return false, fmt.Errorf("AddObserver RPC 失败: %w", err)
	}
	if tipId := resp.GetErrorMessage().GetId(); tipId != 0 {
		return tipId == uint32(table.CommonError_kEntityIsNull), fmt.Errorf("battle 节点拒绝观战 tip_id=%d", tipId)
	}
	return false, nil
}

// removeObserver 尽力调 battle 节点摘观众(清退路径;失败只记日志)。
func removeObserver(svcCtx *svc.ServiceContext, battleNodeId uint32, battleId, observerId uint64, reason string) {
	endpoint, err := svcCtx.BattleNodes.EndpointOfNode(battleNodeId)
	if err != nil {
		logx.Errorf("[spectate] 清退时定位 battle 节点失败 battle=%d node=%d player=%d: %v",
			battleId, battleNodeId, observerId, err)
		return
	}
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		logx.Errorf("[spectate] 清退时连接 battle 节点失败 battle=%d player=%d: %v", battleId, observerId, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), removeObserverTimeout)
	defer cancel()
	if _, err := battlepb.NewBattleNodeClient(conn).RemoveObserver(ctx, &battlepb.RemoveObserverRequest{
		BattleId:         battleId,
		ObserverPlayerId: observerId,
		Reason:           reason,
	}); err != nil {
		logx.Errorf("[spectate] RemoveObserver 失败 battle=%d player=%d reason=%s: %v",
			battleId, observerId, reason, err)
	}
}

// cleanupExpiredSpectateIndex 清理观战索引里 score 早于 TTL 窗口的残留成员
// (记录 string 键有 TTL 自清,ZSET 成员只能靠懒剔除 + 这里的定期兜底;
// matcher loop 每轮顺手执行,O(log N + M) 轻量,不另起 goroutine)。
func cleanupExpiredSpectateIndex(svcCtx *svc.ServiceContext) {
	staleBefore := int64(spectateStaleBeforeMs(svcCtx))
	if staleBefore <= 0 {
		return
	}
	removed, err := svcCtx.Redis.Zremrangebyscore(spectateBattlesActiveKey, 0, staleBefore)
	if err != nil {
		logx.Errorf("[matcher] 清理过期观战索引失败: %v", err)
		return
	}
	if removed > 0 {
		logx.Infof("[matcher] 清理过期观战索引 %d 条", removed)
	}
}

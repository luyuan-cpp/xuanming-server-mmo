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
// 观众满=kRateLimitExceeded、观众是参战者=kInvalidParameter、签不出票=
// kServiceUnavailable(turn-based §22 D70)不触发剔除 —— 这些情况战斗仍在打,索引是好的。

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

// 观战索引写入的时间预算(集群外入口 2b 第 9 条;记录由 gather 在 CreateBattle 之前写,见 writeSpectateRecord)。
//
// 上限必须按 go-redis 的真实行为算,不能按 ctx 截止算:go-zero 没开 go-redis 的 ContextTimeoutEnabled,
// ctx 截止只约束取连接、拨号,以及 go-redis 内部重试之间的退避(截止一过就不再内部重试);socket 读
// 仍按 go-redis 默认 ReadTimeout(3s,go-zero 未覆盖)等满。所以单条命令的上限是 spectateRedisOpWorst,
// 不是 spectateRedisOpTimeout。需要新建连接且握手(HELLO)也卡住的极端情况还会再多等一个 ReadTimeout,
// 不计入上限:超支的后果只是 matched 票据先于 gather 过期(玩家可重新排队),不会串局。
const (
	// spectateRedisOpTimeout 每条观战索引 Redis 命令(SetexCtx / ZaddCtx / DelCtx)的 ctx 截止。
	spectateRedisOpTimeout = time.Second
	// spectateRedisOpWorst 单条命令实际能保证的耗时上限(= go-redis 默认 ReadTimeout)。
	spectateRedisOpWorst = 3 * time.Second
	// spectateRecordWriteAttempts 记录 SET EX 的总尝试次数(重试 1 次)。写入在建房之前,写不进去只是
	// 一次无副作用的干净失败(解冻、回队首),不值得在 matched 票据的关键路径上多等。
	spectateRecordWriteAttempts = 2
	// spectateRecordWriteBackoff 首次退避;第 n 次失败后等 backoff × 2^(n-1),最后一次失败后不再等。
	spectateRecordWriteBackoff = 100 * time.Millisecond
	// spectateRecordWriteWorst 是 writeSpectateRecord 的耗时上限(每次尝试 spectateRedisOpWorst,加各次
	// 退避;当前 2×3s + 0.1s = 6.1s),供 matched 票据 TTL 预算引用(queue.go matchedTicketTTLFor)。
	// gather 常规路径写一次;k8s-client-entry D82 换节点重试路径再改写一次,并多一次 CreateBattle。
	spectateRecordWriteWorst = spectateRecordWriteAttempts*spectateRedisOpWorst +
		spectateRecordWriteBackoff*(1<<(spectateRecordWriteAttempts-1)-1)
)

// spectateRecordRetrySleepFn 退避等待的测试缝(生产恒为 time.Sleep,照 team 包 endMatchSleepFn
// 的写法):单测换成记录器,不真睡,并能断言退避序列。
var spectateRecordRetrySleepFn = time.Sleep

// newSpectateRecord 组一场战斗的观战记录(设计决策 D9:只有 match 知道战斗在哪个 battle 节点)。
// createdAtMs 与 CreateBattleRequest.created_at_ms 同一时刻取值,过期判定对齐 deadline。
func newSpectateRecord(battleId uint64, battleNodeId uint32, mode matchpb.MatchMode, battleConfigId uint32,
	playerNames []string, createdAtMs uint64,
) *matchpb.SpectateBattleRecord {
	return &matchpb.SpectateBattleRecord{
		Summary: &matchpb.BattleWatchSummary{
			BattleId:       battleId,
			Mode:           mode,
			BattleConfigId: battleConfigId,
			PlayerNames:    playerNames,
			CreatedAtMs:    createdAtMs,
		},
		BattleNodeId: battleNodeId,
	}
}

// writeSpectateRecord 写观战记录 spectate:battle:{id}(fail-closed)。gather 在 CreateBattle **之前**调
// (D82 换节点重试前再改写一次);返回非 nil 时调用方不建房、按开局失败补偿(见 gather.go 第 4.1 步)。
//
// 为什么 fail-closed:这条记录不只服务观战。丢票补签 RequestBattleTicket(turn-based §18 D25)
// 同样按它定位 battle 节点,而 gate 已不中继战斗(turn-based §22 D66)。持票断线可凭原票重连
// (D25 同票可重用,期限 = 房间作废期限),不依赖本索引;但丢票(客户端重启 / 重登,或大厅
// 断线连带拆掉直连)、同票重试失败或握手被拒之后,补签是回到本局的唯一通路。记录缺失时
// 补签只会拿到「战斗不存在或已结束」(kInvalidParameter),客户端据此把进行中的战斗判为
// 已结束(BattleGone)—— 与其留一场"打着但断线就回不来"的战斗,不如不开。
//
// 有界重试吸收 Redis 瞬时抖动:SET EX 同值覆盖天然幂等,超时后"其实已落盘"再写一遍也无害;
// 耗时上限见 spectateRecordWriteWorst。失败时这里不清理(超时的那次可能已落盘):调用方在补偿
// 之后 dropSpectateRecord,不占 matched 票据的关键路径。
// 独立的 battle_id → 落点路由键另起任务(turn-based §21.5 第 2 条)。
func writeSpectateRecord(svcCtx *svc.ServiceContext, record *matchpb.SpectateBattleRecord) error {
	battleId := record.GetSummary().GetBattleId()
	raw, err := proto.Marshal(record)
	if err != nil {
		return fmt.Errorf("序列化观战记录失败: %w", err)
	}
	key := spectateBattleKey(battleId)
	ttl := spectateTTLSeconds(svcCtx)
	backoff := spectateRecordWriteBackoff
	var lastErr error
	for attempt := 1; attempt <= spectateRecordWriteAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
		lastErr = svcCtx.MatchRedis.SetexCtx(ctx, key, string(raw), ttl)
		cancel()
		if lastErr == nil {
			return nil
		}
		logx.Errorf("[spectate] 写观战记录失败 battle=%d node=%d attempt=%d/%d: %v",
			battleId, record.GetBattleNodeId(), attempt, spectateRecordWriteAttempts, lastErr)
		if attempt < spectateRecordWriteAttempts {
			spectateRecordRetrySleepFn(backoff)
			backoff *= 2
		}
	}
	return fmt.Errorf("写观战记录失败(已尝试 %d 次): %w", spectateRecordWriteAttempts, lastErr)
}

// publishSpectateBattle 开局成功后公开这场战斗。gather 在推进 ready 之后调,不占 matched 票据的
// 关键路径;两步都 best-effort(每条命令带 spectateRedisOpTimeout 截止),失败只记日志:
//  1. 同值补写一次记录(写成最终记录)。活动开局的 battle_id 在 gather 之前就已回给 guild、在帮会
//     历练大厅可见,建房窗口里有人按 battle_id 观战,battle 会回房间不存在。WatchBattle 现在只在读记录
//     前该场已公开、或已出建房窗口时才删记录(watchbattlelogic.go roomMayBeCreating),补写保留为纵深
//     防御:兜住窗口判定之外的情形(实例间时钟偏差让窗口偏窄等),也纠正"超时的旧写入晚于 D82 改写
//     才落盘"的乱序。补写失败时预写的记录通常仍在,补签不受影响。
//  2. 入活跃集合 spectate:battles:active(观战列表与随机选场)。失败只是该场不进观战列表。
//
// 顺序不变量:先 SETEX 最终记录、后 ZADD,不能调换,也不能把第 2 步挪到第 1 步之前单独提前做。
// 两处依赖它:
//   - 观战列表 / 随机选场挑中的成员保证记录可读(与 JoinQueue 先写 ticket 再入队同理);
//   - WatchBattle 的建房窗口判定(roomMayBeCreating 条件 1)以"读记录前已在活跃集合 ⇒ 读到的是实际
//     建房节点的最终记录"为前提。反过来先 ZADD,WatchBattle 可能先查到已公开、再读到尚未被补写纠正的
//     旧记录(如超时后晚到、指向 D82 首选节点的写入),AddObserver 打到错误节点回房间不存在,又因
//     "已公开"跳过窗口保护直接懒剔除,删掉一场进行中战斗的记录(丢票补签靠它定位节点)。
func publishSpectateBattle(svcCtx *svc.ServiceContext, record *matchpb.SpectateBattleRecord) {
	battleId := record.GetSummary().GetBattleId()
	if raw, err := proto.Marshal(record); err != nil {
		logx.Errorf("[spectate] 补写观战记录时序列化失败(预写记录仍在) battle=%d: %v", battleId, err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
		err = svcCtx.MatchRedis.SetexCtx(ctx, spectateBattleKey(battleId), string(raw), spectateTTLSeconds(svcCtx))
		cancel()
		if err != nil {
			logx.Errorf("[spectate] 补写观战记录失败(预写记录通常仍在) battle=%d: %v", battleId, err)
		}
	}
	addSpectateActive(svcCtx, record)
}

// addSpectateActive 把一场已登记记录的战斗加入活跃集合(best-effort,带截止)。
func addSpectateActive(svcCtx *svc.ServiceContext, record *matchpb.SpectateBattleRecord) {
	summary := record.GetSummary()
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if _, err := svcCtx.MatchRedis.ZaddCtx(ctx, spectateBattlesActiveKey, int64(summary.GetCreatedAtMs()),
		strconv.FormatUint(summary.GetBattleId(), 10)); err != nil {
		logx.Errorf("[spectate] 观战索引 ZADD 失败(不影响补签,该场不进观战列表) battle=%d: %v",
			summary.GetBattleId(), err)
		return
	}
	logx.Infof("[spectate] 战斗已登记可观战 battle=%d node=%d mode=%s players=%d",
		summary.GetBattleId(), record.GetBattleNodeId(), summary.GetMode().String(), len(summary.GetPlayerNames()))
}

// dropSpectateRecord 尽力删掉建房前预写的观战记录(gather 确认房间未建成 / 已销毁、补偿完成之后调;
// 带截止)。删不掉也无害:记录指向的房间不存在,补签 / 按 battle_id 观战拿到"房间不存在",语义正确,
// 随 TTL 自清;它从没进过活跃集合,不会出现在观战列表里。
func dropSpectateRecord(svcCtx *svc.ServiceContext, battleId uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if _, err := svcCtx.MatchRedis.DelCtx(ctx, spectateBattleKey(battleId)); err != nil {
		logx.Errorf("[spectate] 删预写观战记录失败(随 TTL 自清) battle=%d: %v", battleId, err)
	}
}

// loadSpectateBattle 读活跃战斗记录;不存在(TTL 已清/从未登记)返回 (nil, nil)。
func loadSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) (*matchpb.SpectateBattleRecord, error) {
	raw, err := svcCtx.MatchRedis.Get(spectateBattleKey(battleId))
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
	if _, err := svcCtx.MatchRedis.Del(spectateBattleKey(battleId)); err != nil {
		logx.Errorf("[spectate] 删观战记录失败 battle=%d: %v", battleId, err)
	}
	if _, err := svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
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
		count, err := svcCtx.MatchRedis.Zcard(spectateBattlesActiveKey)
		if err != nil {
			return 0, fmt.Errorf("观战索引 ZCARD 失败: %w", err)
		}
		if count <= 0 {
			return 0, nil
		}
		idx := int64(rand.Intn(count))
		pairs, err := svcCtx.MatchRedis.ZrangeWithScores(spectateBattlesActiveKey, idx, idx)
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
			if _, err := svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, pairs[0].Key); err != nil {
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
// 把他从观战中摘除 —— 一名玩家同一时刻只保留一条 battle 直连(客户端单条链路,
// battle 房间 directConnByPlayer 按 player_id 单槽),观战直连与随后的参战直连
// 不能并存。整条路径尽力而为:任何失败只记日志不阻断开局 —— RemoveObserver 丢了,
// 客户端收到参战的 NotifyBattleAssigned 会关掉观战直连、改连参战落点,旧场对该观众
// 的战斗帧因无活直连被丢弃(turn-based §22 D68),名单残留随该场结束清理。
func stopWatchingIfAny(svcCtx *svc.ServiceContext, playerId uint64, reason string) {
	raw, err := svcCtx.MatchRedis.Get(spectateWatchingKey(playerId))
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
	if _, err := svcCtx.MatchRedis.Del(spectateWatchingKey(playerId)); err != nil {
		logx.Errorf("[spectate] 删观战标记失败 player=%d: %v", playerId, err)
	}
}

// battle 节点观战 RPC 的可替换缝(单测用记录器顶替真 gRPC;与 gather.go 的
// prepareBattleFn / createBattleFn 等四个缝同一模式)。生产实现只做"建连 + 发一次
// 请求",节点定位与索引懒剔除判定留在 addObserver / removeObserver 里,
// 这样单测能覆盖到判定逻辑而不需要起 battle 节点。
var (
	addObserverFn    = addObserverRPC
	removeObserverFn = removeObserverRPC
)

// addObserverRPC 是 addObserverFn 的生产实现。
func addObserverRPC(endpoint string, req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), addObserverTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).AddObserver(ctx, req)
}

// removeObserverRPC 是 removeObserverFn 的生产实现。
func removeObserverRPC(endpoint string, req *battlepb.RemoveObserverRequest) error {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), removeObserverTimeout)
	defer cancel()
	_, err = battlepb.NewBattleNodeClient(conn).RemoveObserver(ctx, req)
	return err
}

// addObserver 调 battle 节点挂观众。返回 roomMissing=true 表示 battle 回
// "房间不存在"(战斗已收尾),调用方据此懒剔除索引;其余错误(节点定位失败/
// RPC 失败/满员/观众是参战者/签不出票)不动索引。
func addObserver(svcCtx *svc.ServiceContext, battleNodeId uint32, battleId, observerId uint64,
	observerName string, routing *battlepb.BattleRouting,
) (roomMissing bool, err error) {
	endpoint, err := svcCtx.BattleNodes.EndpointOfNode(battleNodeId)
	if err != nil {
		return false, fmt.Errorf("定位 battle 节点失败(node=%d): %w", battleNodeId, err)
	}
	resp, err := addObserverFn(endpoint, &battlepb.AddObserverRequest{
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
	if err := removeObserverFn(endpoint, &battlepb.RemoveObserverRequest{
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
	removed, err := svcCtx.MatchRedis.Zremrangebyscore(spectateBattlesActiveKey, 0, staleBefore)
	if err != nil {
		logx.Errorf("[matcher] 清理过期观战索引失败: %v", err)
		return
	}
	if removed > 0 {
		logx.Infof("[matcher] 清理过期观战索引 %d 条", removed)
	}
}

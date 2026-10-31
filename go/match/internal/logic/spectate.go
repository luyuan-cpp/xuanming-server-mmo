package logic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 观战索引与互斥标记(设计文档 §10.4;spectate:* 只有 match 读写,不变量 7):
//
//	spectate:battle:{battle_id}   string  SpectateBattleRecord pb,TTL = 战斗最长期限 + 60s
//	spectate:battles:active       zset    member=battle_id 十进制,score=created_at_ms;只收公开局
//	spectate:watching:{player_id} string  本次观战固定的 AddObserverRequest 原件 pb(battle_id / op / 准入截止 /
//	                                      实例),TTL 同上。battle 上可能挂着的观众唯一的恢复入口,去留规则见 stopWatching
//
// 观战档位与 L1 / L2 准入用的键见 spectate_tier.go。
//
// 私密局(记录的 visibility 非 PUBLIC,见 spectate_visibility.go)只有记录、不进活跃集合:记录照写是因为丢票补签
// 靠它定位 battle 节点。ZSET 成员没有 TTL,靠这几条路清理:全局模式战斗终局提交时当场摘除(releaseSpectateOnOutcome)、
// battle 报房间不存在时懒剔除、读到过期 score 时现场剔除、定期 ZREMRANGEBYSCORE 兜底(cleanupExpiredSpectateIndex)。
const spectateBattlesActiveKey = "spectate:battles:active"

// battle 节点观战 RPC 超时(与 gather 的 prepare/rollback 同量级)。
const (
	addObserverTimeout    = 3 * time.Second
	removeObserverTimeout = 3 * time.Second
)

// spectateAdmitWindow 本地观战原件的准入窗口:admit_before_ms = match 墙钟 now + 本值。battle 观战账本只接受
// admit_before_ms ∈ (now, now+30s](battle 墙钟,battle_room_manager.cpp HandleAddObserver)。全局路径按 TiDB now()
// + 30s 取值(authority BeginSpectator);本地没有统一时钟源,match 与 battle 各用本机墙钟,留 5s 给两边的时钟偏差:
// match 快于 battle 不超过 5s 时,Add 不会因截止过远被拒。Add 本身受 addObserverTimeout 约束,25s 足够送达。
const spectateAdmitWindow = 25 * time.Second

// battle 侧 AddObserver 的房间不存在信号 = table.CommonError_kEntityIsNull
// (与 battle_room_manager.cpp AddObserver 契约对齐,match 据此懒剔除索引);
// 观众席位满=MatchError_kMatchSpectateFull、墓碑表满=kRateLimitExceeded、观众是参战者=kInvalidParameter、
// 签不出票=kServiceUnavailable(turn-based §22 D70)不触发剔除 —— 这些情况战斗仍在打,索引是好的。
// 拒绝码到客户端结论的映射见 spectateRejectVerdict(Redis 与全局两条路径共用)。

func spectateBattleKey(battleId uint64) string {
	return fmt.Sprintf("spectate:battle:%d", battleId)
}

func spectateWatchingKey(playerId uint64) string {
	return fmt.Sprintf("spectate:watching:%d", playerId)
}

// globalSpectateTTLSeconds 是全局模式的观战键 TTL。全局成局的期限 = 提交时刻 + 备战预算(authority.WorkLeaseMs)+ 模板战斗时长,
// 模板时长上限是 catalog.MaxBattleSeconds,所以按上限算,再留 60s 吃掉 deadline 强制收尾与终局提交的时间差(与终局宽限同量)。
// 不能用 BattleMaxDurationSeconds:那是本地 gather 的固定期限,比全局模板时长短得多,键会在战斗中途过期,
// 后果是档位回落、L2 配额被退,活着的场被当成过期剔除。
const globalSpectateTTLSeconds = catalog.MaxBattleSeconds + int(authority.WorkLeaseMs/1000) + 60

// spectateTTLSeconds 观战键 TTL 与活跃集合的过期分界:战斗最长期限 + 60s 余量。余量吃掉
// deadline 强制收尾与索引清理之间的时间差,保证 TTL 到点时战斗必已收尾。
func spectateTTLSeconds(svcCtx *svc.ServiceContext) int {
	if svcCtx.Authority != nil {
		return globalSpectateTTLSeconds
	}
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
	// spectateRedisOpTimeout 每条观战索引 Redis 命令(SetexCtx / ZaddCtx / DelCtx / EvalCtx)的 ctx 截止。
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
// visibility 必须取建房原件冻结的 CreateBattleRequest.spectate_visibility(gather 在规范化原件之后取值),
// 不得按模式另算一份:记录与 battle 房间的可见性同源,执法点之间才不会各说各话。
func newSpectateRecord(battleId uint64, battleNodeId uint32, mode matchpb.MatchMode, battleConfigId uint32,
	playerNames []string, createdAtMs uint64, visibility battlepb.ESpectateVisibility,
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
		Visibility:   visibility,
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
		logx.Errorf("[spectate] Failed to write spectator record battle=%d node=%d attempt=%d/%d: %v",
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
//     才落盘"的乱序。补写失败时预写的记录通常仍在,补签不受影响。活动局现为私密局,按 battle_id 观战
//     在 WatchBattle 选场处即被拒、不再打到 battle;补写照做,私密局的补签同样靠这条最终记录。
//  2. 入活跃集合 spectate:battles:active(观战列表与随机选场)。只对公开局做:非公开记录由 addSpectateActive
//     拒收,私密局只做第 1 步。失败只是该场不进观战列表。
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
		logx.Errorf("[spectate] Failed to serialize spectator record update (prewritten record remains) battle=%d: %v", battleId, err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
		err = svcCtx.MatchRedis.SetexCtx(ctx, spectateBattleKey(battleId), string(raw), spectateTTLSeconds(svcCtx))
		cancel()
		if err != nil {
			logx.Errorf("[spectate] Failed to update spectator record (prewritten record usually remains) battle=%d: %v", battleId, err)
		}
	}
	addSpectateActive(svcCtx, record)
}

// addSpectateActive 把一场已登记记录的战斗加入活跃集合(best-effort,带截止)。活跃集合只收公开局:这里是
// spectate:battles:active 唯一的 ZADD,在这里拒收非公开记录,任何调用方都没法把私密局放进观战列表与随机选场。
func addSpectateActive(svcCtx *svc.ServiceContext, record *matchpb.SpectateBattleRecord) {
	summary := record.GetSummary()
	if !isSpectatePublic(record.GetVisibility()) {
		logx.Infof("[spectate] Battle is not public; record kept for ticket reissue only battle=%d node=%d mode=%s visibility=%s",
			summary.GetBattleId(), record.GetBattleNodeId(), summary.GetMode().String(), record.GetVisibility().String())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if _, err := svcCtx.MatchRedis.ZaddCtx(ctx, spectateBattlesActiveKey, int64(summary.GetCreatedAtMs()),
		strconv.FormatUint(summary.GetBattleId(), 10)); err != nil {
		logx.Errorf("[spectate] Spectator index ZADD failed (replacement tickets unaffected; battle excluded from spectator list) battle=%d: %v",
			summary.GetBattleId(), err)
		return
	}
	logx.Infof("[spectate] Battle registered for spectating battle=%d node=%d mode=%s players=%d",
		summary.GetBattleId(), record.GetBattleNodeId(), summary.GetMode().String(), len(summary.GetPlayerNames()))
}

// dropSpectateRecord 尽力删掉建房前预写的观战记录(gather 确认房间未建成 / 已销毁、补偿完成之后调;
// 带截止)。删不掉也无害:记录指向的房间不存在,补签 / 按 battle_id 观战拿到"房间不存在",语义正确,
// 随 TTL 自清;它从没进过活跃集合,不会出现在观战列表里。
func dropSpectateRecord(svcCtx *svc.ServiceContext, battleId uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if _, err := svcCtx.MatchRedis.DelCtx(ctx, spectateBattleKey(battleId)); err != nil {
		logx.Errorf("[spectate] Failed to delete prewritten spectator record (TTL will clear it) battle=%d: %v", battleId, err)
	}
}

// loadSpectateBattle 读活跃战斗记录;不存在(TTL 已清/从未登记)返回 (nil, nil)。
func loadSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) (*matchpb.SpectateBattleRecord, error) {
	if svcCtx.Authority != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		f, e := svcCtx.Authority.Formation(ctx, battleId)
		if errors.Is(e, sql.ErrNoRows) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		if f.State == 5 {
			return nil, nil
		}
		return spectateRecordFromFormation(f)
	}
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

// spectateRecordFromFormation 由全局成局组观战记录。可见性取持久原件里冻结的值(与 battle 房间同源),不按 f.Mode 重算;
// 实例与期限也取成局原件:L2 观众不调 battle RPC,观战凭证的这两个字段全靠这里(§15.6)。
func spectateRecordFromFormation(f *matchpb.MatchFormationRecord) (*matchpb.SpectateBattleRecord, error) {
	if len(f.CreateRequest) == 0 {
		return nil, errors.New("成局备战尚未完成")
	}
	create := &battlepb.CreateBattleRequest{}
	if err := proto.Unmarshal(f.CreateRequest, create); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(create.Players))
	for _, p := range create.Players {
		names = append(names, p.PlayerName)
	}
	record := newSpectateRecord(f.BattleId, f.TargetNodeId, matchpb.MatchMode(f.Mode), f.ConfigId, names, f.CreatedMs,
		create.GetSpectateVisibility())
	record.BattleInstanceId, record.DeadlineMs = f.TargetInstanceId, f.DeadlineMs
	return record, nil
}

// removeSpectateBattle 清掉一场已结束战斗的全部观战数据,幂等:battle 报房间不存在、记录缺失或损坏时懒剔除,
// 全局模式终局提交时也走这里。记录与活跃集合成员一起删;L2 配额与赛事局索引成员一并还掉:它们是全服共用的集合,
// 成员不随本场的键过期,等期限到才清会白占名额。档位、中继 gate 集合与赛事局标记同样删掉:它们带 TTL 兜底,
// 但留着会让这场结束之后到达的观众按旧档位签出收不到帧的凭证。
func removeSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) {
	if _, err := svcCtx.MatchRedis.Del(spectateBattleKey(battleId)); err != nil {
		logx.Errorf("[spectate] Failed to delete spectator record battle=%d: %v", battleId, err)
	}
	removeSpectateActiveMember(svcCtx, battleId)
	member := strconv.FormatUint(battleId, 10)
	if _, err := svcCtx.MatchRedis.Zrem(spectateBroadcastActiveKey, member); err != nil {
		logx.Errorf("[spectate] Failed to release broadcast quota battle=%d: %v", battleId, err)
	}
	if _, err := svcCtx.MatchRedis.Zrem(spectateFeaturedActiveKey, member); err != nil {
		logx.Errorf("[spectate] Failed to remove featured index member battle=%d: %v", battleId, err)
	}
	// 这三个键共用 hash tag {id},落同一 slot,可以一条 DEL。
	if _, err := svcCtx.MatchRedis.Del(spectateTierKey(battleId), spectateRelayGatesKey(battleId), spectateFeaturedKey(battleId)); err != nil {
		logx.Errorf("[spectate] Failed to delete battle tier keys (TTL will clear them) battle=%d: %v", battleId, err)
	}
}

// releaseSpectateOnOutcome 在战斗终局提交之后尽力清掉这场的观战数据(全局模式,本地模式没有终局提交通道):结束的场立刻离开
// 观战列表与随机选场,L2 配额立刻还给别的场,否则要白占到战斗期限(最长约 1 小时),全服配额很快用尽。
// 放进独立任务:Redis 抖动不能拖慢终局回执,battle 收不到回执就会重投。任务发不出(本实例正在排空)只留日志,
// 由键的 TTL 与定期清理兜底。
func releaseSpectateOnOutcome(svcCtx *svc.ServiceContext, battleId uint64) {
	if !svcCtx.LaunchRelayTask("match.global.spectate.release", func() { removeSpectateBattle(svcCtx, battleId) }) {
		logx.Infof("[spectate] Spectate cleanup skipped on outcome (TTL and periodic cleanup will clear it) battle=%d", battleId)
	}
}

// removeSpectateActiveMember 只从活跃集合摘掉一场、不碰记录键:记录已不在时的残留成员,以及不该出现在集合里的
// 私密成员。私密局的记录是丢票补签定位 battle 节点的唯一来源,摘私密成员不能用会 DEL 记录的 removeSpectateBattle。
func removeSpectateActiveMember(svcCtx *svc.ServiceContext, battleId uint64) {
	if _, err := svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] Spectator index ZREM failed battle=%d: %v", battleId, err)
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
			logx.Errorf("[spectate] Invalid member %q in spectator index; removing", pairs[0].Key)
			if _, err := svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, pairs[0].Key); err != nil {
				logx.Errorf("[spectate] Failed to remove invalid member: %v", err)
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
// 不能并存。这条路径不阻断开局:读不到标记或取消未获证明都只记日志,标记不动。此时观众可能
// 还挂在旧场,但客户端收到参战的 NotifyBattleAssigned 会关掉观战直连、改连参战落点,旧场对该观众
// 的战斗帧因无活直连被丢弃(turn-based §22 D68),席位由 battle 的断线宽限回收;保留的标记留给
// 下一次 WatchBattle 沿原 op 重试取消。
func stopWatchingIfAny(svcCtx *svc.ServiceContext, playerId uint64, reason string) {
	raw, err := svcCtx.MatchRedis.Get(spectateWatchingKey(playerId))
	if err != nil {
		logx.Errorf("[spectate] Failed to read spectator marker player=%d: %v", playerId, err)
		return
	}
	if raw == "" {
		return
	}
	if err := stopWatching(svcCtx, playerId, raw, reason); err != nil {
		logx.Errorf("[spectate] Spectator removal not confirmed; marker kept player=%d reason=%s: %v", playerId, reason, err)
	}
}

// stopWatching 沿观战标记固定的原件取消一次观战,拿到取消证明后按值删标记。raw 是标记原值。
// 换场清退、gather 入口清退、复查自我清退、Add 结果未知的补偿都走这里。
//
// 标记是 battle 上可能挂着的观众唯一的恢复入口,所以只有两种情况才删(与全局 cancelGlobalSpectator、Java 版同一规则):
// 这里拿到取消证明;或 Add 确定不会登记(admitDirectSeat 的回滚)。返回错误 = 没有证明或删除失败,调用方不能当作已清退,
// 下一次 WatchBattle 沿同一个 op 重试。battle 实例再也回不来时由 TTL 兜底,TTL 到期时那场必已收尾(spectateTTLSeconds)。
//
// 标记解析不出原件(值损坏或不属于该玩家)时没有可重试的取消,保留只会把玩家卡到 TTL,按值清掉。
func stopWatching(svcCtx *svc.ServiceContext, playerId uint64, raw, reason string) error {
	original, err := decodeWatchingMark(playerId, raw)
	if err != nil {
		logx.Errorf("[spectate] Invalid spectator marker player=%d len=%d; clearing it: %v", playerId, len(raw), err)
		return releaseWatchingMark(svcCtx, playerId, raw)
	}
	if err := cancelObserver(svcCtx, original, reason); err != nil {
		return fmt.Errorf("取消观战未获证明 battle=%d: %w", original.GetBattleId(), err)
	}
	if err := releaseWatchingMark(svcCtx, playerId, raw); err != nil {
		return err
	}
	logx.Infof("[spectate] Spectator removed player=%d battle=%d reason=%s", playerId, original.GetBattleId(), reason)
	return nil
}

// cancelObserver 沿原件向原 battle 实例发 RemoveObserver。返回 nil 才是取消证明:battle 已摘掉该 op 的观众,并在原截止前
// 给它立了封闭墓碑,仍在途中的同 op Add 也不会再准入;对没挂上的观众、已收尾的房间同样给证明。
// 按实例定位、不读观战记录:记录会被 D82 改写、也可能已过期,取消只看原件(与全局 cancelGlobalSpectator 同口径)。
func cancelObserver(svcCtx *svc.ServiceContext, original *battlepb.AddObserverRequest, reason string) error {
	entry, err := svcCtx.BattleNodes.EntryOfInstance(original.GetTargetInstanceId())
	if err != nil {
		return err
	}
	return removeObserverFn(entry.Endpoint, &battlepb.RemoveObserverRequest{
		BattleId:             original.GetBattleId(),
		ObserverPlayerId:     original.GetObserverPlayerId(),
		Reason:               reason,
		SpectatorOperationId: original.GetSpectatorOperationId(),
		AdmitBeforeMs:        original.GetAdmitBeforeMs(),
		TargetInstanceId:     original.GetTargetInstanceId(),
	})
}

// validSpectatorOperation 与 battle 侧 battle_security::ValidSpectatorOperation 同口径:
// 36 位小写规范 UUID、非全 0(全局入口校验 request_id 用的也是这条规则)。
func validSpectatorOperation(op string) bool {
	id, err := uuid.Parse(op)
	return err == nil && id != uuid.Nil && id.String() == op
}

// newLocalObserverAdd 按 battle 观战账本契约组本地观战的 AddObserver 原件(与全局 BeginSpectator 同口径):
//   - spectator_operation_id:新生成的规范 UUID。本地没有 request_id 意图账本,一次 WatchBattle 每选中一场
//     只生成一次;同一观战的 Add 与之后的补偿 / 清退 Remove 都用这一个 op;
//   - admit_before_ms:now + spectateAdmitWindow(battle 只接受至多未来 30s);
//   - target_instance_id:spectateTarget 定位到的 battle 实例。
//
// 原件先随观战标记落 Redis,再发 Add;battle 对同 op 只接受逐字节相同的重放,重试必须原样复用。
func newLocalObserverAdd(battleId, observerId uint64, observerName string, routing *battlepb.BattleRouting,
	instance string,
) *battlepb.AddObserverRequest {
	return &battlepb.AddObserverRequest{
		BattleId:             battleId,
		ObserverPlayerId:     observerId,
		Routing:              routing,
		ObserverName:         observerName,
		SpectatorOperationId: uuid.NewString(),
		AdmitBeforeMs:        nowMs() + uint64(spectateAdmitWindow.Milliseconds()),
		TargetInstanceId:     instance,
	}
}

// encodeWatchingMark 把原件序列化成观战标记值(确定性序列化,与全局原件 add_request 同口径)。
func encodeWatchingMark(original *battlepb.AddObserverRequest) (string, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	if err != nil {
		return "", fmt.Errorf("序列化观战原件失败: %w", err)
	}
	return string(raw), nil
}

// decodeWatchingMark 从观战标记还原原件。值损坏、不属于该玩家、缺账本字段一律报错:
// 那种标记拿不出原 op,battle 不会据它摘人。
func decodeWatchingMark(playerId uint64, raw string) (*battlepb.AddObserverRequest, error) {
	original := &battlepb.AddObserverRequest{}
	if err := proto.Unmarshal([]byte(raw), original); err != nil {
		return nil, fmt.Errorf("观战标记反序列化失败: %w", err)
	}
	if original.GetBattleId() == 0 || original.GetObserverPlayerId() != playerId ||
		!validSpectatorOperation(original.GetSpectatorOperationId()) ||
		original.GetAdmitBeforeMs() == 0 || original.GetTargetInstanceId() == "" {
		return nil, errors.New("观战标记缺少原操作字段")
	}
	return original, nil
}

// spectateWatchingDelCasScript 按值删观战标记(KEYS=[watchingKey],ARGV=[expectedMark])。标记值带唯一 op,
// 值相同才是这一次观战的槽;无条件 DEL 会误删并发 WatchBattle 刚抢到的新标记,新观众的恢复入口就丢了。
const spectateWatchingDelCasScript = `if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return 0
end
return redis.call("DEL", KEYS[1])`

// releaseWatchingMark 按值删观战标记,走独立 ctx(回滚与补偿不继承请求 ctx)。值已不同不算错误:那已不是这次的标记。
func releaseWatchingMark(svcCtx *svc.ServiceContext, playerId uint64, mark string) error {
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if _, err := svcCtx.MatchRedis.EvalCtx(ctx, spectateWatchingDelCasScript, []string{spectateWatchingKey(playerId)}, mark); err != nil {
		return fmt.Errorf("删观战标记失败: %w", err)
	}
	return nil
}

// battle 节点观战 RPC 的可替换缝(单测用记录器顶替真 gRPC;与 gather.go 的
// prepareBattleFn / createBattleFn 等四个缝同一模式)。生产实现只做"建连 + 发一次
// 请求",节点与实例定位(spectateTarget)、原件组装与索引懒剔除判定留在缝外,
// 这样单测能覆盖到判定逻辑而不需要起 battle 节点。
var (
	addObserverFn    = addObserverRPC
	removeObserverFn = removeObserverRPC
)

// addObserverRPC 是 addObserverFn 的生产实现。截止时间取「调用方 ctx 的剩余时间」与 addObserverTimeout
// 二者中较早的那个:WatchBattle 传请求 ctx,请求截止之后不再挂观众。客户端那时已经收到超时,gate 也已放行
// 同一会话的下一条请求,截止之后才绑上的观战会让顺序乱掉(docs/design/client-request-ordering.md §6.1)。
func addObserverRPC(ctx context.Context, endpoint string, req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, addObserverTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).AddObserver(callCtx, req)
}

// removeObserverRPC 是 removeObserverFn 的生产实现。没有取消证明按失败返回,标记由 stopWatching 保留。
func removeObserverRPC(endpoint string, req *battlepb.RemoveObserverRequest) error {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), removeObserverTimeout)
	defer cancel()
	reply, err := battlepb.NewBattleNodeClient(conn).RemoveObserver(ctx, req)
	if err != nil {
		return err
	}
	if !observerCancelProven(req, reply) {
		return errors.New("battle 未确认取消原观战操作")
	}
	return nil
}

// observerCancelProven 判定 RemoveObserver 回包是不是取消证明。battle 对不合账本契约的 Remove(实例不符、op 非法、
// 截止与原件不同)回空包、不摘人,所以只有带回原 op 与实例的 cancelled 才算(与全局 cancelGlobalSpectator 同一判据)。
func observerCancelProven(req *battlepb.RemoveObserverRequest, reply *battlepb.RemoveObserverResponse) bool {
	return reply.GetCancelled() && reply.GetSpectatorOperationId() == req.GetSpectatorOperationId() &&
		reply.GetTargetInstanceId() == req.GetTargetInstanceId()
}

// errAddObserverOutcomeUnknown 标记「没拿到 battle 的业务回包」的失败。请求可能已经送达,C++ battle 同步处理、不一定看
// grpc-timeout,观众可能已挂上并经 Kafka→gate 推了观战落点;任何 gRPC 错误都可能发生在 battle 处理之后,所以不按状态码细分
// (连接被拒也一样:Unavailable 分不清请求有没有送出)。
// 调用方不能把标记当作未登记直接删,要沿原件取消拿证明(stopWatching),否则 match 丢了标记、battle 上却留着观众:
// 客户端先收到失败又收到落点,之后排队时 gather 也清退不到它(D11)。对没挂上的观众取消是 no-op。
var errAddObserverOutcomeUnknown = errors.New("AddObserver outcome unknown")

// spectateRejectVerdict 把 battle AddObserver 的拒绝码映射为客户端结论(Redis 与全局两条路径共用):
//   - kEntityIsNull(房间不存在,战斗已收尾)→ ErrBattleNotWatchable / not_found,roomMissing=true:
//     Redis 路径据此懒剔除索引(建房窗口内除外),随机模式换一场;
//   - kMatchSpectateFull(直连观众席位已满)→ ErrSpectateFull / full:战斗仍在打,索引不动,随机模式也不换场;
//   - 其它(墓碑表满 kRateLimitExceeded、观众是参战者 kInvalidParameter、签不出票 kServiceUnavailable)
//     → ErrBattleNotWatchable / rejected,索引不动、不换场。
func spectateRejectVerdict(battleTip uint32) (clientTip uint32, outcome string, roomMissing bool) {
	switch battleTip {
	case uint32(table.CommonError_kEntityIsNull):
		return constants.ErrBattleNotWatchable, "not_found", true
	case uint32(table.MatchError_kMatchSpectateFull):
		return constants.ErrSpectateFull, "full", false
	default:
		return constants.ErrBattleNotWatchable, "rejected", false
	}
}

// addObserver 按原件(newLocalObserverAdd,已随观战标记落盘)调 battle 节点挂观众。battle 明确拒绝时返回其
// tip(rejectTip≠0,err 同时非 nil),调用方用 spectateRejectVerdict 得出客户端结论与是否懒剔除。RPC 失败一律算结果未知
// (err 带 errAddObserverOutcomeUnknown,rejectTip=0,不动索引);请求截止前没发出时 err 不带这个标记。
// endpoint 是 spectateTarget 按原件实例定位的地址;ctx 是请求 ctx,见 addObserverRPC。
func addObserver(ctx context.Context, endpoint string, original *battlepb.AddObserverRequest) (rejectTip uint32, err error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("请求已截止,不再挂观众: %w", err)
	}
	resp, err := addObserverFn(ctx, endpoint, original)
	if err != nil {
		return 0, fmt.Errorf("AddObserver RPC 结果未知: %w: %w", errAddObserverOutcomeUnknown, err)
	}
	if tipId := resp.GetErrorMessage().GetId(); tipId != 0 {
		return tipId, fmt.Errorf("battle 节点拒绝观战 tip_id=%d", tipId)
	}
	return 0, nil
}

// spectateTarget 定位观战 RPC 的对端:gRPC 地址与 battle 实例(观战账本的 target_instance_id)。
// 本地模式按观战记录的 battle_node_id 查注册表,实例取该节点登记的 NodeUuid —— 记录建房时只有 node_id,实例字段
// 要到 L1 / L2 准入后才回填;节点在建房后重启时这里拿到的是新实例,房间已随旧进程消失,Add 会回房间不存在,语义正确。
// 全局模式取成局记录上固定的实例。实例为空时 fail-closed:battle 按实例拒绝,空实例的请求必然被拒。
func spectateTarget(ctx context.Context, s *svc.ServiceContext, node uint32, battle uint64) (endpoint, instance string, err error) {
	if s.Authority == nil {
		entry, e := s.BattleNodes.EntryOfNode(node)
		if e != nil {
			return "", "", e
		}
		endpoint, instance = entry.Endpoint, entry.NodeUuid
	} else {
		f, e := s.Authority.Formation(ctx, battle)
		if e != nil {
			return "", "", e
		}
		if f.State != 4 {
			return "", "", errors.New("成局尚未确认或已经中止")
		}
		endpoint, instance = f.TargetEndpoint, f.TargetInstanceId
	}
	if instance == "" {
		return "", "", fmt.Errorf("battle 节点未登记实例 uuid(node=%d)", node)
	}
	return endpoint, instance, nil
}

// battleInstanceReplaced 判定注册表里这个 node_id 已经是别的实例:原实例的进程已被替换,房间随它消失。只读内存镜像,不发 RPC。
// 查不到不算:etcd 抖动、租约短暂失效、多条注册歧义时都查不到,不代表实例已死(见 NodeWatcher.EntryOfInstance 的注释)。
func battleInstanceReplaced(svcCtx *svc.ServiceContext, node uint32, instance string) bool {
	if svcCtx.BattleNodes == nil {
		return false
	}
	entry, err := svcCtx.BattleNodes.EntryOfNode(node)
	return err == nil && entry.NodeUuid != "" && entry.NodeUuid != instance
}

// cleanupExpiredSpectateIndex 清理观战索引里 score 早于 TTL 窗口的残留成员
// (记录 string 键有 TTL 自清,ZSET 成员只能靠懒剔除 + 这里的定期兜底)。O(log N + M) 轻量,多实例重复执行幂等:
// 本地模式 matcher loop 每轮顺手执行;全局模式没有 matcher 轮,由 startGlobalSpectateMaintenance 的低频循环执行。
func cleanupExpiredSpectateIndex(svcCtx *svc.ServiceContext) {
	staleBefore := int64(spectateStaleBeforeMs(svcCtx))
	if staleBefore <= 0 {
		return
	}
	removed, err := svcCtx.MatchRedis.Zremrangebyscore(spectateBattlesActiveKey, 0, staleBefore)
	if err != nil {
		logx.Errorf("[matcher] Failed to clean expired spectator index entries: %v", err)
		return
	}
	if removed > 0 {
		logx.Infof("[matcher] Cleaned %d expired spectator index entries", removed)
	}
	// 赛事局索引成员的 score 是到期毫秒,同一轮顺手清(L2 配额的过期项由抢配额的 Lua 自己清)。
	if _, err := svcCtx.MatchRedis.Zremrangebyscore(spectateFeaturedActiveKey, 0, int64(nowMs())); err != nil {
		logx.Errorf("[matcher] Failed to clean expired featured index entries: %v", err)
	}
}

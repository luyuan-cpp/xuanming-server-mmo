package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

// 观战档位与 L1 / L2 准入(battle-spectate-tiered-fanout.md §3、§15.6;spectate:* 只有 match 读写):
//
//	spectate:tier:{id}          string  档位数值(eSpectateTier),不存在 = DIRECT;只升不降,TTL 同观战记录
//	spectate:relay_gates:{id}   hash    gate_node_id → gate_instance_id:本场已在 battle 挂上的中继 gate,TTL 同上
//	spectate:featured:{id}      string  运营标记的赛事局,TTL 同上
//	spectate:promote_try:{id}   string  升 L2 尝试的短租约,见 spectatePromoteLeaseSeconds
//	spectate:bcast:active       zset    全服 L2 配额:member=battle_id,score=战斗期限毫秒
//	spectate:featured:active    zset    赛事局索引:member=battle_id,score=标记到期毫秒;只用来枚举,标记以上面的键为准
//
// 本场的键都带同一个 hash tag {id},落同一 slot,以后要合进一条 Lua 不用改键名。两个全服 ZSET 单独一个 slot,
// 不与本场的键进同一条 Lua。档位一旦升起,这场的新观众就不再撞 L0(全局模式下撞一次 L0 要走事务、锁 formation 行、
// 发两次 gRPC,热门局不能让每个人都撞一次)。
const (
	spectateBroadcastActiveKey = "spectate:bcast:active"
	spectateFeaturedActiveKey  = "spectate:featured:active"
)

// spectatePromoteTimeout 单次 PromoteSpectateBroadcast 的时间预算。比 addObserverTimeout 短:升档只是顺带的优化,
// 全局路径后面还要提交 SQL,不能让它吃光请求预算。超时后租约到期时的下一名观众会重试(battle 侧幂等)。
const spectatePromoteTimeout = time.Second

// spectatePromoteLeaseSeconds 是升 L2 尝试的短租约时长:同一场每 3 秒至多一名观众去抢配额、调 battle,其余观众直接按 RELAY 签票。
// 没有它时,battle 过载让 Promote 超时之后,每名到达的观众都要等满 spectatePromoteTimeout 并打一条 Error,观众请求反过来放大 battle
// 的压力(L1 本来就是要把观众和 battle 隔开,AGENTS §11.3 也要求重试有退避)。失败不提前释放,等它到期就是退避;
// 比 spectatePromoteTimeout 长,持有者的 RPC 必定先结束。
const spectatePromoteLeaseSeconds = 3

func spectateTierKey(battleId uint64) string {
	return fmt.Sprintf("spectate:tier:{%d}", battleId)
}

func spectateRelayGatesKey(battleId uint64) string {
	return fmt.Sprintf("spectate:relay_gates:{%d}", battleId)
}

func spectateFeaturedKey(battleId uint64) string {
	return fmt.Sprintf("spectate:featured:{%d}", battleId)
}

func spectatePromoteTryKey(battleId uint64) string {
	return fmt.Sprintf("spectate:promote_try:{%d}", battleId)
}

// spectateBookkeepingContext 给 RPC 成功之后的记账用:RPC 已经生效,请求 ctx 截止不能让这笔账丢掉。
func spectateBookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), spectateRedisOpTimeout)
}

// ---- 档位 ----

// raiseSpectateTierScript 新值大于旧值才写,保持已有 TTL;首次写入按 ARGV[2] 设 TTL。返回写后的实际档位。
// 必须比较后再写:并发的升档可能已经把这场升到更高档,直接 SET 会把它降回去。
const raiseSpectateTierScript = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0') or 0
local wanted = tonumber(ARGV[1])
if wanted <= current then
	return current
end
if redis.call('EXISTS', KEYS[1]) == 1 then
	redis.call('SET', KEYS[1], ARGV[1], 'KEEPTTL')
else
	redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
end
return wanted
`

// raiseSpectateTier 把本场档位升到 wanted(已不低于它就不动),返回写后的实际档位。
func raiseSpectateTier(ctx context.Context, s *svc.ServiceContext, battleId uint64, wanted battlepb.ESpectateTier) (battlepb.ESpectateTier, error) {
	res, err := s.MatchRedis.EvalCtx(ctx, raiseSpectateTierScript, []string{spectateTierKey(battleId)}, int(wanted), spectateTTLSeconds(s))
	if err != nil {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, fmt.Errorf("升观战档位失败: %w", err)
	}
	tier, _ := res.(int64)
	return battlepb.ESpectateTier(tier), nil
}

// readSpectateTier 读本场档位,键不存在 = DIRECT。读不出来或值不认识一律报错(fail-closed):猜 DIRECT 会让热门局的
// 观众全部回去撞 L0,猜高了会跳过 battle 的准入。
func readSpectateTier(ctx context.Context, s *svc.ServiceContext, battleId uint64) (battlepb.ESpectateTier, error) {
	raw, err := s.MatchRedis.GetCtx(ctx, spectateTierKey(battleId))
	if err != nil {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, fmt.Errorf("读观战档位失败: %w", err)
	}
	if raw == "" {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, fmt.Errorf("观战档位值非法 %q: %w", raw, err)
	}
	if _, known := battlepb.ESpectateTier_name[int32(value)]; !known {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, fmt.Errorf("观战档位值非法 %q", raw)
	}
	return battlepb.ESpectateTier(value), nil
}

// spectateAdmissionTier 是 WatchBattle 的档位判定(Redis 与全局两条路径共用):决定这名观众走 L0 还是 L1 / L2。
// 中继不可用时一律 DIRECT,不看键:开关关掉就回到上一档的行为(§11),直连席满了回 16021。
func spectateAdmissionTier(ctx context.Context, s *svc.ServiceContext, battleId uint64) (battlepb.ESpectateTier, error) {
	if !s.Config.Spectate.RelayUsable() {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, nil
	}
	return readSpectateTier(ctx, s, battleId)
}

// spectateDirectFullToRelay 处理 L0 回「直连席已满」(两条路径共用):中继可用就把本场升到 RELAY 并返回写后的实际档位,
// 调用方继续做 L1 / L2 准入;中继不可用返回 DIRECT,调用方回 16021。
func spectateDirectFullToRelay(ctx context.Context, s *svc.ServiceContext, battleId uint64) (battlepb.ESpectateTier, error) {
	if !s.Config.Spectate.RelayUsable() {
		return battlepb.ESpectateTier_SPECTATE_TIER_DIRECT, nil
	}
	return raiseSpectateTier(ctx, s, battleId, battlepb.ESpectateTier_SPECTATE_TIER_RELAY)
}

func spectateTierLabel(tier battlepb.ESpectateTier) string {
	switch tier {
	case battlepb.ESpectateTier_SPECTATE_TIER_RELAY:
		return metrics.SpectateTierRelay
	case battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST:
		return metrics.SpectateTierBroadcast
	default:
		return metrics.SpectateTierDirect
	}
}

// ---- battle RPC 缝(与 addObserverFn 同一模式:生产实现只做「建连 + 发一次请求」)----

var (
	attachRelayGateFn          = attachRelayGateRPC
	promoteSpectateBroadcastFn = promoteSpectateBroadcastRPC
)

func attachRelayGateRPC(ctx context.Context, endpoint string, req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, addObserverTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).AttachRelayGate(callCtx, req)
}

func promoteSpectateBroadcastRPC(ctx context.Context, endpoint string, req *battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, spectatePromoteTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).PromoteSpectateBroadcast(callCtx, req)
}

// checkedBattleSource 校验 battle 回包里的来源:实例必须等于请求的实例,期限不能缺。缺了签出来的凭证没有用。
func checkedBattleSource(instance string, replyInstance string, deadlineMs uint64) (spectateSource, error) {
	source := spectateSource{battleInstanceId: replyInstance, deadlineMs: deadlineMs}
	if replyInstance != instance || !source.known() {
		return spectateSource{}, fmt.Errorf("battle 回包的实例或期限不符 want_instance=%s instance=%s deadline_ms=%d", instance, replyInstance, deadlineMs)
	}
	return source, nil
}

// ---- L1:中继 gate ----

// recordRelayGateScript 记一台已挂接的 gate,首次写入时设 TTL。HSET 与 EXPIRE 要在一条 Lua 里:中间崩溃会留下永不过期的 HASH。
const recordRelayGateScript = `
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
if redis.call('TTL', KEYS[1]) < 0 then
	redis.call('EXPIRE', KEYS[1], ARGV[3])
end
return 1
`

// relayGateAttached 查本 gate 的这个实例是否已在 battle 挂上。node 相同而实例不同(gate 重启)要重新挂,battle 会覆盖旧实例。
func relayGateAttached(ctx context.Context, s *svc.ServiceContext, battleId uint64, viewer spectateLobbyViewer) (bool, error) {
	instance, err := s.MatchRedis.HgetCtx(ctx, spectateRelayGatesKey(battleId), strconv.FormatUint(uint64(viewer.gateNodeId), 10))
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读中继 gate 集合失败: %w", err)
	}
	return instance == viewer.gateInstanceId, nil
}

// recordRelayGate 在 AttachRelayGate 成功之后记账(先 RPC 后记账:账上有的 gate 一定真挂上了,否则这台 gate 的观众永远收不到帧;
// 并发的重复 Attach 由 battle 幂等兜底)。记账失败只记日志:gate 已经挂上,这名观众不受影响,下一名观众多挂一次而已。
func recordRelayGate(ctx context.Context, s *svc.ServiceContext, battleId uint64, viewer spectateLobbyViewer) {
	writeCtx, cancel := spectateBookkeepingContext(ctx)
	defer cancel()
	if _, err := s.MatchRedis.EvalCtx(writeCtx, recordRelayGateScript, []string{spectateRelayGatesKey(battleId)},
		strconv.FormatUint(uint64(viewer.gateNodeId), 10), viewer.gateInstanceId, spectateTTLSeconds(s)); err != nil {
		logx.Errorf("[spectate] Failed to record relay gate (next viewer attaches again) battle=%d gate=%d: %v", battleId, viewer.gateNodeId, err)
	}
}

// attachRelayGate 让 battle 把观众所在的 gate 挂进本场中继。broadcast=true 表示本场已是广播档,没有挂 gate。
// battle 明确拒绝时 rejectTip≠0 且 err 同时非 nil(与 addObserver 同一约定)。
func attachRelayGate(ctx context.Context, s *svc.ServiceContext, record *matchpb.SpectateBattleRecord, viewer spectateLobbyViewer) (source spectateSource, broadcast bool, rejectTip uint32, err error) {
	battleId := record.GetSummary().GetBattleId()
	if err = ctx.Err(); err != nil {
		return spectateSource{}, false, 0, fmt.Errorf("请求已截止,不再挂接中继: %w", err)
	}
	if viewer.gateNodeId == 0 || viewer.gateInstanceId == "" {
		// 中继帧必须带 target_gate_id 与 target_instance_id(AGENTS §7.2),缺了 gate 收不到。
		return spectateSource{}, false, 0, fmt.Errorf("观众会话缺 gate 路由 gate=%d", viewer.gateNodeId)
	}
	endpoint, instance, err := spectateTarget(ctx, s, record.GetBattleNodeId(), battleId)
	if err != nil {
		return spectateSource{}, false, 0, fmt.Errorf("定位 battle 节点失败: %w", err)
	}
	reply, err := attachRelayGateFn(ctx, endpoint, &battlepb.AttachRelayGateRequest{
		BattleId:         battleId,
		GateNodeId:       viewer.gateNodeId,
		GateInstanceId:   viewer.gateInstanceId,
		TargetInstanceId: instance,
	})
	if err != nil {
		return spectateSource{}, false, 0, fmt.Errorf("AttachRelayGate RPC 失败: %w", err)
	}
	if tip := reply.GetErrorMessage().GetId(); tip != 0 {
		return spectateSource{}, false, tip, fmt.Errorf("battle 节点拒绝挂接中继 tip_id=%d", tip)
	}
	source, err = checkedBattleSource(instance, reply.GetBattleInstanceId(), reply.GetDeadlineMs())
	if err != nil {
		return spectateSource{}, false, 0, err
	}
	return source, reply.GetBroadcast(), 0, nil
}

// ---- L2:全服配额与升档 ----

// broadcastQuotaClaim 是抢全服 L2 配额的结果,取值与 claimBroadcastQuotaScript 的返回值一一对应。
type broadcastQuotaClaim int64

const (
	broadcastQuotaFull    broadcastQuotaClaim = 0 // 配额已满
	broadcastQuotaClaimed broadcastQuotaClaim = 1 // 本次新占到
	broadcastQuotaHeld    broadcastQuotaClaim = 2 // 本场已在集合里(另一名观众正在升档,或已经升上去了)
)

// claimBroadcastQuotaScript 先清过期项,本场已在集合里或 ZCARD < B 才占。清过期与判定必须在一条 Lua 里,否则配额数不准。
const claimBroadcastQuotaScript = `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if redis.call('ZSCORE', KEYS[1], ARGV[3]) then
	return 2
end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then
	return 0
end
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[3])
return 1
`

func claimBroadcastQuota(ctx context.Context, s *svc.ServiceContext, battleId, expiryMs, nowMs uint64) (broadcastQuotaClaim, error) {
	res, err := s.MatchRedis.EvalCtx(ctx, claimBroadcastQuotaScript, []string{spectateBroadcastActiveKey},
		strconv.FormatUint(nowMs, 10), s.Config.Spectate.BroadcastMaxConcurrent,
		strconv.FormatUint(battleId, 10), strconv.FormatUint(expiryMs, 10))
	if err != nil {
		return broadcastQuotaFull, fmt.Errorf("抢全服广播配额失败: %w", err)
	}
	n, _ := res.(int64)
	return broadcastQuotaClaim(n), nil
}

// spectateQuotaExpiryMs 是抢配额时的到期时刻。战斗期限还不知道时(本地模式第一次升档)按记录的创建时刻 + TTL 估算:
// 只在 RPC 在途这一小段用,升档成功后换成 battle 回包里的真实期限;实例中途崩溃时占位最多白占到这个时刻。
func spectateQuotaExpiryMs(s *svc.ServiceContext, record *matchpb.SpectateBattleRecord) uint64 {
	if deadline := record.GetDeadlineMs(); deadline != 0 {
		return deadline
	}
	return record.GetSummary().GetCreatedAtMs() + uint64(spectateTTLSeconds(s))*1000
}

// requestSpectateBroadcast 调 battle 升广播档(单向、幂等),返回 battle 回包里的来源。
func requestSpectateBroadcast(ctx context.Context, s *svc.ServiceContext, record *matchpb.SpectateBattleRecord) (spectateSource, uint32, error) {
	battleId := record.GetSummary().GetBattleId()
	endpoint, instance, err := spectateTarget(ctx, s, record.GetBattleNodeId(), battleId)
	if err != nil {
		return spectateSource{}, 0, fmt.Errorf("定位 battle 节点失败: %w", err)
	}
	reply, err := promoteSpectateBroadcastFn(ctx, endpoint, &battlepb.PromoteSpectateBroadcastRequest{
		BattleId:         battleId,
		TargetInstanceId: instance,
	})
	if err != nil {
		return spectateSource{}, 0, fmt.Errorf("PromoteSpectateBroadcast RPC 失败: %w", err)
	}
	if tip := reply.GetErrorMessage().GetId(); tip != 0 {
		return spectateSource{}, tip, fmt.Errorf("battle 节点拒绝升广播 tip_id=%d", tip)
	}
	source, err := checkedBattleSource(instance, reply.GetBattleInstanceId(), reply.GetDeadlineMs())
	return source, 0, err
}

// promoteSpectateBroadcast 尝试把本场升到 L2:取短租约 → 抢配额 → 调 battle → 档位置 BROADCAST。配额满、拿不到租约或失败都不是错误,
// 本场留在原档,不影响当前观众的准入;租约到期后的下一名观众(或运营重试)再试。升上去时返回 battle 回包里的来源;
// battle 明确拒绝时 rejectTip≠0,调用方据此判断房间是否已不在(spectateRejectVerdict)。
//
// 配额只由新占到的那个请求退还,而且退之前要复读档位(releaseBroadcastQuotaUnlessPromoted):租约只防住正常时序,持有者卡过 3 秒时
// 后来者仍可能看到「已在集合里」并升档成功。升档成功后无条件 ZADD,Attach 回 broadcast=true 时也补记,账目不会一直少这一场。
func promoteSpectateBroadcast(ctx context.Context, s *svc.ServiceContext, record *matchpb.SpectateBattleRecord) (source spectateSource, promoted bool, rejectTip uint32) {
	battleId := record.GetSummary().GetBattleId()
	if s.Config.Spectate.BroadcastMaxConcurrent <= 0 {
		return spectateSource{}, false, 0
	}
	leased, err := s.MatchRedis.SetnxExCtx(ctx, spectatePromoteTryKey(battleId), "1", spectatePromoteLeaseSeconds)
	if err != nil {
		logx.Errorf("[spectate] Broadcast promotion failed to take attempt lease; staying on current tier battle=%d: %v", battleId, err)
		metrics.ObserveSpectatePromote(metrics.SpectatePromoteRPCFailed)
		return spectateSource{}, false, 0
	}
	if !leased {
		// 不打日志:热门局每名观众都可能走到这里,看指标就够了。
		metrics.ObserveSpectatePromote(metrics.SpectatePromoteThrottled)
		return spectateSource{}, false, 0
	}
	claim, err := claimBroadcastQuota(ctx, s, battleId, spectateQuotaExpiryMs(s, record), nowMs())
	if err != nil {
		logx.Errorf("[spectate] Broadcast promotion failed to claim quota; staying on current tier battle=%d: %v", battleId, err)
		metrics.ObserveSpectatePromote(metrics.SpectatePromoteRPCFailed)
		return spectateSource{}, false, 0
	}
	if claim == broadcastQuotaFull {
		// 不打日志:配额满时热门局每个租约周期都会走到这里,看指标就够了。
		metrics.ObserveSpectatePromote(metrics.SpectatePromoteQuotaFull)
		return spectateSource{}, false, 0
	}
	source, rejectTip, err = requestSpectateBroadcast(ctx, s, record)
	if err != nil {
		logx.Errorf("[spectate] Broadcast promotion rejected or failed; staying on current tier battle=%d new_claim=%t: %v",
			battleId, claim == broadcastQuotaClaimed, err)
		if claim == broadcastQuotaClaimed {
			releaseBroadcastQuotaUnlessPromoted(ctx, s, battleId)
		}
		metrics.ObserveSpectatePromote(metrics.SpectatePromoteRPCFailed)
		return spectateSource{}, false, rejectTip
	}
	writeCtx, cancel := spectateBookkeepingContext(ctx)
	defer cancel()
	recordBroadcastQuota(writeCtx, s, battleId, source.deadlineMs)
	rememberSpectateSource(writeCtx, s, record, source)
	if _, err := raiseSpectateTier(writeCtx, s, battleId, battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST); err != nil {
		// battle 已是广播档:下一名观众的 Attach 回 broadcast=true,或重试升档时同步过来。
		logx.Errorf("[spectate] Battle promoted but tier not recorded; next viewer syncs it battle=%d: %v", battleId, err)
	}
	logx.Infof("[spectate] Battle promoted to global broadcast battle=%d", battleId)
	metrics.ObserveSpectatePromote(metrics.SpectatePromoteOK)
	return source, true, 0
}

// recordBroadcastQuota 把正在广播的一场记到全服配额上,score = 战斗期限。不看 B:battle 已经在广播了,账上必须有它,超额也记,
// 后面的抢占才会被挡住,实际 L2 场数不再继续突破 B。
func recordBroadcastQuota(ctx context.Context, s *svc.ServiceContext, battleId, deadlineMs uint64) {
	if _, err := s.MatchRedis.ZaddCtx(ctx, spectateBroadcastActiveKey, int64(deadlineMs), strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] Failed to record broadcast quota battle=%d: %v", battleId, err)
	}
}

// releaseBroadcastQuotaUnlessPromoted 退还新占的配额。退之前复读档位:已是 BROADCAST 说明别的请求已经升上去,这时 ZREM 会抹掉成功者的账,
// 本场继续广播却不计数,B 被突破且不自愈。读不到档位也不退:最坏白占到本场结束(释放见 releaseSpectateOnOutcome)或期限,
// 比突破 B 的代价小。复读与 ZREM 之间还有极窄的窗口,由 Attach 回 broadcast=true 时的补记收回。
func releaseBroadcastQuotaUnlessPromoted(ctx context.Context, s *svc.ServiceContext, battleId uint64) {
	writeCtx, cancel := spectateBookkeepingContext(ctx)
	defer cancel()
	tier, err := readSpectateTier(writeCtx, s, battleId)
	if err != nil {
		logx.Errorf("[spectate] Keeping broadcast quota because tier is unreadable battle=%d: %v", battleId, err)
		return
	}
	if tier == battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST {
		return
	}
	if _, err := s.MatchRedis.ZremCtx(writeCtx, spectateBroadcastActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] Failed to release broadcast quota (expires with the battle deadline) battle=%d: %v", battleId, err)
	}
}

// spectatePromoteDue 判定 RELAY 档的这场该不该试着升 L2:有观众的 gate 数到了阈值,或是运营标记的赛事局。读失败只按「不升」处理。
func spectatePromoteDue(ctx context.Context, s *svc.ServiceContext, battleId uint64) bool {
	conf := s.Config.Spectate
	if conf.BroadcastMaxConcurrent <= 0 {
		return false
	}
	if conf.BroadcastAutoGateThreshold > 0 {
		gates, err := s.MatchRedis.HlenCtx(ctx, spectateRelayGatesKey(battleId))
		if err != nil {
			logx.Errorf("[spectate] Failed to count relay gates battle=%d: %v", battleId, err)
		} else if gates >= conf.BroadcastAutoGateThreshold {
			return true
		}
	}
	featured, err := s.MatchRedis.ExistsCtx(ctx, spectateFeaturedKey(battleId))
	if err != nil {
		logx.Errorf("[spectate] Failed to read featured flag battle=%d: %v", battleId, err)
		return false
	}
	return featured
}

// ---- 观战记录里的来源 ----

// rememberSpectateSourceScript 只改写还存在的记录并保持 TTL:记录已被懒剔除时不能把它写回来。
const rememberSpectateSourceScript = `
if redis.call('EXISTS', KEYS[1]) == 0 then
	return 0
end
redis.call('SET', KEYS[1], ARGV[1], 'KEEPTTL')
return 1
`

// rememberSpectateSource 把 battle 回包里的实例与期限记进观战记录,之后的观众(已挂接的 gate、L2)不用再问 battle。
// 全局模式的记录由成局原件现算(spectateRecordFromFormation),这两项本来就有,不写 Redis。
// 写失败或被 gather 的补写盖掉都无害:字段缺失时下一名观众会再问 battle 一次(幂等)。
func rememberSpectateSource(ctx context.Context, s *svc.ServiceContext, record *matchpb.SpectateBattleRecord, source spectateSource) {
	if record.GetBattleInstanceId() == source.battleInstanceId && record.GetDeadlineMs() == source.deadlineMs {
		return
	}
	record.BattleInstanceId, record.DeadlineMs = source.battleInstanceId, source.deadlineMs
	if s.Authority != nil {
		return
	}
	battleId := record.GetSummary().GetBattleId()
	raw, err := proto.Marshal(record)
	if err != nil {
		logx.Errorf("[spectate] Failed to serialize spectator record with source battle=%d: %v", battleId, err)
		return
	}
	if _, err := s.MatchRedis.EvalCtx(ctx, rememberSpectateSourceScript, []string{spectateBattleKey(battleId)}, string(raw)); err != nil {
		logx.Errorf("[spectate] Failed to store battle source in spectator record battle=%d: %v", battleId, err)
	}
}

// ---- 准入 ----

// spectateLobbyViewer 是 L1 / L2 准入的观众:玩家身份与当前会话所在的 gate(中继要发到这台 gate)。
type spectateLobbyViewer struct {
	playerId       uint64
	gateNodeId     uint32
	gateInstanceId string
}

// spectateLobbyAdmission 是一次 L1 / L2 准入的结论:成功时 ticket 非空;battle 明确拒绝时 rejectTip≠0(err 同时非 nil)。
type spectateLobbyAdmission struct {
	ticket    []byte
	rejectTip uint32
}

// admitSpectateLobby 是 L1 / L2 准入(§15.6 第 3、4 步,两条路径共用),幂等:同一观众重复调用只是多签一张凭证。不写观战标记、
// 不写 match_spectator、不锁 match_formation:L1 / L2 观众在 match 没有占用,互斥是软的(Q1 / §15.1),调用方在此之前做只读检查。
//   - RELAY:本 gate 没挂过就调 AttachRelayGate,成功后再记账;随后判自动升档;
//   - BROADCAST:不调任何 battle RPC,只签凭证;
//   - 凭证的实例与期限取 battle 回包,或观战记录。
func admitSpectateLobby(ctx context.Context, s *svc.ServiceContext, record *matchpb.SpectateBattleRecord, tier battlepb.ESpectateTier, viewer spectateLobbyViewer) (spectateLobbyAdmission, error) {
	battleId := record.GetSummary().GetBattleId()
	source := spectateSource{battleInstanceId: record.GetBattleInstanceId(), deadlineMs: record.GetDeadlineMs()}
	switch tier {
	case battlepb.ESpectateTier_SPECTATE_TIER_RELAY:
		attached, err := relayGateAttached(ctx, s, battleId, viewer)
		if err != nil {
			return spectateLobbyAdmission{}, err
		}
		if !attached || !source.known() {
			attachedSource, broadcast, rejectTip, err := attachRelayGate(ctx, s, record, viewer)
			if err != nil {
				return spectateLobbyAdmission{rejectTip: rejectTip}, err
			}
			source = attachedSource
			writeCtx, cancel := spectateBookkeepingContext(ctx)
			defer cancel()
			rememberSpectateSource(writeCtx, s, record, source)
			if broadcast {
				// 本场已是广播档,这台 gate 没有挂上,不记 gate 账;档位同步失败无害:凭证照样有效,下一名观众再同步。
				// 配额要补记:Promote 在 battle 已生效而回包超时时配额已被退还,此后没有观众再碰配额,这场会一直不计数。
				tier = battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST
				if _, err := raiseSpectateTier(writeCtx, s, battleId, tier); err != nil {
					logx.Errorf("[spectate] Failed to sync broadcast tier battle=%d: %v", battleId, err)
				}
				recordBroadcastQuota(writeCtx, s, battleId, source.deadlineMs)
			} else {
				recordRelayGate(ctx, s, battleId, viewer)
			}
		}
		if tier == battlepb.ESpectateTier_SPECTATE_TIER_RELAY && spectatePromoteDue(ctx, s, battleId) {
			promoted, ok, rejectTip := promoteSpectateBroadcast(ctx, s, record)
			if ok {
				tier, source = battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST, promoted
			} else if _, _, roomMissing := spectateRejectVerdict(rejectTip); roomMissing {
				// 已挂接 gate 的观众不调 Attach,这是 match 在这条路径上唯一能知道场已结束的机会;其它升档失败不影响这名观众。
				return spectateLobbyAdmission{rejectTip: rejectTip}, fmt.Errorf("battle 节点回房间不存在 tip_id=%d", rejectTip)
			}
		}
	case battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST:
		if !source.known() {
			// 只有本地模式才可能走到这里:升档者记来源失败,或记录被 gather 的补写盖掉。升广播幂等,问一次就记下,不影响后面的观众。
			fetched, rejectTip, err := requestSpectateBroadcast(ctx, s, record)
			if err != nil {
				return spectateLobbyAdmission{rejectTip: rejectTip}, err
			}
			source = fetched
			rememberSpectateSource(ctx, s, record, source)
		}
	default:
		return spectateLobbyAdmission{}, fmt.Errorf("档位 %s 不走 L1 / L2 准入", tier.String())
	}
	// 已挂接的 gate 与 L2 观众不调 battle,来源可能是已经崩溃或被替换的实例。凭证签给它,观众只会空等 gate 30 秒后收尾,
	// 再请求又拿到同一个死实例。只查内存注册表,不增加 RPC;按房间不存在回给调用方,走现有的懒剔除与 16018。
	if battleInstanceReplaced(s, record.GetBattleNodeId(), source.battleInstanceId) {
		return spectateLobbyAdmission{rejectTip: uint32(table.CommonError_kEntityIsNull)},
			fmt.Errorf("battle 节点 %d 已换实例,来源实例 %s 已不存在", record.GetBattleNodeId(), source.battleInstanceId)
	}
	ticket, err := issueSpectateTicket(s.Config.Spectate, battleId, viewer.playerId, source, nowMs())
	if err != nil {
		return spectateLobbyAdmission{}, err
	}
	metrics.ObserveSpectateTier(spectateTierLabel(tier))
	return spectateLobbyAdmission{ticket: ticket}, nil
}

// ---- 赛事局标记 ----

// markSpectateFeatured 写赛事局标记与索引,TTL 同观战记录。先写标记后写索引,取消时反过来:索引里有的一定有标记,
// 列表不必逐个核对标记。
func markSpectateFeatured(ctx context.Context, s *svc.ServiceContext, battleId uint64) error {
	ttl := spectateTTLSeconds(s)
	if err := s.MatchRedis.SetexCtx(ctx, spectateFeaturedKey(battleId), "1", ttl); err != nil {
		return fmt.Errorf("写赛事局标记失败: %w", err)
	}
	expiryMs := int64(nowMs()) + int64(ttl)*1000
	if _, err := s.MatchRedis.ZaddCtx(ctx, spectateFeaturedActiveKey, expiryMs, strconv.FormatUint(battleId, 10)); err != nil {
		return fmt.Errorf("写赛事局索引失败: %w", err)
	}
	return nil
}

// unmarkSpectateFeatured 取消赛事局标记,只删标记与索引,不降档(§15.6:一场只升不降)。
func unmarkSpectateFeatured(ctx context.Context, s *svc.ServiceContext, battleId uint64) error {
	if _, err := s.MatchRedis.ZremCtx(ctx, spectateFeaturedActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
		return fmt.Errorf("删赛事局索引失败: %w", err)
	}
	if _, err := s.MatchRedis.DelCtx(ctx, spectateFeaturedKey(battleId)); err != nil {
		return fmt.Errorf("删赛事局标记失败: %w", err)
	}
	return nil
}

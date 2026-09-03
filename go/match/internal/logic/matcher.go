package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
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

// pruneQueueScript 空队列懒剔除(KEYS=[index, queue, rank],ARGV=[queueKey]):
// LLEN==0 且 list key 不存在才 SREM;此时评分镜像若还有成员就是孤儿(每条改
// list 的 Lua 都同步改镜像,list 为空镜像必空;例外只有异常数据),顺手 DEL 掉,
// 剔除后 LLEN==0 且 ZCARD==0 成立。判定与剔除放同一条 Lua —— 分开做的话
// JoinQueue 的 SADD+RPUSH+ZADD 若恰好落在 EXISTS 与 SREM 之间,刚登记的队列会被
// 立刻摘掉;三 key 同 {mq} slot,集群下可用。幂等,多实例重复执行无害。
const pruneQueueScript = `if redis.call("LLEN", KEYS[2]) == 0 and redis.call("EXISTS", KEYS[2]) == 0 then
	if redis.call("ZCARD", KEYS[3]) > 0 then
		redis.call("DEL", KEYS[3])
	end
	return redis.call("SREM", KEYS[1], ARGV[1])
end
return 0`

// queueScanLimit 每次弹组读取的队列前缀长度(queueSnapshotScript 的 LRANGE 上限):
// 锚点与候选都只在这段里找。队列 QPS 极低、深度通常个位数(§6),256 足够;
// 更深的成员等前面的人被弹走后自然进入前缀。
const queueScanLimit = 256

// maxAnchorAttempts 一次 popGroup 最多尝试的锚点数:队首凑不到候选时不阻塞
// 后面的人(后面的人按各自的等待时间取容差),但一轮内不无限扫下去。
const maxAnchorAttempts = 32

// runGatherFn 是 matcher / PVE_SOLO 弹组后调用的开局管线入口。包级变量是为了
// 测试能把它换成记录器 —— 真 RunGather 要 gRPC 到 scene / battle 节点。
var runGatherFn = RunGather

// setQueueDepthFn 是 queue_depth gauge 的写入口,同样留作测试注入点
// (验证"只有持锁实例上报、剔除归零",见设计决策 D5c)。
var setQueueDepthFn = metrics.SetQueueDepth

// setStarvedAnchorWaitFn 是 starved_anchor_wait_seconds gauge 的写入口(测试注入点):
// popGroup 本轮尝试过却凑不到候选的锚点里等得最久的秒数,0 = 没有这样的锚点。
// wait_seconds 只在成组时记样本,饥饿中的锚点在那里不可见,这条 gauge 补上。
var setStarvedAnchorWaitFn = metrics.SetStarvedAnchorWait

// battle 池为空时的"暂停凑单"告警限频状态(UnixNano),见 matchQueueOnce。
var lastNoBattleNodeWarnNs atomic.Int64

const noBattleNodeWarnInterval = 10 * time.Second

// 锚点饥饿告警的限频状态:queueKey → 上次告警 UnixNano,见 warnStarvedAnchor。
var lastStarvedAnchorWarnNs sync.Map

const starvedAnchorWarnInterval = 10 * time.Second

// warnStarvedAnchor 对"容差曲线已饱和仍凑不到候选"的锚点记一条限频(每队列 10s)
// Error 日志:这是曲线本身救不了的饥饿(段位里没人 / 分差 > RatingToleranceMax),
// 只能等 RatingToleranceMaxWaitSeconds 的 ∞ 兜底;运维看到这条就该看队列人数。
func warnStarvedAnchor(queueKey string, anchor uint64, waitSeconds int64, tol float64) {
	now := time.Now().UnixNano()
	if last, ok := lastStarvedAnchorWarnNs.Load(queueKey); ok && now-last.(int64) < int64(starvedAnchorWarnInterval) {
		return
	}
	lastStarvedAnchorWarnNs.Store(queueKey, now)
	logx.Errorf("[matcher] 锚点已等 %ds(容差 %.0f 已饱和)仍凑不到候选,队列在饥饿 queue=%s anchor=%d",
		waitSeconds, tol, queueKey, anchor)
}

// legacyQueueSweepInterval 旧格式队列的定期搬迁间隔(见 migrateLegacyQueues):
// 启动时先搬一次,之后每隔这么久再扫 —— 滚动升级混跑窗口内旧实例仍会往旧 key
// RPUSH,只搬一次不够;SCAN 走的是共享库全键空间,不能每轮 500ms 都做。
const legacyQueueSweepInterval = time.Minute

// StartMatcherLoop 启动 matcher 定时循环(调用方放 goroutine)。
// 多实例安全:每个 (mode, battle_config_id) 队列的凑单临界区由
// match:{mq}:lock:{mode}:{config} 的 SETNX 锁保护,拿不到锁跳过本轮
// (设计文档 §5.4)。锁只覆盖"弹出成组"这一小段;弹出后的 gather 在
// 独立 goroutine 里执行,不占锁。
func StartMatcherLoop(ctx context.Context, svcCtx *svc.ServiceContext) {
	interval := time.Duration(svcCtx.Config.MatcherIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	migrateLegacyQueues(svcCtx)
	lastSweep := time.Now()
	safego.Loop(ctx, "match.matcher", interval, func(ctx context.Context) {
		if time.Since(lastSweep) >= legacyQueueSweepInterval {
			migrateLegacyQueues(svcCtx)
			lastSweep = time.Now()
		}
		runMatcherRound(ctx, svcCtx)
	})
}

// migrateLegacyQueues 把加 hash tag 之前写入的旧格式队列(`match:queue:<mode>:<config>`,
// 见 keys.go legacyMatchQueueKey)搬进新队列。新 matcher 只遍历注册集,旧 list
// 既不在注册集也不会再被读:滚动升级前已入队的玩家永远弹不到,GetQueueStatus
// 永远 QUEUED、JoinQueue 一直 ErrAlreadyQueued(6h),而无 TTL 的旧 list 永久残留。
//
// 旧 key 由改造前的实例写在共享库(SharedRedis;MatchRedis 未配置时两者同一句柄),
// 所以在 SharedRedis 上 SCAN,成员搬进 MatchRedis 的新队列。逐个 RPOP 旧队尾 →
// LPUSH 新队首,旧队列整体保持原相对顺序并排在新队列现有成员之前(他们等得更久);
// 顺手给票据补 queue_key。RPOP 与 LPUSH 之间崩溃会丢一个成员,他的票据仍是
// queued 而队列无人 —— 由 JoinQueue 的 isQueuedTicketInQueue 自愈,不做跨 key 原子。
// MatchRedis 独立于共享库时,旧票据留在共享库里、新实例读不到,搬进来的成员会在
// popGroup 因票据缺失被丢弃,玩家重排即可;只保证旧 list 不再残留。
// 多实例重复执行幂等(RPOP 到空即停)。
func migrateLegacyQueues(svcCtx *svc.ServiceContext) {
	var cursor uint64
	moved := 0
	for {
		keys, next, err := svcCtx.SharedRedis.Scan(cursor, legacyMatchQueueScanPattern, 256)
		if err != nil {
			logx.Errorf("[matcher] 扫描旧格式队列失败: %v", err)
			return
		}
		for _, legacyKey := range keys {
			moved += migrateLegacyQueue(svcCtx, legacyKey)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	if moved > 0 {
		logx.Infof("[matcher] 已把 %d 名旧格式队列成员搬进新队列", moved)
	}
}

// migrateLegacyQueue 搬空一条旧队列,返回搬迁人数。
func migrateLegacyQueue(svcCtx *svc.ServiceContext, legacyKey string) int {
	mode, config, err := parseQueueKey(legacyKey)
	if err != nil {
		logx.Errorf("[matcher] 旧格式队列 key %q 无法解析,跳过: %v", legacyKey, err)
		return 0
	}
	queueKey := matchQueueKey(mode, config)
	moved := 0
	for {
		raw, err := svcCtx.SharedRedis.Rpop(legacyKey)
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				logx.Errorf("[matcher] Rpop 旧格式队列 %s 失败: %v", legacyKey, err)
			}
			break
		}
		if raw == "" {
			// go-zero 对空列表返回 ("", nil)。
			break
		}
		// 评分镜像按玩家当前评分写(旧实例的票据没有 rating 字段);
		// 非法成员按默认分照搬,由 popGroup 丢弃。
		playerId, parseErr := strconv.ParseUint(raw, 10, 64)
		rating := float64(defaultRating)
		if parseErr == nil {
			rating = loadRatingOrDefault(svcCtx, playerId)
		}
		if err := pushFrontRegistered(svcCtx, queueKey, raw, rating); err != nil {
			logx.Errorf("[matcher] 旧队列成员 %s 搬进 %s 失败(票据残留由 JoinQueue 自愈): %v", raw, queueKey, err)
			continue
		}
		moved++
		if parseErr != nil {
			continue
		}
		if _, err := svcCtx.MatchRedis.Eval(ticketSetQueueKeyScript, []string{matchTicketKey(playerId)}, queueKey); err != nil {
			logx.Errorf("[matcher] 旧票据补 queue_key 失败 player=%d: %v", playerId, err)
		}
	}
	if moved > 0 {
		logx.Infof("[matcher] 旧格式队列 %s 已搬进 %s,共 %d 人", legacyKey, queueKey, moved)
	}
	return moved
}

// runMatcherRound 遍历注册集里的队列 key 并逐个尝试凑单。
// 注册集取代 SCAN:集群下 SCAN 只落到随机一个分片,SMEMBERS 是单 key 命令。
// 注册集只增(JoinQueue 的 Lua)不自动减,空队列由 matchQueueOnce 懒剔除。
func runMatcherRound(ctx context.Context, svcCtx *svc.ServiceContext) {
	// 顺手清观战索引的过期残留(ZSET 成员无 TTL,懒剔除之外的定期兜底;
	// 多实例重复执行幂等,不值得为它单独抢锁/起 goroutine)。
	cleanupExpiredSpectateIndex(svcCtx)

	keys, err := svcCtx.MatchRedis.Smembers(matchQueueIndexKey)
	if err != nil {
		logx.Errorf("[matcher] 读队列注册集 %s 失败: %v", matchQueueIndexKey, err)
		return
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		matchQueueOnce(svcCtx, key)
	}
}

// matchQueueOnce 对单个队列执行一次凑单尝试。
func matchQueueOnce(svcCtx *svc.ServiceContext, queueKey string) {
	mode, config, err := parseQueueKey(queueKey)
	if err != nil {
		// 注册集混入异物:不是本服务写的,不动它,只告警。
		logx.Errorf("[matcher] 无法解析队列 key %q: %v", queueKey, err)
		return
	}

	required := requiredPlayers(svcCtx, mode, config)
	if required == 0 {
		// 配置被摘掉后残留的队列:不动数据,只告警,等运维处理。
		logx.Errorf("[matcher] 队列 %s 无凑满人数配置,跳过", queueKey)
		return
	}

	// battle 池为空时不弹组:否则 gather 必以 no_battle_node 秒败 → 幸存者回队首 →
	// 下一轮(500ms)再弹,形成热循环,每轮还白铸一个 battle_id、刷一批错误日志
	// (2026-09-02 本地实测:battle 节点端口撞车未注册,同一对玩家被反复"凑单成功")。
	// 排队保留在队列里,battle 节点回来后自然继续;告警限频 10s 一条。
	if svcCtx.BattleNodes != nil && svcCtx.BattleNodes.Count() == 0 {
		if now := time.Now().UnixNano(); now-lastNoBattleNodeWarnNs.Load() >= int64(noBattleNodeWarnInterval) {
			lastNoBattleNodeWarnNs.Store(now)
			logx.Errorf("[matcher] battle 池为空,暂停凑单 queue=%s(排队保留,等待 battle 节点注册)", queueKey)
		}
		return
	}

	// 凑单临界区:SETNX 锁,拿不到跳过本轮 —— 连 queue_depth 也不上报
	//(设计决策 D5c):多实例各自上报同一队列,看板 sum 会放大 N 倍;
	// 由持锁实例独家上报,任一时刻每个队列只有一个写者。
	lockKey := matcherLockKey(mode, config)
	lockTTL := int(svcCtx.Config.MatcherLockTTLSeconds)
	if lockTTL <= 0 {
		lockTTL = 10
	}
	acquired, err := svcCtx.MatchRedis.SetnxEx(lockKey, svcCtx.InstanceID, lockTTL)
	if err != nil {
		logx.Errorf("[matcher] 抢锁失败 %s: %v", lockKey, err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if _, err := svcCtx.MatchRedis.Eval(releaseLockScript, []string{lockKey}, svcCtx.InstanceID); err != nil {
			logx.Errorf("[matcher] 释放锁失败 %s: %v", lockKey, err)
		}
	}()

	modeName := matchpb.MatchMode(mode).String()
	configName := strconv.FormatUint(uint64(config), 10)
	depth, err := svcCtx.MatchRedis.Llen(queueKey)
	if err != nil {
		logx.Errorf("[matcher] Llen %s 失败: %v", queueKey, err)
		return
	}
	setQueueDepthFn(modeName, configName, depth)
	if depth < int(required) {
		// 人数不够谈不上"锚点凑不到候选":饥饿 gauge 归零,深度看 queue_depth。
		setStarvedAnchorWaitFn(modeName, configName, 0)
		if depth == 0 {
			pruneEmptyQueue(svcCtx, queueKey, modeName, configName)
		}
		return
	}

	// 持锁期间可以连续凑多组(队列深度允许时),摊薄扫描开销。
	matchedTTL := matchedTicketTTLFor(svcCtx, required)
	for {
		members, tickets, ok := popGroup(svcCtx, queueKey, required)
		if !ok {
			return
		}
		if matcherAfterPopHook != nil {
			matcherAfterPopHook(members)
		}
		// 弹出即凑单成功:先把票据推进 matched 态(此后取消太迟;短 TTL 按组
		// 大小算,本实例在 gather 中崩溃票据也会自灭),再放 goroutine 跑 gather ——
		// gather 是多跳 RPC,不能占凑单锁。带弹出时读到的 ticket id 做 CAS:
		// 失败说明票据在弹出后已被取消(CancelQueue 在 popGroup 的多次往返窗口内
		// 删票)或过期被重排 —— 该成员已退出,**不能**带进 gather,否则一个
		// "取消成功"的玩家会被冻结进战斗,而他的新票据与冻结状态互不感知。
		// 凑不满就把其余人 CAS 回 queued 并按原序回队首,本轮不启动 gather。
		var matched []uint64
		var keep []uint64 // 回队首名单:CAS 成功的 + Redis 出错状态未知的
		redisErr := false
		for _, playerId := range members {
			written, err := setTicketMatched(svcCtx, playerId, tickets[playerId], matchedTTL)
			if err != nil {
				logx.Errorf("[matcher] 推进 ticket matched 失败 player=%d: %v", playerId, err)
				redisErr = true
				keep = append(keep, playerId)
				continue
			}
			if !written {
				logx.Infof("[matcher] ticket 已被取消、替换或过期,该成员出局 player=%d expected=%s",
					playerId, tickets[playerId])
				continue
			}
			matched = append(matched, playerId)
			keep = append(keep, playerId)
		}
		if uint32(len(matched)) < required {
			logx.Infof("[matcher] 弹出后仅 %d/%d 人票据有效,其余回队首 queue=%s", len(matched), required, queueKey)
			requeueFront(svcCtx, queueKey, keep, tickets)
			if redisErr {
				// Redis 抖动:结束本轮,不在同一轮反复弹出同一批人。
				return
			}
			continue
		}
		logx.Infof("[matcher] 凑单成功 queue=%s members=%v matched_ttl=%ds", queueKey, members, matchedTTL)
		group := members
		safego.Go("match.gather.queue", func() {
			runGatherFn(svcCtx, matchpb.MatchMode(mode), config, group, true, tickets)
		})
	}
}

// pruneEmptyQueue 把空队列从注册集摘掉(懒清理,见 pruneQueueScript)。
// 剔除后显式把 queue_depth 归零(设计决策 D5c):此后该队列不再被扫到,
// gauge 若停在旧值会一直触发积压误报。
func pruneEmptyQueue(svcCtx *svc.ServiceContext, queueKey string, modeName string, configName string) {
	rankKey, err := rankKeyForQueue(queueKey)
	if err != nil {
		logx.Errorf("[matcher] 剔除空队列 %s 失败: %v", queueKey, err)
		return
	}
	removed, err := svcCtx.MatchRedis.Eval(pruneQueueScript, []string{matchQueueIndexKey, queueKey, rankKey}, queueKey)
	if err != nil {
		logx.Errorf("[matcher] 剔除空队列 %s 失败: %v", queueKey, err)
		return
	}
	if n, ok := removed.(int64); ok && n > 0 {
		setQueueDepthFn(modeName, configName, 0)
		logx.Infof("[matcher] 空队列已从注册集剔除 queue=%s", queueKey)
	}
}

// requiredPlayers 返回该队列凑满所需人数;未知模式/未配置返回 0。
// 必须与 JoinQueue 的 required 口径一致,否则队列永远凑不满或多弹人。
func requiredPlayers(svcCtx *svc.ServiceContext, mode int32, config uint32) uint32 {
	switch matchpb.MatchMode(mode) {
	case matchpb.MatchMode_MATCH_MODE_1V1:
		return 2
	case matchpb.MatchMode_MATCH_MODE_5V5:
		return required5v5Players
	case matchpb.MatchMode_MATCH_MODE_PVE_TEAM:
		required := svcCtx.Config.PveTeamSizeFor(config)
		// 队伍上限 5 收口(D14),与 JoinQueue 同口径。
		if required > kMaxBattleTeamSize {
			required = kMaxBattleTeamSize
		}
		return required
	default:
		// PVE_SOLO 不入队;3v3/切磋没有队列。
		return 0
	}
}

// queueEntry 是 queueSnapshotScript 读回的一个队列成员:等待序下标、评分镜像
// 里的 score(hasScore=false 表示镜像缺失,滚动升级窗口内旧实例只写 list)。
type queueEntry struct {
	raw      string
	playerId uint64 // 0 = 非法成员
	pos      int
	rating   float64
	hasScore bool
}

// loadQueueSnapshot 一次往返读队列前 limit 个成员及其评分(见 queueSnapshotScript)。
func loadQueueSnapshot(svcCtx *svc.ServiceContext, queueKey string, rankKey string, limit int) ([]queueEntry, error) {
	res, err := svcCtx.MatchRedis.Eval(queueSnapshotScript, []string{queueKey, rankKey}, strconv.Itoa(limit))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	raws, _ := res.([]any)
	entries := make([]queueEntry, 0, len(raws)/2)
	for i := 0; i+1 < len(raws); i += 2 {
		member, _ := raws[i].(string)
		score, _ := raws[i+1].(string)
		e := queueEntry{raw: member, pos: len(entries), hasScore: score != "", rating: parseRating(score)}
		if pid, err := strconv.ParseUint(member, 10, 64); err == nil {
			e.playerId = pid
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// groupPicker 是一次 popGroup 的工作集:队列快照 + 各成员的校验缓存
// (同一成员可能先后作为多个锚点的候选,票据只读一次)。
type groupPicker struct {
	svcCtx   *svc.ServiceContext
	queueKey string
	rankKey  string
	modeName string
	rated    bool
	entries  []queueEntry
	tickets  map[uint64]*queueTicket // 已校验通过的成员票据
	dropped  map[int]bool            // 已从队列剔除 / 判定无效的快照下标
}

// validate 校验一个队列成员并缓存结果:ticket 必须存在且处于 queued 态(取消
// 路径先删票再出队,票据缺失即认定已取消);battle:lock 存在的玩家出局并删票
// (排队期间通过切磋等入口进了别的战斗)。无效者当场从 list+ZSET 摘掉。
// 评分镜像缺失的成员按票据里的评分补写 ZADD(滚动升级窗口内旧实例只写 list)。
// 返回 (有效, err);err 非 nil 表示 Redis 抖动,调用方结束本轮,不做出局判定。
// 跨 zone:这里不看 zone,匹配池全局(设计决策 D1)。
func (p *groupPicker) validate(idx int) (bool, error) {
	e := &p.entries[idx]
	if p.dropped[idx] {
		return false, nil
	}
	if e.playerId == 0 {
		logx.Errorf("[matcher] 队列 %s 出现非法成员 %q,丢弃", p.queueKey, e.raw)
		p.drop(idx)
		return false, nil
	}
	if _, ok := p.tickets[e.playerId]; ok {
		return true, nil
	}
	ticket, err := loadTicket(p.svcCtx, e.playerId)
	if err != nil {
		return false, fmt.Errorf("读 ticket 失败 player=%d: %w", e.playerId, err)
	}
	if ticket == nil || ticket.State != ticketStateQueued {
		// 已取消 / 状态异常:摘掉队列项(取消路径已删票)。
		logx.Infof("[matcher] 丢弃无效队列成员 player=%d(ticket 缺失或状态异常)", e.playerId)
		p.drop(idx)
		return false, nil
	}
	locked, err := isPlayerBattleLocked(p.svcCtx, e.playerId)
	if err != nil {
		return false, fmt.Errorf("查战斗锁失败 player=%d: %w", e.playerId, err)
	}
	if locked {
		logx.Infof("[matcher] 队列成员已在战斗中,出局删票 player=%d", e.playerId)
		deleteTicket(p.svcCtx, e.playerId)
		p.drop(idx)
		return false, nil
	}
	if !e.hasScore {
		if _, err := p.svcCtx.MatchRedis.ZaddFloat(p.rankKey, ticket.Rating, e.raw); err != nil {
			return false, fmt.Errorf("补写评分镜像失败 player=%d: %w", e.playerId, err)
		}
		logx.Infof("[matcher] 队列成员缺评分镜像,按票据评分补写 player=%d rating=%s queue=%s",
			e.playerId, formatRating(ticket.Rating), p.queueKey)
		e.rating, e.hasScore = ticket.Rating, true
	}
	p.tickets[e.playerId] = ticket
	return true, nil
}

// drop 把快照里的一个成员从队列摘掉(list+ZSET 原子)并标记。
func (p *groupPicker) drop(idx int) {
	p.dropped[idx] = true
	e := p.entries[idx]
	if e.playerId == 0 {
		// 非法成员:ZSET 里不会有它,单独 LREM。
		if _, err := p.svcCtx.MatchRedis.Lrem(p.queueKey, 0, e.raw); err != nil {
			logx.Errorf("[matcher] 剔除非法成员 %q 失败: %v", e.raw, err)
		}
		return
	}
	if _, err := removeQueueMembers(p.svcCtx, p.queueKey, []uint64{e.playerId}); err != nil {
		logx.Errorf("[matcher] 剔除无效成员失败 player=%d: %v", e.playerId, err)
	}
}

// popGroup 按评分匹配从队列摘出一组(设计文档 §11),成功时同时返回各成员的
// ticket id 供后续票据写入做 CAS(D5)。算法:
//
//  1. 一条 Lua 读队列前缀(等待序)与评分镜像 score;
//  2. 锚点按等待序逐个尝试(队首优先):容差 tol = ratingToleranceFor(锚点已等秒数),
//     候选 = 前缀里评分落在 [anchor±tol] 的其他成员,按与锚点分差升序、同分按
//     等待先后取 required-1 个;PVE 组队不看评分(tol=∞),纯等待序;rated 模式
//     锚点已等 ≥ RatingToleranceMaxWaitSeconds 时也 tol=∞(终态兜底,不永久饥饿);
//  3. 票据 / battle:lock 校验在 Go 侧,无效者当场摘掉、继续找下一个候选;
//     候选不足 → 该锚点继续等(队列不动),换下一个锚点;曲线已饱和仍凑不到的
//     锚点记限频日志,本轮最久的饥饿等待上报 starved_anchor_wait_seconds;
//  4. 凑齐后一条 Lua 把锚点与候选从 list+ZSET 原子摘出;摘出人数不足
//     (校验与摘出之间被 CancelQueue 摘走)→ 已摘出者放回队首,本轮不弹。
//
// 校验失败 / 摘出发生在摘出之前的成员都不需要回滚;Redis 抖动直接结束本轮。
func popGroup(svcCtx *svc.ServiceContext, queueKey string, required uint32) ([]uint64, map[uint64]string, bool) {
	mode, config, err := parseQueueKey(queueKey)
	if err != nil {
		logx.Errorf("[matcher] 无法解析队列 key %q: %v", queueKey, err)
		return nil, nil, false
	}
	configName := strconv.FormatUint(uint64(config), 10)
	rankKey, err := rankKeyForQueue(queueKey)
	if err != nil {
		logx.Errorf("[matcher] 无法推出评分镜像 key %q: %v", queueKey, err)
		return nil, nil, false
	}
	entries, err := loadQueueSnapshot(svcCtx, queueKey, rankKey, queueScanLimit)
	if err != nil {
		logx.Errorf("[matcher] 读队列快照 %s 失败: %v", queueKey, err)
		return nil, nil, false
	}
	p := &groupPicker{
		svcCtx:   svcCtx,
		queueKey: queueKey,
		rankKey:  rankKey,
		modeName: matchpb.MatchMode(mode).String(),
		rated:    isRatedMode(matchpb.MatchMode(mode)),
		entries:  entries,
		tickets:  make(map[uint64]*queueTicket, required),
		dropped:  make(map[int]bool),
	}

	// 饥饿观测:本轮凑不到候选的锚点里等得最久的秒数(-1 = 没有);曲线饱和秒数
	// 之后仍凑不到的锚点记限频日志。
	starvedWait := int64(-1)
	saturation := ratingToleranceSaturationFor(svcCtx)
	attempts := 0
	for ai := range p.entries {
		if attempts >= maxAnchorAttempts {
			break
		}
		if p.dropped[ai] {
			continue
		}
		valid, err := p.validate(ai)
		if err != nil {
			logx.Errorf("[matcher] 校验锚点失败,结束本轮 queue=%s: %v", queueKey, err)
			return nil, nil, false
		}
		if !valid {
			continue
		}
		attempts++
		anchor := p.entries[ai]
		anchorTicket := p.tickets[anchor.playerId]
		waitSeconds := int64(0)
		if anchorTicket.EnqueuedAtMs > 0 {
			if now := nowMs(); now > anchorTicket.EnqueuedAtMs {
				waitSeconds = int64((now - anchorTicket.EnqueuedAtMs) / 1000)
			}
		}
		tol := anchorToleranceFor(svcCtx, p.rated, waitSeconds)

		members, tickets, ok := p.pickGroup(ai, required, tol)
		if !ok {
			// Redis 抖动:结束本轮。
			return nil, nil, false
		}
		if members == nil {
			// 候选不足:锚点继续等,换下一个锚点。饥饿可观测:记最久等待;曲线
			// 已饱和仍凑不到就告警(限频)。
			if waitSeconds > starvedWait {
				starvedWait = waitSeconds
			}
			if p.rated && waitSeconds >= saturation {
				warnStarvedAnchor(queueKey, anchor.playerId, waitSeconds, tol)
			}
			continue
		}
		removed, err := removeQueueMembers(svcCtx, queueKey, members)
		if err != nil {
			logx.Errorf("[matcher] 摘出成组失败 queue=%s members=%v: %v", queueKey, members, err)
			return nil, nil, false
		}
		if uint32(len(removed)) < required {
			// 校验与摘出之间有人被 CancelQueue 摘走:已摘出者按原相对顺序放回队首,
			// 下一轮再凑。
			logx.Infof("[matcher] 摘出时仅 %d/%d 人仍在队列,放回队首 queue=%s", len(removed), required, queueKey)
			for i := len(removed) - 1; i >= 0; i-- {
				pid := removed[i]
				if err := pushFrontRegistered(svcCtx, queueKey, strconv.FormatUint(pid, 10), p.tickets[pid].Rating); err != nil {
					logx.Errorf("[matcher] 放回队首失败 player=%d: %v", pid, err)
				}
			}
			return nil, nil, false
		}

		ratings := make([]string, 0, len(members))
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, pid := range members {
			r := p.tickets[pid].Rating
			ratings = append(ratings, formatRating(r))
			lo, hi = math.Min(lo, r), math.Max(hi, r)
		}
		metrics.ObserveMatchWait(p.modeName, float64(waitSeconds))
		if p.rated {
			metrics.ObserveGroupRatingSpread(p.modeName, hi-lo)
		}
		logx.Infof("[matcher] 成组 queue=%s anchor=%d wait=%ds tol=%.0f spread=%.0f members=%v ratings=%v",
			queueKey, anchor.playerId, waitSeconds, tol, hi-lo, members, ratings)
		return members, tickets, true
	}
	// 本轮没弹出组:上报饥饿 gauge(没有凑不到的锚点则归零)。
	if starvedWait < 0 {
		starvedWait = 0
	}
	setStarvedAnchorWaitFn(p.modeName, configName, float64(starvedWait))
	return nil, nil, false
}

// pickGroup 以快照下标 anchorIdx 为锚点挑 required-1 个候选并校验。返回
// (members, tickets, ok):ok=false 表示 Redis 抖动;members=nil 表示候选不足。
// 候选:锚点之后的快照成员(去重:同一玩家多份只取一份),评分在
// [anchor±tol] 内,rated 模式按 |分差| 升序、同分按等待先后;非 rated 纯等待序。
func (p *groupPicker) pickGroup(anchorIdx int, required uint32, tol float64) ([]uint64, map[uint64]string, bool) {
	anchor := p.entries[anchorIdx]
	anchorRating := p.tickets[anchor.playerId].Rating
	seen := map[uint64]bool{anchor.playerId: true}
	var cands []int
	for ci := range p.entries {
		if ci == anchorIdx || p.dropped[ci] {
			continue
		}
		c := p.entries[ci]
		if c.playerId == 0 {
			continue // 非法成员留给 validate 逐个剔除(作为锚点时)
		}
		if seen[c.playerId] {
			continue // 同一玩家两份(并发 JoinQueue 与自愈 / 回队首窄窗口叠加):组内只留一份
		}
		if p.rated && c.hasScore && math.Abs(c.rating-anchorRating) > tol {
			continue
		}
		seen[c.playerId] = true
		cands = append(cands, ci)
	}
	if p.rated {
		sort.SliceStable(cands, func(a, b int) bool {
			da := math.Abs(p.entries[cands[a]].rating - anchorRating)
			db := math.Abs(p.entries[cands[b]].rating - anchorRating)
			if da != db {
				return da < db
			}
			return p.entries[cands[a]].pos < p.entries[cands[b]].pos
		})
	}

	members := []uint64{anchor.playerId}
	for _, ci := range cands {
		if uint32(len(members)) >= required {
			break
		}
		valid, err := p.validate(ci)
		if err != nil {
			logx.Errorf("[matcher] 校验候选失败,结束本轮 queue=%s: %v", p.queueKey, err)
			return nil, nil, false
		}
		if !valid {
			continue
		}
		c := p.entries[ci]
		if p.rated && math.Abs(c.rating-anchorRating) > tol {
			// 镜像缺失、刚按票据补写评分的成员:补写后才知道分差,超容差就跳过。
			continue
		}
		members = append(members, c.playerId)
	}
	if uint32(len(members)) < required {
		return nil, nil, true
	}
	tickets := make(map[uint64]string, len(members))
	for _, pid := range members {
		tickets[pid] = p.tickets[pid].Ticket
	}
	return members, tickets, true
}

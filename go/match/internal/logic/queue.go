package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// ticket 状态机(QueueState 五态映射见 getqueuestatuslogic.go):
//
//	queued  -> 在 Redis 队列里等待凑单(QUEUE_STATE_QUEUED)
//	matched -> 已被 matcher 弹出,gather 管线执行中(QUEUE_STATE_MATCHED,短 TTL 自愈)
//	ready   -> CreateBattle 成功,等 battle 推 BattleStartS2C(QUEUE_STATE_READY,短 TTL 自清)
//
// ticket 不存在 = QUEUE_STATE_NOT_QUEUED。ENTERING 是场景类匹配预留的进场态,
// 回合制战斗(含二期 5v5)没有进场步骤,不产生该状态。
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
	ticketFieldZoneId     = "zone_id"
	ticketFieldQueueKey   = "queue_key"
	ticketFieldRating     = "rating"
)

// queueTicket 是 match:ticket:{player_id} 的内存镜像。
type queueTicket struct {
	Ticket       string
	Mode         int32
	Config       uint32
	State        string
	EnqueuedAtMs uint64
	// ZoneId 入队时刻玩家所在 zone(取自 player:{id}:location),只用于
	// 可观测性与对局日志 —— 匹配池全局不分 zone(设计决策 D1/D4)。
	ZoneId uint32
	// QueueKey 入队时写入的队列 key:取消 / 回队首直接用它,不再重算,
	// key 格式演进时旧票据仍能正确出队(设计决策 D4)。PVE_SOLO 不入队,为空。
	QueueKey string
	// Rating 入队时刻读到的评分(§11):回队首时按它写回评分镜像 ZSET,
	// 日志 / 对局记录也用它。旧票据没有该字段,按 defaultRating。
	Rating float64
}

// enqueueScript / requeueScript:注册集 SADD、队列 push 与评分镜像 ZADD 一条 Lua
// 原子完成(KEYS=[index, queue, rank],ARGV=[queueKey, playerId, rating];三 key
// 同 {mq} slot)。分步做会留下"队列存在但不在注册集"的窗口:matcher 的空队列
// 剔除若恰好落在中间,该队列就再也不会被扫到;list 与 ZSET 分步写则任一步
// 崩溃 / 故障切换都会让两者脱节。回队首用 LPUSH 保持原相对顺序,ZADD 写回
// 原评分(ZSET 不表达顺序,只表达评分)。
const (
	enqueueScript = `redis.call("SADD", KEYS[1], ARGV[1])
redis.call("ZADD", KEYS[3], ARGV[3], ARGV[2])
return redis.call("RPUSH", KEYS[2], ARGV[2])`
	requeueScript = `redis.call("SADD", KEYS[1], ARGV[1])
redis.call("ZADD", KEYS[3], ARGV[3], ARGV[2])
return redis.call("LPUSH", KEYS[2], ARGV[2])`
)

// removeQueueMembersScript 把一批成员从队列 list 与评分镜像 ZSET 原子摘出
// (KEYS=[queue, rank],ARGV=[member...]):逐个 LREM 0(同一玩家的重复项一并
// 清掉,组内只留一份)+ ZREM,返回**实际从 list 摘出**的成员列表 —— 校验与
// 摘出之间被 CancelQueue 摘走的人不在其中,调用方据此判断组是否凑齐。
// 取消出队 / 无效成员剔除 / 弹组摘出三处共用,两 key 同 {mq} slot。
const removeQueueMembersScript = `local removed = {}
for i = 1, #ARGV do
	if redis.call("LREM", KEYS[1], 0, ARGV[i]) > 0 then
		removed[#removed + 1] = ARGV[i]
	end
	redis.call("ZREM", KEYS[2], ARGV[i])
end
return removed`

// queueSnapshotScript 读队列前 N 个成员及其在评分镜像里的 score
// (KEYS=[queue, rank],ARGV=[limit]),返回扁平数组 [member, score, member, score...],
// 镜像里没有的成员 score 为空串(滚动升级窗口内旧实例只写 list 不写 ZSET,
// matcher 据此补写)。一次往返拿到等待序与评分,弹组选人在 Go 侧完成。
const queueSnapshotScript = `local members = redis.call("LRANGE", KEYS[1], 0, tonumber(ARGV[1]) - 1)
local out = {}
for i, m in ipairs(members) do
	out[#out + 1] = m
	local s = redis.call("ZSCORE", KEYS[2], m)
	if s == false then s = "" end
	out[#out + 1] = s
end
return out`

// ticketCasScript 票据状态的带 ticket id 的 CAS 写(设计决策 D5):
// KEYS=[ticketKey],ARGV=[expectedTicketId, field, value, ttlSeconds(0=不改), (field, value)...]。
// 只有 hash 里的 ticket 字段等于 expected 才 HSET(+EXPIRE),返回 1;票据已过期
// (HGET 返回 nil)或已被玩家重排替换成新 ticket 时返回 0,什么都不写。
// matched 态短 TTL 让崩溃实例的票据自灭、玩家可重排;若旧实例其实没死只是
// gather 拖得久,它迟到的 matched / ready / 回队首写入不能盖到新票据上。
// ARGV[5..] 是可选的额外 (field, value) 对,供 ready 同时写 state 与 battle_id。
const ticketCasScript = `if redis.call("HGET", KEYS[1], "` + ticketFieldTicket + `") ~= ARGV[1] then
	return 0
end
redis.call("HSET", KEYS[1], ARGV[2], ARGV[3])
for i = 5, #ARGV, 2 do
	redis.call("HSET", KEYS[1], ARGV[i], ARGV[i + 1])
end
if tonumber(ARGV[4]) > 0 then
	redis.call("EXPIRE", KEYS[1], ARGV[4])
end
return 1`

// ticketDelCasScript 带 ticket id 的删票(KEYS=[ticketKey],ARGV=[expectedTicketId]):
// gather 失败路径删肇事者票据时用,同样不能误删玩家重排后的新票据。
const ticketDelCasScript = `if redis.call("HGET", KEYS[1], "` + ticketFieldTicket + `") ~= ARGV[1] then
	return 0
end
return redis.call("DEL", KEYS[1])`

// ticketCancelScript 取消排队的删票(KEYS=[ticketKey],ARGV=[expectedTicketId]):
// ticket id 一致 **且 state 仍是 queued** 才 DEL,返回 1;否则返回 0 什么都不写。
// "matched 之后取消太迟"必须在存储层成立:CancelQueue 的 loadTicket 与 DEL 之间,
// matcher 可能已把该玩家弹出并推进 matched,若此时无条件 DEL,玩家会在"取消成功"
// 后被一场他已退出的战斗冻结(setTicketMatched 的 CAS 失败只是日志)。
const ticketCancelScript = `if redis.call("HGET", KEYS[1], "` + ticketFieldTicket + `") ~= ARGV[1] then
	return 0
end
if redis.call("HGET", KEYS[1], "` + ticketFieldState + `") ~= "` + ticketStateQueued + `" then
	return 0
end
return redis.call("DEL", KEYS[1])`

// ticketCreateScript "不存在才创建"的票据写入(KEYS=[ticketKey],
// ARGV=[ttlSeconds, field, value, (field, value)...]):EXISTS 为 1 直接返回 0,
// 否则 HSET 全字段 + EXPIRE 返回 1。JoinQueue 的"读票判存在"与"写票"合成一条
// 原子命令 —— 同一玩家的两条 JoinQueue 落到两个实例时,分开做会各写一张票、
// 各 RPUSH 一次,队列里出现两份,弹组后第二次 PrepareBattle 被 scene 拒,整组
// 白冻结一轮。单 key,集群安全。
const ticketCreateScript = `if redis.call("EXISTS", KEYS[1]) == 1 then
	return 0
end
for i = 2, #ARGV, 2 do
	redis.call("HSET", KEYS[1], ARGV[i], ARGV[i + 1])
end
redis.call("EXPIRE", KEYS[1], ARGV[1])
return 1`

// queueContainsScript 队列成员存在性探测(KEYS=[queueKey],ARGV=[playerId]):
// LPOS(Redis ≥ 6.0.6;部署为 7.2)单 key O(N),队列很短。JoinQueue 用它识别
// "票据 queued 但队列里没有人"的崩溃残留(见 isQueuedTicketInQueue)。
const queueContainsScript = `if redis.call("LPOS", KEYS[1], ARGV[1]) == false then
	return 0
end
return 1`

// ticketSetQueueKeyScript 给已存在的票据补 queue_key(KEYS=[ticketKey],
// ARGV=[queueKey]):票据不存在时不写,避免 HSET 凭空造出一个没有 TTL 的 hash。
// 旧队列迁移(migrateLegacyQueues)用。
const ticketSetQueueKeyScript = `if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end
return redis.call("HSET", KEYS[1], "` + ticketFieldQueueKey + `", ARGV[1])`

// 测试注入点(生产恒为 nil):把并发时序里的"另一实例"插进两步之间,证明
// 调换顺序后不再产生孤儿票据。照 runGatherFn / setQueueDepthFn 的包级变量模式。
//
//	requeueFrontHook  requeueFront 已把票据 CAS 回 queued、尚未 LPUSH 时调用;
//	matcherAfterPopHook  popGroup 弹出成组、尚未 setTicketMatched 时调用;
//	beforeAcquireWatchingHook  WatchBattle 已过入口互斥检查、尚未 SETNX 抢观战标记时调用
//	  (那条 ErrAlreadyWatching 出口只在真并发下可达,靠这个钩子做成确定性用例)。
var (
	requeueFrontHook          func(playerId uint64)
	matcherAfterPopHook       func(members []uint64)
	beforeAcquireWatchingHook func(playerId uint64)
)

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

// isPlayerBattleLocked 咨询性检查 battle:lock:{player_id}(scene 写,契约 key
// 走 SharedRedis)。权威判定仍在 scene 的 InBattleComp;这里只是提前挡明显
// 不合法的请求。Redis 出错时按"有锁"处理(fail-closed:宁可拒绝排队,不可放进双战斗)。
func isPlayerBattleLocked(svcCtx *svc.ServiceContext, playerId uint64) (bool, error) {
	ok, err := svcCtx.SharedRedis.Exists(battleLockKey(playerId))
	if err != nil {
		return true, fmt.Errorf("查询 battle:lock 失败: %w", err)
	}
	return ok, nil
}

// loadTicket 读取玩家 ticket;不存在返回 (nil, nil)。
func loadTicket(svcCtx *svc.ServiceContext, playerId uint64) (*queueTicket, error) {
	fields, err := svcCtx.MatchRedis.Hgetall(matchTicketKey(playerId))
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
	zoneId, _ := strconv.ParseUint(fields[ticketFieldZoneId], 10, 32)
	return &queueTicket{
		Ticket:       fields[ticketFieldTicket],
		Mode:         int32(mode),
		Config:       uint32(config),
		State:        fields[ticketFieldState],
		EnqueuedAtMs: enqueuedAt,
		ZoneId:       uint32(zoneId),
		QueueKey:     fields[ticketFieldQueueKey],
		Rating:       parseRating(fields[ticketFieldRating]),
	}, nil
}

// createTicketIfAbsent 不存在才创建 ticket hash 并挂 TTL(见 ticketCreateScript)。
// 返回 (创建了, err);false 表示已有票据(并发 JoinQueue 的后来者),调用方按
// ErrAlreadyQueued 处理。ttl 由调用方给:排队用长 TTL(只挡进程崩溃等异常残留,
// 正常路径由取消/开战收尾),PVE_SOLO 直接给 matched 短 TTL。
func createTicketIfAbsent(svcCtx *svc.ServiceContext, playerId uint64, t *queueTicket, ttl int) (bool, error) {
	res, err := svcCtx.MatchRedis.Eval(ticketCreateScript, []string{matchTicketKey(playerId)},
		strconv.Itoa(ttl),
		ticketFieldTicket, t.Ticket,
		ticketFieldMode, strconv.FormatInt(int64(t.Mode), 10),
		ticketFieldConfig, strconv.FormatUint(uint64(t.Config), 10),
		ticketFieldState, t.State,
		ticketFieldEnqueuedAt, strconv.FormatUint(t.EnqueuedAtMs, 10),
		ticketFieldZoneId, strconv.FormatUint(uint64(t.ZoneId), 10),
		ticketFieldQueueKey, t.QueueKey,
		ticketFieldRating, formatRating(t.Rating),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.(int64)
	return n == 1, nil
}

// ticketQueueKeys 返回该票据的成员可能所在的队列 key:有 queue_key 就只看它;
// 旧票据(无该字段)新旧两种格式都看(见 keys.go legacyMatchQueueKey)。
func ticketQueueKeys(t *queueTicket) []string {
	if t.QueueKey != "" {
		return []string{t.QueueKey}
	}
	return []string{matchQueueKey(t.Mode, t.Config), legacyMatchQueueKey(t.Mode, t.Config)}
}

// isQueuedTicketInQueue 探测 queued 态票据的玩家是否真的在队列里(见 queueContainsScript)。
// 票据与队列分属不同 slot/master,没有跨 slot 原子性:集群故障切换丢掉队列里的
// RPUSH、实例在 popGroup 弹出与 setTicketMatched 之间崩溃、requeueFront 在 CAS 与
// LPUSH 之间崩溃,都会留下"票据 queued、队列无人"的残留 —— matcher 永远弹不到他,
// JoinQueue 一直 ErrAlreadyQueued,直到 6h 过期。JoinQueue 据此自愈。
func isQueuedTicketInQueue(svcCtx *svc.ServiceContext, playerId uint64, t *queueTicket) (bool, error) {
	member := strconv.FormatUint(playerId, 10)
	for _, key := range ticketQueueKeys(t) {
		res, err := svcCtx.MatchRedis.Eval(queueContainsScript, []string{key}, member)
		if err != nil {
			return false, err
		}
		if n, _ := res.(int64); n == 1 {
			return true, nil
		}
	}
	return false, nil
}

// cancelTicketIfQueued 取消排队的删票:ticket id 一致且仍是 queued 才删
// (见 ticketCancelScript)。返回 (删了, err)。
func cancelTicketIfQueued(svcCtx *svc.ServiceContext, playerId uint64, expectedTicket string) (bool, error) {
	res, err := svcCtx.MatchRedis.Eval(ticketCancelScript, []string{matchTicketKey(playerId)}, expectedTicket)
	if err != nil {
		return false, err
	}
	n, _ := res.(int64)
	return n == 1, nil
}

// ticketTTLSeconds 带缺省值的长 TTL 配置读取
// (直接构造 Config 的测试与漏配 yaml 都不会得到 0 TTL);matched 态见 matchedTicketTTLFor。
func ticketTTLSeconds(svcCtx *svc.ServiceContext) int {
	if ttl := int(svcCtx.Config.TicketTTLSeconds); ttl > 0 {
		return ttl
	}
	return 21600
}

// matchedTicketTTLFor 按组大小算 matched 态 TTL(设计决策 D5):
//
//	max(MatchedTicketTTLSeconds,
//	    required × (removeObserverTimeout + prepareBattleTimeout)
//	    + createBattleTimeout + rollbackTimeout + 10s)
//
// 覆盖 gather 从弹组到"成功写 ready"或"进入补偿"之前的最坏链路:每人先观战
// 清退(RemoveObserver 阻塞 RPC,spectate.go)再串行 PrepareBattle,全齐后
// CreateBattle;CreateBattle 失败还要先 DestroyBattle(rollbackTimeout)才进 fail。
// 5v5 十人 = 10×6+5+3+10 = 78s;只算 prepare+create 的旧公式(45s)会让票据在
// 仍在跑的 gather 途中过期(复审 2026-09-02)。补偿路径(逐人 CancelBattlePrepare)
// 不计入这里:fail 闭包进入补偿前先用 CAS 把幸存者票据续期
// (extendMatchedTickets / compensationTicketTTLFor),TTL 不必一次覆盖全部。
// 配置值只是下限,组越大窗口越长。常量与 gather.go / spectate.go 同源,不另写数字。
func matchedTicketTTLFor(svcCtx *svc.ServiceContext, required uint32) int {
	ttl := int(svcCtx.Config.MatchedTicketTTLSeconds)
	if ttl <= 0 {
		ttl = 30
	}
	perMember := int((removeObserverTimeout + prepareBattleTimeout) / time.Second)
	worst := int(required)*perMember + int((createBattleTimeout+rollbackTimeout)/time.Second) + 10
	if worst > ttl {
		return worst
	}
	return ttl
}

// compensationTicketTTLFor 补偿路径的票据续期窗口:已冻结者逐人
// CancelBattlePrepare(每人最长 rollbackTimeout)+ 回队首余量 10s。
func compensationTicketTTLFor(prepared int) int {
	return prepared*int(rollbackTimeout/time.Second) + 10
}

// extendMatchedTickets 进入补偿前给幸存者的 matched 票据续期(CAS,ticket id 不一致
// 或已过期不写)。不续期的话 10 人组的 destroy + 逐人解冻(≈33s)会让票据在
// requeueFront 之前过期,requeueFront 逐人 loadTicket 得 nil → 十个人全部从队列
// 消失且没有票据。返回续期成功的人数。
func extendMatchedTickets(svcCtx *svc.ServiceContext, members []uint64, tickets map[uint64]string, ttl int) int {
	extended := 0
	for _, playerId := range members {
		expected, known := tickets[playerId]
		if !known {
			continue
		}
		written, err := casTicketFields(svcCtx, playerId, expected, ttl, ticketFieldState, ticketStateMatched)
		if err != nil {
			logx.Errorf("[match] 补偿前续期 ticket 失败 player=%d: %v", playerId, err)
			continue
		}
		if !written {
			logx.Infof("[match] ticket 已被替换或过期,跳过补偿续期 player=%d expected=%s", playerId, expected)
			continue
		}
		extended++
	}
	return extended
}

// casTicketFields 带 ticket id 的票据字段 CAS 写(见 ticketCasScript):
// fieldValues 形如 field, value[, field, value...],ttl>0 时一并 EXPIRE。
// 返回 (写入了, err);false 表示票据已过期或已被玩家重排替换,调用方只记 Info 不覆盖。
func casTicketFields(svcCtx *svc.ServiceContext, playerId uint64, expectedTicket string,
	ttl int, fieldValues ...string,
) (bool, error) {
	if len(fieldValues) < 2 || len(fieldValues)%2 != 0 {
		return false, fmt.Errorf("casTicketFields 参数必须是 (field, value) 对,实得 %d 个", len(fieldValues))
	}
	args := make([]any, 0, 2+len(fieldValues))
	args = append(args, expectedTicket, fieldValues[0], fieldValues[1], strconv.Itoa(ttl))
	for _, v := range fieldValues[2:] {
		args = append(args, v)
	}
	res, err := svcCtx.MatchRedis.Eval(ticketCasScript, []string{matchTicketKey(playerId)}, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.(int64)
	return n == 1, nil
}

// enqueueAtomic 把玩家挂到队尾、写评分镜像并把队列 key 登记进注册集
// (一条 Lua,见 enqueueScript)。rating 由调用方在 Go 侧读出传入:评分 key
// 按玩家分布,与队列不同 slot,Lua 里不能 GET。
func enqueueAtomic(svcCtx *svc.ServiceContext, queueKey string, playerId uint64, rating float64) error {
	rankKey, err := rankKeyForQueue(queueKey)
	if err != nil {
		return err
	}
	_, err = svcCtx.MatchRedis.Eval(enqueueScript, []string{matchQueueIndexKey, queueKey, rankKey},
		queueKey, strconv.FormatUint(playerId, 10), formatRating(rating))
	return err
}

// pushFrontRegistered 把一个原始队列元素放回队首、按原评分写回镜像,并保证
// 队列在注册集里(凑不满回队首 / 旧队列迁移共用;队列弹空后 key 会消失,
// 此刻别的实例可能已把它从注册集剔除,单独 LPUSH 会造出孤儿队列)。
func pushFrontRegistered(svcCtx *svc.ServiceContext, queueKey string, raw string, rating float64) error {
	rankKey, err := rankKeyForQueue(queueKey)
	if err != nil {
		return err
	}
	_, err = svcCtx.MatchRedis.Eval(requeueScript, []string{matchQueueIndexKey, queueKey, rankKey},
		queueKey, raw, formatRating(rating))
	return err
}

// removeQueueMembers 把一批成员从队列 list 与评分镜像 ZSET 原子摘出(见
// removeQueueMembersScript),返回实际从 list 摘出的成员。
func removeQueueMembers(svcCtx *svc.ServiceContext, queueKey string, members []uint64) ([]uint64, error) {
	rankKey, err := rankKeyForQueue(queueKey)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(members))
	for _, pid := range members {
		args = append(args, strconv.FormatUint(pid, 10))
	}
	res, err := svcCtx.MatchRedis.Eval(removeQueueMembersScript, []string{queueKey, rankKey}, args...)
	if err != nil {
		return nil, err
	}
	raws, _ := res.([]any)
	removed := make([]uint64, 0, len(raws))
	for _, raw := range raws {
		s, _ := raw.(string)
		pid, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			continue
		}
		removed = append(removed, pid)
	}
	return removed, nil
}

// dequeueMember 取消 / 剔除时把单个玩家从队列摘出。新格式 key(带 hash tag,
// 有评分镜像)走 list+ZSET 原子 Lua;旧格式 key(`match:queue:<mode>:<config>`,
// 无 tag、从未有过镜像)只做单 key LREM —— 旧 key 与按它推出的 rank key 不同
// slot,集群下不能放进同一条 Lua。
func dequeueMember(svcCtx *svc.ServiceContext, queueKey string, playerId uint64) error {
	if !strings.Contains(queueKey, matchQueueHashTag) {
		_, err := svcCtx.MatchRedis.Lrem(queueKey, 0, strconv.FormatUint(playerId, 10))
		return err
	}
	_, err := removeQueueMembers(svcCtx, queueKey, []uint64{playerId})
	return err
}

// setTicketMatched 把 ticket 推进 matched 态并收紧到短 TTL(设计决策 D5):
// 弹组实例若在 gather 中崩溃,票据在 ttl 秒后自灭,玩家可重排;ttl 由调用方按
// 组大小算(matchedTicketTTLFor)。带 expectedTicket 的 CAS 写:弹出时读到的
// ticket id 与当前不一致(已过期被重排)则不写,返回 false。
// 正常路径由 markTicketReady(成功)或 requeueFront / deleteTicket(失败)接管。
func setTicketMatched(svcCtx *svc.ServiceContext, playerId uint64, expectedTicket string, ttl int) (bool, error) {
	return casTicketFields(svcCtx, playerId, expectedTicket, ttl, ticketFieldState, ticketStateMatched)
}

// markTicketReady 把 ticket 置为 ready 并写 battle_id、收紧 TTL:短暂供 GetQueueStatus
// 查询后自清,战斗内状态改由 battle:lock / InBattleComp 表达。CAS 失败(票据已被
// 玩家重排替换或已过期)只记 Info,不覆盖新票据。
func markTicketReady(svcCtx *svc.ServiceContext, playerId uint64, expectedTicket string, battleId uint64) {
	ttl := int(svcCtx.Config.ReadyTicketTTLSeconds)
	if ttl <= 0 {
		ttl = 60
	}
	written, err := casTicketFields(svcCtx, playerId, expectedTicket, ttl,
		ticketFieldState, ticketStateReady,
		ticketFieldBattleId, strconv.FormatUint(battleId, 10))
	if err != nil {
		logx.Errorf("[match] 更新 ticket ready 失败 player=%d battle=%d: %v", playerId, battleId, err)
		return
	}
	if !written {
		logx.Infof("[match] ticket 已被替换或过期,跳过 ready 写入 player=%d battle=%d expected=%s",
			playerId, battleId, expectedTicket)
	}
}

// deleteTicket 删除 ticket(取消 / matcher 弹出时发现已在战斗中的出局者)。
func deleteTicket(svcCtx *svc.ServiceContext, playerId uint64) {
	if _, err := svcCtx.MatchRedis.Del(matchTicketKey(playerId)); err != nil {
		logx.Errorf("[match] 删除 ticket 失败 player=%d: %v", playerId, err)
	}
}

// deleteTicketIfOwned 带 ticket id 的删票(gather 失败路径的出局者):票据已被
// 玩家重排替换时不动新票据,只记 Info(与 CAS 写同口径,见 ticketDelCasScript)。
func deleteTicketIfOwned(svcCtx *svc.ServiceContext, playerId uint64, expectedTicket string) {
	res, err := svcCtx.MatchRedis.Eval(ticketDelCasScript, []string{matchTicketKey(playerId)}, expectedTicket)
	if err != nil {
		logx.Errorf("[match] CAS 删除 ticket 失败 player=%d: %v", playerId, err)
		return
	}
	if n, _ := res.(int64); n == 0 {
		logx.Infof("[match] ticket 已被替换或过期,跳过删票 player=%d expected=%s", playerId, expectedTicket)
	}
}

// requeueFront 把成员按原相对顺序放回队首(补偿矩阵:组队场景失败成员回队首),
// 票据恢复 queued 态并恢复长 TTL(matched 态收紧过)。tickets 是弹组时读到的
// ticket id:当前票据与之不一致(matched TTL 到期后玩家已重排,或票据已过期)
// 就整个人跳过 —— 既不写票据也不入队,否则会把已重排的玩家在队列里塞成两份。
// 队列 key 优先用票据里的 QueueKey,缺失(旧票据)才用 fallbackQueueKey。
// 逐个从末尾往前 LPUSH,最先弹出的成员最终仍在最前。
//
// 顺序必须是 **先 CAS 回 queued、再 LPUSH**:gather 不持凑单锁,反过来做的话
// 别的实例的 matcher 可以在 LPUSH 与 CAS 之间 LPOP 到这个人,读到 state=matched
// 而静默丢弃(popGroup 的状态校验),随后本实例的 CAS 再把他改成 queued + 6h TTL
// —— 票据 queued、队列无人,玩家被 ErrAlreadyQueued 卡到自己取消。先 CAS 后 LPUSH
// 则窗口内被弹出时票据已是 queued,会被正常凑组;LPUSH 失败再用 CAS 删票,不留
// 反向孤儿(玩家可立即重排)。CAS 与 LPUSH 之间本实例崩溃的残留由 JoinQueue 的
// isQueuedTicketInQueue 自愈。
func requeueFront(svcCtx *svc.ServiceContext, fallbackQueueKey string, members []uint64, tickets map[uint64]string) {
	requeued := 0
	for i := len(members) - 1; i >= 0; i-- {
		playerId := members[i]
		expected, known := tickets[playerId]
		if !known {
			logx.Errorf("[match] 回队首缺少弹组时的 ticket id,跳过 player=%d", playerId)
			continue
		}
		ticket, err := loadTicket(svcCtx, playerId)
		if err != nil {
			logx.Errorf("[match] 回队首前读 ticket 失败,跳过 player=%d: %v", playerId, err)
			continue
		}
		if ticket == nil || ticket.Ticket != expected {
			logx.Infof("[match] ticket 已被替换或过期,不回队首 player=%d expected=%s", playerId, expected)
			continue
		}
		queueKey := fallbackQueueKey
		if ticket.QueueKey != "" {
			queueKey = ticket.QueueKey
		}
		written, err := casTicketFields(svcCtx, playerId, expected, ticketTTLSeconds(svcCtx),
			ticketFieldState, ticketStateQueued)
		if err != nil {
			logx.Errorf("[match] 回队首前恢复 ticket 状态失败,跳过 player=%d: %v", playerId, err)
			continue
		}
		if !written {
			// 读票与 CAS 之间被替换的极窄窗口:不入队,新票据自己已在队列里。
			logx.Infof("[match] 回队首前 ticket 已被替换,不入队 player=%d expected=%s", playerId, expected)
			continue
		}
		if requeueFrontHook != nil {
			requeueFrontHook(playerId)
		}
		if err := pushFrontRegistered(svcCtx, queueKey, strconv.FormatUint(playerId, 10), ticket.Rating); err != nil {
			// 票据已是 queued 但人不在队列:删票让玩家可立即重排,不留孤儿。
			logx.Errorf("[match] 回队首失败,删票放玩家重排 player=%d queue=%s: %v", playerId, queueKey, err)
			deleteTicketIfOwned(svcCtx, playerId, expected)
			continue
		}
		requeued++
	}
	logx.Infof("[match] %d/%d 名成员已回队首 queue=%s", requeued, len(members), fallbackQueueKey)
}

// nowMs 返回当前 Unix 毫秒。
func nowMs() uint64 {
	return uint64(time.Now().UnixMilli())
}

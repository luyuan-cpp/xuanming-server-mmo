package data

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"

	"friend/internal/metrics"
	plpb "proto/player_locator"
)

// session_reader.go —— 好友在线状态的唯一读法(F2 §3.9)。
//
// # 为什么不再有 friend:online
//
// friend 曾经自己维护 `friend:online:<id>`(60s TTL),写者是 proto 里的 NotifyOnline /
// NotifyOffline 两个 RPC。那两个 RPC 已在 F1 随 proto 删除,于是那套 key **只剩读者没有写者** ——
// 读出来恒为"全员离线",而且一个错都不报。本批把它整套删掉,改读 player_locator 维护的
// 跨运行时契约 key。
//
// # 契约约束(契约 §4,一条都不能破)
//
//   - key 形状 `player:session:<id>`,值是 proto 编码的 plpb.PlayerSession,**写者是 player_locator**。
//     friend 只读,不写、不删、不设 TTL。
//   - 只能经 **SharedRedis** 读。FriendRedis 是 friend 的私有库(可以是另一套实例、甚至 Cluster),
//     那里根本没有这个 key。
//   - 这个 key **不许加 friend 自己的 hash tag 或版本前缀**:它的拼法是跨运行时约定,
//     改一个字符就读不到(而且是静默读不到 —— 看起来只是"所有人都离线")。
//     正因为没有 hash tag,一次 MGET 的多个 key 会散落在不同 slot,所以共享库禁 Cluster;
//     这也是 SharedRedis 必须保持 node 形态的又一条理由。
//     (internal/logic/push.go 有同一个 key 的单条读法,两处刻意各写一份短 fmt.Sprintf,
//     而不是抽一个跨包常量 —— 抽了反而会让人以为 friend 拥有这个 key 的定义权。)
//
// # 失败语义:降级为"全部离线",不让好友列表整个失败
//
// 在线状态是**展示字段**。共享 Redis 抖一下就让 GetFriendList 整体报错,比返回一份
// "全部灰着"的列表更糟(玩家会以为好友系统坏了)。所以本文件内部把所有读失败都吃掉:
// 计 metrics.ObserveOnlineLookup(OutcomeError) + 打**限流**日志,并让对应玩家在结果里缺席
// (缺席 == 调用方读 map 拿到零值 == 离线)。包内保留 BatchOnlineStatus 的 err 供将来的
// 调用方使用;logic 走的是 FillOnlineStatus,err 在那里被吃掉,理由见该方法。
// 无论走哪个入口,调用方都**不要**再计指标(会双计)。

// playerSessionKeyPrefix 见文件头的契约说明:写者是 player_locator,friend 只读。
const playerSessionKeyPrefix = "player:session:"

// defaultSessionBatchSize 是 batchSize 传 0 时的兜底分批大小。
// 生产不会走到(config.Validate 拒收 ListReadHardLimit=0),但单测可能手工构造 0 ——
// 那时用 0 去切片会得到"永远切不完"的死循环,比任何降级都糟。
const defaultSessionBatchSize = 256

// onlineLookupLogInterval 是读失败日志的最小间隔。
//
// 共享 Redis 不可用时,每次 GetFriendList 都会失败一次,而拉好友列表是**每次登录都发生**的
// 高频操作 —— 不限流的话日志会以在线人数的速率刷屏,把真正有用的错误冲掉(Loki 那边还要付
// 存储代价)。限流只影响日志,指标 friend_online_lookup_total{outcome="error"} 一条不落,
// 告警接在指标上,不接在日志上。
const onlineLookupLogInterval = 10 * time.Second

// OnlineStatus 是一名玩家的在线展示态。
//
// LastActiveMs 直接取 PlayerSession.last_active_ts(两边都是 int64,不需要转换;
// 而 friend 表里的 since_ms 是 uint64、wire proto 里是 int64,那处转换在 friend_repo.go)。
// 刻意不带 gate / scene 等字段:那些是路由信息,好友列表不需要,带出来等于扩大暴露面。
type OnlineStatus struct {
	Online       bool
	LastActiveMs int64
}

// SessionReader 批量读 player:session:<id>。无状态(只持有句柄 + 一个日志限流时间戳),
// 可被多个请求并发使用。
type SessionReader struct {
	rdb *redis.Redis
	// batchSize 是一次 MGET 的 key 数上限,取 Friend.ListReadHardLimit:
	// 与列表读同源,因为它本来就是"一次请求最多处理多少个好友"的那个数。
	// 有上限是硬要求 —— 一次 MGET 发几千个 key 会做出一个巨大的请求包,
	// 并且在 Redis 单线程上形成一次长阻塞,拖慢同实例上所有别的运行时。
	batchSize int
	// lastLogUnixNano 是限流日志的上次打印时刻(见 onlineLookupLogInterval)。
	// 用 atomic 而不是 mutex:它只是一个"要不要打这条日志"的近似判断,
	// 并发下多打一条无害,少打一条也无害,不值得为它引入锁竞争。
	lastLogUnixNano atomic.Int64
}

// NewSessionReader 构造。sharedRdb **必须**是 svcCtx.SharedRedis(见文件头契约)。
func NewSessionReader(sharedRdb *redis.Redis, batchSize uint32) *SessionReader {
	size := int(batchSize)
	if size <= 0 {
		size = defaultSessionBatchSize
	}
	return &SessionReader{rdb: sharedRdb, batchSize: size}
}

// BatchOnlineStatus 返回这批玩家的在线态。
//
// 返回的 map **只包含读到会话的玩家**:没有会话(没登录)、会话解不开、或者 Redis 故障的玩家
// 都不出现在结果里。调用方按"缺席 = 离线"用即可(Go 里读 nil map 与缺 key 都得到零值)。
// err 非 nil 表示至少有一批 MGET 失败;此时 map 里仍可能有成功批次的结果,
// 调用方可以直接丢掉(照 logic 的 onlineStates)也可以用,两种都安全。
func (s *SessionReader) BatchOnlineStatus(ctx context.Context, playerIDs []uint64) (map[uint64]OnlineStatus, error) {
	if s == nil || s.rdb == nil || len(playerIDs) == 0 {
		return nil, nil
	}

	// 去重:好友列表里不会有重复 id,但推荐候选 / 调用方拼出来的批次可能有。
	// 重复 id 会让同一个 key 在一次 MGET 里出现两次,既浪费带宽也让指标偏高。
	ids := make([]uint64, 0, len(playerIDs))
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, id := range playerIDs {
		if id == 0 {
			continue // 0 不是合法 player_id,拼出来的 key 谁也写不了
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	states := make(map[uint64]OnlineStatus, len(ids))
	var firstErr error
	for start := 0; start < len(ids); start += s.batchSize {
		end := start + s.batchSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		if err := s.readChunk(ctx, chunk, states); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// 不 break:后面的批次可能是好的,能拿到多少在线状态就拿多少。
			// 真正的连接级故障下后续批次也会失败,代价只是多几次已经失败的往返 ——
			// 而 ctx 上有整请求预算(F2-13),不会无界拖下去。
		}
	}
	return states, firstErr
}

// FillOnlineStatus 是 logic 侧 SessionStore 的实现:单返回值,读失败在内部降级。
//
// err 被刻意吃掉:本文件所有失败路径都已就地计 metrics.ObserveOnlineLookup(OutcomeError)
// 并打过限流日志(见文件头「失败语义」),调用方再接一次 err 只会重复计指标;
// 而在线态是展示字段,"缺席 = 离线"就是调用方需要的最终答案,没有需要它决策的失败分支。
func (s *SessionReader) FillOnlineStatus(ctx context.Context, playerIDs []uint64) map[uint64]OnlineStatus {
	states, _ := s.BatchOnlineStatus(ctx, playerIDs)
	return states
}

// readChunk 读一批 key 并把结果填进 states。err 非 nil 只表示"这一批没读到",
// 不影响别的批次;本批涉及的玩家一律计 OutcomeError 并当作离线。
func (s *SessionReader) readChunk(ctx context.Context, ids []uint64, states map[uint64]OnlineStatus) error {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = fmt.Sprintf("%s%d", playerSessionKeyPrefix, id)
	}

	// go-zero 的 MgetCtx 把 go-redis 的 []any 结果统一成 []string:**未命中的 key 变成空串**
	// (core/stores/redis/redis.go 的 toStrings),不会像单 key 的 GetCtx 那样需要另外判 redis.Nil。
	// 所以下面只需要"空串 = 没有会话";而真故障一定从 err 出来。
	values, err := s.rdb.MgetCtx(ctx, keys...)
	if err != nil {
		for range ids {
			metrics.ObserveOnlineLookup(metrics.OutcomeError)
		}
		s.logLookupFailure(ctx, len(ids), err)
		return fmt.Errorf("mget player sessions (%d keys): %w", len(ids), err)
	}
	// 理论上 MGET 的返回条数恒等于入参条数;真不等就说明客户端行为与假设不符,
	// 此时按下标取值会错位(把 A 的在线状态贴到 B 身上)—— 那比"全部离线"严重得多,
	// 所以宁可整批当失败。
	if len(values) != len(ids) {
		for range ids {
			metrics.ObserveOnlineLookup(metrics.OutcomeError)
		}
		s.logLookupFailure(ctx, len(ids),
			fmt.Errorf("mget 返回 %d 条,期望 %d 条", len(values), len(ids)))
		return fmt.Errorf("mget player sessions: got %d values for %d keys", len(values), len(ids))
	}

	for i, raw := range values {
		playerID := ids[i]
		if raw == "" {
			// 没有会话 = 没登录(player_locator 正常删除,或从未登录过)。这是**最常见**的分支,
			// 不是错误。
			metrics.ObserveOnlineLookup(metrics.OutcomeOffline)
			continue
		}
		session := &plpb.PlayerSession{}
		if err := proto.Unmarshal([]byte(raw), session); err != nil {
			// 解不开是真问题(写者换了格式 / key 被别人覆盖),但**不能**让一条坏值毁掉整张列表:
			// 计 error、当离线、继续。它会在指标上持续冒头,而不是变成一次偶发的 500。
			metrics.ObserveOnlineLookup(metrics.OutcomeError)
			s.logLookupFailure(ctx, 1, fmt.Errorf("玩家 %d 的 PlayerSession 解码失败: %w", playerID, err))
			continue
		}
		// **只认 SESSION_STATE_ONLINE**:DISCONNECTING 是"断线等重连"的租约期,
		// 此时客户端收不到任何东西,把它显示成在线会让玩家以为对方在装作不回话。
		if session.GetState() != plpb.PlayerSessionState_SESSION_STATE_ONLINE {
			metrics.ObserveOnlineLookup(metrics.OutcomeOffline)
			continue
		}
		metrics.ObserveOnlineLookup(metrics.OutcomeOK)
		states[playerID] = OnlineStatus{
			Online:       true,
			LastActiveMs: session.GetLastActiveTs(),
		}
	}
	return nil
}

// logLookupFailure 按 onlineLookupLogInterval 限流地打一条错误日志。
//
// 用墙钟(UnixNano)而不是单调时钟:这里只是日志节流,不是超时预算 ——
// 系统时间被调整时最坏结果是多打或少打一条日志。
// go-zero 的 logx 没有 Warn 级,按仓内惯例用 Errorf 打。
func (s *SessionReader) logLookupFailure(ctx context.Context, affected int, err error) {
	now := time.Now().UnixNano()
	last := s.lastLogUnixNano.Load()
	if now-last < int64(onlineLookupLogInterval) {
		return
	}
	// CAS 失败说明别的 goroutine 刚打过,这次就不打了(限流的目的已经达到)。
	if !s.lastLogUnixNano.CompareAndSwap(last, now) {
		return
	}
	logx.WithContext(ctx).Errorf("[friend] 读 player:session 失败,%d 名玩家本次按离线返回(日志每 %v 最多一条,"+
		"完整次数看指标 friend_online_lookup_total{outcome=\"error\"}): %v", affected, onlineLookupLogInterval, err)
}

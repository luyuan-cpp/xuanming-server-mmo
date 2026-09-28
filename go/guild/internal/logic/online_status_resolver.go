package logic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	plpb "proto/player_locator"
	"shared/safego"
)

const playerSessionKeyPrefix = "player:session:"

// playerSessionKey 拼 player_locator 维护的会话键(契约 key,只读不写)。
// 抽成函数是为了让"在线判定"与"推送路由"读的是同一把键 —— 两处各拼一次
// 迟早有一处漏改前缀。
func playerSessionKey(playerID uint64) string {
	return fmt.Sprintf("%s%d", playerSessionKeyPrefix, playerID)
}

// onlineSessionFrom 解一条 MGET 结果。
//
// 第二个返回值只有 SESSION_STATE_ONLINE 才为 true(与 match push.go 同判据):
// 断线等待重连期间的会话虽然还在,但 gate 上已经没有连接,推过去只会被丢弃。
// 解不开、类型不对、键不存在(value 为 nil,落到 default)一律按"不在线"处理 ——
// 这是推送路径,拿不准就不推,比推给错误的会话安全。
func onlineSessionFrom(value any) (*plpb.PlayerSession, bool) {
	var raw []byte
	switch v := value.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return nil, false
	}

	session := &plpb.PlayerSession{}
	if err := proto.Unmarshal(raw, session); err != nil {
		return nil, false
	}
	return session, session.GetState() == plpb.PlayerSessionState_SESSION_STATE_ONLINE
}

// OnlineStatusResolver resolves player online state from player_locator session store.
type OnlineStatusResolver struct {
	rdb *redis.Client
	// strictTimeout 是 BatchResolveStrict 的独立上限;<= 0 用 DefaultStrictOnlineLookupTimeout。
	// 只有单测会改它(同包直接赋值),生产一律走默认值,所以不进构造函数、不改 guild.go 的调用点。
	strictTimeout time.Duration
}

func NewOnlineStatusResolver(rdb *redis.Client) *OnlineStatusResolver {
	return &OnlineStatusResolver{rdb: rdb}
}

func (r *OnlineStatusResolver) BatchResolve(ctx context.Context, playerIDs []uint64) map[uint64]bool {
	onlineMap := make(map[uint64]bool, len(playerIDs))
	if r == nil || r.rdb == nil || len(playerIDs) == 0 {
		return onlineMap
	}

	keys := make([]string, 0, len(playerIDs))
	ids := make([]uint64, 0, len(playerIDs))
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if _, ok := seen[playerID]; ok {
			continue
		}
		seen[playerID] = struct{}{}
		ids = append(ids, playerID)
		keys = append(keys, playerSessionKey(playerID))
	}

	values, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		logx.Errorf("batch get player sessions failed: %v", err)
		return onlineMap
	}
	// MGET 返回值与 keys 一一对应;长度对不上就不能按下标回填,
	// 否则会把 A 的在线状态记到 B 头上(展示成员列表时表现为随机的在线标记)。
	if len(values) != len(ids) {
		logx.Errorf("batch get player sessions: MGET returned %d values for %d keys", len(values), len(ids))
		return onlineMap
	}

	for i, value := range values {
		if _, online := onlineSessionFrom(value); online {
			onlineMap[ids[i]] = true
		}
	}

	return onlineMap
}

// ── 严格版:写路径的在线判定(06 §6.8 末段)────────────────────────

// DefaultStrictOnlineLookupTimeout 是 BatchResolveStrict 一次批量读的独立上限。
//
// 为什么必须有独立上限(92-handoff §8.2 记过的旧问题):locator Redis 客户端没开 ContextTimeoutEnabled,
// go-redis v9 此时的套接字读**不看** ctx 截止时间,每次尝试只受默认 3s ReadTimeout 约束;Redis 卡住时
// BatchResolve 的调用方要等约 6s 才返回,远超 handler 的 3500ms 业务预算。团圆领奖在这一步之后还要发号、
// 跑事务(1500ms 子预算)、同步投递,所以这里只给 800ms:正常一次 MGET 是毫秒级,800ms 仍读不到就是 Redis 出了状况。
const DefaultStrictOnlineLookupTimeout = 800 * time.Millisecond

// ErrOnlineStateUnknown:至少一名玩家的在线状态读不到(Redis 出错 / 超时 / 未配置 / 会话值解不开 / MGET 回包长度不符)。
// 调用方用 errors.Is 判它;具体原因包在后面。
var ErrOnlineStateUnknown = errors.New("guild: player online state unknown")

// BatchResolveStrict 与 BatchResolve 判据相同(只有 SESSION_STATE_ONLINE 算在线;返回的 map 只含在线者,
// len(map) 即在线人数),区别是**任何一名玩家的状态读不到就整体失败**,不把"读不到"当"离线"。
//
// 为什么写路径必须 fail-closed(中秋团圆:数到的人数达标就锁存本档期进度、给在场者发奖):
//   - 读不到当离线 → 人数少算。Redis 抖动时团圆被判"人数不足":玩家看到一句业务拒绝,以为是人没凑齐,
//     实际是服务端故障 —— 故障被伪装成业务结论,告警也看不到;
//   - 反过来,若把读不到的人当在线 → 人数多算,凑不齐人也能锁存。锁存在整个档期内不可撤销(之后每人每天都能领),
//     属于不可逆的错发。
//
// 两个方向都错,唯一正确的答复是"现在判定不了":返回错误,由调用方按故障处理,玩家稍后重试即可。
// 只读展示路径(GetGuildActivities)同样调它,但失败时降级成 0 并计指标 —— 展示错一个数字不值得让整页失败。
//
// 空列表直接回空 map、不读 Redis:没有候选人时"0 人在线"是真话,与 Redis 好坏无关。
// 键不存在(MGET 回 nil)是确定的"没有会话 = 离线",不算读不到。
func (r *OnlineStatusResolver) BatchResolveStrict(ctx context.Context, playerIDs []uint64) (map[uint64]bool, error) {
	ids, keys := uniqueSessionKeys(playerIDs)
	onlineMap := make(map[uint64]bool, len(ids))
	if len(ids) == 0 {
		return onlineMap, nil
	}
	if r == nil || r.rdb == nil {
		return nil, fmt.Errorf("%w: session redis not configured", ErrOnlineStateUnknown)
	}

	values, err := r.mgetWithin(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOnlineStateUnknown, err)
	}
	// 长度对不上就不能按下标回填(会把 A 的状态记到 B 头上),整批作废。
	if len(values) != len(ids) {
		return nil, fmt.Errorf("%w: MGET returned %d values for %d keys", ErrOnlineStateUnknown, len(values), len(ids))
	}
	for i, value := range values {
		online, known := strictOnlineFrom(value)
		if !known {
			return nil, fmt.Errorf("%w: session of player %d undecodable", ErrOnlineStateUnknown, ids[i])
		}
		if online {
			onlineMap[ids[i]] = true
		}
	}
	return onlineMap, nil
}

// uniqueSessionKeys 去重并拼会话键,ids 与 keys 下标一一对应(保留首次出现的顺序)。
// 只给严格版用;BatchResolve 按 06 §6.8 "原函数不改"保持原样,不抽共用。
func uniqueSessionKeys(playerIDs []uint64) ([]uint64, []string) {
	ids := make([]uint64, 0, len(playerIDs))
	keys := make([]string, 0, len(playerIDs))
	seen := make(map[uint64]struct{}, len(playerIDs))
	for _, playerID := range playerIDs {
		if _, ok := seen[playerID]; ok {
			continue
		}
		seen[playerID] = struct{}{}
		ids = append(ids, playerID)
		keys = append(keys, playerSessionKey(playerID))
	}
	return ids, keys
}

// strictOnlineFrom 解一条 MGET 结果。known=false = 这名玩家的状态读不到。
//
// 与 onlineSessionFrom 的区别只在"读不到"的归类:nil(键不存在)是确定的离线;
// 字符串 / 字节解得开就按 ONLINE 判据(与推送、成员列表同一判据,复用 onlineSessionFrom);
// 解不开或类型不对 = 状态未知,交给调用方 fail-closed,而不是像推送路径那样"拿不准就当离线"。
func strictOnlineFrom(value any) (online bool, known bool) {
	switch value.(type) {
	case nil:
		return false, true
	case string, []byte:
		session, isOnline := onlineSessionFrom(value)
		if session == nil {
			return false, false
		}
		return isOnline, true
	default:
		return false, false
	}
}

// strictMGetResult 是后台 MGET 协程交回的结果。
type strictMGetResult struct {
	values []any
	err    error
}

// mgetWithin 在独立上限内做一次 MGET。
//
// 光给 MGet 传一个带超时的 ctx **不够**:locator 客户端没开 ContextTimeoutEnabled,套接字读不看 ctx,
// ctx 只在取连接与重试退避处生效(见 DefaultStrictOnlineLookupTimeout)。所以把 MGET 放进单独的协程,
// 本协程在"结果到达"与"上限到期"之间择先:到期就立刻回错误,handler 不再陪着等套接字超时。
//
// 被丢下的协程不会泄漏:通道带 1 格缓冲,它写结果永不阻塞;它自己最迟在一次 ReadTimeout(默认 3s)后返回,
// 且 ctx 已取消,go-redis 不会再发起重试。用 safego.Go 派生:MGET 里真出 panic 也只丢这一次判定
// (本协程按超时回错误),不打死进程。
func (r *OnlineStatusResolver) mgetWithin(ctx context.Context, keys []string) ([]any, error) {
	timeout := r.strictTimeout
	if timeout <= 0 {
		timeout = DefaultStrictOnlineLookupTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan strictMGetResult, 1)
	safego.Go("guild.online_strict_mget", func() {
		values, err := r.rdb.MGet(lookupCtx, keys...).Result()
		done <- strictMGetResult{values: values, err: err}
	})
	select {
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("mget %d player sessions: %w", len(keys), res.err)
		}
		return res.values, nil
	case <-lookupCtx.Done():
		return nil, fmt.Errorf("mget %d player sessions: %w", len(keys), lookupCtx.Err())
	}
}

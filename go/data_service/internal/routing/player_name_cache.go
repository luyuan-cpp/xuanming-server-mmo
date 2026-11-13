package routing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shared/clusterredis"

	"github.com/redis/go-redis/v9"
)

// 玩家名字读缓存(设计 docs/design/guild-phase2/03-names.md §3.5)。
//
// 【这把 Redis 只放可丢弃的读缓存】它在控制集群上(配置段 ControlRedis,docs/design/redis-cluster-all-domains-20261110.md §4.8;
// 2026-11-13 起取代独立的 NameCacheRedis 段),里面没有任何权威数据:整组节点丢了,名字读路径回源一次全局库,
// 建角和归属都不受影响。不要往上面加要落盘或要保证一致的东西。
//
// 【真相在哪】名字的唯一真源是全局库的 player_name 表(store/player_name_store.go)。
// 本文件里的一切都只是加速:缓存写失败**绝不能**让业务写失败,缓存读到的脏值最坏
// 也只是展示面短暂不一致。唯一性判定从不看缓存。
//
// 【为什么要负缓存】读侧(帮会成员列表、角色列表)会反复查同一批 id,其中一定包含
// "库里根本没有"的 id(早于本功能建的角色、已释放的名字)。不记"查不到"这件事,
// 这些 id 每次都要回源打一次 SQL,而它们恰恰是永远不会命中的那批 —— 缓存就只剩
// 负担没有收益了。
//
// 【键不带 hash tag,批量读走单键 GET 的管道】键是 player:name:<id>,每个玩家一个,不需要和别的键同槽。
// 控制集群的守卫(clusterredis.ControlClusterKeys)允许单键命令不带 tag,但多键命令的每个键都必须带同一个 tag。
// 所以批量读不能用 MGET,也不能用按槽分组的 MGetBySlot:一批 500 个 id 在 16384 个槽里几乎必然有两个落同槽
// (生日问题,约 99.9%),那一组就是两个不带 tag 的键,守卫拒绝,整批读失败、退化成全部回源。
// 单键 GET 的管道没有这个问题;ClusterClient 会按节点拆管道,单实例时就是一条管道。

const (
	playerNameKeyPrefix = "player:name:"

	// playerNameAbsentSentinel 是负缓存的值:一个 NUL 字节。
	//
	// 为什么可以用它当哨兵:合法名字的字符集(shared/playername.IsAllowedRune)只放行
	// 数字、拉丁字母和几段汉字区间,**不可能**出现 NUL。所以"值等于哨兵"与"值是一个
	// 真名字"两件事在语义上永远互斥,不需要再加一层编码或前缀。
	//
	// 为什么不用空串当哨兵:空串是"名字为空"这个非法值的自然写法,写入侧(SetPlayerNames)
	// 拒绝空名字;读侧把空串一律当未命中。哨兵用合法字符集之外的字节,两种判定互不依赖。
	playerNameAbsentSentinel = "\x00"
)

// PlayerNameKey 返回某个玩家名字缓存键。导出是为了让单测与排障脚本引用同一份拼法,
// 而不是各自手写一遍字符串(拼错的后果是"缓存永远不命中",而且不报错)。
func PlayerNameKey(playerID uint64) string {
	return playerNameKeyPrefix + strconv.FormatUint(playerID, 10)
}

// PlayerNameCache 是 player:name:{id} 的读写。并发安全;底层句柄不归这里关闭(见两个构造函数)。
type PlayerNameCache struct {
	rds redis.UniversalClient
}

// NewPlayerNameCache 用调用方建好的控制集群句柄(clusterredis.New(..., clusterredis.ControlClusterKeys))包一层名字缓存。
// 句柄归调用方关闭(常驻服务登记在 ServiceContext 的 upstreamClosers 里)。
func NewPlayerNameCache(rds redis.UniversalClient) *PlayerNameCache {
	return &PlayerNameCache{rds: rds}
}

// OpenPlayerNameCache 按控制集群配置建句柄并 PING 一次;配置不合法、连不上都回错误,不退出进程。
// 给运维工具用(cmd/create_residual 修完名字要删缓存):工具要自己决定拒绝运行、给出固定文案。
// 句柄随工具进程退出释放,不另给关闭入口。
func OpenPlayerNameCache(ctx context.Context, c clusterredis.Config) (*PlayerNameCache, error) {
	rds, err := clusterredis.New(c, clusterredis.ControlClusterKeys)
	if err != nil {
		return nil, err
	}
	if err := rds.Ping(ctx).Err(); err != nil {
		_ = rds.Close()
		return nil, err
	}
	return &PlayerNameCache{rds: rds}, nil
}

// MGetPlayerNames 一次查多个玩家的名字缓存。
//
// 三态,调用方必须分清:
//   - hits[id] 存在   → 缓存里有这个名字,直接用;
//   - absent[id]为真  → 缓存记着"库里没有",本轮**不要**回源;
//   - 两者都没有      → 未命中,需要回源查库。
//
// 入参里的 0 与重复 id 会被跳过(0 不是合法 player_id;重复查同一个键纯属浪费),
// 所以本方法对调用方是否已经去重不敏感。ids 为空时返回两个空 map 和 nil。
//
// Redis 报错时(任何一个键的读失败都算)返回 (nil, nil, err):调用方应当把它降级成"全部未命中"
// (缓存挂了不该让读接口整体失败),而不是把错误往上抛。不返回部分结果,与 clusterredis.MGetBySlot 同口径。
func (c *PlayerNameCache) MGetPlayerNames(ctx context.Context, ids []uint64) (hits map[uint64]string, absent map[uint64]bool, err error) {
	hits = make(map[uint64]string)
	absent = make(map[uint64]bool)
	if len(ids) == 0 {
		return hits, absent, nil
	}

	wanted := make([]uint64, 0, len(ids))
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		wanted = append(wanted, id)
	}
	if len(wanted) == 0 {
		return hits, absent, nil
	}

	// cmds 与 wanted 同序同长:第 i 条 GET 读的就是 wanted[i] 的键。
	cmds := make([]*redis.StringCmd, len(wanted))
	_, err = c.rds.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, id := range wanted {
			cmds[i] = pipe.Get(ctx, PlayerNameKey(id))
		}
		return nil
	})
	// 管道把第一条失败命令的错误带回来;缺键的 GET 也会以 redis.Nil 出现在这里,那不是故障,逐条再判。
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, nil, fmt.Errorf("player name cache get (%d keys): %w", len(wanted), err)
	}

	for i, cmd := range cmds {
		val, getErr := cmd.Result()
		switch {
		case errors.Is(getErr, redis.Nil), getErr == nil && val == "":
			// 键不存在(或被写成了不该出现的空串)= 未命中,两个 map 都不放。
		case getErr != nil:
			return nil, nil, fmt.Errorf("player name cache get %s: %w", PlayerNameKey(wanted[i]), getErr)
		case val == playerNameAbsentSentinel:
			absent[wanted[i]] = true
		default:
			hits[wanted[i]] = val
		}
	}
	return hits, absent, nil
}

// SetPlayerNames 回填正缓存(SET + 过期,**覆盖**已有值)。
//
// 为什么用会覆盖的 SET 而不是 SETNX:这里写的是刚从库里读出来/刚提交进库的真值,
// 它必须能盖掉同一个 id 上可能存在的负缓存(时序见 §3.5)。
//
// TTL 的作用**不是**让数据过期失效(v1 不支持改名,名字不会变),而是给"释放之后
// DEL 缓存失败"留下的脏值封一个顶:最坏 CacheTTL(24h)之后它自己消失。
//
// 跳过 id==0 与空名字:读侧把空串当未命中,写进去也只是白占一个键。
// 全部被跳过时不发任何命令,直接返回 nil。SET 带固定值与 TTL,重放结果相同,守卫允许它进管道。
func (c *PlayerNameCache) SetPlayerNames(ctx context.Context, names map[uint64]string, ttl time.Duration) error {
	if len(names) == 0 {
		return nil
	}
	if ttl <= 0 {
		// 不静默改成"永不过期":那会让脏值永久留在库里。宁可报错让调用方打日志,
		// 说明配置(PlayerName.CacheTTL)没填对。
		return fmt.Errorf("player name cache set: ttl must be positive, got %v", ttl)
	}

	writable := make(map[uint64]string, len(names))
	for id, name := range names {
		if id == 0 || name == "" {
			continue
		}
		writable[id] = name
	}
	if len(writable) == 0 {
		return nil
	}

	_, err := c.rds.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for id, name := range writable {
			pipe.Set(ctx, PlayerNameKey(id), name, ttl)
		}
		return nil
	})
	return err
}

// SetPlayerNamesAbsent 回填负缓存(SET NX + 过期)。
//
// **必须是 NX**:负缓存永远不许盖掉一个真名字。坏时序是这样的 ——
// BatchGet 读库时这个 id 还没名字 → 建角的 Reserve 提交并写下真名 → BatchGet 才
// 轮到写负缓存。NX 让这一步失败,真名留下,正确。反过来用覆盖式 SET,
// 刚建好的角色就会凭空"没名字" NegativeCacheTTL 那么久。
//
// 唯一残留的坏时序是"Reserve 的 SET 失败 + 本方法的 SETNX 成功",后果是名字被隐藏
// ≤ NegativeCacheTTL(60s)。名字是展示数据,这个代价可以接受(§3.5)。
// SETNX 重放不改变终态(键已在就不写),守卫允许它进管道。
func (c *PlayerNameCache) SetPlayerNamesAbsent(ctx context.Context, ids []uint64, ttl time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	if ttl <= 0 {
		return fmt.Errorf("player name negative cache set: ttl must be positive, got %v", ttl)
	}

	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		seen[id] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}

	_, err := c.rds.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for id := range seen {
			pipe.SetNX(ctx, PlayerNameKey(id), playerNameAbsentSentinel, ttl)
		}
		return nil
	})
	return err
}

// DelPlayerName 删掉一个玩家的名字缓存(正负缓存是同一个键,一次删干净)。
//
// 调用点:Release 成功删库**之后**,以及 create_residual 修完名字之后。顺序不能反 —— 先删缓存再删库的话,
// 两步之间的一次读会把库里那个即将消失的名字重新缓存起来,DEL 就白做了。
// 本方法失败不算业务失败(库已经是真相),调用方记日志即可,脏值由 TTL 兜底。
func (c *PlayerNameCache) DelPlayerName(ctx context.Context, playerID uint64) error {
	if playerID == 0 {
		return nil
	}
	return c.rds.Del(ctx, PlayerNameKey(playerID)).Err()
}

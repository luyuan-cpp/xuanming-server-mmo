package routing

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 玩家名字读缓存(设计 docs/design/guild-phase2/03-names.md §3.5)。
//
// 为什么放在 routing 包:缓存键 player:name:{id} 必须与 player:zone:{id} 同库
// ——两者都是"全服一份、与 zone 无关"的映射,而那个库的客户端 Router.mappingRedis
// 是未导出字段。放别处就得把它导出,等于把"谁能往 mapping 库写"这件事敞开。
//
// 【真相在哪】名字的唯一真源是全局库的 player_name 表(store/player_name_store.go)。
// 本文件里的一切都只是加速:缓存写失败**绝不能**让业务写失败,缓存读到的脏值最坏
// 也只是展示面短暂不一致。唯一性判定从不看缓存。
//
// 【为什么要负缓存】读侧(帮会成员列表、角色列表)会反复查同一批 id,其中一定包含
// "库里根本没有"的 id(早于本功能建的角色、已释放的名字)。不记"查不到"这件事,
// 这些 id 每次都要回源打一次 SQL,而它们恰恰是永远不会命中的那批 —— 缓存就只剩
// 负担没有收益了。

const (
	// playerNameKeyPrefix 与 mappingKeyPrefix("player:zone:")同库不同前缀。
	playerNameKeyPrefix = "player:name:"

	// playerNameAbsentSentinel 是负缓存的值:一个 NUL 字节。
	//
	// 为什么可以用它当哨兵:合法名字的字符集(shared/playername.IsAllowedRune)只放行
	// 数字、拉丁字母和几段汉字区间,**不可能**出现 NUL。所以"值等于哨兵"与"值是一个
	// 真名字"两件事在语义上永远互斥,不需要再加一层编码或前缀。
	//
	// 为什么不用空串当哨兵:go-zero 的 MgetCtx 把"键不存在"也映射成空串,空串一旦
	// 被当成哨兵,"没缓存"和"缓存说没有"就分不开了。
	playerNameAbsentSentinel = "\x00"
)

// PlayerNameKey 返回某个玩家名字缓存键。导出是为了让单测与排障脚本引用同一份拼法,
// 而不是各自手写一遍字符串(拼错的后果是"缓存永远不命中",而且不报错)。
func PlayerNameKey(playerID uint64) string {
	return playerNameKeyPrefix + strconv.FormatUint(playerID, 10)
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
// Redis 报错时返回 (nil, nil, err):调用方应当把它降级成"全部未命中"(缓存挂了
// 不该让读接口整体失败),而不是把错误往上抛。
func (r *Router) MGetPlayerNames(ctx context.Context, ids []uint64) (hits map[uint64]string, absent map[uint64]bool, err error) {
	hits = make(map[uint64]string)
	absent = make(map[uint64]bool)
	if len(ids) == 0 {
		return hits, absent, nil
	}

	// keys 与 wanted 必须严格同序同长:MgetCtx 的返回值是按 key 位置对齐的,
	// 少跳过一个或多跳过一个,后面的名字就会安到别人头上。
	keys := make([]string, 0, len(ids))
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
		keys = append(keys, PlayerNameKey(id))
		wanted = append(wanted, id)
	}
	if len(keys) == 0 {
		return hits, absent, nil
	}

	vals, err := r.mappingRedis.MgetCtx(ctx, keys...)
	if err != nil {
		return nil, nil, fmt.Errorf("player name cache mget (%d keys): %w", len(keys), err)
	}
	// 防御:go-zero 正常会按 key 数返回等长切片,长度对不上说明底层语义变了,
	// 这时宁可整体当未命中,也不能按错位的下标取值。
	if len(vals) != len(keys) {
		return nil, nil, fmt.Errorf("player name cache mget returned %d values for %d keys", len(vals), len(keys))
	}

	for i, val := range vals {
		switch val {
		case "":
			// 键不存在 = 未命中,两个 map 都不放。
		case playerNameAbsentSentinel:
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
// 跳过 id==0 与空名字:空名字写进去会和"未命中"(MgetCtx 对缺键返回空串)混为一谈。
// 全部被跳过时不发任何命令,直接返回 nil。
func (r *Router) SetPlayerNames(ctx context.Context, names map[uint64]string, ttl time.Duration) error {
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

	return r.mappingRedis.PipelinedCtx(ctx, func(pipe redis.Pipeliner) error {
		for id, name := range writable {
			pipe.Set(ctx, PlayerNameKey(id), name, ttl)
		}
		return nil
	})
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
func (r *Router) SetPlayerNamesAbsent(ctx context.Context, ids []uint64, ttl time.Duration) error {
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

	return r.mappingRedis.PipelinedCtx(ctx, func(pipe redis.Pipeliner) error {
		for id := range seen {
			pipe.SetNX(ctx, PlayerNameKey(id), playerNameAbsentSentinel, ttl)
		}
		return nil
	})
}

// DelPlayerName 删掉一个玩家的名字缓存(正负缓存是同一个键,一次删干净)。
//
// 调用点只有一处:Release 成功删库**之后**。顺序不能反 —— 先删缓存再删库的话,
// 两步之间的一次读会把库里那个即将消失的名字重新缓存起来,DEL 就白做了。
// 本方法失败不算业务失败(库已经是真相),调用方记日志即可,脏值由 TTL 兜底。
func (r *Router) DelPlayerName(ctx context.Context, playerID uint64) error {
	if playerID == 0 {
		return nil
	}
	_, err := r.mappingRedis.DelCtx(ctx, PlayerNameKey(playerID))
	return err
}

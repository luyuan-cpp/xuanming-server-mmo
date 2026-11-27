package svc

import (
	"errors"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/syncx"
)

// SpectateLobbyCacheTTL 是观战列表读的缓存时长,也是列表最多旧多久。
const SpectateLobbyCacheTTL = time.Second

// SpectateIndexMember 是全服观战索引(ZSET)里的一个成员。Member 是 battle_id 的十进制文本;Score 的含义随索引而定
// (活跃集合是开局毫秒,赛事局索引是标记到期毫秒)。
type SpectateIndexMember struct {
	Member string
	Score  int64
}

// SpectateLobbySnapshot 是观战列表从拆桶索引合并出来的一份结果(logic/listwatchablebattleslogic.go 的 loadSpectateLobby 读)。
// 多个请求共用同一份,只读,不要改里面的切片。
type SpectateLobbySnapshot struct {
	Featured []SpectateIndexMember // 赛事局索引里未到期的成员,后标记的在前
	Ranked   []SpectateIndexMember // 高段正式排位直播索引,开局晚的在前(防普通 PVE 稀释)
	Active   []SpectateIndexMember // 活跃集合,开局晚的在前
}

// SpectateLobbyCache 是观战列表读的进程内短缓存(docs/design/redis-cluster-all-domains-20261110.md §4.9)。列表读要跨 16 个桶,
// 每个请求都回源的话 Redis 读量跟着玩家请求量涨。这里把合并结果留 SpectateLobbyCacheTTL;过期后同一时刻只有一个请求回源
// (singleflight),其余等它的结果,所以每个副本每秒最多成功回源一次,与请求量无关。回源失败不缓存,下一个请求接着重试
// (同一时刻仍只有一个在回源)。
// 只有列表读用它:写路径、随机选场与抢配额都直接读 Redis。nil 缓存 = 不缓存,每次回源(手工构造的单测 ServiceContext)。
type SpectateLobbyCache struct {
	now    func() time.Time
	flight syncx.SingleFlight

	mu        sync.Mutex
	snapshot  SpectateLobbySnapshot
	fetchedAt time.Time
	filled    bool
}

// NewSpectateLobbyCache 建一份缓存。now 是时钟,单测注入假时钟。
func NewSpectateLobbyCache(now func() time.Time) *SpectateLobbyCache {
	return &SpectateLobbyCache{now: now, flight: syncx.NewSingleFlight()}
}

// Load 返回不超过 SpectateLobbyCacheTTL 的结果,过期时用 fetch 回源。并发的调用共用同一次回源。
func (c *SpectateLobbyCache) Load(fetch func() (SpectateLobbySnapshot, error)) (SpectateLobbySnapshot, error) {
	if c == nil {
		return fetch()
	}
	if snapshot, ok := c.fresh(); ok {
		return snapshot, nil
	}
	value, err := c.flight.Do("lobby", func() (any, error) {
		// 上一次回源可能刚好在本请求查完缓存之后写好:再查一次,一个周期只回源一次。
		if snapshot, ok := c.fresh(); ok {
			return snapshot, nil
		}
		// 时刻取在回源之前:按读开始的时刻算新旧,结果不会比 TTL 更旧。
		started := c.now()
		snapshot, err := fetch()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.snapshot, c.fetchedAt, c.filled = snapshot, started, true
		c.mu.Unlock()
		return snapshot, nil
	})
	if err != nil {
		return SpectateLobbySnapshot{}, err
	}
	snapshot, ok := value.(SpectateLobbySnapshot)
	if !ok {
		// 回源 panic 时等待者拿到的是空值,不能当成空列表返回。
		return SpectateLobbySnapshot{}, errors.New("spectate lobby fetch returned no result")
	}
	return snapshot, nil
}

// Forget 从当前结果里去掉一个已经从索引摘掉的成员,本副本之后的列表请求不再读它的成局、不再摘第二次。
// 换新切片,不改已经交出去的那份。
func (c *SpectateLobbyCache) Forget(member string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = SpectateLobbySnapshot{
		Featured: withoutLobbyMember(c.snapshot.Featured, member),
		Ranked:   withoutLobbyMember(c.snapshot.Ranked, member),
		Active:   withoutLobbyMember(c.snapshot.Active, member),
	}
}

func (c *SpectateLobbyCache) fresh() (SpectateLobbySnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot, c.filled && c.now().Sub(c.fetchedAt) < SpectateLobbyCacheTTL
}

func withoutLobbyMember(members []SpectateIndexMember, member string) []SpectateIndexMember {
	for i, entry := range members {
		if entry.Member == member {
			kept := make([]SpectateIndexMember, 0, len(members)-1)
			kept = append(kept, members[:i]...)
			return append(kept, members[i+1:]...)
		}
	}
	return members
}

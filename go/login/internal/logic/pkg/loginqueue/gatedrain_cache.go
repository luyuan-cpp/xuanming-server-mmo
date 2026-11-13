package loginqueue

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DrainMarkCache 是 gate 排空标记(gate:{gm:<zone>}:<id>:draining)的按 zone 读穿透缓存(2026-11-05,服务器缺口清单第 31 条)。
//
// 为什么要缓存:选 gate 的每条路径 —— AssignGate 快速通道、容量判断、dispatcher 每一拍、取走放行时的复核 —— 都经
// CandidatesForZone,原来每次各打一次 MGET。开服洪峰时这几条路径的调用量是登录量的几倍,一个 zone 的标记又都在同一个槽上
// (hash tag 按 zone 打),读量随本 zone 的登录量一起涨。排空是分钟级的计划内动作,标记晚一两秒生效无关紧要:这里把一次查询结果按 zone 缓存
// refresh 时长,同一 zone 在这段时间里的所有选 gate 共用一份,每个 login 实例每个 zone 每 refresh 至多查一次。
//
// 晚生效的这 refresh 时长不改变排空的完成条件:标记之前签出的票在票有效期内照样能连上这台 gate,gate 的在线数
// 也要等它下一次上报(每 10 秒)才看得到,排空流程本来就要容忍这批晚到的玩家(见 gatedrain.go 的三步与
// docs/design/gate-drain-session-migration.md);标记后再分进来的玩家与它们是同一类,数量上限是 refresh 时长里的分配量。
//
// 语义:
//   - 命中:返回上次查到的排空集合。集合里没有的 node_id(包括之后才登记的新 gate)按没在排空处理 —— 新起的 gate
//     不会正在排空;
//   - 过期:由一个调用方去查,同 zone 的并发调用等它查完共用结果,不各查一次;
//   - 查询失败:与原来一样按「没人在排空」放行(见 DrainingGates),并把这个空结果同样缓存 refresh 时长 ——
//     Redis 出问题时不让每个登录请求都去撞一次超时;
//   - refresh <= 0:不缓存,每次都查(单测用)。
//
// 线程安全。零值不可用,用 NewDrainMarkCache 构造;nil *DrainMarkCache 的 Draining 返回 nil(不打排空标记)。
type DrainMarkCache struct {
	rdb redis.UniversalClient
	// markZone 是标记键里的 zone:本 login 的 Node.ZoneId(见 gatedrain.go 里对 zoneID 的说明)。
	// 它不是 Draining 的 zoneID 参数:那个只是缓存的分区键,可以是 0(不按 zone 过滤的那一份候选)。
	markZone uint32
	refresh  time.Duration
	now      func() time.Time

	mu    sync.Mutex
	zones map[uint32]*drainZoneEntry
}

type drainZoneEntry struct {
	mu       sync.Mutex // 同 zone 的查询串行:过期时只有一个调用方去查
	draining map[uint32]bool
	expires  time.Time
}

// NewDrainMarkCache 构造缓存。rdb 为 nil 时 Draining 一律返回 nil(不打排空标记,同原来 rdb 为 nil 的路径);
// markZone 是本 login 的 Node.ZoneId,标记键按它拼;now 为 nil 时用 time.Now(带单调时钟读数,过期判断不受墙钟回拨影响)。
func NewDrainMarkCache(rdb redis.UniversalClient, markZone uint32, refresh time.Duration, now func() time.Time) *DrainMarkCache {
	if now == nil {
		now = time.Now
	}
	return &DrainMarkCache{rdb: rdb, markZone: markZone, refresh: refresh, now: now, zones: make(map[uint32]*drainZoneEntry)}
}

// Draining 返回 candidates 里哪些 gate 正在排空;结果交给 MarkDrainingGates 打标记,调用方只读不改。
// zoneID 是缓存的分区键,与 CandidatesForZone 的 zoneID 相同(0 = 不按 zone 过滤的那一份)。候选全部来自本 login 的
// GateWatcher,都是 markZone 的 gate,所以不管分区键是几,标记都按 markZone 读。
// 候选为空时不查、不建缓存条目,所以条目数不超过真正有 gate 的 zone 数。
func (c *DrainMarkCache) Draining(ctx context.Context, zoneID uint32, candidates []GateCandidate) map[uint32]bool {
	if c == nil || c.rdb == nil || len(candidates) == 0 {
		return nil
	}
	entry := c.entry(zoneID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := c.now()
	if c.refresh > 0 && now.Before(entry.expires) {
		return entry.draining
	}
	entry.draining = DrainingGates(ctx, c.rdb, c.markZone, candidates)
	entry.expires = now.Add(c.refresh)
	return entry.draining
}

func (c *DrainMarkCache) entry(zoneID uint32) *drainZoneEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.zones[zoneID]
	if !ok {
		entry = &drainZoneEntry{}
		c.zones[zoneID] = entry
	}
	return entry
}

// Package workshard 把一类后台工作按固定片数分给存活的副本(最高随机权重哈希,HRW / rendezvous)。
//
// 片号 = 业务实体 id 对 Shards 取模:发号器发的永久 id 已经打散(AGENTS.md §7 不变量 1),取模足够均匀。
// 片数只决定分工,不进任何主键或持久数据,改它不用迁移。每片的负责副本 = 成员表里对这一片的键打分最高的成员;
// 副本增减时只有原本落在增减副本上的片换主,其余片不动。
//
// 分工只省重复劳动,不承担正确性。副本增减的几秒里各副本看到的成员表可能不同:某一片可能两个副本都做,
// 也可能暂时没人做(下一拍补上)。用它分工的工作必须在自己的 SQL 提交点用行锁、版本条件或租约保证互斥与幂等。
//
// 打分函数与 go/match/internal/poolowner 相同(FNV-1a 64 位,成员 + 0 字节 + 键)。match 的那一份是它自己的成员表,
// 两边各自分工,结果互不影响;match 迁到本包之前两份并存(docs/design/jubaozhai-market.md §15.8)。
package workshard

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"shared/releasetrack"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Shards 是每类工作的片数。
const Shards = 64

const (
	// RefreshEvery 是成员表整表重读的间隔。副本增减之后,最多这么久(另加 etcd 租约过期)各副本换到新分工。
	RefreshEvery = 5 * time.Second
	// StaleAfter:连续这么久没读到成员表就当成员表为空,本副本做全部片。宁可几个副本重复做,不让某些片没人做。
	StaleAfter = 30 * time.Second
	// listTimeout 是单次整表读的上限,远小于 RefreshEvery,读卡住不会把下一拍也拖过去。
	listTimeout = 3 * time.Second
)

// Owner 返回 key 的负责成员;members 里没有非空成员时返回空串。结果只取决于成员集合,与顺序无关。
func Owner(key string, members []string) string {
	var owner string
	var best uint64
	for _, member := range members {
		if member == "" {
			continue
		}
		score := weight(member, key)
		if owner == "" || score > best || (score == best && member < owner) {
			owner, best = member, score
		}
	}
	return owner
}

// Owns 报告 self 是否负责 key。self 为空或不在 members 里时不负责。
func Owns(self, key string, members []string) bool {
	return self != "" && Owner(key, members) == self
}

func weight(member, key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(member))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

// ShardKey 是 kind 这类工作第 shard 片的归属键。kind 由调用方写死,不同类的工作用不同的 kind,各自独立分工。
func ShardKey(kind string, shard int) string { return kind + ":" + strconv.Itoa(shard) }

// ShardOf 返回实体 id 所在的片,与 SQLFilter 的 MOD(column, Shards) 同一个算法。
func ShardOf(id uint64) int { return int(id % Shards) }

// Filter 是接在 WHERE 里(用 AND 拼)的 SQL 条件与它的绑定参数。零值不过滤。
type Filter struct {
	cond string
	args []any
}

// SQL 返回条件文本;零值是恒真条件 "1=1"。
func (f Filter) SQL() string {
	if f.cond == "" {
		return "1=1"
	}
	return f.cond
}

// Args 返回条件的绑定参数,调用方按条件在语句里出现的位置把它们接进自己的参数列表。
func (f Filter) Args() []any { return f.args }

// SQLFilter 返回把 column 限定在 owns 负责的 kind 片里的条件(片号 = column % Shards)。owns 为 nil 表示全部归本副本。
//
// column 必须是调用方写死的列名(不是外部输入),片号只走绑定参数。条件要放在 LIMIT 之前:各副本只看自己的片,
// 一个副本积压的工作占不住别的副本的名额。全部片都归本副本时不过滤;一片都不归时回恒假条件 "1=0"。
func SQLFilter(owns func(key string) bool, kind, column string) Filter {
	if owns == nil {
		return Filter{}
	}
	shards := make([]any, 0, Shards)
	for shard := 0; shard < Shards; shard++ {
		if owns(ShardKey(kind, shard)) {
			shards = append(shards, shard)
		}
	}
	switch len(shards) {
	case Shards:
		return Filter{}
	case 0:
		return Filter{cond: "1=0"}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(shards)), ",")
	return Filter{cond: "MOD(" + column + "," + strconv.Itoa(Shards) + ") IN(" + placeholders + ")", args: shards}
}

// Registration 是成员表的一条原始注册:etcd 里的键与值(noderegistry 写的 protojson NodeInfo)。
type Registration struct {
	Key   string
	Value []byte
}

// registrationJSON 是 NodeInfo 的 protojson 里成员表用得到的两个字段(Go noderegistry 与 C++ 都写 camelCase)。
// shared 不 import proto 模块(noderegistry 包注释「为什么不 import proto」),所以按字段名读 JSON,
// 与 shared/scenenode 的做法相同;其余字段一律忽略,滚动升级时新版本多带的字段不影响解析。
type registrationJSON struct {
	NodeUuid     string `json:"nodeUuid"`
	ReleaseTrack string `json:"releaseTrack"`
}

// StableMembers 从注册里取 stable 节点的 UUID,升序、去重。
// 跳过:noderegistry 在同一前缀下写的 "/allocated/" 占位键、解析不了的值、没有 UUID 的、认不出轨道的、canary 的。
// canary 不跑全局后台(docs/design/release-track-gray-canary.md §2.5),算进成员表的话,落在它名下的片就没人做。
func StableMembers(regs []Registration) []string {
	members := make([]string, 0, len(regs))
	for _, reg := range regs {
		if strings.Contains(reg.Key, "/allocated/") {
			continue
		}
		var info registrationJSON
		if err := json.Unmarshal(reg.Value, &info); err != nil || info.NodeUuid == "" {
			continue
		}
		track, err := releasetrack.Parse(info.ReleaseTrack)
		if err != nil || track.IsCanary() {
			continue
		}
		members = append(members, info.NodeUuid)
	}
	slices.Sort(members)
	return slices.Compact(members)
}

// Membership 是一类工作的成员表:etcd 里一个前缀下注册的 stable 节点,每 RefreshEvery 整表读一次。
//
// 判定规则(Owns):
//   - 成员表为空(还没读到,或连续 StaleAfter 没读到):本副本做全部片,保活优先;
//   - 成员表不空、本副本不在其中(还没注册完,或租约丢了正在重注册):什么都不做,交给表里的副本;
//   - 否则按最高随机权重哈希判。
//
// 并发:Run 在一个 goroutine 里写;Owns、Members、SetSelf 可在任意 goroutine 调用(原子指针整表替换)。
// 零值不可用,用 NewMembership 构造;nil 接收者的 Owns 回 true(没装配分工 = 全部归本副本)。
type Membership struct {
	prefix string
	now    func() time.Time

	self       atomic.Pointer[string]
	members    atomic.Pointer[[]string] // 升序、去重;nil = 还没读到过
	listedAtMs atomic.Int64             // 最近一次读成功的时刻(Unix 毫秒)
}

// NewMembership 构造前缀 prefix 的成员表。prefix 要以 "/" 结尾,只圈住一类节点(如 noderegistry.RpcZonePrefix 的结果)。
func NewMembership(prefix string) *Membership {
	return &Membership{prefix: prefix, now: time.Now}
}

// SetSelf 记下本副本在成员表里的身份(注册拿到的 node_uuid)。注册之前为空:成员表不空时本副本什么都不做。
func (m *Membership) SetSelf(uuid string) {
	if m != nil {
		m.self.Store(&uuid)
	}
}

// Owns 报告本副本是否负责 key 这份工作,规则见类型注释。
func (m *Membership) Owns(key string) bool {
	if m == nil {
		return true
	}
	members := m.Members()
	if len(members) == 0 {
		return true
	}
	return Owns(m.selfUUID(), key, members)
}

// Members 返回当前成员表(升序,调用方不得修改);还没读到或已过期返回 nil。
func (m *Membership) Members() []string {
	if m == nil {
		return nil
	}
	members := m.members.Load()
	if members == nil || m.now().UnixMilli()-m.listedAtMs.Load() > StaleAfter.Milliseconds() {
		return nil
	}
	return *members
}

func (m *Membership) selfUUID() string {
	if self := m.self.Load(); self != nil {
		return *self
	}
	return ""
}

// Run 阻塞式定期整表读成员表,ctx 取消即返回。调用方负责放进 goroutine。
// cli 为 nil 时记 ERROR 后返回:成员表一直为空,本副本做全部片(正确性仍由各工作的 SQL 提交点保证)。
func (m *Membership) Run(ctx context.Context, cli *clientv3.Client) {
	if m == nil {
		return
	}
	if cli == nil {
		logx.Errorf("[workshard] No etcd client for prefix=%s: member list stays empty, this replica takes every shard", m.prefix)
		return
	}
	ticker := time.NewTicker(RefreshEvery)
	defer ticker.Stop()
	for {
		m.refresh(ctx, cli)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh 整表读一次。读失败保留上一份成员表,过了 StaleAfter 由 Members 判为过期。
func (m *Membership) refresh(ctx context.Context, kv clientv3.KV) {
	listCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	resp, err := kv.Get(listCtx, m.prefix, clientv3.WithPrefix())
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("[workshard] List members failed prefix=%s (keeping the previous list until it goes stale): %v", m.prefix, err)
		}
		return
	}
	regs := make([]Registration, 0, len(resp.Kvs))
	for _, entry := range resp.Kvs {
		regs = append(regs, Registration{Key: string(entry.Key), Value: entry.Value})
	}
	m.listed(StableMembers(regs))
}

// listed 换上一份新读到的成员表并记下读到的时刻。成员表有变化时记一条日志(不带任何玩家数据)。
func (m *Membership) listed(members []string) {
	if previous := m.members.Load(); previous == nil || !slices.Equal(*previous, members) {
		logx.Infof("[workshard] Members changed prefix=%s count=%d self_listed=%t",
			m.prefix, len(members), slices.Contains(members, m.selfUUID()))
	}
	m.members.Store(&members)
	m.listedAtMs.Store(m.now().UnixMilli())
}

// Package shardowner 决定定时对账的 64 个逻辑分片各由哪个 player_inbox 副本负责
// (docs/design/no-single-node-horizontal-scaling-20261001.md §9,docs/design/player-inbox-service.md §9.3)。
//
// # 为什么要分片
//
// 对账每小时读 24 小时内由消费方终结的行,每轮最多核对 2000 名玩家;它的活随业务量增长(战斗结算也进对账)。
// 原来只在维护领导者上跑,同一时刻只有一个副本在干,加副本扩不了这份吞吐。现在每个 stable 副本只对账自己负责的
// 分片,每个副本各有一份每轮预算,总吞吐随副本数增长。
//
// # 怎么分
//
// 最高随机权重哈希(rendezvous / HRW,与 go/match/internal/poolowner 同法):每个副本对每个分片算一个分数,分数最高者
// 负责。副本增减时只有原本落在增减副本上的分片换主,其余不动。成员表来自 etcd 里本服务注册的 NodeInfo
// (PlayerInboxNodeService.rpc/ 前缀),按 node_uuid 区分进程(换 node_id 不换 uuid,shared/noderegistry)。
// 只算 stable 轨道的副本:全局作用域的后台工作只由 stable 承担(release-track-gray-canary.md §2.5),
// canary 副本不负责任何分片。
//
// # 归属只是分工,不承担正确性
//
// 对账只读。成员表短暂不一致时,两个副本可能同时对账同一片(多读一遍),也可能有一片这一轮没人读(下一轮补上)。
// 不一致行数 gauge 由各副本按自己负责的分片上报,告警取 max(),max_over_time 的 2 小时窗口盖得住换主的空档。
package shardowner

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	base "proto/common/base"
	"shared/etcdwatch"
	"shared/inboxshard"
	"shared/nodeinfo"
	"shared/releasetrack"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ShardCount 是对账分片数,与收件箱表的分片模数同一来源。
const ShardCount = inboxshard.ShardCount

const (
	// etcdOpTimeout 是一次全量读的上限。
	etcdOpTimeout = 5 * time.Second
	// backoffMin / backoffMax:全量读失败后的重试退避。
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// allocatedSegment 出现在节点 id 占位键里(值是 uuid 字符串,不是 NodeInfo),成员表不收它。
	allocatedSegment = "/allocated/"
)

// Owner 返回 shard 的负责副本;members 为空返回空串。结果只取决于成员集合,与顺序无关。
func Owner(shard uint32, members []string) string {
	var owner string
	var best uint64
	for _, member := range members {
		if member == "" {
			continue
		}
		score := weight(member, shard)
		if owner == "" || score > best || (score == best && member < owner) {
			owner, best = member, score
		}
	}
	return owner
}

// Owned 返回 self 负责的分片,升序。self 为空或不在 members 里时返回空:尚未注册或已注销的进程不负责任何分片。
func Owned(self string, members []string) []uint32 {
	if self == "" || !slices.Contains(members, self) {
		return nil
	}
	var owned []uint32
	for shard := uint32(0); shard < ShardCount; shard++ {
		if Owner(shard, members) == self {
			owned = append(owned, shard)
		}
	}
	return owned
}

func weight(member string, shard uint32) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(member))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatUint(uint64(shard), 10)))
	return h.Sum64()
}

// Tracker 用 etcd 维护本服务 stable 副本的成员表,给出本副本负责的分片。并发安全:Run 一条 goroutine 写,
// OwnedShards 任意 goroutine 读。
type Tracker struct {
	cli    *clientv3.Client
	prefix string
	// self 返回本进程注册用的 node_uuid,注册之前为空串。
	self func() string
	// selfCanary 为真:本进程是 canary,不负责任何分片(包注释)。
	selfCanary bool

	mu sync.Mutex
	// members 是 etcd key → node_uuid,只收 stable 副本的注册值。
	members map[string]string
	// synced 为真:至少完成过一次全量读。之后 watch 断开重连期间沿用上一次的成员表(偏差只会多读或少读一轮)。
	synced bool
}

// NewTracker 构造成员表。prefix 是本服务的发现前缀(例如 "PlayerInboxNodeService.rpc/",带结尾斜杠,
// 免得扫到名字以它开头的别的前缀);self 返回本进程的 node_uuid;track 是本进程的发布轨道。
func NewTracker(cli *clientv3.Client, prefix string, self func() string, track releasetrack.Track) (*Tracker, error) {
	if cli == nil || self == nil {
		return nil, fmt.Errorf("shardowner: NewTracker requires an etcd client and a self identity")
	}
	if !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("shardowner: prefix %q must end with '/'", prefix)
	}
	return &Tracker{
		cli: cli, prefix: prefix, self: self, selfCanary: track.IsCanary(),
		members: map[string]string{},
	}, nil
}

// OwnedShards 返回本副本此刻负责的分片(升序)。ready 为 false:成员表还没完成第一次全量读,或本进程还没注册,
// 调用方本轮不对账。canary 进程恒返回空、ready 为 true。
func (t *Tracker) OwnedShards() ([]uint32, bool) {
	if t.selfCanary {
		return nil, true
	}
	self := t.self()
	t.mu.Lock()
	synced := t.synced
	members := make([]string, 0, len(t.members))
	for _, uuid := range t.members {
		members = append(members, uuid)
	}
	t.mu.Unlock()
	if !synced || self == "" {
		return nil, false
	}
	slices.Sort(members)
	return Owned(self, slices.Compact(members)), true
}

// Run 维护成员表直到 ctx 结束:全量读一次,再从读到的 revision 往后 watch;watch 断开、到期(etcdwatch.Open 的
// resync)或读失败都重新全量读。canary 进程不需要成员表,直接返回。
func (t *Tracker) Run(ctx context.Context) {
	if t.selfCanary {
		logx.Info("[player_inbox] Release track is canary: this replica owns no reconcile shards")
		return
	}
	backoff := backoffMin
	for ctx.Err() == nil {
		rev, err := t.resync(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logx.Errorf("[player_inbox] Reconcile shard owners: listing %s failed: %v; keeping the last member list, retry in %v",
				t.prefix, err, backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		backoff = backoffMin
		t.follow(ctx, rev)
	}
}

// resync 全量读前缀,整体替换成员表,返回读到的 revision。
func (t *Tracker) resync(ctx context.Context) (int64, error) {
	getCtx, cancel := context.WithTimeout(ctx, etcdOpTimeout)
	defer cancel()
	resp, err := t.cli.Get(getCtx, t.prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, err
	}
	members := make(map[string]string, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		if uuid, ok := memberOf(kv.Key, kv.Value); ok {
			members[string(kv.Key)] = uuid
		}
	}
	t.mu.Lock()
	t.members = members
	t.synced = true
	t.mu.Unlock()
	return resp.Header.Revision, nil
}

// follow 从 rev 之后 watch 前缀,逐条应用事件;watch 关闭、出错、被取消或到期都返回,由 Run 重新全量读。
func (t *Tracker) follow(ctx context.Context, rev int64) {
	watchCh, resync, stop := etcdwatch.Open(ctx, t.cli, 0, t.prefix, clientv3.WithPrefix(), clientv3.WithRev(rev+1))
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-resync:
			return
		case resp, ok := <-watchCh:
			if !ok || resp.Canceled {
				return
			}
			if err := resp.Err(); err != nil {
				logx.Errorf("[player_inbox] Reconcile shard owners: watch on %s failed: %v; re-listing", t.prefix, err)
				return
			}
			for _, ev := range resp.Events {
				if ev.Kv == nil {
					continue
				}
				if ev.Type == clientv3.EventTypeDelete {
					t.applyDelete(ev.Kv.Key)
				} else {
					t.applyPut(ev.Kv.Key, ev.Kv.Value)
				}
			}
		}
	}
}

// applyPut 记入一条注册值。值变成不算成员(例如改成 canary)时摘掉这个 key。
func (t *Tracker) applyPut(key, value []byte) {
	uuid, ok := memberOf(key, value)
	t.mu.Lock()
	defer t.mu.Unlock()
	if ok {
		t.members[string(key)] = uuid
	} else {
		delete(t.members, string(key))
	}
}

// applyDelete 摘掉一条注册值(注销或租约到期)。
func (t *Tracker) applyDelete(key []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.members, string(key))
}

// memberOf 判断一条 etcd 记录是不是一个 stable 成员,是则返回它的 node_uuid。
// 占位键(/allocated/)、解析不了的值、没有 uuid 的值、canary 与认不出的轨道都不算。
func memberOf(key, value []byte) (string, bool) {
	if strings.Contains(string(key), allocatedSegment) {
		return "", false
	}
	var info base.NodeInfo
	if err := nodeinfo.Unmarshal(value, &info); err != nil {
		return "", false
	}
	if info.GetNodeUuid() == "" {
		return "", false
	}
	track, err := releasetrack.Parse(info.GetReleaseTrack())
	if err != nil {
		logx.Errorf("[player_inbox] Reconcile shard owners: skipping %s: %v", key, err)
		return "", false
	}
	if track.IsCanary() {
		return "", false
	}
	return info.GetNodeUuid(), true
}

// sleep 可被 ctx 打断;返回 false 表示 ctx 已结束。
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

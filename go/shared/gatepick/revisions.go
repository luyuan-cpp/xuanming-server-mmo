package gatepick

import (
	"sync"
	"time"
)

// Revision 是一条 gate 注册记录在一次直读里的版本。
type Revision struct {
	Key string // etcd 键,认的是「同一条记录」
	// ModRev 是这条值的 ModRevision(etcd 每次 Put 都前进);0 = 不知道,当作刚写过,不判停写。
	ModRev int64
}

// RevisionTracker 给每次直读 etcd、没有 watch 镜像的调用方判停写(scene_manager 的跨 zone 重定向)。
// login 的 gate 目录镜像按 watch 事件判同一条规则(node_cache.go 的 changedAt),不用它。
//
// 做法:记每条记录的 ModRevision 和本进程第一次看到这个 ModRevision 的时刻;之后的读又看到同一个 ModRevision、
// 且距第一次看到已超过 StaleAfter,就判停写。gate 每 ReportPeriod 无条件重写一次记录,健康时 ModRevision 每个周期都变。
//
// 停写时长从「本进程第一次看到」起算,第一次看到不早于那次写入,所以算出来的只会偏短:
//   - 不误判健康的 gate:读到同一个 ModRevision,说明这次读的线性化点还在下一次写之前(至多一个上报周期加定时器抖动);
//     读返回的时刻比线性化点至多再晚一次直读的超时(调用方 5s)。健康 gate 的停写时长至多约 15s,StaleAfter(30s)比它大一倍。
//   - 发现得晚:第一次看到时它已经卡了多久不知道。读得稀疏时,最早在第一次看到之后、隔了超过 StaleAfter 的那次读才判出来;
//     进程刚起或某个 zone 第一次被读时一律不判,与没有这层判定时相同。
//
// 每次 Observe 用这一次读到的全量结果替换该 zone 的记录,结果里没有的键(记录已删)随之丢掉,内存与 gate 数同阶。
// 零值可用;并发安全(一把锁,临界区只有 map 操作)。不得复制。
type RevisionTracker struct {
	mu    sync.Mutex
	zones map[uint32]map[string]revisionSeen
}

// revisionSeen 是一条记录最近看到的 ModRevision,以及本进程第一次看到它的时刻。
type revisionSeen struct {
	modRev    int64
	firstSeen time.Time
}

// Observe 记下 zoneID 这一次直读到的全部记录,返回与 revs 同下标的停写判定。
// now 取这次读返回之后的时刻(生产传 time.Now(),带单调时钟读数,墙钟回拨不影响时长)。
//
// 并发的两次读可能乱序到达:看到比已记下的更旧的 ModRevision,说明这条记录之后又被写过,这次判不停写,
// 记下的保持较新的那份。乱序只会让发现变晚,不会误判。
func (t *RevisionTracker) Observe(zoneID uint32, revs []Revision, now time.Time) []bool {
	stale := make([]bool, len(revs))
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.zones[zoneID]
	next := make(map[string]revisionSeen, len(revs))
	for i, r := range revs {
		seen, had := prev[r.Key]
		switch {
		case r.ModRev == 0:
			seen = revisionSeen{firstSeen: now}
		case !had || r.ModRev > seen.modRev:
			seen = revisionSeen{modRev: r.ModRev, firstSeen: now}
		case r.ModRev == seen.modRev:
			stale[i] = now.Sub(seen.firstSeen) > StaleAfter
		}
		// 剩下的情况是 r.ModRev < seen.modRev:乱序到达的旧结果,保留较新的那份,不判停写。
		next[r.Key] = seen
	}
	if len(next) == 0 {
		delete(t.zones, zoneID)
		return stale
	}
	if t.zones == nil {
		t.zones = make(map[uint32]map[string]revisionSeen)
	}
	t.zones[zoneID] = next
	return stale
}

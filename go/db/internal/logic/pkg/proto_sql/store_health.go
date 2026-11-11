package proto_sql

import (
	"context"
	"database/sql/driver"
	"db/internal/metrics"
	"db/internal/storeerr"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// ──────────── 运行期失联的库:探活与冷却(docs/design/db-task-store-unavailable-deferral.md §3.4)────────────
//
// 打开失败的冷却(openOnce)只管还没打开过的库。已打开的库在运行期失联时,Store() 原来会照常交出缓存的句柄,
// 每条任务都去拨号、等到失败,同一子分片里落别的库的玩家一起被拖住。这里给已打开的库补上同一套
// 「冷却期内直接挡回」的机制(之后的路径全是现成的:routeTask 把它包成 errPlacementStoreUnavailable,
// 写延后且不耗重试预算),但进入冷却比打开失败更谨慎:
//
//   - 只有 SQL 调用点报来「连接断开 / 连不上」(storeerr.KindConnLost)才考虑。连接数满(1040 等)说明服务端
//     活着、池里已建立的连接照常可用,只延后那一条任务,不冷却 —— 否则一次局部故障会让整个实例停写,
//     它上面的写全量堆进共享 Redis。
//   - 报来之后先对这个库做一次有时限的探活(同一个库并发只做一次);探活本身也是「连不上」才进入冷却。
//     PingContext 优先复用池里的空闲连接,能通就说明库活着。
//   - 冷却时长带 ±20% 抖动:所有 zone、所有副本共用同一台 MySQL,同一次故障里几乎同时进入冷却,
//     不加抖动就会同时到期、同时重连。
//   - 冷却到期后只放一次探活(并发请求共享结果),成功才放行;冷却期内任何一条 SQL 成功也立即解除。

const (
	// defaultStoreProbeTimeout 是一次探活的预算。探活发生在报错的那个 worker 上,期间同一子分片的任务在等。
	defaultStoreProbeTimeout = 2 * time.Second
	// defaultStoreCooldown 是确认整库不可达之后的冷却中值,实际取值带 ±20% 抖动。
	// 短于 login 预加载的 8 秒等待:冷却期内的读任务会立即回失败回执,冷却太长会白白打掉登录窗口。
	defaultStoreCooldown = 5 * time.Second
)

// ErrStoreCoolingDown 表示这个库已被探活确认不可达,正处于冷却期。Store() 返回的错误同时包着
// ErrStoreUnavailable 与它:前者让调用方按「落点库暂不可用」处理,后者让指标与日志分得清是冷却还是打开失败。
var ErrStoreCoolingDown = errors.New("placement store is cooling down after a probe found it unreachable")

type storeCooldown struct {
	until time.Time
	cause error
}

func defaultCooldownJitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// pingStore 是生产的探活实现:用连接器新建一条连接、Ping、关掉,不经过连接池。
// 经过连接池会误判:池里的空闲连接可能早已失效(Windows 上驱动不做空闲连接活性检查),
// 池满时 PingContext 还要排队到超时。握手阶段被 1040 拒绝时驱动返回 *mysql.MySQLError,照样能识别成「服务端在应答」。
// 没有连接器时退回连接池;连接池也没有(单测里只带库名的模型)视为可用。
func pingStore(ctx context.Context, store *GameDB) error {
	if store == nil {
		return nil
	}
	if store.Connector != nil {
		conn, err := store.Connector.Connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		if pinger, ok := conn.(driver.Pinger); ok {
			return pinger.Ping(ctx)
		}
		return nil
	}
	if store.DB == nil {
		return nil
	}
	return store.DB.PingContext(ctx)
}

// ReportConnLost 由 SQL 调用点在拿到「连接断开 / 连不上」类错误后调用:对该库做一次探活,确认不可达才进入冷却。
// 已在冷却中、或这个编号还没打开过(打开路径自己有冷却)时什么都不做。
func (r *StoreRegistry) ReportConnLost(storageID uint32, cause error) {
	r.mu.RLock()
	store := r.stores[storageID]
	_, cooling := r.cooling[storageID]
	r.mu.RUnlock()
	if store == nil || cooling {
		return
	}
	r.probe(storageID, store, cause, false)
}

// ReportHealthy 由 SQL 调用点在语句成功后调用:库显然是通的,解除可能残留的冷却。
func (r *StoreRegistry) ReportHealthy(storageID uint32) {
	r.mu.RLock()
	_, cooling := r.cooling[storageID]
	r.mu.RUnlock()
	if cooling {
		r.endCooldown(storageID, "a statement on it succeeded")
	}
}

// CoolingDown 报告该库此刻是否在冷却期内(重试循环据此决定本拍是否跳过,见 key_ordered_consumer.go)。
func (r *StoreRegistry) CoolingDown(storageID uint32) bool {
	r.mu.RLock()
	c, cooling := r.cooling[storageID]
	r.mu.RUnlock()
	return cooling && r.now().Before(c.until)
}

// storeAfterCooldown 在 Store() 发现已打开的库带着冷却记录时调用:期内直接挡回;到期只放一次探活。
func (r *StoreRegistry) storeAfterCooldown(storageID uint32, store *GameDB, c storeCooldown) (*GameDB, error) {
	if r.now().Before(c.until) {
		return nil, coolingDownError(storageID, c)
	}
	if r.probe(storageID, store, c.cause, true) {
		return store, nil
	}
	r.mu.RLock()
	c = r.cooling[storageID]
	r.mu.RUnlock()
	return nil, coolingDownError(storageID, c)
}

// probe 对库做一次有时限的探活,返回库是否可用。不可达则进入(或续上)冷却;可用则解除已有的冷却。
// 同一个库的并发探活共享一次结果(与打开共用一个 SingleFlight,键加前缀区分)。
//
// atExpiry 表示调用方是在冷却到期时来放行的(storeAfterCooldown)。调用方读冷却状态与进入 SingleFlight 之间,
// 上一次探活可能刚结束,所以进来之后先按最新状态复查一次,避免多探一次、把冷却白白往后延。
func (r *StoreRegistry) probe(storageID uint32, store *GameDB, cause error, atExpiry bool) bool {
	v, _ := r.flight.Do("probe:"+strconv.FormatUint(uint64(storageID), 10), func() (any, error) {
		r.mu.RLock()
		current, cooling := r.cooling[storageID]
		r.mu.RUnlock()
		switch {
		case cooling && r.now().Before(current.until):
			return false, nil // 上一次探活刚确认不可达并续上了冷却
		case !cooling && atExpiry:
			return true, nil // 上一次探活刚确认可用并解除了冷却
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.probeTimeout)
		defer cancel()
		pingErr := r.ping(ctx, store)
		if pingErr != nil && storeerr.Classify(pingErr) == storeerr.KindConnLost {
			r.beginCooldown(storageID, cause, pingErr)
			return false, nil
		}
		if pingErr != nil {
			// 探活拿到的是服务端的应答(例如连接数满、只读):库是活的,不冷却,原任务照常按延后处理。
			logx.Infof("[placement] store answered the probe with an error, treating it as reachable: storage=%d err=%v", storageID, pingErr)
		}
		r.endCooldown(storageID, "probe succeeded")
		return true, nil
	})
	reachable, _ := v.(bool)
	return reachable
}

func (r *StoreRegistry) beginCooldown(storageID uint32, cause, probeErr error) {
	if cause == nil {
		cause = probeErr
	}
	until := r.now().Add(r.jitter(r.cooldown))
	r.mu.Lock()
	_, again := r.cooling[storageID]
	r.cooling[storageID] = storeCooldown{until: until, cause: cause}
	r.mu.Unlock()
	if again {
		logx.Errorf("[placement] store still unreachable, cooling down again until %s: storage=%d probe=%v",
			until.Format(time.RFC3339Nano), storageID, probeErr)
		return
	}
	logx.Errorf("[placement] store unreachable, deferring its tasks until %s (writes keep their retry budget): storage=%d cause=%v probe=%v",
		until.Format(time.RFC3339Nano), storageID, cause, probeErr)
}

func (r *StoreRegistry) endCooldown(storageID uint32, why string) {
	r.mu.Lock()
	if _, cooling := r.cooling[storageID]; !cooling {
		r.mu.Unlock()
		return
	}
	delete(r.cooling, storageID)
	r.mu.Unlock()
	logx.Infof("[placement] store reachable again (%s), cooldown cleared: storage=%d", why, storageID)
}

// CooldownCounts 按连接池类别数此刻仍在冷却期内的库(db_store_cooldown_stores,抓取时现算)。类别只有 metrics.PoolStore。
// 只数没到期的记录:到期之后要等下一次 Store() 探活才会删掉记录,某个库到期后再也没有流量时,
// 按记录数会让指标与告警一直挂着。
func (r *StoreRegistry) CooldownCounts() map[string]int {
	now := r.now()
	counts := map[string]int{}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.cooling {
		if now.Before(c.until) {
			counts[metrics.PoolStore]++
		}
	}
	return counts
}

func coolingDownError(storageID uint32, c storeCooldown) error {
	return fmt.Errorf("%w: %w: storage %d until %s: %v",
		ErrStoreUnavailable, ErrStoreCoolingDown, storageID, c.until.Format(time.RFC3339Nano), c.cause)
}

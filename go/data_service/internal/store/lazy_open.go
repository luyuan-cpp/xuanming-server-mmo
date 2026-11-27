package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"shared/authoritysql"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/syncx"
)

// 按需打开(docs/design/data-service-startup-isolation.md):一个逻辑库连不上时,data_service 照常启动,
// 落在健康集群上的库照常服务;连不上的库保持关闭,用到时再核对,核对通过才给连接。

// lazyOpenBackoff 是打开失败后的冷却:期间直接回上次的错误,坏库不拖住每一个请求。
const lazyOpenBackoff = 10 * time.Second

// LazyPoolRetryInterval 是常驻进程后台重核关着的池的间隔(LazyPools.RetryClosed)。比冷却长一半:失败的时刻记在核对做完时、比那一拍晚,
// 间隔与冷却同长的话每一拍都还差一点没出冷却,实际两拍才核一次。多出的 5 秒盖得住拨号超时;核对慢过 5 秒时跳过一拍。
const LazyPoolRetryInterval = lazyOpenBackoff + lazyOpenBackoff/2

// openOnce 是「第一次用到才打开」的唯一一份规则,存档库连接池(PlayerStorePools)和目录、流水库各池的首次核对(lazyPool)共用:
//   - 成功一次之后常驻,不再调 open;
//   - 失败后冷却 lazyOpenBackoff,期间直接回上次的错误;
//   - 闸门冻结是搬库窗口,秒级,库本身没有坏:不进冷却,窗口一过下一个请求就能打开;
//   - 并发的调用共用同一次 open 的结果。
//
// open 不带调用方的 ctx:结果被并发请求共享,不能随第一个调用方的取消一起失败。
type openOnce[V any] struct {
	// what 写清楚打开的是什么,进错误和日志(英文)。
	what   string
	now    func() time.Time
	flight syncx.SingleFlight

	mu       sync.RWMutex
	opened   bool
	value    V
	failedAt time.Time
	failure  error
}

func newOpenOnce[V any](what string, now func() time.Time) *openOnce[V] {
	return &openOnce[V]{what: what, now: now, flight: syncx.NewSingleFlight()}
}

// loaded 返回已经打开的值;还没打开时 ok 为假。
func (o *openOnce[V]) loaded() (value V, ok bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.value, o.opened
}

// get 返回已打开的值,必要时调 open。失败回包着原因的错误。
func (o *openOnce[V]) get(open func() (V, error)) (V, error) {
	if value, ok := o.loaded(); ok {
		return value, nil
	}
	v, err := o.flight.Do("", func() (any, error) { return o.attempt(open) })
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}

func (o *openOnce[V]) attempt(open func() (V, error)) (V, error) {
	var zero V
	o.mu.RLock()
	value, opened, failure, failedAt := o.value, o.opened, o.failure, o.failedAt
	o.mu.RUnlock()
	if opened {
		return value, nil
	}
	if failure != nil && o.now().Sub(failedAt) < lazyOpenBackoff {
		return zero, failure
	}
	value, err := open()
	if err != nil {
		wrapped := fmt.Errorf("open %s: %w", o.what, err)
		if authoritysql.IsWriterFrozen(err) {
			logx.Infof("[lazy-open] open hit a frozen writer gate, not backing off: %s", o.what)
			return zero, wrapped
		}
		o.mu.Lock()
		o.failedAt, o.failure = o.now(), wrapped
		o.mu.Unlock()
		logx.Errorf("[lazy-open] open failed, next attempt in %v: %s: %v", lazyOpenBackoff, o.what, err)
		return zero, wrapped
	}
	o.mu.Lock()
	o.opened, o.value, o.failure = true, value, nil
	o.mu.Unlock()
	return value, nil
}

// poolCheck 是一个连接池投入使用之前要过的只读核对。
type poolCheck func(db *sql.DB) error

// closedPool 是这一代没有写资格的逻辑库的池(MySQLConfig.Closed):一条连接都不给,每次借连接都回 err,
// 调用方看到的是「拿不到连接」,和集群失联走同一条路。这一代不会再有这个库的写资格,所以不重试、不冷却。
func closedPool(name string, err error) *sql.DB {
	logx.Errorf("[lazy-open] logical database %s stays closed for this boot: %v", name, err)
	return sql.OpenDB(closedConnector{err: fmt.Errorf("logical database %s: %w", name, err)})
}

type closedConnector struct{ err error }

func (c closedConnector) Connect(context.Context) (driver.Conn, error) { return nil, c.err }
func (c closedConnector) Driver() driver.Driver                        { return closedDriver(c) }

type closedDriver closedConnector

func (d closedDriver) Open(string) (driver.Conn, error) { return nil, d.err }

// lazyPool 是 openCheckedPool 在常驻进程里的那一半。句柄先建出来;verify(对同一个库再做一遍完整的当场打开与核对,用完即关)
// 挪到这个池第一条物理连接建立之前:handle 把传给它的钩子挂到新建的句柄上,没核对过的池一条连接都拿不到,
// 调用方看到的是「拿不到连接」,和集群失联走同一条路(事务按没开始算,确定没写)。verify、handle 由参数给,单测换成替身。
//
// 启动时先试一次:
//   - 通过:这个池已经打开;
//   - 连不上(authoritysql.ErrUnreachable):不算失败。这个池保持关闭,用到时再核对(openOnce 的冷却),别的库照常;
//   - 连上了而核对不过(片号对不上、表形状不对、会话不符):回错误,调用方拒启。这是确定的部署错误,
//     带着它启动,滚动发布会把好副本一个个换成坏的。
//
// registry 非空时把这个池登记进去:后台定期重核还关着的池(LazyPools.RetryClosed),就绪判定看它(LazyPools.Closed)。
func lazyPool(name string, now func() time.Time, verify func() error, handle func(authoritysql.ConnectGate) (*sql.DB, error), registry *LazyPools) (*sql.DB, error) {
	verified := newOpenOnce[struct{}]("logical database "+name, now)
	ensure := func() error {
		_, err := verified.get(func() (struct{}, error) { return struct{}{}, verify() })
		return err
	}
	db, err := handle(func(context.Context, string) (func(error), error) { return nil, ensure() })
	if err != nil {
		return nil, err
	}
	if err := ensure(); err != nil {
		if !errors.Is(err, authoritysql.ErrUnreachable) {
			_ = db.Close()
			return nil, err
		}
		logx.Errorf("[lazy-open] logical database %s is unreachable at startup: it stays closed and is verified again when it is first used or retried in the background", name)
	}
	registry.add(lazyPoolEntry{name: name, ensure: ensure, opened: func() bool {
		_, ok := verified.loaded()
		return ok
	}})
	return db, nil
}

// LazyPools 是常驻进程里按需核对的池(lazyPool)的名单。启动时连不上的池留在这里,
// 后台每隔一段时间把还关着的重核一次(RetryClosed),就绪判定看 Closed:有池还关着就不算就绪,恢复之后自己变回就绪
// (docs/design/data-service-startup-isolation.md §6.1)。nil 接收者什么都不记。并发安全。
type LazyPools struct {
	mu    sync.Mutex
	pools []lazyPoolEntry
}

type lazyPoolEntry struct {
	name   string
	ensure func() error
	opened func() bool
}

func (s *LazyPools) add(entry lazyPoolEntry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pools = append(s.pools, entry)
}

func (s *LazyPools) closedEntries() []lazyPoolEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	entries := slices.Clone(s.pools)
	s.mu.Unlock()
	return slices.DeleteFunc(entries, func(entry lazyPoolEntry) bool { return entry.opened() })
}

// Closed 返回还没核对通过的逻辑库,升序、去重(一个目录片有三个池)。
func (s *LazyPools) Closed() []string {
	var names []string
	for _, entry := range s.closedEntries() {
		names = append(names, entry.name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// RetryClosed 把还关着的池各重核一次,最多 shardFanOut 个同时。冷却期内的直接拿上次的错误,不建连;
// 所以每个关着的池每个冷却期最多拨一次号。失败只由 openOnce 记日志。
func (s *LazyPools) RetryClosed() {
	closed := s.closedEntries()
	_ = runParallel(len(closed), shardFanOut, func(i int) error {
		_ = closed[i].ensure()
		return nil
	})
}

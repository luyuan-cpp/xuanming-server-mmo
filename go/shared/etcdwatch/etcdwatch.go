// Package etcdwatch 收拢 etcd 发现与 watch 的连接层约定:防止 watch 不报错地停住。
//
// clientv3 的 watch 在两种情况下会一直不给事件、也不报错:
//   - 所连的 etcd 成员与 leader 分区:成员本身还活着,只是不再有新的 revision;
//   - 所连成员掉电或网络黑洞,留下半开连接:没有 keepalive 时,gRPC 要等很久才发现连接已死。
//
// 用 watch 维护镜像的地方都要同时做三件事(docs/design/infra-reconnect-overview.md §1.5、§4 R1):
//   - Watch 的 ctx 带 RequireLeader:成员失去 leader 时服务端取消这条 watch,调用方随之重建;
//   - etcd 客户端开 gRPC keepalive(WithKeepAlive):发现半开连接后换成员重连;
//   - watch 最多连续跑一个周期(Open 的 resync):到点返回,调用方重新全量读一次再续看。
//     前两项盖住已知的两种停法,这一项兜住其余情况,把镜像与 etcd 不一致的最长时间限在一个周期内。
//
// 这些都是连接层与发现层维持自身视图的职责,不触发业务逻辑。
package etcdwatch

import (
	"context"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	// KeepAliveTime / KeepAliveTimeout 是 etcd 客户端的 gRPC keepalive:有活跃流(watch、租约续约)时每 10s 探一次,
	// 5s 没有应答就判连接已死、换成员重连。与 login 的 etcd 客户端(login/internal/logic/pkg/etcd/client.go)同值。
	// PermitWithoutStream 保持缺省 false:空闲连接不探,不会被服务端按 too_many_pings 断开。
	KeepAliveTime    = 10 * time.Second
	KeepAliveTimeout = 5 * time.Second

	// ResyncInterval 是 watch 维护的镜像多久强制全量对账一次。RequireLeader 与 keepalive 已经在秒级盖住已知的停法,
	// 这一项只兜未知情况,所以取 5 分钟:镜像不一致最多持续这么久,而全量读和随后的日志、对账动作不至于太频繁。
	// 需要更短周期的(例如 killswitch 要在 StaleAfter 之内证明还连着 etcd)自己传间隔。
	ResyncInterval = 5 * time.Minute
)

// WithKeepAlive 给 etcd 客户端配置补上 keepalive;调用方显式配置过的不覆盖。
func WithKeepAlive(cfg clientv3.Config) clientv3.Config {
	if cfg.DialKeepAliveTime == 0 {
		cfg.DialKeepAliveTime = KeepAliveTime
	}
	if cfg.DialKeepAliveTimeout == 0 {
		cfg.DialKeepAliveTimeout = KeepAliveTimeout
	}
	return cfg
}

// Open 打开一条带 RequireLeader 的 watch,并给出到点重新全量同步的信号。
//
//   - 返回的 WatchChan 与 clientv3 的 Watch 相同;
//   - resync 在 every 之后可读,调用方读到就该返回、重新全量读一次再续看;every ≤ 0 时取 ResyncInterval;
//   - stop 取消这条 watch 的子 ctx 并停掉定时器,调用方返回前必须调用(defer stop())。clientv3 要求不再使用的
//     watch 取消它的 ctx,否则每返回一次就留下一条 watch 子流:客户端一个 goroutine 加无界缓冲,服务端一个 watcher。
func Open(ctx context.Context, w clientv3.Watcher, every time.Duration, key string, opts ...clientv3.OpOption) (watchCh clientv3.WatchChan, resync <-chan time.Time, stop func()) {
	if every <= 0 {
		every = ResyncInterval
	}
	watchCtx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
	timer := time.NewTimer(every)
	stop = func() {
		timer.Stop()
		cancel()
	}
	return w.Watch(watchCtx, key, opts...), timer.C, stop
}

package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"data_service/internal/config"
	"data_service/internal/constants"
	"data_service/internal/metrics"
	"data_service/internal/server"
	"data_service/internal/svc"
	"proto/data_service"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/data_service.yaml", "the config file")

// killSwitchEtcdDialTimeout 与 scene_manager 建 etcd 客户端时的取值一致(5s)。
// 只影响 killswitch 自己的 watch 连接,拨不通也只是退回全放行。
const killSwitchEtcdDialTimeout = 5 * time.Second

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
	svcCtx := svc.NewServiceContext(c)
	defer svcCtx.Router.Close()
	if svcCtx.SnapshotStore != nil {
		defer svcCtx.SnapshotStore.Close()
	}

	// Start Prometheus /metrics endpoint. Empty addr → no-op.
	// Surfaces per-RPC outcome / latency, lock contention, version mismatch,
	// rollback counts. See internal/metrics/metrics.go.
	metrics.Start(c.MetricsListenAddr)

	// ── 热关停(killswitch)────────────────────────────────────────────
	// data_service 是玩家权威数据的读写入口,真出事时最缺的是"秒级止血阀":
	// 往 etcd 前缀 /mmorpg/killswitch/ 下写一个 key 就能把某个方法立刻短路掉
	// (例如 RollbackAll 这种放大面极大的运维接口),而不必走一遍
	// 构建-发布-滚动更新。规则布局与优先级见 shared/killswitch 包注释。
	//
	// New 出来立刻可用(规则为空 = 全放行),Start 非阻塞、etcd 为 nil 也合法,
	// 所以整段接线不会让 data_service 的启动多出任何一个失败点。
	ks := killswitch.New(killswitch.Config{})
	ksCtx, ksCancel := context.WithCancel(context.Background())
	defer ksCancel()
	etcdCli := newKillSwitchEtcdClient(c.Etcd)
	if etcdCli != nil {
		defer etcdCli.Close()
	}
	ks.Start(ksCtx, etcdCli)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		data_service.RegisterDataServiceServer(grpcServer, server.NewDataServiceServer(svcCtx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	// 拦截器顺序是有意的(先加的在外层):
	//   grpcstats → killswitch → serverbase → handler
	// grpcstats 放最外层,被关停的请求也照样计入总量(否则一开闸就像"没人调用");
	// killswitch 命中即短路,handler 根本不会被调用;
	// serverbase 放最内层,只观测真正跑过 handler 的结果,不会把"被人为关停"
	// 误记成一次业务故障。
	s.AddUnaryInterceptors(grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor())
	s.AddUnaryInterceptors(ks.UnaryServerInterceptor())
	// in-band 故障拦截器:本服务的 handler 一律 `return resp, nil`,把失败塞进
	// 响应体的 `uint32 error_code` —— gRPC status 恒 OK,go-zero 自带的指标拦截器
	// 会把每一次"Redis 挂了""快照库写不进去"都记成一次成功请求,监控上看不出
	// 任何比例变化,直到玩家数据已经丢了才有人发现。这层把响应体里的码读出来
	// 定性:故障打日志 + 计数,业务拒绝只计数。它不改响应内容、不吞错,对调用方无感。
	//
	// 传 ErrorCodeClassifier 而**不是** TipClassifier,依据是 proto 与 handler 的
	// 事实:data_service 走的是自己的私有码表(internal/constants/error_codes.go,
	// 0..17),由 `uint32 error_code` 字段承载,与 friend/guild 那套 TipInfoMessage
	// 码表完全无关 —— 数值区间还高度重叠(本表的 1 是 Redis 失败,tip 的 1 是
	// kSuccess),混用会得到垃圾定性。serverbase 在没有 ErrorCodeClassifier 时
	// 对私有码只敢记成 VerdictBizReject,所以这份集合必须由本服务显式给出。
	//
	// 归进"服务端内部故障"的三个码,逐个都有产码点为证:
	//   ErrCodeRedis(1)          —— 玩家数据 Redis 读写失败(data_logic.go 多处)
	//   ErrCodeSnapshotDBError(11) —— 快照/流水 MySQL 出错,或 store 干脆没配起来
	//   ErrCodeRollbackFailed(12)  —— 回滚 fence 拿不到/释放函数为 nil,服务端自身走死
	// 其余非 0 码刻意**不算**故障,免得刷出满屏假告警:
	//   LockConflict(2) / VersionMismatch(3)  正常并发竞争,重试即可
	//   NotFound(4) / SnapshotNotFound(10) / ZoneNotFound(15)  查无此物
	//   PlayerOnline(13)   回滚前置条件不满足,是规则拒绝
	//   InvalidRequest(14) 调用方参数问题
	//   NotImplemented(16) 刻意的 fail-closed(fence 未配置就明确失败,不装成成功),
	//                      是确定性的配置态而非运行期故障,告警只会变噪音
	//   ResultTruncated(17) 命中数超上限的容量拒绝 —— 按 serverbase 的既定口径,
	//                      "满"属于规则拒绝不是故障
	s.AddUnaryInterceptors(serverbase.UnaryInterceptor(serverbase.Options{
		ErrorCodeClassifier: serverbase.FaultCodeSet(
			constants.ErrCodeRedis,
			constants.ErrCodeSnapshotDBError,
			constants.ErrCodeRollbackFailed,
		),
	}))
	defer s.Stop()

	fmt.Println("\n=============================================================")
	fmt.Println("  DATA_SERVICE STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:      %s\n", c.ListenOn)
	fmt.Printf("  Mode:        %s\n", c.Mode)
	if len(c.Etcd.Hosts) > 0 {
		fmt.Printf("  etcd:        %v\n", c.Etcd.Hosts)
	}
	fmt.Printf("  redis:       %s\n", c.MappingRedis.Host)
	if c.MetricsListenAddr != "" {
		fmt.Printf("  metrics:     %s/metrics\n", c.MetricsListenAddr)
	}
	fmt.Println("=============================================================")
	s.Start()
}

// newKillSwitchEtcdClient 按 RpcServerConf.Etcd 建一个只给热关停 watch 用的
// etcd 客户端。**返回 nil 是合法结果**,调用方直接把 nil 传给
// killswitch.Start 即可(它会打一条 Info 后全部放行)。
//
// 为什么不复用 go-zero 服务注册内部那个客户端:go-zero 没把它暴露出来。
//
// 为什么任何失败都只记日志不 panic:fail-open 是 killswitch 的铁律 ——
// 管控组件自身故障绝不能拖垮业务。data_service 是玩家权威数据的读写入口,
// 因为一个"止血阀连不上 etcd"就拒启,等于用小故障换一次全服数据不可用。
//
// 注意 clientv3.New 不带 WithBlock,不会在这里真的去拨号,因此也不会拖慢启动;
// 连不通的后果只是 killswitch 的全量同步失败并按 ResyncBackoff 重试(期间放行)。
func newKillSwitchEtcdClient(cfg discov.EtcdConf) *clientv3.Client {
	if len(cfg.Hosts) == 0 {
		logx.Info("[killswitch] data_service 未配置 etcd Hosts,热关停不生效(全部放行)")
		return nil
	}

	c := clientv3.Config{
		Endpoints:   cfg.Hosts,
		DialTimeout: killSwitchEtcdDialTimeout,
	}
	if cfg.HasAccount() {
		c.Username = cfg.User
		c.Password = cfg.Pass
	}

	cli, err := clientv3.New(c)
	if err != nil {
		logx.Errorf("[killswitch] data_service 建 etcd 客户端失败,热关停不生效(全部放行): %v", err)
		return nil
	}
	return cli
}

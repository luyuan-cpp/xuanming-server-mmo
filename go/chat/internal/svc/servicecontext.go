// Package svc装配同域SQL、全局原登录态、etcd及既有缓存句柄和低基数指标。
package svc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"shared/authoritysql"
	"shared/drainproof"
	"sync"
	"time"

	"chat/internal/authority"
	"chat/internal/config"

	chatpb "proto/chat"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	redv9 "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type ServiceContext struct {
	Config        config.Config
	Authority     authority.Service
	AuthorityDB   *sql.DB
	LoginSessions redv9.UniversalClient

	// 保留旧缓存配置接口；正式发送/拉取只依赖Authority，不向这些句柄回落授权或历史。
	ChatRedis   *redis.Redis
	SharedRedis *redis.Redis

	// ChatRedisTarget 是 ChatRedis 实际落点的可读描述,启动横幅打印用
	// 它不代表聊天历史权威库；历史固定在SQL。
	ChatRedisTarget string

	// Etcd 供 shared/noderegistry 注册与 shared/killswitch 监听共用 —— 连的是同一个集群,不开第二条连接。
	//
	// 注意它与 go-zero 自己的 etcd 注册无关:zrpc.RpcServerConf.HasEtcd() 的判定是
	// `len(Etcd.Hosts) > 0 && len(Etcd.Key) > 0`(go-zero v1.10.0 zrpc/config.go),
	// chat.yaml 按契约 D-13 显式写 Key: ""(不能整行省略,见 config.Validate 注释),所以 zrpc.NewServer 走 internal.NewRpcServer 而不是
	// NewRpcPubServer —— go-zero 不会往 etcd 写 chat.rpc 这类 key,etcd 里只有
	// noderegistry 按 C++ NodeInfo 约定写的 ChatNodeService.rpc/... 两把 key。
	Etcd *clientv3.Client

	WriterConfig  authoritysql.WriterConfig
	RootAdmission *drainproof.Session
	DrainRuntime  *drainproof.Runtime
	stopErr       error
	stopOnce      sync.Once
}

func NewServiceContext(c config.Config) *ServiceContext {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(err)
	}
	return NewServiceContextWithAdmission(c, writer, drainproof.New())
}

func NewServiceContextWithAdmission(c config.Config, writer authoritysql.WriterConfig, root *drainproof.Session) *ServiceContext {
	if root == nil {
		panic("聊天根门禁未装配")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, store, err := OpenAuthorityWithPermission(ctx, c, false, authoritysql.Writer(writer))
	if err != nil {
		panic("聊天权威SQL初始化失败：" + err.Error())
	}
	login, err := openLoginSessions(ctx, c)
	if err != nil {
		db.Close()
		panic(err)
	}
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		login.Close()
		db.Close()
		panic("failed to create etcd client: " + err.Error())
	}

	chatRds, sharedRds, target := NewRedisHandles(c)
	return &ServiceContext{
		Authority: store, AuthorityDB: db, LoginSessions: login,
		WriterConfig: writer, RootAdmission: root,
		Config:          c,
		ChatRedis:       chatRds,
		SharedRedis:     sharedRds,
		ChatRedisTarget: target,
		Etcd:            etcdCli,
	}
}

// NewRedisHandles 按配置建立 (ChatRedis, SharedRedis) 两个句柄,并返回 ChatRedis 落点描述。
// ChatRedis.Host 为空即未配置,私有 key 回落到共享库,两者返回同一句柄。
// 拆成独立函数是为了不依赖 etcd 就能测回落逻辑(与 match 的 NewRedisHandles 同形)。
func NewRedisHandles(c config.Config) (chatRds, sharedRds *redis.Redis, target string) {
	sharedRds = redis.MustNewRedis(c.Redis.RedisConf)
	if c.ChatRedis.Host == "" {
		logx.Infof("[chat] 旧缓存句柄沿共享配置建立；聊天授权和历史只用SQL host=%s type=%s", c.Redis.Host, c.Redis.Type)
		return sharedRds, sharedRds, fmt.Sprintf("shared-fallback host=%s type=%s", c.Redis.Host, c.Redis.Type)
	}
	chatRds = redis.MustNewRedis(c.ChatRedis)
	logx.Infof("[chat] Separate ChatRedis configuration active host=%s type=%s (shared database host=%s used only for contract key access)",
		c.ChatRedis.Host, c.ChatRedis.Type, c.Redis.Host)
	return chatRds, sharedRds, fmt.Sprintf("ChatRedis host=%s type=%s", c.ChatRedis.Host, c.ChatRedis.Type)
}

// Stop关闭本服务拥有的SQL/原登录态/etcd客户端。幂等，必须先排空RPC。
// 调用方须保证 noderegistry.Close() 与 killswitch 的 ctx 取消都**先于**它 ——
// 二者都在用这条连接,先关连接会让注销 Txn 失败、watch 撞上已关闭的 client。
func (sc *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	if runtime == nil || sc.DrainRuntime != nil || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") != nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(sc.WriterConfig.InstanceID(), sc.WriterConfig.WriterKey()); err != nil {
		return err
	}
	sc.DrainRuntime = runtime
	return nil
}

// 聊天只从同步 RPC 写 SQL；没有 Kafka/Redis 发布 worker。P1 等待该集合后即可封同一 SQL boot。
func (sc *ServiceContext) CloseSource(ctx context.Context) error {
	sc.RootAdmission.CloseAdmission()
	if err := sc.RootAdmission.Wait(ctx); err != nil {
		return err
	}
	sc.WriterConfig.Seal()
	return nil
}

func (sc *ServiceContext) PreparedFrontier(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sc.Authority == nil || sc.AuthorityDB == nil || sc.RootAdmission == nil || sc.RootAdmission.Pending() != 0 {
		return nil, drainproof.ErrHold
	}
	return sc.RootAdmission.SnapshotDigest(), nil
}

func (sc *ServiceContext) StopAndReport(ctx context.Context) error {
	if err := sc.CloseSource(ctx); err != nil {
		return err
	}
	sc.stopOnce.Do(func() {
		if sc.LoginSessions != nil {
			sc.stopErr = errors.Join(sc.stopErr, sc.LoginSessions.Close())
		}
		if sc.AuthorityDB != nil {
			sc.stopErr = errors.Join(sc.stopErr, sc.AuthorityDB.Close())
		}
		if sc.Etcd != nil {
			sc.stopErr = errors.Join(sc.stopErr, sc.Etcd.Close())
		}
	})
	return sc.stopErr
}

func (sc *ServiceContext) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sc.StopAndReport(ctx); err != nil {
		logx.Errorf("聊天关闭 HOLD: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Prometheus 指标
// ---------------------------------------------------------------------------
//
// 放在 svc 而不是 main:logic 要记 outcome,而 logic 不能 import main。
// label 只有 channel / outcome 两个有限枚举;player_id 一律只进日志(AGENTS.md §9)。

// outcome label 取值(低基数,全集就是下面这些)。
const (
	OutcomeOK                 = "ok"                  // 写入成功 / 拉取成功
	OutcomeDuplicate          = "duplicate"           // 同 request_id 重发且首发已写入,按成功回但不重复写
	OutcomeInFlight           = "in_flight"           // 保留历史指标值；SQL实现不再产生Redis pending分支
	OutcomeNoSession          = "no_session"          // 缺会话或会话 player_id=0
	OutcomeBadRequest         = "bad_request"         // 缺 message / 对端非法 / 频道枚举未知 / request_id 超长
	OutcomeChannelUnavailable = "channel_unavailable" // TEAM / SYSTEM / UNSPECIFIED
	OutcomeTooLong            = "too_long"            // 内容超 MaxContentBytes 或 trim 后为空
	OutcomeRateLimited        = "rate_limited"        // 超每秒条数
	OutcomeStorageError       = "storage_error"       // SQL/原登录态依赖故障或提交未知
)

var (
	sendTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "chat",
		Name:      "send_total",
		Help:      "SendChat requests by channel (world|private|team|system|unspecified|unknown) and outcome (ok|duplicate|in_flight|no_session|bad_request|channel_unavailable|too_long|rate_limited|storage_error).",
	}, []string{"channel", "outcome"})

	pullTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "chat",
		Name:      "pull_total",
		Help:      "PullChatHistory requests by channel (world|private|team|system|unspecified|unknown) and outcome (ok|no_session|bad_request|channel_unavailable|storage_error).",
	}, []string{"channel", "outcome"})

	registerMetricsOnce sync.Once
)

// ChannelLabel 把频道枚举压成 label。未知数值统一记 unknown:
// 枚举值来自客户端请求体,直接拿数字当 label 等于让客户端决定指标基数。
func ChannelLabel(channel chatpb.ChatChannelType) string {
	switch channel {
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD:
		return "world"
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE:
		return "private"
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_TEAM:
		return "team"
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_SYSTEM:
		return "system"
	case chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_UNSPECIFIED:
		return "unspecified"
	default:
		return "unknown"
	}
}

// ObserveSend 记录一次 SendChat 的终态。
func ObserveSend(channel string, outcome string) {
	sendTotal.WithLabelValues(channel, outcome).Inc()
}

// ObservePull 记录一次 PullChatHistory 的终态。
func ObservePull(channel string, outcome string) {
	pullTotal.WithLabelValues(channel, outcome).Inc()
}

// StartMetrics 启动 Prometheus /metrics 端点;addr 为空则关闭(与 match metrics.Start 同模式)。
// 计数器只在这里注册进默认 registry:单测里没调 StartMetrics 也能正常 Inc,不会重复注册 panic。
// 默认 registry 里同时还有 shared/killswitch、shared/serverbase、shared/safego 自己注册的指标,
// 同一个 /metrics 一并暴露。返回幂等关闭函数,由chat在RPC排空后关闭指标端口。
func StartMetrics(addr string) func() {
	if addr == "" {
		logx.Info("[chat] MetricsListenAddr empty; Prometheus /metrics endpoint disabled")
		return func() {}
	}
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(sendTotal, pullTotal)
	})
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logx.Infof("[chat] Prometheus /metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logx.Errorf("[chat] metrics HTTP server exited: %v", err)
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				logx.Errorf("[chat] Metrics drain timed out; closing connection: %v", err)
				_ = srv.Close()
			}
		})
	}
}

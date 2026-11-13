// Package domainhost 收口mail/rank两个实际进程的会话、发现和有界关闭。
package domainhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	base "proto/common/base"
	"shared/authoritysql"
	"shared/callerauth"
	"shared/dbplacement"
	"shared/drainproof"
	"shared/generated/clientrpc"
	"shared/grpcstats"
	"shared/killswitch"
	"shared/noderegistry"
	"shared/roleidentity"
	"shared/serverbase"
)

type Config struct {
	zrpc.RpcServerConf
	ZoneId            uint32             `json:"ZoneId,default=1"`
	MetricsListenAddr string             `json:"MetricsListenAddr"`
	KillSwitchPrefix  string             `json:"KillSwitchPrefix,optional"`
	AdvertiseHost     string             `json:"AdvertiseHost,optional"`
	DSNEnv            string             `json:"DSNEnv"`
	Backend           string             `json:"Backend,default=tidb"` // tidb | mysql,见 authoritysql.ParseBackend
	MaxOpenConns      int                `json:"MaxOpenConns,default=16"`
	AutoMigrate       bool               `json:"AutoMigrate,default=false"`
	TableDir          string             `json:"TableDir"`
	DataServiceRpc    zrpc.RpcClientConf `json:"DataServiceRpc"`
	// 值只是环境变量名；密钥值不得写YAML或日志。
	Producers map[string]string `json:"Producers"`
	// DbPlacement:按映射拨号(docs/design/horizontal-scale-logical-db.md §8.1),没写就固定连 DSN 的地址。
	DbPlacement dbplacement.Config `json:",optional"`
}

func (c Config) Validate() error {
	if c.Name == "" || c.ListenOn == "" || c.ZoneId == 0 || len(c.Etcd.Hosts) == 0 || c.DSNEnv == "" || c.MaxOpenConns < 2 || c.MaxOpenConns > 128 || len(c.Producers) == 0 || c.Timeout < 1000 || c.Timeout > 4000 {
		return errors.New("领域进程配置不完整或预算越界")
	}
	if _, err := authoritysql.ParseBackend(c.Backend); err != nil {
		return err
	}
	return nil
}

func (c Config) Secrets() (map[string]string, error) {
	result := make(map[string]string, len(c.Producers))
	for caller, env := range c.Producers {
		if strings.TrimSpace(env) == "" {
			return nil, errors.New("调用方密钥环境变量未配置")
		}
		secret := os.Getenv(env)
		if len([]byte(strings.TrimSpace(secret))) < 32 {
			return nil, errors.New("调用方密钥未注入或不足32字节")
		}
		result[caller] = secret
	}
	return result, nil
}

// Session 返回入口准入后的客户端会话副本(验签原件);没有会话(内部调用)时返回 nil。
// 写服务须把原修订带到同一 SQL 事务复核,不能只保留 player_id;原角色归属已由入口准入捕获,roleidentity.Original 可取。
func Session(ctx context.Context) *base.SessionDetails {
	session, _ := roleidentity.Session(ctx)
	return session
}

// Player 返回客户端来源请求的 player_id;没有会话或会话里 player_id 为 0 时返回 false。
func Player(ctx context.Context) (uint64, bool) {
	session, ok := roleidentity.Session(ctx)
	return session.GetPlayerId(), ok && session.GetPlayerId() != 0
}

// Guard 是领域进程的客户端入口:先套整请求预算,再走在线域共用的入口准入(roleidentity.EntryInterceptor,
// 按生成的客户端方法集合)。客户端方法必须带 gate 签名的完整原身份;带会话调内部控制 / S2C 方法一律 PermissionDenied;
// 不带会话的内部调用放行,由方法自己的调用方准入(serviceauth)负责。必须装在 callerauth.GateInterceptor 之后。
func Guard(budget time.Duration) grpc.UnaryServerInterceptor {
	entry, err := roleidentity.EntryInterceptor(clientrpc.Callable)
	if err != nil {
		// 只会在判定函数为 nil 或匿名例外非法时出错,这里两者都是常量:出错即代码缺陷,不能带着它起服务。
		panic(err)
	}
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return entry(ctx, request, info, next)
	}
}

// Serve先监听再注册NodeInfo；返回前注销、关闭入口并有界等待在途处理。
func Serve(ctx context.Context, c Config, nodeType base.ENodeType, register func(*grpc.Server), roots ...*drainproof.Session) error {
	return serve(ctx, c, nodeType, register, nil, roots...)
}

// ServeWithReady 在真实监听和注册成功后通知同一生命周期 owner；通知不等于开放 SOURCE。
// 回调收到本进程注册 etcd 用的 node_uuid(进程内不变,失租换号也不变):按片分工的成员表(shared/workshard)要用它认出自己。
func ServeWithReady(ctx context.Context, c Config, nodeType base.ENodeType, register func(*grpc.Server), listening func(nodeUUID string), roots ...*drainproof.Session) error {
	return serve(ctx, c, nodeType, register, listening, roots...)
}
func serve(ctx context.Context, c Config, nodeType base.ENodeType, register func(*grpc.Server), listening func(nodeUUID string), roots ...*drainproof.Session) (runErr error) {
	if len(roots) < 1 || len(roots) > 3 || roots[0] == nil {
		return errors.New("领域服务缺少显式根发行者门禁")
	}
	root := roots[0]
	var relay *drainproof.Session
	if len(roots) >= 2 {
		relay = roots[1]
	}
	var ingress []*drainproof.Session
	if len(roots) == 3 {
		ingress = roots[2:]
	}
	relayMethods := map[string]bool{}
	defer root.CloseAdmission()
	ctx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	// NodeTypes:认 type/<本服务的 eNodeType 名>/ 下的规则;注册成功后再 AddInstance(docs/design/rpc-kill-switch.md §5)。
	kill := killswitch.New(killswitch.Config{Prefix: c.KillSwitchPrefix, NodeTypes: []string{base.ENodeType_name[int32(nodeType)]}})
	traffic := grpcstats.New(grpcstats.Options{})
	defer traffic.Disable()
	// 使用仓库统一go-zero服务；NodeInfo由下面的单一注册生命周期负责，
	// 禁止框架再写一套纯endpoint的Etcd发布键。
	rpcConfig := c.RpcServerConf
	rpcConfig.Etcd.Hosts = nil
	rpcConfig.Etcd.Key = ""
	configureShutdown(&rpcConfig)
	var server *grpc.Server
	var serverMu sync.Mutex
	stopping := false
	ready := make(chan struct{})
	framework, err := zrpc.NewServer(rpcConfig, func(s *grpc.Server) {
		serverMu.Lock()
		defer serverMu.Unlock()
		server = s
		if stopping {
			go s.Stop()
		} else {
			register(s)
			// 内部签名方法承接上游已接收工作；客户端方法(生成的客户端方法集合)仍归本进程 SOURCE。
			for serviceName, serviceInfo := range s.GetServiceInfo() {
				for _, method := range serviceInfo.Methods {
					full := "/" + serviceName + "/" + method.Name
					if !clientrpc.Callable(full) {
						relayMethods[full] = true
					}
				}
			}
		}
		close(ready)
	})
	if err != nil {
		return err
	}
	gateIdentity, authErr := callerauth.GateInterceptor(os.Getenv)
	if authErr != nil {
		return authErr
	}
	framework.AddUnaryInterceptors(gateIdentity)
	framework.AddOptions(grpc.MaxRecvMsgSize(4 << 20))
	framework.AddUnaryInterceptors(serverbase.PublicationAdmission(root, relay, relayMethods, ingress...), Guard(time.Duration(c.Timeout-500)*time.Millisecond), traffic.UnaryServerInterceptor(), kill.UnaryServerInterceptor(), serverbase.UnaryInterceptor(serverbase.Options{}))
	done := make(chan error, 1)
	go func() {
		var failure error
		defer func() {
			if recover() != nil {
				failure = errors.New("领域RPC启动失败")
			}
			done <- failure
		}()
		framework.Start()
	}()
	var registration *noderegistry.Registration
	var etcd *clientv3.Client
	var healthy atomic.Bool
	defer func() {
		root.CloseAdmission()
		healthy.Store(false)
		if registration != nil {
			registration.Close()
		}
		// 即使启动回调尚未来到，也标记关闭；晚到回调不得把入口重新打开。
		serverMu.Lock()
		stopping = true
		current := server
		serverMu.Unlock()
		if current == nil {
			return
		}
		drained := make(chan struct{})
		go func() { current.GracefulStop(); close(drained) }()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			// Stop也可能等待不响应取消的handler；关闭预算不能被它再次拉长。
			go current.Stop()
			runErr = errors.Join(runErr, errors.New("领域RPC排空超过5秒，已请求强制关闭"))
		}
		if etcd != nil {
			runErr = errors.Join(runErr, etcd.Close())
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
	if c.MetricsListenAddr != "" {
		metricsListener, err := net.Listen("tcp", c.MetricsListenAddr)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096}
		go func() { _ = httpServer.Serve(metricsListener) }()
		defer func() {
			stop, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = httpServer.Shutdown(stop)
		}()
	}
	etcd, err = clientv3.New(clientv3.Config{Endpoints: c.Etcd.Hosts, DialTimeout: 3 * time.Second})
	if err != nil {
		return err
	}
	kill.Start(ctx, etcd)
	host, portText, err := net.SplitHostPort(c.ListenOn)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil {
		return err
	}
	advertise := c.AdvertiseHost
	if pod := os.Getenv("POD_IP"); pod != "" {
		advertise = pod
	}
	if advertise == "" {
		advertise = host
	}
	if advertise == "" || advertise == "0.0.0.0" || advertise == "::" {
		return errors.New("监听所有网卡时必须配置AdvertiseHost/POD_IP")
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	registration, err = noderegistry.RegisterAfterListening(startup, etcd, c.ListenOn, 3*time.Second, noderegistry.Spec{Prefix: base.ENodeType_name[int32(nodeType)] + ".rpc", NodeType: uint32(nodeType), ZoneId: c.ZoneId, LeaseTTL: 60, OnReclaimFailed: noderegistry.ReallocateNewID, BuildValue: func(id uint32, uuid string) ([]byte, error) {
		return protojson.Marshal(&base.NodeInfo{NodeId: id, NodeType: uint32(nodeType), ZoneId: c.ZoneId, NodeUuid: uuid, LaunchTime: uint64(time.Now().Unix()), ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC), Endpoint: &base.EndpointComp{Ip: advertise, Port: uint32(port)}, GrpcEndpoint: &base.EndpointComp{Ip: advertise, Port: uint32(port)}})
	}})
	if err != nil {
		return err
	}
	registration.KeepAlive()
	kill.AddInstance(registration.NodeUUID) // 认 instance/<本进程 uuid>/ 下的热关停规则
	if listening != nil {
		listening(registration.NodeUUID)
	}
	healthy.Store(true)
	fmt.Printf("%s SERVICE STARTED SUCCESSFULLY\n", strings.ToUpper(strings.TrimSuffix(c.Name, ".rpc")))
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}

// content_guard:内容安全服务(docs/design/rename-and-content-safety.md §6)。
//
// 玩家填写的文字(名字、公告、聊天)在落库或转发之前由收文字的服务送到这里判定。
// 无状态、不连数据库 / Redis / Kafka:词库在 etcd,启动时读一次,之后跟着指针热更新;
// 判定是纯内存计算,请求路径上不访问 etcd,etcd 不可用时继续用已加载的词库。
//
// 发现方式:
//   - Go 调用方(login / guild / chat)按 go-zero 的 Etcd.Key(contentguard.rpc)找;
//   - C++ scene 按 NodeInfo 前缀 ContentGuardNodeService.rpc 找。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"content_guard/internal/config"
	"content_guard/internal/engine"
	"content_guard/internal/lexstore"
	"content_guard/internal/metrics"
	"content_guard/internal/platform"
	"content_guard/internal/server"
	"content_guard/internal/wordlist"

	base "proto/common/base"
	cgpb "proto/content_guard"
	dspb "proto/data_service"

	"shared/accountdirectory"
	"shared/buildinfo"
	"shared/clockcheck"
	"shared/dsclient"
	"shared/etcdwatch"
	"shared/grpcstats"
	"shared/grpckeepalive"
	"shared/killswitch"
	sharedregistry "shared/noderegistry"
	"shared/safego"
	"shared/serviceauth"
	"shared/shutdownbudget"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/netx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"
)

var configFile = flag.String("f", "etc/content_guard.yaml", "the config file")

var showVersion = flag.Bool("version", false, "打印版本信息并退出")

const (
	nodeType = base.ENodeType_ContentGuardNodeService

	// listenProbeTimeout 是注册 NodeInfo 之前等本进程 gRPC 端口可连的上限(与路由服、chat 同值)。
	listenProbeTimeout = 30 * time.Second

	// lexiconWatchPoint 是词库监听 goroutine 在 safego 里的名字。
	lexiconWatchPoint = "content_guard.lexicon_watch"
)

func main() {
	flag.Parse()
	// 版本行直写 stdout,先于读配置与 logx 初始化:进程在配置或依赖阶段就崩溃时也留下「跑的是哪一版」。
	fmt.Println(buildinfo.StartupLine("content_guard"))
	if *showVersion {
		return
	}
	clockcheck.MustPass("content_guard")

	var c config.Config
	conf.MustLoad(*configFile, &c)
	if err := c.Validate(); err != nil {
		panic("content_guard config: " + err.Error())
	}
	shutdownbudget.Apply(&c.RpcServerConf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	etcdCli, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	}))
	if err != nil {
		panic("content_guard: failed to create the etcd client: " + err.Error())
	}
	defer etcdCli.Close()

	eng := engine.New()
	var lookup platform.IdentityLookup
	if c.HasDataServiceRpc() {
		// 查渠道身份要带调用方证明(应答里有账号与平台 openid)。接了平台却没有这把密钥,每次平台审核都会被 data_service 拒掉,
		// 名字类场景就一律回「检测不可用」:起不来比带病起来好。没接平台时用不到它。
		identitySecret := strings.TrimSpace(os.Getenv(accountdirectory.ContentGuardSecretEnv))
		if c.Platform.WeChat.AppID != "" || c.Platform.Douyin.AppID != "" {
			if _, err := serviceauth.New(map[string]string{accountdirectory.ContentGuardCaller: identitySecret}); err != nil {
				panic(fmt.Sprintf("content_guard: %s must be set (at least 32 bytes) when Platform.WeChat or Platform.Douyin is configured", accountdirectory.ContentGuardSecretEnv))
			}
		}
		dsCli := zrpc.MustNewClient(dsclient.Conf(c.DataServiceRpc))
		dsRPC := dspb.NewDataServiceClient(dsCli.Conn())
		lookup = platform.IdentityLookupFunc(func(callCtx context.Context, playerID uint64, account string) (platform.Identity, error) {
			req := &dspb.GetPlayerChannelIdentityRequest{PlayerId: playerID, Account: account}
			if err := accountdirectory.SignChannelIdentityRequest(req, accountdirectory.ContentGuardCaller, identitySecret); err != nil {
				return platform.Identity{}, fmt.Errorf("sign channel identity lookup: %w", err)
			}
			resp, rpcErr := dsRPC.GetPlayerChannelIdentity(callCtx, req)
			if rpcErr != nil {
				return platform.Identity{}, rpcErr
			}
			// 目录这一次没读成(Retry):不能当成「这名玩家没有渠道身份」,那会把平台审核悄悄跳过。
			if resp.GetRetry() {
				return platform.Identity{}, fmt.Errorf("directory identity lookup signaled retry for player %d", playerID)
			}
			if !resp.GetFound() {
				return platform.Identity{}, nil
			}
			return platform.Identity{
				Channel: resp.GetChannel(),
				OpenID:  resp.GetOpenId(),
				Account: resp.GetAccount(),
			}, nil
		})
	}
	reviewer, err := buildPlatformRouter(c.Platform, lookup)
	if err != nil {
		panic("content_guard platform reviewer: " + err.Error())
	}
	handler := server.New(eng, c.RequireLexicon, c.MaxTextBytes, server.WithReviewer(reviewer))
	metrics.Start(c.MetricsListenAddr, handler.Ready)
	publishLexiconMetrics(eng.Current())

	// 词库:启动时读一次,之后跟着 etcd 里的指针走。读不到或读到坏的都保持现有词库,只计数并打日志。
	watcher := &lexstore.Watcher{
		Client: etcdCli,
		Prefix: c.EffectiveLexiconPrefix(),
		Apply: func(pointer lexstore.Pointer, text []byte) error {
			lex, parseErr := wordlist.Parse(text)
			if parseErr != nil {
				return parseErr
			}
			report := eng.Swap(pointer.Version, lex)
			publishLexiconMetrics(eng.Current())
			logx.Infof("[lexicon] loaded version=%d deny_words=%d allow_words=%d dropped=%d duplicates=%d published_by=%q published_ms=%d",
				pointer.Version, report.DenyWords, report.AllowWords, report.Dropped, report.Duplicates, pointer.UpdatedBy, pointer.UpdatedMs)
			return nil
		},
		OnFailure: func(reason string, loadErr error) {
			metrics.IncLoadFailure(reason)
			logx.Errorf("[lexicon] load failed (reason=%s), keeping version=%d in use: %v", reason, eng.Current().Version, loadErr)
		},
	}
	safego.Go(lexiconWatchPoint, func() { watcher.Run(ctx) })

	// 要求词库的环境里,没加载到词库之前不监听、不注册。调用方按 etcd 里的注册找实例,K8s 的就绪探针摘不掉一个
	// 已经注册的副本,所以「还没准备好」只能用「不注册」来表达。新版本读不了线上词库时,滚动发布停在这一步,旧副本继续服务。
	if c.RequireLexicon {
		waitForLexicon(ctx, eng)
	}

	// 热关停(docs/design/rpc-kill-switch.md):非阻塞;连不上 etcd 就是全放行。
	kill := killswitch.New(killswitch.Config{
		Prefix:    c.KillSwitchPrefix,
		NodeTypes: []string{base.ENodeType_name[int32(nodeType)]},
	})
	kill.Start(ctx, etcdCli)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		cgpb.RegisterContentGuardServer(grpcServer, handler)
		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddOptions(grpckeepalive.ServerOption(0, 0))
	s.AddUnaryInterceptors(
		grpcstats.New(grpcstats.Options{}).UnaryServerInterceptor(),
		kill.UnaryServerInterceptor(),
	)
	defer s.Stop()

	// 先起 gRPC,再注册:调用方一看到 NodeInfo 就会拨,端口还没 accept 时注册等于公布一个会拒连的节点。
	// Start 阻塞到服务退出,所以放后台 goroutine;刻意用裸 go:Start 在监听失败时 panic,那就应该让进程死掉。
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		s.Start()
	}()

	// 写进 NodeInfo 的是对外地址而不是监听地址:K8s 上 ListenOn 是 0.0.0.0,原样注册的话 scene 拨不通。
	listenHost, port := parseListenOn(c.ListenOn)
	advertise := advertisedHost(listenHost)
	prefix := base.ENodeType_name[int32(nodeType)] + ".rpc" // 由枚举名派生,不手写
	registration, err := sharedregistry.RegisterAfterListening(context.Background(), etcdCli, c.ListenOn, listenProbeTimeout, sharedregistry.Spec{
		Prefix:          prefix,
		NodeType:        uint32(nodeType),
		ZoneId:          c.ZoneId,
		LeaseTTL:        c.LeaseTTL,
		OnReclaimFailed: sharedregistry.ReallocateNewID, // 无状态,不以 node_id 派生任何持久身份,失租后换号即可
		BuildValue: func(nodeID uint32, nodeUUID string) ([]byte, error) {
			return protojson.Marshal(&base.NodeInfo{
				NodeId:       nodeID,
				NodeType:     uint32(nodeType),
				ZoneId:       c.ZoneId,
				NodeUuid:     nodeUUID,
				LaunchTime:   uint64(time.Now().Unix()),
				ProtocolType: uint32(base.ENodeProtocolType_PROTOCOL_GRPC),
				Endpoint:     &base.EndpointComp{Ip: advertise, Port: port},
				GrpcEndpoint: &base.EndpointComp{Ip: advertise, Port: port},
			})
		},
	})
	if err != nil {
		panic("content_guard: failed to register the node in etcd: " + err.Error())
	}
	registration.KeepAlive()
	defer registration.Close()
	kill.AddInstance(registration.NodeUUID) // 认 instance/<本进程 uuid>/ 下的热关停规则

	// 「STARTED SUCCESSFULLY」是 tools/scripts/go_services.ps1 判定就绪的关键字,不要改。
	fmt.Println("\n=============================================================")
	fmt.Println("  CONTENT GUARD STARTED SUCCESSFULLY")
	fmt.Println("=============================================================")
	fmt.Printf("  Listen:          %s\n", c.ListenOn)
	fmt.Printf("  Mode:            %s\n", c.Mode)
	fmt.Printf("  etcd:            %v\n", c.Etcd.Hosts)
	fmt.Printf("  node_id:         %d (etcd CAS)\n", registration.NodeID())
	fmt.Printf("  node_uuid:       %s\n", registration.NodeUUID)
	fmt.Printf("  lexicon_prefix:  %s\n", c.EffectiveLexiconPrefix())
	fmt.Printf("  require_lexicon: %v\n", c.RequireLexicon)
	fmt.Printf("  max_text_bytes:  %d\n", c.MaxTextBytes)
	fmt.Println("=============================================================")

	<-serveDone
}

// waitForLexicon 阻塞到第一份词库加载成功(或 ctx 结束)。每 30 秒打一行日志,让「卡在等词库」在日志里看得见。
func waitForLexicon(ctx context.Context, eng *engine.Engine) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for waited := 1; eng.Current().Version == 0; waited++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if waited%30 == 0 {
			logx.Errorf("[lexicon] no lexicon loaded after %ds; not listening or registering until one is published (go/content_guard/cmd/lexicon publish)", waited)
		}
	}
	logx.Infof("[lexicon] version=%d is loaded, starting to serve", eng.Current().Version)
}

func publishLexiconMetrics(snapshot *engine.Snapshot) {
	metrics.SetLexicon(snapshot.Version, snapshot.Report.DenyWords, snapshot.Report.AllowWords, snapshot.LoadedAt)
}

// parseListenOn 把 "host:port" 拆成两部分(与路由服、match 同写法)。
func parseListenOn(listenOn string) (string, uint32) {
	parts := strings.SplitN(listenOn, ":", 2)
	host := parts[0]
	port := uint32(0)
	if len(parts) == 2 {
		fmt.Sscanf(parts[1], "%d", &port)
	}
	return host, port
}

// isUnspecifiedHost:""、0.0.0.0、:: 这类「监听所有网卡」的 host,不能原样写进 NodeInfo。
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// advertisedHost 决定写进 NodeInfo 的 IP(与路由服、chat、data_service 同一口径):
//  1. POD_IP 环境变量(K8s Downward API 注入)优先;
//  2. 否则 ListenOn 写了具体 host 就用它(本机 127.0.0.1);
//  3. 否则取本机第一个非 loopback IP;拿不到退回 127.0.0.1。
func advertisedHost(listenHost string) string {
	if podIP := os.Getenv("POD_IP"); podIP != "" {
		return podIP
	}
	if !isUnspecifiedHost(listenHost) {
		return listenHost
	}
	if ip := netx.InternalIp(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

// buildPlatformRouter 根据可选 Platform 配置组装外部平台审核路由器;未配置任何渠道时返回 nil(仅运行本地词库层)。
func buildPlatformRouter(cfg config.PlatformConfig, lookup platform.IdentityLookup) (platform.Reviewer, error) {
	var adapters []platform.Adapter
	if cfg.WeChat.AppID != "" {
		secret := strings.TrimSpace(os.Getenv(cfg.WeChat.AppSecretEnv))
		if secret == "" {
			return nil, fmt.Errorf("missing environment variable %s for Platform.WeChat", cfg.WeChat.AppSecretEnv)
		}
		adapters = append(adapters, platform.NewWeChatAdapter(cfg.WeChat.AppID, secret, cfg.WeChat.BaseURL, nil))
	}
	if cfg.Douyin.AppID != "" {
		secret := strings.TrimSpace(os.Getenv(cfg.Douyin.AppSecretEnv))
		if secret == "" {
			return nil, fmt.Errorf("missing environment variable %s for Platform.Douyin", cfg.Douyin.AppSecretEnv)
		}
		adapters = append(adapters, platform.NewDouyinAdapter(cfg.Douyin.AppID, secret, cfg.Douyin.BaseURL, nil))
	}
	if len(adapters) == 0 {
		return nil, nil
	}
	return platform.NewRouter(platform.RouterOptions{
		Lookup:        lookup,
		SyncTimeout:   time.Duration(cfg.SyncTimeoutMs) * time.Millisecond,
		WorldTimeout:  time.Duration(cfg.WorldTimeoutMs) * time.Millisecond,
		AsyncTimeout:  time.Duration(cfg.AsyncTimeoutMs) * time.Millisecond,
		LookupTimeout: time.Duration(cfg.LookupTimeoutMs) * time.Millisecond,
		Adapters:      adapters,
	}), nil
}

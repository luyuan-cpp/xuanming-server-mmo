package rpclogging

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	loginpb "proto/login"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestProtectCredentialsMergesWithoutChangingMiddlewarePolicy(t *testing.T) {
	for _, statEnabled := range []bool{false, true} {
		cfg := zrpc.RpcServerConf{Middlewares: zrpc.ServerMiddlewaresConf{
			Stat: statEnabled, Prometheus: true,
			StatConf: zrpc.StatConf{SlowThreshold: 27 * time.Millisecond, IgnoreContentMethods: []string{
				loginpb.LoginAdmin_RemovePlayersFromAccounts_FullMethodName,
				loginpb.ClientPlayerLogin_Login_FullMethodName,
			}},
		}}
		ProtectCredentials(&cfg)
		ProtectCredentials(&cfg)
		if cfg.Middlewares.Stat != statEnabled || !cfg.Middlewares.Prometheus || cfg.Middlewares.StatConf.SlowThreshold != 27*time.Millisecond {
			t.Fatal("请求体保护改变了统计、指标或慢调用策略")
		}
		if len(cfg.Middlewares.StatConf.IgnoreContentMethods) != 7 {
			t.Fatal("请求体保护丢失已有项目或重复追加")
		}
	}
}

// go-zero 的 Stat 实现位于 internal 包，使用实际 zrpc 服务验证其公开配置接缝。
// 独立子进程避免服务的全局日志/关机 hook 污染其他测试；父进程期限限定所有等待。
func TestStatInterceptorProtectsCredentialRequests(t *testing.T) {
	const childEnv = "LOGIN_RPC_LOGGING_TEST_CHILD"
	if os.Getenv(childEnv) == "1" {
		runStatInterceptorProbe(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStatInterceptorProtectsCredentialRequests$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("真实 Stat 拦截器验证失败: %v\n%s", err, output)
	}
}

func runStatInterceptorProbe(t *testing.T) {
	cfg := zrpc.RpcServerConf{
		ServiceConf: service.ServiceConf{Name: "login-redaction-test", Mode: service.TestMode},
		ListenOn:    "127.0.0.1:0",
		Middlewares: zrpc.ServerMiddlewaresConf{
			Stat: true, StatConf: zrpc.StatConf{SlowThreshold: time.Millisecond},
		},
	}
	if err := conf.FillDefault(&cfg.Log); err != nil {
		t.Fatal(err)
	}
	ProtectCredentials(&cfg)
	ready := make(chan *grpc.Server, 1)
	server, err := zrpc.NewServer(cfg, func(server *grpc.Server) {
		probe := &rpcProbeServer{}
		loginpb.RegisterClientPlayerLoginServer(server, probe)
		loginpb.RegisterLoginPreGateServer(server, probe)
		ready <- server
	})
	if err != nil {
		t.Fatal(err)
	}
	// 空 handler 在系统时钟粒度内可被测为 0；显式制造慢调用，避免以机器速度决定分支。
	// timer 仅等待 2ms，请求另有 5s 期限，父进程另有 20s 总期限。
	server.AddUnaryInterceptors(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		timer := time.NewTimer(2 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return handler(ctx, req)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	var output synchronizedLogBuffer
	previous := logx.Reset()
	logx.SetWriter(logx.NewWriter(&output))
	defer logx.SetWriter(previous)
	go server.Start()
	var grpcServer *grpc.Server
	select {
	case grpcServer = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("zrpc 服务未在期限内创建")
	}
	listener := bufconn.Listen(64 * 1024)
	defer listener.Close()
	go func() { _ = grpcServer.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///login-redaction-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	login := loginpb.NewClientPlayerLoginClient(conn)
	pregate := loginpb.NewLoginPreGateClient(conn)
	const sentinel = "SYNTHETIC_CREDENTIAL_MUST_NOT_BE_LOGGED"
	if _, err := login.Login(ctx, &loginpb.LoginRequest{Account: "probe", Password: sentinel, AuthToken: sentinel}); err != nil {
		t.Fatal(err)
	}
	if _, err := login.RefreshToken(ctx, &loginpb.RefreshTokenRequest{RefreshToken: sentinel}); err != nil {
		t.Fatal(err)
	}
	if _, err := pregate.AssignGate(ctx, &loginpb.AssignGateRequest{QueueToken: sentinel}); err != nil {
		t.Fatal(err)
	}
	if _, err := pregate.QueryQueueStatus(ctx, &loginpb.QueryQueueStatusRequest{QueueToken: sentinel}); err != nil {
		t.Fatal(err)
	}
	if _, err := pregate.SubmitRealName(ctx, &loginpb.SubmitRealNameRequest{AccessToken: sentinel, Name: sentinel, IdNumber: sentinel}); err != nil {
		t.Fatal(err)
	}
	if _, err := pregate.HandlePassportEvent(ctx, &loginpb.PassportEventRequest{Payload: []byte(sentinel)}); err != nil {
		t.Fatal(err)
	}
	const nonSensitiveName = "observable_role_name"
	if _, err := login.CreatePlayer(ctx, &loginpb.CreatePlayerRequest{Name: nonSensitiveName}); err != nil {
		t.Fatal(err)
	}
	logs := output.String()
	if strings.Contains(logs, sentinel) {
		t.Fatal("认证请求体出现在实际 Stat 日志中")
	}
	// bytes 字段(HandlePassportEvent 的 payload)经 json.Marshal 记成 base64,原文查不到也不等于没记。
	if strings.Contains(logs, base64.StdEncoding.EncodeToString([]byte(sentinel))) {
		t.Fatal("通行证回调请求体(base64)出现在实际 Stat 日志中")
	}
	for _, method := range []string{
		loginpb.ClientPlayerLogin_Login_FullMethodName,
		loginpb.ClientPlayerLogin_RefreshToken_FullMethodName,
		loginpb.LoginPreGate_AssignGate_FullMethodName,
		loginpb.LoginPreGate_QueryQueueStatus_FullMethodName,
		loginpb.LoginPreGate_SubmitRealName_FullMethodName,
		loginpb.LoginPreGate_HandlePassportEvent_FullMethodName,
	} {
		if !strings.Contains(logs, method) {
			t.Fatalf("敏感方法的慢调用日志丢失: %s", method)
		}
	}
	if !strings.Contains(logs, nonSensitiveName) || !strings.Contains(logs, "duration") {
		t.Fatal("非敏感请求内容或耗时日志丢失")
	}
	// zrpc.Stop 仅关闭全局日志，不能停止服务；本探针的所有连接随专用子进程退出回收。
}

type synchronizedLogBuffer struct {
	mu   sync.Mutex
	data strings.Builder
}

func (b *synchronizedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

type rpcProbeServer struct {
	loginpb.UnimplementedClientPlayerLoginServer
	loginpb.UnimplementedLoginPreGateServer
}

func (*rpcProbeServer) Login(context.Context, *loginpb.LoginRequest) (*loginpb.LoginResponse, error) {
	return &loginpb.LoginResponse{}, nil
}
func (*rpcProbeServer) RefreshToken(context.Context, *loginpb.RefreshTokenRequest) (*loginpb.RefreshTokenResponse, error) {
	return &loginpb.RefreshTokenResponse{}, nil
}
func (*rpcProbeServer) AssignGate(context.Context, *loginpb.AssignGateRequest) (*loginpb.AssignGateResponse, error) {
	return &loginpb.AssignGateResponse{}, nil
}
func (*rpcProbeServer) QueryQueueStatus(context.Context, *loginpb.QueryQueueStatusRequest) (*loginpb.QueryQueueStatusResponse, error) {
	return &loginpb.QueryQueueStatusResponse{}, nil
}
func (*rpcProbeServer) SubmitRealName(context.Context, *loginpb.SubmitRealNameRequest) (*loginpb.SubmitRealNameResponse, error) {
	return &loginpb.SubmitRealNameResponse{}, nil
}
func (*rpcProbeServer) HandlePassportEvent(context.Context, *loginpb.PassportEventRequest) (*loginpb.PassportEventResponse, error) {
	return &loginpb.PassportEventResponse{}, nil
}
func (*rpcProbeServer) CreatePlayer(context.Context, *loginpb.CreatePlayerRequest) (*loginpb.CreatePlayerResponse, error) {
	return &loginpb.CreatePlayerResponse{}, nil
}

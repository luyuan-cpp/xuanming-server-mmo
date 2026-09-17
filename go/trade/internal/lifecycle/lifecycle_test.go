package lifecycle

// 进程内单测:用 bufconn 上的真实 grpc.Server 验证 Shutdown 的顺序、排空与有界退出。
// 不覆盖 go-zero proc 在 Linux 收到真实 SIGTERM 后的自动停机时刻(那需要子进程 + 真信号,
// 见 go/chat/lifecycle_test.go 的 TestChatShutdownProcess);Configure 写入的推迟值由
// lifecycle_unix_test.go 钉住。

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

// testHealthServer 按 service 名决定 Check 行为:"hold" 等 release;"blocked" 不理会 ctx,只等用例清理。
type testHealthServer struct {
	healthpb.UnimplementedHealthServer
	started chan string
	release chan struct{}
	blocked chan struct{}
}

func (s *testHealthServer) Check(_ context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	switch req.GetService() {
	case "hold":
		s.started <- "hold"
		<-s.release
	case "blocked":
		s.started <- "blocked"
		<-s.blocked
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

type testServer struct {
	server      *grpc.Server
	serveDone   chan struct{}
	client      healthpb.HealthClient
	impl        *testHealthServer
	releaseOnce sync.Once
	unblockOnce sync.Once
}

func startTestServer(t *testing.T) *testServer {
	t.Helper()
	impl := &testHealthServer{
		started: make(chan string, 4),
		release: make(chan struct{}),
		blocked: make(chan struct{}),
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, impl)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.Serve(lis)
	}()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	ts := &testServer{server: srv, serveDone: serveDone, client: healthpb.NewHealthClient(conn), impl: impl}
	t.Cleanup(func() {
		// 先放行被卡住的 handler,再关连接与 server,避免用例结束后留下永久阻塞的 goroutine。
		ts.releaseHold()
		ts.unblock()
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return ts
}

func (ts *testServer) releaseHold() { ts.releaseOnce.Do(func() { close(ts.impl.release) }) }
func (ts *testServer) unblock()     { ts.unblockOnce.Do(func() { close(ts.impl.blocked) }) }

func (ts *testServer) ping(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := ts.client.Check(ctx, &healthpb.HealthCheckRequest{Service: "ping"})
	return err
}

func (ts *testServer) callAsync(service string) <-chan error {
	result := make(chan error, 1)
	go func() {
		_, err := ts.client.Check(context.Background(), &healthpb.HealthCheckRequest{Service: service})
		result <- err
	}()
	return result
}

func (ts *testServer) waitStarted(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-ts.impl.started:
		if got != want {
			t.Fatalf("开始的 handler = %q,期望 %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("handler %q 未开始", want)
	}
}

func stopAsync(s *Shutdown) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.Stop() }()
	return done
}

func waitErr(t *testing.T, ch <-chan error, timeout time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		t.Fatalf("%s 未在 %v 内返回", what, timeout)
		return nil
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("%s(等待 %v)", what, timeout)
	}
}

// 注销尚未返回时服务必须照常接请求,注销返回后才排空;收尾顺序固定为 注销 → 排空 → 资源 → 框架。
func TestShutdownUnregistersBeforeDrain(t *testing.T) {
	ts := startTestServer(t)
	unregisterStarted := make(chan struct{})
	releaseUnregister := make(chan struct{})
	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	shutdown := &Shutdown{
		Server:        ts.server,
		ServeDone:     ts.serveDone,
		DrainTimeout:  5 * time.Second,
		FinishTimeout: time.Second,
		Unregister: func() {
			close(unregisterStarted)
			<-releaseUnregister
			record("unregister")
		},
		CloseResources:  func() { record("resources") },
		FinishFramework: func() { record("framework") },
	}

	stopped := stopAsync(shutdown)
	waitClosed(t, unregisterStarted, 3*time.Second, "Unregister 未被调用")

	// 注销还没返回:路由服仍可能选中本节点,服务必须照常接请求(契约 §2"Close 先于 gRPC Stop")。
	if err := ts.ping(2 * time.Second); err != nil {
		t.Fatalf("注销期间 RPC 应继续服务: %v", err)
	}
	close(releaseUnregister)

	if err := waitErr(t, stopped, 5*time.Second, "Stop"); err != nil {
		t.Fatalf("正常收尾不应报错: %v", err)
	}
	waitClosed(t, ts.serveDone, time.Second, "Stop 返回后 Serve 应已退出")
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "unregister,resources,framework" {
		t.Fatalf("收尾顺序 = %s,期望 unregister,resources,framework", got)
	}
	if err := ts.ping(300 * time.Millisecond); err == nil {
		t.Fatal("收尾后不应再接请求")
	}
}

// 已进入 handler 的请求在排空期内完成;排空期间新请求被拒。
func TestShutdownDrainsInflightRPC(t *testing.T) {
	ts := startTestServer(t)
	inflight := ts.callAsync("hold")
	ts.waitStarted(t, "hold")
	shutdown := &Shutdown{
		Server:        ts.server,
		ServeDone:     ts.serveDone,
		DrainTimeout:  5 * time.Second,
		FinishTimeout: time.Second,
	}

	stopped := stopAsync(shutdown)

	// 新请求被拒 = 已进入真实 GracefulStop;此时在途请求仍未返回。
	deadline := time.Now().Add(3 * time.Second)
	for ts.ping(100*time.Millisecond) == nil {
		if time.Now().After(deadline) {
			t.Fatal("Stop 后迟迟没有进入排空")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-stopped:
		t.Fatalf("在途请求返回前 Stop 不应完成(err=%v)", err)
	default:
	}

	ts.releaseHold()
	if err := waitErr(t, inflight, 3*time.Second, "在途 RPC"); err != nil {
		t.Fatalf("已进入 handler 的 RPC 应成功完成: %v", err)
	}
	if err := waitErr(t, stopped, 5*time.Second, "Stop"); err != nil {
		t.Fatalf("在途请求完成后正常收尾不应报错: %v", err)
	}
}

// handler 不理会取消时,排空超时必须报错并继续关资源,Stop 在有限时间内返回,不能把退出线程锁死。
func TestShutdownBlockedHandlerStillReturns(t *testing.T) {
	ts := startTestServer(t)
	ts.callAsync("blocked")
	ts.waitStarted(t, "blocked")
	var resources atomic.Int32
	shutdown := &Shutdown{
		Server:         ts.server,
		ServeDone:      ts.serveDone,
		DrainTimeout:   150 * time.Millisecond,
		FinishTimeout:  300 * time.Millisecond,
		CloseResources: func() { resources.Add(1) },
	}

	err := waitErr(t, stopAsync(shutdown), 3*time.Second, "Stop")

	if err == nil {
		t.Fatal("排空超时必须报告错误")
	}
	if got := resources.Load(); got != 1 {
		t.Fatalf("排空超时后仍须关闭业务资源一次,实际 %d 次", got)
	}
}

// 并发触发 Stop 只收尾一次,所有调用者拿到同一个结果。
func TestShutdownConcurrentStopRunsOnce(t *testing.T) {
	ts := startTestServer(t)
	var unregisters, resources, frameworks atomic.Int32
	shutdown := &Shutdown{
		Server:          ts.server,
		ServeDone:       ts.serveDone,
		DrainTimeout:    5 * time.Second,
		FinishTimeout:   time.Second,
		Unregister:      func() { unregisters.Add(1) },
		CloseResources:  func() { resources.Add(1) },
		FinishFramework: func() { frameworks.Add(1) },
	}

	const callers = 8
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- shutdown.Stop() }()
	}
	for i := 0; i < callers; i++ {
		if err := waitErr(t, results, 5*time.Second, "并发 Stop"); err != nil {
			t.Fatalf("第 %d 个 Stop 返回错误: %v", i, err)
		}
	}
	if unregisters.Load() != 1 || resources.Load() != 1 || frameworks.Load() != 1 {
		t.Fatalf("每一步都应只执行一次:unregister=%d resources=%d framework=%d",
			unregisters.Load(), resources.Load(), frameworks.Load())
	}
}

// 启动失败路径:还没有 Server / ServeDone / 注销器时,Stop 跳过这些步骤,资源与框架收尾照常执行。
func TestShutdownWithoutServer(t *testing.T) {
	var resources, frameworks atomic.Int32
	shutdown := &Shutdown{
		DrainTimeout:    time.Second,
		FinishTimeout:   time.Second,
		CloseResources:  func() { resources.Add(1) },
		FinishFramework: func() { frameworks.Add(1) },
	}

	if err := shutdown.Stop(); err != nil {
		t.Fatalf("没有 Server 时收尾不应报错: %v", err)
	}
	if resources.Load() != 1 || frameworks.Load() != 1 {
		t.Fatalf("资源与框架收尾应各执行一次:resources=%d framework=%d", resources.Load(), frameworks.Load())
	}
}

func TestServerSlotOwnership(t *testing.T) {
	t.Run("先公布再退出", func(t *testing.T) {
		var slot ServerSlot
		server := grpc.NewServer()
		t.Cleanup(server.Stop)
		slot.Publish(server)
		if got := slot.TakeForShutdown(); got != server {
			t.Fatalf("退出应取得已经公布的 server,得到 %p,期望 %p", got, server)
		}
	})
	t.Run("先退出再公布", func(t *testing.T) {
		var slot ServerSlot
		if got := slot.TakeForShutdown(); got != nil {
			t.Fatalf("尚未公布 server,得到 %p", got)
		}
		server := grpc.NewServer()
		t.Cleanup(server.Stop)
		slot.Publish(server)
		listener := bufconn.Listen(1024)
		t.Cleanup(func() { _ = listener.Close() })
		serveResult := make(chan error, 1)
		go func() { serveResult <- server.Serve(listener) }()
		select {
		case err := <-serveResult:
			if !errors.Is(err, grpc.ErrServerStopped) {
				t.Fatalf("退出后才公布的 server 必须已停止,得到 %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("退出后才公布的 server 仍能 Serve,启动取消时会漏关")
		}
	})
}

// 正常收尾预算必须落在硬截止内;硬截止必须留在 K8s 30s 宽限期扣掉 5s preStop 之内。
func TestShutdownBudgetsFitHardTimeout(t *testing.T) {
	// shared/noderegistry.Close 最坏:等 keepalive 退出 5s + 删 key Txn 5s + Revoke 5s。
	const unregisterWorstCase = 15 * time.Second
	if total := unregisterWorstCase + DrainTimeout + FinishTimeout; total >= HardTimeout {
		t.Fatalf("注销 + 排空 + 框架收尾 = %v,必须小于 HardTimeout %v", total, HardTimeout)
	}
	if HardTimeout > 25*time.Second {
		t.Fatalf("HardTimeout %v 超出 K8s 30s 宽限期扣掉 5s preStop", HardTimeout)
	}
}

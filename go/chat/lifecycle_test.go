package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"chat/internal/config"
	"chat/internal/lifecycle"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

const lifecycleChildEnv = "CHAT_LIFECYCLE_TEST_CHILD"
const lifecycleEventPrefix = "CHAT_LIFECYCLE_EVENT "

type lifecycleEvent struct {
	Name          string   `json:"name"`
	Address       string   `json:"address,omitempty"`
	Errors        []string `json:"errors,omitempty"`
	Unregisters   int32    `json:"unregisters,omitempty"`
	ResourceStops int32    `json:"resource_stops,omitempty"`
	FrameworkEnds int32    `json:"framework_ends,omitempty"`
}

// Linux 必须使用真实信号：只验证 Stop 的模拟调用顺序，发现不了 proc 自己抢先停服。
// Windows 的 Go 进程不能通过 Process.Signal 发送 SIGTERM，改由 stdin 取消同一个退出 context。
func TestChatShutdownSlowUnregisterKeepsRPCServing(t *testing.T) {
	p, client := startLifecycleChild(t, "slow")
	p.shutdown(t)
	p.waitEvent(t, "unregister_started")

	// 这是框架固定 1s 行为的进程集成回归；闸门保证注销不会因机器快而提前完成。
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-p.done:
		t.Fatalf("注销尚未放行，子进程已经退出：%s", p.output())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: "ping"}); err != nil {
		t.Fatalf("慢注销期间 RPC 应继续服务，可能被框架提前停止：%v\n%s", err, p.output())
	}
	p.command(t, "release_unregister")
	p.assertStopped(t, false)
}

func TestChatShutdownCompletesInflightRPC(t *testing.T) {
	p, client := startLifecycleChild(t, "inflight")
	result := startLifecycleRPC(t, client, "hold")
	p.waitEvent(t, "rpc_started")
	p.shutdown(t)
	p.waitEvent(t, "unregistered")

	// 新请求被拒绝，证明已经进入真实 gRPC drain；此时再让在途请求返回。
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: "ping"})
		cancel()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("注销完成后 gRPC 未进入排空：%s", p.output())
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.command(t, "release_rpc")
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("已进入 handler 的 RPC 应成功完成：%v\n%s", err, p.output())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("已放行的在途 RPC 未完成：%s", p.output())
	}
	p.assertStopped(t, false)
}

func TestChatShutdownBlockedHandlerStillExits(t *testing.T) {
	p, client := startLifecycleChild(t, "blocked")
	startLifecycleRPC(t, client, "blocked")
	p.waitEvent(t, "rpc_started")
	p.shutdown(t)

	// handler 故意不监听 context。GracefulStop、Stop 和 zrpc 的 defer 都可能等待它；
	// 排空超时后必须报告错误并让进程退出，不能把任一等待变成同步的无限阻塞。
	p.assertStopped(t, true)
}

func TestChatShutdownConcurrentStopsCleanUpOnce(t *testing.T) {
	p, _ := startLifecycleChild(t, "repeat")
	p.shutdown(t)
	p.waitEvent(t, "unregister_started")
	p.command(t, "release_unregister")
	p.assertStopped(t, false)
}

type lifecycleHealthServer struct {
	healthpb.UnimplementedHealthServer
	release <-chan struct{}
	emit    func(lifecycleEvent)
}

func (s *lifecycleHealthServer) Check(_ context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	switch req.Service {
	case "hold":
		s.emit(lifecycleEvent{Name: "rpc_started"})
		<-s.release
	case "blocked":
		s.emit(lifecycleEvent{Name: "rpc_started"})
		select {}
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

// 仅由同一测试二进制的父进程启动；各子进程独占 proc 全局 listener，且不连接 etcd/Redis。
func TestChatShutdownProcess(t *testing.T) {
	scenario := os.Getenv(lifecycleChildEnv)
	if scenario == "" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var outputMu sync.Mutex
	emit := func(event lifecycleEvent) {
		data, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		outputMu.Lock()
		defer outputMu.Unlock()
		fmt.Fprintln(os.Stdout, lifecycleEventPrefix+string(data))
	}
	if scenario == "signal_before_run" {
		emit(lifecycleEvent{Name: "waiting_for_signal"})
		select {
		case <-proc.Done():
			emit(lifecycleEvent{Name: "framework_signal_received"})
		case <-time.After(3 * time.Second):
			t.Fatal("真实 SIGTERM 未关闭 proc.Done")
		}
		// 空配置的 ListenOn 必定无效；nil 证明退出预检发生在校验和资源创建之前。
		if err := runChat(config.Config{}); err != nil {
			t.Fatalf("已收到退出信号却继续启动：%v", err)
		}
		emit(lifecycleEvent{Name: "run_skipped"})
		return
	}
	releaseUnregister := make(chan struct{})
	releaseRPC := make(chan struct{})
	var unregisterReleaseOnce, rpcReleaseOnce sync.Once
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var command string
			if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
				panic(err)
			}
			switch command {
			case "shutdown":
				cancel()
			case "release_unregister":
				unregisterReleaseOnce.Do(func() { close(releaseUnregister) })
			case "release_rpc":
				rpcReleaseOnce.Do(func() { close(releaseRPC) })
			default:
				panic("未知生命周期测试指令：" + command)
			}
		}
	}()

	// zrpc 不接受外部 listener，只能先取得内核分配的空闲端口再交还给 zrpc。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	c := zrpc.RpcServerConf{
		ServiceConf: service.ServiceConf{
			Name: "chat-lifecycle-test", Mode: service.TestMode,
			Log: logx.LogConf{Mode: "console", Encoding: "plain", Level: "error"},
		},
		ListenOn: address,
	}
	// 保持生产框架的 24s 预算；只缩短本测试的排空与收尾等待。
	lifecycle.Configure(&c)
	serverReady := make(chan *grpc.Server, 1)
	rpcServer, err := zrpc.NewServer(c, func(server *grpc.Server) {
		healthpb.RegisterHealthServer(server, &lifecycleHealthServer{release: releaseRPC, emit: emit})
		serverReady <- server
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		rpcServer.Start()
	}()
	var unregisters, resourceStops, frameworkEnds atomic.Int32
	drainTimeout := 4 * time.Second
	if scenario == "blocked" {
		drainTimeout = 150 * time.Millisecond
	}
	shutdown := &lifecycle.Shutdown{
		Server: <-serverReady, ServeDone: serveDone,
		DrainTimeout: drainTimeout, FinishTimeout: time.Second,
		Unregister: func() {
			unregisters.Add(1)
			emit(lifecycleEvent{Name: "unregister_started"})
			if scenario == "slow" || scenario == "repeat" {
				<-releaseUnregister
			}
			emit(lifecycleEvent{Name: "unregistered"})
		},
		CloseResources: func() {
			resourceStops.Add(1)
			emit(lifecycleEvent{Name: "resources_closed"})
		},
		FinishFramework: func() {
			frameworkEnds.Add(1)
			emit(lifecycleEvent{Name: "framework_started"})
			proc.Shutdown()
		},
	}
	emit(lifecycleEvent{Name: "ready", Address: address})
	<-ctx.Done()
	callers := 1
	if scenario == "repeat" {
		callers = 8
	}
	errors := make([]string, callers+1)
	var callersDone sync.WaitGroup
	for i := 0; i < callers; i++ {
		callersDone.Add(1)
		go func() {
			defer callersDone.Done()
			if err := shutdown.Stop(); err != nil {
				errors[i] = err.Error()
			}
		}()
	}
	callersDone.Wait()
	if err := shutdown.Stop(); err != nil {
		errors[callers] = err.Error()
	}
	emit(lifecycleEvent{
		Name: "stopped", Errors: errors, Unregisters: unregisters.Load(),
		ResourceStops: resourceStops.Load(), FrameworkEnds: frameworkEnds.Load(),
	})
}

type lifecycleChild struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	events   chan lifecycleEvent
	done     chan struct{}
	waitErr  error
	pending  []lifecycleEvent
	outputMu sync.Mutex
	lines    []string
	observed []string
}

func launchLifecycleChild(t *testing.T, scenario string) *lifecycleChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestChatShutdownProcess$", "-test.timeout=14s")
	cmd.Env = append(os.Environ(), lifecycleChildEnv+"="+scenario)
	cmd.Dir = t.TempDir()
	p := &lifecycleChild{cmd: cmd, events: make(chan lifecycleEvent, 32), done: make(chan struct{})}
	p.stdin, err = cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = p
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Errorf("生命周期测试子进程未能清理：%s", p.output())
		}
	})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = p.Write([]byte(line + "\n"))
			if strings.HasPrefix(line, lifecycleEventPrefix) {
				var event lifecycleEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, lifecycleEventPrefix)), &event); err == nil {
					p.outputMu.Lock()
					p.observed = append(p.observed, event.Name)
					p.outputMu.Unlock()
					p.events <- event
				}
			}
		}
		close(p.events)
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p
}

func startLifecycleChild(t *testing.T, scenario string) (*lifecycleChild, healthpb.HealthClient) {
	t.Helper()
	p := launchLifecycleChild(t, scenario)
	ready := p.waitEvent(t, "ready")
	conn, err := grpc.NewClient(ready.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := healthpb.NewHealthClient(conn)
	callCtx, callCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer callCancel()
	if _, err = client.Check(callCtx, &healthpb.HealthCheckRequest{Service: "ping"}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("隔离 gRPC 未就绪：%v\n%s", err, p.output())
	}
	return p, client
}

func (p *lifecycleChild) Write(data []byte) (int, error) {
	p.outputMu.Lock()
	defer p.outputMu.Unlock()
	p.lines = append(p.lines, string(data))
	return len(data), nil
}

func (p *lifecycleChild) output() string {
	p.outputMu.Lock()
	defer p.outputMu.Unlock()
	return strings.Join(p.lines, "")
}

func (p *lifecycleChild) command(t *testing.T, command string) {
	t.Helper()
	if err := json.NewEncoder(p.stdin).Encode(command); err != nil {
		t.Fatalf("发送子进程指令 %q 失败：%v\n%s", command, err, p.output())
	}
}

func (p *lifecycleChild) shutdown(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		p.command(t, "shutdown")
		return
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("向隔离子进程发送 SIGTERM 失败：%v", err)
	}
}

func (p *lifecycleChild) waitEvent(t *testing.T, name string) lifecycleEvent {
	t.Helper()
	for i, event := range p.pending {
		if event.Name == name {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			return event
		}
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-p.events:
			if !ok {
				t.Fatalf("等到 %s 前子进程已关闭输出：%s", name, p.output())
			}
			if event.Name == name {
				return event
			}
			p.pending = append(p.pending, event)
		case <-timer.C:
			t.Fatalf("等待子进程事件 %s 超时：%s", name, p.output())
		}
	}
}

func (p *lifecycleChild) assertStopped(t *testing.T, wantError bool) {
	t.Helper()
	event := p.waitEvent(t, "stopped")
	p.outputMu.Lock()
	observed := append([]string(nil), p.observed...)
	p.outputMu.Unlock()
	stage := 0
	wantStages := []string{"unregistered", "resources_closed", "framework_started", "stopped"}
	for _, name := range observed {
		for index, want := range wantStages {
			if name == want {
				if index != stage {
					t.Fatalf("停机阶段顺序不符，得到 %v，期望 %v", observed, wantStages)
				}
				stage++
			}
		}
	}
	if stage != len(wantStages) {
		t.Fatalf("停机阶段缺失：%v", observed)
	}
	if len(event.Errors) < 2 || (event.Errors[0] != "") != wantError {
		t.Fatalf("停止结果不符，期望有错误=%v，得到 %+v\n%s", wantError, event, p.output())
	}
	for _, err := range event.Errors[1:] {
		if err != event.Errors[0] {
			t.Fatalf("重复 Stop 的结果不一致：%+v", event.Errors)
		}
	}
	if event.Unregisters != 1 || event.ResourceStops != 1 || event.FrameworkEnds != 1 {
		t.Fatalf("注销、资源关闭和框架收尾应各执行一次，得到 %+v\n%s", event, p.output())
	}
	select {
	case <-p.done:
		if p.waitErr != nil {
			t.Fatalf("生命周期测试子进程异常退出：%v\n%s", p.waitErr, p.output())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Stop 返回后进程仍未退出，可能卡在 zrpc 的 defer：%s", p.output())
	}
}

func startLifecycleRPC(t *testing.T, client healthpb.HealthClient, name string) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		_, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: name})
		result <- err
	}()
	return result
}

// 启动被取消时，zrpc 的注册回调可能稍后才公布 server；必须仍能收回其所有权。
func TestChatShutdownServerSlotOwnership(t *testing.T) {
	t.Run("先公布再退出", func(t *testing.T) {
		var slot lifecycle.ServerSlot
		server := grpc.NewServer()
		t.Cleanup(server.Stop)
		slot.Publish(server)
		if got := slot.TakeForShutdown(); got != server {
			t.Fatalf("退出应取得已经公布的 server，得到 %p，期望 %p", got, server)
		}
	})
	t.Run("先退出再公布", func(t *testing.T) {
		var slot lifecycle.ServerSlot
		if got := slot.TakeForShutdown(); got != nil {
			t.Fatalf("尚未公布 server，得到 %p", got)
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
				t.Fatalf("退出后才公布的 server 必须已停止，得到 %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("退出后才公布的 server 仍能 Serve，启动取消时会漏关")
		}
	})
}

// proc 在 init 中订阅信号，runChat 必须识别创建自身 context 之前已发生的退出。
func TestChatShutdownSignalBeforeRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 proc.Done 为 nil，不存在框架先接收 SIGTERM 的路径")
	}
	p := launchLifecycleChild(t, "signal_before_run")
	p.waitEvent(t, "waiting_for_signal")
	p.shutdown(t)
	p.waitEvent(t, "framework_signal_received")
	p.waitEvent(t, "run_skipped")
	select {
	case <-p.done:
		if p.waitErr != nil {
			t.Fatalf("先信号后启动的子进程异常退出：%v\n%s", p.waitErr, p.output())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("已取消的 runChat 返回后进程仍未退出：%s", p.output())
	}
}

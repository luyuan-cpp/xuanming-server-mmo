package grpckeepalive

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TCP_USER_TIMEOUT 只有 Linux 有,生产也跑在 Linux 上;文件名后缀让这组用例只在 Linux 编译。

// userTimeoutAfterOneCall 起一个本机 grpc 服务端,按 opts 拨号调一次,回客户端那条 TCP 连接上的 TCP_USER_TIMEOUT(毫秒)。
func userTimeoutAfterOneCall(t *testing.T, opts ...grpc.DialOption) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	healthpb.RegisterHealthServer(server, health.NewServer())
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conns := make(chan net.Conn, 1)
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err == nil {
			select {
			case conns <- conn:
			default:
			}
		}
		return conn, err
	}
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer))
	client, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = healthpb.NewHealthClient(client).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}

	raw, err := (<-conns).(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var timeoutMs int
	var readErr error
	if err = raw.Control(func(fd uintptr) {
		timeoutMs, readErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return timeoutMs
}

// 修复前 match→rank、rank→mail、data_service→角色护栏参与者的拨号都是这样:没有 TCP_USER_TIMEOUT,
// 对端整机消失后已发出的数据一直重传,连接要约 15 分钟才断。
func TestWithoutKeepaliveUnacknowledgedDataIsNotBounded(t *testing.T) {
	if got := userTimeoutAfterOneCall(t); got != 0 {
		t.Fatalf("TCP_USER_TIMEOUT = %dms without keepalive, want 0 (kernel retransmission default)", got)
	}
}

// 带上 DialOption,grpc 把 TCP_USER_TIMEOUT 设成 ackTimeout:半开连接在一次调用之后 ackTimeout 内断开重连。
func TestDialOptionBoundsUnacknowledgedDataByAckTimeout(t *testing.T) {
	if got, want := userTimeoutAfterOneCall(t, DialOption()), int(ackTimeout/time.Millisecond); got != want {
		t.Fatalf("TCP_USER_TIMEOUT = %dms, want %dms", got, want)
	}
}

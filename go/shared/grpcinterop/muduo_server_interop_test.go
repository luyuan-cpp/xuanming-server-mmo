package grpcinterop

// grpc-go 客户端对 C++ muduo_grpc::Server 的互通用例(docs/design/grpc-muduo-transport.md §15.14 第 6 步)。
//
// 默认跳过。跑法:
//  1. 后台启动 C++ 侧服务:设 MUDUO_GRPC_SERVER_INTEROP_PORT=50999,
//     执行 muduo_grpc_test.exe --gtest_filter=ServerInterop.*(cpp/tests/muduo_grpc_test/server_interop_test.cpp);
//  2. 在 go/shared 下:MUDUO_GRPC_SERVER_INTEROP_ADDR=127.0.0.1:50999 go test ./grpcinterop/ -run TestMuduoServerInterop -count=1 -v
//
// 不依赖任何 .proto:请求与应答都是原始字节(rawCodec)。C++ 侧的服务 /interop.Interop/<M>:
// Echo 原样回;Fail 请求 "<code>|<message>" 以该状态完成;Defer 请求 "<ms>" 延后回 "deferred:<ms>";
// Metadata 回 "<key>=<value>\n" 列表;Shutdown 回 "bye" 后开始优雅关机。
// 子用例按顺序执行,Shutdown 必须是最后一个:它让 C++ 进程排空退出。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"shared/tracing"
)

const (
	serverInteropAddrEnv = "MUDUO_GRPC_SERVER_INTEROP_ADDR"

	interopEcho     = "/interop.Interop/Echo"
	interopFail     = "/interop.Interop/Fail"
	interopDefer    = "/interop.Interop/Defer"
	interopMetadata = "/interop.Interop/Metadata"
	interopShutdown = "/interop.Interop/Shutdown"

	// C++ 服务端的接收上限(ServerSessionOptions.maxReceiveMessageBytes,与 grpc-go 服务端默认一致)。
	serverMaxReceiveBytes = 4 * 1024 * 1024
)

// rawCodec 把请求与应答当原始字节收发。Name 取 "proto":线上 content-type 为 application/grpc+proto,
// 与生产上 Go 调用方的 proto 编解码器同一类;服务端接受 application/grpc 后接 '+' 的形式。
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: unexpected message type %T", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: unexpected message type %T", v)
	}
	// data 在返回后可能被 grpc 复用,必须拷贝。
	*b = append((*b)[:0], data...)
	return nil
}

func (rawCodec) Name() string { return "proto" }

func invokeRaw(ctx context.Context, conn *grpc.ClientConn, method string, request []byte) ([]byte, error) {
	req := request
	var resp []byte
	if err := conn.Invoke(ctx, method, &req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func invokeWithTimeout(conn *grpc.ClientConn, timeout time.Duration, method string, request []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return invokeRaw(ctx, conn, method, request)
}

func requireStatus(t *testing.T, err error, want codes.Code, wantMessage string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != want || st.Message() != wantMessage {
		t.Fatalf("status = %v %q, want %v %q", st.Code(), st.Message(), want, wantMessage)
	}
}

func TestMuduoServerInterop(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv(serverInteropAddrEnv))
	if addr == "" {
		t.Skipf("set %s=ip:port (with muduo_grpc_test --gtest_filter=ServerInterop.* serving it) to run", serverInteropAddrEnv)
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
	if err != nil {
		t.Fatalf("grpc.NewClient(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	t.Run("Echo", func(t *testing.T) {
		resp, err := invokeWithTimeout(conn, 5*time.Second, interopEcho, []byte("hello muduo"))
		if err != nil {
			t.Fatalf("Echo: %v", err)
		}
		if string(resp) != "hello muduo" {
			t.Fatalf("Echo = %q", resp)
		}
	})

	t.Run("EmptyMessage", func(t *testing.T) {
		resp, err := invokeWithTimeout(conn, 5*time.Second, interopEcho, nil)
		if err != nil {
			t.Fatalf("Echo(empty): %v", err)
		}
		if len(resp) != 0 {
			t.Fatalf("Echo(empty) returned %d bytes", len(resp))
		}
	})

	t.Run("LargeMessage", func(t *testing.T) {
		payload := make([]byte, 3*1024*1024)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		resp, err := invokeWithTimeout(conn, 10*time.Second, interopEcho, payload)
		if err != nil {
			t.Fatalf("Echo(3MiB): %v", err)
		}
		if !bytes.Equal(resp, payload) {
			t.Fatalf("Echo(3MiB) returned %d bytes, not the request", len(resp))
		}
	})

	t.Run("OversizedRequestIsResourceExhausted", func(t *testing.T) {
		payload := make([]byte, serverMaxReceiveBytes+1)
		_, err := invokeWithTimeout(conn, 10*time.Second, interopEcho, payload)
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.ResourceExhausted {
			t.Fatalf("oversized request: err = %v, want ResourceExhausted", err)
		}
		want := fmt.Sprintf("received message larger than max (%d vs. %d)", serverMaxReceiveBytes+1, serverMaxReceiveBytes)
		if st.Message() != want {
			t.Fatalf("oversized request message = %q, want %q", st.Message(), want)
		}
	})

	t.Run("StatusCodeAndMessageRoundTrip", func(t *testing.T) {
		cases := []struct {
			code    codes.Code
			message string
		}{
			// battle / scene 守护段的跨语言契约文案,go/match 按状态码加文案精确匹配。
			{codes.Unavailable, "battle_not_allocatable"},
			{codes.Unavailable, "agones allocate not confirmed"},
			{codes.FailedPrecondition, "placement_operation_mismatch"},
			// grpc-message 百分号编码往返:'%'、非 ASCII、控制字符。
			{codes.Internal, "50% done: \u4e2d\u6587\ttab"},
		}
		for _, c := range cases {
			_, err := invokeWithTimeout(conn, 5*time.Second, interopFail, []byte(fmt.Sprintf("%d|%s", uint32(c.code), c.message)))
			requireStatus(t, err, c.code, c.message)
		}
	})

	t.Run("UnknownMethodAndService", func(t *testing.T) {
		_, err := invokeWithTimeout(conn, 5*time.Second, "/interop.Interop/Nope", []byte("x"))
		requireStatus(t, err, codes.Unimplemented, "unknown method Nope for service interop.Interop")
		_, err = invokeWithTimeout(conn, 5*time.Second, "/nope.Svc/M", []byte("x"))
		requireStatus(t, err, codes.Unimplemented, "unknown service nope.Svc")
	})

	t.Run("Metadata", func(t *testing.T) {
		binary := string([]byte{0x00, 0x01, 0x02, 0xff})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = metadata.AppendToOutgoingContext(ctx, "x-trace-id", "abc", "x-session-detail-bin", binary)
		resp, err := invokeRaw(ctx, conn, interopMetadata, nil)
		if err != nil {
			t.Fatalf("Metadata: %v", err)
		}
		// grpc-go 按 map 顺序写自定义头,这里只比集合。
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSuffix(string(resp), "\n"), "\n") {
			if line == "" {
				continue
			}
			kv := strings.SplitN(line, "=", 2)
			if len(kv) != 2 {
				t.Fatalf("malformed metadata line %q", line)
			}
			got[kv[0]] = kv[1]
		}
		if got["x-trace-id"] != "abc" {
			t.Fatalf("x-trace-id = %q (all: %q)", got["x-trace-id"], got)
		}
		if got["x-session-detail-bin"] != binary {
			t.Fatalf("x-session-detail-bin = %x, want %x: the server must decode exactly the wire base64 layer",
				got["x-session-detail-bin"], binary)
		}
		for key := range got {
			if strings.HasPrefix(key, "grpc-") || key == "user-agent" || key == "content-type" || key == "te" {
				t.Fatalf("reserved header %q leaked into metadata", key)
			}
		}
	})

	// 分布式日志(docs/design/distributed-tracing.md §5.3、§11):Go 侧 tracing.UnaryClientInterceptor 写的 traceparent
	// 经真实的 HTTP/2 帧到达 muduo_grpc 服务端的 metadata,值与 Go 侧写的逐字相同。C++ 侧把 metadata 解析成 trace
	// 由 grpc_client_loopback_test 覆盖,两段合起来就是跨语言的整条传播。
	t.Run("TraceparentFromTheGoInterceptorReachesServerMetadata", func(t *testing.T) {
		previousProvider := otel.GetTracerProvider()
		previousPropagator := otel.GetTextMapPropagator()
		otel.SetTracerProvider(sdktrace.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.TraceContext{})
		defer func() {
			otel.SetTracerProvider(previousProvider)
			otel.SetTextMapPropagator(previousPropagator)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx, caller := otel.Tracer("interop").Start(ctx, "caller")
		defer caller.End()

		var resp []byte
		invoker := func(ctx context.Context, method string, _, _ any, cc *grpc.ClientConn, _ ...grpc.CallOption) error {
			var err error
			resp, err = invokeRaw(ctx, cc, method, nil)
			return err
		}
		if err := tracing.UnaryClientInterceptor()(ctx, interopMetadata, nil, nil, conn, invoker); err != nil {
			t.Fatalf("Metadata through the tracing interceptor: %v", err)
		}
		var traceparent string
		for _, line := range strings.Split(strings.TrimSuffix(string(resp), "\n"), "\n") {
			if value, ok := strings.CutPrefix(line, "traceparent="); ok {
				traceparent = value
			}
		}
		want := caller.SpanContext().TraceID().String()
		if len(traceparent) != 55 || !strings.HasPrefix(traceparent, "00-"+want+"-") {
			t.Fatalf("server saw traceparent %q, want a W3C value carrying trace %s", traceparent, want)
		}
		if strings.Contains(traceparent, caller.SpanContext().SpanID().String()) {
			t.Fatalf("traceparent %q should name the interceptor's client span, not the caller span", traceparent)
		}
	})

	t.Run("DeferredCompletion", func(t *testing.T) {
		resp, err := invokeWithTimeout(conn, 5*time.Second, interopDefer, []byte("200"))
		if err != nil {
			t.Fatalf("Defer(200): %v", err)
		}
		if string(resp) != "deferred:200" {
			t.Fatalf("Defer(200) = %q", resp)
		}
	})

	t.Run("DeadlineBeforeDeferredCompletion", func(t *testing.T) {
		_, err := invokeWithTimeout(conn, 300*time.Millisecond, interopDefer, []byte("1500"))
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("Defer(1500) with a 300ms deadline: err = %v, want DeadlineExceeded", err)
		}
	})

	t.Run("Concurrent300", func(t *testing.T) {
		// 超过服务端每连接 SETTINGS_MAX_CONCURRENT_STREAMS(256):多出的流在 grpc-go 客户端排队,全部应成功。
		const n = 300
		var wg sync.WaitGroup
		var mu sync.Mutex
		var failures []string
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				want := fmt.Sprintf("concurrent-%d", i)
				resp, err := invokeWithTimeout(conn, 15*time.Second, interopEcho, []byte(want))
				if err == nil && string(resp) == want {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				failures = append(failures, fmt.Sprintf("#%d: resp=%q err=%v", i, resp, err))
			}(i)
		}
		wg.Wait()
		if len(failures) > 0 {
			t.Fatalf("%d of %d concurrent calls failed, first: %s", len(failures), n, failures[0])
		}
	})

	// 必须最后:让 C++ 进程排空退出。
	t.Run("ShutdownDrainsInFlightCallsAndRefusesNewConnections", func(t *testing.T) {
		type result struct {
			resp []byte
			err  error
		}
		deferred := make(chan result, 1)
		go func() {
			resp, err := invokeWithTimeout(conn, 10*time.Second, interopDefer, []byte("800"))
			deferred <- result{resp: resp, err: err}
		}()
		time.Sleep(200 * time.Millisecond) // 让 Defer 先被派发、业务已开始

		resp, err := invokeWithTimeout(conn, 5*time.Second, interopShutdown, nil)
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if string(resp) != "bye" {
			t.Fatalf("Shutdown = %q", resp)
		}

		select {
		case r := <-deferred:
			if r.err != nil {
				t.Fatalf("in-flight Defer(800) failed during graceful shutdown: %v", r.err)
			}
			if string(r.resp) != "deferred:800" {
				t.Fatalf("in-flight Defer(800) = %q", r.resp)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("in-flight Defer(800) did not complete during graceful shutdown")
		}

		// 第一段 GOAWAY 之前发出的调用可能仍在旧连接上成功;之后 grpc-go 换新连接,监听已关 → Unavailable。
		giveUp := time.Now().Add(15 * time.Second)
		for {
			_, err := invokeWithTimeout(conn, 5*time.Second, interopEcho, []byte("after shutdown"))
			if err != nil {
				if status.Code(err) != codes.Unavailable {
					t.Fatalf("call after shutdown: err = %v, want Unavailable", err)
				}
				break
			}
			if time.Now().After(giveUp) {
				t.Fatalf("calls still succeed 15s after Shutdown")
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

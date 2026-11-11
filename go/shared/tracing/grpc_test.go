package tracing

import (
	"context"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// 用真实的 SDK TracerProvider 与 W3C 传播器,结束时恢复全局状态,不影响同进程的其他用例。
func installTestTracing(t *testing.T) {
	t.Helper()
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})
}

// captureOutgoing 返回一个记录出站 metadata 的 invoker。
func captureOutgoing(sent *metadata.MD) grpc.UnaryInvoker {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		*sent, _ = metadata.FromOutgoingContext(ctx)
		return nil
	}
}

func TestUnaryClientInterceptorInjectsTraceparentAndKeepsExistingMetadata(t *testing.T) {
	installTestTracing(t)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()
	// 模拟 callerauth 已经写好的签名头:拦截器不得冲掉它。
	ctx = metadata.AppendToOutgoingContext(ctx, "x-caller-signature", "sig")

	var sent metadata.MD
	if err := UnaryClientInterceptor()(ctx, "/scene.SceneNodeGrpc/CreateScene", nil, nil, nil, captureOutgoing(&sent)); err != nil {
		t.Fatalf("interceptor returned %v", err)
	}
	if got := sent.Get("x-caller-signature"); len(got) != 1 || got[0] != "sig" {
		t.Fatalf("existing outgoing metadata lost: %v", sent)
	}
	headers := sent.Get("traceparent")
	if len(headers) != 1 {
		t.Fatalf("want exactly one traceparent, got %v", headers)
	}
	if want := parent.SpanContext().TraceID().String(); !strings.Contains(headers[0], want) {
		t.Fatalf("traceparent %q does not carry the caller trace %s", headers[0], want)
	}
	if strings.Contains(headers[0], parent.SpanContext().SpanID().String()) {
		t.Fatalf("traceparent %q should name the client span, not the caller span", headers[0])
	}
}

func TestUnaryClientInterceptorStartsARootWithoutCallerSpan(t *testing.T) {
	installTestTracing(t)
	var sent metadata.MD
	if err := UnaryClientInterceptor()(context.Background(), "/x.Y/Z", nil, nil, nil, captureOutgoing(&sent)); err != nil {
		t.Fatalf("interceptor returned %v", err)
	}
	headers := sent.Get("traceparent")
	if len(headers) != 1 || len(headers[0]) != 55 || !strings.HasPrefix(headers[0], "00-") {
		t.Fatalf("want one W3C traceparent, got %v", headers)
	}
}

type playerRequest struct{ playerID uint64 }

func (r playerRequest) GetPlayerId() uint64 { return r.playerID }

func logOnce(ctx context.Context, _ any) (any, error) {
	logx.WithContext(ctx).Info("handled")
	return nil, nil
}

func TestPlayerFieldInterceptorAddsPlayerIDToContextLogs(t *testing.T) {
	collector := logtest.NewCollector(t)
	_, err := PlayerFieldInterceptor()(context.Background(), playerRequest{playerID: 42}, &grpc.UnaryServerInfo{}, logOnce)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(collector.String(), `"player_id":"42"`) {
		t.Fatalf("log line misses player_id: %s", collector.String())
	}
}

// 2^62+1 按 float64 会变成 2^62:写成字符串才保住末位,Alloy 取到的才是这个玩家。
func TestPlayerFieldInterceptorWritesLargeIDsExactly(t *testing.T) {
	collector := logtest.NewCollector(t)
	const largeID = uint64(1)<<62 + 1
	_, err := PlayerFieldInterceptor()(context.Background(), playerRequest{playerID: largeID}, &grpc.UnaryServerInfo{}, logOnce)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(collector.String(), `"player_id":"4611686018427387905"`) {
		t.Fatalf("log line must carry the exact id as a string: %s", collector.String())
	}
}

func TestPlayerFieldInterceptorSkipsZeroAndRequestsWithoutPlayer(t *testing.T) {
	collector := logtest.NewCollector(t)
	for _, req := range []any{playerRequest{playerID: 0}, struct{}{}} {
		collector.Reset()
		if _, err := PlayerFieldInterceptor()(context.Background(), req, &grpc.UnaryServerInfo{}, logOnce); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(collector.String(), "player_id") {
			t.Fatalf("request %#v must not add player_id: %s", req, collector.String())
		}
	}
}

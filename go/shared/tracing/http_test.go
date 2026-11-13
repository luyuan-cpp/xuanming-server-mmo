package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

// recordingServer 把每次请求的请求头经通道交给测试(通道收发之间有先后保证,-race 下不报竞争),应答固定状态码。
func recordingServer(t *testing.T, status int) (*httptest.Server, <-chan http.Header) {
	t.Helper()
	received := make(chan http.Header, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, received
}

func TestDoHTTPInjectsTheClientSpanAndKeepsExistingHeaders(t *testing.T) {
	installTestTracing(t)
	server, received := recordingServer(t, http.StatusOK)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	defer parent.End()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/orders", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// 模拟商城签名头:注入不得改动已有的头。
	request.Header.Set("X-Commerce-Signature", "sig")
	response, err := DoHTTP(server.Client(), request)
	if err != nil {
		t.Fatalf("DoHTTP returned %v", err)
	}
	response.Body.Close()

	headers := <-received
	if got := headers.Get("X-Commerce-Signature"); got != "sig" {
		t.Fatalf("existing header lost: %q", got)
	}
	traceparent := headers.Get("traceparent")
	if want := parent.SpanContext().TraceID().String(); !strings.Contains(traceparent, want) {
		t.Fatalf("traceparent %q does not carry the caller trace %s", traceparent, want)
	}
	if strings.Contains(traceparent, parent.SpanContext().SpanID().String()) {
		t.Fatalf("traceparent %q should name the client span, not the caller span", traceparent)
	}
}

func TestDoHTTPStartsARootWithoutCallerSpan(t *testing.T) {
	installTestTracing(t)
	server, received := recordingServer(t, http.StatusOK)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/skus", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	response, err := DoHTTP(server.Client(), request)
	if err != nil {
		t.Fatalf("DoHTTP returned %v", err)
	}
	response.Body.Close()
	traceparent := (<-received).Get("traceparent")
	if len(traceparent) != 55 || !strings.HasPrefix(traceparent, "00-") {
		t.Fatalf("want one W3C traceparent, got %q", traceparent)
	}
}

func TestDoHTTPReturnsTheResponseAndErrorUnchanged(t *testing.T) {
	installTestTracing(t)
	server, _ := recordingServer(t, http.StatusServiceUnavailable)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	response, err := DoHTTP(server.Client(), request)
	if err != nil {
		t.Fatalf("DoHTTP returned %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 passed through", response.StatusCode)
	}

	server.Close()
	request, err = http.NewRequest(http.MethodGet, server.URL+"/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if response, err = DoHTTP(server.Client(), request); err == nil {
		response.Body.Close()
		t.Fatal("DoHTTP to a closed server returned no error")
	}
}

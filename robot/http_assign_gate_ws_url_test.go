package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"robot/pkg"
)

// 放行回包里的 gate_ws_url 是客户端可见契约的键名(网关 DTO 按 SNAKE_CASE 输出):解出来并登记进地址簿,
// WebSocket 传输按 ip:port 查它(pkg/transport.go)。排队中的回包不动地址簿;放行回包没带地址时清掉旧记录。
func TestPostJSON_RecordsTheGateWebSocketAddress(t *testing.T) {
	const wsURL = "wss://ws.example.test/ws/10.244.0.5/18000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admitted":
			_, _ = io.WriteString(w, `{"code":0,"gate_ip":"203.0.113.9","gate_port":30000,"gate_ws_url":"`+wsURL+`"}`)
		case "/queued":
			_, _ = io.WriteString(w, `{"code":100,"retry_after_ms":1000}`)
		default: // 放行,但这台 gate 没有自报 WebSocket 地址
			_, _ = io.WriteString(w, `{"code":0,"gate_ip":"203.0.113.9","gate_port":30000}`)
		}
	}))
	defer server.Close()
	t.Cleanup(func() { pkg.AdvertiseWsURL("203.0.113.9", 30000, "") })
	post := func(path string) *httpAssignGateResponse {
		t.Helper()
		rsp, err := postJSON(server.URL+path, map[string]any{}, 5*time.Second)
		if err != nil {
			t.Fatalf("postJSON %s: %v", path, err)
		}
		return rsp
	}

	if rsp := post("/admitted"); rsp.GateWsURL != wsURL {
		t.Fatalf("gate_ws_url decoded as %q, want %q", rsp.GateWsURL, wsURL)
	}
	if got := pkg.AdvertisedWsURL("203.0.113.9", 30000); got != wsURL {
		t.Fatalf("address book = %q, want the advertised address", got)
	}

	post("/queued")
	if got := pkg.AdvertisedWsURL("203.0.113.9", 30000); got != wsURL {
		t.Fatalf("a queued reply carries no gate address and must leave the book alone, got %q", got)
	}

	post("/admitted-without-ws")
	if got := pkg.AdvertisedWsURL("203.0.113.9", 30000); got != "" {
		t.Fatalf("an admitted reply without gate_ws_url must clear the old address, got %q", got)
	}
}

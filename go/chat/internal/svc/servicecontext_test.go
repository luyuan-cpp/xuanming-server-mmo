package svc

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// 退出回调必须实际释放指标监听端口，且可重复调用；仅检查回调计数无法发现HTTP服务遗留。
func TestMetricsStopReleasesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	stop := StartMetrics(address)
	t.Cleanup(stop)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	t.Cleanup(client.CloseIdleConnections)
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := client.Get("http://" + address + "/metrics")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("指标端点返回 %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("指标端点未能启动：%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	stop()
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("关闭回调返回后指标端口仍被占用：%v", err)
	}
	_ = listener.Close()
}

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// kubeWorkloadForTest 把 kubeWorkload 接到一个本地 TLS 服务上,令牌放临时文件。requests 数服务收到几次请求。
func kubeWorkloadForTest(t *testing.T, requests *atomic.Int32, handler http.HandlerFunc) *kubeWorkload {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &kubeWorkload{baseURL: server.URL, namespace: "mmorpg-zone-1", tokenPath: token, client: server.Client()}
}

// 读副本数与 Pod 在不在:200 解码、404 是不存在(StatefulSet 不存在算 0 个副本)、403 等别的状态码报错并带权限提示;
// 名字不合法时不发请求。
func TestKubeWorkloadReadsReplicasAndPods(t *testing.T) {
	var requests atomic.Int32
	workload := kubeWorkloadForTest(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/mmorpg-zone-1/statefulsets/db":
			fmt.Fprint(w, `{"kind":"StatefulSet","spec":{"replicas":2,"serviceName":"db"}}`)
		case "/apis/apps/v1/namespaces/mmorpg-zone-1/statefulsets/broken":
			fmt.Fprint(w, `{"kind":"StatefulSet","spec":{}}`)
		case "/api/v1/namespaces/mmorpg-zone-1/pods/db-2":
			fmt.Fprint(w, `{"kind":"Pod","metadata":{"name":"db-2","deletionTimestamp":"2026-11-11T00:00:00Z"}}`)
		case "/api/v1/namespaces/mmorpg-zone-1/pods/db-9":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"kind":"Status","reason":"Forbidden"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"kind":"Status","reason":"NotFound"}`)
		}
	})
	ctx := context.Background()
	if replicas, err := workload.Replicas(ctx, "db"); err != nil || replicas != 2 {
		t.Fatalf("副本数: %d %v", replicas, err)
	}
	if replicas, err := workload.Replicas(ctx, "gone"); err != nil || replicas != 0 {
		t.Fatalf("StatefulSet 不存在算 0 个副本: %d %v", replicas, err)
	}
	if _, err := workload.Replicas(ctx, "broken"); err == nil {
		t.Fatal("读回来没有 spec.replicas 不能猜")
	}
	if exists, err := workload.PodExists(ctx, "db-2"); err != nil || !exists {
		t.Fatalf("正在删的 Pod 对象也算在: %v %v", exists, err)
	}
	if exists, err := workload.PodExists(ctx, "db-3"); err != nil || exists {
		t.Fatalf("Pod 不存在: %v %v", exists, err)
	}
	if _, err := workload.PodExists(ctx, "db-9"); err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "needs get on statefulsets and pods") {
		t.Fatalf("没权限要报错: %v", err)
	}
	before := requests.Load()
	if _, err := workload.Replicas(ctx, "../secrets"); err == nil {
		t.Fatal("名字不合法要拒绝")
	}
	if _, err := workload.PodExists(ctx, "db-0/exec"); err == nil {
		t.Fatal("名字不合法要拒绝")
	}
	if requests.Load() != before {
		t.Fatal("名字不合法时不发请求")
	}
}

// 不在 Kubernetes 里(没有 KUBERNETES_SERVICE_HOST / PORT)或 POD_NAMESPACE 不合法:在读凭据之前就拒绝。
func TestInClusterWorkloadNeedsKubernetesEnvironment(t *testing.T) {
	if _, err := inClusterWorkload(func(string) string { return "" }); err == nil {
		t.Fatal("集群外要拒绝")
	}
	env := map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "KUBERNETES_SERVICE_PORT": "443", "POD_NAMESPACE": "Bad_Namespace"}
	if _, err := inClusterWorkload(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Fatalf("namespace 不合法要拒绝: %v", err)
	}
}

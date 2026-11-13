package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// kubeClientForTest 把 kubeClient 接到一个本地 TLS 服务上,令牌放临时文件。requests 数服务收到几次请求;
// 没带对令牌的请求回 401,不进 handler。
func kubeClientForTest(t *testing.T, requests *atomic.Int32, handler http.HandlerFunc) *kubeClient {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token" {
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
	return &kubeClient{baseURL: server.URL, tokenPath: token, client: server.Client()}
}

func kubeWorkloadForTest(t *testing.T, requests *atomic.Int32, handler http.HandlerFunc) *kubeWorkload {
	t.Helper()
	return &kubeWorkload{kubeClient: kubeClientForTest(t, requests, handler), namespace: namespaceForTest}
}

// 读副本数、Pod 在不在、ServiceAccount 的 UID:200 解码、404 是不存在(StatefulSet 不存在算 0 个副本)、403 等别的状态码报错;
// 只发 GET;名字不合法时不发请求。
func TestKubeWorkloadReadsReplicasPodsAndServiceAccounts(t *testing.T) {
	var requests atomic.Int32
	workload := kubeWorkloadForTest(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
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
		case "/api/v1/namespaces/mmorpg-zone-1/serviceaccounts/default":
			fmt.Fprintf(w, `{"kind":"ServiceAccount","metadata":{"name":"default","uid":%q}}`, serviceAccountUIDForTest)
		case "/api/v1/namespaces/mmorpg-zone-1/serviceaccounts/no-uid":
			fmt.Fprint(w, `{"kind":"ServiceAccount","metadata":{"name":"no-uid"}}`)
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
	if _, err := workload.PodExists(ctx, "db-9"); err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("没权限要报错,带上 API server 的回答: %v", err)
	}
	if uid, found, err := workload.ServiceAccountUID(ctx, "default"); err != nil || !found || uid != serviceAccountUIDForTest {
		t.Fatalf("ServiceAccount 的 UID: %q %v %v", uid, found, err)
	}
	if _, found, err := workload.ServiceAccountUID(ctx, "removed-account"); err != nil || found {
		t.Fatalf("ServiceAccount 不存在: %v %v", found, err)
	}
	if _, _, err := workload.ServiceAccountUID(ctx, "no-uid"); err == nil {
		t.Fatal("读回来没有 UID 不能当成对上了")
	}
	before := requests.Load()
	if _, err := workload.Replicas(ctx, "../secrets"); err == nil {
		t.Fatal("名字不合法要拒绝")
	}
	if _, err := workload.PodExists(ctx, "db-0/exec"); err == nil {
		t.Fatal("名字不合法要拒绝")
	}
	if _, _, err := workload.ServiceAccountUID(ctx, "default/token"); err == nil {
		t.Fatal("名字不合法要拒绝")
	}
	if requests.Load() != before {
		t.Fatal("名字不合法时不发请求")
	}
}

// 注册器问 API server「我是谁」:POST SelfSubjectReview,回本 Pod 的 ServiceAccount 与它的 UID。
// 不是 ServiceAccount、UID 不成形、API server 没有这个接口(1.28 之前)、没权限都报错,不猜。
func TestSelfIdentityAsksTheAPIServerWhoThisPodIs(t *testing.T) {
	var requests atomic.Int32
	answer := `{"kind":"SelfSubjectReview","status":{"userInfo":{"username":"system:serviceaccount:mmorpg-zone-1:default","uid":"` + serviceAccountUIDForTest + `","groups":["system:serviceaccounts"]}}}`
	status := http.StatusCreated
	client := kubeClientForTest(t, &requests, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" || r.Header.Get("Content-Type") != "application/json" ||
			!strings.Contains(string(body), `"kind":"SelfSubjectReview"`) || !strings.Contains(string(body), `"apiVersion":"authentication.k8s.io/v1"`) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, answer)
	})
	ctx := context.Background()
	identity, err := client.selfIdentity(ctx)
	if err != nil || identity.String() != workloadIdentityForTest {
		t.Fatalf("本 Pod 的工作负载身份: %q %v", identity.String(), err)
	}
	for name, bad := range map[string]string{
		"不是 ServiceAccount": `{"status":{"userInfo":{"username":"kubernetes-admin","uid":"` + serviceAccountUIDForTest + `"}}}`,
		"没有 UID":            `{"status":{"userInfo":{"username":"system:serviceaccount:mmorpg-zone-1:default"}}}`,
		"UID 不成形":           `{"status":{"userInfo":{"username":"system:serviceaccount:mmorpg-zone-1:default","uid":"not-a-uid"}}}`,
		"用户名多一段":            `{"status":{"userInfo":{"username":"system:serviceaccount:mmorpg-zone-1:default:extra","uid":"` + serviceAccountUIDForTest + `"}}}`,
	} {
		answer = bad
		if _, err = client.selfIdentity(ctx); err == nil {
			t.Fatalf("%s 要拒绝", name)
		}
	}
	status = http.StatusNotFound
	if _, err = client.selfIdentity(ctx); err == nil || !strings.Contains(err.Error(), "1.28") {
		t.Fatalf("API server 没有 SelfSubjectReview 要说明版本要求: %v", err)
	}
	status = http.StatusForbidden
	answer = `{"kind":"Status","reason":"Forbidden"}`
	if _, err = client.selfIdentity(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("没权限要报错: %v", err)
	}
}

// 不在 Kubernetes 里(没有 KUBERNETES_SERVICE_HOST / PORT)或 POD_NAMESPACE 不合法:收尾在读凭据之前就拒绝。
// 注册器在集群外不问身份,回空串(本机脚本、调试栈照常登记)。
func TestInClusterWorkloadNeedsKubernetesEnvironment(t *testing.T) {
	outside := func(string) string { return "" }
	if _, err := inClusterWorkload(outside); err == nil {
		t.Fatal("集群外要拒绝")
	}
	if _, err := inClusterClient(outside); err == nil {
		t.Fatal("集群外要拒绝")
	}
	env := map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1", "KUBERNETES_SERVICE_PORT": "443", "POD_NAMESPACE": "Bad_Namespace"}
	if _, err := inClusterWorkload(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Fatalf("namespace 不合法要拒绝: %v", err)
	}
	if identity, err := registrarIdentity(context.Background(), outside); err != nil || identity != "" {
		t.Fatalf("集群外登记不带身份、不报错: %q %v", identity, err)
	}
}

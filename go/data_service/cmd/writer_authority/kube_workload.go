package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// serviceAccountTokenPath、serviceAccountCAPath 是 Pod 里 ServiceAccount 凭据的标准挂载位置。
	serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceAccountCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	// kubeResponseLimit 限制读回的响应体:一个 StatefulSet 或 Pod 对象只有几 KB。
	kubeResponseLimit = 1 << 20
	// kubeErrorSnippet 是错误里带的响应体长度上限。API server 的错误是一个 Status 对象,不含凭据。
	kubeErrorSnippet = 256
	// kubeRequestTimeout 是一次 GET 的上限,整条命令另有 -timeout。
	kubeRequestTimeout = 10 * time.Second
)

// kubeWorkload 读本 namespace 里写者 StatefulSet 的期望副本数与 Pod 在不在。只用标准库对 API server 发 GET,
// 与 internal/clusteridentity 同一做法,不引 client-go(服务按 -mod=readonly 构建,多一串传递依赖的校验和)。
// 要的权限只有 statefulsets 与 pods 的 get(deploy/k8s/manifests/go-svc/sql-writer-slot-retirer.yaml 的 Role)。
type kubeWorkload struct {
	baseURL   string
	namespace string
	tokenPath string
	client    *http.Client
}

// inClusterWorkload 按 Pod 里的标准环境变量与 ServiceAccount 凭据构造 kubeWorkload。namespace 取 POD_NAMESPACE(downward API)。
func inClusterWorkload(getenv func(string) string) (*kubeWorkload, error) {
	host, port, namespace := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT"), getenv("POD_NAMESPACE")
	if host == "" || port == "" {
		return nil, errors.New("retire-removed-slots runs inside Kubernetes: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	if !kubernetesNamePattern.MatchString(namespace) {
		return nil, fmt.Errorf("POD_NAMESPACE %q is not a namespace name", namespace)
	}
	caPEM, err := os.ReadFile(serviceAccountCAPath)
	if err != nil {
		return nil, fmt.Errorf("read the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificate in %s", serviceAccountCAPath)
	}
	client := &http.Client{Timeout: kubeRequestTimeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return &kubeWorkload{baseURL: "https://" + net.JoinHostPort(host, port), namespace: namespace, tokenPath: serviceAccountTokenPath, client: client}, nil
}

// Replicas 回 StatefulSet 的 spec.replicas;StatefulSet 不存在回 0。
func (k *kubeWorkload) Replicas(ctx context.Context, statefulSet string) (int, error) {
	if !kubernetesNamePattern.MatchString(statefulSet) {
		return 0, fmt.Errorf("%q is not a StatefulSet name", statefulSet)
	}
	var object struct {
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
	}
	found, err := k.get(ctx, "/apis/apps/v1/namespaces/"+k.namespace+"/statefulsets/"+statefulSet, &object)
	if err != nil || !found {
		return 0, err
	}
	// API server 会给省略的 replicas 补缺省值 1,读回来没有就是对象不对,不猜。
	if object.Spec.Replicas == nil || *object.Spec.Replicas < 0 {
		return 0, fmt.Errorf("statefulset %s/%s has no spec.replicas", k.namespace, statefulSet)
	}
	return int(*object.Spec.Replicas), nil
}

// PodExists 回这个名字的 Pod 对象在不在。正在删的 Pod 对象还在,回 true。
func (k *kubeWorkload) PodExists(ctx context.Context, pod string) (bool, error) {
	if !kubernetesNamePattern.MatchString(pod) {
		return false, fmt.Errorf("%q is not a pod name", pod)
	}
	return k.get(ctx, "/api/v1/namespaces/"+k.namespace+"/pods/"+pod, nil)
}

// get 发一次 GET:200 把响应体解码进 out(out 为 nil 不解码)回 true;404 回 false;别的状态码回错误。
// 每次重读 ServiceAccount 令牌:令牌会轮换。
func (k *kubeWorkload) get(ctx context.Context, path string, out any) (bool, error) {
	token, err := os.ReadFile(k.tokenPath)
	if err != nil {
		return false, fmt.Errorf("read the service account token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, k.baseURL+path, nil)
	if err != nil {
		return false, fmt.Errorf("build GET %s: %w", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Accept", "application/json")
	response, err := k.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, kubeResponseLimit))
	if err != nil {
		return false, fmt.Errorf("read the response of GET %s: %w", path, err)
	}
	switch response.StatusCode {
	case http.StatusOK:
		if out != nil {
			if err = json.Unmarshal(body, out); err != nil {
				return false, fmt.Errorf("decode the response of GET %s: %w", path, err)
			}
		}
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		text := strings.TrimSpace(string(body))
		if len(text) > kubeErrorSnippet {
			text = text[:kubeErrorSnippet] + "..."
		}
		return false, fmt.Errorf("GET %s returned HTTP %d (the service account needs get on statefulsets and pods in this namespace): %s", path, response.StatusCode, text)
	}
}

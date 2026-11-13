package main

import (
	"bytes"
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
	// kubeResponseLimit 限制读回的响应体:这里读的对象(StatefulSet、Pod、ServiceAccount、SelfSubjectReview)都只有几 KB。
	kubeResponseLimit = 1 << 20
	// kubeErrorSnippet 是错误里带的响应体长度上限。API server 的错误是一个 Status 对象,不含凭据。
	kubeErrorSnippet = 256
	// kubeRequestTimeout 是一次请求的上限,整条命令另有 -timeout。
	kubeRequestTimeout = 10 * time.Second
)

// kubeClient 用 Pod 里的 ServiceAccount 凭据对本集群的 API server 发请求。只用标准库,与 internal/clusteridentity 同一做法,
// 不引 client-go(服务按 -mod=readonly 构建,多一串传递依赖的校验和)。
type kubeClient struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

// inKubernetes 报告本进程是不是跑在 K8s 的 Pod 里:kubelet 给每个容器注入这两个变量。
func inKubernetes(getenv func(string) string) bool {
	return getenv("KUBERNETES_SERVICE_HOST") != "" && getenv("KUBERNETES_SERVICE_PORT") != ""
}

// inClusterClient 按 Pod 里的标准环境变量与 ServiceAccount 凭据构造 kubeClient,只信任集群自己的 CA。
func inClusterClient(getenv func(string) string) (*kubeClient, error) {
	if !inKubernetes(getenv) {
		return nil, errors.New("not running inside Kubernetes: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
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
	return &kubeClient{baseURL: "https://" + net.JoinHostPort(getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")), tokenPath: serviceAccountTokenPath, client: client}, nil
}

// do 发一次请求,回状态码;2xx 时把响应体解码进 out(out 为 nil 不解码)。body 为 nil 不带请求体。
// 2xx 与 404 之外的状态码回错误,带一小段响应体。每次重读 ServiceAccount 令牌:令牌会轮换。
func (k *kubeClient) do(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	token, err := os.ReadFile(k.tokenPath)
	if err != nil {
		return 0, fmt.Errorf("read the service account token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, k.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := k.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, kubeResponseLimit))
	if err != nil {
		return 0, fmt.Errorf("read the response of %s %s: %w", method, path, err)
	}
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		if out != nil {
			if err = json.Unmarshal(raw, out); err != nil {
				return 0, fmt.Errorf("decode the response of %s %s: %w", method, path, err)
			}
		}
		return response.StatusCode, nil
	case response.StatusCode == http.StatusNotFound:
		return response.StatusCode, nil
	default:
		text := strings.TrimSpace(string(raw))
		if len(text) > kubeErrorSnippet {
			text = text[:kubeErrorSnippet] + "..."
		}
		return response.StatusCode, fmt.Errorf("%s %s returned HTTP %d: %s", method, path, response.StatusCode, text)
	}
}

// get 发一次 GET:200 回 true,404 回 false。
func (k *kubeClient) get(ctx context.Context, path string, out any) (bool, error) {
	status, err := k.do(ctx, http.MethodGet, path, nil, out)
	if err != nil {
		return false, err
	}
	return status != http.StatusNotFound, nil
}

// selfIdentity 问 API server「我是谁」(SelfSubjectReview,K8s 1.28 起正式;所有已认证身份默认可调,不用另配 RBAC),
// 回本 Pod 的 ServiceAccount 连同这个对象的 UID。注册器登记一代时用它(registrarIdentity)。
func (k *kubeClient) selfIdentity(ctx context.Context) (workloadIdentity, error) {
	const path = "/apis/authentication.k8s.io/v1/selfsubjectreviews"
	var review struct {
		Status struct {
			UserInfo struct {
				Username string `json:"username"`
				UID      string `json:"uid"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	status, err := k.do(ctx, http.MethodPost, path, []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`), &review)
	if err != nil {
		return workloadIdentity{}, err
	}
	if status == http.StatusNotFound {
		return workloadIdentity{}, errors.New("the API server has no authentication.k8s.io/v1 SelfSubjectReview (needs Kubernetes 1.28 or newer)")
	}
	return serviceAccountIdentity(review.Status.UserInfo.Username, review.Status.UserInfo.UID)
}

// kubeWorkload 读本 namespace 里写者 StatefulSet 的期望副本数、Pod 在不在、ServiceAccount 的 UID(缩容收尾用)。
// 要的权限只有这一个 StatefulSet、pods 与 serviceaccounts 的 get(deploy/k8s/manifests/go-svc/sql-writer-slot-retirer.yaml 的 Role)。
type kubeWorkload struct {
	*kubeClient
	namespace string
}

// inClusterWorkload 在 inClusterClient 之上取本 Pod 的 namespace(POD_NAMESPACE,downward API)。
func inClusterWorkload(getenv func(string) string) (*kubeWorkload, error) {
	namespace := getenv("POD_NAMESPACE")
	if !inKubernetes(getenv) {
		return nil, errors.New("retire-removed-slots runs inside Kubernetes: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	if !kubernetesNamePattern.MatchString(namespace) {
		return nil, fmt.Errorf("POD_NAMESPACE %q is not a namespace name", namespace)
	}
	client, err := inClusterClient(getenv)
	if err != nil {
		return nil, err
	}
	return &kubeWorkload{kubeClient: client, namespace: namespace}, nil
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

// ServiceAccountUID 回本 namespace 里这个 ServiceAccount 对象的 UID;对象不存在回 found=false。
func (k *kubeWorkload) ServiceAccountUID(ctx context.Context, name string) (string, bool, error) {
	if !serviceAccountNamePattern.MatchString(name) {
		return "", false, fmt.Errorf("%q is not a service account name", name)
	}
	var object struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	found, err := k.get(ctx, "/api/v1/namespaces/"+k.namespace+"/serviceaccounts/"+name, &object)
	if err != nil || !found {
		return "", false, err
	}
	if !objectUIDPattern.MatchString(object.Metadata.UID) {
		return "", false, fmt.Errorf("service account %s/%s has no usable metadata.uid", k.namespace, name)
	}
	return object.Metadata.UID, true, nil
}

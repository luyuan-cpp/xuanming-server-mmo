// Package battlepods 经 Kubernetes API 判 battle 的原 Pod 还在不在(docs/design/battle-instance-loss-early-abort.md §4.2 的 E3)。
//
// C++ 节点在 K8s 里登记的内部地址就是 POD_IP(node.cpp 读 POD_IP,部署用 status.podIP 注入,tools/scripts/lib/k8s_client_entry.ps1)。
// 「battle 所在命名空间里已没有存活的 Pod 持有原 IP」说明原进程已经死了,它冻结在内存里的终局永远不会再提交:
//   - Pod 对象不在了:正常删除时,Pod 对象要等 kubelet 确认容器都已停止才从 API server 消失;
//   - Pod 还在但 phase 是 Succeeded / Failed:kubelet 已确认它的容器全部终止(Agones 的 GameServer Pod 是 restartPolicy: Never,
//     进程崩溃后先进 Failed,稍后才被删)。
//
// 仍算存活的:Pending / Running / Unknown(节点失联时是 Unknown 或带删除时间戳的 Terminating,容器可能还在跑)。
// 例外是强删(--force --grace-period=0)和手工删 Node 对象:Pod 对象先消失,进程可能还活在失联的节点上。运维手册禁止这两种操作
// (docs/ops/match-formation-manual-abort-runbook.md「battle Pod 不许强删」)。
// 原 IP 已被另一个存活的 Pod 占用时这里判不了(分不清它是不是原 Pod),交给另两种证据:原地址上是另一个实例,或拒连且已不在册。
// 命名空间由部署脚本按它部署 battle 的命名空间写进 match 的配置;写错成别的命名空间会让这里恒回「没有」,所以不许手改
// (tools/scripts/tests 的部署契约测试核对)。
//
// 只用标准库发 GET,不引 client-go,理由同 data_service/internal/clusteridentity:服务按 -mod=readonly 构建,把 client-go 提成
// 直接依赖要连带一串传递包的校验和。ServiceAccount 只需要 battle 所在命名空间的 pods list
// (deploy/k8s/manifests/go-svc/match-battle-pod-reader-rbac.yaml)。
package battlepods

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
	"net/url"
	"os"
	"strings"
	"sync"
)

const (
	// DefaultTokenPath、DefaultCAPath 是 Pod 里 ServiceAccount 凭据的标准挂载位置。
	DefaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	// listLimit 是一次查询最多取回的 Pod 数。同一个 IP 上通常只有一个,偶尔多一个已终止、状态里还留着这个 IP 的旧 Pod;
	// 取不完(回包带 continue)就判不了。
	listLimit = "20"
	// maxResponseBytes 限制读回的响应体:按 IP 筛过的 PodList 只有一两个对象。
	maxResponseBytes = 4 << 20
	// maxErrorSnippet 是错误里带的响应体长度上限。API server 的错误是一个 Status 对象,不含凭据。
	maxErrorSnippet = 256
)

// ErrNotConfigured:没配命名空间,或进程不在 Kubernetes 里(没有 KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT)。
var ErrNotConfigured = errors.New("battle pods: lookup not configured (no namespace, or not running inside Kubernetes)")

// Lookup 按 Pod IP 查 battle 所在命名空间里的 Pod。可被多个 goroutine 同时调用。
type Lookup struct {
	Namespace string
	Host      string
	Port      string
	TokenPath string
	CAPath    string
	// Client 为 nil 时第一次调用按 CAPath 里的集群 CA 建一个只信任它的客户端,之后复用。
	Client *http.Client

	clientOnce sync.Once
	client     *http.Client
	clientErr  error
}

// InCluster 按 Pod 里的标准环境变量和凭据位置构造 Lookup。不做任何 IO。namespace 为空时 Gone 一律回 ErrNotConfigured。
func InCluster(namespace string) *Lookup {
	return &Lookup{
		Namespace: namespace,
		Host:      os.Getenv("KUBERNETES_SERVICE_HOST"),
		Port:      os.Getenv("KUBERNETES_SERVICE_PORT"),
		TokenPath: DefaultTokenPath,
		CAPath:    DefaultCAPath,
	}
}

// Gone 回报命名空间里是否已经没有存活的 Pod 持有 endpoint 的 IP(endpoint 是 "ip:port",与成局行的 target_endpoint 同形)。
// 只有 API 明确回答「没有存活的」才回 true;endpoint 不是字面 IP、没配置、请求或解码失败、回包没取完都回 false 与原因(判不了)。
// 每次调用都重读令牌(ServiceAccount 令牌会轮换)、发一次请求,不缓存结论:短时复用由调用方决定。
func (l *Lookup) Gone(ctx context.Context, endpoint string) (bool, error) {
	if l == nil || l.Namespace == "" || l.Host == "" || l.Port == "" {
		return false, ErrNotConfigured
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || net.ParseIP(host) == nil {
		return false, fmt.Errorf("battle pods: endpoint %q has no literal pod IP", endpoint)
	}
	token, err := os.ReadFile(l.TokenPath)
	if err != nil {
		return false, fmt.Errorf("battle pods: read the service account token: %w", err)
	}
	client, err := l.httpClient()
	if err != nil {
		return false, err
	}
	query := url.Values{"fieldSelector": {"status.podIP=" + host}, "limit": {listLimit}}
	target := "https://" + net.JoinHostPort(l.Host, l.Port) + "/api/v1/namespaces/" + url.PathEscape(l.Namespace) + "/pods?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, fmt.Errorf("battle pods: build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("battle pods: list pods in %s: %w", l.Namespace, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return false, fmt.Errorf("battle pods: read the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("battle pods: list pods in %s returned HTTP %d (the service account needs list on pods): %s",
			l.Namespace, resp.StatusCode, snippet(body))
	}
	return decodeGone(body)
}

// decodeGone 解 PodList:没有存活的 Pod(列表为空,或每一个的 phase 都是 Succeeded / Failed)= 没有了。
// 不是 PodList、没取完(带 continue)都判不了。纯函数。
func decodeGone(body []byte) (bool, error) {
	var list struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, fmt.Errorf("battle pods: decode the pod list: %w", err)
	}
	if list.Kind != "PodList" {
		return false, fmt.Errorf("battle pods: unexpected object kind %q", list.Kind)
	}
	if list.Metadata.Continue != "" {
		return false, errors.New("battle pods: more pods share this IP than one page holds")
	}
	for _, pod := range list.Items {
		if pod.Status.Phase != "Succeeded" && pod.Status.Phase != "Failed" {
			return false, nil
		}
	}
	return true, nil
}

// httpClient 返回只信任集群 CA 的客户端,第一次调用时建好、之后复用连接。
func (l *Lookup) httpClient() (*http.Client, error) {
	if l.Client != nil {
		return l.Client, nil
	}
	l.clientOnce.Do(func() {
		caPEM, err := os.ReadFile(l.CAPath)
		if err != nil {
			l.clientErr = fmt.Errorf("battle pods: read the cluster CA: %w", err)
			return
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			l.clientErr = fmt.Errorf("battle pods: no certificate in %s", l.CAPath)
			return
		}
		l.client = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}}
	})
	return l.client, l.clientErr
}

// snippet 截一小段响应体放进错误。
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > maxErrorSnippet {
		text = text[:maxErrorSnippet] + "..."
	}
	return text
}

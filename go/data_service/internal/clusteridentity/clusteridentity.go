// Package clusteridentity 读本进程所在 Kubernetes 集群的身份:kube-system 命名空间的 UID。
//
// 发号器用它把地域码绑定到一个具体的 K8s 集群(docs/design/player-data-tidb-scaling-plan.md §6.3)。只比 etcd 集群号不够:
// 集群号只由成员 peer 地址和引导令牌算出,同一份清单部署到两个 K8s 集群就相同;集群级备份恢复(Velero、卷快照)还会把
// 数据目录里的集群号一起带到别的集群。
//
// 身份必须在运行期从集群里读,不能由部署脚本渲染进 ConfigMap、Secret 或环境变量:集群级备份恢复会把这些对象连同
// etcd 数据一起带到另一个集群,渲染进去的值也就跟着过去了。kube-system 的 UID 由 API server 在建集群时生成,按对象
// 恢复出来的东西一律拿新的 UID,所以带不过去。带得过去的只有整个控制面(连同 kube-system)从备份克隆出第二个集群,
// 这种情形由登记处水位兜底,见设计 §6.3。
//
// 只用标准库发一次 GET,不引 client-go:data_service 只需要这一个字段,把 client-go 提成直接依赖要连带一串传递包的
// 校验和,而服务按 -mod=readonly 构建。
package clusteridentity

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
	"regexp"
	"strings"
)

const (
	// DefaultTokenPath、DefaultCAPath 是 Pod 里 ServiceAccount 凭据的标准挂载位置。
	DefaultTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	// Prefix 标明身份的来源。登记处里存的就是 Prefix + UID。
	Prefix = "k8s:"

	// maxResponseBytes 限制读回的响应体:一个 Namespace 对象只有几百字节。
	maxResponseBytes = 1 << 20
	// maxErrorSnippet 是错误里带的响应体长度上限。API server 的错误是一个 Status 对象,不含凭据。
	maxErrorSnippet = 256
)

// ErrNotInCluster:进程不在 Kubernetes 里(没有 KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT)。
var ErrNotInCluster = errors.New("cluster identity: not running inside Kubernetes " +
	"(KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set)")

var uidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Reader 经 Kubernetes API 读 kube-system 命名空间的 UID。生产用 InCluster 构造;单测可以换地址、凭据路径和 HTTP 客户端。
type Reader struct {
	Host      string
	Port      string
	TokenPath string
	CAPath    string
	// Client 为 nil 时按 CAPath 里的集群 CA 建一个只信任它的客户端。
	Client *http.Client
}

// InCluster 按 Pod 里的标准环境变量和凭据位置构造 Reader。不做任何 IO。
func InCluster() Reader {
	return Reader{
		Host:      os.Getenv("KUBERNETES_SERVICE_HOST"),
		Port:      os.Getenv("KUBERNETES_SERVICE_PORT"),
		TokenPath: DefaultTokenPath,
		CAPath:    DefaultCAPath,
	}
}

// Read 返回 Prefix + kube-system 的 UID。每次调用都重读凭据(ServiceAccount 令牌会轮换)、发一次请求,不缓存;
// 缓存由调用方决定。ServiceAccount 要能 get namespaces/kube-system
// (deploy/k8s/manifests/go-svc/data-service-identity-rbac.yaml)。
func (r Reader) Read(ctx context.Context) (string, error) {
	if r.Host == "" || r.Port == "" {
		return "", ErrNotInCluster
	}
	token, err := os.ReadFile(r.TokenPath)
	if err != nil {
		return "", fmt.Errorf("cluster identity: read the service account token: %w", err)
	}
	client := r.Client
	if client == nil {
		if client, err = r.newClient(); err != nil {
			return "", err
		}
	}
	url := "https://" + net.JoinHostPort(r.Host, r.Port) + "/api/v1/namespaces/kube-system"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("cluster identity: build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cluster identity: GET namespace kube-system: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("cluster identity: read the response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cluster identity: GET namespace kube-system returned HTTP %d "+
			"(the service account needs get on namespaces/kube-system): %s", resp.StatusCode, snippet(body))
	}
	var ns struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &ns); err != nil {
		return "", fmt.Errorf("cluster identity: decode namespace kube-system: %w", err)
	}
	if ns.Metadata.Name != "kube-system" || !uidPattern.MatchString(ns.Metadata.UID) {
		return "", fmt.Errorf("cluster identity: unexpected namespace object name=%q uid=%q", ns.Metadata.Name, ns.Metadata.UID)
	}
	return Prefix + ns.Metadata.UID, nil
}

// newClient 建一个只信任集群 CA 的一次性客户端:一个进程只读一次身份,不保留连接。
func (r Reader) newClient() (*http.Client, error) {
	caPEM, err := os.ReadFile(r.CAPath)
	if err != nil {
		return nil, fmt.Errorf("cluster identity: read the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("cluster identity: no certificate in %s", r.CAPath)
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}}, nil
}

// snippet 截一小段响应体放进错误。
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > maxErrorSnippet {
		text = text[:maxErrorSnippet] + "..."
	}
	return text
}

// Package clockcheck 在服务启动时核对本机时钟:和 Kubernetes API server 的时钟差 MaxOffset 以上就拒绝启动。
//
// 为什么:会话与令牌过期、每日重置、活动开关、按时间列排序翻页都按墙钟算。机器时间被调回一年前,过期的令牌又能用、
// 结束的活动又开了,进程却照常起来、什么也不报。时钟由 NTP 维护,跑着时的偏差靠节点告警发现
// (deploy/k8s/clock-alerts.yaml);这一道只拦明显错的时钟。运维说明见 docs/ops/clock-sync-runbook.md。
// C++ 节点(clock_check.h)和 Java 服务(java/server_startup 的 ClockCheck)是同一套规则、同一个开关。
//
// 参照是 API server 回应里的 Date 头:GET /version 不要任何 RBAC 权限,不论回应状态码都带 Date;TLS 用 Pod 里挂载的
// 集群 CA 核对服务端证书。不在 Kubernetes 里(本机开发)不核。参照读不到只告警、照常启动:拿不到参照不等于时钟错了,
// 为此拒启会把 API server 的一次抖动放大成重启的服务全都起不来。
package clockcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	// MaxOffset 是启动时允许的最大偏差。Date 头只到秒,加上往返,测量误差在两秒以内;NTP 正常的机器只差几毫秒。
	// 30 秒只拦明显错的时钟,不会误伤;更细的偏差靠 NTP 和告警(超过 1 秒告警)。
	MaxOffset = 30 * time.Second
	// ModeEnv 设成 warn 时,超限只告警、照常启动:API server 自己的时钟错了、又必须先把服务拉起来时的应急开关。
	// 其余取值(包括不设)都按拒启。
	ModeEnv = "MMORPG_CLOCK_CHECK_MODE"

	clusterCAPath  = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	requestTimeout = 3 * time.Second
)

// MustPass 核对本机时钟,偏差超过 MaxOffset 时向 stderr 写一行原因并以 1 退出。每个服务的 main 在读配置之前调一次。
// 输出只写 stderr:有的入口要保持 stdout 只有版本行或一行统计。
func MustPass(service string) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return // 不在 Kubernetes 里:没有参照,不核
	}
	offset, err := measureCluster(host, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clock check skipped: cannot read the Kubernetes API server clock: %v\n", err)
		return
	}
	if err := judge(offset, os.Getenv(ModeEnv)); err != nil {
		fmt.Fprintf(os.Stderr, "%s refuses to start: %v\n", service, err)
		os.Exit(1)
	}
}

func measureCluster(host, port string) (time.Duration, error) {
	caPEM, err := os.ReadFile(clusterCAPath)
	if err != nil {
		return 0, fmt.Errorf("read the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return 0, fmt.Errorf("no certificate in %s", clusterCAPath)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	return measure(ctx, client, "https://"+net.JoinHostPort(host, port)+"/version", time.Now)
}

// measure 返回本机时钟减参照时钟。参照是回应的 Date 头,只到秒:本机取请求前后的中点,参照取那一秒的中点。
func measure(ctx context.Context, client *http.Client, url string, now func() time.Time) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	sent := now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	received := now()
	_ = resp.Body.Close()
	date := resp.Header.Get("Date")
	reference, err := http.ParseTime(date)
	if err != nil {
		return 0, fmt.Errorf("unusable Date header %q: %w", date, err)
	}
	local := sent.Add(received.Sub(sent) / 2)
	return local.Sub(reference.Add(500 * time.Millisecond)), nil
}

// judge 按偏差和 ModeEnv 的取值决定能不能启动:能启动返回 nil 并写一行结果,要拒启返回原因。
func judge(offset time.Duration, mode string) error {
	measured := fmt.Sprintf("the local clock is %v off the Kubernetes API server clock", offset.Round(time.Millisecond))
	if offset >= -MaxOffset && offset <= MaxOffset {
		fmt.Fprintf(os.Stderr, "clock check passed: %s\n", measured)
		return nil
	}
	err := fmt.Errorf("%s, more than the %v allowed; fix the node clock (docs/ops/clock-sync-runbook.md), "+
		"or set %s=warn to start anyway", measured, MaxOffset, ModeEnv)
	if mode == "warn" {
		fmt.Fprintf(os.Stderr, "clock check failed but %s=warn: %v\n", ModeEnv, err)
		return nil
	}
	return err
}

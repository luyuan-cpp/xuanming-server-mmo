// Package grpckeepalive 是 Go 服务调别的 Go 服务时 gRPC 客户端的 keepalive 口径
// (docs/design/infra-reconnect-overview.md §2「服务间 gRPC」:keepalive 参数与对端的强制策略一致)。
//
// 为什么要开:经 K8s Service 地址(ClusterIP)拨号时,一条连接固定落在 kube-proxy 建连那一刻选中的那个 Pod 上。
// 那个 Pod 所在节点整机掉电或网络分区时,连接收不到 FIN/RST,成了半开连接;不开 keepalive,grpc-go 不设
// TCP_USER_TIMEOUT,这条连接要等内核重传超时(Linux tcp_retries2 缺省约 15 分钟)才报错。这期间每次调用都只能
// 等到截止时间失败,同一个 Service 后面活着的副本也用不上。走 etcd 发现的调用不受影响:实例的租约一过期,
// 发现层就关掉那条连接。
//
// grpc-go 在开了 keepalive 的客户端连接上把 TCP_USER_TIMEOUT 设成 Timeout(gRFC A18):已发出的数据超过
// Timeout 还没有确认,内核就断开连接;之后重新建连,由 kube-proxy 换到存活的副本(K8s 摘掉那个端点之前,
// 新连接仍可能落到它上面建连失败,由 grpc 退避重试)。
//
// Time 取 5 分钟,不小于 grpc-go 服务端缺省强制策略的 MinTime(5 分钟,仓库里没有服务端改过这条策略):
// ping 更频繁会被对端以 too_many_pings 踢断(infra-reconnect-overview.md R16 的教训)。PermitWithoutStream 保持
// false,同样是对端缺省策略的要求。所以 ping 本身很少发出,起作用的主要是 TCP_USER_TIMEOUT。
// 对端改成更宽的强制策略之前,不要把 Time 调小。
package grpckeepalive

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const (
	pingInterval = 5 * time.Minute
	// ackTimeout 同时是 TCP_USER_TIMEOUT:对端整机消失后,一次调用发出去最多这么久连接就断开重连。
	ackTimeout = 20 * time.Second
)

func params() keepalive.ClientParameters {
	return keepalive.ClientParameters{Time: pingInterval, Timeout: ackTimeout}
}

// DialOption 是带本包 keepalive 的拨号选项。裸 grpc.NewClient 直接传入;zrpc 客户端用
// zrpc.WithDialOption(grpckeepalive.DialOption()) 传入(它排在 RpcClientConf.KeepaliveTime 生成的选项之后,以它为准)。
func DialOption() grpc.DialOption {
	return grpc.WithKeepaliveParams(params())
}

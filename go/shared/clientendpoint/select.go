// Package clientendpoint 是「下发给客户端的节点地址怎么选」这条规则在 Go 侧的唯一权威实现
// (集群外入口 D76–D78,见 docs/design/k8s-client-entry.md)。
//
// 背景:NodeInfo 有两个地址,语义不同,不能混用:
//
//   - endpoint:集群内身份。gRPC 绑定、端口 CAS、节点间对端校验、FindNodeByPodIP 都只认它。
//   - client_endpoint(字段 11):客户端可达地址(advertised address,同 Kafka
//     advertised.listeners),只由 gate / battle 在发布到 etcd 前自报;podip 模式下不填。
//
// 所有「把节点地址下发给客户端」的出口(login CandidatesForZone、scene_manager
// RedirectToGate)都必须经 Select 取地址、经 DedupeNewest 去掉同一客户端地址上的陈旧记录,
// 不许各自再写一份判定。C++ 侧的同一条规则是 client_endpoint::ClientFacing。
//
// 本包只做纯判定 + 低基数计数(metrics.go),不做 I/O,除指标注册失败外不打日志;
// 「哪个节点缺地址」这类需要节点 id 的诊断日志由调用方按自己的节流口径打。
package clientendpoint

// maxPort 是 TCP 端口上限。0 表示「未填」,同样视为不可用。
const maxPort = 65535

// usable 判定一对 host/port 能否直接交给客户端去连。
//
// 半填(只有 host 或只有 port)一律视为缺失:proto3 的零值分不清「没填」和「填了 0」,
// 而把 ":18000" 或 "gate-0.example.com:0" 下发出去,客户端只会连不上,且没有任何服务端报错。
func usable(host string, port uint32) bool {
	return host != "" && port >= 1 && port <= maxPort
}

// Select 按 D78 选出下发给客户端的地址。
//
// 规则(按顺序):
//
//  1. client 地址可用(host 非空且 port 在 1..65535)→ 用它,结果计 client;
//  2. require=true → 不回落,ok=false,结果计 rejected。external 模式下缺地址的节点
//     必须跳过:回落到 PodIP 等于把一个集群外连不上的地址发给玩家;
//  3. require=false → 回落 endpoint(podip 模式与滚动过渡期),结果计 fallback;
//     endpoint 自身不可用时同样 ok=false 计 rejected —— 空 host / 0 端口下发出去毫无意义。
//
// ok=false 时 host 为空串、port 为 0,调用方必须跳过该节点,不得拿零值继续下发。
// 本函数无 I/O,可在热路径逐节点调用;唯一副作用是对计数器做一次原子加。
func Select(clientHost string, clientPort uint32, endpointHost string, endpointPort uint32, require bool) (host string, port uint32, ok bool) {
	if usable(clientHost, clientPort) {
		countSelect(clientCounter)
		return clientHost, clientPort, true
	}
	if require {
		countSelect(rejectedCounter)
		return "", 0, false
	}
	if !usable(endpointHost, endpointPort) {
		countSelect(rejectedCounter)
		return "", 0, false
	}
	countSelect(fallbackCounter)
	return endpointHost, endpointPort, true
}

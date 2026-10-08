package logic

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	commonpb "proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/svc"
	"shared/clientendpoint"
	"shared/nodeinfo"

	"github.com/zeromicro/go-zero/core/logx"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
)

const (
	redirectTokenTTLSeconds = 300
	gateEtcdPrefix          = "GateNodeService.rpc"
)

// AssignGateForZone picks the least-loaded Gate in the target zone and signs
// a redirect token. Returns nil if no gate is available.
//
// 票据绑定持票者(player_id)与目标 zone(target_zone_id):目标 zone 的 gate / login
// 只接受本人持票,并且看到 target_zone_id == 本 zone 时不再按 home_zone 弹回
// (cross-zone-scene-travel.md CZ-8,不变量 4)。这里只负责签进去;验签方是 gate
// C++ 与 login,它们看到 player_id == 0 时按旧版签发者处理(兼容窗口)。
//
// 下发给客户端的 TargetGateIp/Port 是 gate 自报的客户端可达地址(NodeInfo.client_endpoint,
// 缺省回落 endpoint),选址规则与 login CandidatesForZone 同一份权威实现
// shared/clientendpoint(k8s-client-entry D76/D78)。本函数只做 I/O(读 etcd、取墙钟),
// 挑哪台、下发哪个地址、怎么签全在纯函数 signRedirectToGate 里。
func AssignGateForZone(ctx context.Context, svcCtx *svc.ServiceContext, targetZoneId uint32, playerID uint64) (*scene_manager.RedirectToGateInfo, error) {
	if svcCtx.Config.GateTokenSecret == "" {
		return nil, fmt.Errorf("GateTokenSecret not configured, cannot sign redirect token")
	}

	gates, err := fetchGateNodes(ctx, svcCtx, targetZoneId)
	if err != nil {
		return nil, fmt.Errorf("fetch gate nodes for zone %d: %w", targetZoneId, err)
	}
	return signRedirectToGate(gates, svcCtx.Config.RequireClientEndpoint,
		svcCtx.Config.GateTokenSecret, targetZoneId, playerID, time.Now())
}

// signRedirectToGate 从目标 zone 的 gate 节点里挑负载最低的一台并签重定向票据。
// 不碰 etcd、不读墙钟(now 由调用方传入),副作用只有日志与低基数计数,便于直接对出口做面向契约的测试。
//
// 前置条件:secret 非空(AssignGateForZone 在读 etcd 之前已校验)。
// 候选先经 selectGateTargets 选客户端地址并按地址去重,再按 player_count 取最小;
// 没有任何可下发地址的 gate 时返回错误(调用方映射成 ErrNoAvailableNode,不改任何状态)。
func signRedirectToGate(nodes []*commonpb.NodeInfo, requireClientEndpoint bool, secret string,
	targetZoneId uint32, playerID uint64, now time.Time) (*scene_manager.RedirectToGateInfo, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no gate nodes available for zone %d", targetZoneId)
	}
	targets := selectGateTargets(nodes, requireClientEndpoint)
	if len(targets) == 0 {
		return nil, fmt.Errorf("no gate with client-reachable address for zone %d (gates=%d require_client_endpoint=%v)",
			targetZoneId, len(nodes), requireClientEndpoint)
	}

	// Pick least-loaded gate.
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].node.GetPlayerCount() < targets[j].node.GetPlayerCount()
	})
	best := targets[0]

	// Build and sign the token (same format as Login service).
	expireTS := now.Unix() + redirectTokenTTLSeconds
	payload := &commonpb.GateTokenPayload{
		GateNodeId:      best.node.GetNodeId(),
		ZoneId:          best.node.GetZoneId(),
		ExpireTimestamp: expireTS,
		PlayerId:        playerID,
		TargetZoneId:    targetZoneId,
	}
	payloadBytes, err := proto.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal token payload: %w", err)
	}

	signature := signHMAC(secret, payloadBytes)

	return &scene_manager.RedirectToGateInfo{
		// 必须是 selectGateTargets 里 clientendpoint.Select 的结果,不能再读 best.node.Endpoint:
		// endpoint 是集群内身份(PodIP),external 模式下客户端连不上(D76)。
		TargetGateIp:   best.host,
		TargetGatePort: best.port,
		TokenPayload:   payloadBytes,
		TokenSignature: signature,
		TokenDeadline:  expireTS,
	}, nil
}

// gateTarget 是一个可下发给客户端的 gate 候选。
//   - node:该 gate 在 etcd 的 NodeInfo(集群内身份,票据里的 gate_node_id / zone_id 取自它);
//   - host/port:clientendpoint.Select 选出的客户端可达地址,下发给客户端只用这两个字段。
type gateTarget struct {
	node *commonpb.NodeInfo
	host string
	port uint32
}

// selectGateTargets 把 gate 节点翻成可下发给客户端的候选(k8s-client-entry D78)。
// 除一条合并的诊断日志与 shared/clientendpoint 的低基数计数外无 I/O:
//  1. 逐个节点经 clientendpoint.Select 选客户端地址:client_endpoint 可用(ip 非空且 port 在
//     1..65535)就用它;否则 requireClientEndpoint=false 时回落 endpoint,=true 时跳过该节点;
//  2. 再按「选出的客户端地址」DedupeNewest:同一地址只留 launch_time 最大的一条。
//     稳定地址(NodePort / DNS 名)下 gate 崩溃重启后,旧 NodeInfo 会残留到 etcd 租约 TTL,
//     两条记录客户端地址相同、node_id 不同;旧记录 player_count 往往更低,不去重就会被当成
//     最空闲的挑中,客户端连上的却是新 gate,被 token_gate_node_mismatch 拒绝。
//
// 返回保持输入的相对顺序;挑哪一台由调用方决定。
func selectGateTargets(nodes []*commonpb.NodeInfo, requireClientEndpoint bool) []gateTarget {
	targets := make([]gateTarget, 0, len(nodes))
	var skipped []string
	for _, n := range nodes {
		host, port, ok := clientendpoint.Select(
			n.GetClientEndpoint().GetIp(), n.GetClientEndpoint().GetPort(),
			n.GetEndpoint().GetIp(), n.GetEndpoint().GetPort(),
			requireClientEndpoint)
		if !ok {
			// Select 拒绝 = 没有能下发给客户端的地址,原因随 require 不同:
			//   - require=true:该 gate 没自报可用的 client_endpoint(查它的 CLIENT_ENDPOINT_SOURCE / 版本);
			//   - require=false:连 endpoint 都不可用(ip 为空或 port 为 0),是一条坏记录。
			// 两种情况都跳过它,不把客户端连不上的地址发下去(fail-closed)。endpoint 与 client_endpoint
			// 都打出来,运维才能从日志直接看出是哪条记录、坏在哪一个地址上(与 login 同一条日志口径一致)。
			skipped = append(skipped, fmt.Sprintf("node_id=%d zone=%d endpoint=%s:%d client_endpoint=%s:%d",
				n.GetNodeId(), n.GetZoneId(),
				n.GetEndpoint().GetIp(), n.GetEndpoint().GetPort(),
				n.GetClientEndpoint().GetIp(), n.GetClientEndpoint().GetPort()))
			continue
		}
		targets = append(targets, gateTarget{node: n, host: host, port: port})
	}
	if len(skipped) > 0 {
		// 这是部署配置错误或坏记录(require=true:external 模式 gate 应以 CLIENT_ENDPOINT_SOURCE=static
		// 启动,D79;require=false:endpoint 不可用的注册值),必须可见。
		// 只在出错时出现,且跨 zone 重定向远比登录稀疏,所以每次调用合并成一行、
		// 不另做节流。mmorpg_client_endpoint_select_total 由 shared/clientendpoint 统一计数,这里不另记指标。
		logx.Errorf("selectGateTargets: %d 台 gate 没有可下发给客户端的地址,已跳过(require_client_endpoint=%v): %v",
			len(skipped), requireClientEndpoint, skipped)
	}
	return clientendpoint.DedupeNewest(targets, gateTargetAddr, gateTargetLaunchTime)
}

// gateTargetAddr 是去重键:客户端实际要连的 host:port。
func gateTargetAddr(t gateTarget) string {
	return net.JoinHostPort(t.host, strconv.FormatUint(uint64(t.port), 10))
}

func gateTargetLaunchTime(t gateTarget) uint64 {
	return t.node.GetLaunchTime()
}

// fetchGateNodes queries etcd for Gate nodes in the given zone.
// 只负责 etcd 读取;逐条解析与过滤全在 decodeGateNodes,测试经它即覆盖本函数的真实解析路径。
func fetchGateNodes(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32) ([]*commonpb.NodeInfo, error) {
	gateNodeType := uint32(commonpb.ENodeType_GateNodeService)
	prefix := fmt.Sprintf("%s/zone/%d/node_type/%d/", gateEtcdPrefix, zoneId, gateNodeType)

	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := svcCtx.Etcd.Get(fetchCtx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	return decodeGateNodes(resp.Kvs), nil
}

// decodeGateNodes 把 etcd gate 前缀下的键值解析成 NodeInfo(fetchGateNodes 唯一的解析路径):
//   - 必须经 nodeinfo.Unmarshal 忽略未知字段(k8s-client-entry D77):滚动升级窗口里新版 gate 写的
//     NodeInfo 可能带本进程还不认识的字段,严格解析会把整台 gate 当坏记录丢掉,跨 zone 重定向随之无 gate 可选;
//   - JSON 语法错误 / 已知字段类型不符:记日志并跳过该条,不连累同批其余 gate;
//   - 没有 endpoint 的值不是可路由的 gate 注册记录,跳过(与抽出前的行为一致,不打日志)。
//
// 除解析失败时的诊断日志外无 I/O;返回保持 etcd 的键序。
func decodeGateNodes(kvs []*mvccpb.KeyValue) []*commonpb.NodeInfo {
	nodes := make([]*commonpb.NodeInfo, 0, len(kvs))
	for _, kv := range kvs {
		info := &commonpb.NodeInfo{}
		if err := nodeinfo.Unmarshal(kv.Value, info); err != nil {
			logx.Errorf("decodeGateNodes: invalid NodeInfo at key=%s: %v", string(kv.Key), err)
			continue
		}
		if info.Endpoint != nil {
			nodes = append(nodes, info)
		}
	}
	return nodes
}

// signHMAC computes HMAC-SHA256 and returns hex-encoded bytes (matches Login and Java Gateway).
func signHMAC(secret string, data []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

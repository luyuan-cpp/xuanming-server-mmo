package logic

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"

	commonpb "proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/clientendpoint"
	"shared/gatepick"
	"shared/nodeinfo"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
)

const (
	redirectTokenTTLSeconds = 300
	gateEtcdPrefix          = "GateNodeService.rpc"
)

// AssignGateForZone picks a Gate in the target zone (two random choices by
// load, skipping gates marked draining, stale or full) and signs a redirect
// token. Returns nil if no gate is available.
//
// 票据绑定持票者(player_id)与目标 zone(target_zone_id):目标 zone 的 gate / login
// 只接受本人持票,并且看到 target_zone_id == 本 zone 时不再按 home_zone 弹回
// (cross-zone-scene-travel.md CZ-8,不变量 4)。这里只负责签进去;验签方是 gate
// C++ 与 login,它们看到 player_id == 0 时按旧版签发者处理(兼容窗口)。
//
// 下发给客户端的 TargetGateIp/Port 是 gate 自报的客户端可达地址(NodeInfo.client_endpoint,
// 缺省回落 endpoint),选址规则与 login CandidatesForZone 同一份权威实现
// shared/clientendpoint(k8s-client-entry D76/D78)。本函数只做 I/O(读 etcd、判停写、读排空标记、取墙钟与随机源),
// 挑哪台、下发哪个地址、怎么签全在纯函数 signRedirectToGate 里。
//
// 停写(2026-11-05 起):每次直读 etcd 时把每条 gate 记录的 ModRevision 交给 svcCtx.RedirectGateRevisions,
// 同一个 ModRevision 在本进程持续超过 gatepick.StaleAfter 的 gate 判停写(staleGateIDs)。没有 watch 镜像,
// 判定只靠本进程读过的历史:进程刚起、或某个 zone 第一次被重定向时不判;读得稀疏时最早在第一次看到之后隔超过
// StaleAfter 的那次重定向才判出来。
func AssignGateForZone(ctx context.Context, svcCtx *svc.ServiceContext, targetZoneId uint32, playerID uint64) (*scene_manager.RedirectToGateInfo, error) {
	if svcCtx.Config.GateTokenSecret == "" {
		return nil, fmt.Errorf("GateTokenSecret not configured, cannot sign redirect token")
	}

	records, err := fetchGateNodes(ctx, svcCtx, targetZoneId)
	if err != nil {
		return nil, fmt.Errorf("fetch gate nodes for zone %d: %w", targetZoneId, err)
	}
	// 读完 etcd 立即取时刻:停写时长按「这次读返回」算,排空标记的 MGET 不算进去。
	stale := staleGateIDs(&svcCtx.RedirectGateRevisions, targetZoneId, records, time.Now())
	gates := gateRecordNodes(records)
	marks := gateMarks{
		draining: drainingGateIDs(ctx, svcCtx.Redis, gates),
		stale:    stale,
	}
	return signRedirectToGate(gates, marks, svcCtx.Config.RequireClientEndpoint,
		svcCtx.Config.GateTokenSecret, targetZoneId, playerID, time.Now(), mathrand.IntN)
}

// gateMarks 是 AssignGateForZone 经 I/O 得到的两类按 node_id 记的标记,纯函数 signRedirectToGate 据此过滤。
// 两个 map 都可以为 nil(没有标记)。
type gateMarks struct {
	draining map[uint32]bool // 正在排空(Redis 标记,drainingGateIDs)
	stale    map[uint32]bool // 注册记录停写(本进程的直读历史,staleGateIDs)
}

// signRedirectToGate 从目标 zone 的 gate 节点里挑一台并签重定向票据。
// 不碰 etcd 与 Redis、不读墙钟(now 由调用方传入)、随机源 intn 由调用方传入,副作用只有日志与低基数计数,
// 便于直接对出口做面向契约的测试。
//
// 前置条件:secret 非空(AssignGateForZone 在读 etcd 之前已校验)。
// 顺序与 login 的 CandidatesForZone + PickGate 一致:先经 selectGateTargets 选客户端地址并按地址去重,再剔除 marks.draining
// 里的 gate(先去重后过滤:排空标记打在活着的那台上,先过滤会让同地址的陈旧影子顶上来),再剔除停写与满载的 gate
// (eligibleTargets,规则在 shared/gatepick.Eligible,2026-11-05 起),最后按 player_count 随机二选一
// (shared/gatepick,2026-10-18 起;此前确定性取最小,一个上报周期里重定向的人全落同一台)。
// 两层过滤各自在全部不合格时回落,不因过滤让重定向无 gate 可选。
// 没有任何可下发地址的 gate 时返回错误(调用方映射成 ErrNoAvailableNode,不改任何状态)。
func signRedirectToGate(nodes []*commonpb.NodeInfo, marks gateMarks, requireClientEndpoint bool, secret string,
	targetZoneId uint32, playerID uint64, now time.Time, intn func(int) int) (*scene_manager.RedirectToGateInfo, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no gate nodes available for zone %d", targetZoneId)
	}
	targets := selectGateTargets(nodes, requireClientEndpoint)
	if len(targets) == 0 {
		return nil, fmt.Errorf("no gate with client-reachable address for zone %d (gates=%d require_client_endpoint=%v)",
			targetZoneId, len(nodes), requireClientEndpoint)
	}
	targets = filterDrainingTargets(targets, marks.draining, targetZoneId)
	targets = eligibleTargets(targets, marks.stale, targetZoneId, now)

	idx, _ := gatepick.TwoChoices(targets, func(t gateTarget) uint32 { return t.node.GetPlayerCount() }, intn)
	best := targets[idx]

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
		logx.Errorf("selectGateTargets: %d gates lack client-advertisable addresses; skipped (require_client_endpoint=%v): %v",
			len(skipped), requireClientEndpoint, skipped)
	}
	return clientendpoint.DedupeNewest(targets, gateTargetAddr, gateTargetLaunchTime)
}

// gateDrainingKeyFmt 是 gate 排空标记的键(值是标记时间)。权威定义在 login
// (go/login/internal/logic/pkg/loginqueue/gatedrain.go 的 GateDrainingKeyFmt),写者是 tools/scripts/k8s_gate_drain.ps1;
// scene_manager 拿不到 login 的包,这里抄一份,由 tools/scripts/tests/k8s_gate_drain.tests.ps1 钉住三处一致。
//
// 标记写在 login 的 Node.RedisClient 所在实例;scene_manager 经自己的 Redis 连接读它,今天 K8s 与本地两者是同一实例、同为 DB 0。
// 两个隐含前提,任一被打破都会让这里静默读不到标记、退回不过滤(放行):
//   - 拆 Redis(G4a)把排空标记挪到 redis-queue 时,scene_manager 这条读要一起指过去;
//   - go-zero 的 RedisConf 没有 DB 字段,scene_manager 只能读 DB 0;login 的 Node.RedisClient.DB 改成非 0 时要同步处理。
const gateDrainingKeyFmt = "gate:%d:draining"

// drainingGateIDs 一次 MGET 查出哪些 gate 正在排空。失败方向是放行(返回 nil、打 ERROR),与 login 的
// loginqueue.DrainingGates 同语义:跨 zone 重定向因为读不到标记就拒绝,比分到一台稍后排空的 gate 更糟。
// 只查有 endpoint 的节点(fetchGateNodes 已过滤),键序与 nodes 对应。
func drainingGateIDs(ctx context.Context, rdb *redis.Redis, nodes []*commonpb.NodeInfo) map[uint32]bool {
	if rdb == nil || len(nodes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(nodes))
	for _, n := range nodes {
		keys = append(keys, fmt.Sprintf(gateDrainingKeyFmt, n.GetNodeId()))
	}
	values, err := rdb.MgetCtx(ctx, keys...)
	if err != nil {
		logx.Errorf("drainingGateIDs: query draining gates failed, treating all as available: %v", err)
		return nil
	}
	draining := make(map[uint32]bool, len(nodes))
	for i, v := range values {
		// go-zero 的 MGET 把不存在的键返回成空串;标记值是 Unix 秒,不会是空串。
		if i < len(nodes) && v != "" {
			draining[nodes[i].GetNodeId()] = true
		}
	}
	return draining
}

// filterDrainingTargets 剔除正在排空的 gate。**全部都在排空时原样返回**并打 ERROR,与 login 的
// loginqueue.FilterDrainingGates 同语义:整个 zone 都被标了排空多半是误操作,此时拒绝重定向比分到待缩容的 gate 更糟。
func filterDrainingTargets(targets []gateTarget, draining map[uint32]bool, targetZoneId uint32) []gateTarget {
	if len(draining) == 0 || len(targets) == 0 {
		return targets
	}
	kept := make([]gateTarget, 0, len(targets))
	for _, t := range targets {
		if !draining[t.node.GetNodeId()] {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		logx.Errorf("filterDrainingTargets: every gate candidate (%d) in zone %d is marked draining; ignoring the marks "+
			"so the redirect can still land — check whether a scale-in marked the whole zone by mistake", len(targets), targetZoneId)
		return targets
	}
	return kept
}

// gateRedirectLogEvery:eligibleTargets 两条日志各自的最小间隔。跨 zone 重定向平时稀疏,但一次大规模跨区(活动开场)
// 会让同一台卡住的 gate 每次重定向都被剔一次;持续多久看指标,日志只负责告诉运维是哪几台。
const gateRedirectLogEvery = 30 * time.Second

var (
	staleRedirectGateLog redirectLogThrottle
	redirectFallbackLog  redirectLogThrottle
)

// eligibleTargets 剔除停写与满载的 gate,保持原顺序;全部不合格时回落全集(规则在 shared/gatepick.Eligible,
// 与 login 挑 gate 同一份;回落的理由见那里)。停写看 stale(按 node_id,staleGateIDs 判),满载看 NodeInfo 自报的
// max_connections(人数到 90%;0 = 没声明,不判)。副作用只有指标与节流日志,now 只用于日志节流。
func eligibleTargets(targets []gateTarget, stale map[uint32]bool, targetZoneId uint32, now time.Time) []gateTarget {
	kept, ex := gatepick.Eligible(targets, func(t gateTarget) gatepick.State {
		return gatepick.State{
			PlayerCount:    t.node.GetPlayerCount(),
			MaxConnections: t.node.GetMaxConnections(),
			Stale:          stale[t.node.GetNodeId()],
		}
	})
	if ex.Stale > 0 {
		metrics.ObserveGateRedirectExcluded(targetZoneId, gatepick.ReasonStale, ex.Stale)
		if staleRedirectGateLog.allow(now, gateRedirectLogEvery) {
			logx.Errorf("eligibleTargets: gates of zone %d left out of the redirect because their etcd record has not been "+
				"rewritten for %s (likely hung while the lease lives on): node_ids=%v; further reports are suppressed for %s, "+
				"see scene_manager_gate_redirect_excluded_total{reason=\"stale\"}",
				targetZoneId, gatepick.StaleAfter, staleTargetIDs(targets, stale), gateRedirectLogEvery)
		}
	}
	if ex.Full > 0 {
		metrics.ObserveGateRedirectExcluded(targetZoneId, gatepick.ReasonFull, ex.Full)
	}
	if ex.FellBack {
		metrics.ObserveGateRedirectFallback(targetZoneId, ex.FallbackReason())
		if redirectFallbackLog.allow(now, gateRedirectLogEvery) {
			logx.Errorf("eligibleTargets: every gate of zone %d is stale or full (stale=%d full=%d of %d); falling back to all "+
				"of them so the redirect can still land; further reports are suppressed for %s, "+
				"see scene_manager_gate_redirect_fallback_total",
				targetZoneId, ex.Stale, ex.Full, len(targets), gateRedirectLogEvery)
		}
	}
	return kept
}

func staleTargetIDs(targets []gateTarget, stale map[uint32]bool) []uint32 {
	ids := make([]uint32, 0, len(stale))
	for _, t := range targets {
		if stale[t.node.GetNodeId()] {
			ids = append(ids, t.node.GetNodeId())
		}
	}
	return ids
}

// redirectLogThrottle 让一条日志每个窗口至多打一次,并发安全。生产传入的 now 来自 time.Now(),带单调时钟读数,
// 墙钟回拨不会把日志压住。
type redirectLogThrottle struct {
	mu   sync.Mutex
	last time.Time
}

func (l *redirectLogThrottle) allow(now time.Time, every time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) >= 0 && now.Sub(l.last) < every {
		return false
	}
	l.last = now
	return true
}

// staleGateIDs 把这一次直读到的 gate 记录交给 tracker,返回判为停写的 gate(按 node_id;没有时为 nil)。
// readAt 取这次读返回之后的时刻。每次读都要过 tracker,哪怕读到零条:零条会清掉该 zone 的历史。
func staleGateIDs(tracker *gatepick.RevisionTracker, zoneID uint32, records []gateRecord, readAt time.Time) map[uint32]bool {
	revs := make([]gatepick.Revision, len(records))
	for i, r := range records {
		revs[i] = gatepick.Revision{Key: r.key, ModRev: r.modRev}
	}
	var stale map[uint32]bool
	for i, s := range tracker.Observe(zoneID, revs, readAt) {
		if !s {
			continue
		}
		if stale == nil {
			stale = make(map[uint32]bool)
		}
		stale[records[i].node.GetNodeId()] = true
	}
	return stale
}

// gateTargetAddr 是去重键:客户端实际要连的 host:port。
func gateTargetAddr(t gateTarget) string {
	return net.JoinHostPort(t.host, strconv.FormatUint(uint64(t.port), 10))
}

func gateTargetLaunchTime(t gateTarget) uint64 {
	return t.node.GetLaunchTime()
}

// gateFetchTimeout 是跨 zone 重定向直读 etcd gate 前缀的时限。它也是停写判定的误差项:读返回的时刻比读的线性化点
// 至多晚这么久,健康 gate 的停写时长上界是「一个上报周期 + 本值」,必须小于 gatepick.StaleAfter(单测钉住)。
const gateFetchTimeout = 5 * time.Second

// gateRecord 是 etcd gate 前缀下的一条注册记录:解析后的 NodeInfo,外加停写判定要用的键与 ModRevision。
type gateRecord struct {
	key    string
	modRev int64
	node   *commonpb.NodeInfo
}

// gateRecordNodes 取出记录里的 NodeInfo,保持顺序。
func gateRecordNodes(records []gateRecord) []*commonpb.NodeInfo {
	nodes := make([]*commonpb.NodeInfo, 0, len(records))
	for _, r := range records {
		nodes = append(nodes, r.node)
	}
	return nodes
}

// fetchGateNodes queries etcd for Gate nodes in the given zone.
// 只负责 etcd 读取;逐条解析与过滤全在 decodeGateNodes,测试经它即覆盖本函数的真实解析路径。
func fetchGateNodes(ctx context.Context, svcCtx *svc.ServiceContext, zoneId uint32) ([]gateRecord, error) {
	gateNodeType := uint32(commonpb.ENodeType_GateNodeService)
	prefix := fmt.Sprintf("%s/zone/%d/node_type/%d/", gateEtcdPrefix, zoneId, gateNodeType)

	fetchCtx, cancel := context.WithTimeout(ctx, gateFetchTimeout)
	defer cancel()

	resp, err := svcCtx.Etcd.Get(fetchCtx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	return decodeGateNodes(resp.Kvs), nil
}

// decodeGateNodes 把 etcd gate 前缀下的键值解析成 gate 记录(fetchGateNodes 唯一的解析路径):
//   - 必须经 nodeinfo.Unmarshal 忽略未知字段(k8s-client-entry D77):滚动升级窗口里新版 gate 写的
//     NodeInfo 可能带本进程还不认识的字段,严格解析会把整台 gate 当坏记录丢掉,跨 zone 重定向随之无 gate 可选;
//   - JSON 语法错误 / 已知字段类型不符:记日志并跳过该条,不连累同批其余 gate;
//   - 没有 endpoint 的值不是可路由的 gate 注册记录,跳过(与抽出前的行为一致,不打日志);
//   - 每条带上 etcd 键与 ModRevision(2026-11-05 起),给停写判定用(staleGateIDs)。
//
// 除解析失败时的诊断日志外无 I/O;返回保持 etcd 的键序。
func decodeGateNodes(kvs []*mvccpb.KeyValue) []gateRecord {
	records := make([]gateRecord, 0, len(kvs))
	for _, kv := range kvs {
		info := &commonpb.NodeInfo{}
		if err := nodeinfo.Unmarshal(kv.Value, info); err != nil {
			logx.Errorf("decodeGateNodes: invalid NodeInfo at key=%s: %v", string(kv.Key), err)
			continue
		}
		if info.Endpoint != nil {
			records = append(records, gateRecord{key: string(kv.Key), modRev: kv.ModRevision, node: info})
		}
	}
	return records
}

// signHMAC computes HMAC-SHA256 and returns hex-encoded bytes (matches Login and Java Gateway).
func signHMAC(secret string, data []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

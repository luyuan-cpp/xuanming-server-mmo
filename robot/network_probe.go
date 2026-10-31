package main

// 地域探测(协议 proto/match/match_network.proto;match 侧 go/match/internal/logic/network_probe.go,
// battle 侧 cpp/nodes/battle/client/battle_client_edge.cpp 的 OnProbeStart / SendProbeChallenge / OnProbeEcho)。
//
// match 只剩 SQL 权威路径之后,玩家进匹配(JoinQueue、整队开战、活动开局)必须持有一份本会话测出来、尚未过期的探测结果:
//
//  1. 经大厅连接 GetNetworkProbePlan:拿计划号,以及每个机房一个 battle 公网端点 + match 签名的授权(grant);
//  2. 对每个端点单独开一条 TCP:发 NetworkProbeStart(grant),battle 逐次发 3 个 NetworkProbeChallenge,每个都原样回
//     NetworkProbeEcho;第 3 次回显后 battle 回 NetworkProbeComplete(它自己签名的回执)并关连接。
//     RTT 由 battle 量,客户端没有可填写的字段,要做的只是及时回显;
//  3. 经大厅连接 CommitNetworkProbe 提交全部回执。JoinQueue 带计划号;整队与活动开局读服务端存的这份结果,请求里不带,
//     所以开局前每名成员都要自己探测过。
//
// 第 2 步不走 pkg.GameClient,也不用 muduo.Client,只借 muduo.TcpCodec 解码,原因有两个:
//   - 编码不能复用:TcpCodec.Encode 写的是 Descriptor().Name(),不带包名。玩家协议的消息恰好都没有 package,所以一直没事;
//     探测消息在 package match 里,battle 的 ProtobufCodec 按全名(match.NetworkProbeStart)反射建对象,收到短名
//     一律 kUnknownMessageType 并断连。全名编码见 encodeNetworkProbeFrame;
//   - 连接不能复用:muduo.Client 拨不通时无限重拨、Dial 没有超时、Recv 阻塞在永不 close 的通道上
//     (battleDirectConnTimeout 的注释写过这条路径会让冒烟进程挂死)。单次 net.Conn 加一个绝对截止时间就没有这些问题:
//     到期后读写都返回错误,不留 goroutine,按玩家反复探测也不会累积。
//
// 任一机房探测失败,整次探测按失败返回,不提交部分回执。协议允许缺测(缺测的机房视为不可用),但冒烟要暴露
// 「注册的公网地址从 robot 所在机器不可达」这类部署问题,跳过它只会让后面的成局落到别的机房,问题被藏起来。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/match"
	"robot/generated/pb/game"
	"robot/metrics"
	"robot/pkg"
)

const (
	// networkProbeTargetTimeout 是单个机房的预算:建连 + Start + 3 轮往返 + Complete。正常是毫秒级。
	// 与直连握手同值;battle 给未验证连接的期限(kHandshakeTimeoutSec)也是 10s。
	networkProbeTargetTimeout = battleDirectConnTimeout
	// networkProbeRPCTimeout 是 GetNetworkProbePlan / CommitNetworkProbe 各自等回包的预算,取值理由同 orderingSmokeReplyTimeout。
	networkProbeRPCTimeout = 15 * time.Second
	// networkProbeRefreshMargin:结果离过期不足这么久就重新探测。排队票的有效期就是探测结果的有效期
	// (authority.Join 把 expires_ms 抄进票),快过期时入队,票可能在等对手途中就过期了,所以取开战等待预算再加余量。
	// 服务端有效期比它还短时(ProbeTTLSeconds 下限 30s)等于每次进匹配都重测,仍然正确。
	networkProbeRefreshMargin = battleSmokeStartTimeout + 10*time.Second
	// 帧长上下限。下限同 ProtobufCodec 的 kMinMessageLen(nameLen + 至少 2 字节名字 + 校验和);
	// 上限远大于真实帧(grant / 回执的 payload 不超过 8KB),只防对端发错协议时按乱长度分配内存。
	networkProbeMinFrameBytes = 10
	networkProbeMaxFrameBytes = 64 * 1024
)

// robotNetworkProbes 是进程内的探测结果缓存:一个 Player 打多场时不必每场重测。
var robotNetworkProbes = newNetworkProbeCache(time.Now)

// ensureNetworkProbe 保证 gc 这个会话持有尚未临近过期的探测结果,返回 JoinQueue 要带的 network_plan_id。
// 结果按玩家缓存,没有可用结果时才走一轮完整探测;失败返回 error,不缓存。
//
// 必须在大厅连接的 RecvLoop 已经起来之后、且不在 RecvLoop goroutine 上调用(smokeCaller 的约束);
// 同一玩家同一时刻只能有一个调用方:后一次 GetNetworkProbePlan 会让前一份计划作废,这与「一个机器人只由一个 goroutine 驱动」一致。
// stats 可为 nil。
func ensureNetworkProbe(gc *pkg.GameClient, stats *metrics.Stats, account string) (string, error) {
	host, port := gc.GateAddr()
	session := networkProbeSession{client: gc, gateHost: host, gatePort: port}
	planID, err := robotNetworkProbes.ensure(gc.PlayerId, session, func() (networkProbeResult, error) {
		lobby := smokeNetworkProbeLobby{caller: newSmokeCaller(gc, stats, account, networkProbeRPCTimeout, nil)}
		result, err := runNetworkProbe(lobby, networkProbeTargetTimeout)
		if err != nil {
			return networkProbeResult{}, err
		}
		zap.L().Info("[network-probe] probe committed",
			zap.String("account", account),
			zap.Duration("valid_for", result.remaining),
			zap.Any("rtt_ms_by_region", result.rttMsByRegion))
		return result, nil
	})
	if err != nil {
		return "", fmt.Errorf("network probe account=%s: %w", account, err)
	}
	return planID, nil
}

// networkProbeResult 是一轮成功提交的探测。
type networkProbeResult struct {
	planID string
	// remaining 是结果还能用多久:CommitNetworkProbe 回的 expires_at_ms 减去回执里最晚的 measured_at_ms。
	// 两个都是服务端时刻,不拿本机时钟与服务端时钟比。
	remaining     time.Duration
	rttMsByRegion map[uint32]uint32
}

// networkProbeLobby 是探测对大厅连接的全部依赖。生产实现是 smokeNetworkProbeLobby,测试用替身。
// 返回 error 表示没拿到响应体;响应体里的业务拒绝由 runNetworkProbe 判断。
type networkProbeLobby interface {
	plan() (*match.GetNetworkProbePlanResponse, error)
	commit(*match.CommitNetworkProbeRequest) (*match.CommitNetworkProbeResponse, error)
}

// smokeNetworkProbeLobby 经 smokeCaller 发这两个 RPC,回包按请求号认领,不进场景的 onMessage。
type smokeNetworkProbeLobby struct {
	caller *smokeCaller
}

func (l smokeNetworkProbeLobby) plan() (*match.GetNetworkProbePlanResponse, error) {
	response := &match.GetNetworkProbePlanResponse{}
	if err := l.call(game.MatchServiceGetNetworkProbePlanMessageId, &match.GetNetworkProbePlanRequest{}, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (l smokeNetworkProbeLobby) commit(request *match.CommitNetworkProbeRequest) (*match.CommitNetworkProbeResponse, error) {
	response := &match.CommitNetworkProbeResponse{}
	if err := l.call(game.MatchServiceCommitNetworkProbeMessageId, request, response); err != nil {
		return nil, err
	}
	return response, nil
}

func (l smokeNetworkProbeLobby) call(messageId uint32, request, response proto.Message) error {
	envelopeTip, err := l.caller.call(messageId, request, response)
	if err != nil {
		return err
	}
	if envelopeTip != 0 {
		// match 对会话、回执身份与时效的拒绝是 gRPC 错误,到客户端就是这条信封,细节只在 match 日志里。
		return fmt.Errorf("envelope rejected tip=%d (match refused the call or no match instance is reachable; see match and client_rpc_router logs)", envelopeTip)
	}
	return nil
}

// runNetworkProbe 走完一轮:拿计划 → 逐个机房采样 → 提交回执。targetTimeout 是每个机房的预算。
// 任一步失败立即返回 error,此时没有提交任何回执。
func runNetworkProbe(lobby networkProbeLobby, targetTimeout time.Duration) (networkProbeResult, error) {
	plan, err := lobby.plan()
	if err != nil {
		return networkProbeResult{}, fmt.Errorf("GetNetworkProbePlan: %w", err)
	}
	if tip := plan.GetErrorMessage(); tip.GetId() != 0 {
		return networkProbeResult{}, fmt.Errorf("GetNetworkProbePlan rejected tip=%d params=%v", tip.GetId(), tip.GetParameters())
	}
	if plan.GetPlanId() == "" || len(plan.GetTargets()) == 0 {
		return networkProbeResult{}, fmt.Errorf("GetNetworkProbePlan returned no usable plan: plan_id=%q targets=%d",
			plan.GetPlanId(), len(plan.GetTargets()))
	}

	receipts := make([]*match.SignedNetworkProbe, 0, len(plan.GetTargets()))
	rttMsByRegion := make(map[uint32]uint32, len(plan.GetTargets()))
	var latestMeasuredMs uint64
	for _, target := range plan.GetTargets() {
		signed, receipt, err := sampleNetworkProbeTarget(target, targetTimeout)
		if err != nil {
			return networkProbeResult{}, fmt.Errorf("region=%d endpoint=%s: %w",
				target.GetPhysicalRegion(), networkProbeEndpoint(target), err)
		}
		receipts = append(receipts, signed)
		rttMsByRegion[target.GetPhysicalRegion()] = receipt.GetRttMs()
		latestMeasuredMs = max(latestMeasuredMs, receipt.GetMeasuredAtMs())
	}

	committed, err := lobby.commit(&match.CommitNetworkProbeRequest{PlanId: plan.GetPlanId(), Receipts: receipts})
	if err != nil {
		return networkProbeResult{}, fmt.Errorf("CommitNetworkProbe: %w", err)
	}
	if tip := committed.GetErrorMessage(); tip.GetId() != 0 {
		return networkProbeResult{}, fmt.Errorf("CommitNetworkProbe rejected tip=%d params=%v", tip.GetId(), tip.GetParameters())
	}
	if committed.GetExpiresAtMs() <= latestMeasuredMs {
		return networkProbeResult{}, fmt.Errorf("CommitNetworkProbe returned expires_at_ms=%d, not after the latest measured_at_ms=%d",
			committed.GetExpiresAtMs(), latestMeasuredMs)
	}
	return networkProbeResult{
		planID:        plan.GetPlanId(),
		remaining:     time.Duration(committed.GetExpiresAtMs()-latestMeasuredMs) * time.Millisecond,
		rttMsByRegion: rttMsByRegion,
	}, nil
}

// sampleNetworkProbeTarget 直连一个 battle 公网端点走完采样,返回提交用的签名回执与解开的回执内容。
// 整个过程(含建连)受同一个截止时间约束;battle 拒绝 grant 时直接断连,这里读到 EOF 或连接重置。
func sampleNetworkProbeTarget(target *match.NetworkProbeTarget, timeout time.Duration) (*match.SignedNetworkProbe, *match.NetworkProbeReceipt, error) {
	if target.GetHost() == "" || target.GetPort() == 0 || target.GetPort() > 65535 {
		return nil, nil, errors.New("plan target has an invalid endpoint")
	}
	deadline := time.Now().Add(timeout)
	conn, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", networkProbeEndpoint(target))
	if err != nil {
		return nil, nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, nil, fmt.Errorf("set deadline: %w", err)
	}
	if err := writeNetworkProbeFrame(conn, &match.NetworkProbeStart{Grant: target.GetGrant()}); err != nil {
		return nil, nil, fmt.Errorf("send NetworkProbeStart: %w", err)
	}

	codec := &muduo.TcpCodec{}
	echoed := 0
	for {
		message, err := readNetworkProbeFrame(conn, codec)
		if err != nil {
			return nil, nil, fmt.Errorf("read after %d echoes: %w (EOF or reset = battle closed the connection, usually a rejected grant; "+
				"deadline = endpoint unreachable from this host, or battle went silent)", echoed, err)
		}
		switch m := message.(type) {
		case *match.NetworkProbeChallenge:
			echo := &match.NetworkProbeEcho{Nonce: m.GetNonce(), SampleIndex: m.GetSampleIndex()}
			if err := writeNetworkProbeFrame(conn, echo); err != nil {
				return nil, nil, fmt.Errorf("send NetworkProbeEcho sample=%d: %w", m.GetSampleIndex(), err)
			}
			echoed++
		case *match.NetworkProbeComplete:
			signed := m.GetReceipt()
			if len(signed.GetPayload()) == 0 || len(signed.GetSignature()) == 0 {
				return nil, nil, errors.New("NetworkProbeComplete carries no signed receipt")
			}
			receipt := &match.NetworkProbeReceipt{}
			if err := proto.Unmarshal(signed.GetPayload(), receipt); err != nil {
				return nil, nil, fmt.Errorf("decode receipt: %w", err)
			}
			return signed, receipt, nil
		default:
			return nil, nil, fmt.Errorf("unexpected message %T on the probe connection", message)
		}
	}
}

func networkProbeEndpoint(target *match.NetworkProbeTarget) string {
	return net.JoinHostPort(target.GetHost(), strconv.FormatUint(uint64(target.GetPort()), 10))
}

// encodeNetworkProbeFrame 按 battle 的 ProtobufCodec::fillEmptyBuffer 编一帧:
//
//	[4B len][4B nameLen][全名 + '\0'][body][4B adler32(nameLen、名字、body)]
//
// len 不含自身,nameLen 含结尾的 '\0',都是大端。名字必须是带包名的全名,见文件头。
func encodeNetworkProbeFrame(message proto.Message) ([]byte, error) {
	name := string(message.ProtoReflect().Descriptor().FullName())
	body, err := proto.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", name, err)
	}
	nameLen := len(name) + 1
	frame := make([]byte, 0, 4+4+nameLen+len(body)+4)
	frame = binary.BigEndian.AppendUint32(frame, uint32(4+nameLen+len(body)+4))
	frame = binary.BigEndian.AppendUint32(frame, uint32(nameLen))
	frame = append(frame, name...)
	frame = append(frame, 0)
	frame = append(frame, body...)
	return binary.BigEndian.AppendUint32(frame, adler32.Checksum(frame[4:])), nil
}

func writeNetworkProbeFrame(conn net.Conn, message proto.Message) error {
	frame, err := encodeNetworkProbeFrame(message)
	if err != nil {
		return err
	}
	_, err = conn.Write(frame)
	return err
}

// readNetworkProbeFrame 读一整帧并解码。返回值是生成的消息指针,调用方按具体类型分派。
func readNetworkProbeFrame(conn net.Conn, codec *muduo.TcpCodec) (any, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size < networkProbeMinFrameBytes || size > networkProbeMaxFrameBytes {
		return nil, fmt.Errorf("frame length %d out of bounds", size)
	}
	frame := make([]byte, 4+int(size))
	copy(frame, header[:])
	if _, err := io.ReadFull(conn, frame[4:]); err != nil {
		return nil, err
	}
	// Decode 按帧里的 nameLen 直接切片,越界会 panic,先挡住。
	if nameLen := binary.BigEndian.Uint32(frame[4:8]); nameLen < 2 || nameLen > size-8 {
		return nil, fmt.Errorf("frame name length %d out of bounds (frame length %d)", nameLen, size)
	}
	message, consumed, err := codec.Decode(frame)
	if err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	if message == nil || consumed != uint32(len(frame)) {
		return nil, fmt.Errorf("decode frame: consumed %d of %d bytes", consumed, len(frame))
	}
	return message, nil
}

// networkProbeSession 标识探测结果绑定的会话。服务端把结果绑在 session_binding(gate 实例 + 会话代数 + 角色)上,
// 重新登录(新 GameClient)或跟随 gate 重定向(同一 GameClient 换 gate)之后,旧结果在 JoinQueue 上会被判缺探测。
type networkProbeSession struct {
	client   *pkg.GameClient
	gateHost string
	gatePort int
}

type networkProbeCacheEntry struct {
	session  networkProbeSession
	planID   string
	deadline time.Time
}

// networkProbeCache 按玩家缓存探测结果,到 deadline(有效期减 networkProbeRefreshMargin)为止复用。
// mu 只保护 entries;探测在锁外跑,不同玩家互不阻塞。now 是注入的时钟,生产用 time.Now(带单调读数)。
type networkProbeCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[uint64]networkProbeCacheEntry
}

func newNetworkProbeCache(now func() time.Time) *networkProbeCache {
	return &networkProbeCache{now: now, entries: make(map[uint64]networkProbeCacheEntry)}
}

// ensure 返回 playerID 在 session 上可用的计划号;缓存没有可用结果时调用 run 探测一次。run 失败不写缓存。
func (c *networkProbeCache) ensure(playerID uint64, session networkProbeSession, run func() (networkProbeResult, error)) (string, error) {
	c.mu.Lock()
	entry, ok := c.entries[playerID]
	c.mu.Unlock()
	if ok && entry.session == session && c.now().Before(entry.deadline) {
		return entry.planID, nil
	}
	// 从发起前起算:remaining 是采样时刻之后的剩余量,起点取早不取晚,算出的 deadline 只会偏保守。
	started := c.now()
	result, err := run()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.entries[playerID] = networkProbeCacheEntry{
		session:  session,
		planID:   result.planID,
		deadline: started.Add(result.remaining - networkProbeRefreshMargin),
	}
	c.mu.Unlock()
	return result.planID, nil
}

package logic

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	base "proto/common/base"
	pb "proto/player_locator"
	"shared/safego"
)

// 会话对账扫描:兜住「gate 整机崩溃 → TCP 断开回调不执行 → SetDisconnecting
// 永远不来 → 会话永久 ONLINE」的结构性缺口。
//
// 会话进入清理状态机的唯一入口是 SetDisconnecting(它的 Lua 才会 ZADD 租约
// ZSET,LeaseMonitor 只消费该 ZSET),而会话键是无 TTL 写入的。断线通知链
// 三处都可能整段蒸发:gate 崩溃(回调不执行)、断线瞬间无 login 节点
// (gate 静默跳过)、login→locator RPC 穷尽重试仍失败。落到这里的会话没有
// 任何自愈路径,只能靠人工 SCAN(release-checklist #B-1)。
//
// 对账原则(fail-closed,宁可漏扫不可误杀):
//  1. 只看 State==ONLINE 且 GateInstanceId != "" 的会话;
//  2. gate 存活性以 etcd 注册表为准(C++ gate 以 lease 注册 NodeInfo,
//     node_uuid 即会话里的 GateInstanceId);etcd 列举失败 → 本轮整体跳过;
//  3. 必须**连续两轮**都不在存活集内才动手 —— 单轮缺席可能只是 etcd 视图
//     抖动或 gate 正在重启重注册;
//  4. 动手 = 走与 SetDisconnecting 完全相同的 CAS 转换(整字节 CAS +
//     版本号递进 + ZADD 租约),期间玩家经新 gate 重连/重登会改写会话字节,
//     CAS 自动失败成 no-op —— 绝不会踢掉活人。
//
// 多实例安全:两个 locator 同时扫,后动手者 CAS 失败,无害。

const (
	defaultReconcileInterval = 60 * time.Second
	// reconcileScanBatch 是 SCAN 每次游标步进的 COUNT 提示。
	reconcileScanBatch = 500
)

// gateRpcPrefix 是 C++ gate 在 etcd 里的注册前缀(与 internal/node 的
// rpcPrefix 同构;那边是私有函数,这里按同一约定重建)。
func gateRpcPrefix() string {
	return fmt.Sprintf("%s.rpc/", base.ENodeType_name[int32(base.ENodeType_GateNodeService)])
}

// StartSessionReconciler 启动对账扫描循环。intervalSeconds<0 表示禁用;
// 0 用默认 60s。阻塞运行,调用方 go 出去。
func StartSessionReconciler(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	etcdEndpoints []string,
	etcdDialTimeout time.Duration,
	intervalSeconds int64,
) {
	if intervalSeconds < 0 {
		logx.Info("SessionReconciler disabled by config")
		return
	}
	interval := defaultReconcileInterval
	if intervalSeconds > 0 {
		interval = time.Duration(intervalSeconds) * time.Second
	}

	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   etcdEndpoints,
		DialTimeout: etcdDialTimeout,
	})
	if err != nil {
		// 连不上 etcd 只能放弃对账(fail-closed:不清理),但要喊出来 ——
		// 这是 gate 崩溃兜底的唯一自动化路径。
		logx.Errorf("SessionReconciler: etcd connect failed, reconciler NOT running: %v", err)
		return
	}
	defer etcdCli.Close()

	logx.Infof("SessionReconciler started: interval=%v", interval)

	// suspects: playerID -> 上一轮缺席时的 GateInstanceId。
	// 连续两轮同一实例缺席才动手;实例变了(玩家换 gate 重连)重新计数。
	suspects := make(map[uint64]string)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logx.Info("SessionReconciler stopped")
			return
		case <-ticker.C:
			// 与 LeaseMonitor 同理:recover 收到"一轮"。对账扫描本身就是兜底链路,
			// 它自己因为一条畸形会话 panic 而永久停摆的话,gate 崩溃留下的永久
			// ONLINE 会话就再也没有自动清理路径(只剩人工 SCAN)。
			safego.Run("player_locator.session_reconciler.round", func() {
				reconcileOnce(ctx, svcCtx, etcdCli, suspects)
			})
		}
	}
}

func reconcileOnce(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	etcdCli *clientv3.Client,
	suspects map[uint64]string,
) {
	liveGates, err := listLiveGateInstances(ctx, etcdCli)
	if err != nil {
		// 看不清存活集就什么都不做(fail-closed),下一轮再试。
		logx.Errorf("SessionReconciler: list live gates failed, skip round: %v", err)
		return
	}

	scanned, orphaned := 0, 0
	var cursor uint64
	for {
		keys, next, err := svcCtx.RedisClient.Scan(ctx, cursor, sessionKeyPrefix+"*", reconcileScanBatch).Result()
		if err != nil {
			logx.Errorf("SessionReconciler: scan sessions failed at cursor %d: %v", cursor, err)
			return
		}
		for _, key := range keys {
			scanned++
			if handleSessionKey(ctx, svcCtx, key, liveGates, suspects) {
				orphaned++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	if orphaned > 0 {
		logx.Infof("SessionReconciler: scanned=%d orphaned_to_disconnecting=%d live_gates=%d",
			scanned, orphaned, len(liveGates))
	}
}

// listLiveGateInstances 拉取 etcd 上当前注册的全部 gate 实例 uuid。
func listLiveGateInstances(ctx context.Context, etcdCli *clientv3.Client) (map[string]struct{}, error) {
	getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := etcdCli.Get(getCtx, gateRpcPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var info base.NodeInfo
		if err := protojson.Unmarshal(kv.Value, &info); err != nil {
			// allocated/ 占位 key 的 value 是裸 uuid 而非 NodeInfo JSON,
			// 解析失败直接把原始值当 uuid 收进存活集 —— 多收不误杀。
			live[string(kv.Value)] = struct{}{}
			continue
		}
		if info.NodeUuid != "" {
			live[info.NodeUuid] = struct{}{}
		}
	}
	return live, nil
}

// handleSessionKey 处理单个会话键;返回 true 表示本轮把它补投了 DISCONNECTING。
func handleSessionKey(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	key string,
	liveGates map[string]struct{},
	suspects map[uint64]string,
) bool {
	data, err := svcCtx.RedisClient.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return false // 扫描与删除竞态,正常
	}
	if err != nil {
		logx.Errorf("SessionReconciler: read %s failed: %v", key, err)
		return false
	}
	session := &pb.PlayerSession{}
	if err := proto.Unmarshal(data, session); err != nil {
		logx.Errorf("SessionReconciler: malformed session at %s: %v", key, err)
		return false
	}

	playerID := session.PlayerId
	if session.State != pb.PlayerSessionState_SESSION_STATE_ONLINE || session.GateInstanceId == "" {
		delete(suspects, playerID)
		return false
	}
	if _, alive := liveGates[session.GateInstanceId]; alive {
		delete(suspects, playerID)
		return false
	}

	// gate 缺席:第一轮只挂嫌疑,第二轮同一实例仍缺席才动手。
	if prev, ok := suspects[playerID]; !ok || prev != session.GateInstanceId {
		suspects[playerID] = session.GateInstanceId
		return false
	}
	delete(suspects, playerID)

	// 与 SetDisconnecting 相同的转换:整字节 CAS + 版本递进 + ZADD 租约。
	// 会话此刻已被改写(重连/重登)则 CAS 失败 no-op,绝不误杀活人。
	updatedSession := proto.Clone(session).(*pb.PlayerSession)
	updatedSession.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	updatedSession.SessionVersion++
	updated, err := proto.Marshal(updatedSession)
	if err != nil {
		logx.Errorf("SessionReconciler: marshal updated session player=%d failed: %v", playerID, err)
		return false
	}

	ttl := time.Duration(svcCtx.Config.Lease.DefaultTTLSeconds) * time.Second
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	swapped, err := setDisconnectingScript.Run(
		ctx,
		svcCtx.RedisClient,
		sessionLifecycleKeys(playerID),
		data,
		updated,
		0,
		time.Now().Add(ttl).Unix(),
		fmt.Sprintf("%d", playerID),
	).Int()
	if err != nil {
		logx.Errorf("SessionReconciler: CAS to disconnecting player=%d failed: %v", playerID, err)
		return false
	}
	if swapped != 1 {
		logx.Infof("SessionReconciler: session changed during CAS player=%d, skip", playerID)
		return false
	}

	logx.Infof("SessionReconciler: orphaned ONLINE session -> DISCONNECTING player=%d session=%d dead_gate_instance=%s",
		playerID, session.SessionId, session.GateInstanceId)
	return true
}

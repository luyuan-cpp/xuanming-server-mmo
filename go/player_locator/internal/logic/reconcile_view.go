package logic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	base "proto/common/base"
	"shared/nodeinfo"
)

// clusterView 是会话对账对 etcd 的全部依赖。抽成接口是为了隔离外部系统、给单测一个注入点
// (AGENTS §11.5.2):生产实现读 etcd,单测用可中途切换返回值的假实现。
type clusterView interface {
	// LiveGateInstances 返回当前在 etcd 注册的全部 gate 实例 uuid。出错时调用方整拍跳过(fail-closed)。
	LiveGateInstances(ctx context.Context) (map[string]struct{}, error)
	// SessionWriters 返回已注册的 player_locator 实例 uuid(all)与其中带「维护 gate 索引」能力键的(capable),
	// 两者取自同一个 etcd revision。出错时调用方只跳过本拍的写者检查,不当作出现了旧写者。
	SessionWriters(ctx context.Context) (all, capable map[string]struct{}, err error)
}

const etcdViewTimeout = 5 * time.Second

// gateRpcPrefix 是 C++ gate 在 etcd 里的注册前缀(与 internal/node 的
// rpcPrefix 同构;那边是私有函数,这里按同一约定重建)。
func gateRpcPrefix() string {
	return fmt.Sprintf("%s.rpc/", base.ENodeType_name[int32(base.ENodeType_GateNodeService)])
}

// locatorRpcPrefix 是 player_locator 自己的 C++ 约定注册前缀(NodeInfo 与 allocated/ 占位都在它下面)。
func locatorRpcPrefix() string {
	return fmt.Sprintf("%s.rpc/", base.ENodeType_name[int32(base.ENodeType_PlayerLocatorNodeService)])
}

// etcdClusterView 是 clusterView 的生产实现。客户端由 Node 持有并负责关闭,这里只借用。
type etcdClusterView struct {
	cli *clientv3.Client
}

func newEtcdClusterView(cli *clientv3.Client) *etcdClusterView {
	return &etcdClusterView{cli: cli}
}

func (v *etcdClusterView) LiveGateInstances(ctx context.Context) (map[string]struct{}, error) {
	getCtx, cancel := context.WithTimeout(ctx, etcdViewTimeout)
	defer cancel()
	resp, err := v.cli.Get(getCtx, gateRpcPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	values := make([][]byte, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		values = append(values, kv.Value)
	}
	return collectLiveGateInstances(values), nil
}

// SessionWriters 用一个只读 Txn 同时 Range 两个前缀,保证两份列表来自同一个 revision。
// 分两次读的话,一个新版 locator 恰在两次读之间正常退出(先删 NodeInfo 再撤租约)时,第一份里有它、
// 第二份里没有它的能力键,就会被误判成旧写者,白白作废就绪标记并触发一次全键补建。
func (v *etcdClusterView) SessionWriters(ctx context.Context) (map[string]struct{}, map[string]struct{}, error) {
	txnCtx, cancel := context.WithTimeout(ctx, etcdViewTimeout)
	defer cancel()
	resp, err := v.cli.Txn(txnCtx).Then(
		clientv3.OpGet(locatorRpcPrefix(), clientv3.WithPrefix()),
		clientv3.OpGet(GateSessionIndexCapabilityPrefix, clientv3.WithPrefix(), clientv3.WithKeysOnly()),
	).Commit()
	if err != nil {
		return nil, nil, err
	}
	if len(resp.Responses) != 2 {
		return nil, nil, fmt.Errorf("session writers txn returned %d responses, want 2", len(resp.Responses))
	}
	registered := resp.Responses[0].GetResponseRange()
	capability := resp.Responses[1].GetResponseRange()
	if registered == nil || capability == nil {
		return nil, nil, fmt.Errorf("session writers txn returned non-range responses")
	}
	values := make([][]byte, 0, len(registered.Kvs))
	for _, kv := range registered.Kvs {
		values = append(values, kv.Value)
	}
	keys := make([]string, 0, len(capability.Kvs))
	for _, kv := range capability.Kvs {
		keys = append(keys, string(kv.Key))
	}
	return collectNodeUUIDs(values), capabilityUUIDs(keys), nil
}

// collectLiveGateInstances 把 gate 前缀下的 etcd 值归并成存活实例 uuid 集合(纯函数,便于测试)。
//
// NodeInfo 必须经 nodeinfo.Unmarshal 宽松解析(忽略未知字段,D77):新版本 gate 写入的
// 新字段(如 client_endpoint)若让解析失败,就会落进下面的"裸值"分支 —— 整段 JSON 被当成
// uuid 收进集合,真实 uuid 反而缺席,两轮后该 gate 上的会话全被误打成 DISCONNECTING。
func collectLiveGateInstances(values [][]byte) map[string]struct{} {
	return collectNodeUUIDs(values)
}

// collectNodeUUIDs 是 collectLiveGateInstances 的通用形式,写者检查复用同一套解析规则:
// NodeInfo 取 node_uuid;解析失败的值(allocated/ 占位 key 的裸 uuid)原样当 uuid 收进集合。
// 对存活集是「多收不误杀」;对写者集,多收只会多判一个旧写者(多一轮补建),不影响正确性。
func collectNodeUUIDs(values [][]byte) map[string]struct{} {
	uuids := make(map[string]struct{}, len(values))
	for _, value := range values {
		var info base.NodeInfo
		if err := nodeinfo.Unmarshal(value, &info); err != nil {
			uuids[string(value)] = struct{}{}
			continue
		}
		if info.NodeUuid != "" {
			uuids[info.NodeUuid] = struct{}{}
		}
	}
	return uuids
}

// capabilityUUIDs 从能力键取键尾的 uuid;前缀不符或键尾为空的键忽略。
func capabilityUUIDs(keys []string) map[string]struct{} {
	uuids := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		uuid, ok := strings.CutPrefix(key, GateSessionIndexCapabilityPrefix)
		if !ok || uuid == "" {
			continue
		}
		uuids[uuid] = struct{}{}
	}
	return uuids
}

// legacyWriters 返回已注册却没有能力键的 locator uuid(排序后,便于日志与测试)。
func legacyWriters(all, capable map[string]struct{}) []string {
	var legacy []string
	for uuid := range all {
		if _, ok := capable[uuid]; !ok {
			legacy = append(legacy, uuid)
		}
	}
	sort.Strings(legacy)
	return legacy
}

// pacer 给一条循环限速:每次动作前 Wait。实现不必并发安全 —— 每个 pacer 只给一个 goroutine 用
// (transitionPacer 归死 gate 清扫,scanPacer 归补建 / 审计循环)。
type pacer interface {
	Wait(ctx context.Context) error
}

// noopPacer 不限速(配置为负数时;单测也用它,不依赖墙钟)。
type noopPacer struct{}

func (noopPacer) Wait(ctx context.Context) error { return ctx.Err() }

// pacerMinSleep:欠账攒到这么多才真睡一次。每次动作都按 1ms 级别去睡,定时器粒度会把实际速率压得远低于配置值。
const pacerMinSleep = 10 * time.Millisecond

// intervalPacer 按固定间隔放行:平均速率 = perSecond,短时允许最多 pacerMinSleep 的突发。
// 不预支:落后于当前时刻的额度不累积,空闲一段时间后不会一口气放出一大批。
type intervalPacer struct {
	interval time.Duration
	next     time.Time
	now      func() time.Time
}

// newIntervalPacer 返回每秒最多放行 perSecond 次的 pacer;perSecond ≤ 0 时不限速。
func newIntervalPacer(perSecond int) pacer {
	if perSecond <= 0 {
		return noopPacer{}
	}
	return &intervalPacer{interval: time.Second / time.Duration(perSecond), now: time.Now}
}

func (p *intervalPacer) Wait(ctx context.Context) error {
	now := p.now()
	if p.next.Before(now) {
		p.next = now
	}
	wait := p.next.Sub(now)
	p.next = p.next.Add(p.interval)
	if wait < pacerMinSleep {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

package svc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
	basepb "proto/common/base"
)

// 本文件盯住 login 对 gate 排空判定循环的接线(集群外入口 D87):
// 快照必须取 etcd 原始节点(不经选址 / 去重 / 排空过滤),配置必须正确换成判定参数,
// 一轮判定必须能把已排空的 gate 写上 gate:{id}:drained。判定逻辑本身在 loginqueue 有矩阵测试。

func drainedKey(nodeID uint32) string {
	return fmt.Sprintf(loginqueue.GateDrainedKeyFmt, nodeID)
}

// 快照除 nil 外一条不丢:陈旧记录、缺客户端地址、没有 endpoint 的记录都要留下,顺序与输入一致。
func TestGateDrainSnapshot_KeepsEveryRecordButNil(t *testing.T) {
	noIdentity := &basepb.NodeInfo{NodeId: 6, ZoneId: 1, LaunchTime: 100, PlayerCount: 2}
	nodes := []*basepb.NodeInfo{
		gateNode(5, 1, 100, "10.244.0.5", 50, clientAddr("127.0.0.1", 30001)), // 陈旧
		nil,
		gateNode(9, 1, 200, "10.244.0.9", 0, clientAddr("127.0.0.1", 30001)),
		gateNode(4, 1, 150, "10.244.0.4", 7, nil), // 没自报客户端地址
		noIdentity,
	}
	want := []loginqueue.GateOnline{
		{NodeID: 5, PlayerCount: 50},
		{NodeID: 9, PlayerCount: 0},
		{NodeID: 4, PlayerCount: 7},
		{NodeID: 6, PlayerCount: 2},
	}
	if got := gateDrainSnapshot(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("快照不符:\n got  %+v\n want %+v", got, want)
	}
	if got := gateDrainSnapshot(nil); len(got) != 0 {
		t.Fatalf("空输入应得空快照,got %+v", got)
	}
}

// 接线的一整轮:原始节点 → 快照 → EvaluateDrainingGates → Redis 标记。
//
// 被排空的 node 9 在 RequireClientEndpoint=true 下本来就不会进候选集(没自报客户端地址),
// 而且它被标了 draining,CandidatesForZone 同样会把它剔掉 —— 这正是快照不能复用候选链的原因:
// 复用了,正在排空的 gate 永远不被判定,drained 永远等不到。
func TestGateDrainMonitorWiring_OneRoundMarksDrainedGate(t *testing.T) {
	rdb, mr := newSvcTestRedis(t)
	ctx := context.Background()
	now := time.Now().Unix()

	nodes := []*basepb.NodeInfo{
		gateNode(4, 1, 150, "10.244.0.4", 7, clientAddr("127.0.0.1", 30000)),
		gateNode(9, 1, 200, "10.244.0.9", 0, nil), // 在线已清空,等待缩容
	}
	if err := loginqueue.MarkGateDraining(ctx, rdb, 9, now, 600); err != nil {
		t.Fatalf("mark gate 9 draining: %v", err)
	}
	// node 4 上残留一个过期的 drained 标记(上一次缩容被取消),本轮必须清掉。
	if err := mr.Set(drainedKey(4), loginqueue.DrainReasonBelowThreshold); err != nil {
		t.Fatalf("seed stale drained mark: %v", err)
	}

	// 对照:候选链里根本没有 node 9。
	for _, c := range candidatesFromNodes(ctx, rdb, nodes, 1, true) {
		if c.NodeID == 9 {
			t.Fatalf("正在排空且缺客户端地址的 node 9 不应进候选集,got %+v", c)
		}
	}

	snapshot := newGateDrainSnapshotFunc(func() ([]*basepb.NodeInfo, error) { return nodes, nil })
	gates, err := snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	policy := gateDrainPolicy(config.GateDrainConf{Interval: 5 * time.Second, Deadline: 25 * time.Minute})
	drained := loginqueue.EvaluateDrainingGates(ctx, rdb, gates, policy, now)

	if len(drained) != 1 || drained[0].NodeID != 9 || drained[0].Reason != loginqueue.DrainReasonBelowThreshold {
		t.Fatalf("本轮应只判定 node 9 排空(below_threshold),got %+v", drained)
	}
	if got, err := mr.Get(drainedKey(9)); err != nil || got != loginqueue.DrainReasonBelowThreshold {
		t.Fatalf("gate 9 的 drained 标记 = %q (err=%v),want %q", got, err, loginqueue.DrainReasonBelowThreshold)
	}
	if mr.Exists(drainedKey(4)) {
		t.Fatal("没在排空的 node 4 上残留的 drained 标记必须被清掉")
	}
}

// 取节点失败时快照原样报错,不能返回空快照 —— 空快照在判定里等于「这一轮没有 gate」,会把错误吞掉。
func TestGateDrainSnapshotFunc_PropagatesFetchError(t *testing.T) {
	boom := errors.New("etcd unavailable")
	snapshot := newGateDrainSnapshotFunc(func() ([]*basepb.NodeInfo, error) { return nil, boom })
	gates, err := snapshot(context.Background())
	if !errors.Is(err, boom) || gates != nil {
		t.Fatalf("取节点失败应原样报错,got gates=%+v err=%v", gates, err)
	}
}

func TestGateDrainPolicy_ConvertsConfig(t *testing.T) {
	cases := map[string]struct {
		cfg  config.GateDrainConf
		want loginqueue.GateDrainPolicy
	}{
		"defaults": {
			cfg:  config.GateDrainConf{Interval: 5 * time.Second, Deadline: 25 * time.Minute},
			want: loginqueue.GateDrainPolicy{DrainedBelowPlayers: 0, DeadlineSeconds: 1500},
		},
		"threshold passes through": {
			cfg:  config.GateDrainConf{DrainedBelowPlayers: 5, Deadline: time.Minute},
			want: loginqueue.GateDrainPolicy{DrainedBelowPlayers: 5, DeadlineSeconds: 60},
		},
		"zero deadline never releases on time": {
			cfg:  config.GateDrainConf{Deadline: 0},
			want: loginqueue.GateDrainPolicy{DeadlineSeconds: 0},
		},
		"negative deadline is not immediate release": {
			cfg:  config.GateDrainConf{Deadline: -time.Minute},
			want: loginqueue.GateDrainPolicy{DeadlineSeconds: 0},
		},
		// 截断会把 500ms 变成 0 = 永不放行,语义反了,所以向上取整。
		"sub-second deadline rounds up": {
			cfg:  config.GateDrainConf{Deadline: 500 * time.Millisecond},
			want: loginqueue.GateDrainPolicy{DeadlineSeconds: 1},
		},
		"fractional seconds round up": {
			cfg:  config.GateDrainConf{Deadline: 1500 * time.Millisecond},
			want: loginqueue.GateDrainPolicy{DeadlineSeconds: 2},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := gateDrainPolicy(tc.cfg); got != tc.want {
				t.Fatalf("gateDrainPolicy(%+v) = %+v, want %+v", tc.cfg, got, tc.want)
			}
		})
	}
}

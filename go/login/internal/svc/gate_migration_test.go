package svc

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/loginqueue"
	basepb "proto/common/base"
	loginpb "proto/login"
)

// 本文件钉住 gate 排空迁移在 login 服务层的判定(docs/design/gate-drain-session-migration.md §4.4)。
// 选目标与票据的规则在 loginqueue(gatedrain_migrate_test.go),这里只验接线:调用方核对、候选来源、不回落、参数换算。

// migratingGate 造一台 zone 1 的 gate:带实例 uuid,自报客户端地址 127.0.0.1:<port>。
func migratingGate(nodeID uint32, players uint32, port uint32) *basepb.NodeInfo {
	n := gateNode(nodeID, 1, 100, "10.244.0."+strconv.FormatUint(uint64(nodeID), 10), players, clientAddr("127.0.0.1", port))
	n.NodeUuid = "uuid-" + strconv.FormatUint(uint64(nodeID), 10)
	return n
}

// intnFirst 让二选一固定抽前两个候选。
func intnFirst(int) int { return 0 }

func TestPlanGateMigration_PicksAnotherGateOfThisZone(t *testing.T) {
	nodes := watched(migratingGate(1, 300, 30001), migratingGate(2, 100, 30002), migratingGate(3, 50, 30003))
	draining := map[uint32]bool{1: true}

	target, result := planGateMigration(nodes, 1, false, 1, "uuid-1", draining, intnFirst)
	if result != loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_OK || target == nil {
		t.Fatalf("result = %v target = %+v, want OK", result, target)
	}
	if target.NodeID == 1 {
		t.Fatal("must never pick the draining caller itself")
	}
	if target.IP != "127.0.0.1" {
		t.Fatalf("target address = %s:%d, want the gate's self-reported client endpoint (D76)", target.IP, target.Port)
	}
}

// 调用方必须是本 zone 目录里的这一台、且正在排空;任何一条不满足都回 NOT_DRAINING,gate 据此停止本轮迁移。
func TestPlanGateMigration_CallerMustBeThisZonesDrainingGate(t *testing.T) {
	foreign := migratingGate(9, 0, 30009)
	foreign.ZoneId = 2
	nodes := watched(migratingGate(1, 300, 30001), migratingGate(2, 100, 30002), foreign)

	cases := []struct {
		name     string
		caller   uint32
		instance string
		draining map[uint32]bool
	}{
		{"调用方不在排空", 1, "uuid-1", map[uint32]bool{}},
		{"实例 uuid 对不上(同号的新进程或旧进程迟到)", 1, "uuid-old", map[uint32]bool{1: true}},
		{"实例 uuid 为空", 1, "", map[uint32]bool{1: true}},
		{"调用方是别的 zone 的 gate(路由送错了 zone)", 9, "uuid-9", map[uint32]bool{9: true}},
		{"目录里没有这台 gate", 7, "uuid-7", map[uint32]bool{7: true}},
	}
	for _, c := range cases {
		_, result := planGateMigration(nodes, 1, false, c.caller, c.instance, c.draining, intnFirst)
		if result != loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NOT_DRAINING {
			t.Errorf("%s: result = %v, want NOT_DRAINING", c.name, result)
		}
	}
}

// 迁移不回落:登录挑 gate 时候选全在排空会退到「连得上的」那一档照常分配(shared/gatepick.Eligible),迁移时不能这样做。
func TestPlanGateMigration_NoTargetDoesNotFallBack(t *testing.T) {
	both := watched(migratingGate(1, 300, 30001), migratingGate(2, 100, 30002))
	_, result := planGateMigration(both, 1, false, 1, "uuid-1", map[uint32]bool{1: true, 2: true}, intnFirst)
	if result != loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NO_TARGET {
		t.Fatalf("every other gate draining: result = %v, want NO_TARGET", result)
	}

	full := migratingGate(2, 900, 30002)
	full.MaxConnections = 1000
	_, result = planGateMigration(watched(migratingGate(1, 300, 30001), full), 1, false, 1, "uuid-1", map[uint32]bool{1: true}, intnFirst)
	if result != loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NO_TARGET {
		t.Fatalf("the only other gate is full: result = %v, want NO_TARGET", result)
	}

	// RequireClientEndpoint 下没自报地址的 gate 不是候选,与登录挑 gate 同一份 buildGateCandidates。
	noAddr := migratingGate(2, 10, 0)
	noAddr.ClientEndpoint = nil
	_, result = planGateMigration(watched(migratingGate(1, 300, 30001), noAddr), 1, true, 1, "uuid-1", map[uint32]bool{1: true}, intnFirst)
	if result != loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_NO_TARGET {
		t.Fatalf("require client endpoint and the other gate has none: result = %v, want NO_TARGET", result)
	}
}

func TestZoneGateIDs_OnlyThisZoneDeduplicated(t *testing.T) {
	foreign := migratingGate(5, 0, 30005)
	foreign.ZoneId = 2
	nodes := watched(migratingGate(1, 0, 30001), foreign, migratingGate(2, 0, 30002), migratingGate(1, 0, 30001))
	nodes = append(nodes, watched(nil)...)

	got := zoneGateIDs(nodes, 1)
	if !reflect.DeepEqual(got, []uint32{1, 2}) {
		t.Fatalf("zoneGateIDs = %v, want [1 2]", got)
	}
}

func TestGateDrainMigratePolicy_LeaseAndRounding(t *testing.T) {
	cfg := config.GateDrainConf{
		Interval:            5 * time.Second,
		DrainedBelowPlayers: 2,
		Migrate: config.GateDrainMigrateConf{
			Enabled:       true,
			RatePerSecond: 20,
			StartAfter:    1500 * time.Millisecond,
		},
	}
	got := gateDrainMigratePolicy(cfg)
	want := loginqueue.DrainMigratePolicy{
		Enabled:             true,
		RatePerSecond:       20,
		StartAfterSeconds:   2, // 不足 1s 的部分向上取整,与 Deadline 同口径
		DrainedBelowPlayers: 2,
		LeaseMs:             15000, // 3 × Interval
	}
	if got != want {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}

	cfg.Migrate.StartAfter = -time.Second
	if p := gateDrainMigratePolicy(cfg); p.StartAfterSeconds != 0 {
		t.Fatalf("negative StartAfter = %d, want 0 (start immediately)", p.StartAfterSeconds)
	}
	cfg.Interval = 0
	if p := gateDrainMigratePolicy(cfg); p.LeaseMs != 0 {
		t.Fatalf("Interval 0 lease = %d, want 0 (no lease can be refreshed when the monitor is off)", p.LeaseMs)
	}
}

func TestGateDrainSnapshot_CarriesInstanceID(t *testing.T) {
	got := gateDrainSnapshot([]*basepb.NodeInfo{{NodeId: 7, PlayerCount: 3, NodeUuid: "uuid-7"}, nil})
	want := []loginqueue.GateOnline{{NodeID: 7, PlayerCount: 3, InstanceID: "uuid-7"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, want %+v (the migration lease is addressed to this instance)", got, want)
	}
}

// 开关关闭时第一步就回 DISABLED,不读目录、不碰 Redis:零值 ServiceContext 走到别处会 nil 解引用。
func TestSignGateMigration_DisabledRepliesDisabled(t *testing.T) {
	previous := config.AppConfig
	t.Cleanup(func() { config.AppConfig = previous })
	config.AppConfig = config.Config{}

	sc := &ServiceContext{}
	resp := sc.SignGateMigration(context.Background(), &basepb.SessionDetails{GateNodeId: 1, GateInstanceId: "uuid-1", SessionIncarnation: 1})
	if resp.GetResult() != loginpb.GateMigrationResult_GATE_MIGRATION_RESULT_DISABLED {
		t.Fatalf("result = %v, want DISABLED", resp.GetResult())
	}
	if len(resp.GetTokenPayload()) != 0 {
		t.Fatal("a disabled reply must not carry a ticket")
	}
}

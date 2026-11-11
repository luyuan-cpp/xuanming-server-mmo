package logic

import "testing"

// 节点重新注册时只在回来的就是判死的那一代进程时才抹 death_at;同号换了新进程,老实例的再入屏障要照常走完
// (docs/design/infra-reconnect-overview.md §4.1 R15)。
func TestClearNodeDeathForInstance(t *testing.T) {
	const nodeID = "7"
	cases := []struct {
		name         string
		deadInstance string // 空串 = 没有 reconcile_instance 记录
		registering  string
		wantCleared  bool
	}{
		{name: "same instance comes back", deadInstance: "uuid-old", registering: "uuid-old", wantCleared: true},
		{name: "new process takes the same node id", deadInstance: "uuid-old", registering: "uuid-new", wantCleared: false},
		{name: "dead instance unknown", deadInstance: "", registering: "uuid-new", wantCleared: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, mr := newTestSvcCtx(t, "sm-test")
			deathKey := nodeDeathAtKey(testZoneId, nodeID)
			if err := mr.Set(deathKey, "1700000000000"); err != nil {
				t.Fatal(err)
			}
			if tc.deadInstance != "" {
				if err := mr.Set(nodeReconcileInstanceKey(testZoneId, nodeID), tc.deadInstance); err != nil {
					t.Fatal(err)
				}
			}

			clearNodeDeathForInstance(svcCtx, testZoneId, nodeID, tc.registering)

			if cleared := !mr.Exists(deathKey); cleared != tc.wantCleared {
				t.Fatalf("death_at cleared = %v, want %v", cleared, tc.wantCleared)
			}
		})
	}
}

// 没有死亡标记时(平常的注册刷新)什么都不写。
func TestClearNodeDeathForInstanceWithoutDeathIsNoop(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t, "sm-test")
	clearNodeDeathForInstance(svcCtx, testZoneId, "7", "uuid-new")
	if mr.Exists(nodeDeathAtKey(testZoneId, "7")) || mr.Exists(nodeReconcileInstanceKey(testZoneId, "7")) {
		t.Fatal("没有死亡标记时不能凭空写键")
	}
}

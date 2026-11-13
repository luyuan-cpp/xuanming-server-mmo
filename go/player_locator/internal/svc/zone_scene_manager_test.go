package svc

import (
	"errors"
	"testing"

	"github.com/zeromicro/go-zero/core/discov"
	"github.com/zeromicro/go-zero/zrpc"

	smpb "proto/scene_manager"
)

// 发现键按 zone 加后缀 `<base>.z<zone>`(control-plane-zone-scoping-20261105.md §1 第 6 条);本副本配的是本 zone 的那一个。
func TestSceneManagerKeyForZone(t *testing.T) {
	cases := []struct {
		name    string
		ownKey  string
		ownZone uint32
		zone    uint32
		want    string
		wantErr bool
	}{
		{"own zone keeps the configured key", "scenemanagerservice.rpc.z3", 3, 3, "scenemanagerservice.rpc.z3", false},
		{"single-zone key is fine for its own zone", "scenemanagerservice.rpc", 1, 1, "scenemanagerservice.rpc", false},
		{"another zone swaps the suffix", "scenemanagerservice.rpc.z3", 3, 12, "scenemanagerservice.rpc.z12", false},
		{"suffix must be the own zone's", "scenemanagerservice.rpc.z13", 3, 12, "", true},
		{"single-zone key cannot address another zone", "scenemanagerservice.rpc", 1, 2, "", true},
		{"zone 0 is never addressable", "scenemanagerservice.rpc.z3", 3, 0, "", true},
		{"own zone 0 cannot derive another zone", "scenemanagerservice.rpc.z0", 0, 2, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SceneManagerKeyForZone(tc.ownKey, tc.ownZone, tc.zone)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SceneManagerKeyForZone(%q, %d, %d) = %q, want error", tc.ownKey, tc.ownZone, tc.zone, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("SceneManagerKeyForZone(%q, %d, %d) = %q, %v; want %q", tc.ownKey, tc.ownZone, tc.zone, got, err, tc.want)
			}
		})
	}
}

// 别的 zone 的客户端按需建一次、之后复用;建不成不缓存,下一次再建。
func TestZoneSceneManagersDialOncePerZone(t *testing.T) {
	var dialedKeys []string
	failNext := true
	z := newZoneSceneManagers(zrpc.RpcClientConf{Etcd: discov.EtcdConf{Key: "scenemanagerservice.rpc.z3"}}, 3)
	z.dial = func(conf zrpc.RpcClientConf) (smpb.SceneManagerClient, error) {
		dialedKeys = append(dialedKeys, conf.Etcd.Key)
		if failNext {
			failNext = false
			return nil, errors.New("no endpoint yet")
		}
		return smpb.NewSceneManagerClient(nil), nil
	}
	if _, err := z.forZone(7); err == nil {
		t.Fatal("first dial fails: want error")
	}
	first, err := z.forZone(7)
	if err != nil || first == nil {
		t.Fatalf("second dial: %v", err)
	}
	again, err := z.forZone(7)
	if err != nil || again != first {
		t.Fatalf("cached client not reused: %v", err)
	}
	if len(dialedKeys) != 2 || dialedKeys[0] != "scenemanagerservice.rpc.z7" || dialedKeys[1] != "scenemanagerservice.rpc.z7" {
		t.Fatalf("dialed keys = %v", dialedKeys)
	}
}

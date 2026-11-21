package loginpregatelogic

import (
	"testing"

	base "proto/common/base"
)

type stubLiveSession struct {
	gateID     string
	gateZoneID uint32
}

// findReconnectingPlayer 的判定:账号的每个角色都看;会话带 gate_zone_id 时以它为准,
// 旧版会话(gate_zone_id == 0)回退到建角区 + 本区现有 gate 集合。
func TestFindReconnectingPlayer(t *testing.T) {
	const ownZone = 7
	role := func(playerID uint64, zoneID uint32) *base.AccountSimplePlayer {
		return &base.AccountSimplePlayer{PlayerId: playerID, ZoneId: zoneID}
	}
	cases := []struct {
		name  string
		roles []*base.AccountSimplePlayer
		// live:player_id → 会话所在 gate 节点号与 gate_zone_id;不在表里 = 没有会话。
		live map[uint64]stubLiveSession
		want uint64 // 0 = 不免排队
	}{
		{
			name:  "no role is online",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, ownZone)},
			live:  map[uint64]stubLiveSession{},
			want:  0,
		},
		{
			// 修复前红:只看第一个角色,用第二个角色在线的账号重连要排队。
			name:  "the online role is not the first one",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, ownZone)},
			live:  map[uint64]stubLiveSession{2: {gateID: "101", gateZoneID: ownZone}},
			want:  2,
		},
		{
			// 合服并进来的角色(建角区 5),连在本区(7)且原 gate 已掉线(不在本区列表中),靠 gate_zone_id 仍免排队。
			name:  "merged role with gate_zone_id equal to own zone skips queue even after gate crash",
			roles: []*base.AccountSimplePlayer{role(3, 5)},
			live:  map[uint64]stubLiveSession{3: {gateID: "999", gateZoneID: ownZone}},
			want:  3,
		},
		{
			// 本区角色跨区到 8 区在线(gate_zone_id=8),不能拿它免本区(7)的排队。
			name:  "own-zone role visiting another zone does not skip own zone queue",
			roles: []*base.AccountSimplePlayer{role(1, ownZone)},
			live:  map[uint64]stubLiveSession{1: {gateID: "555", gateZoneID: 8}},
			want:  0,
		},
		{
			// 第一个角色在别的区在线,第二个角色在本区在线:继续扫描并命中第二个。
			name:  "role in other zone skipped and second role in own zone matched",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, 5)},
			live: map[uint64]stubLiveSession{
				1: {gateID: "555", gateZoneID: 8},
				2: {gateID: "999", gateZoneID: ownZone},
			},
			want: 2,
		},
		{
			// 旧版会话(gate_zone_id=0):一台 gate 掉线后,本区建角的在线角色仍免排队。
			name:  "legacy session own-zone role whose gate is gone still skips the queue",
			roles: []*base.AccountSimplePlayer{role(1, ownZone)},
			live:  map[uint64]stubLiveSession{1: {gateID: "999"}},
			want:  1,
		},
		{
			// 旧版会话(gate_zone_id=0):把角色挂在别的区在线,不能拿它免本区的排队。
			name:  "legacy session role of another zone online on foreign gate does not skip queue",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, 8)},
			live:  map[uint64]stubLiveSession{2: {gateID: "555"}},
			want:  0,
		},
		{
			// 旧版会话(gate_zone_id=0):建角在别的区但正连在本区 gate 上,免排队。
			name:  "legacy session role created in another zone connected to own gate skips queue",
			roles: []*base.AccountSimplePlayer{role(3, 5)},
			live:  map[uint64]stubLiveSession{3: {gateID: "102"}},
			want:  3,
		},
		{
			name:  "role without a player id is skipped",
			roles: []*base.AccountSimplePlayer{role(0, ownZone), role(4, ownZone)},
			live: map[uint64]stubLiveSession{
				0: {gateID: "101", gateZoneID: ownZone},
				4: {gateID: "101", gateZoneID: ownZone},
			},
			want: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			liveSession := func(playerID uint64) (string, uint32) {
				s := tc.live[playerID]
				return s.gateID, s.gateZoneID
			}
			zoneGates := func() map[string]struct{} {
				return map[string]struct{}{"101": {}, "102": {}}
			}
			got, ok := findReconnectingPlayer(tc.roles, ownZone, liveSession, zoneGates)
			if got != tc.want || ok != (tc.want != 0) {
				t.Fatalf("findReconnectingPlayer = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.want != 0)
			}
		})
	}
}

// 会话带 gate_zone_id 时永不读本区 gate 列表;旧版会话只在遇到「别的区的在线角色」时才读,且一次请求至多读一次。
func TestFindReconnectingPlayer_ReadsTheGateListOnlyWhenNeeded(t *testing.T) {
	const ownZone = 7
	reads := 0
	zoneGates := func() map[string]struct{} {
		reads++
		return map[string]struct{}{"101": {}}
	}

	// 带 gate_zone_id 的会话(无论同区还是跨区)都不碰 gate 列表。
	stamped := func(id uint64) (string, uint32) {
		if id == 10 {
			return "999", ownZone
		}
		return "555", 8
	}
	merged := []*base.AccountSimplePlayer{{PlayerId: 10, ZoneId: 5}}
	if _, ok := findReconnectingPlayer(merged, ownZone, stamped, zoneGates); !ok || reads != 0 {
		t.Fatalf("stamped merged role: ok=%v gate list reads=%d, want true and 0", ok, reads)
	}
	visiting := []*base.AccountSimplePlayer{{PlayerId: 11, ZoneId: ownZone}}
	if _, ok := findReconnectingPlayer(visiting, ownZone, stamped, zoneGates); ok || reads != 0 {
		t.Fatalf("stamped visiting role: ok=%v gate list reads=%d, want false and 0", ok, reads)
	}

	// 旧版会话(gate_zone_id == 0):本区建角不读,异区建角至多读一次。
	legacy := func(uint64) (string, uint32) { return "555", 0 }
	ownOnly := []*base.AccountSimplePlayer{{PlayerId: 1, ZoneId: ownZone}}
	if _, ok := findReconnectingPlayer(ownOnly, ownZone, legacy, zoneGates); !ok || reads != 0 {
		t.Fatalf("legacy own-zone role: ok=%v gate list reads=%d, want true and 0", ok, reads)
	}

	foreign := []*base.AccountSimplePlayer{{PlayerId: 2, ZoneId: 8}, {PlayerId: 3, ZoneId: 9}}
	if _, ok := findReconnectingPlayer(foreign, ownZone, legacy, zoneGates); ok || reads != 1 {
		t.Fatalf("legacy foreign roles: ok=%v gate list reads=%d, want false and 1", ok, reads)
	}
}

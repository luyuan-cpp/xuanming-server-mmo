package loginpregatelogic

import (
	"testing"

	base "proto/common/base"
)

// findReconnectingPlayer 的判定(2026-11-20):账号的每个角色都看;建角区是本区的在线角色直接算,
// 别的区的角色只有正连在本区现有的 gate 上才算。
func TestFindReconnectingPlayer(t *testing.T) {
	const ownZone = 7
	role := func(playerID uint64, zoneID uint32) *base.AccountSimplePlayer {
		return &base.AccountSimplePlayer{PlayerId: playerID, ZoneId: zoneID}
	}
	cases := []struct {
		name  string
		roles []*base.AccountSimplePlayer
		// live:player_id → 会话所在 gate 的节点号;不在表里 = 没有会话。
		live map[uint64]string
		want uint64 // 0 = 不免排队
	}{
		{
			name:  "no role is online",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, ownZone)},
			live:  map[uint64]string{},
			want:  0,
		},
		{
			// 修复前红:只看第一个角色,用第二个角色在线的账号重连要排队。
			name:  "the online role is not the first one",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, ownZone)},
			live:  map[uint64]string{2: "101"},
			want:  2,
		},
		{
			// 一台 gate 掉线后,上面的人重连时会话记的还是那台已经不在列表里的 gate。
			name:  "own-zone role whose gate is gone still skips the queue",
			roles: []*base.AccountSimplePlayer{role(1, ownZone)},
			live:  map[uint64]string{1: "999"},
			want:  1,
		},
		{
			// 把角色挂在别的区在线,不能拿它免本区的排队。
			name:  "role of another zone online on that zone's gate does not skip this zone's queue",
			roles: []*base.AccountSimplePlayer{role(1, ownZone), role(2, 8)},
			live:  map[uint64]string{2: "555"},
			want:  0,
		},
		{
			name:  "role created in another zone but connected to one of this zone's gates skips the queue",
			roles: []*base.AccountSimplePlayer{role(3, 5)},
			live:  map[uint64]string{3: "102"},
			want:  3,
		},
		{
			name:  "role without a player id is skipped",
			roles: []*base.AccountSimplePlayer{role(0, ownZone), role(4, ownZone)},
			live:  map[uint64]string{0: "101", 4: "101"},
			want:  4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			liveGate := func(playerID uint64) string { return tc.live[playerID] }
			zoneGates := func() map[string]struct{} {
				return map[string]struct{}{"101": {}, "102": {}}
			}
			got, ok := findReconnectingPlayer(tc.roles, ownZone, liveGate, zoneGates)
			if got != tc.want || ok != (tc.want != 0) {
				t.Fatalf("findReconnectingPlayer = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.want != 0)
			}
		})
	}
}

// 本区的 gate 列表只在遇到「别的区的在线角色」时才读,而且一次请求至多读一次。
func TestFindReconnectingPlayer_ReadsTheGateListOnlyWhenNeeded(t *testing.T) {
	const ownZone = 7
	reads := 0
	zoneGates := func() map[string]struct{} {
		reads++
		return map[string]struct{}{"101": {}}
	}
	allLive := func(uint64) string { return "555" }

	ownOnly := []*base.AccountSimplePlayer{{PlayerId: 1, ZoneId: ownZone}}
	if _, ok := findReconnectingPlayer(ownOnly, ownZone, allLive, zoneGates); !ok || reads != 0 {
		t.Fatalf("own-zone role: ok=%v gate list reads=%d, want true and 0", ok, reads)
	}

	foreign := []*base.AccountSimplePlayer{{PlayerId: 2, ZoneId: 8}, {PlayerId: 3, ZoneId: 9}}
	if _, ok := findReconnectingPlayer(foreign, ownZone, allLive, zoneGates); ok || reads != 1 {
		t.Fatalf("two foreign roles on foreign gates: ok=%v gate list reads=%d, want false and 1", ok, reads)
	}
}

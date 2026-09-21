package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"

	"login/internal/logic/pkg/sessionmanager"
)

// resolveEnterSceneRoute 的四级优先级(票据 > 短线重连 / 顶号 → 0 > RedirectOnEnter > 本 zone)。
//
// 断言对象就是 applyLoadedPlayerSession 原样填进 EnterSceneRequest 的 ZoneId / SceneId。
// 没有从 EnterGame 入口端到端打到 SceneManager.EnterScene:那条路先要经 SendBindSessionToGate,
// 它走 *kafka.KeyOrderedKafkaProducer(具体类型、字段私有,单测里起不出来),与
// enterFailureTipPusher 注释里说的是同一个限制。

const (
	routeOwnZone   = uint32(3)
	routeOtherZone = uint32(7)
	routeAccount   = "acct-route"
)

// fakeHomeZoneLookup 是 homezone.Lookup 的假实现,记录调用次数:优先级更高的分支命中时不该查映射。
type fakeHomeZoneLookup struct {
	home  uint32
	err   error
	calls int
}

func (f *fakeHomeZoneLookup) HomeZone(context.Context, uint64) (uint32, error) {
	f.calls++
	return f.home, f.err
}

func routeState(ticketZone uint32) enterGameSessionState {
	return enterGameSessionState{playerID: identityPlayerID, account: routeAccount, ticketTargetZoneID: ticketZone}
}

// disconnectingSession 是 gate 断线后 login 置成 DISCONNECTING 的会话(30s 租约内)。
// SceneID 故意填非 0:生产上恒为 0,这里用非 0 值才能区分「保留会话值」与「清零」两种行为。
func disconnectingSession() *sessionmanager.PlayerSession {
	return &sessionmanager.PlayerSession{
		PlayerID: identityPlayerID,
		Account:  routeAccount,
		State:    sessionmanager.StateDisconnecting,
		SceneID:  99,
	}
}

func onlineSession() *sessionmanager.PlayerSession {
	return &sessionmanager.PlayerSession{
		PlayerID: identityPlayerID,
		Account:  routeAccount,
		State:    sessionmanager.StateOnline,
		SceneID:  99,
	}
}

func TestResolveEnterSceneRoute_Priority(t *testing.T) {
	cases := map[string]struct {
		ticketZone      uint32
		existing        *sessionmanager.PlayerSession
		redirectEnabled bool
		lookup          *fakeHomeZoneLookup
		wantZone        uint32
		wantScene       uint64
		wantLookups     int
	}{
		// 第 1 条:跨 zone 第二条腿。源 gate 断线让会话成了 DISCONNECTING,decision 是 ShortReconnect,
		// 票据仍须优先,否则会以 ZoneId=0 绕过 CZ-8 的访客识别。
		"ticket beats short reconnect": {
			ticketZone:      routeOwnZone,
			existing:        disconnectingSession(),
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:        routeOwnZone,
			wantScene:       0,
			wantLookups:     0,
		},
		"ticket beats replace login": {
			ticketZone:      routeOwnZone,
			existing:        onlineSession(),
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:        routeOwnZone,
			wantScene:       0,
			wantLookups:     0,
		},
		// 第 1 条压过第 3 条:访客首登落地目标 zone,不按 home_zone 弹回。
		"ticket beats redirect on enter": {
			ticketZone:      routeOwnZone,
			existing:        nil,
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:        routeOwnZone,
			wantScene:       0,
			wantLookups:     0,
		},
		// 第 2 条:断线租约内重登 → 不指定去向,由 scene_manager 按 location 决定(GO-5)。
		// RedirectOnEnter 开着也不查映射;SceneId 清零,会话里的非 0 值不得绕过 scene_manager 的 node_id 规则。
		"short reconnect sends zone 0": {
			existing:        disconnectingSession(),
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:        0,
			wantScene:       0,
			wantLookups:     0,
		},
		// 第 2 条:顶号 → 新设备接管角色当前所在位置。
		"replace login sends zone 0": {
			existing:        onlineSession(),
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:        0,
			wantScene:       0,
			wantLookups:     0,
		},
		// 票据指向的不是本 zone(不是来这里的第二条腿)→ 不命中第 1 条,按第 2 条走。
		"ticket for another zone falls to reconnect rule": {
			ticketZone:  routeOtherZone,
			existing:    disconnectingSession(),
			lookup:      &fakeHomeZoneLookup{},
			wantZone:    0,
			wantScene:   0,
			wantLookups: 0,
		},
		// 第 3 条:首登 + RedirectOnEnter 命中 → 归属 zone,SceneId 清零。
		"first login redirect on enter": {
			existing:        nil,
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:        routeOtherZone,
			wantScene:       0,
			wantLookups:     1,
		},
		// 第 4 条:开关关闭 → 本 zone,连映射都不查。
		"first login redirect disabled": {
			existing:    nil,
			lookup:      &fakeHomeZoneLookup{home: routeOtherZone},
			wantZone:    routeOwnZone,
			wantScene:   0,
			wantLookups: 0,
		},
		// 第 4 条:映射就是本 zone / 未映射 / 查询失败 → 维持本 zone,不阻断。
		"first login home is own zone": {
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: routeOwnZone},
			wantZone:        routeOwnZone,
			wantLookups:     1,
		},
		"first login unmapped": {
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{home: 0},
			wantZone:        routeOwnZone,
			wantLookups:     1,
		},
		"first login lookup error": {
			redirectEnabled: true,
			lookup:          &fakeHomeZoneLookup{err: errors.New("data_service down")},
			wantZone:        routeOwnZone,
			wantLookups:     1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// decision 用生产的判定函数现算,而不是手填:这样「第二条腿 = ShortReconnect」这条
			// 前提本身也被钉住(DISCONNECTING + 同账号 → ShortReconnect;在线会话 → ReplaceLogin)。
			decision := sessionmanager.DecideEnterGame(tc.existing, routeAccount)
			got := resolveEnterSceneRoute(context.Background(), routeState(tc.ticketZone), decision,
				tc.existing, routeOwnZone, tc.redirectEnabled, tc.lookup)
			if got.zoneID != tc.wantZone {
				t.Fatalf("ZoneId = %d, want %d (decision=%s)", got.zoneID, tc.wantZone, decisionLabel(decision))
			}
			if got.sceneID != tc.wantScene {
				t.Fatalf("SceneId = %d, want %d (decision=%s)", got.sceneID, tc.wantScene, decisionLabel(decision))
			}
			if tc.lookup.calls != tc.wantLookups {
				t.Fatalf("home zone lookups = %d, want %d", tc.lookup.calls, tc.wantLookups)
			}
		})
	}
}

// 前提钉子:租约内同账号重登是 ShortReconnect,换账号(顶号)是 ReplaceLogin。上表依赖这条映射,
// 它一变,第 2 条的「窗口」就不再是断线租约了。
func TestResolveEnterSceneRoute_DecisionPremise(t *testing.T) {
	if got := sessionmanager.DecideEnterGame(disconnectingSession(), routeAccount); got != sessionmanager.ShortReconnect {
		t.Fatalf("disconnecting + same account = %s, want reconnect", decisionLabel(got))
	}
	if got := sessionmanager.DecideEnterGame(disconnectingSession(), "other-account"); got != sessionmanager.ReplaceLogin {
		t.Fatalf("disconnecting + other account = %s, want replace", decisionLabel(got))
	}
	if got := sessionmanager.DecideEnterGame(onlineSession(), routeAccount); got != sessionmanager.ReplaceLogin {
		t.Fatalf("online session = %s, want replace", decisionLabel(got))
	}
}

// nil 接口的 Lookup(映射客户端没接)按「查不到」处理,不 panic,维持本 zone。
func TestResolveEnterSceneRoute_NilLookupKeepsOwnZone(t *testing.T) {
	got := resolveEnterSceneRoute(context.Background(), routeState(0), sessionmanager.FirstLogin,
		nil, routeOwnZone, true, nil)
	if got.zoneID != routeOwnZone || got.sceneID != 0 {
		t.Fatalf("route = %+v, want zone=%d scene=0", got, routeOwnZone)
	}
}

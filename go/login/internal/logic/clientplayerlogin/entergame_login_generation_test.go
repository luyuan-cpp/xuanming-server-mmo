package clientplayerloginlogic

import (
	"context"
	"testing"

	"login/internal/logic/pkg/sessionmanager"
	pbbase "proto/common/base"
	loginpb "proto/login"
	plpb "proto/player_locator"
)

// 登录代次的接线(docs/design/scene-enter-session-fence.md §3.3 / §3.4 / §10「接线」)。
//
// 铸造算法本身由 sessionmanager 的 TestMintLoginGeneration_* 覆盖;这里只盯"铸出的值有没有真的
// 交给 player_locator",以及"铸造失败时链有没有停在踢线 / 写会话之前"。复用 newEnterWiringHarness:
// SetSession / Reconnect 回错误,链停在 persist。
// Bind 与 EnterScene 的携带分别由 svc 的 TestBindSessionCommandPayloadCarriesEvent 与下面的
// TestBuildEnterSceneRequest_CarriesLoginGeneration 覆盖:那两步走不到(KafkaClient 是具体类型,
// 单测起不出来,见 entergame_route_test.go 头注释)。

// wiringRecordGeneration 是会话记录里的旧代次。取 2^50(约 1.1e15):远大于任何毫秒时间戳(约 1.8e12),
// 又在 2^53 以内。铸出值恰为它 + 1 即证明"锁内读到的记录代次"被当作下限传进了铸造。
const wiringRecordGeneration = uint64(1) << 50

func wiringPlayers() []*pbbase.AccountSimplePlayer {
	return []*pbbase.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}}
}

// wiringRecord 是 GetSession 回的会话记录。GateID 留空:顶号分支的 kickReplacedSession 因此不发命令
// (本 harness 没有 Kafka 生产者)。SessionID 与本次登录的不同,才是真正的顶号 / 换连接重连。
func wiringRecord(state int32) *plpb.PlayerSession {
	return &plpb.PlayerSession{
		PlayerId:        identityPlayerID,
		SessionId:       1,
		Account:         enterWiringAccount,
		SessionVersion:  4,
		State:           plpb.PlayerSessionState(state),
		LoginGeneration: wiringRecordGeneration,
	}
}

func (h *enterWiringHarness) runEnterGame(t *testing.T) {
	t.Helper()
	resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID})
	if err != nil {
		t.Fatalf("EnterGame: %v", err)
	}
	if resp.GetErrorMessage() != nil {
		t.Fatalf("EnterGame rejected with tip %d, want the async chain started", resp.GetErrorMessage().GetId())
	}
	h.waitChain(t)
}

// mintedGeneration 读铸造键当前值:链里只铸一次,它就是本链铸出的值。
func (h *enterWiringHarness) mintedGeneration(t *testing.T) uint64 {
	t.Helper()
	v, err := h.rdb.Get(context.Background(), sessionmanager.LoginGenerationKey(identityPlayerID)).Uint64()
	if err != nil {
		t.Fatalf("read minted login generation key: %v", err)
	}
	return v
}

// 首登 / 顶号:SetSession 写下的会话带本链铸出的代次,且大于记录里的旧值(player_locator 提交点的
// 严格递增校验因此能过)。
func TestEnterGame_LoginGenerationReachesSetSession(t *testing.T) {
	t.Run("first login", func(t *testing.T) {
		h := newEnterWiringHarness(t, wiringPlayers(), nil)
		h.runEnterGame(t)

		if got := h.locator.setSessionCalls.Load(); got != 1 {
			t.Fatalf("SetSession calls = %d, want 1", got)
		}
		gen := h.locator.lastSetSession.Load().GetSession().GetLoginGeneration()
		if gen == 0 {
			t.Fatal("SetSession carried login_generation 0; scene would treat this login as unversioned")
		}
		if minted := h.mintedGeneration(t); gen != minted {
			t.Fatalf("SetSession login_generation = %d, want the minted %d", gen, minted)
		}
	})

	t.Run("replace login floors at the record", func(t *testing.T) {
		h := newEnterWiringHarness(t, wiringPlayers(), nil)
		h.locator.existing = wiringRecord(sessionmanager.StateOnline)
		h.runEnterGame(t)

		if got := h.locator.setSessionCalls.Load(); got != 1 {
			t.Fatalf("SetSession calls = %d, want 1 (online record must take the ReplaceLogin path)", got)
		}
		gen := h.locator.lastSetSession.Load().GetSession().GetLoginGeneration()
		if gen <= wiringRecordGeneration {
			t.Fatalf("SetSession login_generation = %d, want > record %d", gen, wiringRecordGeneration)
		}
		if gen != wiringRecordGeneration+1 {
			t.Fatalf("SetSession login_generation = %d, want record+1 = %d (record generation must be the mint floor)",
				gen, wiringRecordGeneration+1)
		}
	})
}

// 短线重连:代次走 ReconnectRequest.login_generation,不走 SetSession。
func TestEnterGame_LoginGenerationReachesReconnect(t *testing.T) {
	h := newEnterWiringHarness(t, wiringPlayers(), nil)
	h.locator.existing = wiringRecord(sessionmanager.StateDisconnecting)
	h.runEnterGame(t)

	if got := h.locator.reconnectCalls.Load(); got != 1 {
		t.Fatalf("Reconnect calls = %d, want 1 (disconnecting record of the same account must take ShortReconnect)", got)
	}
	if got := h.locator.setSessionCalls.Load(); got != 0 {
		t.Fatalf("SetSession calls = %d, want 0 on the reconnect path", got)
	}
	gen := h.locator.lastReconnect.Load().GetLoginGeneration()
	if gen != wiringRecordGeneration+1 {
		t.Fatalf("ReconnectRequest.login_generation = %d, want record+1 = %d", gen, wiringRecordGeneration+1)
	}
	if minted := h.mintedGeneration(t); gen != minted {
		t.Fatalf("ReconnectRequest.login_generation = %d, want the minted %d", gen, minted)
	}
}

// 铸造失败(这里让铸造键是个 hash,GET 回 WRONGTYPE):链在写会话之前失败,SetSession / Reconnect
// 一次都不调 —— 不得带 0 继续(§3.2「失败处理」)。
func TestEnterGame_MintFailureStopsBeforeSessionWrite(t *testing.T) {
	cases := map[string]*plpb.PlayerSession{
		"first login":     nil,
		"replace login":   wiringRecord(sessionmanager.StateOnline),
		"short reconnect": wiringRecord(sessionmanager.StateDisconnecting),
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			h := newEnterWiringHarness(t, wiringPlayers(), nil)
			h.locator.existing = record
			key := sessionmanager.LoginGenerationKey(identityPlayerID)
			if err := h.rdb.HSet(context.Background(), key, "poison", "1").Err(); err != nil {
				t.Fatalf("poison mint key: %v", err)
			}

			h.runEnterGame(t)

			if got := h.locator.setSessionCalls.Load(); got != 0 {
				t.Fatalf("SetSession calls = %d, want 0 after a failed mint", got)
			}
			if got := h.locator.reconnectCalls.Load(); got != 0 {
				t.Fatalf("Reconnect calls = %d, want 0 after a failed mint", got)
			}
		})
	}
}

// buildEnterSceneRequest:代次原样进 EnterSceneRequest.login_generation,其余字段与改动前的组装逐项相同。
func TestBuildEnterSceneRequest_CarriesLoginGeneration(t *testing.T) {
	state := enterGameSessionState{
		playerID:       identityPlayerID,
		sessionID:      enterWiringSessionID,
		gateID:         "300",
		gateInstanceID: "b0a1-uuid",
		requestID:      "req-1",
		ownerRevision:  17,
	}
	route := enterSceneRoute{zoneID: routeOtherZone, sceneID: 88}
	const gen = uint64(1_760_000_000_123)

	req := buildEnterSceneRequest(state, route, routeOwnZone, gen)
	if req.GetOwnerRevision() != state.ownerRevision {
		t.Fatalf("owner_revision = %d, want original %d", req.GetOwnerRevision(), state.ownerRevision)
	}

	if req.GetLoginGeneration() != gen {
		t.Fatalf("login_generation = %d, want %d", req.GetLoginGeneration(), gen)
	}
	if req.GetPlayerId() != identityPlayerID || req.GetSessionId() != enterWiringSessionID || req.GetRequestId() != "req-1" {
		t.Errorf("identity fields player=%d session=%d request=%q, want %d/%d/req-1",
			req.GetPlayerId(), req.GetSessionId(), req.GetRequestId(), identityPlayerID, enterWiringSessionID)
	}
	if req.GetGateId() != "300" || req.GetGateInstanceId() != "b0a1-uuid" {
		t.Errorf("gate fields id=%q instance=%q, want 300/b0a1-uuid", req.GetGateId(), req.GetGateInstanceId())
	}
	if req.GetGateZoneId() != routeOwnZone || req.GetZoneId() != routeOtherZone || req.GetSceneId() != 88 {
		t.Errorf("route fields gate_zone=%d zone=%d scene=%d, want %d/%d/88",
			req.GetGateZoneId(), req.GetZoneId(), req.GetSceneId(), routeOwnZone, routeOtherZone)
	}
	// login 不按请求对应答;非 0 会被 scene_manager 当成 scene 侧的交接请求去对应答。
	if req.GetCorrelationId() != 0 {
		t.Errorf("correlation_id = %d, want 0 for login", req.GetCorrelationId())
	}
}

package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"proto/battle"
	base "proto/common/base"
	gatepb "proto/gate"
	"proto/match"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
	"robot/metrics"
	"robot/pkg"
	tiptable "shared/generated/pb/table"
)

type fakeLobbySpectateGate struct {
	ln          net.Listener
	validTicket []byte
	endReason   battle.ESpectateEndReason
	pushOnSub   bool

	mu          sync.Mutex
	subRequests []*gatepb.SubscribeSpectateRequest
	unsubIds    []uint64
}

func startFakeLobbySpectateGate(t *testing.T, validTicket []byte, endReason battle.ESpectateEndReason, pushOnSub bool) *fakeLobbySpectateGate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := &fakeLobbySpectateGate{
		ln:          ln,
		validTicket: validTicket,
		endReason:   endReason,
		pushOnSub:   pushOnSub,
	}
	t.Cleanup(func() { _ = ln.Close() })
	go g.acceptLoop(t)
	return g
}

func (g *fakeLobbySpectateGate) acceptLoop(t *testing.T) {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.serveConn(t, conn)
	}
}

func (g *fakeLobbySpectateGate) serveConn(t *testing.T, conn net.Conn) {
	defer conn.Close()
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		totalLen := binary.BigEndian.Uint32(lenBuf[:])
		payload := make([]byte, totalLen)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		if len(payload) < 8 {
			return
		}
		nameLen := binary.BigEndian.Uint32(payload[:4])
		body := payload[4+nameLen : len(payload)-4]
		req := &base.ClientRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			return
		}

		switch req.GetMessageId() {
		case game.MatchServiceWatchBattleMessageId:
			wReq := &match.WatchBattleRequest{}
			_ = proto.Unmarshal(req.GetBody(), wReq)
			wResp := &match.WatchBattleResponse{
				BattleId:       wReq.GetBattleId(),
				Delivery:       match.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY,
				SpectateTicket: g.validTicket,
			}
			_, _ = conn.Write(encodeBattleNodeFrame(&base.MessageContent{
				Id:                req.GetId(),
				MessageId:         req.GetMessageId(),
				SerializedMessage: mustMarshal(t, wResp),
			}))

		case game.GateClientSpectateSubscribeSpectateMessageId:
			subReq := &gatepb.SubscribeSpectateRequest{}
			_ = proto.Unmarshal(req.GetBody(), subReq)
			g.mu.Lock()
			g.subRequests = append(g.subRequests, subReq)
			g.mu.Unlock()

			subResp := &gatepb.SubscribeSpectateResponse{}
			if string(subReq.GetSpectateTicket()) != string(g.validTicket) {
				subResp.ErrorMessage = &base.TipInfoMessage{Id: uint32(tiptable.MatchError_kMatchSpectateTicketInvalid)}
			}
			_, _ = conn.Write(encodeBattleNodeFrame(&base.MessageContent{
				Id:                req.GetId(),
				MessageId:         req.GetMessageId(),
				SerializedMessage: mustMarshal(t, subResp),
			}))

			if subResp.GetErrorMessage() == nil && g.pushOnSub {
				// 依次推 161 → 158 → 166
				frames := []*base.MessageContent{
					{
						MessageId: game.BattleClientPlayerNotifySpectateStateMessageId,
						SerializedMessage: mustMarshal(t, &battle.SpectateStateS2C{
							State: &battle.BattleStateS2C{
								BattleId:   subReq.GetBattleId(),
								RoundIndex: 1,
								Outcome:    battle.EBattleOutcome_BATTLE_OUTCOME_ONGOING,
							},
							ObserverCount: 0,
						}),
					},
					{
						MessageId: game.BattleClientPlayerNotifySpectateTurnResultMessageId,
						SerializedMessage: mustMarshal(t, &battle.TurnResultS2C{
							BattleId:   subReq.GetBattleId(),
							RoundIndex: 1,
						}),
					},
					{
						MessageId: game.BattleClientPlayerNotifySpectateEndMessageId,
						SerializedMessage: mustMarshal(t, &battle.SpectateEndS2C{
							BattleId: subReq.GetBattleId(),
							Outcome:  battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
							Reason:   g.endReason,
						}),
					},
				}
				for _, f := range frames {
					_, _ = conn.Write(encodeBattleNodeFrame(f))
				}
			}

		case game.GateClientSpectateUnsubscribeSpectateMessageId:
			unsubReq := &gatepb.UnsubscribeSpectateRequest{}
			_ = proto.Unmarshal(req.GetBody(), unsubReq)
			g.mu.Lock()
			g.unsubIds = append(g.unsubIds, unsubReq.GetBattleId())
			g.mu.Unlock()
			_, _ = conn.Write(encodeBattleNodeFrame(&base.MessageContent{
				Id:                req.GetId(),
				MessageId:         req.GetMessageId(),
				SerializedMessage: mustMarshal(t, &gatepb.UnsubscribeSpectateResponse{}),
			}))
		}
	}
}

func dialFakeLobbyGateBot(t *testing.T, gateAddr string, playerId uint64) *battleSmokeBot {
	t.Helper()
	host, portStr, err := net.SplitHostPort(gateAddr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	var port int
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	gc, err := pkg.NewGameClient(host, port)
	if err != nil {
		t.Fatalf("connect fake lobby gate: %v", err)
	}
	gc.PlayerId = playerId
	player := gameobject.NewPlayer(playerId)
	gameobject.PlayerList.Set(playerId, player)
	t.Cleanup(func() {
		gameobject.PlayerList.Delete(playerId)
		gc.Close()
	})
	go gc.RecvLoop(handler.MessageBodyHandler)
	return &battleSmokeBot{account: "robot_lobby_test", gate: gateAddr, gc: gc, player: player}
}

func TestBattleLobbySpectateWireFlowAndOrder(t *testing.T) {
	ticket := []byte("valid-hmac-ticket")
	gate := startFakeLobbySpectateGate(t, ticket, battle.ESpectateEndReason_SPECTATE_END_BATTLE_FINISHED, true)
	bot := dialFakeLobbyGateBot(t, gate.ln.Addr().String(), 88001)
	stats := metrics.NewStats()

	resp, err := battleSmokeWatchBattleResp(bot, stats, 9001, "req-lobby-1", 3*time.Second)
	if err != nil {
		t.Fatalf("WatchBattle failed: %v", err)
	}
	if resp.GetDelivery() != match.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY {
		t.Fatalf("delivery=%s want LOBBY", resp.GetDelivery())
	}
	if string(resp.GetSpectateTicket()) != string(ticket) {
		t.Fatalf("ticket mismatch")
	}

	if err := battleSmokeSubscribeSpectate(bot, stats, 9001, resp.GetSpectateTicket(), 3*time.Second); err != nil {
		t.Fatalf("SubscribeSpectate failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bid, err := bot.player.WaitSpectateState(ctx)
	if err != nil || bid != 9001 {
		t.Fatalf("WaitSpectateState bid=%d err=%v", bid, err)
	}
	reason, err := bot.player.WaitSpectateEnd(ctx)
	if err != nil || battle.ESpectateEndReason(reason) != battle.ESpectateEndReason_SPECTATE_END_BATTLE_FINISHED {
		t.Fatalf("WaitSpectateEnd reason=%d err=%v", reason, err)
	}
	if bot.player.GetSpectateTurnCount() != 1 {
		t.Fatalf("turn count=%d want 1", bot.player.GetSpectateTurnCount())
	}
	sSeq, tSeq, eSeq := bot.player.GetSpectateFrameOrder()
	if !(sSeq > 0 && sSeq < tSeq && tSeq < eSeq) {
		t.Fatalf("expected 161 < 158 < 166 order, got state=%d turn=%d end=%d", sSeq, tSeq, eSeq)
	}
	if err := battleSmokeUnsubscribeSpectate(bot, stats, 9001, 3*time.Second); err != nil {
		t.Fatalf("UnsubscribeSpectate failed: %v", err)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.unsubIds) != 1 || gate.unsubIds[0] != 9001 {
		t.Fatalf("unsubIds=%v want [9001]", gate.unsubIds)
	}
}

func TestBattleLobbySpectateInvalidTicketAndSourceLost(t *testing.T) {
	ticket := []byte("valid-hmac-ticket")
	gate := startFakeLobbySpectateGate(t, ticket, battle.ESpectateEndReason_SPECTATE_END_SOURCE_LOST, true)
	bot := dialFakeLobbyGateBot(t, gate.ln.Addr().String(), 88002)
	stats := metrics.NewStats()

	err := battleSmokeSubscribeSpectate(bot, stats, 9002, []byte("bad-ticket"), 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "16024") {
		t.Fatalf("expected 16024 ticket invalid error, got %v", err)
	}

	if err := battleSmokeSubscribeSpectate(bot, stats, 9002, ticket, 3*time.Second); err != nil {
		t.Fatalf("valid SubscribeSpectate failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reason, err := bot.player.WaitSpectateEnd(ctx)
	if err != nil || battle.ESpectateEndReason(reason) != battle.ESpectateEndReason_SPECTATE_END_SOURCE_LOST {
		t.Fatalf("expected SPECTATE_END_SOURCE_LOST, got reason=%d err=%v", reason, err)
	}
}

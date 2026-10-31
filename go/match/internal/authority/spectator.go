package authority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"match/internal/constants"
	battlepb "proto/battle"
	pb "proto/match"
)

var ErrSpectatorBusy = errors.New("观战原意图尚未关闭或玩家已有匹配占用")

// 下列拒绝原因都包装 ErrSpectatorBusy：既有 errors.Is(err, ErrSpectatorBusy) 判断不变，
// logic 层据此给客户端明确结论；没有细分的裸 ErrSpectatorBusy 仍按未确认处理。
var (
	// ErrSpectatorSuperseded：玩家已有更新的观战请求头，本请求不能再推进。替代发生时
	// SpectatorIntent 已在同一事务给本请求的意图记下关闭原因「已在观战中」，首答与重放同结论。
	ErrSpectatorSuperseded = fmt.Errorf("%w: 已被更新的观战请求替代", ErrSpectatorBusy)
	// ErrSpectatorOccupied：玩家仍有尚未证实关闭的旧观战占用。
	ErrSpectatorOccupied = fmt.Errorf("%w: 旧观战占用尚未关闭", ErrSpectatorBusy)
	// ErrSpectatorFormationClosed：目标成局已不在可观战的已建房状态，或建房端点已变。
	ErrSpectatorFormationClosed = fmt.Errorf("%w: 目标成局不可观战", ErrSpectatorBusy)
	// ErrSpectatorIntentClosed：本请求的观战意图已关闭；结论读意图上保存的 close_tip。
	ErrSpectatorIntentClosed = fmt.Errorf("%w: 原观战请求已关闭", ErrSpectatorBusy)
)

const spectatorColumns = "player_id,operation_id,battle_id,target_endpoint,target_instance_id,state,request_payload,owner_account,owner_revision,add_request,updated_ms,request_high,request_low"
const spectatorIntentColumns = "player_id,request_high,request_low,original,request_payload,owner_account,owner_revision,state,close_tip,delivery"

// spectatorDeliveryLobby 是意图 delivery 列的 LOBBY 取值(eSpectateDelivery):L1 / L2 观众不占 match_spectator 行,
// 意图以 state=5、close_tip=0、delivery=LOBBY 结案,重放时重新做准入并签新凭证(battle-spectate-tiered-fanout.md §15.6)。
const spectatorDeliveryLobby = uint32(pb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY)

func scanSpectator(row scanner) (*pb.MatchSpectatorRecord, error) {
	out := &pb.MatchSpectatorRecord{}
	err := row.Scan(&out.PlayerId, &out.OperationId, &out.BattleId, &out.TargetEndpoint, &out.TargetInstanceId, &out.State, &out.RequestPayload, &out.OwnerAccount, &out.OwnerRevision, &out.AddRequest, &out.UpdatedMs, &out.RequestHigh, &out.RequestLow)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = validateSpectator(out); err != nil {
		return nil, err
	}
	return out, nil
}
func validateSpectator(out *pb.MatchSpectatorRecord) error {
	if out == nil {
		return ErrOriginalPlayer
	}
	request, add := &pb.WatchBattleRequest{}, &battlepb.AddObserverRequest{}
	high, low, err := joinRequestParts(out.OperationId)
	if err != nil || high != out.RequestHigh || low != out.RequestLow || out.PlayerId == 0 || out.BattleId == 0 || out.OwnerAccount == "" || out.OwnerRevision == 0 || out.TargetEndpoint == "" || out.TargetInstanceId == "" || out.State < 1 || out.State > 5 {
		return ErrOriginalPlayer
	}
	if proto.Unmarshal(out.RequestPayload, request) != nil || proto.Unmarshal(out.AddRequest, add) != nil || request.RequestId != out.OperationId || (request.PlayerId != 0 && request.PlayerId != out.PlayerId) || (request.BattleId != 0 && request.BattleId != out.BattleId) || add.ObserverPlayerId != out.PlayerId || add.BattleId != out.BattleId || add.SpectatorOperationId != out.OperationId || add.TargetInstanceId != out.TargetInstanceId || add.AdmitBeforeMs == 0 || add.GetRouting().GetSessionId() == 0 || add.GetRouting().GetGateNodeId() == 0 || add.GetRouting().GetGateInstanceId() == "" {
		return ErrOriginalPlayer
	}
	return nil
}
func spectatorActor(row *pb.MatchSpectatorRecord) *pb.MatchAuthorityTicketRecord {
	return &pb.MatchAuthorityTicketRecord{PlayerId: row.PlayerId, OwnerAccount: row.OwnerAccount, OwnerRevision: row.OwnerRevision}
}
func sameSpectatorOwner(row *pb.MatchSpectatorRecord, actor *pb.MatchAuthorityTicketRecord) bool {
	return row.PlayerId == actor.PlayerId && row.OwnerAccount == actor.OwnerAccount && row.OwnerRevision == actor.OwnerRevision
}
func readSpectator(ctx context.Context, tx *sql.Tx, player uint64) (*pb.MatchSpectatorRecord, error) {
	return scanSpectator(tx.QueryRowContext(ctx, "SELECT "+spectatorColumns+" FROM match_spectator WHERE player_id=? FOR UPDATE", player))
}
func readSpectatorHead(ctx context.Context, tx *sql.Tx, player uint64) (string, error) {
	var head string
	err := tx.QueryRowContext(ctx, "SELECT operation_id FROM match_spectator_head WHERE player_id=? FOR UPDATE", player).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, _, err = joinRequestParts(head); err != nil {
		return "", ErrOriginalPlayer
	}
	return head, nil
}
func requireSpectatorHead(ctx context.Context, tx *sql.Tx, player uint64, expected string) error {
	if _, _, err := joinRequestParts(expected); err != nil {
		return ErrOriginalPlayer
	}
	actual, err := readSpectatorHead(ctx, tx, player)
	if err != nil {
		return err
	}
	if actual != expected {
		return ErrSpectatorSuperseded
	}
	return nil
}
func readSpectatorIntent(ctx context.Context, tx *sql.Tx, player, high, low uint64) (*pb.MatchSpectatorIntentRecord, error) {
	out := &pb.MatchSpectatorIntentRecord{}
	err := tx.QueryRowContext(ctx, "SELECT "+spectatorIntentColumns+" FROM match_spectator_intent WHERE player_id=? AND request_high=? AND request_low=? FOR UPDATE", player, high, low).Scan(&out.PlayerId, &out.RequestHigh, &out.RequestLow, &out.Original, &out.RequestPayload, &out.OwnerAccount, &out.OwnerRevision, &out.State, &out.CloseTip, &out.Delivery)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	request := &pb.WatchBattleRequest{}
	if out.PlayerId != player || out.RequestHigh != high || out.RequestLow != low || out.OwnerAccount == "" || out.OwnerRevision == 0 || proto.Unmarshal(out.RequestPayload, request) != nil || (request.PlayerId != 0 && request.PlayerId != player) {
		return nil, ErrOriginalPlayer
	}
	rh, rl, err := joinRequestParts(request.RequestId)
	if err != nil || rh != high || rl != low {
		return nil, ErrOriginalPlayer
	}
	// LOBBY 与关闭原因二选一(先写为准)；不认识的取值一律按损坏处理。
	if (out.Delivery != 0 && out.Delivery != spectatorDeliveryLobby) || (out.Delivery != 0 && out.CloseTip != 0) {
		return nil, ErrOriginalPlayer
	}
	switch out.State {
	case 1:
		// 关闭原因与投递方式只在关闭或拒绝时写入；尚未发出Add的意图带它们即损坏。
		if len(out.Original) != 0 || out.CloseTip != 0 || out.Delivery != 0 {
			return nil, ErrOriginalPlayer
		}
	case 2, 5:
		if len(out.Original) == 0 {
			// 以LOBBY结案的意图一定固定了目标场(lobbyPin或满员转L1的完整原件)；缺了重放就不知道该看哪一场。
			if out.State != 5 || out.Delivery != 0 {
				return nil, ErrOriginalPlayer
			}
			break
		}
		current := &pb.MatchSpectatorRecord{}
		if proto.Unmarshal(out.Original, current) != nil {
			return nil, ErrOriginalPlayer
		}
		if out.State == 5 && out.Delivery == spectatorDeliveryLobby && len(current.AddRequest) == 0 {
			if !validLobbyPin(current, out, request) {
				return nil, ErrOriginalPlayer
			}
			break
		}
		if validateSpectator(current) != nil || current.PlayerId != player || current.RequestHigh != high || current.RequestLow != low || current.OwnerAccount != out.OwnerAccount || current.OwnerRevision != out.OwnerRevision || !bytes.Equal(current.RequestPayload, out.RequestPayload) || (out.State == 5) != (current.State == 5) {
			return nil, ErrOriginalPlayer
		}
	default:
		return nil, ErrOriginalPlayer
	}
	return out, nil
}

// newLobbyPin 组 L1 / L2 观众的意图原件:只固定这个 request_id 看的是哪一场(随机选场只选一次,重放不能换场)。
// 它不是占用:没有 add_request、没有对应的 match_spectator 行,也没有 battle 侧的 op。
func newLobbyPin(player, high, low, battle, stamp uint64, request *pb.WatchBattleRequest, actor *pb.MatchAuthorityTicketRecord) *pb.MatchSpectatorRecord {
	return &pb.MatchSpectatorRecord{PlayerId: player, OperationId: request.GetRequestId(), BattleId: battle, State: 5, OwnerAccount: actor.OwnerAccount, OwnerRevision: actor.OwnerRevision, UpdatedMs: stamp, RequestHigh: high, RequestLow: low}
}

// validLobbyPin 核对 newLobbyPin 写下的原件与意图一致。指定场的请求只能固定到它指定的那一场。
func validLobbyPin(pin *pb.MatchSpectatorRecord, intent *pb.MatchSpectatorIntentRecord, request *pb.WatchBattleRequest) bool {
	return pin.PlayerId == intent.PlayerId && pin.RequestHigh == intent.RequestHigh && pin.RequestLow == intent.RequestLow && pin.OperationId == request.RequestId && pin.OwnerAccount == intent.OwnerAccount && pin.OwnerRevision == intent.OwnerRevision && pin.State == 5 && pin.BattleId != 0 && (request.BattleId == 0 || request.BattleId == pin.BattleId)
}

// 进入Watch即持久请求头和原payload。重放永久旧UUID绝不再推进head，旧handler也不能
// 在选场/清退之间越过更新的请求。尚未prepare的旧意图无需网络证明即可安全关闭。
func (s *Store) SpectatorIntent(ctx context.Context, player uint64, request *pb.WatchBattleRequest) (*pb.MatchSpectatorIntentRecord, error) {
	high, low, err := joinRequestParts(request.GetRequestId())
	if err != nil {
		return nil, err
	}
	if request.PlayerId != 0 && request.PlayerId != player {
		return nil, ErrOriginalPlayer
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return nil, err
	}
	actor := originalPlayer(ctx, player)
	head, err := readSpectatorHead(ctx, tx, player)
	if err != nil {
		return nil, err
	}
	prior, err := readSpectatorIntent(ctx, tx, player, high, low)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		if prior.OwnerAccount != actor.OwnerAccount || prior.OwnerRevision != actor.OwnerRevision || !bytes.Equal(prior.RequestPayload, payload) {
			return nil, ErrOriginalPlayer
		}
		if prior.State != 5 && head != request.RequestId {
			return nil, ErrSpectatorSuperseded
		}
		return prior, tx.Commit()
	}
	if err = s.requireMatchEntryTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{actor}, nil); err != nil {
		return nil, err
	}
	if err = s.requireIdleMatchTicketsTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{actor}); err != nil {
		return nil, err
	}
	if head != "" {
		hh, hl, _ := joinRequestParts(head)
		old, err := readSpectatorIntent(ctx, tx, player, hh, hl)
		if err != nil {
			return nil, err
		}
		if old == nil {
			return nil, ErrOriginalPlayer
		}
		// 旧请求被本请求替代：同事务给旧意图记下关闭原因（先写为准），旧handler续做时的首答
		// （ErrSpectatorSuperseded）与旧request_id重放都回「已在观战中」。原因是客户端tip，与logic对
		// ErrSpectatorSuperseded的映射同码。未发Add的旧意图直接关闭；已绑定原件的仍由本请求prepare或
		// 后台恢复沿原op取消，关闭到5时保留此原因；已关闭的旧意图不动。
		switch old.State {
		case 1:
			if _, err = tx.ExecContext(ctx, "UPDATE match_spectator_intent SET state=5,close_tip=? WHERE player_id=? AND request_high=? AND request_low=? AND state=1", constants.ErrAlreadyWatching, player, hh, hl); err != nil {
				return nil, err
			}
		case 2:
			// 已记 LOBBY 的旧意图(直连席满、取消进行中)不改写:重放时请求头复核回「已在观战中」,与这里的原因同码。
			if _, err = tx.ExecContext(ctx, "UPDATE match_spectator_intent SET close_tip=? WHERE player_id=? AND request_high=? AND request_low=? AND state=2 AND close_tip=0 AND delivery=0", constants.ErrAlreadyWatching, player, hh, hl); err != nil {
				return nil, err
			}
		}
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := &pb.MatchSpectatorIntentRecord{PlayerId: player, RequestHigh: high, RequestLow: low, RequestPayload: payload, OwnerAccount: actor.OwnerAccount, OwnerRevision: actor.OwnerRevision, State: 1}
	// created_ms 只供保留期清理读(PurgeClosedSpectatorIntents),不进 spectatorIntentColumns:读写意图的路径都不看它。
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_spectator_intent("+spectatorIntentColumns+",created_ms) VALUES(?,?,?,?,?,?,?,1,0,0,?)", player, high, low, []byte{}, payload, actor.OwnerAccount, actor.OwnerRevision, stamp); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_spectator_head(player_id,operation_id) VALUES(?,?) ON DUPLICATE KEY UPDATE operation_id=VALUES(operation_id)", player, request.RequestId); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// 未准备原件还没有网络副作用；明确拒绝（无场可选、成局不可观战、会话已断开）时关闭UUID并记下原因，
// 重试不能重新选择另一局。先写的原因为准：返回意图上实际保存的原因，首答与同request_id重放一致。
func (s *Store) CloseUnpreparedSpectator(ctx context.Context, player uint64, request *pb.WatchBattleRequest, closeTip uint32) (uint32, error) {
	high, low, err := joinRequestParts(request.GetRequestId())
	if err != nil {
		return 0, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	original, err := s.lockHeadSpectatorIntentTx(ctx, tx, player, request, high, low)
	if err != nil {
		return 0, err
	}
	if original.State == 5 {
		return original.CloseTip, tx.Commit()
	}
	if original.State != 1 {
		return 0, ErrSpectatorBusy
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_spectator_intent SET state=5,close_tip=? WHERE player_id=? AND request_high=? AND request_low=? AND state=1", closeTip, player, high, low); err != nil {
		return 0, err
	}
	return closeTip, tx.Commit()
}

// lockHeadSpectatorIntentTx 持玩家P,确认本请求仍是请求头、意图属于当前owner且正文未变,返回意图。
func (s *Store) lockHeadSpectatorIntentTx(ctx context.Context, tx *sql.Tx, player uint64, request *pb.WatchBattleRequest, high, low uint64) (*pb.MatchSpectatorIntentRecord, error) {
	if err := s.lockActor(ctx, tx, player); err != nil {
		return nil, err
	}
	if err := requireSpectatorHead(ctx, tx, player, request.RequestId); err != nil {
		return nil, err
	}
	intent, err := readSpectatorIntent(ctx, tx, player, high, low)
	if err != nil {
		return nil, err
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, err
	}
	actor := originalPlayer(ctx, player)
	if intent == nil || intent.OwnerAccount != actor.OwnerAccount || intent.OwnerRevision != actor.OwnerRevision || !bytes.Equal(payload, intent.RequestPayload) {
		return nil, ErrOriginalPlayer
	}
	return intent, nil
}

// CloseLobbySpectator 以 LOBBY 结案尚未发出Add的意图(L1 / L2 观众,battle-spectate-tiered-fanout.md §15.6):不写 match_spectator、
// 不锁 match_formation,目标场固定在意图原件上,重放不换场。调用方在中继准入成功之后才调。
// 意图已关闭时不改写,返回意图上先写的结论(delivery 不是 LOBBY 时调用方回关闭原因,这次准入的凭证不发出去)。
func (s *Store) CloseLobbySpectator(ctx context.Context, player uint64, request *pb.WatchBattleRequest, battle uint64) (*pb.MatchSpectatorIntentRecord, error) {
	high, low, err := joinRequestParts(request.GetRequestId())
	if err != nil {
		return nil, err
	}
	if battle == 0 || (request.GetBattleId() != 0 && request.GetBattleId() != battle) {
		return nil, ErrOriginalPlayer
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	intent, err := s.lockHeadSpectatorIntentTx(ctx, tx, player, request, high, low)
	if err != nil {
		return nil, err
	}
	if intent.State == 5 {
		return intent, tx.Commit()
	}
	if intent.State != 1 {
		return nil, ErrSpectatorBusy
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	pin, err := proto.MarshalOptions{Deterministic: true}.Marshal(newLobbyPin(player, high, low, battle, stamp, request, originalPlayer(ctx, player)))
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_spectator_intent SET original=?,state=5,delivery=? WHERE player_id=? AND request_high=? AND request_low=? AND state=1", pin, spectatorDeliveryLobby, player, high, low); err != nil {
		return nil, err
	}
	intent.Original, intent.State, intent.Delivery = pin, 5, spectatorDeliveryLobby
	return intent, tx.Commit()
}

// RequireLobbyReplay 是重放以 LOBBY 结案的意图之前的复核。结案的意图不再占请求头,SpectatorIntent 对它不做任何检查,
// 不复核的话,旧请求的重放能在玩家已开新观战、已排队或已开战之后重新拿到凭证。不符时返回与首次进入相同的错误
// (ErrSpectatorSuperseded、匹配票占用细分),客户端得到同样的结论。只读:L1 / L2 是软互斥(Q1),这里不留任何占用。
func (s *Store) RequireLobbyReplay(ctx context.Context, player uint64, requestID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return err
	}
	if err = requireSpectatorHead(ctx, tx, player, requestID); err != nil {
		return err
	}
	actor := originalPlayer(ctx, player)
	if err = s.requireMatchEntryTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{actor}, nil); err != nil {
		return err
	}
	if err = s.requireIdleMatchTicketsTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{actor}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SpectatorForActor(ctx context.Context, player uint64, expectedHead string) (*pb.MatchSpectatorRecord, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return nil, err
	}
	if err = requireSpectatorHead(ctx, tx, player, expectedHead); err != nil {
		return nil, err
	}
	out, err := readSpectator(ctx, tx, player)
	if err != nil {
		return nil, err
	}
	if out != nil && !sameSpectatorOwner(out, originalPlayer(ctx, player)) {
		if out.State != 5 {
			return nil, ErrOriginalPlayer
		}
		// 原占用已证实关闭后允许合法换主；旧终态不把新owner永久锁在观战之外。
		out = nil
	}
	return out, tx.Commit()
}

// 原Add在网络副作用前落SQL；随机选择只在首次prepare固定。head复核与占用提交同一P。
func (s *Store) BeginSpectator(ctx context.Context, player uint64, request *pb.WatchBattleRequest, formation *pb.MatchFormationRecord, add *battlepb.AddObserverRequest) (*pb.MatchSpectatorRecord, error) {
	high, low, err := joinRequestParts(request.GetRequestId())
	if err != nil {
		return nil, err
	}
	if formation == nil || add == nil || formation.State != 4 || formation.BattleId == 0 {
		return nil, ErrSpectatorFormationClosed
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return nil, err
	}
	if err = requireSpectatorHead(ctx, tx, player, request.RequestId); err != nil {
		return nil, err
	}
	actor := originalPlayer(ctx, player)
	prior, err := readSpectatorIntent(ctx, tx, player, high, low)
	if err != nil {
		return nil, err
	}
	if prior == nil || prior.OwnerAccount != actor.OwnerAccount || prior.OwnerRevision != actor.OwnerRevision || !bytes.Equal(prior.RequestPayload, payload) {
		return nil, ErrOriginalPlayer
	}
	if prior.State == 5 {
		return nil, ErrSpectatorIntentClosed
	}
	if prior.State == 2 {
		original := &pb.MatchSpectatorRecord{}
		if err = proto.Unmarshal(prior.Original, original); err != nil {
			return nil, err
		}
		current, err := readSpectator(ctx, tx, player)
		if err != nil {
			return nil, err
		}
		if current == nil || !proto.Equal(current, original) {
			return nil, ErrOriginalPlayer
		}
		return original, tx.Commit()
	}
	current, err := readSpectator(ctx, tx, player)
	if err != nil {
		return nil, err
	}
	if current != nil && current.State != 5 {
		return nil, ErrSpectatorOccupied
	}
	if err = s.requireMatchEntryTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{actor}, nil); err != nil {
		return nil, err
	}
	ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", player))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && TicketOccupied(ticket) {
		// 同时保留观战占用与票据细分两条链，调用方可按任一判断。
		return nil, fmt.Errorf("%w: %w", ErrSpectatorBusy, ticketOccupiedErr(ticket))
	}
	var state uint32
	var endpoint, instance string
	if err = tx.QueryRowContext(ctx, "SELECT state,target_endpoint,target_instance_id FROM match_formation WHERE battle_id=? FOR UPDATE", formation.BattleId).Scan(&state, &endpoint, &instance); err != nil {
		return nil, err
	}
	if state != 4 || endpoint != formation.TargetEndpoint || instance != formation.TargetInstanceId {
		return nil, ErrSpectatorFormationClosed
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	add = proto.Clone(add).(*battlepb.AddObserverRequest)
	add.BattleId, add.ObserverPlayerId, add.SpectatorOperationId, add.TargetInstanceId, add.AdmitBeforeMs = formation.BattleId, player, request.RequestId, instance, stamp+30000
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(add)
	if err != nil {
		return nil, err
	}
	out := &pb.MatchSpectatorRecord{PlayerId: player, OperationId: request.RequestId, BattleId: formation.BattleId, TargetEndpoint: endpoint, TargetInstanceId: instance, State: 1, RequestPayload: payload, OwnerAccount: actor.OwnerAccount, OwnerRevision: actor.OwnerRevision, AddRequest: raw, UpdatedMs: stamp, RequestHigh: high, RequestLow: low}
	if err = validateSpectator(out); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_spectator("+spectatorColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE operation_id=VALUES(operation_id),battle_id=VALUES(battle_id),target_endpoint=VALUES(target_endpoint),target_instance_id=VALUES(target_instance_id),state=VALUES(state),request_payload=VALUES(request_payload),owner_account=VALUES(owner_account),owner_revision=VALUES(owner_revision),add_request=VALUES(add_request),updated_ms=VALUES(updated_ms),request_high=VALUES(request_high),request_low=VALUES(request_low)", out.PlayerId, out.OperationId, out.BattleId, out.TargetEndpoint, out.TargetInstanceId, out.State, out.RequestPayload, out.OwnerAccount, out.OwnerRevision, out.AddRequest, out.UpdatedMs, out.RequestHigh, out.RequestLow)
	if err != nil {
		return nil, err
	}
	record, err := proto.MarshalOptions{Deterministic: true}.Marshal(out)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_spectator_intent SET original=?,state=2 WHERE player_id=? AND request_high=? AND request_low=? AND state=1", record, player, high, low); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// 后台只沿原op推进；前台还须沿新请求head，旧handler不得清退新请求接纳的占用。
func (s *Store) AdvanceSpectator(ctx context.Context, original *pb.MatchSpectatorRecord, state uint32) (bool, error) {
	return s.advanceSpectator(ctx, original, state, "", spectatorConclusion{})
}
func (s *Store) AdvanceSpectatorForHead(ctx context.Context, original *pb.MatchSpectatorRecord, state uint32, expectedHead string) (bool, error) {
	if expectedHead == "" {
		return false, ErrOriginalPlayer
	}
	return s.advanceSpectator(ctx, original, state, expectedHead, spectatorConclusion{})
}

// RejectSpectator 把battle明确拒绝（或准入已截止）的原件转入待取消3，并在同一事务里记下关闭原因。
// 拒绝即终态：转入3后迟到的同op Add回包不能再推进成准入；原因先写为准，取消证明关闭到5时保留。
// expectedHead为空表示后台沿原op推进；返回false表示原op已证实关闭，结论以意图上先写的原因为准。
func (s *Store) RejectSpectator(ctx context.Context, original *pb.MatchSpectatorRecord, closeTip uint32, expectedHead string) (bool, error) {
	if closeTip == 0 {
		return false, errors.New("观战拒绝必须携带关闭原因")
	}
	return s.advanceSpectator(ctx, original, 3, expectedHead, spectatorConclusion{closeTip: closeTip})
}

// RejectSpectatorToLobby 把battle回「直连席已满」的原件转入待取消3,并在同一事务里记下 delivery=LOBBY(不写16021):
// 取消证明关闭到5之后,这个request_id走 L1 / L2 准入。返回值与 RejectSpectator 同义。
func (s *Store) RejectSpectatorToLobby(ctx context.Context, original *pb.MatchSpectatorRecord, expectedHead string) (bool, error) {
	return s.advanceSpectator(ctx, original, 3, expectedHead, spectatorConclusion{delivery: spectatorDeliveryLobby})
}

// spectatorConclusion 是原件推进时同事务记下的结论:关闭原因与 LOBBY 二选一,先写为准。零值表示不记。
type spectatorConclusion struct {
	closeTip uint32
	delivery uint32
}

// conclusion非零时与状态推进同事务记下结论（只在推进成功时写）。
func (s *Store) advanceSpectator(ctx context.Context, original *pb.MatchSpectatorRecord, state uint32, expectedHead string, conclusion spectatorConclusion) (bool, error) {
	if validateSpectator(original) != nil || state < 2 || state > 5 {
		return false, ErrOriginalPlayer
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{spectatorActor(original)}); err != nil {
		return false, err
	}
	if expectedHead != "" {
		if !sameSpectatorOwner(original, originalPlayer(ctx, original.PlayerId)) {
			return false, ErrOriginalPlayer
		}
		if err = requireSpectatorHead(ctx, tx, original.PlayerId, expectedHead); err != nil {
			return false, err
		}
	}
	current, err := readSpectator(ctx, tx, original.PlayerId)
	if err != nil {
		return false, err
	}
	if current == nil || current.OperationId != original.OperationId || !sameSpectatorOwner(current, spectatorActor(original)) || !bytes.Equal(current.AddRequest, original.AddRequest) || !bytes.Equal(current.RequestPayload, original.RequestPayload) || current.TargetEndpoint != original.TargetEndpoint || current.TargetInstanceId != original.TargetInstanceId {
		return false, ErrSpectatorBusy
	}
	receipt, err := readSpectatorIntent(ctx, tx, current.PlayerId, current.RequestHigh, current.RequestLow)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		return false, ErrOriginalPlayer
	}
	rawCurrent, err := proto.MarshalOptions{Deterministic: true}.Marshal(current)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(receipt.Original, rawCurrent) {
		return false, ErrOriginalPlayer
	}
	if current.State == state {
		if err = writeSpectatorConclusionTx(ctx, tx, current, conclusion); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if current.State == 5 {
		return false, tx.Commit()
	}
	if state == 2 && current.State != 1 && current.State != 2 && current.State != 4 {
		return false, nil
	}
	if state == 4 && current.State == 3 {
		return false, nil
	}
	if err = writeSpectatorStateTx(ctx, tx, current, state); err != nil {
		return false, err
	}
	if err = writeSpectatorConclusionTx(ctx, tx, current, conclusion); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// writeSpectatorConclusionTx 记下原观战请求的结论；已有结论时保持不变（先写为准），零值不写。
// 调用方已持该玩家P并确认当前占用就是本原件，意图此时处于2或5，不会给尚未发出Add的意图写结论。
func writeSpectatorConclusionTx(ctx context.Context, tx *sql.Tx, current *pb.MatchSpectatorRecord, conclusion spectatorConclusion) error {
	if conclusion == (spectatorConclusion{}) {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE match_spectator_intent SET close_tip=?,delivery=? WHERE player_id=? AND request_high=? AND request_low=? AND close_tip=0 AND delivery=0", conclusion.closeTip, conclusion.delivery, current.PlayerId, current.RequestHigh, current.RequestLow)
	return err
}

func (s *Store) SpectatorRecoveryHighWater(ctx context.Context) (uint64, error) {
	var player uint64
	err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(MAX(player_id),0) FROM match_spectator").Scan(&player)
	return player, err
}

// 恢复重读原op，不能用旧扫描页中的state发Add；新head接纳后若前台崩溃，后台仍应
// 收尾旧占用，而不是把旧active永久保留。head仅决定需取消，不作为后台原op的授权。
func (s *Store) SpectatorRecoveryCurrent(ctx context.Context, observed *pb.MatchSpectatorRecord) (*pb.MatchSpectatorRecord, bool, error) {
	if validateSpectator(observed) != nil {
		return nil, false, ErrOriginalPlayer
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{spectatorActor(observed)}); err != nil {
		return nil, false, err
	}
	current, err := readSpectator(ctx, tx, observed.PlayerId)
	if err != nil {
		return nil, false, err
	}
	if current == nil || current.OperationId != observed.OperationId || !sameSpectatorOwner(current, spectatorActor(observed)) || !bytes.Equal(current.AddRequest, observed.AddRequest) {
		return nil, false, ErrSpectatorBusy
	}
	receipt, err := readSpectatorIntent(ctx, tx, current.PlayerId, current.RequestHigh, current.RequestLow)
	if err != nil {
		return nil, false, err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(current)
	if err != nil {
		return nil, false, err
	}
	if receipt == nil || !bytes.Equal(receipt.Original, raw) {
		return nil, false, ErrOriginalPlayer
	}
	head, err := readSpectatorHead(ctx, tx, observed.PlayerId)
	if err != nil {
		return nil, false, err
	}
	if head == "" {
		return nil, false, ErrOriginalPlayer
	}
	return current, head != current.OperationId, tx.Commit()
}
func (s *Store) SpectatorsThrough(ctx context.Context, afterPlayer, through uint64) ([]*pb.MatchSpectatorRecord, error) {
	// 每状态独立索引页，最多读32行后合并8行；历史closed再多也不靠PK全表扫描。
	var queries []string
	var args []any
	for state := uint32(1); state <= 4; state++ {
		queries = append(queries, "(SELECT "+spectatorColumns+" FROM match_spectator WHERE state=? AND player_id>? AND player_id<=? ORDER BY player_id LIMIT 8)")
		args = append(args, state, afterPlayer, through)
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT * FROM ("+strings.Join(queries, " UNION ALL ")+") pending ORDER BY player_id LIMIT 8", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchSpectatorRecord
	for rows.Next() {
		row, err := scanSpectator(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SpectatorFormationEnded 判定观战目标成局的房间必已不在:成局已关(state 5),或已过权威截止 + 终局提交宽限,
// 房间必已被 battle 定时器收尾(与到期终局恢复裁决 ABORT 同一分界,terminalAbortDue)。stamp 取 TiDB 时钟。
func SpectatorFormationEnded(formation *pb.MatchFormationRecord, stamp uint64) bool {
	return formation.GetState() == 5 || terminalAbortDue(formation.GetDeadlineMs(), stamp)
}

// spectatorIntentRetentionMs 是已关闭意图的保留期:7 天。意图只为同 request_id 重放提供同一结论,重放窗口是秒级
// (客户端重试、大厅重连),一场战斗最长约 1 小时,7 天之后不会再有合法重放。不删的话 L1 / L2 观众每个新 request_id 永久留一行,
// 存储无界增长。代价:极旧的 request_id 重放会被当成新请求,结论是一次普通的新观战。
//
// 按创建时刻算,不按关闭时刻算:意图从创建到关闭最多约 1 小时(战斗期限 + 宽限,之后原件必能关闭,见 logic 的 spectatorSourceGoneReason),
// 相对 7 天可以忽略,而创建时刻只有一个写入点。
const spectatorIntentRetentionMs uint64 = 7 * 24 * 3600 * 1000

// SpectatorIntentCursor 是保留期清理的翻页位置,零值 = 从头。
type SpectatorIntentCursor struct {
	CreatedMs, Player, High, Low uint64
}

// PurgeClosedSpectatorIntents 读一页 state=5 且超过保留期的意图,删掉其中不是请求头的行,返回下一页的位置与本轮删除行数。
// 扫到末尾时位置回到零值。请求头的行要留着:SpectatorIntent 与 CaptureSpectatorExitsTx 按请求头读意图,读不到按损坏拒绝。
// 这些行可能很多(每名看过观战的玩家一行)而且会一直留在索引前端,所以必须翻页,不能每轮从头取前 N 条。
//
// 幂等,多副本同时跑无害。不持玩家锁也不会误删:state=5 是终态;请求头只会移到全新的 request_id,不是请求头的行不会变回请求头。
// 唯一的例外是行被删后旧 request_id 重放重新插入,它的 created_ms 是新的,DELETE 上的 created_ms 条件挡住这一条。
func (s *Store) PurgeClosedSpectatorIntents(ctx context.Context, after SpectatorIntentCursor, limit int) (SpectatorIntentCursor, int64, error) {
	stamp, err := s.Now(ctx)
	if err != nil {
		return after, 0, err
	}
	if stamp < spectatorIntentRetentionMs {
		return SpectatorIntentCursor{}, 0, nil
	}
	cutoff := stamp - spectatorIntentRetentionMs
	page, err := s.closedSpectatorIntentPage(ctx, after, cutoff, limit)
	if err != nil {
		return after, 0, err
	}
	var next SpectatorIntentCursor
	if len(page) > 0 && len(page) == limit {
		next = page[len(page)-1]
	}
	heads, err := s.spectatorHeads(ctx, page)
	if err != nil {
		return after, 0, err
	}
	var placeholders []string
	var args []any
	for _, row := range page {
		if head, found := heads[row.Player]; found {
			high, low, err := joinRequestParts(head)
			if err != nil || (high == row.High && low == row.Low) {
				// 请求头解析不了就不动这名玩家的行:读路径会按损坏拒绝,留给人工。
				continue
			}
		}
		placeholders = append(placeholders, "(?,?,?)")
		args = append(args, row.Player, row.High, row.Low)
	}
	if len(placeholders) == 0 {
		return next, 0, nil
	}
	args = append(args, cutoff)
	result, err := s.DB.ExecContext(ctx, "DELETE FROM match_spectator_intent WHERE (player_id,request_high,request_low) IN ("+strings.Join(placeholders, ",")+") AND state=5 AND created_ms<?", args...)
	if err != nil {
		return after, 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return after, 0, err
	}
	return next, deleted, nil
}

// closedSpectatorIntentPage 沿 (state, created_ms, 主键) 索引读 after 之后的一页已关闭意图。
// created_ms>= 是给优化器的范围下界,元组比较才是精确的翻页条件。
func (s *Store) closedSpectatorIntentPage(ctx context.Context, after SpectatorIntentCursor, cutoff uint64, limit int) ([]SpectatorIntentCursor, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT created_ms,player_id,request_high,request_low FROM match_spectator_intent WHERE state=5 AND created_ms<? AND created_ms>=? AND (created_ms,player_id,request_high,request_low)>(?,?,?,?) ORDER BY created_ms,player_id,request_high,request_low LIMIT ?", cutoff, after.CreatedMs, after.CreatedMs, after.Player, after.High, after.Low, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []SpectatorIntentCursor
	for rows.Next() {
		var row SpectatorIntentCursor
		if err = rows.Scan(&row.CreatedMs, &row.Player, &row.High, &row.Low); err != nil {
			return nil, err
		}
		page = append(page, row)
	}
	return page, rows.Err()
}

// spectatorHeads 读这一页涉及的玩家的请求头。不开事务、不加锁:判定依据见 PurgeClosedSpectatorIntents。
func (s *Store) spectatorHeads(ctx context.Context, page []SpectatorIntentCursor) (map[uint64]string, error) {
	heads := make(map[uint64]string, len(page))
	if len(page) == 0 {
		return heads, nil
	}
	placeholders := make([]string, 0, len(page))
	args := make([]any, 0, len(page))
	for _, row := range page {
		placeholders = append(placeholders, "?")
		args = append(args, row.Player)
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT player_id,operation_id FROM match_spectator_head WHERE player_id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var player uint64
		var head string
		if err = rows.Scan(&player, &head); err != nil {
			return nil, err
		}
		heads[player] = head
	}
	return heads, rows.Err()
}

// 调用者已持完整原P集合；占用缺失/精确5才可继续，零或未知state不能当无占用。
func (s *Store) RequireNoSpectators(ctx context.Context, tx *sql.Tx, players []uint64) error {
	players = append([]uint64(nil), players...)
	sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
	for _, player := range players {
		var state uint32
		err := tx.QueryRowContext(ctx, "SELECT state FROM match_spectator WHERE player_id=? FOR UPDATE", player).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if state != 5 {
			return ErrSpectatorBusy
		}
	}
	return nil
}
func (s *Store) AuthorizeBattleTicket(ctx context.Context, player, battle uint64) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return "", err
	}
	watch, err := readSpectator(ctx, tx, player)
	if err != nil {
		return "", err
	}
	ticket, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", player))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	actor := originalPlayer(ctx, player)
	if err == nil && ticket.State == 3 && ticket.FormationId == battle && ticket.OwnerAccount == actor.OwnerAccount && ticket.OwnerRevision == actor.OwnerRevision {
		return "", tx.Commit()
	}
	if watch == nil || watch.State != 2 || watch.BattleId != battle || !sameSpectatorOwner(watch, actor) {
		return "", ErrOriginalPlayer
	}
	if err = requireSpectatorHead(ctx, tx, player, watch.OperationId); err != nil {
		return "", err
	}
	receipt, err := readSpectatorIntent(ctx, tx, player, watch.RequestHigh, watch.RequestLow)
	if err != nil {
		return "", err
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(watch)
	if err != nil {
		return "", err
	}
	if receipt == nil || receipt.State != 2 || !bytes.Equal(receipt.Original, raw) {
		return "", ErrOriginalPlayer
	}
	return watch.OperationId, tx.Commit()
}

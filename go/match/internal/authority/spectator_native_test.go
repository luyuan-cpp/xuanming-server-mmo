package authority

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/constants"
	battlepb "proto/battle"
	pb "proto/match"
)

type nativeSpectatorFixture struct {
	store     *Store
	actor     context.Context
	formation *pb.MatchFormationRecord
	add       *battlepb.AddObserverRequest
}

// 真实TiDB隔离库和原生account_role P围栏；房间4也沿Claim/Finish提交，不用目录fake。
func nativeSpectator(t *testing.T) *nativeSpectatorFixture {
	t.Helper()
	s := nativeRoleStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	_, a, _ := nativeRoleJoin(t, s, ctx, 20)
	_, b, _ := nativeRoleJoin(t, s, ctx, 21)
	f := &pb.MatchFormationRecord{BattleId: 700, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 1, TargetInstanceId: uuid.NewString(), TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}
	require.NoError(t, s.Claim(ctx, f, []*pb.MatchAuthorityTicketRecord{a, b}))
	work, err := s.AcquireWork(ctx, f.BattleId)
	require.NoError(t, err)
	require.NoError(t, s.Finish(ctx, work, true))
	f, err = s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "INSERT INTO account_role VALUES(11,'spectator-original',7,0)")
	require.NoError(t, err)
	actor := nativeRoleActor(t, ctx, 11, "spectator-original", 7)
	return &nativeSpectatorFixture{store: s, actor: actor, formation: f, add: &battlepb.AddObserverRequest{ObserverName: "公开角色名", Routing: &battlepb.BattleRouting{SessionId: 1, GateNodeId: 2, GateInstanceId: uuid.NewString(), ZoneId: 1}}}
}
func (f *nativeSpectatorFixture) request() *pb.WatchBattleRequest {
	return &pb.WatchBattleRequest{RequestId: uuid.NewString()}
}
func (f *nativeSpectatorFixture) begin(t *testing.T, request *pb.WatchBattleRequest) *pb.MatchSpectatorRecord {
	t.Helper()
	_, err := f.store.SpectatorIntent(f.actor, 11, request)
	require.NoError(t, err)
	original, err := f.store.BeginSpectator(f.actor, 11, request, f.formation, f.add)
	require.NoError(t, err)
	return original
}

func TestSpectatorTiDBHeadPrecedesSelectionAndOldUUIDNeverRetakesHead(t *testing.T) {
	f := nativeSpectator(t)
	a, b := f.request(), f.request()
	first, err := f.store.SpectatorIntent(f.actor, 11, a)
	require.NoError(t, err)
	require.Equal(t, uint32(1), first.State)
	require.Empty(t, first.Original)
	_, err = f.store.SpectatorIntent(f.actor, 11, b)
	require.NoError(t, err)
	_, err = f.store.BeginSpectator(f.actor, 11, a, f.formation, f.add)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	require.ErrorIs(t, err, ErrSpectatorSuperseded, "旧请求头被替代须可区分,客户端回「已在观战中」")
	original, err := f.store.BeginSpectator(f.actor, 11, b, f.formation, f.add)
	require.NoError(t, err)
	// ACK未知后重放Begin不能重选随机场或刷新准入截止。
	other := proto.Clone(f.formation).(*pb.MatchFormationRecord)
	other.BattleId++
	replay, err := f.store.BeginSpectator(f.actor, 11, b, other, f.add)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, replay))
	old, err := f.store.SpectatorIntent(f.actor, 11, a)
	require.NoError(t, err)
	require.Equal(t, uint32(5), old.State)
	require.Empty(t, old.Original)
	current, err := f.store.SpectatorForActor(f.actor, 11, b.RequestId)
	require.NoError(t, err)
	require.Equal(t, b.RequestId, current.OperationId)
	changed := proto.Clone(b).(*pb.WatchBattleRequest)
	changed.BattleId = f.formation.BattleId
	_, err = f.store.SpectatorIntent(f.actor, 11, changed)
	require.ErrorIs(t, err, ErrOriginalPlayer)
}

func TestSpectatorTiDBLateFrontendCannotCloseNewHeadButBackgroundCanFinishOldOp(t *testing.T) {
	f := nativeSpectator(t)
	a, b, c := f.request(), f.request(), f.request()
	original := f.begin(t, a)
	ok, err := f.store.AdvanceSpectatorForHead(f.actor, original, 2, a.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.store.SpectatorIntent(f.actor, 11, b)
	require.NoError(t, err)
	ok, err = f.store.AdvanceSpectatorForHead(f.actor, original, 3, b.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.store.SpectatorIntent(f.actor, 11, c)
	require.NoError(t, err)
	// B的Remove回包迟到；不能在C的head下提交前台闭环。
	ok, err = f.store.AdvanceSpectatorForHead(f.actor, original, 5, b.RequestId)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	require.False(t, ok)
	recovering, superseded, err := f.store.SpectatorRecoveryCurrent(context.Background(), original)
	require.NoError(t, err)
	require.True(t, superseded)
	require.Equal(t, uint32(3), recovering.State)
	ok, err = f.store.AdvanceSpectator(context.Background(), original, 2)
	require.NoError(t, err)
	require.False(t, ok, "迟到Add不能复活取消中的op")
	ok, err = f.store.AdvanceSpectator(context.Background(), original, 5)
	require.NoError(t, err)
	require.True(t, ok)
	latest, err := f.store.BeginSpectator(f.actor, 11, c, f.formation, f.add)
	require.NoError(t, err)
	for _, state := range []uint32{2, 3, 5} {
		ok, err = f.store.AdvanceSpectator(context.Background(), original, state)
		require.ErrorIs(t, err, ErrSpectatorBusy)
		require.False(t, ok)
	}
	current, err := f.store.SpectatorForActor(f.actor, 11, c.RequestId)
	require.NoError(t, err)
	require.True(t, proto.Equal(latest, current))
}

func TestSpectatorTiDBOriginalOwnerTransferAndFreezeRejectWithoutChangingIntents(t *testing.T) {
	for _, freeze := range []bool{false, true} {
		t.Run(fmt.Sprint(freeze), func(t *testing.T) {
			f := nativeSpectator(t)
			request := f.request()
			original := f.begin(t, request)
			if freeze {
				_, err := f.store.DB.ExecContext(f.actor, "UPDATE account_role SET trade_listing_id=99 WHERE player_id=11")
				require.NoError(t, err)
			} else {
				_, err := f.store.DB.ExecContext(f.actor, "UPDATE account_role SET account='new-owner',owner_revision=8 WHERE player_id=11")
				require.NoError(t, err)
			}
			_, err := f.store.SpectatorIntent(f.actor, 11, request)
			require.ErrorIs(t, err, ErrOriginalPlayer)
			_, err = f.store.BeginSpectator(f.actor, 11, request, f.formation, f.add)
			require.ErrorIs(t, err, ErrOriginalPlayer)
			ok, err := f.store.AdvanceSpectator(context.Background(), original, 2)
			require.ErrorIs(t, err, ErrOriginalPlayer)
			require.False(t, ok)
			_, err = f.store.AuthorizeBattleTicket(f.actor, 11, f.formation.BattleId)
			require.ErrorIs(t, err, ErrOriginalPlayer)
			if !freeze {
				newActor := nativeRoleActor(t, f.actor, 11, "new-owner", 8)
				_, err = f.store.SpectatorIntent(newActor, 11, request)
				require.ErrorIs(t, err, ErrOriginalPlayer)
			}
			var state uint32
			var head string
			require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT state FROM match_spectator WHERE player_id=11").Scan(&state))
			require.Equal(t, uint32(1), state)
			require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT operation_id FROM match_spectator_head WHERE player_id=11").Scan(&head))
			require.Equal(t, request.RequestId, head)
		})
	}
}

func TestSpectatorTiDBActiveTicketAndSpectatorExcludeEachOtherAndUnknownNeverMeansFree(t *testing.T) {
	f := nativeSpectator(t)
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	profile := &pb.MatchNetworkProfile{PlanId: "spectator-plan", SessionBinding: "bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 11, PhysicalRegion: 1, ExpiresAtMs: stamp + 120000}, RttMs: 20, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}}}
	require.NoError(t, f.store.SavePlan(f.actor, 11, profile, stamp+120000))
	request := f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, request)
	require.NoError(t, err)
	ticket, created, err := f.store.Join(f.actor, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2}, profile.PlanId, profile.SessionBinding)
	require.NoError(t, err)
	require.True(t, created)
	for _, state := range []uint32{1, 0, 99} {
		_, err = f.store.DB.ExecContext(f.actor, "UPDATE match_authority_ticket SET state=? WHERE player_id=11", state)
		require.NoError(t, err)
		_, err = f.store.BeginSpectator(f.actor, 11, request, f.formation, f.add)
		require.ErrorIs(t, err, ErrSpectatorBusy)
		require.ErrorIs(t, err, ErrTicketQueued, "非建房占用一律按匹配中区分")
		require.NotErrorIs(t, err, ErrTicketInBattle)
	}
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE match_authority_ticket SET state=1 WHERE player_id=11")
	require.NoError(t, err)
	require.NoError(t, f.store.CancelIntent(f.actor, 11, ticket.Ticket, ticket.RequestId))
	_, err = f.store.BeginSpectator(f.actor, 11, request, f.formation, f.add)
	require.NoError(t, err)
	for _, state := range []uint32{1, 0, 99} {
		_, err = f.store.DB.ExecContext(f.actor, "UPDATE match_spectator SET state=? WHERE player_id=11", state)
		require.NoError(t, err)
		_, _, err = f.store.Join(f.actor, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2}, profile.PlanId, profile.SessionBinding)
		require.ErrorIs(t, err, ErrSpectatorBusy)
	}
}

func TestSpectatorTiDBTicketRefreshRequiresActiveOriginalHeadAndHistory(t *testing.T) {
	f := nativeSpectator(t)
	a := f.request()
	original := f.begin(t, a)
	_, err := f.store.AuthorizeBattleTicket(f.actor, 11, f.formation.BattleId)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	ok, err := f.store.AdvanceSpectatorForHead(f.actor, original, 2, a.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	op, err := f.store.AuthorizeBattleTicket(f.actor, 11, f.formation.BattleId)
	require.NoError(t, err)
	require.Equal(t, a.RequestId, op)
	_, err = f.store.AuthorizeBattleTicket(f.actor, 11, f.formation.BattleId+1)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	_, err = f.store.SpectatorIntent(f.actor, 11, f.request())
	require.NoError(t, err)
	_, err = f.store.AuthorizeBattleTicket(f.actor, 11, f.formation.BattleId)
	require.ErrorIs(t, err, ErrSpectatorBusy)
}

func TestSpectatorTiDBClosedOriginalAllowsNewOwnerNewIntentWithoutReauthorizingOldUUID(t *testing.T) {
	f := nativeSpectator(t)
	oldRequest := f.request()
	original := f.begin(t, oldRequest)
	ok, err := f.store.AdvanceSpectator(context.Background(), original, 5)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE account_role SET account='new-owner',owner_revision=8 WHERE player_id=11")
	require.NoError(t, err)
	actor := nativeRoleActor(t, f.actor, 11, "new-owner", 8)
	_, err = f.store.SpectatorIntent(actor, 11, oldRequest)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	request := f.request()
	_, err = f.store.SpectatorIntent(actor, 11, request)
	require.NoError(t, err)
	current, err := f.store.SpectatorForActor(actor, 11, request.RequestId)
	require.NoError(t, err)
	require.Nil(t, current)
	current, err = f.store.BeginSpectator(actor, 11, request, f.formation, f.add)
	require.NoError(t, err)
	require.Equal(t, "new-owner", current.OwnerAccount)
	require.EqualValues(t, 8, current.OwnerRevision)
}

func TestSpectatorTiDBConcurrentUnpreparedRequestsOnlyCurrentHeadCanPrepare(t *testing.T) {
	f := nativeSpectator(t)
	requests := []*pb.WatchBattleRequest{f.request(), f.request()}
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	start := make(chan struct{})
	for _, request := range requests {
		workers.Add(1)
		go func(request *pb.WatchBattleRequest) {
			defer workers.Done()
			<-start
			_, err := f.store.SpectatorIntent(f.actor, 11, request)
			failures <- err
		}(request)
	}
	close(start)
	workers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var head string
	require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT operation_id FROM match_spectator_head WHERE player_id=11").Scan(&head))
	for _, request := range requests {
		_, err := f.store.BeginSpectator(f.actor, 11, request, f.formation, f.add)
		if request.RequestId == head {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrSpectatorBusy)
		}
	}
}

func TestSpectatorTiDBFixedRecoveryWatermarkAndBoundedStatePages(t *testing.T) {
	f := nativeSpectator(t)
	var active []uint64
	for player := uint64(30); player < 48; player++ {
		account := fmt.Sprintf("observer-%d", player)
		_, err := f.store.DB.ExecContext(f.actor, "INSERT INTO account_role VALUES(?,?,7,0)", player, account)
		require.NoError(t, err)
		actor := nativeRoleActor(t, f.actor, player, account, 7)
		request := f.request()
		_, err = f.store.SpectatorIntent(actor, player, request)
		require.NoError(t, err)
		original, err := f.store.BeginSpectator(actor, player, request, f.formation, f.add)
		require.NoError(t, err)
		state := uint32(4)
		if player%2 == 0 {
			state = 5
		} else {
			active = append(active, player)
		}
		ok, err := f.store.AdvanceSpectator(context.Background(), original, state)
		require.NoError(t, err)
		require.True(t, ok)
	}
	through, err := f.store.SpectatorRecoveryHighWater(f.actor)
	require.NoError(t, err)
	require.Equal(t, uint64(47), through)
	_, err = f.store.DB.ExecContext(f.actor, "INSERT INTO account_role VALUES(90,'observer-later',7,0)")
	require.NoError(t, err)
	laterActor := nativeRoleActor(t, f.actor, 90, "observer-later", 7)
	laterRequest := f.request()
	_, err = f.store.SpectatorIntent(laterActor, 90, laterRequest)
	require.NoError(t, err)
	_, err = f.store.BeginSpectator(laterActor, 90, laterRequest, f.formation, f.add)
	require.NoError(t, err)
	var found []uint64
	var cursor uint64
	for round := 0; round < 3; round++ {
		rows, err := f.store.SpectatorsThrough(f.actor, cursor, through)
		require.NoError(t, err)
		require.LessOrEqual(t, len(rows), 8)
		for _, row := range rows {
			found = append(found, row.PlayerId)
			cursor = row.PlayerId
			require.Equal(t, uint32(4), row.State)
		}
		if len(rows) == 0 {
			break
		}
	}
	require.Equal(t, active, found)
}

func TestSpectatorTiDBBusinessExitCaptureClosesUnpreparedAndRollsBackWithOriginalIntent(t *testing.T) {
	f := nativeSpectator(t)
	unprepared := f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, unprepared)
	require.NoError(t, err)
	tx, err := f.store.DB.BeginTx(f.actor, nil)
	require.NoError(t, err)
	require.NoError(t, f.store.lockActor(f.actor, tx, 11))
	exits, err := f.store.CaptureSpectatorExitsTx(f.actor, tx, []*pb.MatchAuthorityTicketRecord{originalPlayer(f.actor, 11)})
	require.NoError(t, err)
	require.Empty(t, exits)
	require.NoError(t, tx.Commit())
	intent, err := f.store.SpectatorIntent(f.actor, 11, unprepared)
	require.NoError(t, err)
	require.Equal(t, uint32(5), intent.State)
	require.Zero(t, intent.CloseTip, "业务退出关闭不记原因,重放回通用的「原观战请求已关闭」")
	_, err = f.store.BeginSpectator(f.actor, 11, unprepared, f.formation, f.add)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	require.ErrorIs(t, err, ErrSpectatorIntentClosed)
	request := f.request()
	original := f.begin(t, request)
	tx, err = f.store.DB.BeginTx(f.actor, nil)
	require.NoError(t, err)
	require.NoError(t, f.store.lockActor(f.actor, tx, 11))
	exits, err = f.store.CaptureSpectatorExitsTx(f.actor, tx, []*pb.MatchAuthorityTicketRecord{originalPlayer(f.actor, 11)})
	require.NoError(t, err)
	require.Len(t, exits, 1)
	require.Equal(t, uint32(3), exits[0].State)
	require.NoError(t, tx.Rollback(), "业务原意图拒绝时，不能提前提交观战清退")
	current, err := f.store.SpectatorForActor(f.actor, 11, request.RequestId)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, current))
	closed, err := f.store.SpectatorExitClosed(f.actor, original)
	require.NoError(t, err)
	require.False(t, closed)
}

// SpectatorIntent 事务内的票据占用拒绝按票状态细分，仍是 ErrMatchEntryOccupied；整个事务回滚、不占请求头，重放重新判定。
func TestSpectatorTiDBTypedTicketOccupancyRejectsWithoutHead(t *testing.T) {
	f := nativeSpectator(t)
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	profile := &pb.MatchNetworkProfile{PlanId: "spectator-plan", SessionBinding: "bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 11, PhysicalRegion: 1, ExpiresAtMs: stamp + 120000}, RttMs: 20, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}}}
	require.NoError(t, f.store.SavePlan(f.actor, 11, profile, stamp+120000))
	_, created, err := f.store.Join(f.actor, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2}, profile.PlanId, profile.SessionBinding)
	require.NoError(t, err)
	require.True(t, created)
	_, err = f.store.SpectatorIntent(f.actor, 11, f.request())
	require.ErrorIs(t, err, ErrTicketQueued)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	require.NotErrorIs(t, err, ErrTicketInBattle)
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE match_authority_ticket SET state=3 WHERE player_id=11")
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(f.actor, 11, f.request())
	require.ErrorIs(t, err, ErrTicketInBattle, "已建房须与排队区分，客户端回「战斗中」")
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
	var heads int
	require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT COUNT(*) FROM match_spectator_head WHERE player_id=11").Scan(&heads))
	require.Zero(t, heads, "事务内拒绝整体回滚，不占请求头")
}

// 成局已变（非已建房或端点已变）可区分且不落原件；未发Add的意图直接关闭并记原因，之后不能再Begin。
func TestSpectatorTiDBFormationClosedIsTypedAndClosesUnprepared(t *testing.T) {
	f := nativeSpectator(t)
	request := f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, request)
	require.NoError(t, err)
	ended := proto.Clone(f.formation).(*pb.MatchFormationRecord)
	ended.State = 5
	_, err = f.store.BeginSpectator(f.actor, 11, request, ended, f.add)
	require.ErrorIs(t, err, ErrSpectatorFormationClosed)
	require.ErrorIs(t, err, ErrSpectatorBusy)
	moved := proto.Clone(f.formation).(*pb.MatchFormationRecord)
	moved.TargetEndpoint = "127.0.0.1:2"
	_, err = f.store.BeginSpectator(f.actor, 11, request, moved, f.add)
	require.ErrorIs(t, err, ErrSpectatorFormationClosed, "端点已变同样按成局已变处理")
	var rows int
	require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT COUNT(*) FROM match_spectator WHERE player_id=11").Scan(&rows))
	require.Zero(t, rows, "成局已变时不落原件，Add不会发出")
	stored, err := f.store.CloseUnpreparedSpectator(f.actor, 11, request, constants.ErrBattleNotWatchable)
	require.NoError(t, err)
	require.Equal(t, constants.ErrBattleNotWatchable, stored)
	_, err = f.store.BeginSpectator(f.actor, 11, request, f.formation, f.add)
	require.ErrorIs(t, err, ErrSpectatorIntentClosed)
}

// close_tip先写为准：首答与同request_id重放读同一字段；拒绝即终态，迟到的同op Add不能翻成准入。
func TestSpectatorTiDBCloseTipStickyOnReplay(t *testing.T) {
	f := nativeSpectator(t)
	background := context.Background()

	// 未准备线：关闭时记原因，再关不改写。
	unprepared := f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, unprepared)
	require.NoError(t, err)
	stored, err := f.store.CloseUnpreparedSpectator(f.actor, 11, unprepared, constants.ErrNoWatchableBattle)
	require.NoError(t, err)
	require.Equal(t, constants.ErrNoWatchableBattle, stored)
	stored, err = f.store.CloseUnpreparedSpectator(f.actor, 11, unprepared, constants.ErrBattleNotWatchable)
	require.NoError(t, err)
	require.Equal(t, constants.ErrNoWatchableBattle, stored, "已关闭的意图不能改写原因")
	intent, err := f.store.SpectatorIntent(f.actor, 11, unprepared)
	require.NoError(t, err)
	require.Equal(t, uint32(5), intent.State)
	require.Equal(t, constants.ErrNoWatchableBattle, intent.CloseTip)

	// 拒绝线：1→3记原因，迟到Add不能推进，3→5关闭证明保留原因。
	rejected := f.request()
	original := f.begin(t, rejected)
	_, err = f.store.RejectSpectator(background, original, 0, "")
	require.Error(t, err, "拒绝必须携带原因")
	ok, err := f.store.RejectSpectator(background, original, constants.ErrSpectateFull, "")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.store.RejectSpectator(f.actor, original, constants.ErrBattleNotWatchable, rejected.RequestId)
	require.NoError(t, err)
	require.True(t, ok, "重复拒绝幂等")
	for _, state := range []uint32{2, 4} {
		ok, err = f.store.AdvanceSpectator(background, original, state)
		require.NoError(t, err)
		require.False(t, ok, "拒绝即终态：迟到的Add回包不能复活原op")
	}
	ok, err = f.store.AdvanceSpectator(background, original, 5)
	require.NoError(t, err)
	require.True(t, ok)
	intent, err = f.store.SpectatorIntent(f.actor, 11, rejected)
	require.NoError(t, err)
	require.Equal(t, uint32(5), intent.State)
	require.Equal(t, constants.ErrSpectateFull, intent.CloseTip, "先写的拒绝原因不被后写覆盖")
	ok, err = f.store.RejectSpectator(background, original, constants.ErrBattleNotWatchable, "")
	require.NoError(t, err)
	require.False(t, ok, "已证实关闭的原op不再记原因")
	intent, err = f.store.SpectatorIntent(f.actor, 11, rejected)
	require.NoError(t, err)
	require.Equal(t, constants.ErrSpectateFull, intent.CloseTip)

	// 被新请求头关闭的旧请求记「已在观战中」，与旧handler续做时的首答同码。
	older, newer := f.request(), f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, older)
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(f.actor, 11, newer)
	require.NoError(t, err)
	intent, err = f.store.SpectatorIntent(f.actor, 11, older)
	require.NoError(t, err)
	require.Equal(t, uint32(5), intent.State)
	require.Equal(t, constants.ErrAlreadyWatching, intent.CloseTip)

	// 尚未发出Add的意图带原因即损坏，读路径fail-closed。
	high, low, err := joinRequestParts(newer.RequestId)
	require.NoError(t, err)
	_, err = f.store.DB.ExecContext(f.actor, "UPDATE match_spectator_intent SET close_tip=? WHERE player_id=11 AND request_high=? AND request_low=?", constants.ErrSpectateFull, high, low)
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(f.actor, 11, newer)
	require.ErrorIs(t, err, ErrOriginalPlayer)
}

// 被更新请求替代的旧请求：旧handler续做时的首答（ErrSpectatorSuperseded →「已在观战中」）与同request_id重放
// 读到的关闭原因同码。未准备线替代即关闭并记原因；已绑定原件线替代时先记原因，新请求清退旧op关闭到5后保留；
// 已有拒绝原因的旧op被替代时原因不改写（先写为准）。
func TestSpectatorTiDBSupersededRequestFirstAnswerMatchesReplay(t *testing.T) {
	f := nativeSpectator(t)
	background := context.Background()

	// 未准备线：A尚未发Add，B占定请求头。
	a, b := f.request(), f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, a)
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(f.actor, 11, b)
	require.NoError(t, err)
	_, err = f.store.SpectatorForActor(f.actor, 11, a.RequestId)
	require.ErrorIs(t, err, ErrSpectatorSuperseded, "A的handler续做：首答回「已在观战中」")
	_, err = f.store.CloseUnpreparedSpectator(f.actor, 11, a, constants.ErrNoWatchableBattle)
	require.ErrorIs(t, err, ErrSpectatorSuperseded, "被替代后A的handler不能改写原因")
	replay, err := f.store.SpectatorIntent(f.actor, 11, a)
	require.NoError(t, err)
	require.Equal(t, uint32(5), replay.State)
	require.Equal(t, constants.ErrAlreadyWatching, replay.CloseTip)

	// 已绑定原件线：B已发Add并准入，C占定请求头。
	original := f.begin(t, b)
	ok, err := f.store.AdvanceSpectatorForHead(f.actor, original, 2, b.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	c := f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, c)
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(f.actor, 11, b)
	require.ErrorIs(t, err, ErrSpectatorSuperseded, "旧op关闭前重放：已在观战中")
	ok, err = f.store.AdvanceSpectatorForHead(f.actor, original, 3, c.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.store.AdvanceSpectatorForHead(f.actor, original, 5, c.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	replay, err = f.store.SpectatorIntent(f.actor, 11, b)
	require.NoError(t, err)
	require.Equal(t, uint32(5), replay.State)
	require.Equal(t, constants.ErrAlreadyWatching, replay.CloseTip, "新请求清退旧op关闭后重放仍回同一原因")

	// 先写为准：C已被battle拒绝并记原因，D替代C时不改写。
	rejected := f.begin(t, c)
	ok, err = f.store.RejectSpectator(f.actor, rejected, constants.ErrSpectateFull, c.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.store.SpectatorIntent(f.actor, 11, f.request())
	require.NoError(t, err)
	ok, err = f.store.AdvanceSpectator(background, rejected, 5)
	require.NoError(t, err)
	require.True(t, ok)
	replay, err = f.store.SpectatorIntent(f.actor, 11, c)
	require.NoError(t, err)
	require.Equal(t, uint32(5), replay.State)
	require.Equal(t, constants.ErrSpectateFull, replay.CloseTip)
}

// requireLobbyIntent 断言意图以 LOBBY 结案(state=5、close_tip=0、delivery=LOBBY),原件固定了目标场。
func requireLobbyIntent(t *testing.T, intent *pb.MatchSpectatorIntentRecord, battle uint64) {
	t.Helper()
	require.Equal(t, uint32(5), intent.State)
	require.Zero(t, intent.CloseTip)
	require.Equal(t, uint32(pb.ESpectateDelivery_SPECTATE_DELIVERY_LOBBY), intent.Delivery)
	pinned := &pb.MatchSpectatorRecord{}
	require.NoError(t, proto.Unmarshal(intent.Original, pinned))
	require.Equal(t, battle, pinned.BattleId)
}

// L1 / L2 观众以 LOBBY 结案:不写 match_spectator,目标场固定在意图上(随机模式重放不换场),结论先写为准。
func TestSpectatorTiDBLobbyCloseWritesNoOccupancyAndPinsBattle(t *testing.T) {
	f := nativeSpectator(t)
	battle := f.formation.BattleId
	random := f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, random)
	require.NoError(t, err)
	intent, err := f.store.CloseLobbySpectator(f.actor, 11, random, battle)
	require.NoError(t, err)
	requireLobbyIntent(t, intent, battle)

	var rows int
	require.NoError(t, f.store.DB.QueryRowContext(f.actor, "SELECT COUNT(*) FROM match_spectator WHERE player_id=11").Scan(&rows))
	require.Zero(t, rows, "L1 / L2 观众不占 match_spectator 行")

	replay, err := f.store.SpectatorIntent(f.actor, 11, random)
	require.NoError(t, err)
	requireLobbyIntent(t, replay, battle)
	again, err := f.store.CloseLobbySpectator(f.actor, 11, random, battle+1)
	require.NoError(t, err)
	requireLobbyIntent(t, again, battle)
	_, err = f.store.BeginSpectator(f.actor, 11, random, f.formation, f.add)
	require.ErrorIs(t, err, ErrSpectatorIntentClosed, "已结案的意图不能再走L0")
	stored, err := f.store.CloseUnpreparedSpectator(f.actor, 11, random, constants.ErrBattleNotWatchable)
	require.NoError(t, err)
	require.Zero(t, stored, "已结案的意图不能改写成关闭原因")
	require.NoError(t, f.store.RequireLobbyReplay(f.actor, 11, random.RequestId))

	// 指定场的请求只能固定到它指定的那一场。
	specified := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battle}
	_, err = f.store.SpectatorIntent(f.actor, 11, specified)
	require.NoError(t, err)
	_, err = f.store.CloseLobbySpectator(f.actor, 11, specified, battle+1)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	_, err = f.store.CloseLobbySpectator(f.actor, 11, specified, 0)
	require.ErrorIs(t, err, ErrOriginalPlayer)

	// 新请求占定请求头后,旧LOBBY重放复核回「已被替代」,旧handler也不能再结案。
	require.ErrorIs(t, f.store.RequireLobbyReplay(f.actor, 11, random.RequestId), ErrSpectatorSuperseded)
	latest := f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, latest)
	require.NoError(t, err)
	_, err = f.store.CloseLobbySpectator(f.actor, 11, specified, battle)
	require.ErrorIs(t, err, ErrSpectatorSuperseded)
}

// LOBBY重放先复核互斥(Q1软互斥的只读检查):玩家排队后拿不到新凭证,结论与首次进入相同。
func TestSpectatorTiDBLobbyReplayRejectsQueuedPlayer(t *testing.T) {
	f := nativeSpectator(t)
	request := f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, request)
	require.NoError(t, err)
	_, err = f.store.CloseLobbySpectator(f.actor, 11, request, f.formation.BattleId)
	require.NoError(t, err)
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	profile := &pb.MatchNetworkProfile{PlanId: "spectator-plan", SessionBinding: "bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 11, PhysicalRegion: 1, ExpiresAtMs: stamp + 120000}, RttMs: 20, SampleCount: 3, MeasuredAtMs: stamp, ExpiresAtMs: stamp + 120000}}}
	require.NoError(t, f.store.SavePlan(f.actor, 11, profile, stamp+120000))
	_, created, err := f.store.Join(f.actor, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2}, profile.PlanId, profile.SessionBinding)
	require.NoError(t, err)
	require.True(t, created, "LOBBY观众没有占用,不挡排队")

	err = f.store.RequireLobbyReplay(f.actor, 11, request.RequestId)
	require.ErrorIs(t, err, ErrTicketQueued)
	require.ErrorIs(t, err, ErrMatchEntryOccupied)
}

// 直连席已满转L1:原件照常取消拿关闭证明,意图上记LOBBY、不写16021;迟到的Add不能翻回准入,关闭到5后重放走LOBBY。
func TestSpectatorTiDBFullRejectToLobbySurvivesCancel(t *testing.T) {
	f := nativeSpectator(t)
	background := context.Background()
	request := f.request()
	original := f.begin(t, request)
	ok, err := f.store.RejectSpectatorToLobby(f.actor, original, request.RequestId)
	require.NoError(t, err)
	require.True(t, ok)
	for _, state := range []uint32{2, 4} {
		ok, err = f.store.AdvanceSpectator(background, original, state)
		require.NoError(t, err)
		require.False(t, ok, "拒绝即终态：迟到的Add回包不能复活原op")
	}
	ok, err = f.store.AdvanceSpectator(background, original, 5)
	require.NoError(t, err)
	require.True(t, ok)
	intent, err := f.store.SpectatorIntent(f.actor, 11, request)
	require.NoError(t, err)
	requireLobbyIntent(t, intent, f.formation.BattleId)
	require.NoError(t, f.store.RequireLobbyReplay(f.actor, 11, request.RequestId))
	ok, err = f.store.RejectSpectatorToLobby(background, original, "")
	require.NoError(t, err)
	require.False(t, ok, "已证实关闭的原op不再记结论")
}

// 关闭原因与LOBBY先写为准,互不覆盖。
func TestSpectatorTiDBLobbyAndCloseTipFirstWriteWins(t *testing.T) {
	f := nativeSpectator(t)
	background := context.Background()

	// 先记拒绝原因:后到的满员转L1不改写。
	rejected := f.request()
	original := f.begin(t, rejected)
	ok, err := f.store.RejectSpectator(background, original, constants.ErrBattleNotWatchable, "")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.store.RejectSpectatorToLobby(background, original, "")
	require.NoError(t, err)
	require.True(t, ok, "同为待取消,重复推进幂等")
	ok, err = f.store.AdvanceSpectator(background, original, 5)
	require.NoError(t, err)
	require.True(t, ok)
	intent, err := f.store.SpectatorIntent(f.actor, 11, rejected)
	require.NoError(t, err)
	require.Equal(t, constants.ErrBattleNotWatchable, intent.CloseTip)
	require.Zero(t, intent.Delivery)

	// 先记LOBBY:新请求替代时不写「已在观战中」;旧request_id重放由请求头复核回同一结论。
	lobby := f.request()
	original = f.begin(t, lobby)
	ok, err = f.store.RejectSpectatorToLobby(background, original, "")
	require.NoError(t, err)
	require.True(t, ok)
	newer := f.request()
	_, err = f.store.SpectatorIntent(f.actor, 11, newer)
	require.NoError(t, err)
	ok, err = f.store.AdvanceSpectator(background, original, 5)
	require.NoError(t, err)
	require.True(t, ok)
	intent, err = f.store.SpectatorIntent(f.actor, 11, lobby)
	require.NoError(t, err)
	requireLobbyIntent(t, intent, f.formation.BattleId)
	require.ErrorIs(t, f.store.RequireLobbyReplay(f.actor, 11, lobby.RequestId), ErrSpectatorSuperseded)
}

// 房间必已不在的判据:成局已关,或已过权威截止 + 终局提交宽限(与到期终局恢复裁决 ABORT 同一分界)。
func TestSpectatorFormationEnded(t *testing.T) {
	const deadline = uint64(1_000_000)
	for _, c := range []struct {
		name  string
		state uint32
		limit uint64
		stamp uint64
		want  bool
	}{
		{"战斗中", 4, deadline, deadline - 1, false},
		{"刚到期限", 4, deadline, deadline, false},
		{"还差1毫秒过宽限", 4, deadline, deadline + terminalOutcomeGraceMs - 1, false},
		{"过了宽限", 4, deadline, deadline + terminalOutcomeGraceMs, true},
		{"成局已关", 5, deadline, 0, true},
		{"没有期限", 4, 0, deadline, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, SpectatorFormationEnded(&pb.MatchFormationRecord{State: c.state, DeadlineMs: c.limit}, c.stamp))
		})
	}
}

// intentRetentionFixture 把观战意图摆成指定的创建时刻,读删除结果(都走 SQL:created_ms 不进读路径)。
type intentRetentionFixture struct {
	*nativeSpectatorFixture
	t *testing.T
}

func (f intentRetentionFixture) setCreated(player uint64, request *pb.WatchBattleRequest, createdMs uint64) {
	f.t.Helper()
	high, low, err := joinRequestParts(request.RequestId)
	require.NoError(f.t, err)
	result, err := f.store.DB.ExecContext(f.actor, "UPDATE match_spectator_intent SET created_ms=? WHERE player_id=? AND request_high=? AND request_low=?", createdMs, player, high, low)
	require.NoError(f.t, err)
	affected, err := result.RowsAffected()
	require.NoError(f.t, err)
	require.EqualValues(f.t, 1, affected)
}

func (f intentRetentionFixture) exists(player uint64, request *pb.WatchBattleRequest) bool {
	f.t.Helper()
	high, low, err := joinRequestParts(request.RequestId)
	require.NoError(f.t, err)
	var rows int
	require.NoError(f.t, f.store.DB.QueryRowContext(f.actor, "SELECT COUNT(*) FROM match_spectator_intent WHERE player_id=? AND request_high=? AND request_low=?", player, high, low).Scan(&rows))
	return rows == 1
}

func (f intentRetentionFixture) newObserver(player uint64) context.Context {
	f.t.Helper()
	account := fmt.Sprintf("observer-%d", player)
	_, err := f.store.DB.ExecContext(f.actor, "INSERT INTO account_role VALUES(?,?,7,0)", player, account)
	require.NoError(f.t, err)
	return nativeRoleActor(f.t, f.actor, player, account, 7)
}

// 保留期清理只删 state=5、不是请求头、超过保留期的行,幂等。删掉之后旧 request_id 重放按新请求处理;请求头移走后,原来的请求头下一轮也能清掉。
func TestSpectatorTiDBIntentRetentionPurgesOnlyOldClosedNonHead(t *testing.T) {
	f := intentRetentionFixture{nativeSpectator(t), t}
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	old := stamp - spectatorIntentRetentionMs - 1000

	oldClosed, recentClosed, oldHead := f.request(), f.request(), f.request()
	for _, request := range []*pb.WatchBattleRequest{oldClosed, recentClosed, oldHead} {
		_, err = f.store.SpectatorIntent(f.actor, 11, request)
		require.NoError(t, err)
	}
	_, err = f.store.CloseUnpreparedSpectator(f.actor, 11, oldHead, constants.ErrNoWatchableBattle)
	require.NoError(t, err)
	f.setCreated(11, oldClosed, old)
	f.setCreated(11, oldHead, old)

	// 已绑定原件、尚未关闭的旧意图不能删(后台恢复还要沿它收尾)。
	observer := f.newObserver(30)
	bound, successor := f.request(), f.request()
	_, err = f.store.SpectatorIntent(observer, 30, bound)
	require.NoError(t, err)
	_, err = f.store.BeginSpectator(observer, 30, bound, f.formation, f.add)
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(observer, 30, successor)
	require.NoError(t, err)
	f.setCreated(30, bound, old)

	next, deleted, err := f.store.PurgeClosedSpectatorIntents(f.actor, SpectatorIntentCursor{}, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	require.Equal(t, SpectatorIntentCursor{}, next, "不满一页:下一轮从头扫")
	require.False(t, f.exists(11, oldClosed))
	require.True(t, f.exists(11, recentClosed), "还在保留期内")
	require.True(t, f.exists(11, oldHead), "请求头不能删:读路径按请求头读意图")
	require.True(t, f.exists(30, bound), "还没关闭")

	_, deleted, err = f.store.PurgeClosedSpectatorIntents(f.actor, SpectatorIntentCursor{}, 100)
	require.NoError(t, err)
	require.Zero(t, deleted, "幂等")

	// 极旧 request_id 的重放被当成新请求:占定请求头,结论是一次普通的新观战。
	replay, err := f.store.SpectatorIntent(f.actor, 11, oldClosed)
	require.NoError(t, err)
	require.Equal(t, uint32(1), replay.State)
	require.True(t, f.exists(11, oldClosed))

	_, deleted, err = f.store.PurgeClosedSpectatorIntents(f.actor, SpectatorIntentCursor{}, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "请求头移走后,原来的请求头也能清")
	require.False(t, f.exists(11, oldHead))
	require.True(t, f.exists(11, oldClosed), "重放新插入的行 created_ms 是新的,不在保留期外")
}

// 请求头的行一直留在索引前端(每名看过观战的玩家一行),翻页才不会被它们饿死:后面的行总能轮到。
func TestSpectatorTiDBIntentRetentionPagesPastHeadRows(t *testing.T) {
	f := intentRetentionFixture{nativeSpectator(t), t}
	stamp, err := f.store.Now(f.actor)
	require.NoError(t, err)
	old := stamp - spectatorIntentRetentionMs - 10000

	// 两名玩家各有一行已关闭的旧请求头,排在索引最前面。
	for offset, player := range []uint64{40, 41} {
		observer := f.newObserver(player)
		head := f.request()
		_, err = f.store.SpectatorIntent(observer, player, head)
		require.NoError(t, err)
		_, err = f.store.CloseUnpreparedSpectator(observer, player, head, constants.ErrNoWatchableBattle)
		require.NoError(t, err)
		f.setCreated(player, head, old+uint64(offset))
	}
	// 第三名玩家有一行已关闭的旧意图,排在它们后面。
	observer := f.newObserver(42)
	closed, current := f.request(), f.request()
	_, err = f.store.SpectatorIntent(observer, 42, closed)
	require.NoError(t, err)
	_, err = f.store.SpectatorIntent(observer, 42, current)
	require.NoError(t, err)
	f.setCreated(42, closed, old+100)

	cursor := SpectatorIntentCursor{}
	var total int64
	for round := 0; round < 4; round++ {
		var deleted int64
		cursor, deleted, err = f.store.PurgeClosedSpectatorIntents(f.actor, cursor, 1)
		require.NoError(t, err)
		total += deleted
	}
	require.EqualValues(t, 1, total)
	require.False(t, f.exists(42, closed))
	require.Equal(t, SpectatorIntentCursor{}, cursor, "扫到末尾回到开头")
}

// delivery 的非法组合即损坏,读路径fail-closed。
func TestSpectatorTiDBCorruptDeliveryIsRejected(t *testing.T) {
	f := nativeSpectator(t)
	request := f.request()
	_, err := f.store.SpectatorIntent(f.actor, 11, request)
	require.NoError(t, err)
	high, low, err := joinRequestParts(request.RequestId)
	require.NoError(t, err)
	corrupt := func(set string, args ...any) {
		t.Helper()
		args = append(args, high, low)
		_, err := f.store.DB.ExecContext(f.actor, "UPDATE match_spectator_intent SET "+set+" WHERE player_id=11 AND request_high=? AND request_low=?", args...)
		require.NoError(t, err)
		_, err = f.store.SpectatorIntent(f.actor, 11, request)
		require.ErrorIs(t, err, ErrOriginalPlayer, set)
	}
	corrupt("delivery=?", spectatorDeliveryLobby)                                                      // 尚未发Add的意图不能带LOBBY
	corrupt("state=5,delivery=?", spectatorDeliveryLobby)                                              // LOBBY结案必须固定目标场
	corrupt("state=5,delivery=?,original=?", 9, []byte{})                                              // 不认识的取值
	corrupt("state=5,delivery=?,close_tip=?", spectatorDeliveryLobby, constants.ErrBattleNotWatchable) // LOBBY与关闭原因二选一
}

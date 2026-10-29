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

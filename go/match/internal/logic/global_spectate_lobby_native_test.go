package logic

// 全局路径的 L1 / L2 观众(battle-spectate-tiered-fanout.md §15.6):意图以 LOBBY 结案、不写 match_spectator、重放签新凭证。
// 真实 TiDB(MATCH_IT_TIDB,与 authority / team 的原生夹具同一组环境变量)+ 回环 gRPC 桩 battle + miniredis。
// 没配 TiDB 时跳过;MATCH_IT_REQUIRE_TIDB=1 时缺环境直接失败,不许漏跑。

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"match/internal/authority"
	"match/internal/config"
	"match/internal/constants"
	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	dspb "proto/data_service"
	guildpb "proto/guild"
	pb "proto/match"
	"schemamigrate"
	"shared/authoritysql/sqltest"
	"shared/generated/pb/table"
	"shared/roleidentity"
	"shared/serviceauth"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const lobbyFixtureDeadlineMs = testRelayDeadlineMs

// fakeBattleNode 是回环 gRPC 上的桩 battle 节点:全局路径的 L0 直接用生成的客户端发 RPC(不走 addObserverFn),
// 所以这里连 L1 / L2 的 RPC 也走真实 gRPC,一并覆盖 attachRelayGateRPC / promoteSpectateBroadcastRPC。
type fakeBattleNode struct {
	battlepb.UnimplementedBattleNodeServer
	mu       sync.Mutex
	instance string
	full     bool
	// roomMissing:Add 回房间不存在。
	roomMissing bool
	// instanceGone:目标实例已被替换。真实 battle 对实例不符的 Remove / Query 回空包,这样的回包不是取消证明。
	instanceGone bool
	// tombstones 是 op → 首次拒绝码:真实 battle 的观战账本给被拒的 op 留墓碑,同 op 重放回首次拒绝码,不重新判定。
	tombstones map[string]uint32
	adds       int
	removes    int
	attaches   []*battlepb.AttachRelayGateRequest
	promotes   int
}

func (b *fakeBattleNode) AddObserver(_ context.Context, req *battlepb.AddObserverRequest) (*battlepb.AddObserverResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adds++
	op := req.GetSpectatorOperationId()
	tip, replayed := b.tombstones[op]
	if !replayed {
		switch {
		case b.roomMissing:
			tip = uint32(table.CommonError_kEntityIsNull)
		case b.full:
			tip = uint32(table.MatchError_kMatchSpectateFull)
		default:
			return &battlepb.AddObserverResponse{}, nil
		}
		b.tombstones[op] = tip
	}
	return &battlepb.AddObserverResponse{ErrorMessage: tipErr(tip, "观战被拒绝")}, nil
}

func (b *fakeBattleNode) RemoveObserver(_ context.Context, req *battlepb.RemoveObserverRequest) (*battlepb.RemoveObserverResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removes++
	if b.instanceGone {
		return &battlepb.RemoveObserverResponse{}, nil
	}
	return &battlepb.RemoveObserverResponse{Cancelled: true, SpectatorOperationId: req.GetSpectatorOperationId(), TargetInstanceId: req.GetTargetInstanceId()}, nil
}

func (b *fakeBattleNode) QueryObserver(_ context.Context, req *battlepb.QueryObserverRequest) (*battlepb.QueryObserverResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.instanceGone {
		return &battlepb.QueryObserverResponse{}, nil
	}
	return &battlepb.QueryObserverResponse{State: battlepb.ObserverAdmissionState_OBSERVER_ADMISSION_ACTIVE, SpectatorOperationId: req.GetSpectatorOperationId(), TargetInstanceId: req.GetTargetInstanceId()}, nil
}

func (b *fakeBattleNode) AttachRelayGate(_ context.Context, req *battlepb.AttachRelayGateRequest) (*battlepb.AttachRelayGateResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attaches = append(b.attaches, req)
	return &battlepb.AttachRelayGateResponse{BattleInstanceId: b.instance, DeadlineMs: lobbyFixtureDeadlineMs}, nil
}

func (b *fakeBattleNode) PromoteSpectateBroadcast(_ context.Context, _ *battlepb.PromoteSpectateBroadcastRequest) (*battlepb.PromoteSpectateBroadcastResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.promotes++
	return &battlepb.PromoteSpectateBroadcastResponse{BattleInstanceId: b.instance, DeadlineMs: lobbyFixtureDeadlineMs}, nil
}

func (b *fakeBattleNode) counts() (adds, removes, attaches, promotes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.adds, b.removes, len(b.attaches), b.promotes
}

func (b *fakeBattleNode) setFull(full bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.full = full
}

func (b *fakeBattleNode) setRoomMissing(missing bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roomMissing = missing
}

func (b *fakeBattleNode) setInstanceGone(gone bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.instanceGone = gone
}

// tombstone 预置一个 op 的墓碑:上一轮 Add 已被 battle 拒绝,而 match 这边的结论没有落库。
func (b *fakeBattleNode) tombstone(op string, tip uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tombstones[op] = tip
}

func (b *fakeBattleNode) attachedGate(index int) uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attaches[index].GetGateNodeId()
}

type globalLobbyFixture struct {
	svcCtx *svc.ServiceContext
	mr     *miniredis.Miniredis
	db     *sql.DB
	battle *fakeBattleNode
	addr   string
}

func newGlobalLobbyFixture(t *testing.T) *globalLobbyFixture {
	t.Helper()
	address := os.Getenv("MATCH_IT_TIDB")
	if address == "" {
		if os.Getenv("MATCH_IT_REQUIRE_TIDB") == "1" {
			t.Fatal("缺MATCH_IT_TIDB，全局观战原生验收不得跳过")
		}
		t.Skip("设置MATCH_IT_TIDB为已核任务所有的数值loopback地址启用隔离TiDB观战回归")
	}
	host, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	require.True(t, ip.IsLoopback())
	connection := mysql.NewConfig()
	connection.Net = "tcp"
	connection.Addr = address
	connection.User = "root"
	connection.Timeout, connection.ReadTimeout, connection.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	connection.Params = map[string]string{"tidb_txn_mode": "'pessimistic'", "transaction_isolation": "'READ-COMMITTED'", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF", "tidb_replica_read": "'leader'", "tidb_read_consistency": "'strict'", "tidb_snapshot": "''"}
	schema := "spectateit_" + uuid.NewString()[:8]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts, err := sqltest.OptionsFromEnv(connection.FormatDSN(), schema+"_writer", schema)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	install, err := fixture.OpenSchema(ctx, schema)
	require.NoError(t, err)
	tables := append(authority.Tables(), &guildpb.GuildTrialLaunchRecord{}, &guildpb.GuildTrialLaunchRoleRecord{}, &guildpb.GuildTrialLobbyRecord{}, &guildpb.GuildTrialLobbySlotRecord{})
	_, err = schemamigrate.Up(ctx, install, schemamigrate.Options{Database: schema, Tables: tables})
	require.NoError(t, err)
	_, err = install.ExecContext(ctx, "CREATE TABLE account_role(player_id BIGINT UNSIGNED PRIMARY KEY,account VARBINARY(191) NOT NULL,owner_revision BIGINT UNSIGNED NOT NULL,trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0)")
	require.NoError(t, err)
	require.NoError(t, fixture.GrantProtoTables(ctx, schema, tables))
	require.NoError(t, fixture.GrantTables(ctx, schema, "account_role"))
	db, err := fixture.Open(ctx, schema, 16)
	require.NoError(t, err)
	store := &authority.Store{DB: db, Config: config.GlobalMatchConf{Backend: "tidb", HardLatencyMs: 180, CrossRegionWaitSeconds: 30}}
	require.NoError(t, store.ConfigureRoleAuthority(ctx, schema))
	require.NoError(t, store.ConfigureMatchEntryAuthority(ctx, schema))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	battle := &fakeBattleNode{instance: uuid.NewString(), tombstones: map[string]uint32{}}
	grpcServer := grpc.NewServer()
	battlepb.RegisterBattleNodeServer(grpcServer, battle)
	go func() { _ = grpcServer.Serve(listener) }()
	addr := listener.Addr().String()
	t.Cleanup(func() {
		discovery.RemoveEndpointConn(addr)
		grpcServer.Stop()
	})

	svcCtx, mr := newTestSvcCtx(t)
	svcCtx.Authority = store
	svcCtx.Config.Spectate = config.SpectateConf{RelayEnabled: true, BroadcastMaxConcurrent: 2, BroadcastAutoGateThreshold: 0, TicketTTLSeconds: 60, TicketSecret: testSpectateTicketSecret}
	return &globalLobbyFixture{svcCtx: svcCtx, mr: mr, db: db, battle: battle, addr: addr}
}

// addFormation 插一场已开房(state 4)的公开局,落点指向回环桩 battle。
func (f *globalLobbyFixture) addFormation(t *testing.T, battleId uint64) {
	t.Helper()
	f.addFormationAt(t, battleId, nowMs(), lobbyFixtureDeadlineMs)
}

func (f *globalLobbyFixture) addFormationAt(t *testing.T, battleId, createdMs, deadlineMs uint64) {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.CreateBattleRequest{
		BattleId:           battleId,
		MatchMode:          uint32(pb.MatchMode_MATCH_MODE_PVE_SOLO),
		DeadlineMs:         deadlineMs,
		SpectateVisibility: visPublic,
	})
	require.NoError(t, err)
	_, err = f.db.Exec("INSERT INTO match_formation(battle_id,operation_id,state,physical_region,target_node_id,target_instance_id,target_endpoint,mode,config_id,members,create_request,created_ms,deadline_ms,retry_at_ms,activity_context,requeue_on_failure,prepared,outcome_protocol_version,preflight_supported_version,preflight_instance_id,adopted_outcome_protocol_version,adopted_outcome_observed) VALUES(?,?,4,1,7,?,?,?,1,?,?,?,?,0,?,0,?,0,0,'',0,0)",
		battleId, uuid.NewString(), f.battle.instance, f.addr, uint32(pb.MatchMode_MATCH_MODE_PVE_SOLO), []byte{}, raw, createdMs, deadlineMs, []byte{}, []byte{})
	require.NoError(t, err)
}

// addCompletableFormation 插一场能走完 CommitBattleOutcome 的成局(带 v1 能力证据、建房原件与两名成员的票),
// 返回已签名的终局请求。成员的角色目录行由 addViewer 建好。
func (f *globalLobbyFixture) addCompletableFormation(t *testing.T, battleId, playerA, playerB uint64, secret string) *pb.CommitBattleOutcomeRequest {
	t.Helper()
	const configId = uint32(1)
	mode := uint32(pb.MatchMode_MATCH_MODE_PVE_SOLO)
	operation := uuid.NewString()
	createdMs := nowMs() - 1000
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.CreateBattleRequest{
		BattleId: battleId, PlacementOperationId: operation, TargetInstanceId: f.battle.instance, BattleConfigId: configId, MatchMode: mode,
		OutcomeProtocolVersion: 1, CreatedAtMs: createdMs, DeadlineMs: lobbyFixtureDeadlineMs, SpectateVisibility: visPublic,
		Players: []*battlepb.BattlePlayerSnapshot{{PlayerId: playerA, TeamIndex: 0}, {PlayerId: playerB, TeamIndex: 1}},
	})
	require.NoError(t, err)
	var members []*pb.MatchFormationMember
	for _, player := range []uint64{playerA, playerB} {
		ticket := uuid.NewString()
		members = append(members, &pb.MatchFormationMember{PlayerId: player, Ticket: ticket})
		_, err = f.db.Exec("INSERT INTO match_authority_ticket(player_id,ticket,request_id,mode,config_id,team_size,state,enqueued_ms,not_before_ms,zone_id,rating_centi,formation_id,team_id,network_profile,expires_ms,selected_region,preferred_region,owner_account,owner_revision) VALUES(?,?,?,?,?,2,2,?,0,1,0,?,0,?,0,0,0,?,7)",
			player, ticket, uuid.NewString(), mode, configId, createdMs, battleId, []byte{}, lobbyFixtureAccount(player))
		require.NoError(t, err)
	}
	memberBlob, err := proto.Marshal(&pb.MatchFormationRecord{Members: members})
	require.NoError(t, err)
	_, err = f.db.Exec("INSERT INTO match_formation(battle_id,operation_id,state,physical_region,target_node_id,target_instance_id,target_endpoint,mode,config_id,members,create_request,created_ms,deadline_ms,retry_at_ms,activity_context,requeue_on_failure,prepared,outcome_protocol_version,preflight_supported_version,preflight_instance_id,adopted_outcome_protocol_version,adopted_outcome_observed) VALUES(?,?,4,1,7,?,?,?,?,?,?,?,?,0,?,0,?,1,1,?,0,0)",
		battleId, operation, f.battle.instance, f.addr, mode, configId, memberBlob, raw, createdMs, lobbyFixtureDeadlineMs, []byte{}, []byte{}, f.battle.instance)
	require.NoError(t, err)

	outcome := battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN
	event, err := proto.Marshal(&kafkapb.BattleResultEvent{
		BattleId: battleId, MatchMode: mode, BattleConfigId: configId, Outcome: outcome, FinishedAtMs: nowMs(), SettlementFormatVersion: 1,
		Teams: []*kafkapb.BattleResultTeam{{TeamIndex: 0, PlayerIds: []uint64{playerA}}, {TeamIndex: 1, PlayerIds: []uint64{playerB}}},
		Settlements: []*battlepb.BattleSettlementData{
			{BattleId: battleId, PlayerId: playerA, Outcome: outcome, PlayerTeamIndex: 0},
			{BattleId: battleId, PlayerId: playerB, Outcome: outcome, PlayerTeamIndex: 1},
		},
	})
	require.NoError(t, err)
	request := &pb.CommitBattleOutcomeRequest{BattleId: battleId, PlacementOperationId: operation, TargetInstanceId: f.battle.instance, RawResultPayload: event}
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_CommitBattleOutcome_FullMethodName, request, "battle", secret, uint64(time.Now().UnixMilli())))
	return request
}

// addViewer 建角色目录行并把会话挂到 gate 3。
func (f *globalLobbyFixture) addViewer(t *testing.T, player uint64) {
	t.Helper()
	_, err := f.db.Exec("INSERT INTO account_role VALUES(?,?,7,0)", player, []byte(lobbyFixtureAccount(player)))
	require.NoError(t, err)
	setPlayerSessionOnGate(t, f.mr, player, "3", "gate-inst-3")
}

func lobbyFixtureAccount(player uint64) string {
	return fmt.Sprintf("lobby-original-%d", player)
}

// actor 组 gate 路由过来的请求 ctx:带会话详情(路由取当前 player:session)与原角色身份。
func (f *globalLobbyFixture) actor(t *testing.T, player uint64) context.Context {
	t.Helper()
	session, err := loadPlayerSession(f.svcCtx, player)
	require.NoError(t, err)
	gate, err := strconv.ParseUint(session.GateId, 10, 32)
	require.NoError(t, err)
	ctx := ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: session.SessionId, PlayerId: player, GateNodeId: uint32(gate), GateInstanceId: session.GateInstanceId, OwnerRevision: 7})
	actor, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: lobbyFixtureAccount(player), OwnerRevision: 7, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return actor
}

// watch 按 gate 路由过来的样子发 WatchBattle。
func (f *globalLobbyFixture) watch(t *testing.T, player uint64, request *pb.WatchBattleRequest) *pb.WatchBattleResponse {
	t.Helper()
	resp, err := NewWatchBattleLogic(f.actor(t, player), f.svcCtx).WatchBattle(request)
	require.NoError(t, err)
	return resp
}

// requireIntentRow 直接读意图行:结论必须持久化,不靠重放算出来。
func (f *globalLobbyFixture) requireIntentRow(t *testing.T, player uint64, request *pb.WatchBattleRequest, state, closeTip, delivery uint32) {
	t.Helper()
	id, err := uuid.Parse(request.RequestId)
	require.NoError(t, err)
	var gotState, gotTip, gotDelivery uint32
	require.NoError(t, f.db.QueryRow("SELECT state,close_tip,delivery FROM match_spectator_intent WHERE player_id=? AND request_high=? AND request_low=?",
		player, beUint64(id[:8]), beUint64(id[8:])).Scan(&gotState, &gotTip, &gotDelivery))
	require.Equal(t, state, gotState)
	require.Equal(t, closeTip, gotTip)
	require.Equal(t, delivery, gotDelivery)
}

func beUint64(raw []byte) uint64 {
	var value uint64
	for _, b := range raw {
		value = value<<8 | uint64(b)
	}
	return value
}

func (f *globalLobbyFixture) spectatorRows(t *testing.T, player uint64) int {
	t.Helper()
	var rows int
	require.NoError(t, f.db.QueryRow("SELECT COUNT(*) FROM match_spectator WHERE player_id=?", player).Scan(&rows))
	return rows
}

func requireGlobalLobbyTicket(t *testing.T, resp *pb.WatchBattleResponse, battleId, player uint64, instance string) {
	t.Helper()
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, battleId, resp.GetBattleId())
	require.Equal(t, deliveryLobby, resp.GetDelivery())
	require.Empty(t, resp.GetSpectatorOperationId(), "L1 / L2 观众在 battle 没有观战原操作")
	payload := verifiedTicketPayload(t, resp.GetSpectateTicket(), testSpectateTicketSecret)
	require.Equal(t, battleId, payload.GetBattleId())
	require.Equal(t, player, payload.GetPlayerId())
	require.Equal(t, instance, payload.GetBattleInstanceId())
	require.Equal(t, lobbyFixtureDeadlineMs, payload.GetDeadlineMs())
	require.Greater(t, payload.GetExpireAtMs(), nowMs())
}

// 直连席已满:L0 原件照常拿到取消证明,这次请求以 LOBBY 结案(不写 16021);同 request_id 重放重新准入并签新凭证,
// 观众换了 gate(大厅重连)时挂到新 gate。
func TestGlobalWatchTiDBFullSeatClosesLobbyAndReplaySignsFreshTicket(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	f.battle.setFull(true)
	const player, battleId = uint64(61), uint64(910001)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}

	first := f.watch(t, player, request)
	requireGlobalLobbyTicket(t, first, battleId, player, f.battle.instance)
	adds, removes, attaches, _ := f.battle.counts()
	require.Equal(t, 1, adds, "这次先撞了一次 L0")
	require.Equal(t, 1, removes, "L0 原件必须拿到关闭证明")
	require.Equal(t, 1, attaches)
	f.requireIntentRow(t, player, request, 5, 0, uint32(deliveryLobby))
	var state uint32
	require.NoError(t, f.db.QueryRow("SELECT state FROM match_spectator WHERE player_id=?", player).Scan(&state))
	require.Equal(t, uint32(5), state, "L0 原件已证实关闭,不占用")
	require.Equal(t, tierRelay, tierOf(t, f.svcCtx, battleId))

	replay := f.watch(t, player, request)
	requireGlobalLobbyTicket(t, replay, battleId, player, f.battle.instance)
	adds, removes, attaches, _ = f.battle.counts()
	require.Equal(t, 1, adds, "重放不再撞 L0")
	require.Equal(t, 1, removes)
	require.Equal(t, 1, attaches, "同 gate 不再 Attach")

	setPlayerSessionOnGate(t, f.mr, player, "4", "gate-inst-4")
	moved := f.watch(t, player, request)
	requireGlobalLobbyTicket(t, moved, battleId, player, f.battle.instance)
	_, _, attaches, _ = f.battle.counts()
	require.Equal(t, 2, attaches, "大厅重连到新 gate:重放按当前会话重新挂接")
	require.Equal(t, uint32(4), f.battle.attachedGate(1))
}

// 已升档的场:不撞 L0、不写 match_spectator,意图直接以 LOBBY 结案。随机模式重放不换场(目标场固定在意图上)。
func TestGlobalWatchTiDBRelayTierClosesLobbyWithoutOccupancy(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(62), uint64(910002)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	raiseTier(t, f.svcCtx, battleId, tierRelay)
	_, err := f.mr.ZAdd(spectateBattlesActiveKey, float64(nowMs()), strconv.FormatUint(battleId, 10))
	require.NoError(t, err)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString()} // 随机观战

	first := f.watch(t, player, request)
	requireGlobalLobbyTicket(t, first, battleId, player, f.battle.instance)
	adds, removes, attaches, _ := f.battle.counts()
	require.Zero(t, adds, "已升档的场不撞 L0")
	require.Zero(t, removes)
	require.Equal(t, 1, attaches)
	require.Zero(t, f.spectatorRows(t, player), "L1 观众不写 match_spectator")
	f.requireIntentRow(t, player, request, 5, 0, uint32(deliveryLobby))

	// 活跃集合换成另一场:重放仍是原来那一场。
	const other = uint64(910003)
	f.addFormation(t, other)
	removeSpectateActiveMember(f.svcCtx, battleId)
	_, err = f.mr.ZAdd(spectateBattlesActiveKey, float64(nowMs()), strconv.FormatUint(other, 10))
	require.NoError(t, err)
	replay := f.watch(t, player, request)
	requireGlobalLobbyTicket(t, replay, battleId, player, f.battle.instance)
	require.Zero(t, f.spectatorRows(t, player))
}

// BROADCAST 档的观众不调任何 battle RPC:实例与期限取自成局原件。
func TestGlobalWatchTiDBBroadcastTierCallsNoBattleRPC(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(63), uint64(910004)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	raiseTier(t, f.svcCtx, battleId, tierBroadcast)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}

	requireGlobalLobbyTicket(t, f.watch(t, player, request), battleId, player, f.battle.instance)
	adds, removes, attaches, promotes := f.battle.counts()
	require.Zero(t, adds+removes+attaches+promotes)
	require.Zero(t, f.spectatorRows(t, player))
}

// 被新请求替代的 LOBBY 意图:旧 request_id 重放拿不到新凭证,回「已在观战中」(与被替代的其它请求同结论)。
func TestGlobalWatchTiDBSupersededLobbyReplayGetsNoTicket(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(64), uint64(910005)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	raiseTier(t, f.svcCtx, battleId, tierRelay)
	older := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	newer := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	requireGlobalLobbyTicket(t, f.watch(t, player, older), battleId, player, f.battle.instance)
	requireGlobalLobbyTicket(t, f.watch(t, player, newer), battleId, player, f.battle.instance)

	replay := f.watch(t, player, older)
	require.Equal(t, constants.ErrAlreadyWatching, replay.GetErrorMessage().GetId())
	require.Equal(t, deliveryNone, replay.GetDelivery())
	require.Empty(t, replay.GetSpectateTicket())
	requireGlobalLobbyTicket(t, f.watch(t, player, newer), battleId, player, f.battle.instance)
}

// 中继关闭:满员写 16021,不转 L1;同 request_id 重放同结论。L0 有座时回 DIRECT。
func TestGlobalWatchTiDBRelayDisabledAndDirectSeat(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	f.svcCtx.Config.Spectate.RelayEnabled = false
	const battleId = uint64(910006)
	f.addFormation(t, battleId)

	const seated = uint64(65)
	f.addViewer(t, seated)
	direct := f.watch(t, seated, &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId})
	require.Zero(t, direct.GetErrorMessage().GetId(), "tip=%v", direct.GetErrorMessage())
	require.Equal(t, deliveryDirect, direct.GetDelivery())
	require.Empty(t, direct.GetSpectateTicket())

	f.battle.setFull(true)
	const rejected = uint64(66)
	f.addViewer(t, rejected)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	for attempt := 0; attempt < 2; attempt++ {
		resp := f.watch(t, rejected, request)
		require.Equal(t, constants.ErrSpectateFull, resp.GetErrorMessage().GetId())
		require.Equal(t, deliveryNone, resp.GetDelivery())
		require.Empty(t, resp.GetSpectateTicket())
	}
	f.requireIntentRow(t, rejected, request, 5, constants.ErrSpectateFull, 0)
	_, _, attaches, _ := f.battle.counts()
	require.Zero(t, attaches)
}

// 私密局不进 L1 / L2:即使残留了档位键,prepare 也按 16018 关闭意图,不挂 gate、不签凭证。
func TestGlobalWatchTiDBPrivateBattleNeverAdmitsLobby(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(67), uint64(910007)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(&battlepb.CreateBattleRequest{BattleId: battleId, DeadlineMs: lobbyFixtureDeadlineMs, SpectateVisibility: visPrivate})
	require.NoError(t, err)
	f.addFormation(t, battleId)
	_, err = f.db.Exec("UPDATE match_formation SET create_request=? WHERE battle_id=?", raw, battleId)
	require.NoError(t, err)
	f.addViewer(t, player)
	raiseTier(t, f.svcCtx, battleId, tierRelay)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}

	resp := f.watch(t, player, request)
	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Empty(t, resp.GetSpectateTicket())
	_, _, attaches, _ := f.battle.counts()
	require.Zero(t, attaches)
	f.requireIntentRow(t, player, request, 5, constants.ErrBattleNotWatchable, 0)
}

// 中继开着、直连席没满的场照常走 L0:回 DIRECT,意图绑定原件(state 2),不带 LOBBY。
func TestGlobalWatchTiDBFreeSeatStaysDirect(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(68), uint64(910008)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	resp := f.watch(t, player, request)
	require.Zero(t, resp.GetErrorMessage().GetId(), "tip=%v", resp.GetErrorMessage())
	require.Equal(t, deliveryDirect, resp.GetDelivery())
	require.Equal(t, request.RequestId, resp.GetSpectatorOperationId())
	require.Empty(t, resp.GetSpectateTicket())
	f.requireIntentRow(t, player, request, 2, 0, 0)
	require.Equal(t, 1, f.spectatorRows(t, player))
	_, _, attaches, _ := f.battle.counts()
	require.Zero(t, attaches)
}

// failTierRaise 让升档 Lua 报错,把返回的开关置 false 后恢复。钩子与 SetError 共用槽位;
// 单个 svcCtx 注入的失败控制在 5 次以内(go-zero Redis 客户端自带熔断)。
func failTierRaise(t *testing.T, mr *miniredis.Miniredis) *atomic.Bool {
	t.Helper()
	enabled := &atomic.Bool{}
	enabled.Store(true)
	mr.Server().SetPreHook(func(c *server.Peer, cmd string, args ...string) bool {
		if enabled.Load() && cmd == "EVAL" && len(args) > 0 && args[0] == raiseSpectateTierScript {
			c.WriteError("ERR injected tier raise failure")
			return true
		}
		return false
	})
	t.Cleanup(func() { mr.Server().SetPreHook(nil) })
	return enabled
}

// 直连席已满时先落 SQL 结论再升档:升档失败不能让行停在 state 1,否则重试同 op 的 Add 可能把请求关成 16018。
// 这次请求以 LOBBY 结案,重放补升档;重试不再发 Add。
func TestGlobalWatchTiDBFullSeatConclusionSurvivesTierRaiseFailure(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	f.battle.setFull(true)
	const player, battleId = uint64(75), uint64(910020)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	failing := failTierRaise(t, f.mr)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}

	first := f.watch(t, player, request)
	require.Equal(t, constants.ErrInternal, first.GetErrorMessage().GetId(), "重放补升档也失败:这次回故障,客户端重试")
	f.requireIntentRow(t, player, request, 5, 0, uint32(deliveryLobby))
	adds, removes, _, _ := f.battle.counts()
	require.Equal(t, 1, adds)
	require.Equal(t, 1, removes, "L0 原件照常拿关闭证明")

	failing.Store(false)
	retry := f.watch(t, player, request)
	requireGlobalLobbyTicket(t, retry, battleId, player, f.battle.instance)
	adds, _, _, _ = f.battle.counts()
	require.Equal(t, 1, adds, "结论已落库,重试不再发 Add")
	require.Equal(t, tierRelay, tierOf(t, f.svcCtx, battleId), "重放补上档位")
}

// 同 op 重放回首次拒绝码(真实 battle 的墓碑语义):上一轮 Add 已被拒、结论没落库时,重试仍以 LOBBY 结案,不会漂成 16018。
func TestGlobalWatchTiDBFullSeatReplayOfSameOpStaysLobby(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(76), uint64(910021)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	f.battle.tombstone(request.RequestId, uint32(table.MatchError_kMatchSpectateFull))

	requireGlobalLobbyTicket(t, f.watch(t, player, request), battleId, player, f.battle.instance)
	f.requireIntentRow(t, player, request, 5, 0, uint32(deliveryLobby))
}

// 全局路径 battle 回房间不存在:这场不能再留在观战列表、随机选场与配额里(成局 state 4 只说明建房成功)。
func TestGlobalWatchTiDBMissingRoomEvictsIndex(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	f.battle.setRoomMissing(true)
	const player, battleId = uint64(77), uint64(910022)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	member := strconv.FormatUint(battleId, 10)
	for _, key := range []string{spectateBattlesActiveKey, spectateBroadcastActiveKey, spectateFeaturedActiveKey} {
		_, err := f.mr.ZAdd(key, float64(lobbyFixtureDeadlineMs), member)
		require.NoError(t, err)
	}
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}

	resp := f.watch(t, player, request)

	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	f.requireIntentRow(t, player, request, 5, constants.ErrBattleNotWatchable, 0)
	requireBattleSpectateKeysGone(t, f.svcCtx, f.mr, battleId)
}

// 全局期限是模板时长:开局超过旧 TTL(360 秒)的活场不能被当过期剔除,档位也不能回落,否则新观众又回去撞 L0。
func TestGlobalWatchTiDBLongBattleKeepsTierAndListingPastOldTTL(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(78), uint64(910023)
	createdMs := nowMs() - 10*60*1000
	f.addFormationAt(t, battleId, createdMs, createdMs+30*60*1000)
	f.addViewer(t, player)
	_, err := f.mr.ZAdd(spectateBattlesActiveKey, float64(createdMs), strconv.FormatUint(battleId, 10))
	require.NoError(t, err)
	raiseTier(t, f.svcCtx, battleId, tierRelay)

	f.mr.FastForward(7 * time.Minute)

	listed, err := NewListWatchableBattlesLogic(context.Background(), f.svcCtx).ListWatchableBattles(&pb.ListWatchableBattlesRequest{})
	require.NoError(t, err)
	require.Len(t, listed.GetBattles(), 1, "活着的场还在列表里")
	require.Equal(t, 1, activeIndexSize(t, f.svcCtx))
	requireGlobalLobbyTicket(t, f.watch(t, player, &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}), battleId, player, f.battle.instance)
	adds, _, _, _ := f.battle.counts()
	require.Zero(t, adds, "档位还在,不撞 L0")
}

// 终局提交后清掉这场的观战数据:L2 配额立刻还给别的场,结束的场离开列表与随机选场。清理是尽力而为,不影响终局回执。
func TestCommitBattleOutcomeTiDBReleasesSpectateData(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const secret = "fixture-only-battle-terminal-signature-000000000"
	verifier, err := serviceauth.New(map[string]string{"battle": secret})
	require.NoError(t, err)
	f.svcCtx.BattleOutcomeAuth, f.svcCtx.BattleOutcomeSecret = verifier, secret
	const battleId, playerA, playerB = uint64(910024), uint64(79), uint64(80)
	f.addViewer(t, playerA)
	f.addViewer(t, playerB)
	request := f.addCompletableFormation(t, battleId, playerA, playerB, secret)
	seedBattleSpectateKeys(t, f.svcCtx, f.mr, battleId)

	reply, err := CommitBattleOutcome(context.Background(), f.svcCtx, request)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, reply.GetDecision())

	idle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.svcCtx.RelayAdmission.WaitIdle(idle))
	requireBattleSpectateKeysGone(t, f.svcCtx, f.mr, battleId)
}

// registerBattleNode 把注册表换成只有节点 7 一条登记、实例为 instance。
func (f *globalLobbyFixture) registerBattleNode(instance string) {
	f.svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)
	f.svcCtx.BattleNodes.Upsert(svc.BattleNodeRpcPrefix+"7", discovery.NodeEntry{NodeId: 7, NodeUuid: instance, Endpoint: f.addr})
}

// intentState 读意图行的 state(原件关闭之后读 match_spectator 可能已是下一个 op 的行,判定要看意图)。
func (f *globalLobbyFixture) intentState(t *testing.T, player uint64, request *pb.WatchBattleRequest) uint32 {
	t.Helper()
	id, err := uuid.Parse(request.RequestId)
	require.NoError(t, err)
	var state uint32
	require.NoError(t, f.db.QueryRow("SELECT state FROM match_spectator_intent WHERE player_id=? AND request_high=? AND request_low=?",
		player, beUint64(id[:8]), beUint64(id[8:])).Scan(&state))
	return state
}

// setFormationDeadlineAgo 把成局期限改成 TiDB 时钟的 ago 之前。
func (f *globalLobbyFixture) setFormationDeadlineAgo(t *testing.T, battleId uint64, ago time.Duration) {
	t.Helper()
	stamp, err := f.svcCtx.Authority.Now(context.Background())
	require.NoError(t, err)
	_, err = f.db.Exec("UPDATE match_formation SET deadline_ms=? WHERE battle_id=?", stamp-uint64(ago.Milliseconds()), battleId)
	require.NoError(t, err)
}

// 原实例已不存在时永远拿不到取消证明(battle 对实例不符的 Remove / Query 回空包):满足任一判据就不带证明关闭,
// 后台恢复、排队路径与新观战共用同一判定;都不满足时不关,原件继续等证明。
func TestGlobalSpectatorClosesWithoutProofOnlyWhenSourceGone(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	cases := []struct {
		name       string
		arrange    func(t *testing.T, battleId uint64)
		wantReason string // 空 = 判不出原实例已不存在,原件不能关
	}{
		{"成局已关", func(t *testing.T, battleId uint64) {
			_, err := f.db.Exec("UPDATE match_formation SET state=5 WHERE battle_id=?", battleId)
			require.NoError(t, err)
		}, metrics.SpectatorUnprovenCloseFormationEnded},
		{"已过期限加宽限", func(t *testing.T, battleId uint64) { f.setFormationDeadlineAgo(t, battleId, 61*time.Second) }, metrics.SpectatorUnprovenCloseFormationEnded},
		{"同 node_id 已是别的实例", func(*testing.T, uint64) { f.registerBattleNode("replaced-instance") }, metrics.SpectatorUnprovenCloseInstanceReplaced},
		{"期限已过但还在宽限内", func(t *testing.T, battleId uint64) { f.setFormationDeadlineAgo(t, battleId, 30*time.Second) }, ""},
		{"注册表里还是原实例", func(*testing.T, uint64) { f.registerBattleNode(f.battle.instance) }, ""},
		{"注册表查不到", func(*testing.T, uint64) {
			f.svcCtx.BattleNodes = discovery.NewNodeWatcher("battle", svc.BattleNodeRpcPrefix, nil, nil)
		}, ""},
	}
	entries := []struct {
		name string
		run  func(t *testing.T, player, otherBattle uint64, original *pb.MatchSpectatorRecord) error
	}{
		{"后台恢复", func(_ *testing.T, _, _ uint64, original *pb.MatchSpectatorRecord) error {
			return resumeGlobalSpectator(context.Background(), f.svcCtx, original, "")
		}},
		{"排队路径", func(_ *testing.T, _, _ uint64, original *pb.MatchSpectatorRecord) error {
			return closeCapturedSpectatorExit(context.Background(), f.svcCtx, original)
		}},
		{"新观战", func(t *testing.T, player, otherBattle uint64, _ *pb.MatchSpectatorRecord) error {
			f.addFormation(t, otherBattle)
			resp := f.watch(t, player, &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: otherBattle})
			if tip := resp.GetErrorMessage().GetId(); tip != 0 {
				return fmt.Errorf("新观战被拒 tip=%d", tip)
			}
			return nil
		}},
	}
	sequence := 0
	for _, c := range cases {
		for _, entry := range entries {
			sequence++
			player, battleId := uint64(200+sequence), uint64(920000+sequence)
			t.Run(c.name+"/"+entry.name, func(t *testing.T) {
				f.svcCtx.BattleNodes = nil
				f.battle.setInstanceGone(false)
				f.addFormation(t, battleId)
				f.addViewer(t, player)
				request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
				require.Equal(t, deliveryDirect, f.watch(t, player, request).GetDelivery())
				original, err := f.svcCtx.Authority.SpectatorForActor(f.actor(t, player), player, request.RequestId)
				require.NoError(t, err)
				require.Equal(t, uint32(2), original.GetState())

				f.battle.setInstanceGone(true)
				c.arrange(t, battleId)
				before := 0.0
				if c.wantReason != "" {
					before = metrics.SpectatorUnprovenCloseValue(c.wantReason)
				}
				err = entry.run(t, player, battleId+5000, original)

				if c.wantReason == "" {
					require.Error(t, err)
					require.Equal(t, uint32(2), f.intentState(t, player, request), "判不出来就不关,继续等证明")
					return
				}
				require.NoError(t, err)
				require.Equal(t, uint32(5), f.intentState(t, player, request))
				require.Equal(t, before+1, metrics.SpectatorUnprovenCloseValue(c.wantReason))
			})
		}
	}
}

// 实例还活着时优先拿正证明:成局已关也先发 Remove,不算不带证明的关闭。
func TestGlobalSpectatorPrefersCancelProofWhenInstanceAlive(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const player, battleId = uint64(81), uint64(910025)
	f.addFormation(t, battleId)
	f.addViewer(t, player)
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	require.Equal(t, deliveryDirect, f.watch(t, player, request).GetDelivery())
	original, err := f.svcCtx.Authority.SpectatorForActor(f.actor(t, player), player, request.RequestId)
	require.NoError(t, err)
	_, err = f.db.Exec("UPDATE match_formation SET state=5 WHERE battle_id=?", battleId)
	require.NoError(t, err)
	before := metrics.SpectatorUnprovenCloseValue(metrics.SpectatorUnprovenCloseFormationEnded)

	require.NoError(t, resumeGlobalSpectator(context.Background(), f.svcCtx, original, ""))

	require.Equal(t, uint32(5), f.intentState(t, player, request))
	_, removes, _, _ := f.battle.counts()
	require.Equal(t, 1, removes)
	require.Equal(t, before, metrics.SpectatorUnprovenCloseValue(metrics.SpectatorUnprovenCloseFormationEnded))
}

// battle 节点重启(同 node_id 新实例):L2 观众不调 battle,靠注册表发现来源实例已不存在,按房间不存在收场;注册表还是原实例时照常签票。
func TestGlobalWatchTiDBReplacedInstanceRejectedWithoutRPC(t *testing.T) {
	f := newGlobalLobbyFixture(t)
	const seated, rejected, battleId = uint64(82), uint64(83), uint64(910026)
	f.addFormation(t, battleId)
	f.addViewer(t, seated)
	f.addViewer(t, rejected)
	raiseTier(t, f.svcCtx, battleId, tierBroadcast)
	f.registerBattleNode(f.battle.instance)
	requireGlobalLobbyTicket(t, f.watch(t, seated, &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}), battleId, seated, f.battle.instance)

	f.registerBattleNode("replaced-instance")
	request := &pb.WatchBattleRequest{RequestId: uuid.NewString(), BattleId: battleId}
	resp := f.watch(t, rejected, request)

	require.Equal(t, constants.ErrBattleNotWatchable, resp.GetErrorMessage().GetId())
	require.Empty(t, resp.GetSpectateTicket())
	f.requireIntentRow(t, rejected, request, 5, constants.ErrBattleNotWatchable, 0)
	requireBattleSpectateKeysGone(t, f.svcCtx, f.mr, battleId)
	adds, removes, attaches, promotes := f.battle.counts()
	require.Zero(t, adds+removes+attaches+promotes, "不增加 RPC")
}

package authority

// 客户端写方法在请求截止之后不提交(docs/design/client-request-ordering.md §6.1 / O11)的回归测试,不连 TiDB。
//
// gate 对同一会话的路由类请求单飞:路由服回超时,gate 就放行同会话的下一条。match 的 handler 若在截止之后仍把写
// 提交出去,这条写就晚于下一条请求落地,例如「入队 → 撤销」执行成「撤销 → 入队」,两条都回成功、玩家最终仍在排队(§1.3)。
// Redis 队列时代的两份 request_deadline_fence_test.go 随 65c69f4a9 的权威 SQL 重构整体删除,这里给现在的 SQL 路径补上。
//
// 用录制驱动把「有没有语句、有没有 COMMIT 到达驱动」钉死:
//   - 请求 ctx 已结束:一条语句都不到达驱动(database/sql 取连接前先看 ctx),错误就是 ctx 的错误。
//     它抓的回归是客户端写路径上某处改用 context.Background() / WithoutCancel 开事务或发语句;
//   - 提交点之前的最后一条写已经发出、执行中跑过了截止:不发 COMMIT,驱动收到 ROLLBACK(Tx.Commit 先看 ctx;
//     生产的 authoritysql 守卫驱动在 COMMIT 前再看一次事务 ctx,并把自动提交语句也包成同样受 ctx 约束的事务)。
//     它抓的回归是把事务体或其中一段挪到独立 ctx 上提交。
//
// 截止之前已经发出的 COMMIT 撤不回来,那一段窗口由角色行锁串行化兜住(§6.1 首段,同一玩家的 Join / Cancel / 整队撤销
// 先争同一把 P 锁,后到的请求读到的是已经定局的结果),不在本文件范围。真库上的锁与提交结果仍由同目录 *TiDB* 用例证明;
// 这里的角色护栏与入场闸换成放行替身,只看事务体与 COMMIT 之间跑过截止会怎样。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/config"
	base "proto/common/base"
	dspb "proto/data_service"
	pb "proto/match"
	teampb "proto/team"
	"shared/roleidentity"
)

// ── 录制驱动 ────────────────────────────────────────────────

var errSQLTraceUnexpected = errors.New("sql trace fixture: statement not scripted")

// sqlTraceRecorder 录下到达驱动的每一步(CONNECT / BEGIN / 语句 / COMMIT / ROLLBACK,语句压缩空白)。
// 读语句按 query 脚本回结果,脚本为 nil 或回 nil 表示没有行;写语句一律成功,onExec 在写执行时被调用,
// 用例在这里取消请求 ctx,模拟「语句已经发出、执行中跑过截止」。
type sqlTraceRecorder struct {
	mu         sync.Mutex
	statements []string
	query      func(statement string) *sqlTraceRows
	onExec     func(statement string)
	// ended 收到 "COMMIT" 或 "ROLLBACK"。ctx 结束后的回滚由 database/sql 的后台协程执行,可能晚于被测方法返回,
	// 所以用例等这个驱动事件,而不是方法一返回就断言。
	ended chan string
}

func newSQLTraceRecorder() *sqlTraceRecorder {
	return &sqlTraceRecorder{ended: make(chan string, 8)}
}

func (r *sqlTraceRecorder) record(step string) string {
	compact := strings.Join(strings.Fields(step), " ")
	r.mu.Lock()
	r.statements = append(r.statements, compact)
	r.mu.Unlock()
	return compact
}

func (r *sqlTraceRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.statements...)
}

func (r *sqlTraceRecorder) endTx(verb string) {
	r.record(verb)
	select {
	case r.ended <- verb:
	default:
	}
}

// awaitTxEnd 等事务结束。上限只用于事务永不结束(回归)时及早判红、不把 go test 挂到超时,正常路径不依赖它。
func (r *sqlTraceRecorder) awaitTxEnd(t *testing.T) string {
	t.Helper()
	select {
	case verb := <-r.ended:
		return verb
	case <-time.After(10 * time.Second):
		t.Fatal("transaction neither committed nor rolled back")
		return ""
	}
}

func (r *sqlTraceRecorder) openDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(sqlTraceConnector{r: r})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type sqlTraceConnector struct{ r *sqlTraceRecorder }

func (c sqlTraceConnector) Connect(context.Context) (driver.Conn, error) {
	c.r.record("CONNECT")
	return sqlTraceConn{r: c.r}, nil
}
func (c sqlTraceConnector) Driver() driver.Driver { return sqlTraceDriver{} }

type sqlTraceDriver struct{}

func (sqlTraceDriver) Open(string) (driver.Conn, error) { return nil, errSQLTraceUnexpected }

type sqlTraceConn struct{ r *sqlTraceRecorder }

func (c sqlTraceConn) Prepare(query string) (driver.Stmt, error) {
	c.r.record("PREPARE " + query)
	return nil, errSQLTraceUnexpected
}
func (c sqlTraceConn) Close() error { return nil }
func (c sqlTraceConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c sqlTraceConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.r.record("BEGIN")
	return sqlTraceTx{r: c.r}, nil
}
func (c sqlTraceConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	statement := c.r.record(query)
	if c.r.onExec != nil {
		c.r.onExec(statement)
	}
	return driver.RowsAffected(1), nil
}
func (c sqlTraceConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	statement := c.r.record(query)
	if c.r.query != nil {
		if rows := c.r.query(statement); rows != nil {
			return rows, nil
		}
	}
	return &sqlTraceRows{}, nil
}

// CheckNamedValue 原样接收全部参数:请求号拆出的 uint64 可能置了最高位,database/sql 的缺省转换会拒绝它。
func (c sqlTraceConn) CheckNamedValue(*driver.NamedValue) error { return nil }

type sqlTraceTx struct{ r *sqlTraceRecorder }

func (tx sqlTraceTx) Commit() error   { tx.r.endTx("COMMIT"); return nil }
func (tx sqlTraceTx) Rollback() error { tx.r.endTx("ROLLBACK"); return nil }

// sqlTraceRows 是一份脚本结果集。值直接用被扫描字段的 Go 类型(uint64 / uint32 / string / []byte),
// database/sql 按可赋值直接写入,不经字符串转换。
type sqlTraceRows struct {
	values [][]driver.Value
	next   int
}

func sqlTraceRow(values ...driver.Value) *sqlTraceRows {
	return &sqlTraceRows{values: [][]driver.Value{values}}
}

func (r *sqlTraceRows) Columns() []string {
	if len(r.values) == 0 {
		return nil
	}
	columns := make([]string, len(r.values[0]))
	for i := range columns {
		columns[i] = fmt.Sprintf("c%d", i)
	}
	return columns
}
func (r *sqlTraceRows) Close() error { return nil }
func (r *sqlTraceRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

// sqlTraceTicketRow 按 ticketColumns 的列序给一行票据。
func sqlTraceTicketRow(t *pb.MatchAuthorityTicketRecord) *sqlTraceRows {
	return sqlTraceRow(t.PlayerId, t.Ticket, t.RequestId, t.Mode, t.ConfigId, t.TeamSize, t.State, t.EnqueuedMs, t.NotBeforeMs, t.ZoneId,
		t.RatingCenti, t.FormationId, t.TeamId, t.NetworkProfile, t.ExpiresMs, t.SelectedRegion, t.OwnerAccount, t.OwnerRevision)
}

// sqlTraceLaunchRow 按 teamLaunchColumns 的列序给一行整队开战原件。
func sqlTraceLaunchRow(l *teampb.TeamLaunchRecord) *sqlTraceRows {
	return sqlTraceRow(l.ActorId, l.RequestHigh, l.RequestLow, l.TeamId, l.TeamVersion, l.LockToken, l.ConfigId, l.TargetSize, l.MemberCount,
		l.State, l.EnqueuedMs, l.NotBeforeMs, l.BattleId, l.Payload, l.OwnerAccount, l.OwnerRevision, l.Mode)
}

// ── 夹具 ────────────────────────────────────────────────────

// sqlTraceStore 是只连录制驱动的权威库。角色护栏与入场闸换成放行替身(它们的锁语义由真库用例证明)。
func sqlTraceStore(t *testing.T, r *sqlTraceRecorder) *Store {
	t.Helper()
	s := &Store{DB: r.openDB(t), Config: config.GlobalMatchConf{HardLatencyMs: 200}}
	s.roleGuard = func(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord) error { return nil }
	s.entryGuard = func(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord, *matchEntryOriginal) error { return nil }
	return s
}

func sqlTraceAccount(player uint64) string { return fmt.Sprintf("original-%d", player) }

// sqlTraceActor 是带原会话角色归属的请求 ctx(生产上由入口拦截器 roleidentity.EntryInterceptor 写入)。
func sqlTraceActor(t *testing.T, player uint64) context.Context {
	t.Helper()
	actor, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: sqlTraceAccount(player), OwnerRevision: 7, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return actor
}

// sqlTraceTeamLaunch 组一份 11 号发起、11 / 12 两人满员的整队开战原件(state 1,尚未成局),以及与它一致的队伍权威行。
// mode 是请求的匹配模式:0 = 老客户端(按 PVE_TEAM),PVP_TEAM 时目标人数是每侧人数,两人满员一侧。
func sqlTraceTeamLaunch(t *testing.T, mode pb.MatchMode) (*teampb.TeamLaunchRecord, []*pb.MatchAuthorityTicketRecord, *teampb.TeamAuthorityData) {
	t.Helper()
	group := []*pb.MatchAuthorityTicketRecord{
		{PlayerId: 11, Ticket: uuid.NewString(), TeamId: 701, ZoneId: 1, OwnerAccount: sqlTraceAccount(11), OwnerRevision: 7},
		{PlayerId: 12, Ticket: uuid.NewString(), TeamId: 701, ZoneId: 1, OwnerAccount: sqlTraceAccount(12), OwnerRevision: 7},
	}
	request := &teampb.StartTeamMatchRequest{RequestId: uuid.NewString(), ExpectedTeamId: 701, BattleConfigId: 1, TargetTeamSize: uint32(len(group)), MatchMode: mode}
	key, err := DirectUUIDKey(1, group[0].PlayerId, request.RequestId)
	require.NoError(t, err)
	token := uuid.NewString()
	record := &teampb.TeamRecord{TeamId: 701, LeaderId: group[0].PlayerId, MatchLockToken: token}
	team := &teampb.TeamAuthorityData{Record: record}
	original := &teampb.TeamLaunchOriginal{TeamId: 701, TeamVersion: 3, LockToken: token, Request: request, Zones: map[uint64]uint32{}, EnqueuedMs: 1}
	for index, ticket := range group {
		role := &teampb.TeamOriginalRole{PlayerId: ticket.PlayerId, OwnerAccount: ticket.OwnerAccount, OwnerRevision: ticket.OwnerRevision}
		record.Members = append(record.Members, &teampb.TeamMemberRecord{PlayerId: ticket.PlayerId, ZoneId: ticket.ZoneId, JoinSeq: uint32(index + 1)})
		team.Members = append(team.Members, role)
		original.Members = append(original.Members, &teampb.TeamLaunchMember{Original: role, RosterOrder: uint32(index + 1), Ticket: ticket.Ticket})
		original.Zones[ticket.PlayerId] = ticket.ZoneId
	}
	original.Actor = team.Members[0]
	requestRaw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&teampb.TeamLaunchData{Original: original, Members: original.Members, RequestPayload: requestRaw, TeamRecord: record})
	require.NoError(t, err)
	launch := &teampb.TeamLaunchRecord{ActorId: key.OwnerId, RequestHigh: key.RequestHigh, RequestLow: key.RequestLow, TeamId: 701, TeamVersion: original.TeamVersion,
		LockToken: token, ConfigId: request.BattleConfigId, TargetSize: request.TargetTeamSize, MemberCount: uint32(len(group)), State: 1, EnqueuedMs: original.EnqueuedMs,
		Payload: payload, OwnerAccount: original.Actor.OwnerAccount, OwnerRevision: original.Actor.OwnerRevision, Mode: uint32(mode)}
	_, err = DecodeTeamLaunch(launch)
	require.NoError(t, err)
	return launch, group, team
}

// requireDeadlineRefusal:提交点因请求 ctx 已结束被拒。Tx.Commit 回 ctx 的错误;database/sql 的后台协程抢先回滚时回 ErrTxDone。
func requireDeadlineRefusal(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrTxDone), "unexpected error: %v", err)
}

// requireReachedStatement 证明用例确实走到了提交点之前的那条写,否则「没有 COMMIT」可能只是更早就失败了。
func requireReachedStatement(t *testing.T, statements []string, prefix string) {
	t.Helper()
	for _, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			return
		}
	}
	t.Fatalf("no statement starting with %q reached the driver; recorded: %v", prefix, statements)
}

// ── 用例 ────────────────────────────────────────────────────

// 请求 ctx 已经结束:客户端路径上的每个写方法一条语句都不发(连连接都不取),错误就是 ctx 的错误。
// 名单覆盖入队(探测计划、探测回执、待提交原意图、入队)、撤销(单排、整队发起、整队排队)、整队开战的两步提交、
// 切磋(发起、应答前读、应答、拒战)与点名开战的原件准备;Ticket 也在里面,它会顺手把过期票写成 4;
// ReleaseCompletedReady 是 JoinQueue 入口释放上一局已结算票的那一步。
func TestClientWritesAfterRequestDeadlineSendNothing(t *testing.T) {
	recorder := newSQLTraceRecorder()
	s := sqlTraceStore(t, recorder)
	ctx, cancel := context.WithCancel(sqlTraceActor(t, 11))
	cancel()

	ticket := &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2, ZoneId: 1}
	join := &pb.JoinQueueRequest{RequestId: ticket.RequestId, NetworkPlanId: "plan-11", Mode: pb.MatchMode(3), BattleConfigId: 1}
	profile := &pb.MatchNetworkProfile{PlanId: "plan-11", SessionBinding: "bound-11"}
	launch, group, _ := sqlTraceTeamLaunch(t, 0)
	formation := &pb.MatchFormationRecord{BattleId: 9001, Mode: uint32(pb.MatchMode_MATCH_MODE_PVE_TEAM), ConfigId: 1}
	challenge := &pb.MatchChallengeInvitationRecord{ChallengeId: 5001, ChallengerPlayerId: 12, TargetPlayerId: 11, ConfigId: 1,
		ChallengerAccount: sqlTraceAccount(12), ChallengerRevision: 7, TargetAccount: sqlTraceAccount(11), TargetRevision: 7}
	decline, err := DirectUUIDKey(3, 11, uuid.NewString())
	require.NoError(t, err)
	direct, err := DirectUUIDKey(1, 11, uuid.NewString())
	require.NoError(t, err)

	writes := []struct {
		name string
		call func() error
	}{
		{"Ticket", func() error {
			_, err := s.Ticket(ctx, 11)
			return err
		}},
		{"ReleaseCompletedReady", func() error {
			_, err := s.ReleaseCompletedReady(ctx, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: ticket.Ticket, State: 3, FormationId: 9001})
			return err
		}},
		{"SavePlan", func() error {
			return s.SavePlan(ctx, 11, profile, 1)
		}},
		{"CommitProfile", func() error {
			return s.CommitProfile(ctx, 11, profile.PlanId, profile.SessionBinding, nil, 1)
		}},
		{"PrepareJoin", func() error {
			_, _, _, err := s.PrepareJoin(ctx, ticket, join, profile.SessionBinding)
			return err
		}},
		{"Join", func() error {
			_, _, err := s.Join(ctx, ticket, profile.PlanId, profile.SessionBinding)
			return err
		}},
		{"CancelIntent", func() error {
			return s.CancelIntent(ctx, 11, ticket.Ticket, ticket.RequestId)
		}},
		{"Cancel", func() error {
			return s.Cancel(ctx, 11, ticket.Ticket)
		}},
		{"CancelTeamLaunchForActor", func() error {
			_, err := s.CancelTeamLaunchForActor(ctx, 11, launch)
			return err
		}},
		{"CancelTeamQueueForActor", func() error {
			_, err := s.CancelTeamQueueForActor(ctx, 11, &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: group[0].Ticket, TeamId: 701, State: 1})
			return err
		}},
		{"EnqueuePreparedTeamLaunch", func() error {
			return s.EnqueuePreparedTeamLaunch(ctx, launch)
		}},
		{"ReserveTeamLaunch", func() error {
			_, err := s.ReserveTeamLaunch(ctx, launch, formation, group)
			return err
		}},
		{"CreateChallenge", func() error {
			_, err := s.CreateChallenge(ctx, 5001, 11, 12, 1, 60000)
			return err
		}},
		{"ChallengeForActor", func() error {
			_, err := s.ChallengeForActor(ctx, 5001, 11)
			return err
		}},
		{"DecideChallenge", func() error {
			_, _, err := s.DecideChallenge(ctx, challenge, false, nil, nil)
			return err
		}},
		{"DeclineDirect", func() error {
			_, err := s.DeclineDirect(ctx, decline, []byte("decline"))
			return err
		}},
		{"PrepareDirectAdmission", func() error {
			_, err := s.PrepareDirectAdmission(ctx, direct, []byte("start"), nil, nil)
			return err
		}},
	}
	for _, write := range writes {
		require.ErrorIs(t, write.call(), context.Canceled, write.name)
	}
	require.Empty(t, recorder.recorded(), "no statement may reach the driver after the request deadline")
}

// 入队:原票与原意图两条写都已发出,提交前跑过了截止 → 不发 COMMIT,事务回滚,票没落。
// 下一条同会话请求(撤销)因此不会被一张迟到的票盖住。
func TestJoinDeadlineBeforeCommitLeavesNoTicket(t *testing.T) {
	recorder := newSQLTraceRecorder()
	s := sqlTraceStore(t, recorder)
	ctx, cancel := context.WithCancel(sqlTraceActor(t, 11))
	defer cancel()

	const nowMs uint64 = 1_800_000_000_000
	profile := &pb.MatchNetworkProfile{PlanId: "plan-11", SessionBinding: "bound-11", OwnerAccount: sqlTraceAccount(11), OwnerRevision: 7,
		Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: 11, PlanId: "plan-11", SessionBinding: "bound-11", PhysicalRegion: 1, ExpiresAtMs: nowMs + 120000},
			RttMs: 40, SampleCount: 3, MeasuredAtMs: nowMs, ExpiresAtMs: nowMs + 120000}}}
	rawProfile, err := proto.Marshal(profile)
	require.NoError(t, err)
	recorder.query = func(statement string) *sqlTraceRows {
		switch {
		case strings.HasPrefix(statement, "SELECT CAST(UNIX_TIMESTAMP("):
			return sqlTraceRow(nowMs)
		case strings.HasPrefix(statement, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE"):
			// INSERT IGNORE 补出来的占位行:这名玩家从没入过队。
			return sqlTraceTicketRow(&pb.MatchAuthorityTicketRecord{PlayerId: 11})
		case strings.HasPrefix(statement, "SELECT payload,expires_ms FROM match_network_profile"):
			return sqlTraceRow(rawProfile, nowMs+120000)
		}
		return nil // 没有同请求的原意图,没有观战
	}
	recorder.onExec = func(statement string) {
		if strings.HasPrefix(statement, "INSERT INTO match_join_intent") {
			cancel() // 提交点之前的最后一条写执行中跑过截止
		}
	}

	ticket := &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2, ZoneId: 1}
	_, _, err = s.Join(ctx, ticket, profile.PlanId, profile.SessionBinding)
	requireDeadlineRefusal(t, err)
	require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
	statements := recorder.recorded()
	requireReachedStatement(t, statements, "UPDATE match_authority_ticket SET ticket=?")
	requireReachedStatement(t, statements, "INSERT INTO match_join_intent")
	require.NotContains(t, statements, "COMMIT")
}

// 单排撤销:撤销墓碑与票据出队两条写都已发出,提交前跑过了截止 → 不发 COMMIT,事务回滚。
func TestCancelIntentDeadlineBeforeCommitRollsBack(t *testing.T) {
	recorder := newSQLTraceRecorder()
	s := sqlTraceStore(t, recorder)
	ctx, cancel := context.WithCancel(sqlTraceActor(t, 11))
	defer cancel()

	queued := &pb.MatchAuthorityTicketRecord{PlayerId: 11, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2, State: 1,
		EnqueuedMs: 1, ZoneId: 1, ExpiresMs: 1_800_000_120_000, OwnerAccount: sqlTraceAccount(11), OwnerRevision: 7}
	recorder.query = func(statement string) *sqlTraceRows {
		if strings.HasPrefix(statement, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE") {
			return sqlTraceTicketRow(queued)
		}
		return nil // 原意图行还没有(撤销会补一行墓碑)
	}
	recorder.onExec = func(statement string) {
		if strings.HasPrefix(statement, "UPDATE match_authority_ticket SET state=4") {
			cancel()
		}
	}

	requireDeadlineRefusal(t, s.CancelIntent(ctx, 11, queued.Ticket, queued.RequestId))
	require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
	statements := recorder.recorded()
	requireReachedStatement(t, statements, "INSERT INTO match_join_intent")
	requireReachedStatement(t, statements, "UPDATE match_authority_ticket SET state=4")
	require.NotContains(t, statements, "COMMIT")
}

// 整队开战意图的撤销(CancelQueue 带原请求号命中尚未成局的 launch):清开战锁、作废 launch、作废名单三条写都已发出,
// 提交前跑过了截止 → 不发 COMMIT。launch 仍是 state 1,由同一 request_id 的重试、撤销或后台恢复按原件续推,
// 不会出现「撤销回了超时、实际却已作废」与重试交错。
func TestCancelTeamLaunchDeadlineBeforeCommitKeepsLaunch(t *testing.T) {
	recorder := newSQLTraceRecorder()
	s := sqlTraceStore(t, recorder)
	ctx, cancel := context.WithCancel(sqlTraceActor(t, 11))
	defer cancel()

	launch, _, team := sqlTraceTeamLaunch(t, 0)
	teamPayload, err := proto.MarshalOptions{Deterministic: true}.Marshal(team)
	require.NoError(t, err)
	recorder.query = func(statement string) *sqlTraceRows {
		switch {
		case strings.HasPrefix(statement, "SELECT version,payload,state FROM team_authority WHERE team_id=? FOR UPDATE"):
			return sqlTraceRow(launch.TeamVersion+1, teamPayload, uint32(1))
		case strings.HasPrefix(statement, "SELECT "+teamLaunchColumns+" FROM team_launch WHERE actor_id=? AND request_high=? AND request_low=? FOR UPDATE"):
			return sqlTraceLaunchRow(launch)
		}
		return nil
	}
	recorder.onExec = func(statement string) {
		if strings.HasPrefix(statement, "UPDATE team_launch_role SET state=5") {
			cancel()
		}
	}

	_, err = s.CancelTeamLaunchForActor(ctx, 11, launch)
	requireDeadlineRefusal(t, err)
	require.Equal(t, "ROLLBACK", recorder.awaitTxEnd(t))
	statements := recorder.recorded()
	requireReachedStatement(t, statements, "UPDATE team_authority SET version=?")
	requireReachedStatement(t, statements, "UPDATE team_launch SET state=5")
	requireReachedStatement(t, statements, "UPDATE team_launch_role SET state=5")
	require.NotContains(t, statements, "COMMIT")
}

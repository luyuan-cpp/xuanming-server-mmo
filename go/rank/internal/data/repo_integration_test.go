package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"regexp"
	"shared/authoritysql/sqltest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	rankpb "proto/rank"
	"schemamigrate"
)

func rankFixture(t *testing.T) (context.Context, *sql.DB, NextID) {
	ctx, db, next, _ := rankFixtureWithWriter(t)
	return ctx, db, next
}

func rankFixtureWithWriter(t *testing.T) (context.Context, *sql.DB, NextID, *sqltest.Fixture) {
	t.Helper()
	address := os.Getenv("MAIL_RANK_TEST_TIDB")
	if address == "" {
		if os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" {
			t.Fatal("MAIL_RANK_TEST_TIDB必须显式配置本任务独占TiDB")
		}
		t.Skip("需显式本任务数值loopback TiDB端点，随机自有库")
	}
	if err := rankDataFixtureEndpoint(address); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "rank_it_" + hex.EncodeToString(random[:])
	opts, err := sqltest.OptionsFromEnv("root@tcp("+address+")/mysql", database+"_writer", database)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	install, err := fixture.OpenSchema(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	report, err := schemamigrate.Up(ctx, install, schemamigrate.Options{Database: database, Tables: Tables()})
	if err != nil || len(report.Manual) > 0 {
		t.Fatalf("schema: %v %+v", err, report)
	}
	if err = fixture.GrantProtoTables(ctx, database, Tables()); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, database, 8)
	if err != nil {
		t.Fatal(err)
	}
	var next atomic.Uint64
	return ctx, db, func(context.Context) (uint64, error) { return next.Add(1), nil }, fixture
}

func rankDataFixtureEndpoint(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("排行榜夹具必须显式数值loopback端口")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || number < 1 || number > 65535 {
		return errors.New("排行榜夹具仅接受数值loopback与合法非零端口")
	}
	return nil
}
func TestRankDataFixtureEndpointRejectsForeignOrInvalidPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:24000", "[::1]:24000"} {
		if err := rankDataFixtureEndpoint(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"localhost:24000", "10.0.0.1:24000", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:mysql", "127.0.0.1"} {
		if rankDataFixtureEndpoint(address) == nil {
			t.Fatal("非法夹具端点未拒绝")
		}
	}
}
func testOpen(t *testing.T, ctx context.Context, r *Repo) *rankpb.OpenRankGenerationResponse {
	t.Helper()
	out, err := r.Open(ctx, "match", &rankpb.OpenRankGenerationRequest{Board: &rankpb.RankBoardKey{BoardType: 1, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "season-1", Generation: 1}, Producer: "match", SourceEpoch: 11, RequestId: 1})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestTiDBOrderedScoresFreezeAndReliableRewardIntents(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, next, func(uint32) bool { return true })
	board := testOpen(t, ctx, r)
	input := &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceCursor: 1, RequestId: 10, Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET, Entries: []*rankpb.RankScoreInput{{EntityId: 7, Score: 100, AchievedMs: 10}, {EntityId: 8, Score: 100, AchievedMs: 20}, {EntityId: 3, Score: 90, AchievedMs: 30}}}
	if _, err := r.Submit(ctx, "other", input); !errors.Is(err, ErrDenied) {
		t.Fatalf("未授权写分: %v", err)
	}
	if _, err := r.Submit(ctx, "match", input); err != nil {
		t.Fatal(err)
	}
	replayed, err := r.Submit(ctx, "match", input)
	if err != nil || !replayed.Duplicated {
		t.Fatalf("重复cursor: %v %+v", err, replayed)
	}
	changed := proto.Clone(input).(*rankpb.SubmitRankScoresRequest)
	changed.Entries[0].Score++
	if _, err = r.Submit(ctx, "match", changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("篡改cursor: %v", err)
	}
	changed = proto.Clone(input).(*rankpb.SubmitRankScoresRequest)
	changed.SourceCursor = 3
	changed.RequestId = 12
	if _, err = r.Submit(ctx, "match", changed); !errors.Is(err, ErrCursor) {
		t.Fatalf("缺口cursor: %v", err)
	}
	page, err := r.Page(ctx, 3, &rankpb.GetRankPageRequest{Board: &rankpb.RankBoardKey{BoardType: 1, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "season-1"}, Page: 2, PageSize: 1})
	if err != nil || page.TotalCount != 3 || len(page.Entries) != 1 || page.Entries[0].EntityId != 8 || page.Self.GetRank() != 3 {
		t.Fatalf("权威分页: %v %+v", err, page)
	}
	freeze := &rankpb.FreezeRankGenerationRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, ExpectedSourceCursor: 2, RequestId: 20, TopN: 2, Rewards: []*rankpb.RankRewardTier{{FirstRank: 1, LastRank: 2, Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 10}}}}}}
	if _, err = r.Freeze(ctx, "match", freeze); !errors.Is(err, ErrCursor) {
		t.Fatalf("未齐输入不可seal: %v", err)
	}
	freeze.ExpectedSourceCursor = 1
	settled, err := r.Freeze(ctx, "match", freeze)
	if err != nil || settled.WinnerCount != 2 {
		t.Fatalf("seal: %v %+v", err, settled)
	}
	for _, table := range []string{"rank_snapshot", "rank_mail_reward"} {
		var count int
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 2 {
			t.Fatalf("完整%s: %d %v", table, count, err)
		}
	}
	replay, err := r.Freeze(ctx, "match", freeze)
	if err != nil || !replay.Duplicated || replay.SettlementId != settled.SettlementId {
		t.Fatalf("seal回放: %v %+v", err, replay)
	}
	changed = proto.Clone(input).(*rankpb.SubmitRankScoresRequest)
	changed.SourceCursor = 2
	changed.RequestId = 11
	if _, err = r.Submit(ctx, "match", changed); !errors.Is(err, ErrClosed) {
		t.Fatalf("seal后写入: %v", err)
	}
	if err = r.Remove(ctx, "match", &rankpb.RemoveRankEntryRequest{BoardId: board.BoardId, Generation: 1, RequestId: 30}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Page(ctx, 3, &rankpb.GetRankPageRequest{Board: page.Board}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("隐藏榜仍可查: %v", err)
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rank_mail_reward").Scan(&count); err != nil || count != 2 {
		t.Fatal("隐藏不得删除奖励意图")
	}
}
func TestTiDBFreezeFailureRollsBackEverySnapshotAndIntent(t *testing.T) {
	ctx, db, next := rankFixture(t)
	r := New(db, next, next, func(context.Context) (uint64, error) { return 999, nil }, func(uint32) bool { return true })
	board := testOpen(t, ctx, r)
	_, err := r.Submit(ctx, "match", &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceCursor: 1, RequestId: 10, Mode: rankpb.RankSubmitMode_RANK_SUBMIT_SET, Entries: []*rankpb.RankScoreInput{{EntityId: 7, Score: 100, AchievedMs: 1}, {EntityId: 8, Score: 90, AchievedMs: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	freeze := &rankpb.FreezeRankGenerationRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, ExpectedSourceCursor: 1, RequestId: 20, TopN: 2, Rewards: []*rankpb.RankRewardTier{{FirstRank: 1, LastRank: 2, Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 10}}}}}}
	if _, err = r.Freeze(ctx, "match", freeze); err == nil {
		t.Fatal("第二条奖励ID碰撞应使整个seal回滚")
	}
	for _, table := range []string{"rank_settlement", "rank_snapshot", "rank_mail_reward"} {
		var count int
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("半结算泄漏%s count%d err%v", table, count, err)
		}
	}
	var state uint32
	if err = db.QueryRowContext(ctx, "SELECT state FROM rank_board WHERE board_id=?", board.BoardId).Scan(&state); err != nil || state != 1 {
		t.Fatalf("未完整提交不得封榜: %v %d", err, state)
	}
	fixed := New(db, next, next, next, func(uint32) bool { return true })
	if _, err = fixed.Freeze(ctx, "match", freeze); err != nil {
		t.Fatalf("回滚后相同请求可完成: %v", err)
	}
}

func independentRankPool(t *testing.T, ctx context.Context, original *sql.DB, fixture *sqltest.Fixture) *sql.DB {
	t.Helper()
	var database string
	require.NoError(t, original.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database))
	require.Regexp(t, regexp.MustCompile(`^rank_it_[0-9a-f]{16}$`), database)
	address := os.Getenv("MAIL_RANK_TEST_TIDB")
	require.NoError(t, rankDataFixtureEndpoint(address))
	db, err := fixture.Open(ctx, database, 8)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	first, err := original.Conn(ctx)
	require.NoError(t, err)
	defer first.Close()
	second, err := db.Conn(ctx)
	require.NoError(t, err)
	defer second.Close()
	var firstID, secondID uint64
	require.NoError(t, first.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&firstID))
	require.NoError(t, second.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&secondID))
	require.NotEqual(t, firstID, secondID, "副本不能复用同一物理连接")
	return db
}

func TestTiDBRankIndependentPoolsReplayConflictFreezeAndRewardFence(t *testing.T) {
	ctx, db, next, fixture := rankFixtureWithWriter(t)
	peerDB := independentRankPool(t, ctx, db, fixture)
	first := New(db, next, next, next, func(uint32) bool { return true })
	second := New(peerDB, next, next, next, func(uint32) bool { return true })
	// 固定业务时钟，不靠真实等待驱动租约过期。
	first.now = func() time.Time { return time.UnixMilli(2000000) }
	second.now = first.now
	replicas := []*Repo{first, second}
	board := testOpen(t, ctx, first)
	input := &rankpb.SubmitRankScoresRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, SourceCursor: 1, RequestId: 10, Mode: rankpb.RankSubmitMode_RANK_SUBMIT_INCREMENT, Entries: []*rankpb.RankScoreInput{{EntityId: 7, Score: 100, AchievedMs: 1}}}
	concurrent := func(operation func(int)) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; operation(i) }()
		}
		close(start)
		wg.Wait()
	}
	var submitted [2]*rankpb.SubmitRankScoresResponse
	var failures [2]error
	concurrent(func(i int) {
		submitted[i], failures[i] = replicas[i].Submit(ctx, "match", proto.Clone(input).(*rankpb.SubmitRankScoresRequest))
	})
	for _, err := range failures {
		require.NoError(t, err)
	}
	require.NotEqual(t, submitted[0].Duplicated, submitted[1].Duplicated, "同游标增量只能首次提交一次")
	for _, mutate := range []func(*rankpb.SubmitRankScoresRequest){
		func(in *rankpb.SubmitRankScoresRequest) { in.Entries[0].Score++ },
		func(in *rankpb.SubmitRankScoresRequest) { in.Generation++ },
		func(in *rankpb.SubmitRankScoresRequest) { in.SourceEpoch-- },
	} {
		changed := proto.Clone(input).(*rankpb.SubmitRankScoresRequest)
		mutate(changed)
		_, err := second.Submit(ctx, "match", changed)
		require.ErrorIs(t, err, ErrConflict, "另一副本不能替换原件或使用旧代次写者")
	}
	pageRequest := &rankpb.GetRankPageRequest{Board: &rankpb.RankBoardKey{BoardType: 1, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "season-1", Generation: 1}, Page: 1, PageSize: 10}
	page, err := second.Page(ctx, 7, pageRequest)
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.EqualValues(t, 100, page.Entries[0].Score, "跨副本回放不能重复累加")
	concurrent(func(i int) {
		changed := proto.Clone(input).(*rankpb.SubmitRankScoresRequest)
		changed.SourceCursor, changed.RequestId, changed.Entries[0].Score = 2, 11, int64(100+i)
		_, failures[i] = replicas[i].Submit(ctx, "match", changed)
	})
	accepted, rejected := 0, 0
	for _, err := range failures {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, ErrConflict)
			rejected++
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, 1, rejected)
	page, err = second.Page(ctx, 7, pageRequest)
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.Contains(t, []int64{200, 201}, page.Entries[0].Score)
	freeze := &rankpb.FreezeRankGenerationRequest{BoardId: board.BoardId, Generation: 1, SourceEpoch: 11, ExpectedSourceCursor: 2, RequestId: 20, TopN: 1, Rewards: []*rankpb.RankRewardTier{{FirstRank: 1, LastRank: 1, Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 10}}}}}}
	var frozen [2]*rankpb.FreezeRankGenerationResponse
	concurrent(func(i int) {
		frozen[i], failures[i] = replicas[i].Freeze(ctx, "match", proto.Clone(freeze).(*rankpb.FreezeRankGenerationRequest))
	})
	for _, err := range failures {
		require.NoError(t, err)
	}
	require.NotEqual(t, frozen[0].Duplicated, frozen[1].Duplicated)
	require.Equal(t, frozen[0].SettlementId, frozen[1].SettlementId)
	require.NoError(t, db.Close())
	recovered := New(independentRankPool(t, ctx, peerDB), next, next, next, func(uint32) bool { return true })
	recovered.now = second.now
	replayed, err := recovered.Freeze(ctx, "match", freeze)
	require.NoError(t, err)
	require.True(t, replayed.Duplicated)
	require.Equal(t, frozen[0].SettlementId, replayed.SettlementId, "原池退出后仍恢复同一终态")
	late := proto.Clone(input).(*rankpb.SubmitRankScoresRequest)
	late.SourceCursor, late.RequestId = 3, 12
	_, err = recovered.Submit(ctx, "match", late)
	require.ErrorIs(t, err, ErrClosed)
	ids, err := recovered.DueRewards(ctx, 2000001, 10)
	require.NoError(t, err)
	require.Len(t, ids, 1, "并发封榜只创建一份奖励意图")
	replicas = []*Repo{second, recovered}
	var claimed [2]*rankpb.RankMailRewardRecord
	var won [2]bool
	concurrent(func(i int) {
		claimed[i], won[i], failures[i] = replicas[i].ClaimReward(ctx, ids[0], 2000001, 2000100, uint64(71+i))
	})
	for _, err := range failures {
		require.NoError(t, err)
	}
	require.NotEqual(t, won[0], won[1], "同一奖励租约只能有一个工作器赢得")
	winner := 0
	if won[1] {
		winner = 1
	}
	current, ok, err := replicas[1-winner].ClaimReward(ctx, ids[0], 2000101, 2000200, 88)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorIs(t, replicas[winner].SettleReward(ctx, claimed[winner], 501, 0, 2000102), ErrConflict, "旧租约的迟到写必须拒绝")
	require.NoError(t, replicas[1-winner].SettleReward(ctx, current, 501, 0, 2000103))
	ids, err = recovered.DueRewards(ctx, 2000300, 10)
	require.NoError(t, err)
	require.Empty(t, ids)
}

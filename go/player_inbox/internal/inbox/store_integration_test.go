//go:build integration

package inbox

// 真TiDB集成测试:go test -tags integration ./internal/inbox/... -count=1 -v
// 环境变量MYSQL字样仅保留历史名称兼容，连接必须是数值loopback端点且通过TiDB强读探针。
// 本任务实证端点为127.0.0.1:24000，运行前仍须核对Docker任务标签与端口归属。
// 独立目录P与完整admission均为此一次性测试库内的真实SQL夹具，不是生产授权替身。
//
// PLAYER_INBOX_TEST_MYSQL_DSN 为空则整组 Skip。⚠ 只能指向**一次性**测试库:每个用例都会 DROP 并重建
// player_inbox_stream / player_inbox_item / player_inbox_audit / schema_migrations。DSN 必须带库名,
// 且库名必须是 player_inbox_it 或 player_inbox_it_<pid>_<n>(白名单 inboxTestDBNamePattern,DROP 之前断言,评审 L14);
// 指向 mmorpg_player_inbox 等任何别的库都直接判红,不会动它的表。
//
// 账号:deploy/mysql-init/00_init_zone_dbs.sql 只给 appuser 授了 mmorpg_player_inbox 等业务库,**没有** player_inbox_it,
// 也没有全局 CREATE 权限 —— 所以测试库要用 root 先建好(`CREATE DATABASE IF NOT EXISTS player_inbox_it`),DSN 也用 root:
//   root:<root 密码>@tcp(127.0.0.1:24000)/player_inbox_it?parseTime=true&charset=utf8mb4
// (或由 root 另行 `GRANT ALL ON player_inbox_it.* TO 'appuser'@'%'` 之后改用 appuser;密码不要写进任何被跟踪的文件。)
// 表结构先经schemamigrate.Up按proto建出，强读池逐连接清历史快照并固定悲观RC/leader/strict。
//
// # 验收开关 PLAYER_INBOX_REQUIRE_MYSQL_TESTS(与 go/trade 的 TRADE_REQUIRE_MYSQL_TESTS 同一口径)
//
// 它不是第二个门控,只决定「跳过算不算失败」:设了之后 DSN 为空 → 判红,不再整组静默 Skip。验收一律这样跑
// (PowerShell,工作目录 go/player_inbox;密码见 deploy/docker-compose.yml,不要写进任何被跟踪的文件):
//
//   $env:PLAYER_INBOX_TEST_MYSQL_DSN='root:<root 密码>@tcp(127.0.0.1:24000)/player_inbox_it?parseTime=true&charset=utf8mb4'
//   $env:PLAYER_INBOX_REQUIRE_MYSQL_TESTS='1'
//   go test -tags integration ./internal/inbox/... -count=1 -v
//   go test -tags integration ./internal/inbox/... -run 'Concurrent|Race' -count=20
//
// ⚠ 不带 -tags integration 时本文件不参与编译,go test 照样输出 ok —— 验收必须带标签,并在 -v 输出里逐条看到 PASS。

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/data"
	"player_inbox/internal/metrics"

	battlepb "proto/battle"
	base "proto/common/base"
	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"

	"schemamigrate"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

const requireMySQLEnv = "PLAYER_INBOX_REQUIRE_MYSQL_TESTS"

type itClock struct{ ms atomic.Int64 }

func (c *itClock) Now() time.Time    { return time.UnixMilli(c.ms.Load()) }
func (c *itClock) set(ms uint64)     { c.ms.Store(int64(ms)) }
func (c *itClock) advance(ms uint64) { c.ms.Add(int64(ms)) }

type itPlayers struct {
	missing map[uint64]bool
	db      *sql.DB
}

func (p itPlayers) PlayerExists(ctx context.Context, id uint64) (bool, error) {
	if p.missing[id] {
		return false, nil
	}
	var existing uint64
	err := p.db.QueryRowContext(ctx, "SELECT player_id FROM account_role WHERE player_id=?", id).Scan(&existing)
	if err == nil {
		err = ensureInboxFixtureRoute(ctx, p.db, id)
		return err == nil, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = p.db.ExecContext(ctx, "INSERT INTO account_role(player_id,account,owner_revision,trade_listing_id) VALUES(?,?,1,0) ON DUPLICATE KEY UPDATE player_id=player_id", id, "inbox-fixture-"+strconv.FormatUint(id, 10))
	if err == nil {
		err = ensureInboxFixtureRoute(ctx, p.db, id)
	}
	return err == nil, err
}

type itEpochs struct {
	mu  sync.Mutex
	m   map[uint64]uint64
	err error
}

func (e *itEpochs) OwnerEpoch(_ context.Context, id uint64) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return 0, e.err
	}
	return e.m[id], nil
}

func (e *itEpochs) set(id, epoch uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.m[id] = epoch
}

type itNotifier struct {
	mu     sync.Mutex
	result NotifyResult
	calls  int
}

func (n *itNotifier) Notify(context.Context, uint64, inboxpb.PlayerInboxDomain) NotifyResult {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	return n.result
}

func (n *itNotifier) setResult(r NotifyResult) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.result = r
}

type itEnv struct {
	db       *sql.DB
	store    *Store
	clock    *itClock
	epochs   *itEpochs
	notifier *itNotifier
	missing  map[uint64]bool
}

func TestIntegrationBattleSettlementKeepsPermanentOriginalUntilApplied(t *testing.T) {
	e := openIntegration(t)
	ctx := itCtx(t)
	const player = uint64(8911)
	e.epochs.set(player, 7)
	payload := marshal(t, &battlepb.BattleSettlementData{PlayerId: player, BattleId: 9123, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, GoldGain: 5})
	r := &inboxpb.BattleSettlementInboxRequest{PlayerId: player, BattleId: 9123, ExpectedOwnerAccount: "inbox-fixture-8911", ExpectedOwnerRevision: 1, IssuedAtMs: testNowMs - uint64((8 * 24 * time.Hour).Milliseconds()), SettlementPayload: payload}
	first, err := e.store.EnqueueBattleSettlement(ctx, r)
	if err != nil || first.GetErrorMessage().GetId() != 0 || first.Seq == 0 {
		t.Fatalf("可信老来源必须可入箱: %+v %v", first, err)
	}
	again, err := e.store.EnqueueBattleSettlement(ctx, r)
	if err != nil || again.Seq != first.Seq {
		t.Fatalf("永久原收据必须同seq: %+v %v", again, err)
	}
	other := proto.Clone(r).(*inboxpb.BattleSettlementInboxRequest)
	other.BattleId++
	other.SettlementPayload = marshal(t, &battlepb.BattleSettlementData{PlayerId: player, BattleId: other.BattleId, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW})
	full, err := e.store.EnqueueBattleSettlement(ctx, other)
	if err != nil || full.GetErrorMessage().GetId() != constants.ErrFull {
		t.Fatalf("未确认原条必须占单槽: %+v %v", full, err)
	}
	domain := inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT
	identity := e.admission(t, player, 7)
	fetched, err := e.store.Fetch(ctx, &inboxpb.FetchPlayerInboxRequest{PlayerId: player, Domain: domain, OwnerEpoch: 7, MaxItems: 1, Admission: identity})
	if err != nil || len(fetched.GetItems()) != 1 || fetched.Items[0].Seq != first.Seq {
		t.Fatalf("按原入场交付: %+v %v", fetched, err)
	}
	for _, outcome := range []inboxpb.PlayerInboxOutcome{inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_REJECTED, inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_SKIPPED, inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_UNKNOWN} {
		ack, err := e.store.Ack(ctx, &inboxpb.AckPlayerInboxRequest{PlayerId: player, Domain: domain, OwnerEpoch: 7, Admission: identity, Decisions: []*inboxpb.PlayerInboxDecisionReport{dec(first.Seq, outcome, 0, r.BattleId)}})
		if err != nil || len(ack.GetResults()) != 1 || ack.Results[0].State != statePending {
			t.Fatalf("非APPLIED不得终结: %+v %v", ack, err)
		}
	}
	void, err := e.store.Void(ctx, VoidParams{PlayerID: player, Domain: domain, Seq: first.Seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED, Operator: "fixture"}, nil)
	if err != nil || void.GetErrorMessage().GetId() != constants.ErrItemStateConflict {
		t.Fatalf("通用VOID不得取消奖励: %+v %v", void, err)
	}
	ack, err := e.store.Ack(ctx, &inboxpb.AckPlayerInboxRequest{PlayerId: player, Domain: domain, OwnerEpoch: 7, Admission: identity, Decisions: []*inboxpb.PlayerInboxDecisionReport{dec(first.Seq, inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_APPLIED, 0, r.BattleId)}})
	if err != nil || len(ack.GetResults()) != 1 || ack.Results[0].State != stateApplied {
		t.Fatalf("真实APPLIED可终结: %+v %v", ack, err)
	}
	lookup, err := e.store.LookupBattleSettlement(ctx, r)
	if err != nil || lookup.State != stateApplied || lookup.Seq != first.Seq {
		t.Fatalf("永久SQL原收据必须保全: %+v %v", lookup, err)
	}
	// 模拟消费环已滚出及远超GM七天窗：SQL永久原件仍能重放同一序号，不新增待项。
	e.clock.advance(uint64((40 * 24 * time.Hour).Milliseconds()))
	again, err = e.store.EnqueueBattleSettlement(ctx, r)
	if err != nil || again.State != stateApplied || again.Seq != first.Seq {
		t.Fatalf("老来源不可复活/换seq: %+v %v", again, err)
	}
	if _, err = e.db.ExecContext(ctx, "UPDATE account_role SET account='new-owner',owner_revision=2 WHERE player_id=?", player); err != nil {
		t.Fatal(err)
	}
	if _, err = e.store.LookupBattleSettlement(ctx, r); err == nil {
		t.Fatal("换主后不能以旧来源补权")
	}
}

func openIntegration(t *testing.T) *itEnv {
	t.Helper()
	dsn := os.Getenv("PLAYER_INBOX_TEST_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv(requireMySQLEnv) != "" {
			t.Fatalf("%s 已设置(验收模式),但 PLAYER_INBOX_TEST_MYSQL_DSN 为空", requireMySQLEnv)
		}
		t.Skip("PLAYER_INBOX_TEST_MYSQL_DSN 未设置,跳过收件箱存储集成测试(只能指向一次性库)")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("测试DSN无效")
	}
	if !inboxTestDBNamePattern.MatchString(parsed.DBName) {
		t.Fatal("测试DSN目标不是一次性Inbox白名单库")
	}
	host, portText, addrErr := net.SplitHostPort(parsed.Addr)
	port, portErr := strconv.ParseUint(portText, 10, 16)
	address := net.ParseIP(host)
	if parsed.Net != "tcp" || addrErr != nil || portErr != nil || port == 0 || address == nil || !address.IsLoopback() {
		t.Fatal("全球Inbox原生SQL夹具只允许数值loopback的合法TiDB端口")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	dbName := "player_inbox_it_" + strconv.FormatUint(binary.LittleEndian.Uint64(nonce[:]), 10) + "_1"
	opts, err := sqltest.OptionsFromEnv(dsn, dbName+"_writer", dbName)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	db, err := fixture.OpenSchema(ctx, dbName)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS account_role_admission",
		"DROP TABLE IF EXISTS account_role",
		"DROP TABLE IF EXISTS player_inbox_audit",
		"DROP TABLE IF EXISTS player_inbox_item",
		"DROP TABLE IF EXISTS player_inbox_stream",
		"DROP TABLE IF EXISTS schema_migrations",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	report, err := schemamigrate.Up(ctx, db, schemamigrate.Options{Database: dbName, Tables: data.Tables(), Logf: t.Logf})
	if err != nil {
		t.Fatalf("schemamigrate.Up: %v", err)
	}
	if len(report.Manual) != 0 {
		t.Fatalf("新建库不应出现需人工项: %v", report.Manual)
	}
	for _, statement := range []string{
		"CREATE TABLE account_role(player_id BIGINT UNSIGNED PRIMARY KEY,account VARBINARY(191) NOT NULL,owner_revision BIGINT UNSIGNED NOT NULL,trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0)",
		"CREATE TABLE account_role_admission(player_id BIGINT UNSIGNED NOT NULL,scene_instance_id VARBINARY(36) NOT NULL,highest_generation BIGINT UNSIGNED NOT NULL,closed_through BIGINT UNSIGNED NOT NULL,identity_proto MEDIUMBLOB NOT NULL,PRIMARY KEY(player_id,scene_instance_id))",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = createInboxFixtureRouteTables(ctx, db, dbName); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantProtoTables(ctx, dbName, data.Tables()); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantTables(ctx, dbName, "account_role", "account_role_admission", "route_meta", "player_route", "zone_merged_into", "route_reconcile"); err != nil {
		t.Fatal(err)
	}
	db, err = fixture.Open(ctx, dbName, 64)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := NewSQLRoleAuthority(ctx, db, dbName, authoritysql.BackendTiDB)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	env := &itEnv{
		db:       db,
		clock:    &itClock{},
		epochs:   &itEpochs{m: map[uint64]uint64{}},
		notifier: &itNotifier{result: NotifySent},
		missing:  map[uint64]bool{},
	}
	env.clock.set(testNowMs)
	env.store, err = NewStore(StoreDeps{
		DB:       db,
		Registry: reg,
		Allowed:  allTypesAllowed(),
		Players:  itPlayers{missing: env.missing, db: db},
		Epochs:   env.epochs,
		Notifier: env.notifier,
		Now:      env.clock.Now,
		Roles:    roles,
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func itCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func gainPayload(t *testing.T, currency uint32) []byte {
	return marshal(t, &inboxpb.GmCurrencyGainPayload{CurrencyType: currency})
}

// enqueue 用 GM 封锁类型入箱,同事务写审计。
func (e *itEnv) enqueue(t *testing.T, playerID, requestID, issuedAt uint64, payload []byte) (EnqueueResult, error) {
	ctx := itCtx(t)
	return e.store.Enqueue(ctx, EnqueueParams{
		PlayerID:   playerID,
		Type:       gmBlock,
		Producer:   inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM,
		RequestID:  requestID,
		IssuedAtMs: issuedAt,
		Payload:    payload,
		Operator:   "ops_it",
		Note:       "it",
	}, func(tx *sql.Tx, seq uint64) error {
		return InsertAudit(ctx, tx, &inboxpb.PlayerInboxAudit{
			PlayerId: playerID, Domain: uint32(gmDomain), Seq: seq,
			Action:   uint32(inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_SANCTION),
			Operator: "ops_it", PeerAddr: "127.0.0.1:1", Reason: "it", CreatedMs: testNowMs,
		})
	})
}

func (e *itEnv) mustEnqueue(t *testing.T, playerID, requestID uint64) EnqueueResult {
	t.Helper()
	res, err := e.enqueue(t, playerID, requestID, uint64(e.clock.ms.Load()), gainPayload(t, 1))
	if err != nil || res.Code != 0 || !res.Created {
		t.Fatalf("enqueue player=%d request=%d: res=%+v err=%v", playerID, requestID, res, err)
	}
	return res
}

func (e *itEnv) stream(t *testing.T, playerID uint64) *inboxpb.PlayerInboxStream {
	t.Helper()
	st, err := readStream(itCtx(t), e.db, RowShard(playerID), playerID, gmDomain)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *itEnv) item(t *testing.T, playerID, seq uint64) *inboxpb.PlayerInboxItemRow {
	t.Helper()
	it, err := readItem(itCtx(t), e.db, RowShard(playerID), playerID, gmDomain, seq)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *itEnv) fetch(t *testing.T, playerID, epoch, decidedThrough uint64, maxItems uint32) *inboxpb.FetchPlayerInboxResponse {
	t.Helper()
	resp, err := e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{
		PlayerId: playerID, Domain: gmDomain, OwnerEpoch: epoch, CorrelationId: 99,
		MaxItems: maxItems, ConsumerDecidedThrough: decidedThrough,
		Admission: e.admission(t, playerID, epoch),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.GetPlayerId() != playerID || resp.GetCorrelationId() != 99 || resp.GetDomain() != gmDomain {
		t.Fatalf("回显字段不对: %+v", resp)
	}
	return resp
}

func (e *itEnv) ack(t *testing.T, playerID, epoch uint64, reports ...*inboxpb.PlayerInboxDecisionReport) *inboxpb.AckPlayerInboxResponse {
	t.Helper()
	resp, err := e.store.Ack(itCtx(t), &inboxpb.AckPlayerInboxRequest{
		PlayerId: playerID, Domain: gmDomain, OwnerEpoch: epoch, CorrelationId: 7, Decisions: reports,
		Admission: e.admission(t, playerID, epoch),
	})
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	return resp
}

// 用真实SQL注册完整入场原件；不把目录替身/E整数当作消费授权。
func (e *itEnv) admission(t *testing.T, player, epoch uint64) *base.RoleAdmissionIdentity {
	t.Helper()
	if epoch == 0 {
		return nil
	}
	if _, err := (itPlayers{missing: map[uint64]bool{}, db: e.db}).PlayerExists(itCtx(t), player); err != nil {
		t.Fatal(err)
	}
	var revision uint64
	if err := e.db.QueryRow("SELECT owner_revision FROM account_role WHERE player_id=?", player).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	identity := &base.RoleAdmissionIdentity{PlayerId: player, SceneInstanceId: fmt.Sprintf("10000000-0000-4000-8000-%012x", epoch), Generation: 1, OwnerRevision: revision, OwnerEpoch: epoch, SceneNodeId: "7", SceneId: 99, ZoneId: 1, SessionId: 1, LoginGeneration: 1}
	raw, err := proto.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.db.Exec("INSERT INTO account_role_admission VALUES(?,?,1,0,?) ON DUPLICATE KEY UPDATE identity_proto=identity_proto", player, []byte(identity.SceneInstanceId), raw)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func dec(seq uint64, outcome inboxpb.PlayerInboxOutcome, reason uint32, requestID uint64) *inboxpb.PlayerInboxDecisionReport {
	return &inboxpb.PlayerInboxDecisionReport{Seq: seq, Outcome: outcome, Reason: reason, RequestId: requestID}
}

func auditCount(t *testing.T, db *sql.DB, playerID uint64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(itCtx(t), "SELECT COUNT(*) FROM player_inbox_audit WHERE `shard` = ? AND `player_id` = ?",
		RowShard(playerID), playerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ── 入箱 ─────────────────────────────────────────────────────────────────

func TestIntegrationEnqueueCreateDuplicateConflict(t *testing.T) {
	e := openIntegration(t)
	const pid, rid = uint64(1001), uint64(5001)
	issued := testNowMs - 1000

	res, err := e.enqueue(t, pid, rid, issued, gainPayload(t, 1))
	if err != nil || res.Code != 0 || !res.Created {
		t.Fatalf("首次入箱: %+v %v", res, err)
	}
	if res.Seq != testNowMs<<12 {
		t.Fatalf("seq = %d, want now<<12 = %d", res.Seq, testNowMs<<12)
	}
	st := e.stream(t, pid)
	if st.GetNextSeq() != res.Seq+1 || st.GetOpenCount() != 1 {
		t.Fatalf("流行 = %+v", st)
	}
	if st.GetNotifyDueMs() != 0 {
		t.Fatalf("提醒送达后 notify_due_ms 应以 next_seq CAS 清零,got %d", st.GetNotifyDueMs())
	}

	// 原样重试 → DUPLICATE(原 seq、当前状态),不新增行与审计。
	dup, err := e.enqueue(t, pid, rid, issued, gainPayload(t, 1))
	if err != nil || dup.Code != 0 || dup.Created || dup.Seq != res.Seq || dup.State != statePending {
		t.Fatalf("重试应 DUPLICATE: %+v %v", dup, err)
	}
	// 刷新 issued_at 重试 → CONFLICT(看得见的错误,不会静默重复入箱)。
	conflict, err := e.enqueue(t, pid, rid, issued+1, gainPayload(t, 1))
	if err != nil || conflict.Code != constants.ErrIdempotencyConflict {
		t.Fatalf("issued_at 不同应 CONFLICT: %+v %v", conflict, err)
	}
	// 载荷不同 → CONFLICT。
	conflict, err = e.enqueue(t, pid, rid, issued, gainPayload(t, 2))
	if err != nil || conflict.Code != constants.ErrIdempotencyConflict {
		t.Fatalf("载荷不同应 CONFLICT: %+v %v", conflict, err)
	}
	if n := auditCount(t, e.db, pid); n != 1 {
		t.Fatalf("审计行 = %d, want 1(只有新建那次)", n)
	}
	if st := e.stream(t, pid); st.GetOpenCount() != 1 {
		t.Fatalf("重试不应改 open_count: %+v", st)
	}
}

func TestIntegrationEnqueueRejectionsWriteNothing(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(1002)
	e.missing[1003] = true
	ctx := itCtx(t)
	cases := []struct {
		name string
		p    EnqueueParams
		want uint32
	}{
		{"未知类型", EnqueueParams{PlayerID: pid, Type: inboxpb.PlayerInboxMessageType(99), RequestID: 1, IssuedAtMs: testNowMs, Operator: "o"}, constants.ErrUnknownType},
		{"玩家不存在", EnqueueParams{PlayerID: 1003, Type: gmBlock, RequestID: 2, IssuedAtMs: testNowMs, Operator: "o"}, constants.ErrPlayerNotFound},
		{"operator 超长", EnqueueParams{PlayerID: pid, Type: gmBlock, RequestID: 3, IssuedAtMs: testNowMs, Operator: strings.Repeat("x", 65)}, constants.ErrTextTooLong},
		{"note 超长", EnqueueParams{PlayerID: pid, Type: gmBlock, RequestID: 4, IssuedAtMs: testNowMs, Operator: "o", Note: strings.Repeat("n", 257)}, constants.ErrTextTooLong},
		{"签发太早", EnqueueParams{PlayerID: pid, Type: gmBlock, RequestID: 5, IssuedAtMs: testNowMs - uint64(constants.IssuedAtMaxPast.Milliseconds()) - 1, Operator: "o"}, constants.ErrIssuedOutOfWindow},
		{"签发在未来", EnqueueParams{PlayerID: pid, Type: gmBlock, RequestID: 6, IssuedAtMs: testNowMs + uint64(constants.IssuedAtMaxFuture.Milliseconds()) + 1, Operator: "o"}, constants.ErrIssuedOutOfWindow},
		{"request_id 为 0", EnqueueParams{PlayerID: pid, Type: gmBlock, RequestID: 0, IssuedAtMs: testNowMs, Operator: "o"}, constants.ErrInvalidParameter},
	}
	for _, tc := range cases {
		tc.p.Producer = inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM
		res, err := e.store.Enqueue(ctx, tc.p, nil)
		if err != nil || res.Code != tc.want {
			t.Errorf("%s: res=%+v err=%v, want code %d", tc.name, res, err, tc.want)
		}
	}
	if st := e.stream(t, pid); st != nil {
		t.Fatalf("被拒的入箱不应建流行: %+v", st)
	}
}

func TestIntegrationEnqueueFullKeepsDuplicates(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(1004)
	spec, _ := e.store.registry.Domain(gmDomain)
	for i := uint64(1); i <= uint64(spec.GetMaxPending()); i++ {
		e.mustEnqueue(t, pid, i)
	}
	res, err := e.enqueue(t, pid, 9999, testNowMs, gainPayload(t, 1))
	if err != nil || res.Code != constants.ErrFull {
		t.Fatalf("满箱应 FULL: %+v %v", res, err)
	}
	// 满箱时原请求的重试仍是 DUPLICATE,不是 FULL。
	dup, err := e.enqueue(t, pid, 1, testNowMs, gainPayload(t, 1))
	if err != nil || dup.Code != 0 || dup.Created {
		t.Fatalf("满箱时重试应 DUPLICATE: %+v %v", dup, err)
	}
}

// TestIntegrationRaceRetryGetsDuplicateNotFull:差一条满箱时原请求与重试赛跑,输的一方必须拿到 DUPLICATE(设计 §6.2 7.2、§8 #2)。
func TestIntegrationRaceRetryGetsDuplicateNotFull(t *testing.T) {
	e := openIntegration(t)
	spec, _ := e.store.registry.Domain(gmDomain)
	for round := uint64(0); round < 5; round++ {
		pid := 2000 + round
		for i := uint64(1); i < uint64(spec.GetMaxPending()); i++ {
			e.mustEnqueue(t, pid, i)
		}
		const raceRID = uint64(777)
		payload := gainPayload(t, 1) // 在测试 goroutine 里序列化:子 goroutine 不调 t.Fatal
		var wg sync.WaitGroup
		results := make([]EnqueueResult, 2)
		errs := make([]error, 2)
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				results[g], errs[g] = e.enqueue(t, pid, raceRID, testNowMs, payload)
			}(g)
		}
		wg.Wait()
		created, dup := 0, 0
		for g := 0; g < 2; g++ {
			if errs[g] != nil {
				t.Fatalf("round %d: %v", round, errs[g])
			}
			switch {
			case results[g].Code == 0 && results[g].Created:
				created++
			case results[g].Code == 0 && !results[g].Created:
				dup++
			default:
				t.Fatalf("round %d: 赛跑一方拿到 code=%d(不应是 FULL)", round, results[g].Code)
			}
		}
		if created != 1 || dup != 1 {
			t.Fatalf("round %d: created=%d dup=%d, want 1/1", round, created, dup)
		}
	}
}

// TestIntegrationConcurrentEnqueueSeqContiguous:同一毫秒并发入箱,seq 连续无空洞、无重复,next_seq / open_count 一致。
func TestIntegrationConcurrentEnqueueSeqContiguous(t *testing.T) {
	e := openIntegration(t)
	const pid, n = uint64(1005), 20
	payload := gainPayload(t, 1)
	var wg sync.WaitGroup
	seqs := make([]uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.enqueue(t, pid, uint64(100+i), testNowMs, payload)
			if err != nil || res.Code != 0 || !res.Created {
				t.Errorf("goroutine %d: %+v %v", i, res, err)
				return
			}
			seqs[i] = res.Seq
		}(i)
	}
	wg.Wait()
	sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
	floor := testNowMs << 12
	for i, s := range seqs {
		if s != floor+uint64(i) {
			t.Fatalf("seqs = %v, want %d..%d 连续", seqs, floor, floor+n-1)
		}
	}
	if st := e.stream(t, pid); st.GetNextSeq() != floor+n || st.GetOpenCount() != n {
		t.Fatalf("流行 = %+v", st)
	}
}

// ── Fetch ────────────────────────────────────────────────────────────────

func TestIntegrationFetchNoStreamAndFastPathWritesNothing(t *testing.T) {
	e := openIntegration(t)
	if resp := e.fetch(t, 3001, 1, 0, 0); resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_NO_STREAM {
		t.Fatalf("无流应 NO_STREAM: %+v", resp)
	}

	const pid = uint64(3002)
	if err := e.store.ensureStream(itCtx(t), RowShard(pid), pid, gmDomain, testNowMs); err != nil {
		t.Fatal(err)
	}
	before := e.stream(t, pid)
	e.epochs.set(pid, 5)
	e.clock.advance(10_000)
	resp := e.fetch(t, pid, 5, 0, 0)
	if resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_FIRST || len(resp.GetItems()) != 0 {
		t.Fatalf("快路径应按快照判 FIRST 且无条目: %+v", resp)
	}
	after := e.stream(t, pid)
	if !proto.Equal(before, after) {
		t.Fatalf("快路径不得写任何东西:before=%+v after=%+v", before, after)
	}
}

func TestIntegrationFetchLockedPathDeliversAndFences(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(3003)
	var seqs []uint64
	for i := uint64(1); i <= 3; i++ {
		seqs = append(seqs, e.mustEnqueue(t, pid, i).Seq)
	}
	e.epochs.set(pid, 5)

	resp := e.fetch(t, pid, 5, 0, 2)
	if resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_FIRST || !resp.GetHasMore() || len(resp.GetItems()) != 2 {
		t.Fatalf("第一页: %+v", resp)
	}
	if resp.GetItems()[0].GetSeq() != seqs[0] || resp.GetItems()[1].GetSeq() != seqs[1] {
		t.Fatalf("应按 seq 升序: %+v", resp.GetItems())
	}
	if it := resp.GetItems()[0]; it.GetDeliveryCount() != 1 || len(it.GetPayload()) == 0 || it.GetRequestId() != 1 {
		t.Fatalf("条目字段: %+v", it)
	}
	if st := e.stream(t, pid); st.GetFenceEpoch() != 5 {
		t.Fatalf("FIRST 应写回 fence_epoch=5: %+v", st)
	}

	resp = e.fetch(t, pid, 5, 0, 0)
	if resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_MATCH || resp.GetHasMore() || len(resp.GetItems()) != 3 {
		t.Fatalf("第二次拉取: %+v", resp)
	}
	if got := e.item(t, pid, seqs[0]); got.GetDeliveryCount() != 2 || got.GetLastDeliveredEpoch() != 5 {
		t.Fatalf("投递计数: %+v", got)
	}
}

func TestIntegrationFetchFenceRejections(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(3004)
	e.mustEnqueue(t, pid, 1)
	e.epochs.set(pid, 5)
	e.fetch(t, pid, 5, 0, 0) // FIRST → fence = 5

	if resp := e.fetch(t, pid, 4, 0, 0); resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_STALE ||
		resp.GetErrorMessage().GetId() != constants.ErrStaleOwner || len(resp.GetItems()) != 0 {
		t.Fatalf("旧 epoch 应 STALE: %+v", resp)
	}
	if resp := e.fetch(t, pid, 0, 0, 0); resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_EPOCH_REQUIRED {
		t.Fatalf("epoch 0 应 EPOCH_REQUIRED: %+v", resp)
	}
	e.epochs.set(pid, 6)
	forged := e.admission(t, pid, 7)
	forged.Generation++
	if _, err := e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{PlayerId: pid, Domain: gmDomain, OwnerEpoch: 7, Admission: forged}); err == nil {
		t.Fatal("未登记原generation不得消费，不凭E大于Redis当前值授予权限")
	}
	if st := e.stream(t, pid); st.GetFenceEpoch() != 5 {
		t.Fatalf("拒绝结局不得改围栏: %+v", st)
	}
	// Redis故障不能推翻完整SQL原入场；真实SQL围栏仍单调推进。
	e.epochs.mu.Lock()
	e.epochs.err = errors.New("redis down")
	e.epochs.mu.Unlock()
	if _, err := e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{PlayerId: pid, Domain: gmDomain, OwnerEpoch: 6, Admission: e.admission(t, pid, 6)}); err != nil {
		t.Fatalf("SQL原入场不依赖Redis授权: %v", err)
	}
	if st := e.stream(t, pid); st.GetFenceEpoch() != 6 {
		t.Fatalf("SQL原入场应推进围栏: %+v", st)
	}
}

func TestIntegrationFetchCursorAheadRaisesNextSeq(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(3005)
	res := e.mustEnqueue(t, pid, 1)
	e.epochs.set(pid, 2)
	watermark := res.Seq + 1000
	resp := e.fetch(t, pid, 2, watermark, 0)
	if !FenceAllows(resp.GetFence()) {
		t.Fatalf("CursorAhead 仍应照常投递: %+v", resp)
	}
	if st := e.stream(t, pid); st.GetNextSeq() != watermark+1 {
		t.Fatalf("next_seq 应抬到水位+1 = %d,got %d", watermark+1, st.GetNextSeq())
	}
	next := e.mustEnqueue(t, pid, 2)
	if next.Seq <= watermark {
		t.Fatalf("抬号后新 seq %d 应大于水位 %d", next.Seq, watermark)
	}
}

// ── Ack ──────────────────────────────────────────────────────────────────

func TestIntegrationAckResults(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(4001)
	a := e.mustEnqueue(t, pid, 11).Seq
	b := e.mustEnqueue(t, pid, 12).Seq
	c := e.mustEnqueue(t, pid, 13).Seq
	e.epochs.set(pid, 3)
	e.fetch(t, pid, 3, 0, 2) // 只投递 a、b

	resp := e.ack(t, pid, 3,
		dec(a, outApplied, 0, 11),
		dec(b, outSkipped, 1, 12),     // b 没有指令 → SKIP_NOT_REQUESTED
		dec(c, outApplied, 0, 13),     // c 未投递 → NOT_DELIVERED
		dec(c+1000, outApplied, 0, 1), // 不存在 → UNKNOWN_SEQ
	)
	want := []inboxpb.PlayerInboxAckResult{
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_SKIP_NOT_REQUESTED,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_NOT_DELIVERED,
		inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_UNKNOWN_SEQ,
	}
	if len(resp.GetResults()) != len(want) {
		t.Fatalf("results = %+v", resp.GetResults())
	}
	for i, w := range want {
		if resp.GetResults()[i].GetResult() != w {
			t.Fatalf("results[%d] = %v, want %v", i, resp.GetResults()[i].GetResult(), w)
		}
	}
	if st := e.stream(t, pid); st.GetOpenCount() != 2 {
		t.Fatalf("只终结了 a,open_count 应为 2: %+v", st)
	}
	if it := e.item(t, pid, a); it.GetState() != uint32(stateApplied) || it.GetDecidedEpoch() != 3 ||
		it.GetFinalizedBy() != uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_CONSUMER) {
		t.Fatalf("a 终结字段: %+v", it)
	}

	resp = e.ack(t, pid, 3,
		dec(a, outApplied, 0, 11),  // 重复确认 → ALREADY_SAME
		dec(b, outApplied, 0, 999), // request_id 不一致 → MISMATCH
	)
	if resp.GetResults()[0].GetResult() != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_ALREADY_SAME ||
		resp.GetResults()[1].GetResult() != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_MISMATCH {
		t.Fatalf("第二轮: %+v", resp.GetResults())
	}
	resp = e.ack(t, pid, 3, dec(a, outRejected, 29003, 11)) // 终态不一致 → CONFLICT,永不翻转
	if resp.GetResults()[0].GetResult() != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_CONFLICT {
		t.Fatalf("应 CONFLICT: %+v", resp.GetResults())
	}
	if it := e.item(t, pid, a); it.GetState() != uint32(stateApplied) {
		t.Fatalf("CONFLICT 不得翻转终态: %+v", it)
	}
	// UNKNOWN → SETTLED_UNKNOWN。
	resp = e.ack(t, pid, 3, dec(b, outUnknown, 0, 12))
	if resp.GetResults()[0].GetResult() != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED ||
		resp.GetResults()[0].GetState() != stateUnknown {
		t.Fatalf("UNKNOWN 应收尾为 SETTLED_UNKNOWN: %+v", resp.GetResults())
	}
}

// TestIntegrationAckStaleAfterAdvance:新 owner 推进围栏之后,旧 owner 的 Ack 被判 STALE 且不改任何行(设计 §7.10)。
func TestIntegrationAckStaleAfterAdvance(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(4002)
	a := e.mustEnqueue(t, pid, 1).Seq
	e.epochs.set(pid, 5)
	e.fetch(t, pid, 5, 0, 0) // FIRST
	e.epochs.set(pid, 6)
	if resp := e.fetch(t, pid, 6, 0, 0); resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_ADVANCE {
		t.Fatalf("新 owner 应 ADVANCE: %+v", resp)
	}
	resp := e.ack(t, pid, 5, dec(a, outApplied, 0, 1))
	if resp.GetFence() != inboxpb.PlayerInboxFenceVerdict_PLAYER_INBOX_FENCE_STALE || len(resp.GetResults()) != 0 {
		t.Fatalf("旧 owner 的 Ack 应 STALE 且无逐条结果: %+v", resp)
	}
	if it := e.item(t, pid, a); it.GetState() != uint32(statePending) {
		t.Fatalf("STALE 不得终结: %+v", it)
	}
	if resp := e.ack(t, pid, 6, dec(a, outApplied, 0, 1)); resp.GetResults()[0].GetResult() != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED {
		t.Fatalf("新 owner 的 Ack 应 FINALIZED: %+v", resp)
	}
}

// TestIntegrationAckEpochBoundReadFailureChangesNothing:Ack 需要上界 R(req > fence)而 R 读失败时按故障返回,
// 行与流都不改(设计 §7.8「R 读失败不改任何状态」,§11.4 验收)。
func TestIntegrationAckRejectsUnregisteredAdmissionDespiteRedisFailure(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(4003)
	a := e.mustEnqueue(t, pid, 1).Seq
	e.epochs.set(pid, 5)
	e.fetch(t, pid, 5, 0, 0) // FIRST → fence = 5,a 已投递
	before := e.stream(t, pid)

	e.epochs.mu.Lock()
	e.epochs.err = errors.New("redis down")
	e.epochs.mu.Unlock()
	_, err := e.store.Ack(itCtx(t), &inboxpb.AckPlayerInboxRequest{
		PlayerId: pid, Domain: gmDomain, OwnerEpoch: 6, Decisions: []*inboxpb.PlayerInboxDecisionReport{dec(a, outApplied, 0, 1)},
		Admission: &base.RoleAdmissionIdentity{PlayerId: pid, OwnerEpoch: 6},
	})
	if err == nil {
		t.Fatal("不完整SQL原入场应拒绝(由服务层折成故障)")
	}
	if after := e.stream(t, pid); !proto.Equal(before, after) {
		t.Fatalf("R 读失败不得改流行:before=%+v after=%+v", before, after)
	}
	if it := e.item(t, pid, a); it.GetState() != uint32(statePending) {
		t.Fatalf("R 读失败不得终结条目: %+v", it)
	}
	// 不需要 R 的 MATCH 不受 Redis 故障影响。
	if resp := e.ack(t, pid, 5, dec(a, outApplied, 0, 1)); resp.GetResults()[0].GetResult() != inboxpb.PlayerInboxAckResult_PLAYER_INBOX_ACK_FINALIZED {
		t.Fatalf("MATCH 不读 R,应照常 FINALIZED: %+v", resp)
	}
}

// TestIntegrationCountScannedRows 钉住 fetch_scanned_rows 数的是锁路径走过的行(设计 §4.2):
// 终态行夹在 PENDING 行前面时也要数进去;读满 LIMIT 时只数到最后读出那一行。
func TestIntegrationCountScannedRows(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(4004)
	var seqs []uint64
	for i := uint64(1); i <= 4; i++ {
		seqs = append(seqs, e.mustEnqueue(t, pid, i).Seq)
	}
	e.epochs.set(pid, 1)
	e.fetch(t, pid, 1, 0, 2)
	e.ack(t, pid, 1, dec(seqs[0], outApplied, 0, 1), dec(seqs[1], outApplied, 0, 2)) // 前两行终态,后两行 PENDING
	ctx := itCtx(t)
	shard := RowShard(pid)

	if n, err := e.store.countScannedRows(ctx, shard, pid, gmDomain, 0, true); err != nil || n != 4 {
		t.Fatalf("整条流 = %d (%v), want 4(含两条终态行)", n, err)
	}
	if n, err := e.store.countScannedRows(ctx, shard, pid, gmDomain, seqs[2], false); err != nil || n != 3 {
		t.Fatalf("到 seqs[2] 为止 = %d (%v), want 3", n, err)
	}
	// 与 Fetch 的口径对上:LIMIT 2(max_items=1)读满时停在 seqs[3],走过 4 行;LIMIT 3 读不满 → 整条流。
	resp := e.fetch(t, pid, 1, 0, 1)
	if len(resp.GetItems()) != 1 || !resp.GetHasMore() || resp.GetItems()[0].GetSeq() != seqs[2] {
		t.Fatalf("max_items=1 应只投递 seqs[2] 且 has_more: %+v", resp)
	}
	if n, err := e.store.countScannedRows(ctx, shard, pid, gmDomain, seqs[3], false); err != nil || n != 4 {
		t.Fatalf("到 seqs[3] 为止 = %d (%v), want 4", n, err)
	}
}

// ── 作废 ─────────────────────────────────────────────────────────────────

func TestIntegrationVoid(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(5001)
	a := e.mustEnqueue(t, pid, 1).Seq
	b := e.mustEnqueue(t, pid, 2).Seq
	e.epochs.set(pid, 2)
	e.fetch(t, pid, 2, 0, 1) // 只投递 a
	ctx := itCtx(t)
	void := func(seq uint64) *inboxpb.VoidPlayerInboxItemResponse {
		resp, err := e.store.Void(ctx, VoidParams{
			PlayerID: pid, Domain: gmDomain, Seq: seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED,
			Operator: "ops_it", Note: "wrong target", PeerAddr: "127.0.0.1:2",
		}, nil) // 普通模式不读账本
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetForceVerdict() != inboxpb.PlayerInboxForceVoidVerdict_PLAYER_INBOX_FORCE_VOID_UNSPECIFIED {
			t.Fatalf("普通模式不应带强制证明结论: %+v", resp)
		}
		return resp
	}

	// b 从未投递 → 直接 VOIDED(finalized_by=ADMIN),open_count−1。
	if resp := void(b); resp.GetErrorMessage() != nil || resp.GetState() != stateVoided || resp.GetVoidRequested() {
		t.Fatalf("未投递应直接作废: %+v", resp)
	}
	if it := e.item(t, pid, b); it.GetFinalizedBy() != uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_ADMIN) {
		t.Fatalf("finalized_by 应为 ADMIN: %+v", it)
	}
	// a 已投递 → 只挂指令,仍 PENDING;再作废一次幂等。
	if resp := void(a); resp.GetState() != statePending || !resp.GetVoidRequested() {
		t.Fatalf("已投递应只挂指令: %+v", resp)
	}
	if resp := void(a); resp.GetState() != statePending || !resp.GetVoidRequested() || resp.GetErrorMessage() != nil {
		t.Fatalf("重复作废应幂等: %+v", resp)
	}
	if st := e.stream(t, pid); st.GetOpenCount() != 1 {
		t.Fatalf("open_count = %d, want 1(挂指令的仍计入)", st.GetOpenCount())
	}
	// 再拉取:a 带 VOID 指令且不带载荷;消费方报 SKIPPED → VOIDED。
	resp := e.fetch(t, pid, 2, 0, 0)
	if len(resp.GetItems()) != 1 || resp.GetItems()[0].GetDirective() != directiveVoid || len(resp.GetItems()[0].GetPayload()) != 0 {
		t.Fatalf("挂指令的条目应不带载荷: %+v", resp.GetItems())
	}
	if ack := e.ack(t, pid, 2, dec(a, outSkipped, 1, 1)); ack.GetResults()[0].GetState() != stateVoided {
		t.Fatalf("SKIPPED 应终结为 VOIDED: %+v", ack.GetResults())
	}
	// 终态 → StateConflict;不存在 → NotFound。
	if resp := void(a); resp.GetErrorMessage().GetId() != constants.ErrItemStateConflict {
		t.Fatalf("终态作废应 StateConflict: %+v", resp)
	}
	if resp := void(a + 99999); resp.GetErrorMessage().GetId() != constants.ErrItemNotFound {
		t.Fatalf("不存在应 NotFound: %+v", resp)
	}
	if n := auditCount(t, e.db, pid); n != 4 { // 2 次入箱 + b 作废 + a 作废(重复作废 INSERT IGNORE)
		t.Fatalf("审计行 = %d, want 4", n)
	}
}

// auditRow 读一条审计行;不存在时 ok=false。
func auditRow(t *testing.T, db *sql.DB, playerID, seq uint64, action inboxpb.PlayerInboxAuditAction) (operator, peer, reason string, ok bool) {
	t.Helper()
	err := db.QueryRowContext(itCtx(t), "SELECT `operator`,`peer_addr`,`reason` FROM player_inbox_audit"+
		" WHERE `shard` = ? AND `player_id` = ? AND `domain` = ? AND `seq` = ? AND `action` = ?",
		RowShard(playerID), playerID, uint32(gmDomain), seq, uint32(action)).Scan(&operator, &peer, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return operator, peer, reason, true
}

// auditRecord 读一整行审计(列清单与写入路径共用 auditColumns);不存在时返回 nil。
func auditRecord(t *testing.T, db *sql.DB, playerID, seq uint64, action inboxpb.PlayerInboxAuditAction) *inboxpb.PlayerInboxAudit {
	t.Helper()
	a := &inboxpb.PlayerInboxAudit{}
	err := db.QueryRowContext(itCtx(t), "SELECT "+auditColumns+" FROM player_inbox_audit"+
		" WHERE `shard` = ? AND `player_id` = ? AND `domain` = ? AND `seq` = ? AND `action` = ?",
		RowShard(playerID), playerID, uint32(gmDomain), seq, uint32(action)).Scan(
		&a.Shard, &a.PlayerId, &a.Domain, &a.Seq, &a.Action, &a.Operator, &a.PeerAddr, &a.Reason, &a.CreatedMs,
		&a.MsgType, &a.RequestId, &a.IssuedAtMs, &a.ContentSha256, &a.Payload, &a.VoidReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestIntegrationAuditRecordsWhatWasDone 是评审 L9 的回归:条目终态行 30 天后被清理,原来的审计行只有
// 「谁、何时、为什么」,清理之后就看不出处置类型、载荷和 request_id。现在审计行自足:
//   - SANCTION:msg_type / request_id / issued_at_ms / 摘要 / 载荷取自刚写出的条目行,void_reason = 0;
//   - VOID(从未投递直接作废、已投递挂指令):内容取自被作废的那一条,void_reason = 本次请求的原因;
//   - 摘要可由审计行自己的 msg_type / issued_at_ms / payload 复算;零字节载荷也照样记下;
//   - 条目终态行被清理之后,审计行原样还在。
func TestIntegrationAuditRecordsWhatWasDone(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(5003)
	const ridA, ridB = uint64(31), uint64(32)
	issuedA, issuedB := testNowMs-2_000, testNowMs-1_000
	payloadA, payloadB := gainPayload(t, 1), gainPayload(t, 0) // currency_type=0:序列化后是零字节
	resA, err := e.enqueue(t, pid, ridA, issuedA, payloadA)
	if err != nil || !resA.Created {
		t.Fatalf("入箱 a: %+v %v", resA, err)
	}
	resB, err := e.enqueue(t, pid, ridB, issuedB, payloadB)
	if err != nil || !resB.Created {
		t.Fatalf("入箱 b: %+v %v", resB, err)
	}
	a, b := resA.Seq, resB.Seq
	e.epochs.set(pid, 1)
	e.fetch(t, pid, 1, 0, 1) // 只投递 a

	const (
		sanction    = inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_SANCTION
		voidAudit   = inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_VOID
		noReason    = inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_UNSPECIFIED
		cancelled   = inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED
		quarantined = inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED
	)
	check := func(what string, got *inboxpb.PlayerInboxAudit, seq, requestID, issued uint64, payload []byte,
		reason inboxpb.PlayerInboxVoidReason) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: 审计行不存在", what)
		}
		if got.GetShard() != RowShard(pid) || got.GetSeq() != seq || got.GetMsgType() != uint32(gmBlock) ||
			got.GetRequestId() != requestID || got.GetIssuedAtMs() != issued || !bytes.Equal(got.GetPayload(), payload) ||
			!bytes.Equal(got.GetContentSha256(), ContentSHA256(gmBlock, issued, payload)) || got.GetVoidReason() != uint32(reason) {
			t.Fatalf("%s: 审计行 = %+v", what, got)
		}
		if !bytes.Equal(got.GetContentSha256(),
			ContentSHA256(inboxpb.PlayerInboxMessageType(got.GetMsgType()), got.GetIssuedAtMs(), got.GetPayload())) {
			t.Fatalf("%s: 摘要不能由审计行自己复算: %+v", what, got)
		}
	}
	check("a 入箱", auditRecord(t, e.db, pid, a, sanction), a, ridA, issuedA, payloadA, noReason)
	check("b 入箱(零字节载荷)", auditRecord(t, e.db, pid, b, sanction), b, ridB, issuedB, payloadB, noReason)

	ctx := itCtx(t)
	void := func(seq uint64, reason inboxpb.PlayerInboxVoidReason) {
		t.Helper()
		if resp, err := e.store.Void(ctx, VoidParams{
			PlayerID: pid, Domain: gmDomain, Seq: seq, Reason: reason, Operator: "ops_audit", Note: "工单 #9", PeerAddr: "127.0.0.1:4",
		}, nil); err != nil || resp.GetErrorMessage() != nil {
			t.Fatalf("作废 seq=%d: %+v %v", seq, resp, err)
		}
	}
	void(b, cancelled)   // 从未投递 → 直接 VOIDED
	void(a, quarantined) // 已投递 → 挂指令
	check("b 直接作废", auditRecord(t, e.db, pid, b, voidAudit), b, ridB, issuedB, payloadB, cancelled)
	check("a 挂指令", auditRecord(t, e.db, pid, a, voidAudit), a, ridA, issuedA, payloadA, quarantined)
	if v := auditRecord(t, e.db, pid, a, voidAudit); v.GetOperator() != "ops_audit" || v.GetPeerAddr() != "127.0.0.1:4" ||
		v.GetReason() != "工单 #9" {
		t.Fatalf("「谁、为什么」仍取调用方: %+v", v)
	}

	// a 由消费方 SKIPPED 终结;保留期过后 a、b 两条终态行都被清理,审计行仍答得出「做了什么」。
	e.fetch(t, pid, 1, 0, 0)
	if ack := e.ack(t, pid, 1, dec(a, outSkipped, uint32(quarantined), ridA)); ack.GetResults()[0].GetState() != stateVoided {
		t.Fatalf("a SKIPPED 应终结为 VOIDED: %+v", ack.GetResults())
	}
	if n, err := e.store.SweepTerminal(ctx, testNowMs+1, 500); err != nil || n != 0 {
		t.Fatalf("永久唯一收据不得清理 = %d (%v), want 0", n, err)
	}
	if e.item(t, pid, a) == nil || e.item(t, pid, b) == nil {
		t.Fatal("a、b 的终态唯一收据必须保留")
	}
	check("清理后 a 的作废审计", auditRecord(t, e.db, pid, a, voidAudit), a, ridA, issuedA, payloadA, quarantined)
	check("清理后 b 的入箱审计", auditRecord(t, e.db, pid, b, sanction), b, ridB, issuedB, payloadB, noReason)
	if n := auditCount(t, e.db, pid); n != 4 {
		t.Fatalf("审计行 = %d, want 4(2 次入箱 + 2 次作废),清理不得动审计", n)
	}
}

// TestIntegrationForceVoid 钉住 VoidItem 强制模式在真库上的读写(设计 §7.9,评审 M1):
//   - 证明不成立的每种情形(从未投递 / 读不到存档 / 回放 / UNKNOWN / 水位没到)都不改行、不写审计,读账本失败回故障;
//   - 证明成立 → VOIDED(finalized_by=ADMIN、delivery_count 与先前挂的指令原样保留),open_count−1,
//     审计 action=FORCE_VOID(与先前挂指令时的 VOID 审计并存,不被 INSERT IGNORE 吞掉);
//   - 终结后 Fetch 不再投递它、对账不再报 PENDING_BUT_DECIDED;重复调用回 StateConflict,不再写审计。
func TestIntegrationForceVoid(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(5002)
	const ledgerRequestAtB = uint64(999) // 消费方在 b 这个 seq 上实际裁决的是另一条(库恢复后旧行与账本对不上)
	a := e.mustEnqueue(t, pid, 1).Seq    // 已执行并确认
	b := e.mustEnqueue(t, pid, 2).Seq    // scene 判 MISMATCH 的那一行
	c := e.mustEnqueue(t, pid, 3).Seq    // 从未投递
	e.epochs.set(pid, 2)
	e.fetch(t, pid, 2, 0, 2) // 只投递 a、b
	if ack := e.ack(t, pid, 2, dec(a, outApplied, 0, 1)); ack.GetResults()[0].GetState() != stateApplied {
		t.Fatalf("a 应终结为 APPLIED: %+v", ack.GetResults())
	}
	ctx := itCtx(t)
	const note = "工单 #42:库恢复后旧行,账本同 seq 为另一 request_id"
	force := func(seq uint64, ledgers LedgerReader) (*inboxpb.VoidPlayerInboxItemResponse, error) {
		return e.store.Void(ctx, VoidParams{
			PlayerID: pid, Domain: gmDomain, Seq: seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED,
			Operator: "ops_force", Note: note, PeerAddr: "127.0.0.1:3",
			Mode: inboxpb.PlayerInboxVoidMode_PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH,
		}, ledgers)
	}
	proving := itLedgers{found: true, ledger: fvLedger(fvCursor(gmDomain, b, fvDecision(a, 1), fvDecision(b, ledgerRequestAtB)))}

	// 终结之前,对账把 b 报成 PENDING_BUT_DECIDED(账本水位已过、收件箱仍 PENDING)。
	rec, err := e.store.ReconcilePlayer(ctx, &inboxpb.ReconcilePlayerInboxRequest{PlayerId: pid}, proving)
	if err != nil || len(rec.GetDiffs()) != 1 || rec.GetDiffs()[0].GetSeq() != b ||
		rec.GetDiffs()[0].GetKind() != inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_PENDING_BUT_DECIDED {
		t.Fatalf("终结前的对账: %+v %v", rec.GetDiffs(), err)
	}

	auditsBefore := auditCount(t, e.db, pid)
	refusals := []struct {
		name    string
		seq     uint64
		ledgers LedgerReader
		want    inboxpb.PlayerInboxForceVoidVerdict
	}{
		{"从未投递", c, proving, forceVoidNotDelivered},
		{"读不到存档", b, itLedgers{found: false}, forceVoidLedgerAbsent},
		{"回放:环里同 seq 同 request_id", b, itLedgers{found: true, ledger: fvLedger(fvCursor(gmDomain, b, fvDecision(a, 1), fvDecision(b, 2)))},
			forceVoidRequestInLedger},
		{"UNKNOWN:水位已过、环里没有 b", b, itLedgers{found: true, ledger: fvLedger(fvCursor(gmDomain, b, fvDecision(a, 1)))},
			forceVoidSeqNotInRing},
		{"水位没到 b", b, itLedgers{found: true, ledger: fvLedger(fvCursor(gmDomain, a, fvDecision(a, 1)))}, forceVoidNotDecided},
	}
	for _, r := range refusals {
		resp, err := force(r.seq, r.ledgers)
		if err != nil || resp.GetErrorMessage().GetId() != constants.ErrItemStateConflict || resp.GetForceVerdict() != r.want ||
			resp.GetState() != statePending || resp.GetVoidRequested() {
			t.Fatalf("%s: resp=%+v err=%v, want StateConflict + %v,行仍 PENDING", r.name, resp, err, r.want)
		}
		if it := e.item(t, pid, r.seq); it.GetState() != uint32(statePending) || it.GetFinalizedBy() != 0 {
			t.Fatalf("%s: 证明不成立不得改行: %+v", r.name, it)
		}
	}
	if _, err := force(b, itLedgers{err: errors.New("data_service down")}); err == nil {
		t.Fatal("读账本失败应回故障(适配层折成 StoreUnavailable)")
	}
	if st := e.stream(t, pid); st.GetOpenCount() != 2 {
		t.Fatalf("拒绝路径不得动 open_count: %d, want 2(b、c)", st.GetOpenCount())
	}
	if n := auditCount(t, e.db, pid); n != auditsBefore {
		t.Fatalf("拒绝路径不得写审计: %d, want %d", n, auditsBefore)
	}

	// 按 runbook 先对 b 挂过 VOID 指令(scene 照样判 MISMATCH),再强制终结。
	if resp, err := e.store.Void(ctx, VoidParams{
		PlayerID: pid, Domain: gmDomain, Seq: b, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED,
		Operator: "ops_force", Note: note, PeerAddr: "127.0.0.1:3",
	}, nil); err != nil || !resp.GetVoidRequested() {
		t.Fatalf("普通模式挂指令: %+v %v", resp, err)
	}
	resp, err := force(b, proving)
	if err != nil || resp.GetErrorMessage() != nil || resp.GetState() != stateVoided || resp.GetVoidRequested() ||
		resp.GetForceVerdict() != forceVoidProven {
		t.Fatalf("证明成立应终结为 VOIDED: %+v %v", resp, err)
	}
	it := e.item(t, pid, b)
	if it.GetState() != uint32(stateVoided) ||
		it.GetFinalizedBy() != uint32(inboxpb.PlayerInboxFinalizer_PLAYER_INBOX_FINALIZER_ADMIN) ||
		it.GetVoidReason() != uint32(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED) ||
		it.GetDeliveryCount() != 1 || it.GetDirective() != uint32(directiveVoid) || it.GetRequestId() != 2 {
		t.Fatalf("强制终结后的行: %+v", it)
	}
	if st := e.stream(t, pid); st.GetOpenCount() != 1 {
		t.Fatalf("open_count = %d, want 1(只剩 c)", st.GetOpenCount())
	}
	if op, peer, reason, ok := auditRow(t, e.db, pid, b, inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_FORCE_VOID); !ok ||
		op != "ops_force" || peer != "127.0.0.1:3" || reason != note {
		t.Fatalf("FORCE_VOID 审计行: ok=%v operator=%q peer=%q reason=%q", ok, op, peer, reason)
	}
	// 审计行记下被终结的是哪一条(评审 L9):内容取自条目行,void_reason 是本次请求的原因。
	if fv := auditRecord(t, e.db, pid, b, inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_FORCE_VOID); fv.GetRequestId() != 2 ||
		fv.GetMsgType() != uint32(gmBlock) || fv.GetIssuedAtMs() != it.GetIssuedAtMs() || !bytes.Equal(fv.GetPayload(), it.GetPayload()) ||
		!bytes.Equal(fv.GetContentSha256(), it.GetContentSha256()) ||
		fv.GetVoidReason() != uint32(inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED) {
		t.Fatalf("FORCE_VOID 审计行应记下被终结的那一条: %+v, item=%+v", fv, it)
	}
	if _, _, _, ok := auditRow(t, e.db, pid, b, inboxpb.PlayerInboxAuditAction_PLAYER_INBOX_AUDIT_VOID); !ok {
		t.Fatal("先前挂指令的 VOID 审计行应原样保留")
	}
	if n := auditCount(t, e.db, pid); n != auditsBefore+2 {
		t.Fatalf("审计行 = %d, want %d(挂指令 + 强制终结各一行)", n, auditsBefore+2)
	}

	// 终结后:Fetch 只投递 c;对账不再报 b(管理员终结的行不比对)。
	fetched := e.fetch(t, pid, 2, b, 0)
	if len(fetched.GetItems()) != 1 || fetched.GetItems()[0].GetSeq() != c {
		t.Fatalf("强制终结后不应再投递 b: %+v", fetched.GetItems())
	}
	rec, err = e.store.ReconcilePlayer(ctx, &inboxpb.ReconcilePlayerInboxRequest{PlayerId: pid}, proving)
	if err != nil || len(rec.GetDiffs()) != 0 {
		t.Fatalf("终结后的对账: %+v %v", rec.GetDiffs(), err)
	}

	// 重复调用:终态 → StateConflict,不看账本、不再写审计;不存在 → NotFound。
	if resp, err := force(b, proving); err != nil || resp.GetErrorMessage().GetId() != constants.ErrItemStateConflict ||
		resp.GetState() != stateVoided || resp.GetForceVerdict() != forceVoidUnspecified {
		t.Fatalf("重复强制终结: %+v %v", resp, err)
	}
	if resp, err := force(b+99999, proving); err != nil || resp.GetErrorMessage().GetId() != constants.ErrItemNotFound {
		t.Fatalf("不存在: %+v %v", resp, err)
	}
	if n := auditCount(t, e.db, pid); n != auditsBefore+2 {
		t.Fatalf("重复调用不得再写审计: %d", n)
	}
}

// ── 后台循环的存储操作 ───────────────────────────────────────────────────

func TestIntegrationSweepPreservesPermanentRequestRows(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(6001)
	// T0:入箱 a、b;只投递 a 并确认 APPLIED(a.updated_ms = T0)。
	a := e.mustEnqueue(t, pid, 1).Seq
	b := e.mustEnqueue(t, pid, 2).Seq
	e.epochs.set(pid, 1)
	e.fetch(t, pid, 1, 0, 1)
	e.ack(t, pid, 1, dec(a, outApplied, 0, 1))
	// T0+60s:投递并确认 b(b.updated_ms = T0+60s)。
	e.clock.advance(60_000)
	e.fetch(t, pid, 1, 0, 0)
	e.ack(t, pid, 1, dec(b, outApplied, 0, 2))
	// 回到 T0 再入箱 c:PENDING 且 updated_ms = T0(seq 由 next_seq 保证仍单调)。
	e.clock.set(testNowMs)
	c := e.mustEnqueue(t, pid, 3).Seq

	cutoff := testNowMs + 30_000 // 只有 a 是「保留期外的终态行」
	deleted, err := e.store.SweepTerminal(itCtx(t), cutoff, 500)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0(永久唯一收据)", deleted)
	}
	if e.item(t, pid, a) == nil {
		t.Fatal("旧终态行 a 仍须阻止历史request换主重入")
	}
	if e.item(t, pid, b) == nil || e.item(t, pid, c) == nil {
		t.Fatal("保留期内的终态行 b 与 PENDING 行 c 不得删除")
	}
	if e.stream(t, pid) == nil || auditCount(t, e.db, pid) != 3 {
		t.Fatal("流行与审计行永不自动删除")
	}
}

// TestIntegrationNotifyCASSameMillisecond:同一毫秒两次入箱时,拿旧 next_seq 的补发不能清掉新入箱的提醒(设计 §6.3,无 ABA)。
func TestIntegrationNotifyCASSameMillisecond(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(6002)
	e.notifier.setResult(NotifyError) // 入箱后那次提醒失败,留给补发
	first := e.mustEnqueue(t, pid, 1)
	due, err := e.store.DueNotifications(itCtx(t), RowShard(pid), testNowMs, 100)
	if err != nil || len(due) != 1 || due[0].NextSeq != first.Seq+1 {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	stale := due[0]
	second := e.mustEnqueue(t, pid, 2) // 同一毫秒(时钟固定)
	if st := e.stream(t, pid); st.GetNotifyDueMs() != testNowMs {
		t.Fatalf("两次 notify_due_ms 相同(同一毫秒):%+v", st)
	}

	e.notifier.setResult(NotifySent)
	e.store.ResendNotify(itCtx(t), stale)
	if st := e.stream(t, pid); st.GetNotifyDueMs() == 0 {
		t.Fatal("旧令牌的 CAS 不应清掉第二次入箱的提醒")
	}
	due, err = e.store.DueNotifications(itCtx(t), RowShard(pid), testNowMs, 100)
	if err != nil || len(due) != 1 || due[0].NextSeq != second.Seq+1 {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	e.store.ResendNotify(itCtx(t), due[0])
	if st := e.stream(t, pid); st.GetNotifyDueMs() != 0 {
		t.Fatalf("当前令牌的 CAS 应清零: %+v", st)
	}
}

// TestIntegrationScanGauges 钉住扫描型 gauge 的 SQL 一侧(设计 §9.4;跨分片折叠另见 TestFoldScanGauges):
// 只数 PENDING 行;已投递未确认按最后投递时刻算最老;提醒积压数「notify_due_ms 非 0 且流行最后一次写入不晚于截止」的流。
// 评审 M4:最后投递早于 DeliveredStaleBeforeMs 的已投递未确认行(玩家被踢下线后再没回来,ackQueue 随实体丢了)
// 不进 delivered_unacked / 最老时长 / max_delivery_count,只进 delivered_unacked_stale。
func TestIntegrationScanGauges(t *testing.T) {
	e := openIntegration(t)
	// 玩家 A:两条 PENDING,投递其中一条两次后不确认。
	// 玩家 C:一条 PENDING,在 T0 被投递三次后不确认 —— 截止取 T0+0.5s 时它是陈旧行。
	const pa, pb, pc = uint64(9001), uint64(9066), uint64(9067) // RowShard = 46 / 32 / 13:三个分片,覆盖跨分片折叠
	if RowShard(pa) == RowShard(pb) || RowShard(pb) == RowShard(pc) || RowShard(pa) == RowShard(pc) {
		t.Fatalf("测试前提:三名玩家必须落在三个不同分片,got %d / %d / %d", RowShard(pa), RowShard(pb), RowShard(pc))
	}
	e.mustEnqueue(t, pa, 1)
	e.mustEnqueue(t, pa, 2)
	e.epochs.set(pa, 1)
	e.mustEnqueue(t, pc, 1)
	e.epochs.set(pc, 1)
	for i := 0; i < 3; i++ {
		e.fetch(t, pc, 1, 0, 0) // delivery_count 1→3,last_delivered = T0
	}
	e.clock.advance(1_000)
	e.fetch(t, pa, 1, 0, 1) // 第一条 delivery_count=1,last_delivered=T0+1s
	e.clock.advance(1_000)
	e.fetch(t, pa, 1, 0, 1) // 第一条 delivery_count=2,last_delivered=T0+2s
	// 玩家 B(另一分片):一条已确认(终态,不计入),一条 PENDING 从未投递;入箱后提醒失败,留下 notify_due_ms。
	e.clock.advance(1_000)
	b1 := e.mustEnqueue(t, pb, 1).Seq
	e.epochs.set(pb, 1)
	e.fetch(t, pb, 1, 0, 0)
	e.ack(t, pb, 1, dec(b1, outApplied, 0, 1))
	e.notifier.setResult(NotifyError)
	e.mustEnqueue(t, pb, 2) // created_ms = T0+3s,notify_due_ms = T0+3s

	now := testNowMs + 10_000
	cut := GaugeCutoffs{NotifyOverdueBeforeMs: testNowMs + 3_000, DeliveredStaleBeforeMs: testNowMs + 500}
	g, err := e.store.ScanGauges(itCtx(t), now, cut)
	if err != nil {
		t.Fatal(err)
	}
	want := metrics.ScanGauges{
		OpenItems:                     4,  // A 两条 + B 一条 + C 一条 PENDING
		DeliveredUnacked:              1,  // 只有 A 的第一条在截止之后投递过
		DeliveredUnackedStale:         1,  // C:最后投递 T0 < 截止 T0+0.5s
		MaxDeliveryCount:              2,  // C 投了 3 次,但它是陈旧行,不进 PoisonSuspect 的口径
		OldestOpenAgeSeconds:          10, // A / C created_ms = T0
		DeliveredUnackedOldestSeconds: 8,  // A 最后投递 T0+2s(C 的 T0 不算)
		NotifyBacklog:                 1,  // B 的流 notify_due_ms 非 0,最后一次写入 T0+3s ≤ 截止
	}
	if g != want {
		t.Fatalf("ScanGauges = %+v, want %+v", g, want)
	}
	// 截止就是陈旧判定的边界:last_delivered_ms ≥ 截止算近期。
	edge := cut
	edge.DeliveredStaleBeforeMs = testNowMs + 2_000
	if g, err := e.store.ScanGauges(itCtx(t), now, edge); err != nil || g.DeliveredUnacked != 1 || g.DeliveredUnackedStale != 1 {
		t.Fatalf("A 最后投递恰好等于截止时仍算近期: %+v %v", g, err)
	}
	edge.DeliveredStaleBeforeMs = testNowMs + 2_001
	if g, err := e.store.ScanGauges(itCtx(t), now, edge); err != nil || g.DeliveredUnacked != 0 || g.DeliveredUnackedStale != 2 ||
		g.MaxDeliveryCount != 0 || g.DeliveredUnackedOldestSeconds != 0 {
		t.Fatalf("全部已投递行都陈旧时告警口径三项应为 0: %+v %v", g, err)
	}
	// 截止为 0(时钟早于 24h 窗口):全部算近期。
	edge.DeliveredStaleBeforeMs = 0
	if g, err := e.store.ScanGauges(itCtx(t), now, edge); err != nil || g.DeliveredUnacked != 2 || g.DeliveredUnackedStale != 0 ||
		g.MaxDeliveryCount != 3 || g.DeliveredUnackedOldestSeconds != 10 {
		t.Fatalf("截止为 0 时全部算近期: %+v %v", g, err)
	}
	edge = cut
	edge.NotifyOverdueBeforeMs = testNowMs + 2_999
	if g, err := e.store.ScanGauges(itCtx(t), now, edge); err != nil || g.NotifyBacklog != 0 {
		t.Fatalf("截止早于流行最后一次写入时不算积压: %+v %v", g, err)
	}
}

// TestIntegrationNotifyResendBackoff 钉住补发没落定时的退避与不可恢复结局的落定(评审 L6):
//   - 失败(Error 一类)时以同一条 next_seq CAS 把 notify_due_ms 推到 now + 退避,不再每轮占着分片队头;
//   - 退避从流行最后一次写入起算,按 2 倍递增、封顶 30 分钟;
//   - 推迟之后积压 gauge 照样数它(按流行最后一次写入算,不看被推后的 notify_due_ms);
//   - 期间又有入箱(next_seq 变了),旧令牌的推迟什么都不改;
//   - 不可恢复的结局(NoNode)落定清零,交给落地拉取;
//   - 镜像已同步却查不到节点(NodeMissing,评审 R3)先按退避推迟,悬满 1 小时才落定清零。
func TestIntegrationNotifyResendBackoff(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(6003)
	sec := func(n uint64) uint64 { return n * 1000 }
	e.notifier.setResult(NotifyError) // 入箱后那次提醒失败;补发也一直失败
	e.mustEnqueue(t, pid, 1)          // T0:notify_due_ms = updated_ms = T0

	resendAt := func(at uint64) NotifyDue {
		t.Helper()
		e.clock.set(at)
		due, err := e.store.DueNotifications(itCtx(t), RowShard(pid), at-sec(5), 100)
		if err != nil || len(due) != 1 {
			t.Fatalf("at=T0+%dms: due = %+v err=%v", at-testNowMs, due, err)
		}
		if due[0].PendingSinceMs != testNowMs {
			t.Fatalf("退避起算点应是流行最后一次写入 T0,got %d", due[0].PendingSinceMs)
		}
		if got := e.store.ResendNotify(itCtx(t), due[0]); got != NotifyError {
			t.Fatalf("ResendNotify = %s, want error", got)
		}
		return due[0]
	}
	expectDue := func(want uint64, what string) {
		t.Helper()
		if st := e.stream(t, pid); st.GetNotifyDueMs() != want || st.GetUpdatedMs() != testNowMs {
			t.Fatalf("%s: notify_due_ms = T0%+dms updated_ms = %d, want T0%+dms 且 updated_ms 不动",
				what, int64(st.GetNotifyDueMs()-testNowMs), st.GetUpdatedMs(), int64(want-testNowMs))
		}
	}

	resendAt(testNowMs + sec(10))
	expectDue(testNowMs+sec(40), "悬了 10s:退避取下限 30s")
	if due, err := e.store.DueNotifications(itCtx(t), RowShard(pid), testNowMs+sec(39), 100); err != nil || len(due) != 0 {
		t.Fatalf("推迟之后到点之前不应再被扫到: %+v %v", due, err)
	}
	resendAt(testNowMs + sec(100))
	expectDue(testNowMs+sec(200), "悬了 100s:再等 100s")
	stale := resendAt(testNowMs + sec(3*3600))
	expectDue(testNowMs+sec(3*3600+1800), "悬了 3 小时:封顶 30 分钟")

	// 推迟到未来的提醒仍算积压:它从 T0 起就没落定。
	now := testNowMs + sec(3*3600)
	if g, err := e.store.ScanGauges(itCtx(t), now, GaugeCutoffs{NotifyOverdueBeforeMs: now - sec(300)}); err != nil || g.NotifyBacklog != 1 {
		t.Fatalf("推迟后的提醒应计入积压: %+v %v", g, err)
	}

	// 又入箱一条(next_seq 变了、notify_due_ms 回到 now):拿旧令牌的推迟不改它。
	second := e.mustEnqueue(t, pid, 2)
	if ok, err := e.store.DeferNotifyDue(itCtx(t), pid, gmDomain, stale.NextSeq, now+sec(999)); err != nil || ok {
		t.Fatalf("旧令牌的推迟应 CAS 失败: ok=%v err=%v", ok, err)
	}
	if st := e.stream(t, pid); st.GetNotifyDueMs() != now || st.GetNextSeq() != second.Seq+1 {
		t.Fatalf("新入箱的提醒应保持 notify_due_ms = now: %+v", st)
	}

	// 不可恢复的结局落定清零。
	e.notifier.setResult(NotifyNoNode)
	e.clock.set(now + sec(10))
	due, err := e.store.DueNotifications(itCtx(t), RowShard(pid), now+sec(5), 100)
	if err != nil || len(due) != 1 || due[0].NextSeq != second.Seq+1 {
		t.Fatalf("due = %+v err=%v", due, err)
	}
	if got := e.store.ResendNotify(itCtx(t), due[0]); got != NotifyNoNode || !got.Settled() {
		t.Fatalf("ResendNotify = %s, want no_node(落定)", got)
	}
	if st := e.stream(t, pid); st.GetNotifyDueMs() != 0 {
		t.Fatalf("不可恢复的结局应落定清零: %+v", st)
	}

	// 镜像已同步却查不到节点:可能只是租约抖动后的重新注册(玩家仍在线),先按退避重试,悬满 1 小时才落定。
	e.notifier.setResult(NotifyNodeMissing)
	t1 := now + sec(10)
	third := e.mustEnqueue(t, pid, 3) // 入箱后那次提醒同样查不到节点:不清零,notify_due_ms = updated_ms = T1
	e.clock.set(t1 + sec(10))
	due, err = e.store.DueNotifications(itCtx(t), RowShard(pid), t1+sec(5), 100)
	if err != nil || len(due) != 1 || due[0].NextSeq != third.Seq+1 || due[0].PendingSinceMs != t1 {
		t.Fatalf("node_missing: due = %+v err=%v", due, err)
	}
	if got := e.store.ResendNotify(itCtx(t), due[0]); got != NotifyNodeMissing || got.Settled() {
		t.Fatalf("ResendNotify = %s, want node_missing(没落定)", got)
	}
	if st := e.stream(t, pid); st.GetNotifyDueMs() != t1+sec(40) {
		t.Fatalf("刚开始查不到节点:应按退避推迟到 T1+40s,got notify_due_ms = T1%+dms", int64(st.GetNotifyDueMs()-t1))
	}
	e.clock.set(t1 + uint64(notifyNodeMissingSettleAfter.Milliseconds()))
	if got := e.store.ResendNotify(itCtx(t), due[0]); got != NotifyNodeMissing {
		t.Fatalf("ResendNotify = %s, want node_missing", got)
	}
	if st := e.stream(t, pid); st.GetNotifyDueMs() != 0 {
		t.Fatalf("悬满 1 小时仍查不到节点:应落定清零: %+v", st)
	}
}

func TestIntegrationSummaryListLookup(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(7001)
	a := e.mustEnqueue(t, pid, 1).Seq
	e.mustEnqueue(t, pid, 2)
	e.epochs.set(pid, 9)
	e.fetch(t, pid, 9, 0, 1)
	ctx := itCtx(t)

	sum, err := e.store.Summary(ctx, pid)
	if err != nil || len(sum.GetStreams()) != 1 {
		t.Fatalf("Summary: %+v %v", sum, err)
	}
	s := sum.GetStreams()[0]
	if s.GetOpenCount() != 2 || s.GetDeliveredUnackedCount() != 1 || s.GetFenceEpoch() != 9 || sum.GetMaxFenceEpoch() != 9 ||
		s.GetOldestPendingCreatedMs() != testNowMs {
		t.Fatalf("Summary 字段: %+v", sum)
	}

	list, err := e.store.List(ctx, &inboxpb.ListPlayerInboxItemsRequest{PlayerId: pid, Domain: gmDomain, Limit: 1})
	if err != nil || len(list.GetItems()) != 1 || !list.GetHasMore() || list.GetItems()[0].GetSeq() != a {
		t.Fatalf("List 第一页: %+v %v", list, err)
	}
	list, err = e.store.List(ctx, &inboxpb.ListPlayerInboxItemsRequest{PlayerId: pid, Domain: gmDomain, AfterSeq: a})
	if err != nil || len(list.GetItems()) != 1 || list.GetHasMore() {
		t.Fatalf("List 第二页: %+v %v", list, err)
	}

	look, err := e.store.Lookup(ctx, &inboxpb.LookupPlayerInboxRequest{
		PlayerId: pid, ProducerKind: inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM, RequestId: 1,
	})
	if err != nil || look.GetItem().GetSeq() != a || look.GetItem().GetOperatorName() != "ops_it" {
		t.Fatalf("Lookup: %+v %v", look, err)
	}
	look, err = e.store.Lookup(ctx, &inboxpb.LookupPlayerInboxRequest{
		PlayerId: pid, ProducerKind: inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM, RequestId: 404,
	})
	if err != nil || look.GetErrorMessage().GetId() != constants.ErrItemNotFound {
		t.Fatalf("Lookup 不存在: %+v %v", look, err)
	}
}

type itLedgers struct {
	ledger *componentpb.PlayerInboxLedgerComp
	found  bool
	err    error
}

func (l itLedgers) ReadInboxLedger(context.Context, uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	if l.err != nil {
		return nil, false, l.err
	}
	return l.ledger, l.found, nil
}

// TestIntegrationReconcile 钉住两个对账入口的读库一侧(判定谓词另见 TestFinalizedLedgerGap)。
// 收件箱:a、b 已 APPLIED,c 已投递未确认。账本一律是真实形状(水位 = 环尾,rcCursor):
//   - b 的落盘丢了:环 [a]、水位 a → b 报 FINAL_NOT_IN_LEDGER;
//   - c 已执行、确认丢了:环 [a, b, c] → c 报 PENDING_BUT_DECIDED;
//   - b 的落盘丢了、之后 c 又执行(评审 M6):环 [a, c]、水位 c —— 只比水位会被掩盖,b 照样报 FINAL_NOT_IN_LEDGER;
//   - 账本结构不合法(环超过 32 条)→ ReconcilePlayer 回 error,定时对账计 invalid,都不是「无差异」。
func TestIntegrationReconcile(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(8001)
	a := e.mustEnqueue(t, pid, 1).Seq
	b := e.mustEnqueue(t, pid, 2).Seq
	e.epochs.set(pid, 1)
	e.fetch(t, pid, 1, 0, 0)
	e.ack(t, pid, 1, dec(a, outApplied, 0, 1), dec(b, outApplied, 0, 2))
	c := e.mustEnqueue(t, pid, 3).Seq
	e.fetch(t, pid, 1, b, 0) // 投递 c,不确认
	ctx := itCtx(t)

	da, db, dc := rcDecision(a, 1, outApplied, 0), rcDecision(b, 2, outApplied, 0), rcDecision(c, 3, outApplied, 0)
	lostB := rcLedger(rcCursor(gmDomain, da))
	ackLost := rcLedger(rcCursor(gmDomain, da, db, dc))
	masked := rcLedger(rcCursor(gmDomain, da, dc))
	broken := rcLedger(rcRing(1, ledgerRecentCapacity+1, 0))

	final := inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_FINAL_NOT_IN_LEDGER
	decided := inboxpb.PlayerInboxReconcileDiffKind_PLAYER_INBOX_RECONCILE_PENDING_BUT_DECIDED
	req := &inboxpb.ReconcilePlayerInboxRequest{PlayerId: pid}
	diffsOf := func(name string, ledger *componentpb.PlayerInboxLedgerComp) []*inboxpb.PlayerInboxReconcileDiff {
		t.Helper()
		resp, err := e.store.ReconcilePlayer(ctx, req, itLedgers{ledger: ledger, found: true})
		if err != nil || !resp.GetLedgerFound() {
			t.Fatalf("%s: ReconcilePlayer: %+v %v", name, resp, err)
		}
		return resp.GetDiffs()
	}
	if d := diffsOf("b 的落盘丢了", lostB); len(d) != 1 || d[0].GetSeq() != b || d[0].GetKind() != final {
		t.Fatalf("b 的落盘丢了: diffs = %+v", d)
	}
	if d := diffsOf("确认丢了", ackLost); len(d) != 1 || d[0].GetSeq() != c || d[0].GetKind() != decided {
		t.Fatalf("确认丢了: diffs = %+v", d)
	}
	if d := diffsOf("被 c 掩盖", masked); len(d) != 2 || d[0].GetSeq() != b || d[0].GetKind() != final ||
		d[1].GetSeq() != c || d[1].GetKind() != decided {
		t.Fatalf("被 c 掩盖: diffs = %+v, want b FINAL_NOT_IN_LEDGER + c PENDING_BUT_DECIDED", d)
	}
	if resp, err := e.store.ReconcilePlayer(ctx, req, itLedgers{ledger: broken, found: true}); err == nil {
		t.Fatalf("账本不合法应回 error(适配层折成 StoreUnavailable),不能回「无差异」: %+v", resp)
	}

	// 定时对账:同一个谓词,只核对消费方终结的 a、b。
	start := RowShard(pid)
	recent := func(name string, l LedgerReader, wantInconsistent int) {
		t.Helper()
		res, err := e.store.ReconcileRecent(ctx, 0, start, l)
		if err != nil || res.Inconsistent != wantInconsistent || res.PlayersPlanned != 1 || res.PlayersChecked != 1 || res.Truncated {
			t.Fatalf("%s: ReconcileRecent = %+v err=%v, want %d 处不一致、1 名玩家、未截断", name, res, err, wantInconsistent)
		}
	}
	recent("b 的落盘丢了", itLedgers{ledger: lostB, found: true}, 1)
	recent("被 c 掩盖", itLedgers{ledger: masked, found: true}, 1)
	recent("a、b 都在环里", itLedgers{ledger: ackLost, found: true}, 0)
	recent("读不到存档计 unknown", itLedgers{found: false}, 0)
	recent("账本不合法计 invalid", itLedgers{ledger: broken, found: true}, 0)
	recent("读失败计 error", itLedgers{err: errors.New("data_service down")}, 0)
}

// TestIntegrationRecentlyFinalizedOrder 钉住定时对账读终结行的 SQL(评审 M5):
//   - 按 (updated_ms, player_id, domain, seq) 降序。回归:原 SQL 没有 ORDER BY,LIMIT 截下的是索引
//     (shard, state, updated_ms) 上 state 最小的 APPLIED,更新的 REJECTED 被挤掉;
//   - 键集游标逐页不重不漏;sinceMs 之前的行不读;管理员终结的行不读;对账要的列都读出来。
func TestIntegrationRecentlyFinalizedOrder(t *testing.T) {
	e := openIntegration(t)
	const pid = uint64(8065) // 分片按 RowShard(pid) 算,不写死
	a := e.mustEnqueue(t, pid, 1).Seq
	b := e.mustEnqueue(t, pid, 2).Seq
	c := e.mustEnqueue(t, pid, 3).Seq
	d := e.mustEnqueue(t, pid, 4).Seq // 从未投递,管理员直接作废(finalized_by=ADMIN)
	e.epochs.set(pid, 1)
	e.fetch(t, pid, 1, 0, 3)                   // 投递 a、b、c
	e.ack(t, pid, 1, dec(a, outApplied, 0, 1)) // a:updated_ms = T0
	e.clock.advance(1_000)
	e.ack(t, pid, 1, dec(b, outRejected, 7, 2), dec(c, outApplied, 0, 3)) // b、c:updated_ms = T0+1s
	ctx := itCtx(t)
	if resp, err := e.store.Void(ctx, VoidParams{
		PlayerID: pid, Domain: gmDomain, Seq: d, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_QUARANTINED,
		Operator: "ops_it", Note: "it", PeerAddr: "127.0.0.1:1",
	}, nil); err != nil || resp.GetState() != stateVoided {
		t.Fatalf("直接作废 d: %+v %v", resp, err)
	}
	shard := RowShard(pid)
	page := func(sinceMs uint64, before *FinalizedRef, limit int) []FinalizedRef {
		t.Helper()
		refs, err := e.store.RecentlyFinalized(ctx, shard, sinceMs, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		return refs
	}
	seqs := func(refs []FinalizedRef) []uint64 {
		out := make([]uint64, 0, len(refs))
		for _, f := range refs {
			out = append(out, f.Seq)
		}
		return out
	}

	all := page(0, nil, 10)
	if got := seqs(all); len(got) != 3 || got[0] != c || got[1] != b || got[2] != a {
		t.Fatalf("降序 = %v, want [c b a](同一毫秒按 seq 降序;管理员作废的 d 不读)", got)
	}
	if rb := all[1]; rb.State != stateRejected || rb.OutcomeReason != 7 || rb.RequestID != 2 || rb.MsgType != gmBlock ||
		rb.FinalizedBy != byConsumer || rb.UpdatedMs != testNowMs+1_000 || rb.PlayerID != pid || rb.Domain != gmDomain {
		t.Fatalf("b 的列: %+v", rb)
	}
	if got := seqs(page(0, nil, 2)); len(got) != 2 || got[0] != c || got[1] != b {
		t.Fatalf("LIMIT 2 = %v, want [c b]:更新的 REJECTED 不得被更旧的 APPLIED 挤掉", got)
	}
	// 逐页翻:每页 1 行,游标是上一页最后一行。
	var walked []uint64
	var before *FinalizedRef
	for i := 0; i < 5; i++ {
		p := page(0, before, 1)
		if len(p) == 0 {
			break
		}
		walked = append(walked, p[0].Seq)
		before = &p[0]
	}
	if len(walked) != 3 || walked[0] != c || walked[1] != b || walked[2] != a {
		t.Fatalf("键集翻页 = %v, want [c b a]", walked)
	}
	if got := seqs(page(testNowMs+1, nil, 10)); len(got) != 2 || got[0] != c || got[1] != b {
		t.Fatalf("sinceMs 之后 = %v, want [c b]", got)
	}
}

// TestIntegrationCheckRowShards 是评审 R3 的回归,走真实路径:库里预埋按旧公式(player_id % 64)写出的一名玩家的
// 流行 / 条目行 / 审计行,原注释声称「读出时被 checkRowShard 拒绝」并不成立 ——
//   - 按玩家读(带 WHERE shard = RowShard(player_id))读不到它:静默不可见;
//   - 按分片扫描(DueNotifications)读到它,整片报错;
//   - CheckRowShards 逐表判出不一致(三张表各自都查),改回之后通过;正常写出的行从不误报。
func TestIntegrationCheckRowShards(t *testing.T) {
	e := openIntegration(t)
	ctx := itCtx(t)
	if err := CheckRowShards(ctx, e.db); err != nil {
		t.Fatalf("空库: %v", err)
	}
	e.mustEnqueue(t, 9001, 1)
	if err := CheckRowShards(ctx, e.db); err != nil {
		t.Fatalf("正常写出的行不应报错: %v", err)
	}

	pid := uint64(9002)
	for uint32(pid%ShardCount) == RowShard(pid) { // 取一个新旧公式落在不同分片的玩家
		pid++
	}
	oldShard := uint32(pid % ShardCount)
	e.notifier.setResult(NotifyError) // 让流行挂着 notify_due_ms,好让按分片的补发扫描读到它
	e.mustEnqueue(t, pid, 1)
	moveTo := func(table string, from, to uint32) {
		t.Helper()
		res, err := e.db.ExecContext(ctx, "UPDATE "+table+" SET `shard` = ? WHERE `shard` = ? AND `player_id` = ?", to, from, pid)
		if err != nil {
			t.Fatalf("改 %s 的 shard: %v", table, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("%s 里玩家 %d 应恰有 1 行,改了 %d 行", table, pid, n)
		}
	}
	for _, table := range []string{streamTable, itemTable, auditTable} {
		moveTo(table, RowShard(pid), oldShard)
	}

	if st := e.stream(t, pid); st != nil {
		t.Fatalf("按玩家读不应看得见旧公式的流行: %+v", st)
	}
	if _, err := e.store.DueNotifications(ctx, oldShard, testNowMs+1, 100); err == nil {
		t.Fatal("按分片扫描读到旧公式的流行应整片报错")
	}

	for _, table := range []string{streamTable, itemTable, auditTable} {
		err := CheckRowShards(ctx, e.db)
		// 错误信息的处置一段会列出全部三张表,所以按开头的「inbox: <表名> has」认是哪张表报的。
		if err == nil || !strings.HasPrefix(err.Error(), "inbox: "+table+" has ") ||
			!strings.Contains(err.Error(), "player_id="+strconv.FormatUint(pid, 10)) {
			t.Fatalf("%s 里有旧公式的行,CheckRowShards 应点名这张表与玩家: %v", table, err)
		}
		moveTo(table, oldShard, RowShard(pid)) // 改回这一张,下一张表的旧行应接着被判出
	}
	if err := CheckRowShards(ctx, e.db); err != nil {
		t.Fatalf("全部改回之后应通过: %v", err)
	}
}

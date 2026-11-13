//go:build integration

package inbox

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	inboxpb "proto/player_inbox"
	rfpb "proto/role_fence"
	"shared/drainproof"
	"shared/rolefence"
	"shared/serviceauth"
)

// 只给真实SQL调用加测试屏障，不替换任何身份判定或锁。
type observedSQLRoles struct {
	inner   RoleAuthority
	entered chan struct{}
	once    sync.Once
}

func (r *observedSQLRoles) RunTx(ctx context.Context, action func() error) error {
	return r.inner.RunTx(ctx, action)
}
func (r *observedSQLRoles) LockCurrent(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error) {
	r.once.Do(func() { close(r.entered) })
	return r.inner.LockCurrent(ctx, tx, player)
}
func (r *observedSQLRoles) VerifyAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) error {
	return r.inner.VerifyAdmission(ctx, admission)
}

// 串行点是副本行锁:冻结事务持锁时入箱排队,冻结先提交则入箱被拒,不留下新条目。
func TestIntegrationInboxEnqueueSharesFenceLockWithFreeze(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8101)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.directory.ensure(player)
	state, err := e.directory.GetRoleFenceState(ctx, player)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rolefence.Apply(ctx, e.db, state); err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = rolefence.Lock(ctx, tx, player); err != nil {
		t.Fatal(err)
	}
	observed := &observedSQLRoles{inner: e.store.roles, entered: make(chan struct{})}
	e.store.roles = observed
	result := make(chan error, 1)
	payload := gainPayload(t, 1)
	go func() {
		_, err := e.store.Enqueue(ctx, EnqueueParams{PlayerID: player, Type: gmBlock, Producer: inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM, RequestID: 1, IssuedAtMs: testNowMs, Payload: payload, Operator: "fixture"}, nil)
		result <- err
	}()
	select {
	case <-observed.entered:
	case <-ctx.Done():
		t.Fatal("未到真实副本行锁")
	}
	select {
	case err := <-result:
		t.Fatalf("持副本行锁期间入箱不得提前完成: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, err = rolefence.Apply(ctx, tx, e.directory.set(player, state.Account, state.OwnerRevision, 901)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, ErrOriginalRole) {
			t.Fatalf("冻结先提交后必须拒绝新入箱: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("副本行锁释放后入箱未结束")
	}
	var count int
	if err = e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM player_inbox_item WHERE shard=? AND player_id=?", RowShard(player), player).Scan(&count); err != nil || count != 0 {
		t.Fatalf("冻结后留下新条目: %d %v", count, err)
	}
}

func TestIntegrationInboxPermanentRequestCannotRebindAfterTransfer(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8102)
	first := e.mustEnqueue(t, player, 1)
	oldAdmission := e.admission(t, player, 1)
	_, err := e.store.Void(itCtx(t), VoidParams{PlayerID: player, Domain: gmDomain, Seq: first.Seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED, Operator: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.push(t, player, "new-inbox-owner", 2, 0)
	e.clock.advance(60000)
	if _, err = e.enqueue(t, player, 1, uint64(e.clock.ms.Load()), gainPayload(t, 1)); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("旧request即使刷新签发时刻也不能换主: %v", err)
	}
	// 终态行是永久收据,没有清理(设计 §9.1):原唯一行一直在,也不随换主补权。
	old := e.item(t, player, first.Seq)
	if old == nil || old.OwnerRevision != 1 || old.OwnerAccount == "new-inbox-owner" {
		t.Fatalf("永久原唯一行被删除/补权: %+v", old)
	}
	if _, err = e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{PlayerId: player, Domain: gmDomain, OwnerEpoch: 1, Admission: oldAdmission}); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("旧scene原件不得取得新主消息: %v", err)
	}
	second := e.mustEnqueue(t, player, 2)
	current := e.item(t, player, second.Seq)
	if current.OwnerAccount != "new-inbox-owner" || current.OwnerRevision != 2 {
		t.Fatalf("新意图未捕获新原件: %+v", current)
	}
	if _, err = e.enqueue(t, player, 1, uint64(e.clock.ms.Load()), gainPayload(t, 1)); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("清理入口后旧请求仍必须拒绝: %v", err)
	}
}

func TestIntegrationInboxRequiresExactRegisteredOpenAdmission(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8103)
	e.mustEnqueue(t, player, 1)
	registered := e.admission(t, player, 11)
	request := &inboxpb.FetchPlayerInboxRequest{PlayerId: player, Domain: gmDomain, OwnerEpoch: 11, Admission: registered}
	changed := proto.Clone(registered).(*base.RoleAdmissionIdentity)
	changed.SceneId++
	request.Admission = changed
	if _, err := e.store.Fetch(itCtx(t), request); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("同E不同完整原件不能消费: %v", err)
	}
	request.Admission = registered
	e.directory.closeAdmission(registered)
	if _, err := e.store.Fetch(itCtx(t), request); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("已封原世代不能复活: %v", err)
	}
	if _, err := e.db.Exec("UPDATE player_inbox_item SET owner_account='',owner_revision=0 WHERE shard=? AND player_id=?", RowShard(player), player); err != nil {
		t.Fatal(err)
	}
	request.OwnerEpoch = 12
	request.Admission = e.admission(t, player, 12)
	if _, err := e.store.Fetch(itCtx(t), request); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("历史缺owner不得从当前目录补权: %v", err)
	}
}

// 入场登记过了目录,但归属修订已经变了:事务里按副本核,旧世代不能消费。
func TestIntegrationInboxAdmissionOfOldRevisionRejectedInTx(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8104)
	e.mustEnqueue(t, player, 1)
	registered := e.admission(t, player, 21)
	e.push(t, player, "buyer", 2, 0)
	if _, err := e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{PlayerId: player, Domain: gmDomain, OwnerEpoch: 21, Admission: registered}); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("旧修订的入场不能消费: %v", err)
	}
}

func TestIntegrationInboxEveryMutationHonorsFrozenFence(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8201)
	first := e.mustEnqueue(t, player, 1)
	e.epochs.set(player, 1)
	admission := e.admission(t, player, 1)
	beforeItem, beforeStream, beforeAudit := e.item(t, player, first.Seq), e.stream(t, player), auditCount(t, e.db, player)
	e.push(t, player, "inbox-fixture-8201", 1, 901)
	actions := []struct {
		name string
		call func() error
	}{
		{"Enqueue", func() error { _, err := e.enqueue(t, player, 2, testNowMs, gainPayload(t, 2)); return err }},
		{"Fetch", func() error {
			_, err := e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{PlayerId: player, Domain: gmDomain, OwnerEpoch: 1, Admission: admission})
			return err
		}},
		{"Ack", func() error {
			_, err := e.store.Ack(itCtx(t), &inboxpb.AckPlayerInboxRequest{PlayerId: player, Domain: gmDomain, OwnerEpoch: 1, Admission: admission, Decisions: []*inboxpb.PlayerInboxDecisionReport{dec(first.Seq, inboxpb.PlayerInboxOutcome_PLAYER_INBOX_OUTCOME_APPLIED, 0, 1)}})
			return err
		}},
		{"Void", func() error {
			_, err := e.store.Void(itCtx(t), VoidParams{PlayerID: player, Domain: gmDomain, Seq: first.Seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED, Operator: "fixture"}, nil)
			return err
		}},
	}
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			if err := action.call(); !errors.Is(err, ErrOriginalRole) {
				t.Fatalf("冻结必须拒绝本次提交: %v", err)
			}
		})
	}
	if !proto.Equal(beforeItem, e.item(t, player, first.Seq)) || !proto.Equal(beforeStream, e.stream(t, player)) || beforeAudit != auditCount(t, e.db, player) {
		t.Fatal("冻结拒绝后Inbox原行/stream/审计发生变化")
	}
}

// 规格 §3.4:提醒标记是纯簿记,不过角色护栏。条目终结之后角色被冻结,补发照样能推迟、能清,不回错误:
// 回错误的话这一行每轮都打排空 Hold,还一直占着扫描名额。条目和审计都不动。
func TestIntegrationInboxNotifyBookkeepingSkipsRoleFence(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8206)
	e.notifier.setResult(NotifyError) // 入箱后那次提醒没落定,notify_due_ms 留着。
	first := e.mustEnqueue(t, player, 1)
	if _, err := e.store.Void(itCtx(t), VoidParams{PlayerID: player, Domain: gmDomain, Seq: first.Seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED, Operator: "fixture"}, nil); err != nil {
		t.Fatal(err)
	}
	e.push(t, player, "inbox-fixture-8206", 1, 901)
	relay := drainproof.New()
	e.store.RelayAdmission = relay
	beforeItem, beforeAudit, nextSeq := e.item(t, player, first.Seq), auditCount(t, e.db, player), e.stream(t, player).NextSeq

	// 没落定:推迟。
	if got := e.store.ResendNotify(itCtx(t), NotifyDue{PlayerID: player, Domain: gmDomain, NextSeq: nextSeq, PendingSinceMs: testNowMs}); got != NotifyError {
		t.Fatalf("ResendNotify = %s", got)
	}
	deferred := e.stream(t, player)
	if deferred.GetNotifyDueMs() <= testNowMs {
		t.Fatalf("角色冻结也要能推迟: %+v", deferred)
	}
	// 落定:清掉。
	e.notifier.setResult(NotifySent)
	if got := e.store.ResendNotify(itCtx(t), NotifyDue{PlayerID: player, Domain: gmDomain, NextSeq: nextSeq, PendingSinceMs: testNowMs}); got != NotifySent {
		t.Fatalf("ResendNotify = %s,清标记不该因为冻结失败", got)
	}
	if st := e.stream(t, player); st.GetNotifyDueMs() != 0 || st.GetNextSeq() != nextSeq {
		t.Fatalf("角色冻结也要能清: %+v", st)
	}
	// 旧令牌不改,CAS 的语义不变。
	if changed, err := e.store.ClearNotifyDue(itCtx(t), player, gmDomain, nextSeq); err != nil || changed {
		t.Fatalf("已经清过: changed=%t err=%v", changed, err)
	}
	if !proto.Equal(beforeItem, e.item(t, player, first.Seq)) || beforeAudit != auditCount(t, e.db, player) {
		t.Fatal("提醒簿记不该碰条目和审计")
	}
	relay.CloseAdmission()
	if err := relay.Wait(itCtx(t)); err != nil {
		t.Fatalf("确定没写的簿记不该打 Hold: %v", err)
	}
}

func TestIntegrationInboxFrozenFenceRejectsEvenEmptyFetch(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8203)
	e.epochs.set(player, 1)
	admission := e.admission(t, player, 1)
	e.push(t, player, "inbox-fixture-8203", 1, 901)
	_, err := e.store.Fetch(itCtx(t), &inboxpb.FetchPlayerInboxRequest{PlayerId: player, Domain: gmDomain, OwnerEpoch: 1, Admission: admission})
	if !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("空箱也必须核副本: %v", err)
	}
}

// 解冻推送丢了:副本还是冻结态,下一次访问读到权威状态之后通过。新主也一样,旧主被拒。
func TestIntegrationInboxLostPushHealsOnNextAccess(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8204)
	first := e.mustEnqueue(t, player, 1)
	e.push(t, player, "inbox-fixture-8204", 1, 901)
	e.directory.set(player, "inbox-fixture-8204", 1, 0)
	if _, err := e.store.Void(itCtx(t), VoidParams{PlayerID: player, Domain: gmDomain, Seq: first.Seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED, Operator: "fixture"}, nil); err != nil {
		t.Fatalf("解冻推送丢了不该挡住原主: %v", err)
	}
	e.push(t, player, "inbox-fixture-8204", 1, 902)
	e.directory.set(player, "buyer", 2, 0)
	if res := e.mustEnqueue(t, player, 2); e.item(t, player, res.Seq).OwnerAccount != "buyer" {
		t.Fatal("过户推送丢了,自愈之后新条目记新主")
	}
	if _, err := e.enqueue(t, player, 1, uint64(e.clock.ms.Load()), gainPayload(t, 1)); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("旧请求属于原主,过户之后不能重放: %v", err)
	}
}

// 目录里没有这名角色:不写副本,和原来缺行一样拒绝。
func TestIntegrationInboxRoleMissingFromDirectoryIsRejected(t *testing.T) {
	e := openIntegration(t)
	const player = uint64(8205)
	if err := e.store.ensureStream(itCtx(t), RowShard(player), player, gmDomain, testNowMs); !errors.Is(err, ErrOriginalRole) {
		t.Fatalf("err = %v", err)
	}
	var rows int
	if err := e.db.QueryRowContext(itCtx(t), "SELECT COUNT(*) FROM role_fence WHERE player_id=?", player).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("目录没有的角色不写副本: %d %v", rows, err)
	}
}

// ---- 挂起全集,从 before-stage-c 的 account_role_cross_domain_tidb_test.go 搬来,判据没变 ----

const inboxFenceCoordinatorSecret = "role-fence-inbox-test-only-coordinator-secret"

func (e *itEnv) prepareFreeze(t *testing.T, player, token uint64) *rfpb.PrepareRoleFreezeResponse {
	t.Helper()
	server, err := rolefence.NewParticipantServer(e.db, RolePending, inboxFenceCoordinatorSecret)
	if err != nil {
		t.Fatal(err)
	}
	in := &rfpb.PrepareRoleFreezeRequest{State: &rfpb.RoleFenceState{PlayerId: player, Account: "seller", OwnerRevision: 1, FreezeToken: token, Version: 2, HomeZone: 1}}
	if err = serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, rolefence.CoordinatorCaller, inboxFenceCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	out, err := server.PrepareRoleFreeze(itCtx(t), in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *itEnv) requireFreezeBlocked(t *testing.T, player uint64) {
	t.Helper()
	for _, token := range []uint64{88, rolefence.RemovalToken(player)} {
		out := e.prepareFreeze(t, player, token)
		if out.Clean || out.Blocker != roleBlockerItem {
			t.Fatalf("应被挡住: %+v", out)
		}
	}
	var rows int
	if err := e.db.QueryRowContext(itCtx(t), "SELECT COUNT(*) FROM role_fence WHERE player_id=?", player).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("有挂起时本库不留冻结: %d %v", rows, err)
	}
}

func (e *itEnv) insertRawItem(t *testing.T, player, seq uint64, state uint32) {
	t.Helper()
	_, err := e.db.Exec("INSERT INTO "+itemTable+" ("+itemColumns+") VALUES ("+placeholders(25)+")",
		RowShard(player), player, uint32(gmDomain), seq, uint32(gmBlock), uint32(inboxpb.PlayerInboxProducerKind_PLAYER_INBOX_PRODUCER_GM), seq, make([]byte, 32), []byte{},
		state, 0, 0, 0, 0, 0, 0, 0, 0, testNowMs, testNowMs, testNowMs, "", "", "seller", 1)
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationInboxPendingItemStatesBlockFreeze(t *testing.T) {
	for _, state := range []uint32{1, 0, 99} { // pending、未指定、未知
		e := openIntegration(t)
		e.insertRawItem(t, 11, 1, state)
		e.requireFreezeBlocked(t, 11)
		if _, err := e.db.Exec("UPDATE player_inbox_item SET state=2 WHERE player_id=11"); err != nil {
			t.Fatal(err)
		}
		if out := e.prepareFreeze(t, 11, 88); !out.Clean {
			t.Fatalf("state=%d 终结之后应通过: %+v", state, out)
		}
	}
}

func TestIntegrationInboxKnownItemTerminalsAllowFreeze(t *testing.T) {
	e := openIntegration(t)
	for _, state := range []uint32{2, 3, 4, 5} {
		e.insertRawItem(t, 11, uint64(state), state)
	}
	if out := e.prepareFreeze(t, 11, rolefence.RemovalToken(11)); !out.Clean {
		t.Fatalf("终态不阻塞: %+v", out)
	}
}

func TestIntegrationInboxNullItemStateRemainsBlocking(t *testing.T) {
	e := openIntegration(t)
	e.insertRawItem(t, 11, 1, 2)
	// 只破坏本例随机库,模拟绕过 schema 装配后的坏值:不能把 NULL 当成安全终态。
	if _, err := e.install.ExecContext(itCtx(t), "ALTER TABLE player_inbox_item MODIFY `state` INT UNSIGNED NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.install.ExecContext(itCtx(t), "UPDATE player_inbox_item SET `state`=NULL WHERE player_id=11"); err != nil {
		t.Fatal(err)
	}
	e.requireFreezeBlocked(t, 11)
}

//go:build integration

package inbox

import (
	"testing"
	"time"

	inboxpb "proto/player_inbox"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/serviceauth"
)

// 删角收尾(docs/design/account-deletion.md §6.2):他的条目与流行删掉,运维审计留着,别人的一行不动。
// 第二次调用没有可删的行,照样成功、什么都不变。
func TestIntegrationInboxPurgeRemovedRoleDeletesItemsAndStreamsKeepsAudit(t *testing.T) {
	e := openIntegration(t)
	const removed, other = uint64(8301), uint64(8302)
	items := []EnqueueResult{e.mustEnqueue(t, removed, 1), e.mustEnqueue(t, removed, 2)}
	e.mustEnqueue(t, other, 1)
	// 删角前挂起判定挡住未终结的条目:走到清理时条目都已终结。
	for _, item := range items {
		if _, err := e.store.Void(itCtx(t), VoidParams{PlayerID: removed, Domain: gmDomain, Seq: item.Seq, Reason: inboxpb.PlayerInboxVoidReason_PLAYER_INBOX_VOID_CANCELLED, Operator: "fixture"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	count := func(table string, player uint64) int {
		t.Helper()
		var n int
		if err := e.db.QueryRowContext(itCtx(t), "SELECT COUNT(*) FROM "+table+" WHERE `shard` = ? AND `player_id` = ?", RowShard(player), player).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	audits := count(auditTable, removed)
	if audits == 0 {
		t.Fatal("入箱与作废都该留下审计")
	}
	otherItems, otherStreams, otherAudits := count(itemTable, other), count(streamTable, other), count(auditTable, other)

	server, err := rolefence.NewParticipantServer(e.db, RolePending, RolePurge, inboxFenceCoordinatorSecret)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := e.directory.set(removed, "", 2, 0)
	for round := 1; round <= 2; round++ {
		in := &rfpb.PurgeRemovedRoleRequest{State: tombstone}
		if err = serviceauth.Sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in, rolefence.CoordinatorCaller, inboxFenceCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
			t.Fatal(err)
		}
		out, err := server.PurgeRemovedRole(itCtx(t), in)
		if err != nil || out.GetRetry() {
			t.Fatalf("第 %d 次清理应成功: %+v %v", round, out, err)
		}
		if got := count(itemTable, removed); got != 0 {
			t.Fatalf("第 %d 次清理之后还剩 %d 条条目", round, got)
		}
		if got := count(streamTable, removed); got != 0 {
			t.Fatalf("第 %d 次清理之后还剩 %d 条流行", round, got)
		}
		if got := count(auditTable, removed); got != audits {
			t.Fatalf("审计必须保留: %d -> %d", audits, got)
		}
		if count(itemTable, other) != otherItems || count(streamTable, other) != otherStreams || count(auditTable, other) != otherAudits {
			t.Fatal("别人的条目、流行、审计被动了")
		}
	}
}

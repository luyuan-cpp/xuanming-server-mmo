package main

// 复制连续性、目标账号、翻转标记、审计的状态机用例。替身在 mover_test.go。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shared/dbplacement"
)

func TestVerifyRecordsFeedAndSyncPointTime(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	h.store.records[testDB].Placement.Move.VerifiedFeed = nil
	h.repl.point.AtMs = 12345
	h.repl.point.PrimaryTs = 77
	if err := h.m.verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	move := h.placement().Move
	if move.VerifiedMs != 12345 {
		t.Fatalf("新鲜度从 syncpoint 的物理时间算,不从工具开始的时刻算: %d", move.VerifiedMs)
	}
	if move.VerifiedFeed == nil || *move.VerifiedFeed != testFeed || move.VerifiedPrimaryTs != 77 {
		t.Fatalf("要记下复制流: %+v %d", move.VerifiedFeed, move.VerifiedPrimaryTs)
	}
}

// 校验和算了很久,这段时间复制被重建:比过的数据和之后复制过来的数据之间可能有缺口。
func TestVerifyRefusesWhenFeedChangesWhileComparing(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	h.repl.feed.CreateTime = "2026-10-31 11:00:00"
	requireRefused(t, h.m.verify(context.Background()), "changed during verify")
	if h.placement().Move.VerifiedMs != 0 {
		t.Fatal("不能记成功")
	}
}

func TestVerifyRefusesWrongAdminAddress(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.sourceAdminAddr = "10.9.9.9:4000"
	requireRefused(t, h.m.verify(context.Background()), "the DSN points at")
}

// 窗口里复制断了:修好之后这段时间的写有没有丢只有重新校验才知道,所以清掉 verify 记录,重跑 cutover 要先 verify。
func TestCutoverBrokenReplicationClearsVerify(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.repl.caughtUp = func() (bool, error) { return false, errReplicationBroken }
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	move := h.placement().Move
	if move.VerifiedMs != 0 || move.VerifiedFeed != nil {
		t.Fatalf("要清掉 verify 记录: %+v", move)
	}
	h.repl.caughtUp = nil
	requireRefused(t, h.m.cutover(context.Background()), "no successful verify")
}

// 别的原因回滚(超预算)复制流没断过,verify 记录留着。
func TestCutoverOverBudgetKeepsVerify(t *testing.T) {
	h := newHarness(t)
	rec := h.seedMoving()
	h.m.o.budget = 1
	h.repl.caughtUp = func() (bool, error) { return false, nil }
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if got := h.placement().Move.VerifiedMs; got != rec.Placement.Move.VerifiedMs {
		t.Fatalf("超预算不动 verify 记录: %d", got)
	}
}

func TestPrepareLocksWritersBeforeCreatingTarget(t *testing.T) {
	h := newHarness(t)
	h.m.o.rosterPrincipals = []string{"writer_next@%"}
	h.targetAdmin.accounts["writer_next@%"] = true
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"writer@%", "writer_next@%"} {
		if !h.targetAdmin.locked[account] {
			t.Fatalf("%s 要锁上,名册里还没注册的也一样", account)
		}
	}
	if got := h.placement().Move.LockedAccounts; strings.Join(got, ",") != "writer@%,writer_next@%" {
		t.Fatalf("要记进映射,cutover 和 rollback 按它: %v", got)
	}
	h.log.mustPrecede(t, "store.put", "admin.lock")
	h.log.mustPrecede(t, "admin.lock", "admin.create")
	if h.targetAdmin.locked["maintenance@%"] {
		t.Fatal("维护账号要控制目标闸门,不能锁")
	}
}

func TestPrepareResumeLocksNewRosterAccount(t *testing.T) {
	h := newHarness(t)
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.m.o.rosterPrincipals = []string{"writer_next@%"}
	h.targetAdmin.accounts["writer_next@%"] = true
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if got := h.placement().Move.LockedAccounts; strings.Join(got, ",") != "writer@%,writer_next@%" {
		t.Fatalf("重跑要把新账号记进映射: %v", got)
	}
	if !h.targetAdmin.locked["writer_next@%"] {
		t.Fatal("新账号要锁上")
	}
}

func TestPrepareRefusesMissingAccountBeforeWriting(t *testing.T) {
	h := newHarness(t)
	h.m.o.rosterPrincipals = []string{"writer_next@%"}
	requireRefused(t, h.m.prepare(context.Background()), "writer_next@%")
	if h.log.has("store.put") || h.log.has("admin.lock") || h.log.has("admin.create") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}
}

// ACCOUNT LOCK 按账号生效:账号还在写目标集群上别的库时,锁了就把那些库一起断了。默认拒绝,确认之后不锁。
func TestPrepareSharedWriterAccount(t *testing.T) {
	shared := func(h *harness) {
		h.targetAdmin.grants = map[string][]string{"writer@%": {
			"GRANT USAGE ON *.* TO 'writer'@'%'",
			"GRANT SELECT,UPDATE ON `player_store_2_db`.`player_store` TO 'writer'@'%'",
		}}
	}
	h := newHarness(t)
	shared(h)
	requireRefused(t, h.m.prepare(context.Background()), "-allow-shared-writer-accounts")
	if h.log.has("store.put") || h.log.has("admin.lock") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}

	h = newHarness(t)
	shared(h)
	h.m.o.allowSharedAccounts = true
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.log.has("admin.lock") {
		t.Fatal("确认之后不锁")
	}
	move := h.placement().Move
	if len(move.LockedAccounts) != 0 || strings.Join(move.SharedAccounts, ",") != "writer@%" {
		t.Fatalf("映射要分开记: %+v", move)
	}

	// 只有这个库的授权(反向搬时旧授权还在)不算共用。
	h = newHarness(t)
	h.targetAdmin.grants = map[string][]string{"writer@%": {"GRANT SELECT ON `" + testDB + "`.`guild` TO 'writer'@'%'", "GRANT USAGE ON *.* TO 'writer'@'%'"}}
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.targetAdmin.locked["writer@%"] {
		t.Fatal("没有别的库的授权就要锁")
	}
}

func TestCutoverWriterConnectedToTargetRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.connections = []accountConnection{{User: "writer", Database: testDB, Count: 2}}
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !strings.Contains(err.Error(), "writer") {
		t.Fatalf("要点名账号: %v", err)
	}
	if h.log.has("admin.unlock") || h.log.has("target.release") {
		t.Fatalf("有写者连着不能开目标: %v", h.log.events)
	}
	if !h.targetAdmin.locked["writer@%"] {
		t.Fatal("回滚要保持锁定")
	}
	// 它连着的时候目标的闸门可能是开的,可能已经写过:修好之后不重新校验就切,目标带着这些写开放。
	if move := h.placement().Move; move.VerifiedMs != 0 || move.VerifiedFeed != nil {
		t.Fatalf("有写者连过目标要清掉 verify 记录: %+v", move)
	}
}

func TestCutoverConnectionsUnreadableRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.connErr = errors.New("denied")
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if h.log.has("target.release") {
		t.Fatal("看不到连接不能开目标")
	}
	if h.placement().Move.VerifiedMs == 0 {
		t.Fatal("只是没看到,没有证据目标被写过,verify 记录留着")
	}
}

func TestStrayConnections(t *testing.T) {
	move := &dbplacement.Move{LockedAccounts: []string{"locked@%"}, SharedAccounts: []string{"shared@%"}}
	connections := []accountConnection{
		{User: "locked", Database: "other_db", Count: 1},
		{User: "shared", Database: "other_db", Count: 5},
		{User: "shared", Database: testDB, Count: 1},
		{User: "maintenance", Database: testDB, Count: 1},
	}
	got := strings.Join(strayConnections(connections, move, testDB), ";")
	if !strings.Contains(got, "locked") || !strings.Contains(got, `shared x1 on database "`+testDB+`"`) {
		t.Fatalf("锁定的账号连哪都算,没锁的只看连到这个库的: %s", got)
	}
	if strings.Contains(got, "other_db\"") && strings.Contains(got, "shared x5") {
		t.Fatalf("还在写别的库的账号不算: %s", got)
	}
	if strings.Contains(got, "maintenance") {
		t.Fatalf("维护账号不算: %s", got)
	}
}

// 解锁之后回滚:解冻之后源库的闸门表复制过去,目标又是 writable=1,账号要先锁回去。
func TestCutoverAbortAfterUnlockRelocks(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.target.releaseErr = errors.New("denied")
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	h.log.mustPrecede(t, "admin.unlock", "admin.lock")
	h.log.mustPrecede(t, "admin.lock", "source.release")
	if !h.targetAdmin.locked["writer@%"] {
		t.Fatal("要锁回去")
	}
}

// 还没走到解锁就回滚:不碰目标,回滚常常就是因为目标连不上。
func TestCutoverAbortBeforeTargetPointLeavesAccountsAlone(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 1
	h.targetAdmin.lockErr = errors.New("target is down")
	h.repl.caughtUp = func() (bool, error) { return false, nil }
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if h.log.has("admin.lock") {
		t.Fatalf("没解锁过不用再锁: %v", h.log.events)
	}
}

// 锁不回去:源库照样解冻,业务要紧,但结局要报出来。
func TestCutoverRelockFailureStillUnfreezesAndIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.lockErr = errors.New("target is down")
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || !strings.Contains(err.Error(), "may still be unlocked") {
		t.Fatalf("要报账号可能还开着: %v", err)
	}
	if !h.source.writable() {
		t.Fatal("源库要解冻")
	}
}

// 崩在解锁之后:重跑 cutover 能接着做完,解锁幂等。
func TestCutoverUnlockIsIdempotentOnRerun(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.locked["writer@%"] = false
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.targetAdmin.locked["writer@%"] {
		t.Fatal("翻转之后账号是解锁的")
	}
}

// 崩在解锁之后,改走 rollback:账号要锁回去,而且排在解冻之前。
func TestRollbackRelocksTargetAccounts(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.locked["writer@%"] = false
	if _, err := h.source.FreezeWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.targetAdmin.locked["writer@%"] {
		t.Fatal("rollback 保持锁定")
	}
	h.log.mustPrecede(t, "admin.lock", "source.release")
	if !h.source.writable() {
		t.Fatal("要解冻")
	}
}

func TestRollbackCannotLockChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.lockErr = errors.New("target is down")
	err := h.m.rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "-target-lost") {
		t.Fatalf("要提示 -target-lost: %v", err)
	}
	if h.log.has("store.put") || h.log.has("source.release") {
		t.Fatalf("什么都不能动: %v", h.log.events)
	}
}

// etcd 回档:映射说没翻转,目标上却有标记。解冻的话两边各写一半。
func TestRollbackRefusedByFlipMarker(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.flipMarker = true
	requireRefused(t, h.m.rollback(context.Background()), "flip marker")
	if h.log.has("store.put") || h.log.has("source.release") || h.log.has("admin.lock") {
		t.Fatalf("什么都不能动: %v", h.log.events)
	}
}

func TestRollbackCannotReadMarkerRefuses(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.markerErr = errors.New("target is down")
	requireRefused(t, h.m.rollback(context.Background()), "-target-lost")
	if h.log.has("store.put") || h.log.has("source.release") {
		t.Fatalf("什么都不能动: %v", h.log.events)
	}
}

// 目标确实没了,回滚才是恢复服务的唯一办法。
func TestRollbackTargetLost(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	if _, err := h.source.FreezeWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	h.m.o.targetLost = true
	h.m.targetAdmin = nil
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.source.writable() {
		t.Fatal("要解冻")
	}
}

func TestCutoverFlipMarkerFailureAsksForRerun(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.target.sentinelErr = errors.New("denied")
	err := h.m.cutover(context.Background())
	if err == nil || exitCodeOf(err) != exitFailed || !strings.Contains(err.Error(), "Re-run cutover") {
		t.Fatalf("翻转已经做完,标记写不成要让人重跑: %v", err)
	}
	if p := h.placement(); p.Cluster != testTarget || !h.source.frozenBy(h.run) {
		t.Fatalf("翻转不受影响: %+v", p)
	}
	h.target.sentinelErr = nil
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if !h.target.sentinels[string(flipMarkerNonce(testRun))] {
		t.Fatal("重跑要补上标记")
	}
}

func TestCompareGateRows(t *testing.T) {
	run := "0123"
	source := gateRows{
		Meta: gateMetaRow{Generation: "ab", Writable: 0, WriterEpoch: 6, CatalogRevision: 9, FreezeRunID: run},
		Slots: []gateSlotRow{
			{WriterKey: "aa", Slot: 0, Generation: "ab", WriterEpoch: 6, State: 1},
			{WriterKey: "bb", Slot: 0, Generation: "ab", WriterEpoch: 2, State: 2}, // 已退休的 slot 不跟着 epoch 走
		},
	}
	released := gateRows{
		Meta: gateMetaRow{Generation: "ab", Writable: 1, WriterEpoch: 7, CatalogRevision: 9},
		Slots: []gateSlotRow{
			{WriterKey: "aa", Slot: 0, Generation: "ab", WriterEpoch: 7, State: 1},
			{WriterKey: "bb", Slot: 0, Generation: "ab", WriterEpoch: 2, State: 2},
		},
	}
	if got := compareGateRows(source, source); len(got) != 0 {
		t.Fatalf("站在记下的位置上两边相同: %v", got)
	}
	if got := compareGateRows(source, released); len(got) != 0 {
		t.Fatalf("正好差一次 release: %v", got)
	}
	for name, change := range map[string]func(*gateRows){
		"多一次 release": func(g *gateRows) { g.Meta.WriterEpoch = 8 },
		"世代不同":        func(g *gateRows) { g.Meta.Generation = "cd" },
		"注册表多了一笔":     func(g *gateRows) { g.Meta.CatalogRevision = 10 },
		"还冻着":         func(g *gateRows) { g.Meta.FreezeRunID = run },
		"slot 没跟上":    func(g *gateRows) { g.Slots[0].WriterEpoch = 6 },
		"退休的 slot 变了": func(g *gateRows) { g.Slots[1].WriterEpoch = 3 },
		"少一行":         func(g *gateRows) { g.Slots = g.Slots[:1] },
	} {
		target := gateRows{Meta: released.Meta, Slots: append([]gateSlotRow(nil), released.Slots...)}
		change(&target)
		if got := compareGateRows(source, target); len(got) == 0 {
			t.Fatalf("%s 要算不一致", name)
		}
	}
}

// audit 单独比闸门表,哨兵表不比:重跑时源库多一行新哨兵,和业务数据无关。
func TestAuditComparesGateSeparately(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.metrics["game:9101"] = 3
	guild := tableDigest{Structure: testStructure, Rows: 3, Digest: "abc"}
	h.sourceView.tables = map[string]tableDigest{"guild": guild, "route_meta": {Digest: "frozen"}, "writer_slot": {Digest: "frozen"}, "db_move_sentinel": {Digest: "two rows"}}
	h.targetView.tables = map[string]tableDigest{"guild": guild, "route_meta": {Digest: "opened"}, "writer_slot": {Digest: "opened"}, "db_move_sentinel": {Digest: "one row"}}
	h.sourceView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", WriterEpoch: 6, FreezeRunID: "0123"}}
	h.targetView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", Writable: 1, WriterEpoch: 7}}
	if err := h.m.audit(context.Background()); err != nil {
		t.Fatalf("正好差一次 release 不算不一致: %v", err)
	}

	h = newHarness(t)
	h.seedFlipped(false)
	h.metrics["game:9101"] = 3
	h.sourceView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", WriterEpoch: 6, FreezeRunID: "0123"}}
	h.targetView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", Writable: 1, WriterEpoch: 9}}
	err := h.m.audit(context.Background())
	if !errors.Is(err, errMismatch) || !strings.Contains(err.Error(), "route_meta") {
		t.Fatalf("多于一次 release 要报: %v", err)
	}
}

func TestAuditRefusesWrongAdminAddress(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.metrics["game:9101"] = 3
	h.m.o.targetAdminAddr = "10.9.9.9:4000"
	requireRefused(t, h.m.audit(context.Background()), "the DSN points at")
}

// seedOpenedTarget 把状态放到窗口打开过目标闸门之后:源库被这个 run 冻结,目标闸门比它多一次 release,记过目标位置。
func (h *harness) seedOpenedTarget(t *testing.T) {
	t.Helper()
	h.seedMoving()
	h.store.records[testDB].Placement.Move.TargetPoint = "point-before-open"
	if _, err := h.source.FreezeWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	h.repl.replicate()
	if _, err := h.target.ReleaseWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	// 冻结没有变化,复制过去的只有新的哨兵;目标闸门表不被覆盖。
	h.repl.caughtUp = func() (bool, error) {
		for nonce := range h.source.sentinels {
			h.target.sentinels[nonce] = true
		}
		return true, nil
	}
}

var strayWriter = []accountConnection{{User: "writer", Database: testDB, Count: 2}}

// etcd 回档之后映射说没翻转,标记还在:重跑 cutover 走进窗口,看到目标上的写者就回滚、解冻,两边各写一半。
func TestCutoverRefusedByFlipMarker(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.flipMarker = true
	requireRefused(t, h.m.cutover(context.Background()), "flip marker")
	if h.log.has("source.freeze") || h.log.has("store.put") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}
}

func TestCutoverCannotReadMarkerRefuses(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.markerErr = errors.New("target is down")
	requireRefused(t, h.m.cutover(context.Background()), "cannot read the flip marker")
	if h.log.has("source.freeze") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}
}

// 重跑 prepare 要锁账号:etcd 回档过的话目标正在收写入,锁了就把写者断了。
func TestPrepareResumeRefusedByFlipMarker(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.targetAdmin.flipMarker = true
	requireRefused(t, h.m.prepare(context.Background()), "flip marker")
	if h.log.has("admin.lock") {
		t.Fatalf("不能锁账号: %v", h.log.events)
	}
}

// 重跑 prepare 也要核连接串:指错了就把账号锁在别的集群上。
func TestPrepareResumeRefusesWrongAddress(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.targetAddr = "10.9.9.9:4000"
	requireRefused(t, h.m.prepare(context.Background()), "the DSN points at")
	if h.log.has("admin.lock") {
		t.Fatalf("不能锁账号: %v", h.log.events)
	}
}

// 翻转落地了但标记还没写,然后 etcd 回档:目标闸门开过、写者连着。rollback 不能解冻。
func TestRollbackHoldsWhenOpenedTargetHasWriters(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	h.targetAdmin.connections = strayWriter
	requireRefused(t, h.m.rollback(context.Background()), "cannot rule out that the flip took effect")
	if !h.source.frozenBy(h.run) {
		t.Fatal("源库要保持冻结")
	}
	if h.log.has("admin.lock") || h.placement().State != dbplacement.StateMoving {
		t.Fatalf("拒绝时什么都不能动: %v %+v", h.log.events, h.placement())
	}
}

func TestRollbackHoldsWhenTargetGateUnreadable(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	h.targetAdmin.gateErr = errors.New("target is down")
	requireRefused(t, h.m.rollback(context.Background()), "cannot rule out that the flip took effect")
	if !h.source.frozenBy(h.run) {
		t.Fatal("源库要保持冻结")
	}
}

// 窗口崩在打开之后、翻转之前,没有写者连着:rollback 照常做。
func TestRollbackAfterOpenedTargetWithoutWriters(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.source.writable() || !h.targetAdmin.locked["writer@%"] {
		t.Fatalf("要解冻、要锁回账号: %+v %v", h.source.meta, h.targetAdmin.locked)
	}
}

// 复制期间目标的闸门表是源库的副本,本来就可写,不能当成窗口打开过。
func TestRollbackNotHeldByCopiedGate(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.repl.replicate()
	h.targetAdmin.connections = strayWriter
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.source.writable() || h.placement().State != dbplacement.StateActive {
		t.Fatalf("回滚结果: %+v %+v", h.source.meta, h.placement())
	}
}

// 重跑窗口碰到同样的局面:不解冻,退出码 3。
func TestCutoverAbortHoldsWhenOpenedTargetHasWriters(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	h.targetAdmin.connections = strayWriter
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || !strings.Contains(err.Error(), "may have taken effect") {
		t.Fatalf("要报翻转可能已经生效: %v", err)
	}
	if !h.source.frozenBy(h.run) {
		t.Fatal("源库要保持冻结")
	}
	if h.log.has("store.flip") {
		t.Fatalf("不能翻转: %v", h.log.events)
	}
}

func TestCutoverAbortHoldsWhenTargetGateUnreadable(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	h.m.o.budget = 30 * time.Millisecond
	h.targetAdmin.gateErr = errors.New("target is down")
	h.repl.caughtUp = func() (bool, error) { return false, nil }
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || !h.source.frozenBy(h.run) {
		t.Fatalf("说不准就不解冻: %v %+v", err, h.source.meta)
	}
}

// 上一次窗口走到过翻转附近,它发出的翻转可能还没有结局。这次即使还没发翻转,etcd 不通时也不能解冻。
func TestCutoverEtcdDownAfterEarlierWindowKeepsSourceFrozen(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	h.m.o.budget = 30 * time.Millisecond
	h.m.o.rollbackBudget = 30 * time.Millisecond
	h.repl.caughtUp = func() (bool, error) {
		h.store.getErr = errEtcdDown
		return false, nil
	}
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || !h.source.frozenBy(h.run) {
		t.Fatalf("要保持冻结: %v %+v", err, h.source.meta)
	}
}

// 预检之后复制流被删了重建:新流一样能追上冻结点,目标也读得到哨兵,中间那段写却不在目标上。
func TestCutoverFeedChangedInWindowRollsBackAndClearsVerify(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.repl.caughtUp = func() (bool, error) {
		h.repl.replicate()
		h.repl.feed.CreateTime = "2026-10-31 11:00:00"
		return true, nil
	}
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if move := h.placement().Move; move.VerifiedMs != 0 || move.VerifiedFeed != nil {
		t.Fatalf("要清掉 verify 记录: %+v", move)
	}
	if h.log.has("target.release") || h.log.has("store.flip") {
		t.Fatalf("不能打开目标: %v", h.log.events)
	}
}

// 从全量重来会删库重建目标:复制流的身份可能都没变,只有目标闸门表的建表时间变了。旧的 verify 记录不算数。
func TestCutoverRefusesRebuiltTarget(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.repl.feed.TargetCreated = "2026-10-31 11:00:00"
	requireRefused(t, h.m.cutover(context.Background()), "changed since the last verify")
	if h.log.has("source.freeze") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}
}

// 并发跑的 rollback 把账号锁上,翻转赢了:重跑 cutover 要解锁,不然新连接全部失败。
func TestCutoverAlreadyDoneUnlocksAccounts(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.store.records[testDB].Placement.Move.LockedAccounts = []string{"writer@%"}
	h.targetAdmin.locked["writer@%"] = true
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.targetAdmin.locked["writer@%"] {
		t.Fatal("要解锁")
	}
}

func TestCutoverAlreadyDoneUnlockFailureAsksForRerun(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.store.records[testDB].Placement.Move.LockedAccounts = []string{"writer@%"}
	h.targetAdmin.lockErr = errors.New("denied")
	err := h.m.cutover(context.Background())
	if exitCodeOf(err) != exitFailed || !strings.Contains(err.Error(), "Re-run cutover") {
		t.Fatalf("要提示重跑: %v", err)
	}
}

// 记成共享的账号不在解锁名单里。它其实锁着的话,翻转之后这个库连不上,也回不去。
func TestCutoverRefusesLockedSharedAccount(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	move := h.store.records[testDB].Placement.Move
	move.LockedAccounts, move.SharedAccounts = nil, []string{"writer@%"}
	h.targetAdmin.locked["writer@%"] = true
	requireRefused(t, h.m.cutover(context.Background()), "shared writer accounts are locked")
	if h.log.has("source.freeze") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}

	h.targetAdmin.locked["writer@%"] = false
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("没锁就能切: %v", err)
	}
}

// 管理员看不到别的账号的连接时,查连接不报错,只是查不到。冻结之前就要拦住。
func TestAdminThatCannotSeeConnectionsIsRefused(t *testing.T) {
	h := newHarness(t)
	h.targetAdmin.seeAllErr = errors.New("no PROCESS")
	requireRefused(t, h.m.plan(context.Background()), "no PROCESS")

	h = newHarness(t)
	h.seedMoving()
	h.targetAdmin.seeAllErr = errors.New("no PROCESS")
	requireRefused(t, h.m.cutover(context.Background()), "no PROCESS")
	if h.log.has("source.freeze") {
		t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
	}
}

// 窗口崩在打开目标闸门之后再跑 verify:目标的闸门表正好多一次 release,这不是数据差异。多于一次才算。
func TestVerifyToleratesOneReleaseOnTargetGate(t *testing.T) {
	seed := func() *harness {
		h := newHarness(t)
		h.seedMoving()
		guild := tableDigest{Structure: testStructure, Rows: 3, Digest: "abc"}
		h.sourceView.tables = map[string]tableDigest{"guild": guild, "route_meta": {Digest: "frozen"}}
		h.targetView.tables = map[string]tableDigest{"guild": guild, "route_meta": {Digest: "open"}}
		h.sourceView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", Writable: 0, WriterEpoch: 6, FreezeRunID: "0123"}}
		return h
	}
	h := seed()
	h.targetView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", Writable: 1, WriterEpoch: 7}}
	if err := h.m.verify(context.Background()); err != nil {
		t.Fatalf("多一次 release: %v", err)
	}
	h = seed()
	h.targetView.gate = gateRows{Meta: gateMetaRow{Generation: "ab", Writable: 1, WriterEpoch: 8}}
	if err := h.m.verify(context.Background()); !errors.Is(err, errMismatch) {
		t.Fatalf("多于一次 release 要算不一致: %v", err)
	}
}

func TestFeedChange(t *testing.T) {
	verified := testFeed
	if got := feedChange(&verified, testFeed); got != "" {
		t.Fatalf("同一条流、同一份配置: %s", got)
	}
	if got := feedChange(nil, testFeed); !strings.Contains(got, "no verified replication stream") {
		t.Fatalf("映射里没有记录: %s", got)
	}
	tests := []struct {
		name   string
		change func(feed *dbplacement.FeedIdentity)
		want   string
	}{
		{"只改了过滤", func(feed *dbplacement.FeedIdentity) { feed.ConfigDigest = "config-2" }, "the config of the replication stream changed"},
		{"同名删了重建", func(feed *dbplacement.FeedIdentity) { feed.CreateTime = "2026-10-31 11:00:00" }, "the replication stream changed"},
		{"重建的同时改了过滤", func(feed *dbplacement.FeedIdentity) { feed.StartTs, feed.ConfigDigest = 999, "config-2" }, "the replication stream changed"},
		{"目标删库重建", func(feed *dbplacement.FeedIdentity) { feed.TargetCreated = "2026-10-31 11:00:00" }, "the replication stream changed"},
	}
	for _, test := range tests {
		now := testFeed
		test.change(&now)
		// 比开头:「配置变了」那句话里也含有「the replication stream changed」。
		if got := feedChange(&verified, now); !strings.HasPrefix(got, test.want) {
			t.Fatalf("%s: %s", test.name, got)
		}
	}
}

// 预检之后有人暂停 changefeed、改了过滤再恢复:id、start_ts、create_time 都没变,只有配置摘要变了。
// 新的过滤一样能追上冻结点,被滤掉的写却不在目标上:按复制断开处理,回滚并清掉 verify 记录。
func TestCutoverFeedConfigChangedInWindowRollsBackAndClearsVerify(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.repl.caughtUp = func() (bool, error) {
		h.repl.replicate()
		h.repl.feed.ConfigDigest = "config-2"
		return true, nil
	}
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !errors.Is(err, errReplicationBroken) || !strings.Contains(err.Error(), "the config of the replication stream changed") {
		t.Fatalf("要说清楚是配置变了: %v", err)
	}
	if move := h.placement().Move; move.VerifiedMs != 0 || move.VerifiedFeed != nil {
		t.Fatalf("要清掉 verify 记录: %+v", move)
	}
	if h.log.has("view.source") || h.log.has("target.release") || h.log.has("store.flip") {
		t.Fatalf("不能往下走: %v", h.log.events)
	}
}

// 追平之后两边都不动,再比一次全量:读当前数据,不带快照。比过了才记目标位置、解锁账号、开目标闸门。
func TestCutoverComparesQuiescentSidesBeforeOpeningTarget(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	// 上一次 verify 留下的快照位置不能带进窗口。
	h.sourceView.snapshot, h.targetView.snapshot = "100", "200"
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][2]string{
		{"source.freeze", "view.source"},
		{"repl.mark", "view.source"},
		{"view.target", "store.put"},
		{"view.target", "admin.unlock"},
		{"view.target", "target.release"},
		{"view.target", "store.flip"},
	} {
		h.log.mustPrecede(t, order[0], order[1])
	}
	if h.sourceView.snapshot != "" || h.targetView.snapshot != "" {
		t.Fatalf("两边静止,读当前数据: %q %q", h.sourceView.snapshot, h.targetView.snapshot)
	}
	if !h.sourceView.closed || !h.targetView.closed {
		t.Fatal("视图要关")
	}
	move := h.placement().Move
	if move.FinalCompareSkipped || move.FeedEndpoint != testFeedEndpoint {
		t.Fatalf("翻转要记下比过了,以及复制流是从哪个地址读的: %+v", move)
	}
	if h.errOut.Len() != 0 {
		t.Fatalf("没有放弃保护就不该有告警: %s", h.errOut)
	}
}

// verify 之后复制被跳过一段(changefeed 用 --overwrite-checkpoint-ts 恢复,往目标注入空事务跳过 GTID):
// 位置和哨兵都对得上,只有数据对不上。不翻转,回滚,清掉 verify 记录。
func TestCutoverFinalCompareMismatchRollsBackAndClearsVerify(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifyTookMs = 100
	h.targetView.tables = map[string]tableDigest{"guild": {Structure: testStructure, Rows: 2, Digest: "abc"}}
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !errors.Is(err, errTargetDiverged) || errors.Is(err, errMismatch) || !strings.Contains(err.Error(), "guild: source rows=3") {
		t.Fatalf("要点名不一致的表,退出码按回滚算: %v", err)
	}
	move := h.placement().Move
	if move.VerifiedMs != 0 || move.VerifiedFeed != nil || move.VerifyTookMs != 0 {
		t.Fatalf("要清掉 verify 记录: %+v", move)
	}
	if move.TargetPoint != "" {
		t.Fatalf("比不过就没走到记目标位置: %q", move.TargetPoint)
	}
	if h.log.has("admin.unlock") || h.log.has("admin.lock") || h.log.has("target.release") || h.log.has("store.flip") {
		t.Fatalf("不一致就不碰目标、不翻转: %v", h.log.events)
	}
	// 回滚的顺序不变:先作废翻转,再解冻。
	h.log.mustPrecede(t, "store.put", "source.release")
	// 记录清掉了,不重新 verify 不能再切。
	requireRefused(t, h.m.cutover(context.Background()), "no successful verify")
}

// 上一次窗口崩在打开目标闸门之后,这一次静止比对不一致:回滚照旧先作废翻转、把账号锁回去、再解冻。
func TestCutoverFinalCompareMismatchAfterOpenedTargetRelocksThenUnfreezes(t *testing.T) {
	h := newHarness(t)
	h.seedOpenedTarget(t)
	h.targetView.tables = map[string]tableDigest{"guild": {Structure: testStructure, Rows: 4, Digest: "abc"}}
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !errors.Is(err, errTargetDiverged) {
		t.Fatalf("要报两边不一致: %v", err)
	}
	h.log.mustPrecede(t, "store.put", "admin.lock")
	h.log.mustPrecede(t, "admin.lock", "source.release")
	if !h.targetAdmin.locked["writer@%"] {
		t.Fatal("账号要锁回去")
	}
	if h.log.has("store.flip") {
		t.Fatalf("不能翻转: %v", h.log.events)
	}
}

// 比对本身没做成:回滚,但没有证据说数据不一致,verify 记录留着。底下哪怕是一条拒绝,冻结过又回滚的退出码也是 2。
func TestCutoverFinalCompareFailureRollsBackKeepsVerify(t *testing.T) {
	h := newHarness(t)
	rec := h.seedMoving()
	h.targetView.digestErr = noPrimaryKeyError("guild")
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if errors.Is(err, errRefused) || errors.Is(err, errTargetDiverged) || !strings.Contains(err.Error(), "table guild has no primary key") {
		t.Fatalf("要带上原因,按回滚报: %v", err)
	}
	if got := h.placement().Move.VerifiedMs; got != rec.Placement.Move.VerifiedMs {
		t.Fatalf("没比完不动 verify 记录: %d", got)
	}
	if h.log.has("target.release") || h.log.has("store.flip") {
		t.Fatalf("没比完不能开目标: %v", h.log.events)
	}
}

// 库太大,窗口里比不完:到预算就回滚,提示加大 -budget。
func TestCutoverFinalCompareOverBudgetRollsBack(t *testing.T) {
	h := newHarness(t)
	rec := h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.sourceView.stall = true
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !strings.Contains(err.Error(), "-budget=30ms") {
		t.Fatalf("要说明是预算里没比完: %v", err)
	}
	if got := h.placement().Move.VerifiedMs; got != rec.Placement.Move.VerifiedMs {
		t.Fatalf("超预算不动 verify 记录: %d", got)
	}
	if h.log.has("target.release") || h.log.has("store.flip") {
		t.Fatalf("没比完不能开目标: %v", h.log.events)
	}
}

// 超大库显式跳过静止比对:不比,也不按 verify 的耗时卡预算;但要在标准错误输出告警,并把「跳过了」和翻转一起记进映射。
func TestCutoverSkipFinalCompareWarnsAndRecords(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.skipFinalCompare = true
	h.store.records[testDB].Placement.Move.VerifyTookMs = uint64(time.Hour.Milliseconds())
	// 两边其实不一样也看不出来,这正是跳过的代价。
	h.targetView.tables = map[string]tableDigest{"guild": {Structure: testStructure, Rows: 2, Digest: "abc"}}
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.log.has("view.source") || h.log.has("view.target") {
		t.Fatalf("跳过就不比: %v", h.log.events)
	}
	if !strings.Contains(h.errOut.String(), "WARNING: [cutover] -skip-final-compare is set") {
		t.Fatalf("要在标准错误输出告警: %q", h.errOut.String())
	}
	if strings.Contains(h.out.String(), "WARNING") {
		t.Fatalf("告警走标准错误输出,不混在步骤输出里: %q", h.out.String())
	}
	if p := h.placement(); p.Cluster != testTarget || !p.Move.FinalCompareSkipped {
		t.Fatalf("跳过要记进映射: %+v %+v", p, p.Move)
	}
}

// 表没有主键:verify 拒绝并点名,不记成功。
func TestVerifyRefusesTableWithoutPrimaryKey(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	h.sourceView.digestErr = noPrimaryKeyError("guild")
	requireRefused(t, h.m.verify(context.Background()), "table guild has no primary key")
	if h.placement().Move.VerifiedMs != 0 {
		t.Fatal("不能记成功")
	}
}

// 数据一样、表定义不一样(目标把唯一索引建成了普通索引):verify 判不一致,报出差在哪一行定义。
func TestVerifyStructureMismatchRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	h.sourceView.tables = map[string]tableDigest{"guild": {Structure: "column 1 \"id\"\nunique index \"uk_name\"", Rows: 3, Digest: "abc"}}
	h.targetView.tables = map[string]tableDigest{"guild": {Structure: "column 1 \"id\"\nindex \"uk_name\"", Rows: 3, Digest: "abc"}}
	err := h.m.verify(context.Background())
	if !errors.Is(err, errMismatch) || !strings.Contains(err.Error(), "the table definition differs") || !strings.Contains(err.Error(), `only on the source [unique index "uk_name"]`) {
		t.Fatalf("定义不同要算不一致: %v", err)
	}
	if h.placement().Move.VerifiedMs != 0 {
		t.Fatal("不一致不能记成功")
	}
}

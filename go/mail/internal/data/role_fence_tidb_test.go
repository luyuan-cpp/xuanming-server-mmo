package data

// 角色护栏接到 mail 之后的真库回归(docs/design/horizontal-scale-logical-db.md §3)。门控同 repo_integration_test.go:MAIL_RANK_TEST_TIDB。
// 目录是替身,副本表和挂起 SQL 都是真的。挂起用例从 before-stage-c 的 account_role_cross_domain_tidb_test.go 搬来,判据没变。

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	basepb "proto/common/base"
	dspb "proto/data_service"
	mailpb "proto/mail"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/roleidentity"
	"shared/serviceauth"
)

const mailFenceCoordinatorSecret = "role-fence-mail-test-only-coordinator-secret"

func mailFenceActor(t *testing.T, account string, revision uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &basepb.AccountSimplePlayer{PlayerId: 7}})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// 冻结写进了副本,过户提交,推送丢了。新主先到:副本修订号落后,自愈后通过;旧主被拒。
func TestTiDBMailNewOwnerHealsStaleFrozenReplicaOldOwnerRejected(t *testing.T) {
	ctx, db, r := mailFixture(t)
	directory := r.guilds.(*mailTestDirectory)
	r.ConfigureRoleFence(rolefence.NewGuard(db, directory))
	if _, err := rolefence.Apply(ctx, db, directory.setRole(7, "原卖家", 1, 88)); err != nil {
		t.Fatal(err)
	}
	directory.setRole(7, "新买家", 2, 0)
	buyer, seller := mailFenceActor(t, "新买家", 2), mailFenceActor(t, "原卖家", 1)
	if _, err := r.List(buyer, 7, &mailpb.ListMailsRequest{}); err != nil {
		t.Fatalf("新主自愈后应通过: %v", err)
	}
	if directory.readCount() != 1 {
		t.Fatalf("自愈读了 %d 次权威状态", directory.readCount())
	}
	if _, err := r.List(seller, 7, &mailpb.ListMailsRequest{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("旧主应被拒: %v", err)
	}
	if directory.readCount() != 1 {
		t.Fatal("副本修订号更大时直接拒绝,不打 RPC")
	}
}

func TestTiDBMailFrozenRoleRejectsClaimWithoutSideEffects(t *testing.T) {
	ctx, db, r := mailFixture(t)
	directory := r.guilds.(*mailTestDirectory)
	r.ConfigureRoleFence(rolefence.NewGuard(db, directory))
	input := personal(801, 7)
	input.SenderKind = SenderKind("admin")
	sent, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, input)
	if err != nil {
		t.Fatal(err)
	}
	// 副本还没有这一行:第一次写入先自愈,读到冻结态,拒绝。
	directory.setRole(7, "原卖家", 1, 88)
	owner := mailFenceActor(t, "原卖家", 1)
	if _, err = r.Claim(owner, 7, sent.MailId); !errors.Is(err, ErrDenied) {
		t.Fatalf("冻结后领取应被拒: %v", err)
	}
	var ops int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_asset_op").Scan(&ops); err != nil || ops != 0 {
		t.Fatalf("拒绝不能留下资产操作: %d %v", ops, err)
	}
	// 解冻推送丢了:副本还是冻结态,下一次访问读到权威状态之后通过。
	directory.setRole(7, "原卖家", 1, 0)
	if _, err = r.Claim(owner, 7, sent.MailId); err != nil {
		t.Fatalf("解冻后领取应通过: %v", err)
	}
}

// 目录里没有的角色不写副本:和原来缺行一样不放行,只是从存储错误变成明确拒绝。
func TestTiDBMailActorMissingFromDirectoryIsDenied(t *testing.T) {
	_, db, r := mailFixture(t)
	r.ConfigureRoleFence(rolefence.NewGuard(db, r.guilds.(*mailTestDirectory)))
	if _, err := r.List(mailFenceActor(t, "原卖家", 1), 7, &mailpb.ListMailsRequest{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v", err)
	}
}

// 读不到所在的帮只藏帮会文本邮件,个人邮件照常:不能因为帮会服务抖动让玩家打不开邮箱。
func TestTiDBGuildLookupFailureOnlyHidesGuildText(t *testing.T) {
	ctx, _, r := mailFixture(t)
	directory := r.guilds.(*mailTestDirectory)
	directory.setGuild(7, 77)
	if _, err := r.Send(ctx, "guild", mailpb.MailKind_MAIL_KIND_GUILD, &mailpb.SendMailRequest{SenderKind: 2, RequestId: 1, GuildId: 77, Title: "帮会通知", Body: "当前成员可读"}); err != nil {
		t.Fatal(err)
	}
	input := personal(802, 7)
	input.SenderKind = SenderKind("admin")
	if _, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, input); err != nil {
		t.Fatal(err)
	}
	page, err := r.List(ctx, 7, &mailpb.ListMailsRequest{})
	if err != nil || len(page.Mails) != 2 {
		t.Fatalf("成员看得到两封: %v %+v", err, page)
	}
	directory.setGuildErr(errors.New("guild unavailable"))
	page, err = r.List(ctx, 7, &mailpb.ListMailsRequest{})
	if err != nil || len(page.Mails) != 1 || page.Mails[0].Kind != mailpb.MailKind_MAIL_KIND_PERSONAL {
		t.Fatalf("帮会服务不可用时只藏帮会文本: %v %+v", err, page)
	}
}

// ---- 挂起全集 ----

func mailPrepareFreeze(t *testing.T, ctx context.Context, db *sql.DB, token uint64) *rfpb.PrepareRoleFreezeResponse {
	t.Helper()
	server, err := rolefence.NewParticipantServer(db, RolePending, mailFenceCoordinatorSecret)
	if err != nil {
		t.Fatal(err)
	}
	in := &rfpb.PrepareRoleFreezeRequest{State: &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 1, FreezeToken: token, Version: 2, HomeZone: 1}}
	if err = serviceauth.Sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in, rolefence.CoordinatorCaller, mailFenceCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	out, err := server.PrepareRoleFreeze(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// 过户和删角都被挡住,本库不留冻结。
func mailRequireBlocked(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, token := range []uint64{88, rolefence.RemovalToken(11)} {
		out := mailPrepareFreeze(t, ctx, db, token)
		if out.Clean || out.Blocker != roleBlockerAssetOp {
			t.Fatalf("应被挡住: %+v", out)
		}
	}
	var frozen int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM role_fence WHERE player_id=11").Scan(&frozen); err != nil || frozen != 0 {
		t.Fatalf("有挂起时本库不留冻结: %d %v", frozen, err)
	}
}

func insertMailAssetOp(t *testing.T, ctx context.Context, install *sql.DB, opID, seq uint64, status uint32) {
	t.Helper()
	_, err := install.ExecContext(ctx, "INSERT INTO mail_asset_op (op_id,player_id,stream,seq,stream_epoch,mail_id,claim_version,status,payload,tx_type,attempts,next_attempt_ms,lease_until_ms,lease_token,last_outcome,last_reason,durable,reason_tip_id,created_ms,updated_ms,last_send_ms,resolved_by,resolve_reason) VALUES (?,11,0,?,0,0,0,?,?,0,0,0,0,0,0,0,0,0,0,0,0,'','')", opID, seq, status, []byte{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTiDBMailPendingAssetOpStatesBlockFreeze(t *testing.T) {
	for _, status := range []uint32{1, 4, 0, 99} { // pending、manual、未指定、未知
		ctx, db, _, install := mailFixtureWithInstall(t)
		insertMailAssetOp(t, ctx, install, 51, 1, status)
		mailRequireBlocked(t, ctx, db)
		if _, err := install.ExecContext(ctx, "DELETE FROM mail_asset_op WHERE player_id=11"); err != nil {
			t.Fatal(err)
		}
		if out := mailPrepareFreeze(t, ctx, db, 88); !out.Clean {
			t.Fatalf("status=%d 清掉之后应通过: %+v", status, out)
		}
	}
}

func TestTiDBMailKnownAssetTerminalsAllowFreeze(t *testing.T) {
	ctx, db, _, install := mailFixtureWithInstall(t)
	for index, status := range []uint32{2, 3, 5} {
		insertMailAssetOp(t, ctx, install, 300+uint64(index), uint64(index+1), status)
	}
	if out := mailPrepareFreeze(t, ctx, db, rolefence.RemovalToken(11)); !out.Clean {
		t.Fatalf("终态不阻塞: %+v", out)
	}
}

func TestTiDBMailNullAssetStateRemainsBlocking(t *testing.T) {
	ctx, db, _, install := mailFixtureWithInstall(t)
	insertMailAssetOp(t, ctx, install, 51, 1, 2)
	// 只破坏本例随机库,模拟绕过 schema 装配后的坏值:不能把 NULL 当成安全终态。
	if _, err := install.ExecContext(ctx, "ALTER TABLE `mail_asset_op` MODIFY `status` INT UNSIGNED NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := install.ExecContext(ctx, "UPDATE `mail_asset_op` SET `status`=NULL WHERE player_id=11"); err != nil {
		t.Fatal(err)
	}
	mailRequireBlocked(t, ctx, db)
}

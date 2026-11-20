package data

// 删角收尾清数据的真库回归(docs/design/account-deletion.md §6.2)。门控同 repo_integration_test.go:MAIL_RANK_TEST_TIDB。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	mailpb "proto/mail"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/serviceauth"
)

// mailPurge 走真的 PurgeRemovedRole:鉴权、按版本合并墓碑、锁副本、清理,同一个事务。
func mailPurge(t *testing.T, ctx context.Context, db *sql.DB, state *rfpb.RoleFenceState) {
	t.Helper()
	server, err := rolefence.NewParticipantServer(db, RolePending, RolePurge, mailFenceCoordinatorSecret)
	if err != nil {
		t.Fatal(err)
	}
	in := &rfpb.PurgeRemovedRoleRequest{State: state}
	if err = serviceauth.Sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in, rolefence.CoordinatorCaller, mailFenceCoordinatorSecret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	out, err := server.PurgeRemovedRole(ctx, in)
	if err != nil || out.GetRetry() {
		t.Fatalf("清理应一次成功: %+v %v", out, err)
	}
}

func mailCount(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// mailCampaign 建一份两人名单(7、9)的系统活动邮件,导入唯一的一片;seal 为真时封存发布。
func mailCampaign(t *testing.T, ctx context.Context, r *Repo, campaign, request uint64, seal bool) {
	t.Helper()
	hash, _, err := ShardHash([]uint64{7, 9})
	if err != nil {
		t.Fatal(err)
	}
	manifest := sha256.Sum256([]byte(fmt.Sprintf("v1\n0:%s:2\n", hash)))
	send := &mailpb.SendMailRequest{SenderKind: 1, RequestId: request, Title: "活动奖", Body: "两人名单", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 5}}},
		CampaignId: campaign, ManifestGeneration: 1, ExpectedRecipientCount: 2, ExpectedShardCount: 1, ManifestHash: hex.EncodeToString(manifest[:])}
	if _, err = r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, send); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Import(ctx, "admin", &mailpb.ImportMailRecipientsRequest{SenderKind: 1, CampaignId: campaign, ManifestGeneration: 1, PlayerIds: []uint64{7, 9}}); err != nil {
		t.Fatal(err)
	}
	if !seal {
		return
	}
	if _, err = r.Seal(ctx, "admin", &mailpb.SealMailCampaignRequest{SenderKind: 1, CampaignId: campaign, ManifestGeneration: 1}); err != nil {
		t.Fatal(err)
	}
}

// 删掉他的邮箱、个人邮件、状态、seq 和已封存名单里的他;终态资产指令、共享邮件原件、别人的数据都不动。
// 还在导入的名单里他那一行留着,清理之后名单照样能封存。第二次调用没有可删的行,照样成功。
func TestTiDBMailPurgeRemovedRoleDeletesPersonalRowsAndKeepsSharedOnes(t *testing.T) {
	ctx, db, r := mailFixture(t)
	const removed, other = uint64(7), uint64(9)
	claimed, err := r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(801, removed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(802, removed)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(803, other)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Claim(ctx, removed, claimed.MailId); err != nil {
		t.Fatal(err)
	}
	// 删角前挂起判定挡住未终态指令:走到清理时指令已终结。
	if _, err = db.ExecContext(ctx, "UPDATE mail_asset_op SET status=2 WHERE player_id=?", removed); err != nil {
		t.Fatal(err)
	}
	broadcast, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, &mailpb.SendMailRequest{SenderKind: 1, RequestId: 10, Title: "公告", Body: "全服可见"})
	if err != nil {
		t.Fatal(err)
	}
	for _, player := range []uint64{removed, other} {
		if _, err = r.Read(ctx, player, broadcast.MailId); err != nil {
			t.Fatal(err)
		}
	}
	mailCampaign(t, ctx, r, 11, 100, true)
	mailCampaign(t, ctx, r, 21, 200, false)
	otherStates := mailCount(t, ctx, db, "SELECT COUNT(*) FROM mail_state WHERE player_id=?", other)

	tombstone := &rfpb.RoleFenceState{PlayerId: removed, OwnerRevision: 2, Version: 2, HomeZone: 1}
	for round := 0; round < 2; round++ {
		mailPurge(t, ctx, db, tombstone)
		for _, check := range []struct {
			query string
			want  int
		}{
			{"SELECT COUNT(*) FROM mail_box WHERE player_id=7", 0},
			{"SELECT COUNT(*) FROM mail_message WHERE owner_player_id=7", 0},
			{"SELECT COUNT(*) FROM mail_state WHERE player_id=7", 0},
			{"SELECT COUNT(*) FROM mail_player_op_seq WHERE player_id=7", 0},
			{"SELECT COUNT(*) FROM mail_asset_op WHERE player_id=7", 1},
			{"SELECT COUNT(*) FROM mail_campaign_recipient WHERE player_id=7 AND campaign_id=11", 0},
			{"SELECT COUNT(*) FROM mail_campaign_recipient WHERE player_id=7 AND campaign_id=21", 1},
			{"SELECT COUNT(*) FROM mail_campaign_recipient WHERE player_id=9", 2},
			{"SELECT COUNT(*) FROM mail_box WHERE player_id=9", 1},
			{"SELECT COUNT(*) FROM mail_message WHERE owner_player_id=9", 1},
			{"SELECT COUNT(*) FROM mail_message WHERE owner_player_id=0", 3},
		} {
			if got := mailCount(t, ctx, db, check.query); got != check.want {
				t.Fatalf("第 %d 次清理之后 %s = %d,应为 %d", round+1, check.query, got, check.want)
			}
		}
		if got := mailCount(t, ctx, db, "SELECT COUNT(*) FROM mail_state WHERE player_id=?", other); got != otherStates {
			t.Fatalf("别人的邮件状态被动了: %d -> %d", otherStates, got)
		}
	}
	if _, err = r.Seal(ctx, "admin", &mailpb.SealMailCampaignRequest{SenderKind: 1, CampaignId: 21, ManifestGeneration: 1}); err != nil {
		t.Fatalf("导入中的名单清理之后应仍能封存: %v", err)
	}

	// 清理之后再寄给他的个人邮件明确拒收(发件方按终态处理),不留空邮箱行;寄给别人照常。
	if _, err = r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(804, removed)); !errors.Is(err, ErrRecipientDeleted) {
		t.Fatalf("寄给已删除的角色应拒收: %v", err)
	}
	if got := mailCount(t, ctx, db, "SELECT COUNT(*) FROM mail_box WHERE player_id=7"); got != 0 {
		t.Fatalf("拒收时不该建邮箱行: %d", got)
	}
	if _, err = r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(805, other)); err != nil {
		t.Fatalf("寄给在役角色照常: %v", err)
	}
}

// 发件事务在锁内复核:预检读到在役之后、提交之前副本变成墓碑(收尾先提交),这一封也拒收。
func TestTiDBMailSendRechecksRecipientUnderLock(t *testing.T) {
	ctx, db, r := mailFixture(t)
	const removed = uint64(7)
	if _, err := r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(811, removed)); err != nil {
		t.Fatal(err)
	}
	mailPurge(t, ctx, db, &rfpb.RoleFenceState{PlayerId: removed, OwnerRevision: 2, Version: 2, HomeZone: 1})
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = lockLiveRecipient(ctx, tx, removed); !errors.Is(err, ErrRecipientDeleted) {
		t.Fatalf("锁内复核应拒收已删除的角色: %v", err)
	}
	if err = lockLiveRecipient(ctx, tx, 9); err != nil {
		t.Fatalf("在役或没有副本行的角色照常投: %v", err)
	}
}

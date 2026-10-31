package data

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assetpb "proto/common/asset"
	basepb "proto/common/base"
	dspb "proto/data_service"
	mailpb "proto/mail"
	"schemamigrate"
	"shared/assetop"
	"shared/roleidentity"
)

func mailFixture(t *testing.T) (context.Context, *sql.DB, *Repo) {
	t.Helper()
	address := os.Getenv("MAIL_RANK_TEST_TIDB")
	if address == "" {
		if os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" {
			t.Fatal("MAIL_RANK_TEST_TIDB必须显式配置本任务独占TiDB")
		}
		t.Skip("需显式本任务数值loopback TiDB端点，随机自有库")
	}
	if err := mailDataFixtureEndpoint(address); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "mail_it_" + hex.EncodeToString(random[:])
	guildDB := "mail_guild_it_" + hex.EncodeToString(random[:])
	opts, err := sqltest.OptionsFromEnv("root@tcp("+address+")/mysql", database+"_writer", database, guildDB)
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
	if _, err = install.ExecContext(ctx, "CREATE TABLE "+guildDB+".guild_member (guild_id BIGINT UNSIGNED NOT NULL,player_id BIGINT UNSIGNED NOT NULL,PRIMARY KEY(guild_id,player_id),UNIQUE KEY player_id(player_id))"); err != nil {
		t.Fatal(err)
	}
	if _, err = install.ExecContext(ctx, "CREATE TABLE account_role (player_id BIGINT UNSIGNED PRIMARY KEY,account VARBINARY(191) NOT NULL,owner_revision BIGINT UNSIGNED NOT NULL,trade_listing_id BIGINT UNSIGNED NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantProtoTables(ctx, database, Tables()); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantTables(ctx, database, "account_role"); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantTables(ctx, guildDB, "guild_member"); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, database, 12)
	if err != nil {
		t.Fatal(err)
	}
	var next atomic.Uint64
	r := New(db, func(context.Context) (uint64, error) { return next.Add(1), nil }, func(context.Context) (uint64, error) { return next.Add(1), nil }, func(id uint32) bool { return id == 42 }, guildDB)
	return ctx, db, r
}

func mailDataFixtureEndpoint(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("邮件夹具必须显式数值loopback端口")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || number < 1 || number > 65535 {
		return errors.New("邮件夹具仅接受数值loopback与合法非零端口")
	}
	return nil
}
func TestMailDataFixtureEndpointRejectsForeignOrInvalidPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:24000", "[::1]:24000"} {
		if err := mailDataFixtureEndpoint(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"localhost:24000", "10.0.0.1:24000", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:mysql", "127.0.0.1"} {
		if mailDataFixtureEndpoint(address) == nil {
			t.Fatal("非法夹具端点未拒绝")
		}
	}
}
func personal(request, player uint64) *mailpb.SendMailRequest {
	return &mailpb.SendMailRequest{SenderKind: 3, RequestId: request, RecipientPlayerId: player, Title: "奖励", Body: "离线也可领取", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 10}}, Items: []*mailpb.MailItem{{ConfigId: 42, Count: 2}}}}
}

func mailRoleOwner(t *testing.T, ctx context.Context, db *sql.DB, r *Repo, account string, revision, listing uint64) context.Context {
	t.Helper()
	if _, err := db.ExecContext(ctx, "INSERT INTO account_role VALUES (7,?,?,?) ON DUPLICATE KEY UPDATE account=?,owner_revision=?,trade_listing_id=?", []byte(account), revision, listing, []byte(account), revision, listing); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigureRoleAuthority(ctx, schema); err != nil {
		t.Fatal(err)
	}
	actor, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &basepb.AccountSimplePlayer{PlayerId: 7}})
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

func TestTiDBMailOldSessionCannotReadOrMutateTransferredOrAbaRole(t *testing.T) {
	ctx, db, r := mailFixture(t)
	input := personal(701, 7)
	input.SenderKind = SenderKind("admin")
	sent, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, input)
	if err != nil {
		t.Fatal(err)
	}
	original := mailRoleOwner(t, ctx, db, r, "原卖家", 1, 0)
	if _, err = r.List(original, 7, &mailpb.ListMailsRequest{}); err != nil {
		t.Fatal(err)
	}
	mailRoleOwner(t, ctx, db, r, "新买家", 2, 0)
	if _, err = r.List(original, 7, &mailpb.ListMailsRequest{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("旧列表未拒绝: %v", err)
	}
	if _, err = r.Read(original, 7, sent.MailId); !errors.Is(err, ErrDenied) {
		t.Fatalf("旧已读未拒绝: %v", err)
	}
	if _, err = r.Claim(original, 7, sent.MailId); !errors.Is(err, ErrDenied) {
		t.Fatalf("旧领取未拒绝: %v", err)
	}
	if _, _, err = r.Delete(original, 7, []uint64{sent.MailId}, false); !errors.Is(err, ErrDenied) {
		t.Fatalf("旧删除未拒绝: %v", err)
	}
	var changed, ops int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_state WHERE read_ms<>0 OR deleted_ms<>0").Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_asset_op").Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if changed != 0 || ops != 0 {
		t.Fatalf("失效会话留下副作用: state=%d ops=%d", changed, ops)
	}
	current := mailRoleOwner(t, ctx, db, r, "原卖家", 3, 0)
	if _, err = r.Claim(original, 7, sent.MailId); !errors.Is(err, ErrDenied) {
		t.Fatalf("ABA旧修订未拒绝: %v", err)
	}
	frozen := mailRoleOwner(t, ctx, db, r, "原卖家", 3, 99)
	if _, err = r.Claim(frozen, 7, sent.MailId); !errors.Is(err, ErrDenied) {
		t.Fatalf("冻结领取未拒绝: %v", err)
	}
	mailRoleOwner(t, ctx, db, r, "原卖家", 3, 0)
	if _, err = r.List(current, 7, &mailpb.ListMailsRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestTiDBMailChecksOriginalSessionBeforePendingClaimReplay(t *testing.T) {
	ctx, db, r := mailFixture(t)
	input := personal(702, 7)
	input.SenderKind = SenderKind("admin")
	sent, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, input)
	if err != nil {
		t.Fatal(err)
	}
	original := mailRoleOwner(t, ctx, db, r, "原卖家", 1, 0)
	claim, err := r.Claim(original, 7, sent.MailId)
	if err != nil || claim.GetAssetOpId() == 0 {
		t.Fatalf("原领取失败: %v %v", claim, err)
	}
	current := mailRoleOwner(t, ctx, db, r, "新买家", 2, 0)
	if _, err = r.Claim(original, 7, sent.MailId); !errors.Is(err, ErrDenied) {
		t.Fatalf("原会话借在途短路: %v", err)
	}
	replayed, err := r.Claim(current, 7, sent.MailId)
	if err != nil || replayed.AssetOpId != claim.AssetOpId {
		t.Fatalf("重复领取改变原操作: %v %v", replayed, err)
	}
	var ops int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_asset_op").Scan(&ops); err != nil || ops != 1 {
		t.Fatalf("操作数=%d: %v", ops, err)
	}
}
func TestTiDBUnknownClaimStateNeverDeletesEvenAfterExpiry(t *testing.T) {
	ctx, db, repo := mailFixture(t)
	mail, err := repo.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(991, 7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE mail_state SET claim_state=99 WHERE player_id=7 AND mail_id=?", mail.MailId); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE mail_message SET expire_ms=1 WHERE mail_id=?", mail.MailId); err != nil {
		t.Fatal(err)
	}
	deleted, protected, err := repo.Delete(ctx, 7, []uint64{mail.MailId}, false)
	if err != nil || len(deleted) != 0 || len(protected) != 1 {
		t.Fatalf("未知态未保护: %v %v %v", deleted, protected, err)
	}
	view, err := repo.List(ctx, 7, &mailpb.ListMailsRequest{})
	if err != nil || len(view.Mails) != 1 || uint32(view.Mails[0].ClaimState) != 99 {
		t.Fatalf("未知态丢失: %+v %v", view, err)
	}
}
func TestTiDBConcurrentClaimRequiresSQLCommitAndProtectsPending(t *testing.T) {
	ctx, db, r := mailFixture(t)
	sent, err := r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(1, 7))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := r.Send(ctx, "rank", mailpb.MailKind_MAIL_KIND_PERSONAL, personal(1, 7))
	if err != nil || !replayed.Duplicated || sent.MailId != replayed.MailId {
		t.Fatalf("发信幂等: %v %+v", err, replayed)
	}
	other, err := r.List(ctx, 8, &mailpb.ListMailsRequest{})
	if err != nil || len(other.Mails) != 0 {
		t.Fatalf("他人邮件泄漏: %v %+v", err, other)
	}
	view, err := r.Read(ctx, 7, sent.MailId)
	if err != nil || !view.IsRead {
		t.Fatalf("独立已读: %v %+v", err, view)
	}
	const calls = 8
	results := make(chan *mailpb.MailActionResult, calls)
	failures := make(chan error, calls)
	var group sync.WaitGroup
	for i := 0; i < calls; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			out, e := r.Claim(ctx, 7, sent.MailId)
			if e != nil {
				failures <- e
				return
			}
			results <- out
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	var opID uint64
	for out := range results {
		if out.ClaimState != mailpb.MailClaimState_MAIL_CLAIM_PENDING || out.AssetOpId == 0 {
			t.Fatalf("并发领取状态: %+v", out)
		}
		if opID != 0 && opID != out.AssetOpId {
			t.Fatal("同邮件生成两条op")
		}
		opID = out.AssetOpId
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_asset_op").Scan(&count); err != nil || count != 1 {
		t.Fatalf("持久单意图: %d %v", count, err)
	}
	deleted, protected, err := r.Delete(ctx, 7, []uint64{sent.MailId}, false)
	if err != nil || len(deleted) != 0 || len(protected) != 1 {
		t.Fatalf("在途保护: %v %v %v", err, deleted, protected)
	}
	now := uint64(r.now().UnixMilli())
	op, ok, err := r.ClaimOp(ctx, opID, now, now+10000, now+3600000, 123)
	if err != nil || !ok {
		t.Fatalf("后台实际领取: %v %v", ok, err)
	}
	response := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	if _, err = r.Finalize(ctx, op, assetop.StatusApplied, response, now); err == nil {
		t.Fatal("Redis-only durable不能终结邮件")
	}
	response.SQLCommitted = true
	changed, err := r.Finalize(ctx, op, assetop.StatusApplied, response, now)
	if err != nil || !changed {
		t.Fatalf("SQL确认终结: %v %v", changed, err)
	}
	if changed, err = r.Finalize(ctx, op, assetop.StatusApplied, response, now); err != nil || changed {
		t.Fatalf("重复终结: %v %v", changed, err)
	}
	claimed, err := r.Claim(ctx, 7, sent.MailId)
	if err != nil || claimed.ClaimState != mailpb.MailClaimState_MAIL_CLAIM_CLAIMED || claimed.AssetOpId != opID {
		t.Fatalf("已领取回放: %v %+v", err, claimed)
	}
	deleted, protected, err = r.Delete(ctx, 7, []uint64{sent.MailId}, false)
	if err != nil || len(deleted) != 1 || len(protected) != 0 {
		t.Fatal("持久已领取后应允许删除")
	}
}

// 系统邮件不再排全服共用的 mail_box(0):同一请求并发首发恰好一封、其余回同一 mail_id,没有 player_id=0 的行。
func TestTiDBConcurrentSystemSendIsDecidedByUniqueKey(t *testing.T) {
	ctx, db, r := mailFixture(t)
	request := func() *mailpb.SendMailRequest {
		return &mailpb.SendMailRequest{SenderKind: 1, RequestId: 200, Title: "公告", Body: "并发首发"}
	}
	type outcome struct {
		out *mailpb.SendMailResponse
		err error
	}
	results := make(chan outcome, 8)
	start := make(chan struct{})
	for range 8 {
		go func() {
			<-start
			out, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, request())
			results <- outcome{out, err}
		}()
	}
	close(start)
	var mailID uint64
	fresh := 0
	for range 8 {
		got := <-results
		if got.err != nil {
			t.Fatalf("并发首发: %v", got.err)
		}
		if mailID != 0 && got.out.MailId != mailID {
			t.Fatalf("同一请求回了不同的 mail_id: %d %d", mailID, got.out.MailId)
		}
		mailID = got.out.MailId
		if !got.out.Duplicated {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("恰好一路首发成功,实际 %d", fresh)
	}
	var rows, sentinels int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_message WHERE sender_kind=1 AND request_id=200").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("恰好一封: %v %d", err, rows)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_box WHERE player_id=0").Scan(&sentinels); err != nil || sentinels != 0 {
		t.Fatalf("不再有 player_id=0 的守卫行: %v %d", err, sentinels)
	}
	changed := request()
	changed.Body = "同键换内容"
	if _, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("同键换内容: %v", err)
	}
}

// 活动首建同样靠唯一键:同一请求并发只建一份;同 (sender,campaign,generation) 换请求整笔回滚,不留孤儿消息行。
func TestTiDBConcurrentCampaignCreateIsDecidedByUniqueKey(t *testing.T) {
	ctx, db, r := mailFixture(t)
	hash, _, err := ShardHash([]uint64{7})
	if err != nil {
		t.Fatal(err)
	}
	manifest := sha256.Sum256([]byte(fmt.Sprintf("v1\n0:%s:1\n", hash)))
	request := func(requestID uint64) *mailpb.SendMailRequest {
		return &mailpb.SendMailRequest{SenderKind: 1, RequestId: requestID, Title: "活动奖", Body: "并发首建", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 20}}}, CampaignId: 21, ManifestGeneration: 1, ExpectedRecipientCount: 1, ExpectedShardCount: 1, ManifestHash: hex.EncodeToString(manifest[:])}
	}
	type outcome struct {
		out *mailpb.SendMailResponse
		err error
	}
	results := make(chan outcome, 6)
	start := make(chan struct{})
	for range 6 {
		go func() {
			<-start
			out, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, request(300))
			results <- outcome{out, err}
		}()
	}
	close(start)
	var mailID uint64
	for range 6 {
		got := <-results
		if got.err != nil {
			t.Fatalf("并发首建: %v", got.err)
		}
		if mailID != 0 && got.out.MailId != mailID {
			t.Fatalf("同一请求回了不同的 mail_id: %d %d", mailID, got.out.MailId)
		}
		mailID = got.out.MailId
	}
	if _, err = r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, request(301)); !errors.Is(err, ErrConflict) {
		t.Fatalf("同活动换请求: %v", err)
	}
	for query, want := range map[string]int{
		"SELECT COUNT(*) FROM mail_campaign WHERE sender_kind=1 AND campaign_id=21": 1,
		"SELECT COUNT(*) FROM mail_message WHERE sender_kind=1 AND request_id=300":  1,
		"SELECT COUNT(*) FROM mail_message WHERE sender_kind=1 AND request_id=301":  0,
		"SELECT COUNT(*) FROM mail_box WHERE player_id=0":                           0,
	} {
		var n int
		if err = db.QueryRowContext(ctx, query).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: %v %d want %d", query, err, n, want)
		}
	}
}

func TestTiDBManifestSealFreezesBroadcastRecipients(t *testing.T) {
	ctx, db, r := mailFixture(t)
	hash, _, err := ShardHash([]uint64{9, 7, 7})
	if err != nil {
		t.Fatal(err)
	}
	manifest := sha256.Sum256([]byte(fmt.Sprintf("v1\n0:%s:2\n", hash)))
	request := &mailpb.SendMailRequest{SenderKind: 1, RequestId: 100, Title: "活动奖", Body: "名单冻结", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 20}}}, CampaignId: 11, ManifestGeneration: 1, ExpectedRecipientCount: 2, ExpectedShardCount: 1, ManifestHash: hex.EncodeToString(manifest[:])}
	sent, err := r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_SYSTEM, request)
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.List(ctx, 7, &mailpb.ListMailsRequest{})
	if err != nil || len(page.Mails) != 0 {
		t.Fatalf("未seal不得可见: %v %+v", err, page)
	}
	if _, err = r.Claim(ctx, 7, sent.MailId); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未seal不得领: %v", err)
	}
	seal := &mailpb.SealMailCampaignRequest{SenderKind: 1, CampaignId: 11, ManifestGeneration: 1}
	if _, err = r.Seal(ctx, "admin", seal); !errors.Is(err, ErrConflict) {
		t.Fatalf("缺片不得seal: %v", err)
	}
	shard := &mailpb.ImportMailRecipientsRequest{SenderKind: 1, CampaignId: 11, ManifestGeneration: 1, ShardIndex: 0, PlayerIds: []uint64{9, 7, 7}}
	if _, err = r.Import(ctx, "admin", shard); err != nil {
		t.Fatal(err)
	}
	again, err := r.Import(ctx, "admin", shard)
	if err != nil || !again.Duplicated || again.ImportedCount != 2 {
		t.Fatalf("片重放: %v %+v", err, again)
	}
	if _, err = r.Seal(ctx, "admin", seal); err != nil {
		t.Fatal(err)
	}
	page, err = r.List(ctx, 7, &mailpb.ListMailsRequest{})
	if err != nil || len(page.Mails) != 1 {
		t.Fatalf("已seal应可见: %v %+v", err, page)
	}
	page, err = r.List(ctx, 8, &mailpb.ListMailsRequest{})
	if err != nil || len(page.Mails) != 0 {
		t.Fatalf("新号/非名单不得领取广播: %v %+v", err, page)
	}
	if _, err = r.Claim(ctx, 8, sent.MailId); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非名单claim: %v", err)
	}
	if _, err = r.Send(ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, &mailpb.SendMailRequest{SenderKind: 1, RequestId: 100, RecipientPlayerId: 8, Title: "换收件人", Body: "不能复用请求"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("跨频道幂等域不可重用: %v", err)
	}
	var visible int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_message WHERE kind<>0").Scan(&visible); err != nil || visible != 1 {
		t.Fatal("seal只发布一封可见广播")
	}
}

func TestTiDBGuildTextUsesSameClusterAuthorityAndFrozenRewardsSurviveLeave(t *testing.T) {
	ctx, db, r := mailFixture(t)
	if _, err := db.ExecContext(ctx, "INSERT INTO "+r.guildDatabase+".guild_member (guild_id,player_id) VALUES (77,7)"); err != nil {
		t.Fatal(err)
	}
	sent, err := r.Send(ctx, "guild", mailpb.MailKind_MAIL_KIND_GUILD, &mailpb.SendMailRequest{SenderKind: 2, RequestId: 1, GuildId: 77, Title: "帮会通知", Body: "当前成员可读"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.List(ctx, 7, &mailpb.ListMailsRequest{})
	if err != nil || len(page.Mails) != 1 {
		t.Fatalf("成员文本: %v %+v", err, page)
	}
	if _, err = db.ExecContext(ctx, "DELETE FROM "+r.guildDatabase+".guild_member WHERE guild_id=77 AND player_id=7"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Read(ctx, 7, sent.MailId); !errors.Is(err, ErrNotFound) {
		t.Fatalf("离帮文本资格没有复核: %v", err)
	}
	shardHash, _, _ := ShardHash([]uint64{7})
	digest := sha256.Sum256([]byte(fmt.Sprintf("v1\n0:%s:1\n", shardHash)))
	reward := &mailpb.SendMailRequest{SenderKind: 2, RequestId: 2, GuildId: 77, Title: "帮会奖励", Body: "既得收件资格", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 1}}}, CampaignId: 12, ManifestGeneration: 1, ExpectedRecipientCount: 1, ExpectedShardCount: 1, ManifestHash: hex.EncodeToString(digest[:])}
	if _, err = r.Send(ctx, "guild", mailpb.MailKind_MAIL_KIND_GUILD, reward); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Import(ctx, "guild", &mailpb.ImportMailRecipientsRequest{SenderKind: 2, CampaignId: 12, ManifestGeneration: 1, PlayerIds: []uint64{7}}); err != nil {
		t.Fatal(err)
	}
	sealed, err := r.Seal(ctx, "guild", &mailpb.SealMailCampaignRequest{SenderKind: 2, CampaignId: 12, ManifestGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Claim(ctx, 7, sealed.MailId); err != nil {
		t.Fatalf("历史名单资格不能随离帮撤回: %v", err)
	}
}

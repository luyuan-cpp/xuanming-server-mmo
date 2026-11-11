package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"mail/internal/data"
	"mail/internal/server"
	mailpb "proto/mail"
	"schemamigrate"
	"shared/callerauth"
	"shared/callerauth/callerauthtest"
	"shared/domainhost"
	"shared/rolefence"
	"shared/serviceauth"
)

// fixtureGateIdentity 是夹具服务端的 gate 验签闸门,与生产 domainhost 装配一样放在拦截器链最外层。
// 在线域原账号只认 gate 签名原件,不读 login Redis;夹具和调用方共用 callerauthtest 的测试密钥。
func fixtureGateIdentity() grpc.UnaryServerInterceptor {
	return callerauth.SessionInterceptor(callerauth.NewVerifier(callerauth.Options{Keys: callerauthtest.Key{}, Enforce: true, AllowedCallers: []string{"gate"}}))
}

func mailMainFixtureEndpoint(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("邮件RPC夹具必须显式数值loopback端口")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || number < 1 || number > 65535 {
		return errors.New("邮件RPC夹具仅接受数值loopback与合法非零端口")
	}
	return nil
}
func TestMailMainFixtureEndpointRejectsForeignOrInvalidPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:24000", "[::1]:24000"} {
		if err := mailMainFixtureEndpoint(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"localhost:24000", "10.0.0.1:24000", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:mysql", "127.0.0.1"} {
		if mailMainFixtureEndpoint(address) == nil {
			t.Fatal("非法夹具端点未拒绝")
		}
	}
}

type committedReminder struct {
	db    *sql.DB
	calls atomic.Int64
	t     *testing.T
}

func (n *committedReminder) Personal(ctx context.Context, player, mail uint64) {
	var count uint64
	if err := n.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_message WHERE mail_id=? AND owner_player_id=?", mail, player).Scan(&count); err != nil || count != 1 {
		n.t.Errorf("提醒早于SQL提交: count=%d err=%v", count, err)
	}
	n.calls.Add(1)
}

func TestTiDBRealRPCAuthSendOfflineReadClaimAndDeleteProtection(t *testing.T) {
	address := os.Getenv("MAIL_RANK_TEST_TIDB")
	if address == "" {
		if os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" {
			t.Fatal("MAIL_RANK_TEST_TIDB必须显式配置本任务独占TiDB")
		}
		t.Skip("需显式本任务数值loopback TiDB端点，随机库与真实loopback RPC")
	}
	if err := mailMainFixtureEndpoint(address); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "mail_rpc_it_" + hex.EncodeToString(random[:])
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
	if report, e := schemamigrate.Up(ctx, install, schemamigrate.Options{Database: database, Tables: data.Tables()}); e != nil || len(report.Manual) > 0 {
		t.Fatalf("schema: %v %+v", e, report)
	}
	if err = rolefence.EnsureSchema(ctx, install); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantProtoTables(ctx, database, data.Tables()); err != nil {
		t.Fatal(err)
	}
	if err = fixture.GrantTables(ctx, database, rolefence.TableName); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, database, 8)
	if err != nil {
		t.Fatal(err)
	}
	var ids atomic.Uint64
	next := func(context.Context) (uint64, error) { return ids.Add(1), nil }
	repo := data.New(db, next, next, func(uint32) bool { return true }, noGuilds{})
	repo.ConfigureRoleFence(rolefence.NewGuard(db, fixedRoleOwner{account: "邮件原账号", revision: 3}))
	const secret = "mail-rpc-integration-only-never-a-real-secret"
	verifier, err := serviceauth.New(map[string]string{"rank": secret})
	if err != nil {
		t.Fatal(err)
	}
	reminder := &committedReminder{db: db, t: t}
	handler := server.New(repo, verifier, reminder)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer(grpc.ChainUnaryInterceptor(fixtureGateIdentity(), domainhost.Guard(3*time.Second)))
	mailpb.RegisterMailAdminServer(rpc, handler)
	mailpb.RegisterClientPlayerMailServer(rpc, handler)
	go func() { _ = rpc.Serve(listener) }()
	defer rpc.Stop()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	sender := mailpb.NewMailAdminClient(connection)
	client := mailpb.NewClientPlayerMailClient(connection)
	request := &mailpb.SendMailRequest{SenderKind: 3, RequestId: 1, RecipientPlayerId: 7, Title: "真实RPC奖励", Body: "离线投递到SQL", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{Amount: 10}}}}
	if _, err = sender.SendPersonalMail(ctx, request); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("无凭据不得发信: %v", err)
	}
	if err = serviceauth.Sign(mailpb.MailAdmin_SendPersonalMail_FullMethodName, request, "rank", secret, uint64(time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	sent, err := sender.SendPersonalMail(ctx, request)
	if err != nil || sent.GetErrorMessage().GetId() != 0 || sent.GetMailId() == 0 {
		t.Fatalf("内部真实发件: %v %+v", err, sent)
	}
	replayed, e := sender.SendPersonalMail(ctx, request)
	if e != nil || !replayed.Duplicated || reminder.calls.Load() != 1 {
		t.Fatalf("提交后只提醒一次，重放不得新推: %+v %v count=%d", replayed, e, reminder.calls.Load())
	}
	tampered := proto.Clone(request).(*mailpb.SendMailRequest)
	tampered.RecipientPlayerId = 8
	if _, err = sender.SendPersonalMail(ctx, tampered); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("签名不能换收件人: %v", err)
	}
	// gate 对每次转发按方法现签整份会话,nonce 只能用一次,所以每个调用各签一份。
	player := callerauthtest.OnlineSession("邮件原账号", 7, 3)
	playerCtx := func(method string) context.Context {
		t.Helper()
		md, err := callerauth.SignedMetadataForDetails(callerauthtest.Key{}, "gate", method, player)
		if err != nil {
			t.Fatal(err)
		}
		return metadata.NewOutgoingContext(ctx, md)
	}
	if _, err = sender.SendPersonalMail(playerCtx(mailpb.MailAdmin_SendPersonalMail_FullMethodName), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("客户端metadata不能进入Admin: %v", err)
	}
	if _, err = client.NotifyMailChanged(playerCtx(mailpb.ClientPlayerMail_NotifyMailChanged_FullMethodName), &mailpb.MailChangedEvent{MailId: sent.MailId}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("S2C入口不可被客户端伪造: %v", err)
	}
	listed, err := client.ListMails(playerCtx(mailpb.ClientPlayerMail_ListMails_FullMethodName), &mailpb.ListMailsRequest{})
	if err != nil || listed.GetErrorMessage().GetId() != 0 || len(listed.Mails) != 1 {
		t.Fatalf("真实会话读取: %v %+v", err, listed)
	}
	claimed, err := client.ClaimMailAttachments(playerCtx(mailpb.ClientPlayerMail_ClaimMailAttachments_FullMethodName), &mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{sent.MailId}})
	if err != nil || len(claimed.Results) != 1 || claimed.Results[0].ClaimState != mailpb.MailClaimState_MAIL_CLAIM_PENDING {
		t.Fatalf("实际领取入口: %v %+v", err, claimed)
	}
	again, err := client.ClaimMailAttachments(playerCtx(mailpb.ClientPlayerMail_ClaimMailAttachments_FullMethodName), &mailpb.ClaimMailAttachmentsRequest{MailIds: []uint64{sent.MailId}})
	if err != nil || again.Results[0].AssetOpId != claimed.Results[0].AssetOpId {
		t.Fatalf("RPC重放应同op: %v %+v", err, again)
	}
	deleted, err := client.DeleteMails(playerCtx(mailpb.ClientPlayerMail_DeleteMails_FullMethodName), &mailpb.DeleteMailsRequest{MailIds: []uint64{sent.MailId}})
	if err != nil || len(deleted.ProtectedMailIds) != 1 || len(deleted.DeletedMailIds) != 0 {
		t.Fatalf("RPC不能删除待到账邮件: %v %+v", err, deleted)
	}
}

package authority

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/encoding/protojson"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"shared/authoritysql/sqltest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dbpb "proto/common/database"
	kafkapb "proto/contracts/kafka"
	mailpb "proto/mail"
	rankpb "proto/rank"
	"shared/authoritysql"
	"shared/domainhost"
	"shared/serviceauth"
)

// 跨 module 的 internal 边界由独立测试进程保留；评分、封榜、发信和列表均调用生产入口。
// 只替代外部号段，并在已提交的邮件 RPC 回包处注入一次网络故障，不直写任何业务状态。
func TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss(t *testing.T) {
	address := os.Getenv("RANK_MAIL_CHAIN_IT_TIDB")
	if address == "" {
		t.Skip("显式设置本机 TiDB 和预编译 mail/rank 夹具后运行整链验收")
	}
	requireNativeTiDBAddress(t, address)
	mailExecutable := chainExecutable(t, "RANK_MAIL_CHAIN_MAIL_EXE")
	rankExecutable := chainExecutable(t, "RANK_MAIL_CHAIN_RANK_EXE")
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	var random [32]byte
	_, err := rand.Read(random[:])
	require.NoError(t, err)
	secret := hex.EncodeToString(random[:])
	name := "rank_mail_chain_" + hex.EncodeToString(random[:8])
	mailDatabase, rankDatabase := name+"_mail", name+"_rank"
	evidence := os.Getenv("RANK_MAIL_CHAIN_EVIDENCE_DIR")
	if evidence == "" {
		evidence = t.TempDir()
	}
	require.True(t, filepath.IsAbs(evidence), "证据目录必须是绝对路径")
	require.NoError(t, os.MkdirAll(evidence, 0700))
	opts, err := sqltest.OptionsFromEnv("root@tcp("+address+")/mysql", name+"_writer", mailDatabase, rankDatabase)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	_, err = fixture.Schema.ExecContext(ctx, "CREATE TABLE __rank_mail_chain_subject_owner(name VARBINARY(64) NOT NULL,owner_nonce BINARY(32) NOT NULL,physical_sha256 BINARY(32) NOT NULL,PRIMARY KEY(name))")
	require.NoError(t, err)
	var engine string
	require.NoError(t, fixture.Schema.QueryRowContext(ctx, "SELECT VERSION()").Scan(&engine))
	require.Contains(t, strings.ToLower(engine), "tidb")
	// 观察池只读同一任务的自有库；每个业务子进程仍使用自己的最小权限 normal 主体。
	openIsolated := func(database string) *sql.DB {
		t.Helper()
		observer, err := mysql.ParseDSN(opts.AdminDSN)
		require.NoError(t, err)
		observer.DBName = database
		db, err := authoritysql.Open(ctx, observer.FormatDSN(), database, authoritysql.BackendTiDB, 2, authoritysql.Reader())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	mailDB, rankDB := openIsolated(mailDatabase), openIsolated(rankDatabase)
	t.Setenv("MATCH_IT_TIDB", address)
	s := testStore(t)
	mailDirectory := t.TempDir()
	mailReady := filepath.Join(mailDirectory, "ready")
	common := []string{"RANK_MAIL_CHAIN_IT_TIDB=" + address, "RANK_MAIL_CHAIN_SECRET=" + secret}
	mailEnvironment := append(append([]string{}, common...), chainBootEnvironment(t, ctx, fixture, opts, mailDatabase)...)
	mailProcess := startChainFixture(t, mailExecutable, append(mailEnvironment,
		"RANK_MAIL_CHAIN_FIXTURE=mail", "RANK_MAIL_CHAIN_DATABASE="+mailDatabase,
		"RANK_MAIL_CHAIN_READY_FILE="+mailReady), filepath.Join(evidence, name+"_mail.log"))
	mailEndpoint := chainReady(t, ctx, mailProcess, mailReady)
	newRank := func(label string) (*chainFixture, string) {
		t.Helper()
		directory := t.TempDir()
		rankEnvironment := append(append([]string{}, common...), chainBootEnvironment(t, ctx, fixture, opts, rankDatabase)...)
		process := startChainFixture(t, rankExecutable, append(rankEnvironment,
			"RANK_MAIL_CHAIN_FIXTURE=rank", "RANK_MAIL_CHAIN_DATABASE="+rankDatabase,
			"RANK_MAIL_CHAIN_MAIL_ENDPOINT="+mailEndpoint, "RANK_MAIL_CHAIN_CONTROL_DIR="+directory),
			filepath.Join(evidence, name+"_"+label+".log"))
		return process, chainReady(t, ctx, process, filepath.Join(directory, "ready"))
	}
	rankProcess, rankEndpoint := newRank("rank_before_restart")
	rankConnection := chainConnection(t, rankEndpoint)
	rankClient := rankpb.NewRankAdminClient(rankConnection)
	mailClient := mailpb.NewClientPlayerMailClient(chainConnection(t, mailEndpoint))

	for i, score := range []float64{1, 0.5} {
		battle := uint64(5001 + i)
		shard := rankSourceShard(battle) // 两局落在相邻两片,各片游标从 1 起,榜上的游标是总数
		event := &kafkapb.BattleResultEvent{BattleId: battle, MatchMode: 3}
		applied, err := s.ApplyRating(ctx, battle, event, 3, []uint64{7}, []uint64{8}, score)
		require.NoError(t, err)
		require.True(t, applied)
		applied, err = s.ApplyRating(ctx, battle, event, 3, []uint64{7}, []uint64{8}, score)
		require.NoError(t, err)
		require.False(t, applied, "同局重放不得多出评分或游标")
		// 每一步都从实际 SQL outbox 经 HMAC RPC 连续投榜。
		require.NoError(t, s.PublishRankOnce(ctx, rankClient, secret))
		require.NoError(t, s.PublishRankOnce(ctx, rankClient, secret), "来源重放不得重复提交榜批次")
		var produced, committed, board, rankCursor uint64
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT produced_cursor,committed_cursor,board_id FROM match_rank_source WHERE source_epoch=1 AND shard=?", shard).Scan(&produced, &committed, &board))
		require.Equal(t, uint64(1), produced)
		require.Equal(t, produced, committed)
		require.NotZero(t, board)
		require.NoError(t, rankDB.QueryRowContext(ctx, "SELECT committed_cursor FROM rank_board WHERE board_id=?", board).Scan(&rankCursor))
		require.Equal(t, uint64(i+1), rankCursor)
		var body []byte
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND shard=? AND source_cursor=?", shard, produced).Scan(&body))
		replay := &rankpb.SubmitRankScoresRequest{}
		require.NoError(t, proto.Unmarshal(body, replay))
		replay.BoardId = board
		require.NoError(t, serviceauth.Sign(rankpb.RankAdmin_SubmitRankScores_FullMethodName, replay, "match", secret, uint64(time.Now().UnixMilli())))
		receipt, err := rankClient.SubmitRankScores(ctx, replay)
		require.NoError(t, err)
		require.Zero(t, receipt.GetErrorMessage().GetId())
		require.True(t, receipt.Duplicated, "原 SQL outbox 批次重投必须回原收据")
		require.Equal(t, produced, receipt.CommittedCursor)
	}
	chainCount(t, ctx, s.DB, "SELECT COUNT(*) FROM match_rank_outbox", 2)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_input_receipt", 2)
	for _, player := range []uint64{7, 8} {
		expected, err := s.Rating(ctx, player)
		require.NoError(t, err)
		var score int64
		var games uint64
		require.NoError(t, rankDB.QueryRowContext(ctx, "SELECT score FROM rank_score WHERE entity_id=?", player).Scan(&score))
		require.Equal(t, expected, score)
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT games FROM match_rating WHERE player_id=?", player).Scan(&games))
		require.Equal(t, uint64(2), games)
	}
	finalCursor, err := s.StopRankSource(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(2), finalCursor)
	before, err := s.Rating(ctx, 7)
	require.NoError(t, err)
	_, err = s.ApplyRating(ctx, 5003, &kafkapb.BattleResultEvent{BattleId: 5003, MatchMode: 3}, 3, []uint64{7}, []uint64{8}, 1)
	require.Error(t, err, "停代屏障必须阻止迟到评分")
	after, err := s.Rating(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, before, after)
	chainCount(t, ctx, s.DB, "SELECT COUNT(*) FROM match_rating_applied WHERE battle_id=5003", 0)
	var board uint64
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT MAX(board_id) FROM match_rank_source WHERE source_epoch=1").Scan(&board))
	attachments := &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 10}}}
	freeze := &rankpb.FreezeRankGenerationRequest{BoardId: board, Generation: 1, SourceEpoch: 1,
		ExpectedSourceCursor: finalCursor + 1, RequestId: 9901, TopN: 2,
		Rewards: []*rankpb.RankRewardTier{{FirstRank: 1, LastRank: 1, Attachments: attachments}}}
	seal := func() *rankpb.FreezeRankGenerationResponse {
		t.Helper()
		require.NoError(t, serviceauth.Sign(rankpb.RankAdmin_FreezeRankGeneration_FullMethodName, freeze, "match", secret, uint64(time.Now().UnixMilli())))
		response, err := rankClient.FreezeRankGeneration(ctx, freeze)
		require.NoError(t, err)
		return response
	}
	require.NotZero(t, seal().GetErrorMessage().GetId(), "游标未齐不能封榜")
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_snapshot", 0)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_mail_reward", 0)
	freeze.ExpectedSourceCursor = finalCursor
	sealed := seal()
	require.Zero(t, sealed.GetErrorMessage().GetId())
	require.NotZero(t, sealed.SettlementId)
	require.Equal(t, uint64(2), sealed.WinnerCount)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_snapshot", 2)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_mail_reward", 1)
	var rewardID, recipient, created uint64
	var rewardBody []byte
	require.NoError(t, rankDB.QueryRowContext(ctx, "SELECT reward_id,player_id,attachments,created_ms FROM rank_mail_reward WHERE settlement_id=?", sealed.SettlementId).Scan(&rewardID, &recipient, &rewardBody, &created))
	require.Equal(t, uint64(7), recipient)
	require.NotZero(t, rewardID)
	t.Log("真实评分/outbox 经两片各自的连续游标入榜；停代回滚迟到评分；未齐封榜拒绝，正式封榜生成 2 快照、1 奖励")

	chainTick(t, ctx, rankProcess)
	var state, attempts uint32
	var settledMailID uint64
	require.NoError(t, rankDB.QueryRowContext(ctx, "SELECT status,mail_id,attempts FROM rank_mail_reward WHERE reward_id=?", rewardID).Scan(&state, &settledMailID, &attempts))
	require.Equal(t, uint32(1), state)
	require.Zero(t, settledMailID, "已提交但回包丢失时奖励须保持待重试")
	require.Equal(t, uint32(1), attempts)
	chainCount(t, ctx, mailDB, "SELECT COUNT(*) FROM mail_message", 1)
	var mailID, mailRequestID, expire uint64
	var mailBody []byte
	require.NoError(t, mailDB.QueryRowContext(ctx, "SELECT mail_id,request_id,attachments,expire_ms FROM mail_message WHERE owner_player_id=?", recipient).Scan(&mailID, &mailRequestID, &mailBody, &expire))
	require.Equal(t, rewardID, mailRequestID)
	require.Equal(t, rewardBody, mailBody)
	require.Equal(t, created+30*86400000, expire)
	list := func() {
		t.Helper()
		session, err := proto.Marshal(&base.SessionDetails{PlayerId: recipient, SessionId: 7, GateInstanceId: "mail-rank-chain-original", SessionIncarnation: 19, OwnerRevision: 3})
		require.NoError(t, err)
		playerCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(domainhost.SessionMetadata, base64.StdEncoding.EncodeToString(session)))
		response, err := mailClient.ListMails(playerCtx, &mailpb.ListMailsRequest{})
		require.NoError(t, err)
		require.Zero(t, response.GetErrorMessage().GetId())
		require.Len(t, response.Mails, 1)
		require.Equal(t, mailID, response.Mails[0].MailId)
		require.True(t, proto.Equal(attachments, response.Mails[0].Attachments))
	}
	list()
	t.Log("首轮真实 MailAdmin 已提交，注入回包丢失；奖励仍待重试，真实邮箱列表已可见唯一邮件")
	rankProcess.stop(t)
	rankProcess, rankEndpoint = newRank("rank_after_restart")
	rankClient = rankpb.NewRankAdminClient(chainConnection(t, rankEndpoint))
	replayedSeal := seal()
	require.Zero(t, replayedSeal.GetErrorMessage().GetId())
	require.True(t, replayedSeal.Duplicated)
	require.Equal(t, sealed.SettlementId, replayedSeal.SettlementId)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_snapshot", 2)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_mail_reward", 1)
	// 等待生产退避时间真实到期，不修改 SQL 时间或奖励终态。
	require.NoError(t, chainAwait(ctx, func() (bool, error) {
		var next, lease uint64
		err := rankDB.QueryRowContext(ctx, "SELECT next_attempt_ms,lease_until_ms FROM rank_mail_reward WHERE reward_id=?", rewardID).Scan(&next, &lease)
		now := uint64(time.Now().UnixMilli())
		return next <= now && lease < now, err
	}))
	chainTick(t, ctx, rankProcess)
	require.NoError(t, rankDB.QueryRowContext(ctx, "SELECT status,mail_id,attempts FROM rank_mail_reward WHERE reward_id=?", rewardID).Scan(&state, &settledMailID, &attempts))
	require.Equal(t, uint32(2), state)
	require.Equal(t, mailID, settledMailID)
	require.Equal(t, uint32(2), attempts)
	chainCount(t, ctx, rankDB, "SELECT COUNT(*) FROM rank_mail_reward", 1)
	chainCount(t, ctx, mailDB, "SELECT COUNT(*) FROM mail_message", 1)
	var replayRequestID, replayExpire uint64
	var replayBody []byte
	require.NoError(t, mailDB.QueryRowContext(ctx, "SELECT request_id,attachments,expire_ms FROM mail_message WHERE mail_id=?", mailID).Scan(&replayRequestID, &replayBody, &replayExpire))
	require.Equal(t, mailRequestID, replayRequestID)
	require.Equal(t, mailBody, replayBody)
	require.Equal(t, expire, replayExpire)
	list()
	chainTick(t, ctx, rankProcess)
	chainCount(t, ctx, mailDB, "SELECT COUNT(*) FROM mail_message", 1)
	require.NoError(t, rankDB.QueryRowContext(ctx, "SELECT attempts FROM rank_mail_reward WHERE reward_id=?", rewardID).Scan(&attempts))
	require.Equal(t, uint32(2), attempts, "已投递奖励不再调用发信")
	t.Log("重启奖励进程后原 reward_id/附件/到期时间重放；原 mail_id 被确认，邮件总数仍为 1，再次 Tick 无副作用")
}

type chainFixture struct {
	command *exec.Cmd
	input   io.WriteCloser
	done    chan struct{}
	err     error
	logPath string
	control string
	stopped bool
}

func chainExecutable(t *testing.T, variable string) string {
	t.Helper()
	path := os.Getenv(variable)
	require.True(t, filepath.IsAbs(path), "%s 必须指定预编译夹具绝对路径", variable)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, info.IsDir())
	return path
}

func startChainFixture(t *testing.T, executable string, environment []string, logPath string) *chainFixture {
	t.Helper()
	command := exec.Command(executable, "-test.run=^TestRankMailChainFixture$", "-test.timeout=90s", "-test.v")
	command.Env = os.Environ()
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		for i := len(command.Env) - 1; i >= 0; i-- {
			if strings.HasPrefix(command.Env[i], key+"=") {
				command.Env = append(command.Env[:i], command.Env[i+1:]...)
			}
		}
		command.Env = append(command.Env, value)
	}
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, err)
	command.Stdout, command.Stderr = output, output
	input, err := command.StdinPipe()
	require.NoError(t, err)
	process := &chainFixture{command: command, input: input, done: make(chan struct{}), logPath: logPath}
	for _, value := range environment {
		if strings.HasPrefix(value, "RANK_MAIL_CHAIN_CONTROL_DIR=") {
			process.control = strings.TrimPrefix(value, "RANK_MAIL_CHAIN_CONTROL_DIR=")
		}
	}
	if err = command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		t.Fatal(err)
	}
	go func() {
		process.err = command.Wait()
		_ = output.Close()
		close(process.done)
	}()
	t.Cleanup(func() { process.stop(t) })
	return process
}

func (p *chainFixture) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_, _ = io.WriteString(p.input, "stop\n")
	_ = p.input.Close()
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		_ = p.command.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			t.Errorf("自有夹具进程未在预算内退出，日志: %s", p.logPath)
			return
		}
		t.Errorf("自有夹具停机超时，已终止；日志: %s", p.logPath)
	}
	if p.err != nil {
		t.Errorf("自有夹具失败: %v；日志: %s", p.err, p.logPath)
	}
}

func chainAwait(parent context.Context, check func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := check()
		if err != nil || ready {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func chainReady(t *testing.T, ctx context.Context, process *chainFixture, file string) string {
	t.Helper()
	var endpoint string
	require.NoError(t, chainAwait(ctx, func() (bool, error) {
		select {
		case <-process.done:
			return false, fmt.Errorf("夹具就绪前退出: %v；日志: %s", process.err, process.logPath)
		default:
		}
		body, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		endpoint = strings.TrimSpace(string(body))
		return endpoint != "", nil
	}))
	host, port, err := net.SplitHostPort(endpoint)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	require.NotEmpty(t, port)
	require.NotEqual(t, "0", port)
	return endpoint
}

func chainConnection(t *testing.T, endpoint string) *grpc.ClientConn {
	t.Helper()
	connection, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	return connection
}

func chainTick(t *testing.T, ctx context.Context, process *chainFixture) {
	t.Helper()
	file := filepath.Join(process.control, "tick.done")
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	_, err := io.WriteString(process.input, "tick\n")
	require.NoError(t, err)
	require.NoError(t, chainAwait(ctx, func() (bool, error) {
		select {
		case <-process.done:
			return false, fmt.Errorf("奖励夹具提前退出: %v；日志: %s", process.err, process.logPath)
		default:
		}
		body, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) || err == nil && len(body) == 0 {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var response struct{ Error string }
		if err := json.Unmarshal(body, &response); err != nil {
			return false, err
		}
		if response.Error != "" {
			return false, errors.New(response.Error)
		}
		return true, nil
	}))
}

func chainCount(t *testing.T, ctx context.Context, db *sql.DB, query string, expected uint64) {
	t.Helper()
	var count uint64
	require.NoError(t, db.QueryRowContext(ctx, query).Scan(&count))
	require.Equal(t, expected, count)
}

// 每次真实进程启动申请新boot原件；重启绝不重复消费上一进程的grant。
func chainBootEnvironment(t *testing.T, ctx context.Context, fixture *sqltest.Fixture, opts sqltest.Options, database string) []string {
	t.Helper()
	normal, err := mysql.ParseDSN(fixture.DSN(database))
	require.NoError(t, err)
	var writerID, instance, requestID, grantID [16]byte
	for _, value := range [][]byte{writerID[:], instance[:], requestID[:], grantID[:]} {
		_, err = rand.Read(value)
		require.NoError(t, err)
	}
	normal.User = "chain_writer_" + hex.EncodeToString(instance[:8])
	var password, owner [32]byte
	_, err = rand.Read(password[:])
	require.NoError(t, err)
	_, err = rand.Read(owner[:])
	require.NoError(t, err)
	normal.Passwd = hex.EncodeToString(password[:])
	_, err = fixture.Schema.ExecContext(ctx, "CREATE USER '"+normal.User+"'@'%' IDENTIFIED BY '"+normal.Passwd+"'")
	require.NoError(t, err)
	directory := os.Getenv("MMORPG_SQL_FIXTURE_RECEIPT_DIR")
	require.True(t, filepath.IsAbs(directory))
	ownerFile := filepath.Join(directory, "chain-subject-"+normal.User+".owner")
	file, err := os.OpenFile(ownerFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, err)
	expected := append(append([]byte(nil), owner[:]...), opts.PhysicalAuthoritySHA256...)
	_, writeErr := file.Write(expected)
	require.NoError(t, writeErr)
	require.NoError(t, file.Sync())
	require.NoError(t, file.Close())
	_, err = fixture.Schema.ExecContext(ctx, "INSERT INTO __rank_mail_chain_subject_owner(name,owner_nonce,physical_sha256) VALUES(?,?,?)", normal.User, owner[:], opts.PhysicalAuthoritySHA256)
	require.NoError(t, err)
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		raw, err := os.ReadFile(ownerFile)
		if err != nil || !bytes.Equal(raw, expected) {
			t.Errorf("链主体原件未确认，保留对象及原件 %s", ownerFile)
			return
		}
		var actualOwner, actualPhysical []byte
		err = fixture.Schema.QueryRowContext(clean, "SELECT owner_nonce,physical_sha256 FROM __rank_mail_chain_subject_owner WHERE name=?", normal.User).Scan(&actualOwner, &actualPhysical)
		if err != nil || !bytes.Equal(actualOwner, owner[:]) || !bytes.Equal(actualPhysical, opts.PhysicalAuthoritySHA256) {
			t.Errorf("链主体归属未确认，保留对象及原件 %s", ownerFile)
			return
		}
		_, err = fixture.Schema.ExecContext(clean, "DROP USER '"+normal.User+"'@'%'")
		require.NoError(t, err)
	})
	for _, table := range []string{"route_meta", "writer_registry", "writer_slot", "writer_lifecycle_receipt", "sql_writer_drain_run", "sql_writer_freeze_ticket", "sql_writer_maintenance_receipt"} {
		_, err = fixture.Schema.ExecContext(ctx, "GRANT SELECT ON "+fixture.Config.AuthoritySchema()+"."+table+" TO '"+normal.User+"'@'%'")
		require.NoError(t, err)
	}
	if opts.LockPrimitive == dbpb.SqlWriterBootGrant_LOCK_TOUCH {
		_, err = fixture.Schema.ExecContext(ctx, "GRANT UPDATE(lock_touch) ON "+fixture.Config.AuthoritySchema()+".writer_slot TO '"+normal.User+"'@'%'")
		require.NoError(t, err)
	}
	meta, err := fixture.Catalog.CurrentMeta(ctx)
	require.NoError(t, err)
	deployment := "chain-" + hex.EncodeToString(instance[:])
	writer := &dbpb.SqlWriterRegistry{WriterKey: writerID[:], InstanceId: instance[:], Generation: fixture.Config.Generation(), SqlPrincipal: []byte(normal.User + "@%"), State: dbpb.SqlWriterRegistry_ACTIVE, RegisteredMs: uint64(time.Now().UnixMilli())}
	request := &dbpb.SqlWriterLifecycleReceipt_Request{RequestId: requestID[:], Kind: dbpb.SqlWriterLifecycleReceipt_REGISTER, Writer: writer, ExpectedEpoch: meta.WriterEpoch, ExpectedCatalogRevision: meta.CatalogRevision, AuthoritySchema: fixture.Config.AuthoritySchema(), ProtectedSchemas: fixture.Config.ProtectedSchemas(), DeploymentDigest: opts.PhysicalAuthoritySHA256, DeploymentId: deployment, LockPrimitive: opts.LockPrimitive, LockQualificationSha256: opts.LockQualificationSHA256}
	request, err = authoritysql.CanonicalRegistrationRequest(request)
	require.NoError(t, err)
	_, err = fixture.Catalog.Register(ctx, request)
	require.NoError(t, err)
	writer = request.Writer
	grant := &dbpb.SqlWriterBootGrant{Format: "SQL_WRITER_BOOT_V1", WriterKey: writerID[:], InstanceId: instance[:], Generation: writer.Generation, AuthoritySchema: fixture.Config.AuthoritySchema(), ProtectedSchemas: fixture.Config.ProtectedSchemas(), SqlPrincipal: writer.SqlPrincipal, RegistrationDigest: writer.RegistrationDigest, DeploymentId: deployment, GrantId: grantID[:], GrantedMs: writer.RegisteredMs, Permission: "NORMAL", PhysicalAuthoritySha256: opts.PhysicalAuthoritySHA256, LockPrimitive: opts.LockPrimitive, LockQualificationSha256: opts.LockQualificationSHA256}
	raw, err := protojson.Marshal(grant)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	grantDirectory := t.TempDir()
	require.NoError(t, os.Chmod(grantDirectory, 0700))
	grantFile := filepath.Join(grantDirectory, "grant.json")
	require.NoError(t, os.WriteFile(grantFile, raw, 0600))
	admin, err := mysql.ParseDSN(opts.AdminDSN)
	require.NoError(t, err)
	admin.DBName = database
	var principal string
	require.NoError(t, fixture.Schema.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal))
	return []string{"MMORPG_SQL_WRITER_GRANT_FILE=" + grantFile, "MMORPG_SQL_WRITER_GRANT_SHA256=" + hex.EncodeToString(sum[:]), "MMORPG_SQL_WRITER_CONSUME_DIR=" + grantDirectory, "MMORPG_SQL_DEPLOYMENT_ID=" + deployment, "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256=" + hex.EncodeToString(opts.PhysicalAuthoritySHA256), "MMORPG_SQL_SCHEMA_PRINCIPAL=" + principal, "RANK_MAIL_CHAIN_SCHEMA_DSN=" + admin.FormatDSN(), "RANK_MAIL_CHAIN_NORMAL_DSN=" + normal.FormatDSN()}
}

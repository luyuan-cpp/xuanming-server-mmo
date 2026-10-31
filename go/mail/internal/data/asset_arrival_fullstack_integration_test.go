package data

// 正式到账验收（不启动服务、不清现有玩家数据、不回滚玩家资产）：
//   cd go/mail
//   go test -p 1 ./internal/data -run TestFullstackMailAttachmentsReachPlayerSQLExactlyOnce -count=1 -v
//
// 必须显式设置 MAIL_ASSET_FULLSTACK_REQUIRE=1、MAIL_ASSET_FULLSTACK_MODE=dev、
// MAIL_ASSET_FULLSTACK_DEDICATED=1、MAIL_ASSET_FULLSTACK_PLAYER_ID、
// MAIL_ASSET_FULLSTACK_ADMIN_DSN（无库名，只用于创建/删除本次随机邮件库）、
// MAIL_ASSET_FULLSTACK_PLAYER_DSN（含实际玩家分片库名，仅查询及短时锁定专用玩家行）、
// MAIL_ASSET_FULLSTACK_DATASERVICE_ADDR、MAIL_ASSET_FULLSTACK_ETCD_ADDR、
// MAIL_ASSET_FULLSTACK_REDIS_ADDR、MAIL_ASSET_FULLSTACK_TABLE_DIR、
// MAIL_ASSET_FULLSTACK_ITEM_ID（非堆叠模板）及 MMORPG_ASSET_OP_SECRET_MAIL。
// Redis 有密码时另设 MAIL_ASSET_FULLSTACK_REDIS_PASSWORD；所有地址只允许本机回环。
//
// 玩家必须由外部专用机器人登录并保持在线/空闲，背包有空位、无欠款/冻结/战斗/交接，
// 不得同时领取正式邮件或参与其他奖励。scene 须加载相同物品表、MAIL 密钥与真实 item 号段；
// home zone 的 DB 须为 dual 且指向 TiDB，DataService 须提供 SQL 证据。
// ID 使用正式 DataService 的 mail/mail_asset_op 号源；它们全局唯一，不复用假 ID。
// 本次随机业务库首次建流使用正式的建行毫秒纪元；不能与正式 mail 共用这个专用角色。
// 未开启 require 时仅跳过，不代表全栈通过；开启后缺任一前置必须失败。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	playerpb "proto/common/database"
	guildpb "proto/guild"
	mailpb "proto/mail"
	"schemamigrate"
	"shared/assetop"
	"shared/authoritysql"
	"shared/domainhost"
	tables "shared/generated/table"
	"shared/scenenode"
)

type mailFullstackRig struct {
	ctx      context.Context
	player   uint64
	item     uint32
	repo     *Repo
	playerDB *sql.DB
	caller   *assetop.Caller
	ledger   *assetop.DataServiceLedger
	loop     *assetop.Loop
}

// 这份结构只装真实 SELECT 的结果，不提供任何账本或资产替身。
type mailFullstackSQL struct {
	currency *componentpb.CurrencyComp
	bag      *playerpb.BagAllData
	ledger   *componentpb.PlayerAssetOpLedgerComp
	epoch    uint64
	seq      uint64
}

func TestFullstackMailAttachmentsReachPlayerSQLExactlyOnce(t *testing.T) {
	rig := newMailFullstackRig(t)
	before := rig.readSQL(t)
	if len(before.currency.Values) == 0 || len(before.currency.BlockedTypes) != 0 || len(before.currency.Debts) != 0 || before.currency.Values[0] > math.MaxUint64-18 {
		t.Fatal("专用角色的金币槽、冻结、欠款或余额上界不满足验收前置")
	}
	beforeCount, beforeIDs := mailFullstackItems(t, before.bag, rig.item)
	for _, stream := range before.ledger.Streams {
		if stream.Stream == assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT && stream.StreamEpoch >= uint64(time.Now().UnixMilli()) {
			t.Fatal("专用角色已有不早于本次建行时钟的邮件流纪元，拒绝撞流")
		}
	}

	sent, err := rig.repo.Send(rig.ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, &mailpb.SendMailRequest{
		SenderKind: SenderKind("admin"), RequestId: 1, RecipientPlayerId: rig.player,
		Title: "专用到账验收", Body: "真实scene和SQL到账；此奖励留在专用测试角色",
		Attachments: &mailpb.MailAttachments{
			Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 17}},
			Items:      []*mailpb.MailItem{{ConfigId: rig.item, Count: 1}},
		},
	})
	if err != nil {
		t.Fatalf("正式发信失败: %v", err)
	}
	claim, err := rig.repo.Claim(rig.ctx, rig.player, sent.MailId)
	if err != nil || claim.GetAssetOpId() == 0 || claim.GetClaimState() != mailpb.MailClaimState_MAIL_CLAIM_PENDING {
		t.Fatalf("正式领取未创建唯一在途指令: %v", err)
	}
	op := rig.readOp(t, claim.AssetOpId)
	t.Logf("专用邮件到账开始 mail_id=%d op_id=%d stream_epoch=%d seq=%d", sent.MailId, op.OpID, op.StreamEpoch, op.Seq)

	rig.assertPendingWhileSQLLocked(t, sent.MailId, op, before)
	committed := rig.awaitClaimed(t, sent.MailId, op)
	count, ids := mailFullstackItems(t, committed.bag, rig.item)
	if len(committed.currency.Values) == 0 || committed.currency.Values[0] != before.currency.Values[0]+17 || count != beforeCount+1 || len(ids) != len(beforeIDs)+1 || committed.epoch < before.epoch || committed.seq == 0 {
		t.Fatal("业务首次终态对应的SQL资产增量或原实例身份不符")
	}
	for id := range beforeIDs {
		if !ids[id] {
			t.Fatal("领取覆盖了原有物品GUID")
		}
	}
	proof, err := rig.ledger.ReadSQLCommittedLedger(rig.ctx, rig.player)
	if err != nil || !proto.Equal(proof, committed.ledger) {
		t.Fatalf("DataService SQL证据必须与实际玩家SQL账本一致: %v", err)
	}
	// 重投真实库里读出的同一操作，不占第二个seq，也不手设SQLCommitted。
	res, err := rig.caller.Do(rig.ctx, assetop.RPCCredit, op.Request())
	if err != nil || res.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || res.Partial {
		t.Fatalf("真实scene重投结局不一致: %v", err)
	}
	replayed, err := rig.repo.Claim(rig.ctx, rig.player, sent.MailId)
	if err != nil || replayed.GetAssetOpId() != op.OpID || replayed.GetClaimState() != mailpb.MailClaimState_MAIL_CLAIM_CLAIMED {
		t.Fatalf("重复领取没有回放原业务终态: %v", err)
	}
	// 再经过一笔正式资产操作的SQL提交屏障，不能在重投后立即读旧SQL快照而假绿。
	// 这1金币也保留在专用角色；没有测试专用写接口或强造账本。
	marker, err := rig.repo.Send(rig.ctx, "admin", mailpb.MailKind_MAIL_KIND_PERSONAL, &mailpb.SendMailRequest{
		SenderKind: SenderKind("admin"), RequestId: 2, RecipientPlayerId: rig.player,
		Title: "重投后提交屏障", Attachments: &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 1}}},
	})
	if err != nil {
		t.Fatalf("创建正式重投提交屏障失败: %v", err)
	}
	markerClaim, err := rig.repo.Claim(rig.ctx, rig.player, marker.MailId)
	if err != nil || markerClaim.GetAssetOpId() == 0 {
		t.Fatalf("领取重投提交屏障失败: %v", err)
	}
	markerOp := rig.readOp(t, markerClaim.AssetOpId)
	if markerOp.StreamEpoch != op.StreamEpoch || markerOp.Seq != op.Seq+1 {
		t.Fatal("正式邮件流没有连续分配下一笔seq")
	}
	after := rig.awaitClaimed(t, marker.MailId, markerOp)
	markerOutcome, partial, _ := assetop.ClassifyPersisted(after.ledger, markerOp.Stream, markerOp.StreamEpoch, markerOp.Seq)
	expectedCurrency := proto.Clone(committed.currency).(*componentpb.CurrencyComp)
	expectedCurrency.Values[0]++
	if markerOutcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || partial ||
		!mailFullstackReceipt(after.ledger, markerOp) ||
		!proto.Equal(after.currency, expectedCurrency) || !proto.Equal(after.bag, committed.bag) {
		t.Fatal("重投后的新SQL提交丢失屏障、重复增发资产或改变原GUID")
	}
	firstOutcome, firstPartial, _ := assetop.ClassifyPersisted(after.ledger, op.Stream, op.StreamEpoch, op.Seq)
	if firstOutcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || firstPartial || !mailFullstackReceipt(after.ledger, op) {
		t.Fatal("新提交改变了首笔资产的固定结局")
	}
	proof, err = rig.ledger.ReadSQLCommittedLedger(rig.ctx, rig.player)
	if err != nil || !proto.Equal(proof, after.ledger) {
		t.Fatalf("重投屏障后的实际SQL与收据证据不一致: %v", err)
	}
	var intents int
	if err := rig.repo.db.QueryRowContext(rig.ctx, "SELECT COUNT(*) FROM mail_asset_op WHERE mail_id=?", sent.MailId).Scan(&intents); err != nil || intents != 1 {
		t.Fatalf("重复领取生成第二条资产意图: %v", err)
	}
	t.Log("正式邮件附件→scene→玩家TiDB→SQL证据→业务终态通过；奖励保留在专用角色，未清理玩家资产")
}

func mailFullstackReceipt(ledger *componentpb.PlayerAssetOpLedgerComp, op assetop.Op) bool {
	req := op.Request()
	req.Auth = &assetpb.AssetOpAuth{Caller: "mail"}
	digest := sha256.Sum256(assetop.Canonical(assetop.RPCDebit, req, 0))
	var found *componentpb.AssetOpReceipt
	var selected *componentpb.AssetOpStreamLedger
	for _, stream := range ledger.GetStreams() {
		if stream.GetStream() != op.Stream {
			continue
		}
		if selected != nil || stream.GetStreamEpoch() != op.StreamEpoch {
			return false
		}
		selected = stream
	}
	for _, receipt := range selected.GetReceipts() {
		if receipt.GetSeq() != op.Seq {
			continue
		}
		if found != nil || receipt.GetCorrelationId() != op.CorrelationID || !bytes.Equal(receipt.GetRequestDigest(), digest[:]) ||
			len(receipt.GetSnapshot().GetItems()) != 0 || receipt.GetSnapshot().GetPet() != nil {
			return false
		}
		found = receipt
	}
	return found != nil
}

func newMailFullstackRig(t *testing.T) *mailFullstackRig {
	t.Helper()
	if os.Getenv("MAIL_ASSET_FULLSTACK_REQUIRE") != "1" {
		t.Skip("未显式开启MAIL_ASSET_FULLSTACK_REQUIRE=1；未执行全栈到账验收")
	}
	if mailFullstackEnv(t, "MODE") != "dev" || mailFullstackEnv(t, "DEDICATED") != "1" {
		t.Fatal("全栈到账验收只允许显式dev专用角色，拒绝prod或共享业务角色")
	}
	player, err := strconv.ParseUint(mailFullstackEnv(t, "PLAYER_ID"), 10, 64)
	if err != nil || player == 0 {
		t.Fatal("专用PLAYER_ID无效")
	}
	item, err := strconv.ParseUint(mailFullstackEnv(t, "ITEM_ID"), 10, 32)
	if err != nil || item == 0 {
		t.Fatal("专用ITEM_ID无效")
	}
	items := tables.NewItemTableManager()
	if err := items.Load(mailFullstackEnv(t, "TABLE_DIR"), false); err != nil {
		t.Fatalf("真实物品表未就绪: %v", err)
	}
	itemRow, ok := items.FindById(uint32(item))
	if !ok || itemRow.MaxStackSize != 1 {
		t.Fatal("验收物品必须是实际非堆叠模板，以核对新GUID和重放身份")
	}
	secret := os.Getenv("MMORPG_ASSET_OP_SECRET_MAIL")
	if secret == "" {
		t.Fatal("缺少与真实scene一致的MAIL资产密钥")
	}
	signer, err := assetop.NewSigner("mail", secret)
	if err != nil {
		t.Fatal("MAIL资产签名器配置无效")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	rig := &mailFullstackRig{ctx: ctx, player: player, item: uint32(item)}

	playerCfg := mailFullstackDSN(t, "PLAYER_DSN", true)
	rig.playerDB, err = authoritysql.Open(ctx, playerCfg.FormatDSN(), playerCfg.DBName, authoritysql.BackendTiDB, 2, authoritysql.Reader())
	if err != nil {
		t.Fatalf("实际玩家TiDB不可用: %v", err)
	}
	t.Cleanup(func() { _ = rig.playerDB.Close() })
	initial := rig.readSQL(t)
	if initial.epoch == 0 || initial.seq == 0 {
		t.Fatal("实际玩家SQL没有dual围栏；不能验收为强一致到账")
	}

	dsAddr := mailFullstackEnv(t, "DATASERVICE_ADDR")
	mailFullstackLocal(t, dsAddr)
	ids, err := domainhost.OpenIDs(ctx, zrpc.RpcClientConf{Endpoints: []string{dsAddr}, Timeout: 2000}, "mail", "mail_asset_op")
	if err != nil {
		t.Fatalf("真实DataService号段/连接未就绪: %v", err)
	}
	t.Cleanup(ids.Close)
	rig.ledger = &assetop.DataServiceLedger{Client: ids.Data, Timeout: 300 * time.Millisecond}
	baseline, err := rig.ledger.ReadSQLCommittedLedger(ctx, player)
	if err != nil || baseline == nil || !proto.Equal(baseline, initial.ledger) {
		t.Fatalf("实际玩家SQL与DataService证据未收敛: %v", err)
	}

	redisAddr, etcdAddr := mailFullstackEnv(t, "REDIS_ADDR"), mailFullstackEnv(t, "ETCD_ADDR")
	mailFullstackLocal(t, redisAddr)
	mailFullstackLocal(t, etcdAddr)
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, Password: os.Getenv("MAIL_ASSET_FULLSTACK_REDIS_PASSWORD"), PoolSize: 2, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal("本地玩家位置Redis不可用")
	}
	etcd, err := clientv3.New(clientv3.Config{Endpoints: []string{etcdAddr}, DialTimeout: time.Second})
	if err != nil {
		t.Fatal("本地etcd配置无效")
	}
	t.Cleanup(func() { _ = etcd.Close() })
	conns := scenenode.NewConnCache()
	t.Cleanup(conns.Close)
	watcher := scenenode.NewWatcher("mail_fullstack", scenenode.SceneNodeRpcPrefix, nil, func(e scenenode.NodeEntry) { conns.Remove(e.Endpoint) })
	watchCtx, stopWatch := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); watcher.Run(watchCtx, etcd) }()
	t.Cleanup(func() {
		stopWatch()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("场景镜像未在关闭预算内退出")
		}
	})
	mailFullstackAwait(t, ctx, "etcd首次同步", func() (bool, error) { return watcher.Synced(), nil })
	locator := &scenenode.Locator{Reader: scenenode.GoRedisReader{Client: rdb}, Watcher: watcher, Conns: conns}
	target, err := locator.Resolve(ctx, player)
	if err != nil || target.Location == nil || target.Location.GetOwnerEpoch() != initial.epoch {
		t.Fatalf("专用角色不在线、正在交接或SQL归属未收敛: %v", err)
	}
	mailFullstackLocal(t, target.Endpoint)
	rig.caller = &assetop.Caller{Resolver: locator, Signer: signer}

	adminCfg := mailFullstackDSN(t, "ADMIN_DSN", false)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "mail_fullstack_" + hex.EncodeToString(nonce[:])
	opts, err := sqltest.OptionsFromEnv(adminCfg.FormatDSN(), name+"_writer", name)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	install, err := fixture.OpenSchema(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	schemaTables := append(Tables(), &guildpb.GuildMemberRecord{})
	report, err := schemamigrate.Up(ctx, install, schemamigrate.Options{Database: name, Tables: schemaTables})
	if err != nil || len(report.Manual) != 0 {
		t.Fatalf("正式业务schema迁移未完成: %v", err)
	}
	if err = fixture.GrantProtoTables(ctx, name, schemaTables); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, name, 2)
	if err != nil {
		t.Fatal(err)
	}
	rig.repo = New(db, ids.IDs["mail"].Next, ids.IDs["mail_asset_op"].Next, func(id uint32) bool { _, ok := items.FindById(id); return ok }, name)
	rows, err := db.QueryContext(ctx, "SELECT m.mail_id FROM mail_message m WHERE "+rig.repo.visiblePredicate()+" LIMIT 0", 0, 0, 0)
	if err != nil {
		t.Fatalf("正式邮件可见性 schema 依赖未完成: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("关闭邮件 schema 前置查询失败: %v", err)
	}
	cfg := assetop.DefaultLoopConfig()
	cfg.RequireSQLCommit, cfg.Workers, cfg.Batch = true, 1, 1
	cfg.Streams = []assetpb.AssetOpStream{assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT}
	cfg.AbandonAfter, cfg.AuditInterval, cfg.OfflineAfterAttempts = 0, 0, 0
	rig.loop, err = assetop.NewLoop(cfg, StoreAdapter{Repo: rig.repo}, rig.caller, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.loop.Ledger = rig.ledger
	if err := rig.loop.CheckWiring(); err != nil {
		t.Fatal(err)
	}
	return rig
}

func (r *mailFullstackRig) readSQL(t *testing.T) *mailFullstackSQL {
	return r.readSQLContext(t, r.ctx)
}

func (r *mailFullstackRig) readSQLContext(t *testing.T, ctx context.Context) *mailFullstackSQL {
	t.Helper()
	row := &mailFullstackSQL{currency: &componentpb.CurrencyComp{}, bag: &playerpb.BagAllData{}, ledger: &componentpb.PlayerAssetOpLedgerComp{}}
	var currency, bag, ledger []byte
	if err := r.playerDB.QueryRowContext(ctx, "SELECT currency,bag_component,asset_op_ledger,fence_owner_epoch,fence_save_seq FROM player_database WHERE player_id=?", r.player).Scan(&currency, &bag, &ledger, &row.epoch, &row.seq); err != nil {
		t.Fatalf("直接读取实际玩家SQL失败: %v", err)
	}
	if proto.Unmarshal(currency, row.currency) != nil || proto.Unmarshal(bag, row.bag) != nil || proto.Unmarshal(ledger, row.ledger) != nil {
		t.Fatal("实际玩家SQL资产/账本编码无效")
	}
	return row
}

func (r *mailFullstackRig) readOp(t *testing.T, id uint64) assetop.Op {
	t.Helper()
	op := assetop.Op{OpID: id, PlayerID: r.player, CorrelationID: id, Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT, Bundle: &assetpb.AssetBundle{}}
	var payload []byte
	var stream uint32
	if err := r.repo.db.QueryRowContext(r.ctx, "SELECT stream,seq,stream_epoch,tx_type,payload FROM mail_asset_op WHERE op_id=? AND player_id=?", id, r.player).Scan(&stream, &op.Seq, &op.StreamEpoch, &op.TxType, &payload); err != nil {
		t.Fatal(err)
	}
	if assetpb.AssetOpStream(stream) != op.Stream {
		t.Fatal("正式邮件指令落入错误的资产流")
	}
	if proto.Unmarshal(payload, op.Bundle) != nil {
		t.Fatal("正式邮件意图编码损坏")
	}
	return op
}

// 只持有专用行的锁，不写资产。BeginTx 的5秒截止使database/sql自动回滚，
// defer另兜正常/失败退出；阶段4.5秒截止早于本轮DB的10秒锁等待。
func (r *mailFullstackRig) assertPendingWhileSQLLocked(t *testing.T, mailID uint64, op assetop.Op, before *mailFullstackSQL) {
	t.Helper()
	lockCtx, cancelLock := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancelLock()
	tx, err := r.playerDB.BeginTx(lockCtx, nil)
	if err != nil {
		t.Fatalf("创建专用玩家行锁失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	phaseCtx, cancelPhase := context.WithTimeout(lockCtx, 4500*time.Millisecond)
	defer cancelPhase()
	var locked uint64
	if err := tx.QueryRowContext(phaseCtx, "SELECT player_id FROM player_database WHERE player_id=? FOR UPDATE", r.player).Scan(&locked); err != nil || locked != r.player {
		t.Fatalf("锁定专用玩家行失败: %v", err)
	}
	lockedBefore := r.readSQLContext(t, phaseCtx)
	if !proto.Equal(lockedBefore.currency, before.currency) || !proto.Equal(lockedBefore.bag, before.bag) || !proto.Equal(lockedBefore.ledger, before.ledger) {
		t.Fatal("锁定前专用玩家资产已被其他写入改变")
	}
	if n := r.loop.Tick(phaseCtx); n != 1 {
		t.Fatalf("行锁负向阶段必须真实投递唯一邮件指令，实际处理%d条", n)
	}
	mailFullstackAwait(t, phaseCtx, "行锁期间真实Redis父账本已应用", func() (bool, error) {
		persisted, err := r.ledger.ReadPersistedLedger(phaseCtx, r.player)
		if err != nil {
			return false, err
		}
		outcome, partial, _ := assetop.ClassifyPersisted(persisted, op.Stream, op.StreamEpoch, op.Seq)
		return outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED && !partial && mailFullstackReceipt(persisted, op), nil
	})
	// 连接池有两条连接；普通SELECT经另一连接读取已提交行，绝不借锁事务读快照。
	blocked := r.readSQLContext(t, phaseCtx)
	if !proto.Equal(blocked.currency, lockedBefore.currency) || !proto.Equal(blocked.bag, lockedBefore.bag) || !proto.Equal(blocked.ledger, lockedBefore.ledger) || blocked.epoch != lockedBefore.epoch || blocked.seq != lockedBefore.seq {
		t.Fatal("持有真实SQL行锁时玩家提交行已变化，负向验证前置无效")
	}
	var state, status, durable uint32
	if err := r.repo.db.QueryRowContext(phaseCtx, "SELECT s.claim_state,o.status,o.durable FROM mail_state s JOIN mail_asset_op o ON o.op_id=s.current_op_id WHERE s.player_id=? AND s.mail_id=?", r.player, mailID).Scan(&state, &status, &durable); err != nil {
		t.Fatalf("读取行锁期间邮件状态失败: %v", err)
	}
	if state != uint32(mailpb.MailClaimState_MAIL_CLAIM_PENDING) || status != 1 || durable != 0 {
		t.Fatal("仅Redis已应用而SQL未提交时，邮件必须保持PENDING且不可宣称durable")
	}
	if err := phaseCtx.Err(); err != nil {
		t.Fatalf("行锁负向阶段超时，不能据已自动释放的锁宣称通过: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("释放专用玩家行锁失败: %v", err)
	}
	t.Log("真实SQL行锁负向通过：Redis已见同笔APPLIED及摘要，玩家SQL仍旧，邮件仍PENDING；已释放锁")
}

func (r *mailFullstackRig) awaitClaimed(t *testing.T, mailID uint64, op assetop.Op) *mailFullstackSQL {
	t.Helper()
	var committed *mailFullstackSQL
	mailFullstackAwait(t, r.ctx, "正式Loop和邮件业务终态", func() (bool, error) {
		r.loop.Tick(r.ctx)
		var state, status, durable uint32
		err := r.repo.db.QueryRowContext(r.ctx, "SELECT s.claim_state,o.status,o.durable FROM mail_state s JOIN mail_asset_op o ON o.op_id=s.current_op_id WHERE s.player_id=? AND s.mail_id=?", r.player, mailID).Scan(&state, &status, &durable)
		if err != nil {
			return false, err
		}
		if status != 1 && status != 2 {
			return false, fmt.Errorf("资产指令未全额应用: status=%d", status)
		}
		if state != uint32(mailpb.MailClaimState_MAIL_CLAIM_CLAIMED) {
			return false, nil
		}
		// 首次见CLAIMED立即核SQL，不允许先终态、另等SQL补齐而假绿。
		committed = r.readSQL(t)
		outcome, partial, _ := assetop.ClassifyPersisted(committed.ledger, op.Stream, op.StreamEpoch, op.Seq)
		if status != 2 || durable != 1 || outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || partial || !mailFullstackReceipt(committed.ledger, op) {
			return false, errors.New("首次CLAIMED没有同笔实际SQL提交收据")
		}
		proof, err := r.ledger.ReadSQLCommittedLedger(r.ctx, r.player)
		if err != nil || !proto.Equal(proof, committed.ledger) {
			return false, fmt.Errorf("首次CLAIMED没有对应DataService SQL证据: %v", err)
		}
		return true, nil
	})
	return committed
}

func mailFullstackItems(t *testing.T, bag *playerpb.BagAllData, config uint32) (uint64, map[uint64]bool) {
	t.Helper()
	count, ids := uint64(0), map[uint64]bool{}
	visit := func(items []*playerpb.ItemEntry) {
		for _, item := range items {
			if item.GetItemUuid() == 0 {
				t.Fatal("实际SQL背包含零GUID")
			}
			if item.GetConfigId() == config {
				if ids[item.ItemUuid] {
					t.Fatal("实际SQL重复物品GUID")
				}
				ids[item.ItemUuid] = true
				count += uint64(item.StackSize)
			}
		}
	}
	visit(bag.GetItems())
	for _, dynamic := range bag.GetDynamicBags() {
		visit(dynamic.GetItems())
	}
	return count, ids
}

func mailFullstackEnv(t *testing.T, suffix string) string {
	t.Helper()
	name := "MAIL_ASSET_FULLSTACK_" + suffix
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("已要求全栈验收，缺少%s", name)
	}
	return value
}

func mailFullstackLocal(t *testing.T, address string) {
	t.Helper()
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		t.Fatal("全栈验收只允许显式本机回环开发服务地址")
	}
}

func mailFullstackDSN(t *testing.T, name string, hasDatabase bool) *mysql.Config {
	t.Helper()
	cfg, err := mysql.ParseDSN(mailFullstackEnv(t, name))
	if err != nil || cfg.Net != "tcp" || (cfg.DBName != "") != hasDatabase {
		t.Fatalf("%s必须使用本地tcp且库名约束不符；不打印DSN", name)
	}
	mailFullstackLocal(t, cfg.Addr)
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = time.Second, 5*time.Second, 5*time.Second
	return cfg
}

func mailFullstackAwait(t *testing.T, ctx context.Context, stage string, check func() (bool, error)) {
	t.Helper()
	for {
		ok, err := check()
		if err != nil {
			t.Fatalf("%s失败: %v", stage, err)
		}
		if ok {
			return
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("%s超过全栈验收截止时间", stage)
		case <-timer.C:
		}
	}
}

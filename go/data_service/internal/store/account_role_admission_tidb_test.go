package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/placement"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"shared/ownerepoch"
	"shared/savebarrier"
)

func TestMaintenanceRecoveryTargetSeparatesScopeFromDebugAuthorization(t *testing.T) {
	target := MaintenanceRecoveryTarget{ID: strings.Repeat("a", 32), SQLAddr: "tidb.maintenance.example:4000"}
	require.True(t, target.Valid(), "正式维护目标不以loopback作为身份或权限")
	for _, bad := range []MaintenanceRecoveryTarget{{ID: strings.Repeat("0", 32), SQLAddr: target.SQLAddr}, {ID: target.ID, SQLAddr: ""}, {ID: target.ID, SQLAddr: "root:secret@tidb.example:4000"}, {ID: target.ID, SQLAddr: "tidb.example:04000"}, {ID: target.ID, SQLAddr: "tidb.example:0"}, {ID: target.ID, SQLAddr: "tidb.example|other:4000"}} {
		require.False(t, bad.Valid())
	}
}

// 真实维护正向链从正式初始SQL双行与真实登记/Begin开始；目标高E只由已编译维护CLI产生。
// 专用parent与mapping Redis必须为空命名空间；不SCAN/FLUSH，不清父或待投来制造前置。
func recoveredHolderTiDBFixture(t *testing.T) (*AccountDirectoryStore, *sql.DB, *AccountRoleAdmissionStore, *SQLRoleFreezeVerifier, *dbpb.AccountRoleAdmissionRecovery_Request) {
	t.Helper()
	addr, mappingAddr, binaryPath := os.Getenv("ADMISSION_RECOVERY_TEST_REDIS"), os.Getenv("ADMISSION_RECOVERY_TEST_MAPPING_REDIS"), os.Getenv("BARRIER_RESTORE_TEST_BINARY")
	if addr == "" && mappingAddr == "" && binaryPath == "" && os.Getenv("ADMISSION_RECOVERY_REQUIRE_TESTS") == "" {
		t.Skip("未启用旧holder完整维护链专用Redis/CLI")
	}
	require.Equal(t, "1", os.Getenv("ADMISSION_RECOVERY_REQUIRE_TESTS"))
	require.NotEmpty(t, addr)
	require.NotEmpty(t, mappingAddr)
	require.NotEqual(t, addr, mappingAddr)
	require.NotEmpty(t, binaryPath)
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{PlayerId: 11, ZoneId: 1})
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	identity := admissionTestRequest(11, 1, 1)
	_, err = a.Register(ctx, identity)
	require.NoError(t, err)
	trades := newTestAccountRoleTradeStore(d, testRoleFreezeVerifier())
	listing, err := trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Len(t, listing.Holders, 1)
	var storage uint32
	var routeRevision, placementVersion uint64
	var generation string
	var watermark uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT storage_id,placement_version,rev FROM player_route WHERE route_shard=? AND player_id=11", placement.RouteRowShard(11)).Scan(&storage, &placementVersion, &routeRevision))
	rowSchema, ok := placement.StoreDBName(storage)
	require.True(t, ok)
	// 沿正式维护方法冻结路由并钉当前已审水位，不修改玩家版本/业务数据。
	routes := &RouteStore{db: global}
	_, err = routes.FreezeRouteWriters(ctx)
	require.NoError(t, err)
	_, err = routes.UnfreezeRouteWriters(ctx, 1000)
	require.NoError(t, err)
	_, err = routes.FreezeRouteWriters(ctx)
	require.NoError(t, err)
	require.NoError(t, global.QueryRowContext(ctx, "SELECT generation FROM route_meta WHERE id=1").Scan(&generation))
	require.NoError(t, global.QueryRowContext(ctx, "SELECT watermark_ms FROM route_reconcile WHERE id=1").Scan(&watermark))
	require.Positive(t, watermark)
	cfg, err := mysql.ParseDSN(os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN"))
	require.NoError(t, err)
	cfg.DBName = rowSchema
	rows, err := authoritysql.Open(ctx, cfg.FormatDSN(), rowSchema, true, 1)
	require.NoError(t, err)
	t.Cleanup(func() { rows.Close() })
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(rows, rowSchema))
	first, second := &dbpb.PlayerDatabase{PlayerId: 11}, &dbpb.PlayerDatabase_1{PlayerId: 11}
	model.RegisterTable(first)
	model.RegisterTable(second)
	require.NoError(t, model.FindOneByPK(first))
	require.NoError(t, model.FindOneByPK(second))
	require.EqualValues(t, 1, first.FenceOwnerEpoch)
	require.EqualValues(t, 1, first.FenceSaveSeq)
	parent := &dbpb.PlayerAllData{PlayerDatabaseData: first, PlayerDatabase_1Data: second, LastSaveSeq: 1}
	parentRaw, err := proto.Marshal(parent)
	require.NoError(t, err)
	cache := redis.NewClient(&redis.Options{Addr: addr, DB: 14, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	mapping := redis.NewClient(&redis.Options{Addr: mappingAddr, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { cache.Close(); mapping.Close() })
	maintenance := "old-holder-" + d.schema
	// 固定home1的原工具MaintenanceKey(1)，只登记本夹具实际创建的键以便精确清理。
	maintenanceKey := "player:save_barrier:maintenance:1"
	parentKey := savebarrier.ParentKey(11)
	coreMarker := fmt.Sprintf("maintenance_recovery:target:promotion:1:%s:core", rowSchema)
	mapMarker := fmt.Sprintf("maintenance_recovery:target:promotion:1:%s:mapping", rowSchema)
	ownedCache := []string{ownerepoch.OwnerEpochKey(11), savebarrier.Key(11), parentKey, maintenanceKey, coreMarker, coreMarker + ":nonce"}
	ownedMapping := []string{placement.HomeZoneKey(11), placement.Key(11), placement.MapRevKey(11), placement.RouteWatermarkKey, maintenanceKey, placement.MergeFenceKey(1), mapMarker, mapMarker + ":nonce"}
	for _, group := range []struct {
		client *redis.Client
		keys   []string
	}{{cache, ownedCache}, {mapping, ownedMapping}} {
		for _, key := range group.keys {
			n, err := group.client.Exists(ctx, key).Result()
			require.NoError(t, err)
			require.Zero(t, n, "不接管任何既有Redis键")
		}
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		require.NoError(t, cache.Del(cleanup, ownedCache...).Err())
		require.NoError(t, mapping.Del(cleanup, ownedMapping...).Err())
	})
	created, err := cache.SetNX(ctx, ownerepoch.OwnerEpochKey(11), "17", 0).Result()
	require.NoError(t, err)
	require.True(t, created)
	created, err = cache.SetNX(ctx, parentKey, parentRaw, 0).Result()
	require.NoError(t, err)
	require.True(t, created)
	var nonce [16]byte
	_, err = rand.Read(nonce[:])
	require.NoError(t, err)
	localTarget := hex.EncodeToString(nonce[:])
	for _, schema := range []string{d.schema, rowSchema} {
		_, err := global.ExecContext(ctx, "CREATE TABLE `"+schema+"`.maintenance_recovery_target(purpose VARBINARY(16) NOT NULL,zone_id INT UNSIGNED NOT NULL,schema_name VARBINARY(64) NOT NULL,target_nonce VARBINARY(32) NOT NULL,physical_addr VARBINARY(255) NOT NULL,PRIMARY KEY(purpose,zone_id,schema_name))")
		require.NoError(t, err)
		for _, purpose := range []string{"promotion", "admission"} {
			_, err = global.ExecContext(ctx, "INSERT INTO `"+schema+"`.maintenance_recovery_target(purpose,zone_id,schema_name,target_nonce,physical_addr) VALUES(?,1,?,?,?)", purpose, []byte(schema), localTarget, cfg.Addr)
			require.NoError(t, err)
		}
	}
	for i, client := range []*redis.Client{cache, mapping} {
		role := []string{"core", "mapping"}[i]
		key := []string{coreMarker, mapMarker}[i]
		scope := fmt.Sprintf("maintenance-recovery-v1|promotion|%s|1|%s|%s|%s|%d", role, d.schema, rowSchema, client.Options().Addr, client.Options().DB)
		created, err := client.SetNX(ctx, key, scope, 0).Result()
		require.NoError(t, err)
		require.True(t, created)
		_, err = rand.Read(nonce[:])
		require.NoError(t, err)
		created, err = client.SetNX(ctx, key+":nonce", hex.EncodeToString(nonce[:]), 0).Result()
		require.NoError(t, err)
		require.True(t, created)
	}
	v := savebarrier.Version(1, 1)
	oldR := "17|" + admissionTestInstance + "|1|" + strings.Repeat("a", 32) + "|R|" + strings.Repeat("b", 32)
	require.NoError(t, cache.HSet(ctx, savebarrier.Key(11), "pending", v, "player_database", v, "player_database_1", v, "schema_gen", strconv.FormatUint(uint64(first.PersistSchemaGen), 10), "handoff_fence", oldR).Err())
	wm, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: generation, WatermarkMs: watermark})
	require.NoError(t, err)
	for key, value := range map[string]string{placement.HomeZoneKey(11): "1", placement.Key(11): placement.StableValue(storage, placementVersion), placement.MapRevKey(11): strconv.FormatUint(routeRevision, 10), placement.RouteWatermarkKey: wm} {
		created, err := mapping.SetNX(ctx, key, value, 0).Result()
		require.NoError(t, err)
		require.True(t, created)
	}
	temp := t.TempDir()
	planFile, evidenceFile := filepath.Join(temp, "plan.json"), filepath.Join(temp, "audit.json")
	evidence := []byte(`{"fixture":"no-live-writer","upper_bound":"19","queues_drained":true}`)
	require.NoError(t, os.WriteFile(evidenceFile, evidence, 0600))
	sum := sha256.Sum256(evidence)
	args := []string{"-schema", rowSchema, "-zone", "1", "-maintenance-id", maintenance, "-redis", addr, "-redis-db", "14", "-mapping-redis", mappingAddr, "-routing-mode", "db_first", "-route-schema", d.schema, "-audited-owner-upper-bound", "19", "-writers-stopped", "-queues-drained"}
	command := func(extra ...string) []byte {
		if len(args) > 0 && strings.Contains(strings.Join(args, " "), "-fence-only-promotion") {
			extra = append(extra, "-target-observation", filepath.Join(temp, "target-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".json"))
		}
		cmd := exec.CommandContext(ctx, binaryPath, append(append([]string{}, args...), extra...)...)
		cmd.Env = append(os.Environ(), "SAVE_BARRIER_RESTORE_DSN="+cfg.FormatDSN())
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "正式维护CLI输出: %s", string(output))
		return output
	}
	command("-mode", "guard")
	args = append(args, "-fence-only-promotion", "-plan", planFile, "-audit-evidence", evidenceFile, "-audit-evidence-sha256", hex.EncodeToString(sum[:]), "-local-target-id", localTarget, "-sql-target-address", cfg.Addr)
	command("-mode", "plan", "-players", "11")
	planRaw, err := os.ReadFile(planFile)
	require.NoError(t, err)
	planHash := sha256.Sum256(planRaw)
	output := command("-mode", "apply", "-plan-sha256", hex.EncodeToString(planHash[:]))
	receipt := &dbpb.SaveBarrierMaintenanceReceipt{}
	require.NoError(t, protojson.Unmarshal(bytes.TrimSpace(output), receipt))
	require.Equal(t, dbpb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED, receipt.Phase)
	var admissionDigest []byte
	require.NoError(t, global.QueryRowContext(ctx, "SELECT request_digest FROM account_role_admission WHERE player_id=11 AND scene_instance_id=?", []byte(admissionTestInstance)).Scan(&admissionDigest))
	in := &dbpb.AccountRoleAdmissionRecovery_Request{RequestId: roleImportTestID(23), SellerAccount: "seller", Seal: listing.Holders[0].Seal, Admission: identity.Identity, AdmissionRequestDigest: admissionDigest, MaintenanceReceiptId: receipt.ReceiptId, MaintenanceRequestDigest: receipt.RequestDigest}
	verifier, err := NewSQLRoleFreezeVerifier([]uint32{storage})
	require.NoError(t, err)
	return d, global, a, verifier, in
}

func TestTiDBRecoverOldHolderFromRealPromotionCLIAndPermanentReplay(t *testing.T) {
	d, global, a, verifier, in := recoveredHolderTiDBFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var localTarget MaintenanceRecoveryTarget
	require.NoError(t, global.QueryRowContext(ctx, "SELECT target_nonce,physical_addr FROM maintenance_recovery_target WHERE purpose='admission' AND zone_id=1 AND schema_name=?", []byte(d.schema)).Scan(&localTarget.ID, &localTarget.SQLAddr))
	_, err := a.PlanCrashedTradeHolder(ctx, in, verifier, localTarget)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "FREEZING不能先退休再让后台Confirm重新上架")
	var beforeRecovery int
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_admission_recovery").Scan(&beforeRecovery))
	require.Zero(t, beforeRecovery)
	trades := newTestAccountRoleTradeStore(d, verifier)
	cancelOriginal := &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	waiting, err := trades.Cancel(ctx, cancelOriginal)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING, waiting.State, "正式Cancel先持久同listing取消原件；原holder未排空仍等待")
	for _, kind := range []string{"wrong-request-digest", "wrong-prior-floor", "wrong-closed", "new-generation", "wrong-maintenance"} {
		t.Run(kind, func(t *testing.T) {
			bad := proto.Clone(in).(*dbpb.AccountRoleAdmissionRecovery_Request)
			switch kind {
			case "wrong-request-digest":
				bad.AdmissionRequestDigest[0] ^= 1
			case "wrong-prior-floor":
				bad.ExpectedRequiredOwnerEpoch = 17
				bad.ExpectedRequiredSaveSeq = 2
			case "wrong-closed":
				bad.ExpectedClosedThrough = 1
			case "new-generation":
				bad.Admission.Generation++
				bad.Seal.ThroughGeneration++
			case "wrong-maintenance":
				bad.MaintenanceRequestDigest[0] ^= 1
			}
			_, err := a.PlanCrashedTradeHolder(ctx, bad, verifier, localTarget)
			require.Error(t, err)
			var n int
			require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_admission_recovery").Scan(&n))
			require.Zero(t, n)
		})
	}
	plan, err := a.PlanCrashedTradeHolder(ctx, in, verifier, localTarget)
	require.NoError(t, err)
	var count, drained int
	var closedBefore uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_admission_recovery").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, global.QueryRowContext(ctx, "SELECT closed_through FROM account_role_admission WHERE player_id=11").Scan(&closedBefore))
	require.Zero(t, closedBefore)
	_, err = a.ApplyCrashedTradeHolder(ctx, in, verifier, MaintenanceRecoveryTarget{ID: strings.Repeat("0", 32), SQLAddr: localTarget.SQLAddr}, plan.SqlProofDigest)
	require.Error(t, err, "换SQL实例marker必须在任何副作用前拒绝")
	wrongProof := bytes.Clone(plan.SqlProofDigest)
	wrongProof[0] ^= 1
	_, err = a.ApplyCrashedTradeHolder(ctx, in, verifier, localTarget, wrongProof)
	require.Error(t, err, "不接受新校验结果替换审定SQL正文")
	require.NoError(t, global.QueryRowContext(ctx, "SELECT drained FROM account_role_trade_holder WHERE listing_id=101").Scan(&drained))
	require.Zero(t, drained)
	// 真实TiDB退休事务已经提交，只在测试侧丢一次客户端COMMIT确认；同原件必须可只读恢复。
	// 不注入生产hook、不伪造网络断线，故障期间没有其它使用该directory的并发请求。
	cfg, err := mysql.ParseDSN(os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN"))
	require.NoError(t, err)
	cfg.DBName = d.schema
	cfg.Timeout = time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	connector, err := mysql.NewConnector(cfg)
	require.NoError(t, err)
	fault := &admissionRecoveryCommitReplyLoss{}
	faultDB := sql.OpenDB(admissionRecoveryCommitReplyConnector{inner: authoritysql.PinCurrentSession(connector, true), fault: fault})
	defer faultDB.Close()
	faultDB.SetMaxOpenConns(1)
	require.NoError(t, authoritysql.AssertCurrentSession(ctx, faultDB, true))
	originalDB := d.db
	func() {
		d.db = faultDB
		defer func() { d.db = originalDB }()
		receipt, err := a.ApplyCrashedTradeHolder(ctx, in, verifier, localTarget, plan.SqlProofDigest)
		require.Error(t, err)
		require.Nil(t, receipt)
		require.True(t, fault.lost.Load(), "底层真实COMMIT必须已成功")
	}()
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_admission_recovery WHERE request_id=?", in.RequestId).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, global.QueryRowContext(ctx, "SELECT drained FROM account_role_trade_holder WHERE listing_id=101").Scan(&drained))
	require.Equal(t, 1, drained)
	type recoveryResult struct {
		receipt *dbpb.AccountRoleAdmissionRecovery
		err     error
	}
	results := make(chan recoveryResult, 8)
	var writers sync.WaitGroup
	for n := 0; n < 8; n++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			receipt, err := a.ApplyCrashedTradeHolder(ctx, proto.Clone(in).(*dbpb.AccountRoleAdmissionRecovery_Request), verifier, localTarget, plan.SqlProofDigest)
			results <- recoveryResult{receipt, err}
		}()
	}
	writers.Wait()
	close(results)
	var first *dbpb.AccountRoleAdmissionRecovery
	for result := range results {
		require.NoError(t, result.err)
		if first == nil {
			first = result.receipt
		} else {
			require.True(t, proto.Equal(first, result.receipt), "并发只回同一永久原件")
		}
	}
	require.NotNil(t, first)
	require.EqualValues(t, 20, first.RequiredOwnerEpoch)
	require.EqualValues(t, 1, first.RequiredSaveSeq)
	var actualAck []byte
	require.NoError(t, global.QueryRowContext(ctx, "SELECT ack_digest FROM account_role_trade_holder WHERE listing_id=101 AND scene_instance_id=?", []byte(admissionTestInstance)).Scan(&actualAck))
	rpcDigest, err := roleTradeDigest(&dspb.AckRoleTradeDrainRequest{Seal: in.Seal, RequiredOwnerEpoch: 20, RequiredSaveSeq: 1})
	require.NoError(t, err)
	require.Equal(t, first.RequestDigest, actualAck)
	require.NotEqual(t, rpcDigest, actualAck, "维护收据不是伪造的Scene RPC ACK")
	for attempt := 0; attempt < 2; attempt++ {
		same, err := a.ApplyCrashedTradeHolder(ctx, proto.Clone(in).(*dbpb.AccountRoleAdmissionRecovery_Request), verifier, localTarget, plan.SqlProofDigest)
		require.NoError(t, err)
		require.True(t, proto.Equal(first, same))
	}
	bad := proto.Clone(in).(*dbpb.AccountRoleAdmissionRecovery_Request)
	bad.Admission.SessionId++
	_, err = a.ApplyCrashedTradeHolder(ctx, bad, verifier, localTarget, plan.SqlProofDigest)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = trades.Confirm(ctx, &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "已取消原件不能在恢复后重开上架")
	finished, err := trades.Cancel(ctx, cancelOriginal)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, finished.State, "退休原holder后重放同正式Cancel才解除")
	newAdmission := admissionTestRequest(11, 2, 1)
	newAdmission.Identity.OwnerEpoch = 22
	_, err = a.Register(ctx, newAdmission)
	require.NoError(t, err)
	_, err = a.ApplyCrashedTradeHolder(ctx, in, verifier, localTarget, plan.SqlProofDigest)
	require.NoError(t, err, "原永久收据重放不碰同owner新N")
	var highest, closed uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT highest_generation,closed_through FROM account_role_admission WHERE player_id=11 AND scene_instance_id=?", []byte(admissionTestInstance)).Scan(&highest, &closed))
	require.Equal(t, []uint64{2, 1}, []uint64{highest, closed})
}

type admissionRecoveryCommitReplyLoss struct{ lost atomic.Bool }
type admissionRecoveryCommitReplyConnector struct {
	inner driver.Connector
	fault *admissionRecoveryCommitReplyLoss
}

func (c admissionRecoveryCommitReplyConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c admissionRecoveryCommitReplyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &admissionRecoveryCommitReplyConn{Conn: conn, fault: c.fault}, nil
}

type admissionRecoveryCommitReplyConn struct {
	driver.Conn
	fault *admissionRecoveryCommitReplyLoss
	armed bool
}

func (c *admissionRecoveryCommitReplyConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *admissionRecoveryCommitReplyConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	c.armed = false
	return &admissionRecoveryCommitReplyTx{Tx: tx, conn: c}, nil
}
func (c *admissionRecoveryCommitReplyConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(q, "INSERT INTO account_role_admission_recovery(") {
		c.armed = true
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}
func (c *admissionRecoveryCommitReplyConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
}
func (c *admissionRecoveryCommitReplyConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}
func (c *admissionRecoveryCommitReplyConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}
func (c *admissionRecoveryCommitReplyConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

type admissionRecoveryCommitReplyTx struct {
	driver.Tx
	conn *admissionRecoveryCommitReplyConn
}

func (tx *admissionRecoveryCommitReplyTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.conn.armed && tx.conn.fault.lost.CompareAndSwap(false, true) {
		return errors.New("测试侧真实退休收据已提交但确认丢失")
	}
	return err
}

const admissionTestInstance = "11111111-2222-3333-4444-555555555555"

func roleImportTestID(n byte) []byte { out := make([]byte, 32); out[31] = n; return out }

func admissionTestRequest(player, generation, revision uint64) *dspb.RegisterRoleAdmissionRequest {
	return &dspb.RegisterRoleAdmissionRequest{Identity: &base.RoleAdmissionIdentity{PlayerId: player, SceneInstanceId: admissionTestInstance, Generation: generation, OwnerRevision: revision, OwnerEpoch: 17, SceneNodeId: "11", SceneId: 111, ZoneId: 1, SessionId: 7, LoginGeneration: 999}}
}
func TestTiDBRoleAdmissionFixedHolderCancelAndLateGenerations(t *testing.T) {
	d, db := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	in := admissionTestRequest(11, 1, 1)
	type result struct {
		out *dspb.RegisterRoleAdmissionResponse
		err error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := a.Register(ctx, proto.Clone(in).(*dspb.RegisterRoleAdmissionRequest))
			results <- result{out, err}
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for r := range results {
		require.NoError(t, r.err)
		require.True(t, proto.Equal(in.Identity, r.out.Identity))
		require.Zero(t, r.out.ClosedThrough)
		count++
	}
	require.Equal(t, 8, count)
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_admission").Scan(&rows))
	require.Equal(t, 1, rows)
	changed := proto.Clone(in).(*dspb.RegisterRoleAdmissionRequest)
	changed.Identity.SessionId++
	_, err = a.Register(ctx, changed)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	in.Identity.Generation = 7
	_, err = a.Register(ctx, in)
	require.NoError(t, err)
	trades := newTestAccountRoleTradeStore(d, testRoleFreezeVerifier())
	begin := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	first, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.Len(t, first.Holders, 1)
	require.Equal(t, uint64(7), first.Holders[0].Seal.ThroughGeneration)
	_, err = a.Register(ctx, admissionTestRequest(11, 8, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	closeReq := &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 7, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3}
	_, err = a.CloseAdmissions(ctx, closeReq)
	require.NoError(t, err)
	stop := &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	waiting, err := trades.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCEL_PENDING, waiting.State)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(101), owned.TradeListingId, "Close 不能代替 listing 墓碑/排空 ACK")
	ack := &dspb.AckRoleTradeDrainRequest{Seal: proto.Clone(first.Holders[0].Seal).(*base.RoleTradeSeal), RequiredOwnerEpoch: 17, RequiredSaveSeq: 3}
	bad := proto.Clone(ack).(*dspb.AckRoleTradeDrainRequest)
	bad.Seal.ThroughGeneration++
	require.ErrorIs(t, a.AckDrain(ctx, bad), accountdirectory.ErrConflict)
	require.NoError(t, a.AckDrain(ctx, ack))
	require.NoError(t, a.AckDrain(ctx, ack))
	bad = proto.Clone(ack).(*dspb.AckRoleTradeDrainRequest)
	bad.RequiredSaveSeq++
	require.ErrorIs(t, a.AckDrain(ctx, bad), accountdirectory.ErrConflict)
	finished, err := trades.Cancel(ctx, stop)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, finished.State)
	require.True(t, finished.Holders[0].Drained)
	_, err = a.Register(ctx, in)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "取消后旧 generation 不能再激活")
	_, err = a.Register(ctx, admissionTestRequest(11, 8, 1))
	require.NoError(t, err)
	require.NoError(t, a.AckDrain(ctx, ack), "旧 listing ACK 只重放原件，不关闭新 generation")
	var highest, closed, epoch, seq uint64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT highest_generation,closed_through,required_owner_epoch,required_save_seq FROM account_role_admission WHERE player_id=11").Scan(&highest, &closed, &epoch, &seq))
	require.Equal(t, []uint64{8, 7, 17, 3}, []uint64{highest, closed, epoch, seq})
	old, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED, old.State)
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Zero(t, owned.TradeListingId)
	_, err = a.Register(ctx, admissionTestRequest(11, 9, 2))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
}

// 真实 TiDB、独立随机 storage schema、正式 proto2mysql 建两张完整玩家表；只删本例已创建的库。
func roleProofStorage(t *testing.T, global *sql.DB, player uint64) (uint32, string, *sql.DB) {
	t.Helper()
	var nonce [4]byte
	_, err := rand.Read(nonce[:])
	require.NoError(t, err)
	id := placement.FirstNonZoneStorageID + binary.BigEndian.Uint32(nonce[:])%4000000000
	name, ok := placement.StoreDBName(id)
	require.True(t, ok)
	_, err = global.Exec("CREATE DATABASE `" + name + "`")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := global.Exec("DROP DATABASE `" + name + "`"); require.NoError(t, err) })
	cfg, err := mysql.ParseDSN(os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN"))
	require.NoError(t, err)
	cfg.DBName = name
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	handle, err := authoritysql.Open(ctx, cfg.FormatDSN(), name, true, 4)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(handle, name))
	for _, row := range []proto.Message{&dbpb.PlayerDatabase{PlayerId: player}, &dbpb.PlayerDatabase_1{PlayerId: player}} {
		model.RegisterTable(row)
		require.NoError(t, model.CreateOrUpdateTable(row))
		require.NoError(t, model.Insert(row))
	}
	_, err = global.Exec("INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,updated_by,created_at_ms,updated_at_ms) VALUES (?,?,1,?,1,1,'role-proof-test',1,1) ON DUPLICATE KEY UPDATE home_zone=VALUES(home_zone),storage_id=VALUES(storage_id),placement_version=VALUES(placement_version),rev=rev+1", placement.RouteRowShard(player), player, id)
	require.NoError(t, err)
	return id, name, handle
}

func setProofFence(t *testing.T, db *sql.DB, player, epoch, seq uint64) {
	t.Helper()
	for _, table := range []string{"player_database", "player_database_1"} {
		result, err := db.Exec("UPDATE "+table+" SET fence_owner_epoch=?,fence_save_seq=? WHERE player_id=?", epoch, seq, player)
		require.NoError(t, err)
		_, err = result.RowsAffected()
		require.NoError(t, err)
	}
}

func TestTiDBRoleAdmissionStrongSQLProofRejectsUnsyncedRowsAndKeepsReceipt(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	// 目录按正式初始事务建立后，本例再把路由切到专门的旧版/追盘证据夹具。
	id, _, rows := roleProofStorage(t, global, 11)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades, admissions := newTestAccountRoleTradeStore(d, v), NewAccountRoleAdmissionStore(d)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	begin := &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	frozen, err := trades.Begin(ctx, begin)
	require.NoError(t, err)
	confirm := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "未排空不能只凭SQL行放行")
	setProofFence(t, rows, 11, 17, 2)
	ack := &dspb.AckRoleTradeDrainRequest{Seal: frozen.Holders[0].Seal, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3}
	require.NoError(t, admissions.AckDrain(ctx, ack))
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "scene ACK不替代真实SQL已追上")
	_, err = rows.Exec("UPDATE player_database SET fence_save_seq=3 WHERE player_id=11")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "一表追上不能代替两表同版")
	setProofFence(t, rows, 11, 17, 3)
	frozen, err = trades.Confirm(ctx, confirm)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE, frozen.FreezeEvidence.Kind)
	require.Equal(t, []uint64{17, 3}, []uint64{frozen.FreezeEvidence.OwnerEpoch, frozen.FreezeEvidence.SaveSeq})
	require.Len(t, frozen.FreezeEvidence.PersistedDigest, 32)
	transferred, err := trades.Transfer(ctx, &dspb.TransferAccountRoleRequest{TxId: 1001, ListingId: 101, PlayerId: 11, SellerAccount: "seller", BuyerAccount: "buyer", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	require.Equal(t, uint64(2), transferred.BuyerOwnerRevision)
	require.NoError(t, admissions.AckDrain(ctx, ack), "过户后旧ACK仍只返回原成功")
	_, err = admissions.Register(ctx, admissionTestRequest(11, 2, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = admissions.Register(ctx, admissionTestRequest(11, 2, 2))
	require.NoError(t, err)
	require.NoError(t, admissions.AckDrain(ctx, ack))
	var highest, closed uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT highest_generation,closed_through FROM account_role_admission WHERE player_id=11").Scan(&highest, &closed))
	require.Equal(t, []uint64{2, 1}, []uint64{highest, closed})
}

func TestTiDBRoleAdmissionSQLProofNoAdmissionAndHistoricalFloor(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	// 目录按正式初始事务建立后，本例再把路由切到专门的旧版/追盘证据夹具。
	id, _, rows := roleProofStorage(t, global, 11)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades := newTestAccountRoleTradeStore(d, v)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	confirm := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	out, err := trades.Confirm(ctx, confirm)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_NO_ADMISSION, out.FreezeEvidence.Kind)
	require.Zero(t, out.FreezeEvidence.OwnerEpoch)
	_, err = trades.Cancel(ctx, &dspb.CancelAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1, RequiredOwnerEpoch: 17, RequiredSaveSeq: 9})
	require.NoError(t, err)
	_, err = a.Register(ctx, admissionTestRequest(11, 2, 1))
	require.NoError(t, err)
	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 2})
	require.NoError(t, err)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 102, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	confirm.ListingId = 102
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "无实体不能把历史SQL下限归零")
	setProofFence(t, rows, 11, 17, 9)
	out, err = trades.Confirm(ctx, confirm)
	require.NoError(t, err)
	require.Equal(t, dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE, out.FreezeEvidence.Kind)
	require.Empty(t, out.Holders, "已关闭登记不制造新holder，但历史floor仍必须检查")
}

func TestTiDBRoleAdmissionRegisterAndBeginShareCommitLock(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	a, trades := NewAccountRoleAdmissionStore(d), newTestAccountRoleTradeStore(d, nil)
	start := make(chan struct{})
	registered := make(chan error, 1)
	go func() { <-start; _, err := a.Register(ctx, admissionTestRequest(11, 1, 1)); registered <- err }()
	close(start)
	out, err := trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	registerErr := <-registered
	if registerErr == nil {
		require.Len(t, out.Holders, 1, "先提交的登记必须纳入本轮固定全集")
		require.Equal(t, uint64(1), out.Holders[0].Seal.ThroughGeneration)
	} else {
		require.ErrorIs(t, registerErr, accountdirectory.ErrConflict)
		require.Empty(t, out.Holders, "冻结先提交时不能出现未纳入holder的成功登记")
	}
	var listing uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT trade_listing_id FROM account_role WHERE player_id=11").Scan(&listing))
	require.Equal(t, uint64(101), listing)
	_, err = a.Register(ctx, admissionTestRequest(11, 2, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
}

func TestTiDBRoleAdmissionSQLProofRejectsRouteAndSchemaFailures(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := createInitialRoleForTest(d, ctx, "seller", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11})
	require.NoError(t, err)
	// 目录按正式初始事务建立后，本例再把路由切到专门的旧版/追盘证据夹具。
	id, _, rows := roleProofStorage(t, global, 11)
	v, err := NewSQLRoleFreezeVerifier([]uint32{id})
	require.NoError(t, err)
	trades := newTestAccountRoleTradeStore(d, v)
	_, err = trades.Begin(ctx, &dspb.BeginAccountRoleTradeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	confirm := &dspb.ConfirmAccountRoleTradeFreezeRequest{ListingId: 101, PlayerId: 11, SellerAccount: "seller", ExpectedOwnerRevision: 1}
	_, err = global.Exec("UPDATE player_route SET frozen_run_id='proof-maintenance' WHERE player_id=11")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = global.Exec("UPDATE player_route SET frozen_run_id=NULL WHERE player_id=11")
	require.NoError(t, err)
	deny, err := NewSQLRoleFreezeVerifier([]uint32{id + 1})
	require.NoError(t, err)
	trades.verifier = deny
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrUnavailable)
	trades.verifier = v
	_, err = rows.Exec("UPDATE player_database_1 SET persist_schema_gen=4294967295 WHERE player_id=11")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorContains(t, err, "世代")
	_, err = rows.Exec("DELETE FROM player_database_1 WHERE player_id=11")
	require.NoError(t, err)
	_, err = trades.Confirm(ctx, confirm)
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
	var state uint32
	require.NoError(t, global.QueryRowContext(ctx, "SELECT state FROM account_role_trade WHERE listing_id=101").Scan(&state))
	require.EqualValues(t, dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, state, "失败不能留下FROZEN原件")
	var receipts int
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_transfer_receipt").Scan(&receipts))
	require.Zero(t, receipts)
}

func TestTiDBRoleAdmissionOpenRejectsRemovalAndToolRestoreKeepsFloor(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, AppearanceId: "original"}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	a := NewAccountRoleAdmissionStore(d)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.NoError(t, err)
	require.ErrorIs(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}), accountdirectory.ErrConflict)
	for _, restore := range []bool{false, true} {
		_, err = d.ImportRole(ctx, "seller", role, restore, roleImportTestID(1))
		require.ErrorIs(t, err, accountdirectory.ErrConflict)
	}
	_, err = createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err, "普通建角原请求幂等不扩大成全部在线角色禁用")
	_, err = a.CloseAdmissions(ctx, &dspb.CloseRoleAdmissionsRequest{PlayerId: 11, SceneInstanceId: admissionTestInstance, OwnerRevision: 1, ThroughGeneration: 1, RequiredOwnerEpoch: 17, RequiredSaveSeq: 3})
	require.NoError(t, err)
	require.NoError(t, d.Remove(ctx, "seller", []*dspb.AccountRoleVersion{{PlayerId: 11, OwnerRevision: 1}}))
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(1))
	require.NoError(t, err)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(3), owned.OwnerRevision)
	var floorEpoch, floorSeq, closed uint64
	require.NoError(t, global.QueryRowContext(ctx, "SELECT required_owner_epoch,required_save_seq,closed_through FROM account_role_admission WHERE player_id=11").Scan(&floorEpoch, &floorSeq, &closed))
	require.Equal(t, []uint64{17, 3, 1}, []uint64{floorEpoch, floorSeq, closed})
}

func TestTiDBRoleImportReceiptRevokesOldRoutesAndDoesNotBumpOnResume(t *testing.T) {
	d, global := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	role := &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 11, AppearanceId: "original"}
	_, err := createInitialRoleForTest(d, ctx, "seller", role)
	require.NoError(t, err)
	type result struct{ err error }
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.ImportRole(ctx, "seller", role, false, roleImportTestID(1))
			results <- result{err}
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	for r := range results {
		require.NoError(t, r.err)
		count++
	}
	require.Equal(t, 8, count)
	owned, err := d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(2), owned.OwnerRevision)
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(1))
	require.NoError(t, err, "从false加replace恢复只能回原维护修订")
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(2), owned.OwnerRevision)
	a := NewAccountRoleAdmissionStore(d)
	_, err = a.Register(ctx, admissionTestRequest(11, 1, 1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "旧路由不能在目录改修订后入场")
	_, err = d.ImportRole(ctx, "other", role, true, roleImportTestID(1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "同维护键换目标账号拒绝")
	changed := proto.Clone(role).(*base.AccountSimplePlayer)
	changed.AppearanceId = "changed"
	_, err = d.ImportRole(ctx, "seller", changed, true, roleImportTestID(1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict)
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(2))
	require.NoError(t, err)
	owned, err = d.Lookup(ctx, 11)
	require.NoError(t, err)
	require.Equal(t, uint64(3), owned.OwnerRevision)
	_, err = d.ImportRole(ctx, "seller", role, true, roleImportTestID(1))
	require.ErrorIs(t, err, accountdirectory.ErrConflict, "之后新导入使旧包不能覆盖新状态")
	var receipts int
	require.NoError(t, global.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_role_import_receipt").Scan(&receipts))
	require.Equal(t, 2, receipts)
}

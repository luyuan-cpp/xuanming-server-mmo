package barrierrestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	componentpb "proto/common/component"
	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/ownerepoch"
	"shared/placement"
	"shared/savebarrier"
)

// 本例必须调用当轮正式CLI二进制；只构造隔离的已提交原快照，不预先删除父/待投或UPDATE高E。
// 目标E只能由生产CLI提升。未启用真库时按既有外部集成门控跳过，显式启用缺任一输入即失败。
func TestIntegrationFenceOnlyPromotionCLIConvergedSnapshotAndPendingPreserved(t *testing.T) {
	dsn, addr, strict := os.Getenv("BARRIER_RESTORE_TEST_DSN"), os.Getenv("BARRIER_RESTORE_TEST_REDIS"), os.Getenv("BARRIER_RESTORE_REQUIRE_TIDB")
	if dsn == "" && addr == "" && strict == "" {
		t.Skip("未启用维护CLI真TiDB/Redis集成")
	}
	require.NotEmpty(t, dsn)
	require.NotEmpty(t, addr)
	require.Equal(t, "1", strict)
	binaryPath, mappingAddr := os.Getenv("BARRIER_RESTORE_TEST_BINARY"), os.Getenv("BARRIER_RESTORE_TEST_MAPPING_REDIS")
	require.NotEmpty(t, binaryPath, "必须先按当前来源正式构建维护CLI")
	require.NotEmpty(t, mappingAddr)
	require.NotEqual(t, addr, mappingAddr)
	f := newRestoreIntegrationFixture(t, dsn, addr, true)
	id, first, second := f.player(t, 8, 90)
	before := f.assets(t, id)
	routeSchema := fmt.Sprintf("promote_routes_%d", f.zone)
	_, err := f.r.DB.ExecContext(f.ctx, "CREATE DATABASE `"+routeSchema+"`")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := f.r.DB.ExecContext(ctx, "DROP DATABASE `"+routeSchema+"`")
		require.NoError(t, err)
	})
	parsed, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	parsed.DBName = routeSchema
	migration, err := authoritysql.OpenForMigration(f.ctx, parsed.FormatDSN(), routeSchema, true)
	require.NoError(t, err)
	defer migration.Close()
	for _, ddl := range productionRouteDDL(t) {
		_, err := migration.ExecContext(f.ctx, ddl)
		require.NoError(t, err)
	}
	// 原维护表DDL从正式唯一const提取，不在测试另抄第二份事实。
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "data_service", "internal", "store", "account_role_admission_store.go"))
	require.NoError(t, err)
	match := regexp.MustCompile(`(?m)^const saveBarrierMaintenanceDDL = ("(?:[^"\\]|\\.)*")`).FindSubmatch(source)
	require.Len(t, match, 2)
	ddl, err := strconv.Unquote(string(match[1]))
	require.NoError(t, err)
	_, err = migration.ExecContext(f.ctx, ddl)
	require.NoError(t, err)
	generation := strings.Repeat("c", 32)
	_, err = migration.ExecContext(f.ctx, "INSERT INTO route_meta(id,generation,writable) VALUES(1,?,0)", generation)
	require.NoError(t, err)
	_, err = migration.ExecContext(f.ctx, "INSERT INTO route_reconcile(id,watermark_ms) VALUES(1,1000)")
	require.NoError(t, err)
	storage, ok := placement.StorageIDFromDBName(f.schema)
	require.True(t, ok)
	_, err = migration.ExecContext(f.ctx, "INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,created_at_ms,updated_at_ms) VALUES(?,?,?,?,1,4,1000,1000)", placement.RouteRowShard(id), id, f.zone, storage)
	require.NoError(t, err)
	var targetNonce [16]byte
	_, err = rand.Read(targetNonce[:])
	require.NoError(t, err)
	localTarget := hex.EncodeToString(targetNonce[:])
	for _, schema := range []string{f.schema, routeSchema} {
		_, err := f.r.DB.ExecContext(f.ctx, "CREATE TABLE `"+schema+"`.maintenance_recovery_target(purpose VARBINARY(16) NOT NULL,zone_id INT UNSIGNED NOT NULL,schema_name VARBINARY(64) NOT NULL,target_nonce VARBINARY(32) NOT NULL,physical_addr VARBINARY(255) NOT NULL,PRIMARY KEY(purpose,zone_id,schema_name))")
		require.NoError(t, err)
		_, err = f.r.DB.ExecContext(f.ctx, "INSERT INTO `"+schema+"`.maintenance_recovery_target(purpose,zone_id,schema_name,target_nonce,physical_addr) VALUES('promotion',?,?,?,?)", f.zone, []byte(schema), localTarget, parsed.Addr)
		require.NoError(t, err)
	}
	mapping := redis.NewClient(&redis.Options{Addr: mappingAddr, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { require.NoError(t, mapping.Close()) })
	coreMarker, _, err := MaintenanceRedisScope("promotion", "core", f.zone, routeSchema, f.schema, addr, f.r.Redis.Options().DB)
	require.NoError(t, err)
	mapMarker, _, err := MaintenanceRedisScope("promotion", "mapping", f.zone, routeSchema, f.schema, mappingAddr, mapping.Options().DB)
	require.NoError(t, err)
	mapKeys := []string{MaintenanceKey(f.zone), placement.HomeZoneKey(id), placement.Key(id), placement.MapRevKey(id), placement.MergeFenceKey(f.zone), placement.RouteWatermarkKey, mapMarker, mapMarker + ":nonce"}
	for _, key := range mapKeys {
		n, err := mapping.Exists(f.ctx, key).Result()
		require.NoError(t, err)
		require.Zero(t, n, "只用独占空mapping实例，不覆盖当前全局水位")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, mapping.Del(ctx, mapKeys...).Err())
	})
	watermark, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: generation, WatermarkMs: 1000})
	require.NoError(t, err)
	for key, value := range map[string]string{placement.HomeZoneKey(id): strconv.FormatUint(uint64(f.zone), 10), placement.Key(id): placement.StableValue(storage, 1), placement.MapRevKey(id): "4", placement.RouteWatermarkKey: watermark} {
		created, err := mapping.SetNX(f.ctx, key, value, 0).Result()
		require.NoError(t, err)
		require.True(t, created)
	}
	var parentTarget, mappingTarget string
	for i, client := range []*redis.Client{f.r.Redis, mapping} {
		role := []string{"core", "mapping"}[i]
		key, scope, err := MaintenanceRedisScope("promotion", role, f.zone, routeSchema, f.schema, client.Options().Addr, client.Options().DB)
		require.NoError(t, err)
		created, err := client.SetNX(f.ctx, key, scope, 0).Result()
		require.NoError(t, err)
		require.True(t, created)
		_, err = rand.Read(targetNonce[:])
		require.NoError(t, err)
		nonce := hex.EncodeToString(targetNonce[:])
		created, err = client.SetNX(f.ctx, key+":nonce", nonce, 0).Result()
		require.NoError(t, err)
		require.True(t, created)
		if client == f.r.Redis {
			parentTarget = nonce
		} else {
			mappingTarget = nonce
		}
	}
	f.ownedKeys = append(f.ownedKeys, coreMarker, coreMarker+":nonce")
	r := Restorer{DB: f.r.DB, Redis: f.r.Redis, MappingRedis: mapping, RoutingMode: RoutingModeDBFirst, RouteSchema: routeSchema, AuditedOwnerUpperBound: "21", LocalTargetID: localTarget, RedisTargetID: parentTarget, MappingTargetID: mappingTarget, MaintenanceSQLAddr: parsed.Addr}
	target := Plan{Format: FormatV2, Schema: f.schema, Zone: f.zone, RedisAddr: addr, MappingAddr: mappingAddr, RoutingMode: RoutingModeDBFirst, RouteSchema: routeSchema, MaintenanceID: f.maintenance}
	require.NoError(t, r.Guard(f.ctx, target))
	parent := &databasepb.PlayerAllData{PlayerDatabaseData: first, PlayerDatabase_1Data: second, LastSaveSeq: 90}
	parentRaw, err := proto.Marshal(parent)
	require.NoError(t, err)
	keys := recoveryKeys(f.zone, id)
	for _, key := range keys[2:6] {
		f.ownedKeys = append(f.ownedKeys, key)
	}
	created, err := r.Redis.SetNX(f.ctx, keys[2], parentRaw, 0).Result()
	require.NoError(t, err)
	require.True(t, created)
	v := savebarrier.Version(8, 90)
	oldR := fmt.Sprintf("8|original-scene|7|%s|R|%s", strings.Repeat("a", 32), strings.Repeat("b", 32))
	require.NoError(t, r.Redis.HSet(f.ctx, savebarrier.Key(id), "pending", v, "player_database", v, "player_database_1", v, "schema_gen", "1", "handoff_fence", oldR).Err())
	// outbox仍在即拒绝；测试不删它来凑成功。独立的坏玩家支路不参与下方已收敛成功原件。
	t.Run("已有待投不清不提升", func(t *testing.T) {
		pendingID, _, _ := f.player(t, 8, 90)
		pendingKey := savebarrier.Key(pendingID)
		require.NoError(t, r.Redis.HSet(f.ctx, pendingKey, "pending", v, "player_database", v, "player_database_1", v, "outbox_payload", "unprocessed-original").Err())
		_, err := migration.ExecContext(f.ctx, "INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,created_at_ms,updated_at_ms) VALUES(?,?,?,?,1,4,1000,1000)", placement.RouteRowShard(pendingID), pendingID, f.zone, storage)
		require.NoError(t, err)
		for key, value := range map[string]string{placement.HomeZoneKey(pendingID): strconv.FormatUint(uint64(f.zone), 10), placement.Key(pendingID): placement.StableValue(storage, 1), placement.MapRevKey(pendingID): "4"} {
			created, err := mapping.SetNX(f.ctx, key, value, 0).Result()
			require.NoError(t, err)
			require.True(t, created)
			mapKeys = append(mapKeys, key)
		}
		_, err = r.PreparePromotion(f.ctx, f.schema, f.zone, f.maintenance, strconv.FormatUint(pendingID, 10), bytes.Repeat([]byte{5}, 32))
		require.ErrorContains(t, err, "outbox still present")
		raw, err := r.Redis.HGet(f.ctx, pendingKey, "outbox_payload").Result()
		require.NoError(t, err)
		require.Equal(t, "unprocessed-original", raw)
	})
	temp := t.TempDir()
	planPath := filepath.Join(temp, "promotion.json")
	evidencePath := filepath.Join(temp, "audit.json")
	evidence := []byte(`{"fixture":"exclusive-owned-snapshot","writers_stopped":true,"queues_drained":true,"audited_upper_bound":"21"}`)
	require.NoError(t, os.WriteFile(evidencePath, evidence, 0600))
	evidenceSHA := sha256.Sum256(evidence)
	args := []string{"-fence-only-promotion", "-schema", f.schema, "-zone", strconv.FormatUint(uint64(f.zone), 10), "-maintenance-id", f.maintenance, "-plan", planPath, "-redis", addr, "-mapping-redis", mappingAddr, "-routing-mode", "db_first", "-route-schema", routeSchema, "-audited-owner-upper-bound", "21", "-writers-stopped", "-queues-drained", "-audit-evidence", evidencePath, "-audit-evidence-sha256", hex.EncodeToString(evidenceSHA[:]), "-local-target-id", localTarget, "-sql-target-address", parsed.Addr}
	parsed.DBName = f.schema
	command := func(extra ...string) []byte {
		extra = append(extra, "-target-observation", filepath.Join(temp, fmt.Sprintf("target-%d.json", time.Now().UnixNano())))
		cmd := exec.CommandContext(f.ctx, binaryPath, append(append([]string{}, args...), extra...)...)
		cmd.Env = append(os.Environ(), "SAVE_BARRIER_RESTORE_DSN="+parsed.FormatDSN())
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "维护CLI失败（输出不含凭据）: %s", string(output))
		return output
	}
	command("-mode", "plan", "-players", strconv.FormatUint(id, 10))
	planBytes, err := os.ReadFile(planPath)
	require.NoError(t, err)
	var envelope struct {
		Plan json.RawMessage `json:"plan"`
	}
	require.NoError(t, json.Unmarshal(planBytes, &envelope))
	plan := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
	require.NoError(t, protojson.Unmarshal(envelope.Plan, plan))
	require.EqualValues(t, 22, plan.TargetOwner)
	// 不以DEL制造前置：计划和执行前父原件都仍存在，实际业务未知字段未改。
	cacheBefore, err := r.Redis.Get(f.ctx, keys[2]).Bytes()
	require.NoError(t, err)
	require.Equal(t, parentRaw, cacheBefore)
	// 当次进程身份不是永久实例标识。以旧epoch观测的原审定封套重入，仍只认原永久nonce。
	// 此处是观测epoch边界注入，不会重启外部Redis，也不冒称实际AOF重启测试。
	var observedEnvelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(planBytes, &observedEnvelope))
	observedEnvelope["redis_observed_identity"] = json.RawMessage(`"` + strings.Repeat("1", 40) + ":" + strings.Repeat("2", 40) + `"`)
	observedEnvelope["mapping_observed_identity"] = json.RawMessage(`"` + strings.Repeat("3", 40) + ":" + strings.Repeat("4", 40) + `"`)
	planBytes, err = json.Marshal(observedEnvelope)
	require.NoError(t, err)
	planPath = filepath.Join(temp, "approved-prior-epoch-plan.json")
	require.NoError(t, os.WriteFile(planPath, planBytes, 0600))
	for i := range args {
		if args[i] == "-plan" {
			args[i+1] = planPath
		}
	}
	planSHA := sha256.Sum256(planBytes)
	wrongArgs := append([]string{}, args...)
	for i := range wrongArgs {
		if wrongArgs[i] == "-local-target-id" {
			wrongArgs[i+1] = strings.Repeat("0", 32)
		}
	}
	wrong := exec.CommandContext(f.ctx, binaryPath, append(wrongArgs, "-mode", "apply", "-plan-sha256", hex.EncodeToString(planSHA[:]), "-target-observation", filepath.Join(temp, "wrong-sql-target.json"))...)
	wrong.Env = append(os.Environ(), "SAVE_BARRIER_RESTORE_DSN="+parsed.FormatDSN())
	_, err = wrong.CombinedOutput()
	require.Error(t, err, "换目标实例marker不得消费旧封套")
	require.Equal(t, before, f.assets(t, id))
	unchangedParent, err := r.Redis.Get(f.ctx, keys[2]).Bytes()
	require.NoError(t, err)
	require.Equal(t, parentRaw, unchangedParent)
	t.Run("换Redis永久实例拒旧封套", func(t *testing.T) {
		require.NoError(t, r.Redis.Set(f.ctx, coreMarker+":nonce", strings.Repeat("5", 32), 0).Err())
		defer func() {
			require.NoError(t, r.Redis.Set(f.ctx, coreMarker+":nonce", parentTarget, 0).Err())
		}()
		cmd := exec.CommandContext(f.ctx, binaryPath, append(append([]string{}, args...), "-mode", "apply", "-plan-sha256", hex.EncodeToString(planSHA[:]), "-target-observation", filepath.Join(temp, "wrong-redis-target.json"))...)
		cmd.Env = append(os.Environ(), "SAVE_BARRIER_RESTORE_DSN="+parsed.FormatDSN())
		_, err := cmd.CombinedOutput()
		require.Error(t, err)
		require.Equal(t, before, f.assets(t, id))
		stillParent, err := r.Redis.Get(f.ctx, keys[2]).Bytes()
		require.NoError(t, err)
		require.Equal(t, parentRaw, stillParent)
	})
	var originalReceipt *databasepb.SaveBarrierMaintenanceReceipt
	for attempt := 0; attempt < 2; attempt++ {
		output := command("-mode", "apply", "-plan-sha256", hex.EncodeToString(planSHA[:]))
		receipt := &databasepb.SaveBarrierMaintenanceReceipt{}
		require.NoError(t, protojson.Unmarshal(bytes.TrimSpace(output), receipt))
		require.Equal(t, databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED, receipt.Phase)
		if originalReceipt == nil {
			originalReceipt = receipt
		} else {
			require.True(t, proto.Equal(originalReceipt, receipt))
		}
		require.Equal(t, before, f.assets(t, id))
		require.Equal(t, plan.RevokedFence, r.Redis.HGet(f.ctx, savebarrier.Key(id), "handoff_fence").Val())
	}
	first.FenceOwnerEpoch, first.FenceSaveSeq = 22, 1
	second.FenceOwnerEpoch, second.FenceSaveSeq = 22, 1
	f.rowsEqual(t, first, second)
	remaining, err := r.Redis.Exists(f.ctx, keys[2:6]...).Result()
	require.NoError(t, err)
	require.Zero(t, remaining, "只有生产CLI证明同内容后清过时缓存")
	t.Run("真实TiDB已提交SQL提升但确认丢失同计划恢复", func(t *testing.T) {
		lostID, one, two := f.player(t, 8, 90)
		businessBefore := f.assets(t, lostID)
		_, err := migration.ExecContext(f.ctx, "INSERT INTO player_route(route_shard,player_id,home_zone,storage_id,placement_version,rev,created_at_ms,updated_at_ms) VALUES(?,?,?,?,1,4,1000,1000)", placement.RouteRowShard(lostID), lostID, f.zone, storage)
		require.NoError(t, err)
		for key, value := range map[string]string{placement.HomeZoneKey(lostID): strconv.FormatUint(uint64(f.zone), 10), placement.Key(lostID): placement.StableValue(storage, 1), placement.MapRevKey(lostID): "4"} {
			created, err := mapping.SetNX(f.ctx, key, value, 0).Result()
			require.NoError(t, err)
			require.True(t, created)
			mapKeys = append(mapKeys, key)
		}
		lostKeys := recoveryKeys(f.zone, lostID)
		raw, err := proto.Marshal(&databasepb.PlayerAllData{PlayerDatabaseData: one, PlayerDatabase_1Data: two, LastSaveSeq: 90})
		require.NoError(t, err)
		f.ownedKeys = append(f.ownedKeys, lostKeys[2])
		require.NoError(t, r.Redis.Set(f.ctx, lostKeys[2], raw, 0).Err())
		require.NoError(t, r.Redis.HSet(f.ctx, savebarrier.Key(lostID), "pending", v, "player_database", v, "player_database_1", v, "schema_gen", "1", "handoff_fence", oldR).Err())
		p, err := r.PreparePromotion(f.ctx, f.schema, f.zone, f.maintenance, strconv.FormatUint(lostID, 10), evidenceSHA[:])
		require.NoError(t, err)
		cfg := *parsed
		cfg.DBName = f.schema
		cfg.Timeout = time.Second
		cfg.ReadTimeout = 5 * time.Second
		cfg.WriteTimeout = 5 * time.Second
		base, err := mysql.NewConnector(&cfg)
		require.NoError(t, err)
		fault := &promotionCommitReplyLoss{}
		faultDB := sql.OpenDB(promotionCommitReplyConnector{inner: authoritysql.PinCurrentSession(base, true), fault: fault})
		defer faultDB.Close()
		faultDB.SetMaxOpenConns(1)
		require.NoError(t, authoritysql.AssertCurrentSession(f.ctx, faultDB, true))
		withFault := r
		withFault.DB = faultDB
		_, err = withFault.ApplyPromotion(f.ctx, p)
		require.ErrorContains(t, err, "结局未知")
		require.True(t, fault.lost.Load(), "必须底层真实COMMIT成功后才注入一次确认丢失")
		var phase databasepb.SaveBarrierMaintenanceReceipt_Phase
		require.NoError(t, migration.QueryRowContext(f.ctx, "SELECT phase FROM save_barrier_maintenance_receipt WHERE receipt_id=?", p.ReceiptId).Scan(&phase))
		require.Equal(t, databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED, phase)
		require.Equal(t, p.RevokedFence, r.Redis.HGet(f.ctx, savebarrier.Key(lostID), "handoff_fence").Val())
		for attempt := 0; attempt < 2; attempt++ {
			receipt, err := r.ApplyPromotion(f.ctx, p)
			require.NoError(t, err)
			require.Equal(t, databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED, receipt.Phase)
			require.Equal(t, p.ReceiptId, receipt.ReceiptId)
		}
		require.Equal(t, businessBefore, f.assets(t, lostID))
		one.FenceOwnerEpoch, one.FenceSaveSeq = 22, 1
		two.FenceOwnerEpoch, two.FenceSaveSeq = 22, 1
		f.rowsEqual(t, one, two)
		var n int
		require.NoError(t, migration.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM save_barrier_maintenance_receipt WHERE receipt_id=?", p.ReceiptId).Scan(&n))
		require.Equal(t, 1, n)
	})
}

// 测试侧只截获客户端COMMIT确认：底层TiDB提交成功后才返回一次受控错误；不是网络断线或生产hook。
type promotionCommitReplyLoss struct{ lost atomic.Bool }
type promotionCommitReplyConnector struct {
	inner driver.Connector
	fault *promotionCommitReplyLoss
}

func (c promotionCommitReplyConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c promotionCommitReplyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &promotionCommitReplyConn{Conn: conn, fault: c.fault}, nil
}

type promotionCommitReplyConn struct {
	driver.Conn
	fault *promotionCommitReplyLoss
	armed bool
}

func (c *promotionCommitReplyConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *promotionCommitReplyConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	c.armed = false
	return &promotionCommitReplyTx{Tx: tx, conn: c}, nil
}
func (c *promotionCommitReplyConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(q, "save_barrier_maintenance_receipt SET phase=") && len(a) > 0 && fmt.Sprint(a[0].Value) == strconv.Itoa(int(databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED)) {
		c.armed = true
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}
func (c *promotionCommitReplyConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
}
func (c *promotionCommitReplyConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}
func (c *promotionCommitReplyConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}
func (c *promotionCommitReplyConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

type promotionCommitReplyTx struct {
	driver.Tx
	conn *promotionCommitReplyConn
}

func (tx *promotionCommitReplyTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.conn.armed && tx.conn.fault.lost.CompareAndSwap(false, true) {
		return errors.New("测试侧真实SQL_PROMOTED提交成功后丢失确认")
	}
	return err
}

// 全部门控为空时遵循默认单测口径；显式集成一旦启用，任何缺项都失败，不能退化为 MySQL 或跳过。
func TestIntegrationBarrierRestoreStrongTiDBWithRedis(t *testing.T) {
	dsn, addr, strict := os.Getenv("BARRIER_RESTORE_TEST_DSN"), os.Getenv("BARRIER_RESTORE_TEST_REDIS"), os.Getenv("BARRIER_RESTORE_REQUIRE_TIDB")
	if dsn == "" && addr == "" && strict == "" {
		t.Skip("未启用独立 BARRIER_RESTORE 真 TiDB/Redis 集成")
	}
	if dsn == "" || addr == "" || strict != "1" {
		t.Fatal("显式集成必须同时指定无库 DSN、Redis 地址和 BARRIER_RESTORE_REQUIRE_TIDB=1")
	}
	f := newRestoreIntegrationFixture(t, dsn, addr)

	t.Run("非零SQL围栏恢复和原计划重入不改资产", func(t *testing.T) {
		id, first, second := f.player(t, 8, 90)
		before := f.assets(t, id)
		p, err := f.r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, false)
		require.NoError(t, err)
		require.Equal(t, savebarrier.Version(8, 90), p.Players[0].Target)
		f.noBarrier(t, id)
		for attempt := 0; attempt < 2; attempt++ {
			require.NoError(t, f.r.ApplyOne(f.ctx, p, 0))
			f.barrier(t, id, p.Players[0].Target)
			require.Equal(t, before, f.assets(t, id))
			f.rowsEqual(t, first, second)
		}
	})

	t.Run("历史零围栏双表升级且完整实例原字节不变", func(t *testing.T) {
		id, first, second := f.player(t, 0, 0)
		before := f.assets(t, id)
		instanceBefore, err := proto.Marshal(first.BagComponent.Items[0].Instance)
		require.NoError(t, err)
		p, err := f.r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, true)
		require.NoError(t, err)
		require.Equal(t, savebarrier.Version(19, 1), p.Players[0].Target)
		first.FenceOwnerEpoch, first.FenceSaveSeq = 19, 1
		second.FenceOwnerEpoch, second.FenceSaveSeq = 19, 1
		for attempt := 0; attempt < 2; attempt++ {
			require.NoError(t, f.r.ApplyOne(f.ctx, p, 0))
			f.barrier(t, id, p.Players[0].Target)
			require.Equal(t, before, f.assets(t, id), "只可迁移围栏列，所有业务列字节必须保留")
			f.rowsEqual(t, first, second)
		}
		loaded := &databasepb.PlayerDatabase{PlayerId: id}
		require.NoError(t, f.model.WithContext(f.ctx).FindOneByPK(loaded))
		instanceAfter, err := proto.Marshal(loaded.BagComponent.Items[0].Instance)
		require.NoError(t, err)
		require.Equal(t, instanceBefore, instanceAfter, "成长、GUID 与未来 unknown 字段不得重建或丢失")
	})

	t.Run("审定计划后SQL资产变化拒绝且不升级元数据", func(t *testing.T) {
		id, first, second := f.player(t, 0, 0)
		p, err := f.r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, true)
		require.NoError(t, err)
		first.Currency.Values[1]++
		changed, err := proto.Marshal(first.Currency)
		require.NoError(t, err)
		_, err = f.r.DB.ExecContext(f.ctx, "UPDATE player_database SET currency=? WHERE player_id=?", changed, strconv.FormatUint(id, 10))
		require.NoError(t, err)
		beforeRejected := f.assets(t, id)
		err = f.r.ApplyOne(f.ctx, p, 0)
		require.ErrorContains(t, err, "SQL 资产行偏离审定计划")
		require.Equal(t, beforeRejected, f.assets(t, id))
		f.rowsEqual(t, first, second)
		f.noBarrier(t, id)
	})

	t.Run("第63桶仍有待投意图时拒绝且零围栏保持原样", func(t *testing.T) {
		id, first, second := f.player(t, 0, 0)
		p, err := f.r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, true)
		require.NoError(t, err)
		before := f.assets(t, id)
		key := fmt.Sprintf("player:save_pending:%d:63", f.zone)
		created, err := f.r.Redis.Eval(f.ctx, `if redis.call('EXISTS',KEYS[1]) ~= 0 then return 0 end
redis.call('ZADD',KEYS[1],0,ARGV[1]); return 1`, []string{key}, strconv.FormatUint(id, 10)).Int()
		require.NoError(t, err)
		require.Equal(t, 1, created, "不得借用或修改预先存在的桶")
		f.ownedKeys = append(f.ownedKeys, key)
		err = f.r.ApplyOne(f.ctx, p, 0)
		require.ErrorContains(t, err, "zone pending outbox not drained")
		_, err = f.r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{strconv.FormatUint(id, 10)}, true)
		require.ErrorContains(t, err, "zone pending outbox not drained")
		require.Equal(t, before, f.assets(t, id))
		f.rowsEqual(t, first, second)
		pending, err := f.r.Redis.ZCard(f.ctx, key).Result()
		require.NoError(t, err)
		require.EqualValues(t, 1, pending, "拒绝不能丢弃待投原件")
		f.noBarrier(t, id)
	})
}

type restoreIntegrationFixture struct {
	ctx         context.Context
	r           Restorer
	model       *proto2mysql.DB
	schema      string
	zone        uint32
	maintenance string
	ownedKeys   []string
}

func newRestoreIntegrationFixture(t *testing.T, dsn, addr string, global ...bool) *restoreIntegrationFixture {
	t.Helper()
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil || parsed.DBName != "" {
		t.Fatal("fixture DSN 必须合法且不指定数据库")
	}
	parsed.Timeout, parsed.ReadTimeout, parsed.WriteTimeout = time.Second, 5*time.Second, 5*time.Second
	admin, err := sql.Open("mysql", parsed.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	var nonce [4]byte
	_, err = rand.Read(nonce[:])
	require.NoError(t, err)
	zone := uint32(100000 + binary.BigEndian.Uint32(nonce[:])%900000)
	schema := fmt.Sprintf("zone_%d_db", zone)
	if len(global) > 0 && global[0] {
		schema = fmt.Sprintf("player_store_%d_db", placement.FirstNonZoneStorageID+zone)
	}
	// CREATE 成功才登记清理；名称冲突即失败，不触碰旧库。
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+schema+"`")
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_, err := admin.ExecContext(cleanup, "DROP DATABASE `"+schema+"`")
		require.NoError(t, err, "清理本次新建库")
		var remaining int
		require.NoError(t, admin.QueryRowContext(cleanup, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name=?", schema).Scan(&remaining))
		require.Zero(t, remaining)
		t.Logf("SQL 清理确认：%s 剩余 0", schema)
	})
	parsed.DBName = schema
	migration, err := authoritysql.OpenForMigration(ctx, parsed.FormatDSN(), schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, migration.Close()) })
	ddlModel := proto2mysql.NewDB()
	require.NoError(t, ddlModel.OpenDB(migration, schema))
	for _, row := range []proto.Message{&databasepb.PlayerDatabase{}, &databasepb.PlayerDatabase_1{}} {
		ddlModel.RegisterTable(row)
		require.NoError(t, ddlModel.WithContext(ctx).CreateOrUpdateTable(row))
	}
	for _, table := range []string{"player_database", "player_database_1"} {
		var name, ddl string
		require.NoError(t, migration.QueryRowContext(ctx, "SHOW CREATE TABLE `"+table+"`").Scan(&name, &ddl))
		t.Logf("正式夹具 DDL（%s）：%s", name, ddl)
	}
	require.NoError(t, migration.Close())
	handle, err := authoritysql.Open(ctx, parsed.FormatDSN(), schema, true, 1)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	require.NoError(t, authoritysql.AssertCurrentSession(ctx, handle, true))
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(handle, schema))
	model.RegisterTable(&databasepb.PlayerDatabase{})
	model.RegisterTable(&databasepb.PlayerDatabase_1{})
	rc := redis.NewClient(&redis.Options{Addr: addr, DB: 0, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	require.NoError(t, rc.Ping(ctx).Err())
	f := &restoreIntegrationFixture{ctx: ctx, r: Restorer{DB: handle, Redis: rc, RoutingMode: RoutingModeRedis}, model: model, schema: schema, zone: zone, maintenance: "barrier-integration-" + schema}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if len(f.ownedKeys) > 0 {
			require.NoError(t, rc.Del(cleanup, f.ownedKeys...).Err())
			remaining, err := rc.Exists(cleanup, f.ownedKeys...).Result()
			require.NoError(t, err)
			require.Zero(t, remaining)
		}
		t.Logf("Redis DB0 清理确认：本次 %d 个键剩余 0", len(f.ownedKeys))
	})
	created, err := rc.SetNX(ctx, MaintenanceKey(zone), f.maintenance, 0).Result()
	require.NoError(t, err)
	require.True(t, created, "不覆盖既有维护工单")
	f.ownedKeys = append(f.ownedKeys, MaintenanceKey(zone))
	return f
}

func (f *restoreIntegrationFixture) player(t *testing.T, epoch, seq uint64) (uint64, *databasepb.PlayerDatabase, *databasepb.PlayerDatabase_1) {
	t.Helper()
	var nonce [8]byte
	_, err := rand.Read(nonce[:])
	require.NoError(t, err)
	// 本专项用 10^12 以内随机号；保留正式自增/分片 DDL，不覆盖分配器高位边界，生产字段与参数仍为 uint64。
	id := binary.BigEndian.Uint64(nonce[:])%999_999_999_999 + 1
	t.Logf("隔离随机玩家 ID：%d（低于 10^12 的测试夹具）", id)
	for _, key := range recoveryKeys(f.zone, id)[1:8] {
		n, err := f.r.Redis.Exists(f.ctx, key).Result()
		require.NoError(t, err)
		require.Zero(t, n, "随机玩家不得借用既有 Redis 状态")
	}
	created, err := f.r.Redis.SetNX(f.ctx, ownerepoch.OwnerEpochKey(id), "19", 0).Result()
	require.NoError(t, err)
	require.True(t, created)
	f.ownedKeys = append(f.ownedKeys, ownerepoch.OwnerEpochKey(id), savebarrier.Key(id))
	storageID, ok := placement.StorageIDFromDBName(f.schema)
	require.True(t, ok)
	for _, mapping := range []struct{ key, value string }{
		{placement.HomeZoneKey(id), strconv.FormatUint(uint64(f.zone), 10)},
		{placement.Key(id), placement.StableValue(storageID, 1)},
	} {
		created, err := f.r.Redis.SetNX(f.ctx, mapping.key, mapping.value, 0).Result()
		require.NoError(t, err)
		require.True(t, created, "不覆盖既有玩家路由映射")
		f.ownedKeys = append(f.ownedKeys, mapping.key)
	}
	instance := &componentpb.ItemComp{ItemId: id ^ 37, ConfigId: 1101, Size: 1, AcquireSeq: 41,
		Equip: &componentpb.EquipInstanceData{EnhanceLevel: 17, Affixes: []*componentpb.EquipAffix{{AttrId: 3, Tier: 2, Value: 79, Seq: 0}}}}
	instance.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x07})
	first := &databasepb.PlayerDatabase{PlayerId: id, FenceOwnerEpoch: epoch, FenceSaveSeq: seq,
		Currency:     &componentpb.CurrencyComp{Values: []uint64{0, 17}},
		BagComponent: &databasepb.BagAllData{Items: []*databasepb.ItemEntry{{ItemUuid: instance.ItemId, ConfigId: instance.ConfigId, StackSize: 1, Instance: instance}}}}
	probe := &componentpb.PlayerStressTestProbe{}
	probe.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x09})
	second := &databasepb.PlayerDatabase_1{PlayerId: id, FenceOwnerEpoch: epoch, FenceSaveSeq: seq, StressTestProbe: probe}
	require.NoError(t, f.model.WithContext(f.ctx).Insert(first))
	require.NoError(t, f.model.WithContext(f.ctx).Insert(second))
	return id, first, second
}

// 独立读取全部非围栏列，保留 NULL/空字节差异；不用被测摘要算法证明自身正确。
func (f *restoreIntegrationFixture) assets(t *testing.T, id uint64) [2]map[string][]byte {
	t.Helper()
	var pair [2]map[string][]byte
	for i, table := range []string{"player_database", "player_database_1"} {
		rows, err := f.r.DB.QueryContext(f.ctx, "SELECT * FROM "+table+" WHERE player_id=?", strconv.FormatUint(id, 10))
		require.NoError(t, err)
		defer rows.Close()
		columns, err := rows.Columns()
		require.NoError(t, err)
		require.True(t, rows.Next())
		values := make([]sql.RawBytes, len(columns))
		args := make([]any, len(columns))
		for j := range values {
			args[j] = &values[j]
		}
		require.NoError(t, rows.Scan(args...))
		pair[i] = make(map[string][]byte)
		for j, name := range columns {
			if name == "fence_owner_epoch" || name == "fence_save_seq" {
				continue
			}
			if values[j] == nil {
				pair[i][name] = nil
			} else {
				pair[i][name] = append([]byte{}, values[j]...)
			}
		}
		require.False(t, rows.Next())
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return pair
}

func (f *restoreIntegrationFixture) rowsEqual(t *testing.T, first *databasepb.PlayerDatabase, second *databasepb.PlayerDatabase_1) {
	t.Helper()
	gotFirst, gotSecond := &databasepb.PlayerDatabase{PlayerId: first.PlayerId}, &databasepb.PlayerDatabase_1{PlayerId: first.PlayerId}
	require.NoError(t, f.model.WithContext(f.ctx).FindOneByPK(gotFirst))
	require.NoError(t, f.model.WithContext(f.ctx).FindOneByPK(gotSecond))
	require.True(t, proto.Equal(first, gotFirst), "SQL 资产或围栏与预期不同")
	require.True(t, proto.Equal(second, gotSecond), "第二张 SQL 行与预期不同")
}

func (f *restoreIntegrationFixture) barrier(t *testing.T, id uint64, version string) {
	t.Helper()
	fields, err := f.r.Redis.HGetAll(f.ctx, savebarrier.Key(id)).Result()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"pending": version, "player_database": version, "player_database_1": version, "schema_gen": "1", "handoff_fence": assertRestoredFence(t, fields["handoff_fence"], "19", "", "0", strings.Repeat("0", 32))}, fields)
	ttl, err := f.r.Redis.PTTL(f.ctx, savebarrier.Key(id)).Result()
	require.NoError(t, err)
	require.Equal(t, -time.Nanosecond, ttl, "提交屏障必须永久保留")
	owner, err := f.r.Redis.Get(f.ctx, ownerepoch.OwnerEpochKey(id)).Result()
	require.NoError(t, err)
	require.Equal(t, "19", owner, "工具不能重置归属计数器")
}

func (f *restoreIntegrationFixture) noBarrier(t *testing.T, id uint64) {
	t.Helper()
	n, err := f.r.Redis.Exists(f.ctx, savebarrier.Key(id)).Result()
	require.NoError(t, err)
	require.Zero(t, n, "未完成 SQL 核验不得发布屏障")
}

// 此测试消费已经完成真实promotion/正式退休/正式Cancel的隔离维护世界原件。
// 不在测试内直接UPDATE P/CANCELLED、DEL父或伪造排空。源码就绪不代表该世界已具备。
// 须由验收coordinator给当前Root短窗和全部实际producer原件；普通全Unit保持外部依赖skip。
func TestIntegrationMaintenanceReleaseCommittedReplyLossAndOriginalReplay(t *testing.T) {
	if os.Getenv("BARRIER_RESTORE_RELEASE_TEST_REQUIRED") != "1" {
		t.Skip("仅显式隔离TiDB/Redis维护恢复验收运行")
	}
	required := func(key string) string {
		v := os.Getenv(key)
		require.NotEmpty(t, v, "显式真库回归缺%s不得SKIP", key)
		return v
	}
	planPin := ReleaseFile{Path: required("BARRIER_RESTORE_RELEASE_TEST_PLAN"), SHA256: required("BARRIER_RESTORE_RELEASE_TEST_PLAN_SHA256")}
	review := ReleaseFile{Path: required("BARRIER_RESTORE_RELEASE_TEST_REVIEW"), SHA256: required("BARRIER_RESTORE_RELEASE_TEST_REVIEW_SHA256")}
	attempts := []ReleaseFile{{Path: required("BARRIER_RESTORE_RELEASE_TEST_ATTEMPT_1"), SHA256: required("BARRIER_RESTORE_RELEASE_TEST_ATTEMPT_1_SHA256")}, {Path: required("BARRIER_RESTORE_RELEASE_TEST_ATTEMPT_2"), SHA256: required("BARRIER_RESTORE_RELEASE_TEST_ATTEMPT_2_SHA256")}}
	var plan MaintenanceReleasePlan
	require.NoError(t, readReleaseJSON(planPin, &plan))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	dsn := required("BARRIER_RESTORE_RELEASE_TEST_DSN")
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	require.Equal(t, "tcp", cfg.Net)
	require.Equal(t, plan.SQLAddr, cfg.Addr)
	db, err := authoritysql.Open(ctx, dsn, plan.RouteSchema, true, 1)
	require.NoError(t, err)
	defer db.Close()
	r := Restorer{DB: db, RoutingMode: RoutingModeDBFirst, RouteSchema: plan.RouteSchema, LocalTargetID: plan.LocalTarget, MaintenanceSQLAddr: cfg.Addr}
	require.NotEmpty(t, plan.Targets)
	makeClient := func(target MaintenanceReleaseTarget) *redis.Client {
		return redis.NewClient(&redis.Options{Addr: target.Addr, DB: target.DB, Password: os.Getenv("BARRIER_RESTORE_RELEASE_TEST_REDIS_PASSWORD"), MaxRetries: -1, ContextTimeoutEnabled: true, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	}
	r.Redis = makeClient(plan.Targets[0])
	defer r.Redis.Close()
	r.MappingRedis = r.Redis
	if len(plan.Targets) == 2 {
		r.MappingRedis = makeClient(plan.Targets[1])
		defer r.MappingRedis.Close()
	}
	require.NoError(t, r.validateMaintenanceRelease(plan))
	before, err := r.releaseSQLSnapshot(ctx, plan)
	require.NoError(t, err)
	require.Equal(t, plan.SQLSHA256, before)
	// 仅客户端提交回包故障：先让真实Redis EVAL完整执行成功，再返回受控error。
	// 非真实网络切断；hook不修改生产命令、不预写receipt/guard、不伪成功。
	fault := &releaseOnceHook{after: true}
	r.Redis.AddHook(fault)
	first, err := ReadMaintenanceReleasePermit(ctx, review, attempts[0], planPin.SHA256)
	require.NoError(t, err)
	require.Error(t, r.ApplyMaintenanceRelease(ctx, plan, planPin.SHA256, first))
	require.True(t, fault.done)
	require.EqualValues(t, 1, r.Redis.Exists(ctx, maintenanceReleaseReceiptKey(plan.Zone, plan.MaintenanceID)).Val())
	second, err := ReadMaintenanceReleasePermit(ctx, review, attempts[1], planPin.SHA256)
	require.NoError(t, err)
	require.NoError(t, r.ApplyMaintenanceRelease(ctx, plan, planPin.SHA256, second))
	after, err := r.releaseSQLSnapshot(ctx, plan)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, target := range plan.Targets {
		state, err := r.releaseTarget(ctx, plan, target, planPin.SHA256, false, second.expiresMS)
		require.NoError(t, err)
		require.Equal(t, "committed", state)
	}
	changed := plan
	changed.Intent = strings.Repeat("a", 64)
	require.Error(t, r.ApplyMaintenanceRelease(ctx, changed, planPin.SHA256, second))
	// 本函数不执行route_tool unfreeze/重登；B最终还需那些独立正式叶，不能以本例冒全部B绿。
}

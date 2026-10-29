package barrierrestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"io"
	"os"
	"path/filepath"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/placement"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	databasepb "proto/common/database"
	"shared/savebarrier"
)

// 录制驱动只隔离 SQL I/O；回归调用完整 Prepare/ApplyOne 与真实 Lua，核对提交前后可观察状态。
type testStore struct {
	mu         sync.Mutex
	rows       [2][]driver.Value
	commitFail bool
	onCommit   func()
	commits    int
	columns    []string
}

// promotionDriver只隔离SQL传输，路由读仍复用原fixture；收据与双行同一模拟事务提交。
type promotionState struct {
	receipt     *databasepb.SaveBarrierMaintenanceReceipt
	loseReplyAt databasepb.SaveBarrierMaintenanceReceipt_Phase
	active      atomic.Int32
	localTarget string
}
type promotionDriver struct {
	assets *testStore
	route  *routeFixture
	state  *promotionState
}
type promotionConn struct {
	*routeConn
	state   *promotionState
	pending *databasepb.SaveBarrierMaintenanceReceipt
}
type promotionTx struct {
	driver.Tx
	c    *promotionConn
	done bool
}

func (d promotionDriver) Open(string) (driver.Conn, error) {
	return &promotionConn{routeConn: &routeConn{storeConn: &storeConn{s: d.assets}, route: d.route}, state: d.state}, nil
}
func (c *promotionConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	base, err := c.storeConn.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	c.pending = nil
	if c.state.receipt != nil {
		c.pending = proto.Clone(c.state.receipt).(*databasepb.SaveBarrierMaintenanceReceipt)
	}
	c.state.active.Add(1)
	return &promotionTx{Tx: base, c: c}, nil
}
func (c *promotionConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelReadCommitted)})
}
func (tx *promotionTx) Commit() error {
	err := tx.Tx.Commit()
	if !tx.done {
		tx.c.state.active.Add(-1)
		tx.done = true
	}
	if err != nil {
		return err
	}
	tx.c.state.receipt = tx.c.pending
	if tx.c.pending != nil && tx.c.state.loseReplyAt == tx.c.pending.Phase {
		tx.c.state.loseReplyAt = databasepb.SaveBarrierMaintenanceReceipt_PHASE_UNKNOWN
		return errors.New("SQL已提交但回包丢失")
	}
	return nil
}
func (tx *promotionTx) Rollback() error {
	if !tx.done {
		tx.c.state.active.Add(-1)
		tx.done = true
	}
	return tx.Tx.Rollback()
}
func (c *promotionConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "maintenance_recovery_target") {
		if len(args) != 3 || fmt.Sprint(args[0].Value) != "promotion" || fmt.Sprint(args[1].Value) != "17" || !strings.Contains(q, "FOR UPDATE") {
			return nil, errors.New("目标核验必须原purpose/zone/schema行锁")
		}
		schema := string(args[2].Value.([]byte))
		if schema != "player_store_1000000_db" && schema != "global_routes" {
			return nil, errors.New("未审定schema")
		}
		return &storeRows{columns: []string{"target_nonce", "physical_addr"}, values: []driver.Value{[]byte(c.state.localTarget), []byte("tidb.maintenance.example:4000")}}, nil
	}
	if !strings.Contains(q, "save_barrier_maintenance_receipt") {
		return c.routeConn.QueryContext(ctx, q, args)
	}
	r := c.pending
	if c.tx == nil {
		r = c.state.receipt
	}
	if r == nil {
		return &storeRows{columns: []string{"player_id", "request_digest", "plan_proto", "phase"}}, nil
	}
	if strings.HasPrefix(q, "SELECT updated_ms") {
		return &storeRows{columns: []string{"updated_ms"}, values: []driver.Value{int64(r.UpdatedMs)}}, nil
	}
	return &storeRows{columns: []string{"player_id", "request_digest", "plan_proto", "phase"}, values: []driver.Value{int64(r.PlayerId), r.RequestDigest, r.PlanProto, int64(r.Phase)}}, nil
}
func (c *promotionConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.tx == nil {
		return nil, errors.New("写入必须在模拟SQL事务内")
	}
	if strings.HasPrefix(q, "INSERT INTO") && strings.Contains(q, "save_barrier_maintenance_receipt") {
		if c.pending != nil {
			return nil, errors.New("收据重复")
		}
		c.pending = &databasepb.SaveBarrierMaintenanceReceipt{ReceiptId: bytes.Clone(args[0].Value.([]byte)), PlayerId: uint64(args[1].Value.(int64)), RequestDigest: bytes.Clone(args[2].Value.([]byte)), PlanProto: bytes.Clone(args[3].Value.([]byte)), Phase: databasepb.SaveBarrierMaintenanceReceipt_Phase(args[4].Value.(int64)), UpdatedMs: 1}
		return driver.RowsAffected(1), nil
	}
	if strings.Contains(q, "save_barrier_maintenance_receipt SET phase=") {
		if c.pending == nil || int64(c.pending.Phase) != args[2].Value.(int64) {
			return driver.RowsAffected(0), nil
		}
		c.pending.Phase = databasepb.SaveBarrierMaintenanceReceipt_Phase(args[0].Value.(int64))
		c.pending.UpdatedMs++
		return driver.RowsAffected(1), nil
	}
	if strings.HasPrefix(q, "UPDATE") && strings.Contains(q, "player_database") && len(args) == 5 {
		i := 0
		if strings.Contains(q, "player_database_1 ") {
			i = 1
		}
		e, qcol := -1, -1
		for n, col := range c.s.columns {
			if col == "fence_owner_epoch" {
				e = n
			}
			if col == "fence_save_seq" {
				qcol = n
			}
		}
		if e < 0 || qcol < 0 {
			return nil, errors.New("缺版本列")
		}
		if string(c.tx.pending[i][e].([]byte)) != fmt.Sprint(args[3].Value) || string(c.tx.pending[i][qcol].([]byte)) != fmt.Sprint(args[4].Value) {
			return driver.RowsAffected(0), nil
		}
		c.tx.pending[i][e] = []byte(fmt.Sprint(args[0].Value))
		c.tx.pending[i][qcol] = []byte(fmt.Sprint(args[1].Value))
		c.tx.writes = true
		return driver.RowsAffected(1), nil
	}
	return c.storeConn.ExecContext(ctx, q, args)
}

type noSQLNetworkHook struct {
	t     *testing.T
	state *promotionState
}

func (h noSQLNetworkHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h noSQLNetworkHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.state.active.Load() != 0 {
			h.t.Errorf("SQL持锁期间发出了Redis命令 %s", cmd.Name())
		}
		return next(ctx, cmd)
	}
}
func (h noSQLNetworkHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.state.active.Load() != 0 {
			h.t.Error("SQL事务内不得Redis pipeline")
		}
		return next(ctx, cmds)
	}
}

func provisionMaintenanceRedisTest(t *testing.T, r Restorer, purpose, schema string) {
	t.Helper()
	for i, c := range []*redis.Client{r.Redis, r.mapping()} {
		role := []string{"core", "mapping"}[i]
		nonce := r.RedisTargetID
		if i == 1 {
			nonce = r.MappingTargetID
		}
		if c.Options().Addr == r.Redis.Options().Addr && c.Options().DB == r.Redis.Options().DB {
			nonce = r.RedisTargetID
		}
		key, scope, err := MaintenanceRedisScope(purpose, role, 17, r.RouteSchema, schema, c.Options().Addr, c.Options().DB)
		require.NoError(t, err)
		require.NoError(t, c.Set(context.Background(), key, scope, 0).Err())
		require.NoError(t, c.Set(context.Background(), key+":nonce", nonce, 0).Err())
	}
}
func promotionTestMarkerKey(r Restorer) string {
	key, _, _ := MaintenanceRedisScope("promotion", "core", 17, r.RouteSchema, "player_store_1000000_db", r.Redis.Options().Addr, r.Redis.Options().DB)
	return key + ":nonce"
}
func TestMaintenanceTargetUsesIndependentPermanentScope(t *testing.T) {
	ctx := context.Background()
	cache := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: cache.Addr(), MaxRetries: -1})
	defer c.Close()
	require.NoError(t, c.Set(ctx, "debug_import:local_target", "local-debug-import-target", 0).Err())
	require.NoError(t, c.Set(ctx, "debug_import:local_instance_id", strings.Repeat("a", 32), 0).Err())
	_, err := ReadMaintenanceRedisMarker(ctx, c, "release", "core", 17, "global_routes", "global_routes")
	require.Error(t, err, "旧调试标记不能授权正式维护")
	key, scope, err := MaintenanceRedisScope("release", "core", 17, "global_routes", "global_routes", c.Options().Addr, 0)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, key, scope, 0).Err())
	require.NoError(t, c.Set(ctx, key+":nonce", strings.Repeat("b", 32), 0).Err())
	marker, err := ReadMaintenanceRedisMarker(ctx, c, "release", "core", 17, "global_routes", "global_routes")
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("b", 32), marker.Nonce)
	_, err = ReadMaintenanceRedisMarker(ctx, c, "release", "mapping", 17, "global_routes", "global_routes")
	require.Error(t, err)
	require.NoError(t, c.Expire(ctx, key+":nonce", time.Minute).Err())
	_, err = ReadMaintenanceRedisMarker(ctx, c, "release", "core", 17, "global_routes", "global_routes")
	require.Error(t, err)
	_, remote, err := MaintenanceRedisScope("release", "core", 17, "global_routes", "global_routes", "redis.maintenance.example:6379", 0)
	require.NoError(t, err)
	require.Contains(t, remote, "redis.maintenance.example:6379")
	require.Equal(t, "local-debug-import-target", c.Get(ctx, "debug_import:local_target").Val(), "正式维护不创建或覆盖debug授权")
}
func promotionFixture(t *testing.T) (Restorer, *testStore, *miniredis.Miniredis, *promotionState) {
	t.Helper()
	r, s, cache, _, route := splitFixture(t, true)
	state := &promotionState{localTarget: strings.Repeat("f", 32)}
	r.LocalTargetID = state.localTarget
	r.MaintenanceSQLAddr = "tidb.maintenance.example:4000"
	r.RedisTargetID = strings.Repeat("e", 32)
	r.MappingTargetID = strings.Repeat("d", 32)
	provisionMaintenanceRedisTest(t, r, "promotion", "player_store_1000000_db")
	name := fmt.Sprintf("promotion-%d", driverID.Add(1))
	sql.Register(name, promotionDriver{assets: s, route: route, state: state})
	db, err := sql.Open(name, "")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	r.DB = db
	r.Redis.AddHook(noSQLNetworkHook{t, state})
	r.mapping().AddHook(noSQLNetworkHook{t, state})
	// 两表列不同，模拟驱动允许同一个全列列表：以真实第一表为全集，第二表多出的空列仍原样保留。
	first := &databasepb.PlayerDatabase{PlayerId: 7, FenceOwnerEpoch: 8, FenceSaveSeq: 90, PersistSchemaGen: 1}
	fields := first.ProtoReflect().Descriptor().Fields()
	s.columns = nil
	s.rows = [2][]driver.Value{}
	for n := 0; n < fields.Len(); n++ {
		field := fields.Get(n)
		s.columns = append(s.columns, string(field.Name()))
		var raw []byte
		switch field.Kind() {
		case protoreflect.Uint64Kind, protoreflect.Uint32Kind:
			raw = []byte(fmt.Sprint(first.ProtoReflect().Get(field).Uint()))
		case protoreflect.MessageKind:
			raw = []byte{}
		}
		s.rows[0] = append(s.rows[0], raw)
		s.rows[1] = append(s.rows[1], bytes.Clone(raw))
	}
	// 无第二子缓存时不要求用错误模型解释模拟第二表的额外空列；真实集成另覆盖两种正式列形状。
	version := savebarrier.Version(8, 90)
	require.NoError(t, r.Redis.HSet(context.Background(), savebarrier.Key(7), "pending", version, "player_database", version, "player_database_1", version, "schema_gen", "1", "handoff_fence", "8|scene|12|"+strings.Repeat("a", 32)+"|R|"+strings.Repeat("b", 32)).Err())
	return r, s, cache, state
}

func TestPromotionStableRevocationAndUnknownCommitResume(t *testing.T) {
	for _, lost := range []databasepb.SaveBarrierMaintenanceReceipt_Phase{databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED, databasepb.SaveBarrierMaintenanceReceipt_WRITERS_REVOKED, databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED, databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED} {
		t.Run(lost.String(), func(t *testing.T) {
			r, s, _, state := promotionFixture(t)
			p, err := r.PreparePromotion(context.Background(), "player_store_1000000_db", 17, "维护工单", "7", bytes.Repeat([]byte{7}, 32))
			require.NoError(t, err)
			require.EqualValues(t, 10, p.TargetOwner)
			before := make([][]byte, len(s.rows[0]))
			for i, v := range s.rows[0] {
				before[i] = bytes.Clone(v.([]byte))
			}
			state.loseReplyAt = lost
			_, err = r.ApplyPromotion(context.Background(), p)
			require.ErrorContains(t, err, "结局未知")
			receipt, err := r.ApplyPromotion(context.Background(), p)
			require.NoError(t, err)
			require.Equal(t, databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED, receipt.Phase)
			revokedFence, err := r.Redis.HGet(context.Background(), savebarrier.Key(7), "handoff_fence").Result()
			require.NoError(t, err)
			require.Equal(t, p.RevokedFence, revokedFence)
			ttl, err := r.Redis.PTTL(context.Background(), savebarrier.Key(7)).Result()
			require.NoError(t, err)
			require.Equal(t, time.Duration(-1), ttl)
			owner, err := r.Redis.Get(context.Background(), "player:{7}:owner_epoch").Result()
			require.NoError(t, err)
			require.Equal(t, "10", owner)
			for i, name := range s.columns {
				if name == "fence_owner_epoch" || name == "fence_save_seq" {
					continue
				}
				require.Equal(t, before[i], s.rows[0][i], name)
			}
			repeated, err := r.ApplyPromotion(context.Background(), p)
			require.NoError(t, err)
			require.True(t, proto.Equal(receipt, repeated))
			changed := proto.Clone(p).(*databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan)
			changed.AuditEvidenceSha256[0] ^= 1
			_, err = r.ApplyPromotion(context.Background(), changed)
			require.Error(t, err)
		})
	}
}

// 模拟SQL收据已提交而Redis回退到原AOF前缀；重入必须重新撤销同一计划，不能只信SQL阶段。
func TestPromotionResumeRechecksRevocationAfterRedisRollback(t *testing.T) {
	for _, phase := range []databasepb.SaveBarrierMaintenanceReceipt_Phase{databasepb.SaveBarrierMaintenanceReceipt_WRITERS_REVOKED, databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED} {
		for _, damaged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/outbox=%t", phase, damaged), func(t *testing.T) {
				ctx := context.Background()
				r, store, _, state := promotionFixture(t)
				p, err := r.PreparePromotion(ctx, "player_store_1000000_db", 17, "维护工单", "7", bytes.Repeat([]byte{7}, 32))
				require.NoError(t, err)
				originalOwner, err := r.Redis.Get(ctx, "player:{7}:owner_epoch").Result()
				require.NoError(t, err)
				originalBarrier, err := r.Redis.HGetAll(ctx, savebarrier.Key(7)).Result()
				require.NoError(t, err)
				for _, key := range recoveryKeys(p.HomeZone, p.PlayerId)[2:6] {
					kind, err := r.Redis.Type(ctx, key).Result()
					require.NoError(t, err)
					require.Equal(t, "none", kind, "本例原缓存为空；只回退真实owner/资格/ACK前缀")
				}
				business := [2][][]byte{}
				for n := range store.rows {
					for _, value := range store.rows[n] {
						business[n] = append(business[n], bytes.Clone(value.([]byte)))
					}
				}
				state.loseReplyAt = phase
				_, err = r.ApplyPromotion(ctx, p)
				require.ErrorContains(t, err, "结局未知")
				require.Equal(t, phase, state.receipt.Phase)
				originalReceiptID := bytes.Clone(state.receipt.ReceiptId)
				originalPlan := bytes.Clone(state.receipt.PlanProto)
				// 仅测试故障注入回退Redis原前缀；SQL与marker均不回退，不清待投伪造完成。
				require.NoError(t, r.Redis.Set(ctx, "player:{7}:owner_epoch", originalOwner, 0).Err())
				require.NoError(t, r.Redis.Del(ctx, savebarrier.Key(7)).Err())
				require.NoError(t, r.Redis.HSet(ctx, savebarrier.Key(7), originalBarrier).Err())
				if damaged {
					require.NoError(t, r.Redis.HSet(ctx, savebarrier.Key(7), "outbox_payload", "must-not-clear").Err())
				}
				receipt, err := r.ApplyPromotion(ctx, p)
				if damaged {
					require.Error(t, err)
					require.Equal(t, phase, state.receipt.Phase)
					owner, readErr := r.Redis.Get(ctx, "player:{7}:owner_epoch").Result()
					require.NoError(t, readErr)
					require.Equal(t, originalOwner, owner)
					fence, readErr := r.Redis.HGet(ctx, savebarrier.Key(7), "handoff_fence").Result()
					require.NoError(t, readErr)
					require.Equal(t, p.OriginalFence, fence)
					outbox, readErr := r.Redis.HGet(ctx, savebarrier.Key(7), "outbox_payload").Result()
					require.NoError(t, readErr)
					require.Equal(t, "must-not-clear", outbox)
				} else {
					require.NoError(t, err)
					require.Equal(t, databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED, receipt.Phase)
					require.Equal(t, originalReceiptID, receipt.ReceiptId)
					require.Equal(t, originalPlan, receipt.PlanProto)
					fence, readErr := r.Redis.HGet(ctx, savebarrier.Key(7), "handoff_fence").Result()
					require.NoError(t, readErr)
					require.Equal(t, p.RevokedFence, fence)
					ttl, readErr := r.Redis.PTTL(ctx, savebarrier.Key(7)).Result()
					require.NoError(t, readErr)
					require.Equal(t, time.Duration(-1), ttl)
					owner, readErr := r.Redis.Get(ctx, "player:{7}:owner_epoch").Result()
					require.NoError(t, readErr)
					require.Equal(t, strconv.FormatUint(p.TargetOwner, 10), owner)
					repeated, readErr := r.ApplyPromotion(ctx, p)
					require.NoError(t, readErr)
					require.True(t, proto.Equal(receipt, repeated))
				}
				for n := range store.rows {
					for i, column := range store.columns {
						if column != "fence_owner_epoch" && column != "fence_save_seq" {
							require.Equal(t, business[n][i], store.rows[n][i], column)
						}
					}
				}
			})
		}
	}
}

func TestPromotionRejectsUndrainedOrDamagedStateWithoutMutation(t *testing.T) {
	for _, kind := range []string{"outbox", "pending-ahead", "owner-overflow", "cache-unproven", "wrongtype", "target-mismatch", "redis-target-mismatch", "redis-marker-missing", "redis-marker-expiring"} {
		t.Run(kind, func(t *testing.T) {
			r, s, cache, state := promotionFixture(t)
			switch kind {
			case "outbox":
				require.NoError(t, r.Redis.HSet(context.Background(), savebarrier.Key(7), "outbox_payload", "latest-original").Err())
			case "pending-ahead":
				require.NoError(t, r.Redis.HSet(context.Background(), savebarrier.Key(7), "pending", savebarrier.Version(9, 91)).Err())
			case "owner-overflow":
				r.AuditedOwnerUpperBound = "18446744073709551615"
			case "cache-unproven":
				require.NoError(t, r.Redis.Set(context.Background(), recoveryKeys(17, 7)[2], "not-a-proto", 0).Err())
			case "wrongtype":
				require.NoError(t, r.Redis.Set(context.Background(), savebarrier.Key(7), "corrupt", 0).Err())
			case "target-mismatch":
				r.LocalTargetID = strings.Repeat("e", 32)
			case "redis-target-mismatch":
				r.RedisTargetID = strings.Repeat("d", 32)
			case "redis-marker-missing":
				require.NoError(t, r.Redis.Del(context.Background(), promotionTestMarkerKey(r)).Err())
			case "redis-marker-expiring":
				require.NoError(t, r.Redis.Expire(context.Background(), promotionTestMarkerKey(r), time.Minute).Err())
			}
			before := cache.Dump()
			_, err := r.PreparePromotion(context.Background(), "player_store_1000000_db", 17, "维护工单", "7", bytes.Repeat([]byte{1}, 32))
			require.Error(t, err)
			require.Equal(t, before, cache.Dump())
			require.Nil(t, state.receipt)
			require.Zero(t, s.commits)
		})
	}
}

func TestPromotionRevocationRejectsOriginalCppB5AndLateRegister(t *testing.T) {
	r, _, cache, _ := promotionFixture(t)
	ctx := context.Background()
	p, err := r.PreparePromotion(ctx, "player_store_1000000_db", 17, "维护工单", "7", bytes.Repeat([]byte{4}, 32))
	require.NoError(t, err)
	_, err = r.ApplyPromotion(ctx, p)
	require.NoError(t, err)
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "cpp", "libs", "services", "scene", "player", "system", "handoff_sql_barrier.h"))
	require.NoError(t, err)
	extract := func(name string) string {
		start := strings.Index(string(source), " "+name+" =")
		require.GreaterOrEqual(t, start, 0)
		tail := string(source[start:])
		begin := strings.Index(tail, `R"(`)
		require.GreaterOrEqual(t, begin, 0)
		tail = tail[begin+3:]
		end := strings.Index(tail, `)"`)
		require.GreaterOrEqual(t, end, 0)
		return tail[:end]
	}
	helpers := extract("kFenceLuaHelpers")
	keys := []string{"player:{7}:owner_epoch", savebarrier.Key(7), "player:7:handoff", "player:7:location"}
	before := cache.Dump()
	oldR := p.OriginalFence
	oldH := "10|scene|12|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("c", 32)
	// 即使旧B5预期E+2恰好等于新owner，也不能拿旧R复活；该反例正钉nonce永久撤销。
	n, err := r.Redis.Eval(ctx, helpers+extract("kResumeFenceLua"), keys, oldR, oldH, "10", "").Int()
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = r.Redis.Eval(ctx, helpers+extract("kRegisterFenceLua"), keys[:3], "8", "8|scene|13|"+strings.Repeat("a", 32)+"|H|"+strings.Repeat("d", 32), p.RevokedFence).Int()
	require.NoError(t, err)
	require.Less(t, n, 0)
	require.Equal(t, before, cache.Dump())
}

type storeDriver struct{ s *testStore }
type storeConn struct {
	s  *testStore
	tx *storeTx
}
type storeTx struct {
	c       *storeConn
	pending [2][]driver.Value
	writes  bool
}
type storeRows struct {
	values  []driver.Value
	columns []string
	done    bool
}

var driverID atomic.Uint64

func (d storeDriver) Open(string) (driver.Conn, error) { return &storeConn{s: d.s}, nil }
func (c *storeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("不使用预备语句")
}
func (c *storeConn) Close() error { return nil }
func (c *storeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *storeConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	if o.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
		return nil, errors.New("必须使用 RC")
	}
	c.s.mu.Lock()
	tx := &storeTx{c: c}
	for i := range tx.pending {
		tx.pending[i] = append([]driver.Value(nil), c.s.rows[i]...)
	}
	c.s.mu.Unlock()
	c.tx = tx
	return tx, nil
}
func (c *storeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.tx == nil || !strings.HasSuffix(q, " WHERE player_id=? FOR UPDATE") {
		return nil, errors.New("必须按完整主键点锁")
	}
	i := 0
	if strings.Contains(q, "player_database_1 ") {
		i = 1
	}
	return &storeRows{values: append([]driver.Value(nil), c.tx.pending[i]...), columns: c.s.columns}, nil
}
func (c *storeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if c.tx == nil || !strings.Contains(q, "WHERE player_id=? AND fence_owner_epoch=0 AND fence_save_seq=0") {
		return nil, errors.New("必须沿锁定行 CAS")
	}
	i := 0
	if strings.Contains(q, "player_database_1 ") {
		i = 1
	}
	c.tx.pending[i][1] = []byte(fmt.Sprint(args[0].Value))
	c.tx.pending[i][2] = []byte(fmt.Sprint(args[1].Value))
	c.tx.writes = true
	return driver.RowsAffected(1), nil
}
func (tx *storeTx) Commit() error {
	tx.c.s.mu.Lock()
	defer tx.c.s.mu.Unlock()
	if tx.c.s.commitFail {
		return errors.New("合成提交失败")
	}
	if tx.writes {
		tx.c.s.rows = tx.pending
		tx.c.s.commits++
		if tx.c.s.onCommit != nil {
			tx.c.s.onCommit()
		}
	}
	tx.c.tx = nil
	return nil
}
func (tx *storeTx) Rollback() error { tx.c.tx = nil; return nil }
func (r *storeRows) Columns() []string {
	if r.columns != nil {
		return r.columns
	}
	return []string{"player_id", "fence_owner_epoch", "fence_save_seq", "payload", "persist_schema_gen"}
}
func (r *storeRows) Close() error { return nil }
func (r *storeRows) Next(v []driver.Value) error {
	if r.done || len(r.values) == 0 {
		return io.EOF
	}
	copy(v, r.values)
	r.done = true
	return nil
}

func fixture(t *testing.T, epoch, seq string) (Restorer, *testStore, *miniredis.Miniredis) {
	t.Helper()
	// 只描述正常迁移后列默认0的录制结果，不冒称真实SQL迁移已执行。
	s := &testStore{rows: [2][]driver.Value{{[]byte("7"), []byte(epoch), []byte(seq), []byte("真实资产甲"), []byte("0")}, {[]byte("7"), []byte(epoch), []byte(seq), []byte("真实资产乙"), []byte("0")}}}
	name := fmt.Sprintf("barrier-restore-%d", driverID.Add(1))
	sql.Register(name, storeDriver{s})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: m.Addr(), MaxRetries: -1})
	t.Cleanup(func() { rc.Close() })
	m.Set(MaintenanceKey(17), "维护工单")
	m.Set("player:{7}:owner_epoch", "9")
	m.Set("player:zone:7", "17")
	m.Set("player:placement:7", "17:1")
	return Restorer{DB: db, Redis: rc, RoutingMode: RoutingModeRedis}, s, m
}
func prepare(t *testing.T, r Restorer, legacy bool) Plan {
	t.Helper()
	p, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, legacy)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestoreNonzeroAndReplay(t *testing.T) {
	r, s, m := fixture(t, "8", "90")
	p := prepare(t, r, false)
	if len(m.Keys()) != 4 {
		t.Fatal("plan 写了 Redis")
	}
	for i := 0; i < 2; i++ {
		if err := r.ApplyOne(context.Background(), p, 0); err != nil {
			t.Fatal(err)
		}
	}
	v := savebarrier.Version(8, 90)
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		if got := m.HGet("player:{7}:save_barrier", field); got != v {
			t.Fatalf("%s=%s", field, got)
		}
	}
	if s.commits != 0 {
		t.Fatal("非零恢复不应修改 SQL")
	}
}

func TestLegacyMigrationCommitsBeforePublicationAndRechecks(t *testing.T) {
	r, s, m := fixture(t, "0", "0")
	p := prepare(t, r, true)
	s.onCommit = func() {
		if m.Exists("player:{7}:save_barrier") {
			t.Error("SQL 提交前已发布屏障")
		}
	}
	if err := r.ApplyOne(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if s.commits != 1 || string(s.rows[0][3].([]byte)) != "真实资产甲" || string(s.rows[1][3].([]byte)) != "真实资产乙" {
		t.Fatal("迁移改了资产或未原子提交")
	}
	if got := m.HGet("player:{7}:save_barrier", "pending"); got != savebarrier.Version(9, 1) {
		t.Fatal(got)
	}
	if err := r.ApplyOne(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if s.commits != 1 {
		t.Fatal("重投再次修改 SQL")
	}
}

func TestLegacyCommitFailureNeverPublishes(t *testing.T) {
	r, s, m := fixture(t, "0", "0")
	p := prepare(t, r, true)
	s.commitFail = true
	if err := r.ApplyOne(context.Background(), p, 0); err == nil {
		t.Fatal("预期提交失败")
	}
	if m.Exists("player:{7}:save_barrier") {
		t.Fatal("失败提交冒充成功 ACK")
	}
	if string(s.rows[0][1].([]byte)) != "0" || string(s.rows[1][1].([]byte)) != "0" {
		t.Fatal("失败事务部分写入")
	}
}

func TestLegacyInterruptedAfterCommitCanResume(t *testing.T) {
	r, s, m := fixture(t, "0", "0")
	p := prepare(t, r, true)
	s.onCommit = func() { m.Set(MaintenanceKey(17), "其他维护") }
	if err := r.ApplyOne(context.Background(), p, 0); err == nil {
		t.Fatal("维护号变更应拒绝")
	}
	if s.commits != 1 || m.Exists("player:{7}:save_barrier") {
		t.Fatal("SQL 提交与屏障阶段未隔离")
	}
	m.Set(MaintenanceKey(17), "维护工单")
	s.onCommit = nil
	if err := r.ApplyOne(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if s.commits != 1 {
		t.Fatal("同计划续跑重复迁移")
	}
}

func TestLegacyRechecksSQLAfterCommit(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*testStore)
	}{
		{"资产变化", func(s *testStore) { s.rows[0][3] = []byte("提交后发生了其他写") }},
		{"行删除", func(s *testStore) { s.rows[1] = nil }},
		{"围栏变化", func(s *testStore) { s.rows[0][2] = []byte("2"); s.rows[1][2] = []byte("2") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			r, s, m := fixture(t, "0", "0")
			p := prepare(t, r, true)
			s.onCommit = func() { change.run(s) }
			if err := r.ApplyOne(context.Background(), p, 0); err == nil {
				t.Fatal("重锁没有识别 SQL 改变")
			}
			if m.Exists("player:{7}:save_barrier") {
				t.Fatal("给变化后的 SQL 发布了旧证据")
			}
		})
	}
}

func TestPlanBindsHomeZoneAndRedisTarget(t *testing.T) {
	r, _, _ := fixture(t, "8", "90")
	p := prepare(t, r, false)
	if _, err := r.Prepare(context.Background(), "zone_17_db", 18, "维护工单", []string{"7"}, false); err == nil {
		t.Fatal("错区预检未拒绝")
	}
	if _, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, false); err == nil {
		t.Fatal("无权威落点证明却恢复异区库")
	}
	p.RedisDB = 1
	if err := r.ApplyOne(context.Background(), p, 0); err == nil {
		t.Fatal("Redis DB 偏离计划未拒绝")
	}
	p.RedisDB = 0
	p.LocalMySQL = true
	if err := r.ApplyOne(context.Background(), p, 0); err == nil {
		t.Fatal("SQL 强读模式偏离计划未拒绝")
	}
	p.LocalMySQL = false
	p.RedisAddr = "127.0.0.1:1"
	if err := r.ApplyOne(context.Background(), p, 0); err == nil {
		t.Fatal("Redis 地址偏离计划未拒绝")
	}
}

func TestLegacyRefusesPrematureBarrier(t *testing.T) {
	r, _, m := fixture(t, "0", "0")
	v := savebarrier.Version(9, 1)
	m.HSet("player:{7}:save_barrier", "pending", v, "player_database", v, "player_database_1", v)
	if _, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, true); err == nil {
		t.Fatal("零 SQL 围栏不能接受提前发布的 ACK")
	}
}

func TestFailClosedDriftAndRedisConflicts(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testStore, *miniredis.Miniredis, []string)
	}{
		{"资产改变", func(s *testStore, _ *miniredis.Miniredis, _ []string) { s.rows[0][3] = []byte("别的资产") }},
		{"两表不同版", func(s *testStore, _ *miniredis.Miniredis, _ []string) { s.rows[1][2] = []byte("91") }},
		{"行缺失", func(s *testStore, _ *miniredis.Miniredis, _ []string) { s.rows[1] = nil }},
		{"归属改变", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[1], "10") }},
		{"归属丢失", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Del(k[1]) }},
		{"维护号改变", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[0], "新维护") }},
		{"维护号有过期", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.SetTTL(k[0], time.Minute) }},
		{"归属有过期", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.SetTTL(k[1], time.Minute) }},
		{"未提交意图", func(_ *testStore, m *miniredis.Miniredis, k []string) {
			m.HSet(k[7], "outbox_version", savebarrier.Version(8, 90))
		}},
		{"屏障版本冲突", func(_ *testStore, m *miniredis.Miniredis, k []string) {
			m.HSet(k[7], "pending", savebarrier.Version(8, 91))
		}},
		{"末桶未排空", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.ZAdd(k[71], 0, "其他玩家") }},
		{"桶错型", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[8], "错型") }},
		{"父键存在", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[2], "旧父快照") }},
		{"子缓存存在", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[4], "旧子表") }},
		{"旧资产证据", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[5], "旧证据") }},
		{"交接仍在", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[6], "8:1") }},
		{"home区变化", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[72], "18") }},
		{"落点变化", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[73], "1000000:2") }},
		{"映射缺失", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Del(k[73]) }},
		{"落点冻结", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[73], "17:1:frozen:run1") }},
		{"合服在途", func(_ *testStore, m *miniredis.Miniredis, k []string) { m.Set(k[74], "维护中") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s, m := fixture(t, "8", "90")
			p := prepare(t, r, false)
			tc.change(s, m, recoveryKeys(17, 7))
			before := m.Dump()
			if err := r.ApplyOne(context.Background(), p, 0); err == nil {
				t.Fatal("冲突未拒绝")
			}
			if m.Dump() != before || s.commits != 0 {
				t.Fatal("拒绝留下副作用")
			}
		})
	}
}

func TestPlanValidationBeforeIO(t *testing.T) {
	r, s, m := fixture(t, "0", "0")
	if _, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, false); err == nil {
		t.Fatal("未授权零围栏迁移")
	}
	for _, ids := range [][]string{{"7", "7"}, {"07"}, {"18446744073709551616"}, {"0"}} {
		if _, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", ids, true); err == nil {
			t.Fatal(ids)
		}
	}
	if _, err := r.Prepare(context.Background(), "zone_17_db;DROP TABLE player_database", 17, "维护工单", []string{"7"}, true); err == nil {
		t.Fatal("危险 schema 未拒绝")
	}
	if s.commits != 0 || len(m.Keys()) != 4 {
		t.Fatal("预检改了状态")
	}
}

// 保留全部四持久字段，另验永久撤销 floor；返回原文供完整 map 精确比较。
func assertRestoredFence(t *testing.T, raw, owner, instance, generation, admission string) string {
	t.Helper()
	parts := strings.Split(raw, "|")
	require.Len(t, parts, 6)
	require.Equal(t, []string{owner, instance, generation, admission, "R"}, parts[:5])
	require.Len(t, parts[5], 32)
	require.NotEqual(t, strings.Repeat("0", 32), parts[5])
	for _, ch := range parts[5] {
		require.True(t, ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f')
	}
	return raw
}

func TestRestoreRevokesOriginalHandoffFloorAndRotatesEveryTime(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint("known=", known), func(t *testing.T) {
			r, _, cache := fixture(t, "8", "90")
			plan := prepare(t, r, false)
			admission := strings.Repeat("a", 32)
			if known {
				v := savebarrier.Version(8, 90)
				cache.HSet(savebarrier.Key(7), "pending", v, "player_database", v, "player_database_1", v,
					"handoff_fence", "8|scene|12|"+admission+"|H|"+strings.Repeat("b", 32))
			}
			last := ""
			for i := 0; i < 2; i++ {
				require.NoError(t, r.ApplyOne(context.Background(), plan, 0))
				raw := cache.HGet(savebarrier.Key(7), "handoff_fence")
				if known {
					assertRestoredFence(t, raw, "8", "scene", "12", admission)
				} else {
					assertRestoredFence(t, raw, "9", "", "0", strings.Repeat("0", 32))
				}
				require.NotEqual(t, last, raw)
				last = raw
				require.Equal(t, time.Duration(0), cache.TTL(savebarrier.Key(7)), "永久下限不随标记TTL失效")
			}
		})
	}
}

func TestRestoreRejectsDamagedHandoffFloorWithoutAnyMutation(t *testing.T) {
	for _, raw := range []string{"", "8|scene|1|bad|H|bad", "10|scene|1|" + strings.Repeat("a", 32) + "|H|" + strings.Repeat("b", 32)} {
		t.Run(raw, func(t *testing.T) {
			r, store, cache := fixture(t, "8", "90")
			p := prepare(t, r, false)
			v := savebarrier.Version(8, 90)
			cache.HSet(savebarrier.Key(7), "pending", v, "player_database", v, "player_database_1", v, "handoff_fence", raw)
			before := cache.Dump()
			require.Error(t, r.ApplyOne(context.Background(), p, 0))
			require.Equal(t, before, cache.Dump())
			require.Zero(t, store.commits)
		})
	}
}

// 以下原件/SQL驱动仅为确定接口回归替身，不冒本机生产调用或恢复actual。
type releaseReadDriver struct {
	records map[string][]releaseSQLRecord
	active  *atomic.Int32
}
type releaseReadConn struct {
	records map[string][]releaseSQLRecord
	active  *atomic.Int32
}
type releaseReadTx struct {
	active *atomic.Int32
	done   bool
}
type releaseReadRows struct {
	columns []string
	records []releaseSQLRecord
	index   int
}

func (d releaseReadDriver) Open(string) (driver.Conn, error) {
	return &releaseReadConn{d.records, d.active}, nil
}
func (c *releaseReadConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("只读fixture不允许Prepare写入")
}
func (c *releaseReadConn) Close() error { return nil }
func (c *releaseReadConn) Begin() (driver.Tx, error) {
	return nil, errors.New("必须显式只读一致事务")
}
func (c *releaseReadConn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	if o.ReadOnly || o.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
		return nil, errors.New("解除SQL必须RR并允许锁专用目标行；最终只Rollback")
	}
	c.active.Add(1)
	return &releaseReadTx{active: c.active}, nil
}
func (t *releaseReadTx) Commit() error { return errors.New("解除不提交SQL事务") }
func (t *releaseReadTx) Rollback() error {
	if !t.done {
		t.active.Add(-1)
		t.done = true
	}
	return nil
}
func (c *releaseReadConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.active.Load() != 1 || !strings.HasPrefix(q, "SELECT ") || strings.Contains(q, "FOR UPDATE") && !strings.Contains(q, "maintenance_recovery_target") {
		return nil, errors.New("fixture只允许事务内一致只读")
	}
	if strings.Contains(q, "maintenance_recovery_target") {
		if len(args) != 3 || fmt.Sprint(args[0].Value) != "release" || fmt.Sprint(args[1].Value) != "17" || !strings.Contains(q, "FOR UPDATE") {
			return nil, errors.New("维护目标原purpose/zone/schema锁缺失")
		}
		records := c.records["maintenance_recovery_target"]
		if strings.HasPrefix(q, "SELECT target_nonce,physical_addr") {
			if len(records) != 1 {
				return &releaseReadRows{columns: []string{"target_nonce", "physical_addr"}}, nil
			}
			return &releaseReadRows{columns: []string{"target_nonce", "physical_addr"}, records: []releaseSQLRecord{releaseTestRow("target_nonce", records[0].value("target_nonce"), "physical_addr", records[0].value("physical_addr"))}}, nil
		}
	}
	table := ""
	for name := range c.records {
		if strings.Contains(q, "`."+name+" ") {
			table = name
			break
		}
	}
	records, ok := c.records[table]
	if !ok {
		return nil, fmt.Errorf("未声明只读表: %s", q)
	}
	columns := []string{}
	if len(records) > 0 {
		for _, c := range records[0].Cells {
			columns = append(columns, c.Name)
		}
	} else {
		columns = []string{"empty_fixture"}
	}
	return &releaseReadRows{columns: columns, records: records}, nil
}
func (r *releaseReadRows) Columns() []string { return r.columns }
func (r *releaseReadRows) Close() error      { return nil }
func (r *releaseReadRows) Next(dest []driver.Value) error {
	if r.index >= len(r.records) {
		return io.EOF
	}
	for i, c := range r.records[r.index].Cells {
		if c.Null {
			dest[i] = nil
		} else {
			dest[i] = bytes.Clone(c.Bytes)
		}
	}
	r.index++
	return nil
}
func releaseTestRow(values ...any) releaseSQLRecord {
	r := releaseSQLRecord{}
	for i := 0; i < len(values); i += 2 {
		c := releaseSQLCell{Name: values[i].(string), Null: values[i+1] == nil}
		if !c.Null {
			switch v := values[i+1].(type) {
			case []byte:
				c.Bytes = bytes.Clone(v)
			default:
				c.Bytes = []byte(fmt.Sprint(v))
			}
		}
		r.Cells = append(r.Cells, c)
	}
	return r
}
func releaseTestFile(t *testing.T, name string, v any) ReleaseFile {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	h := sha256.Sum256(raw)
	return ReleaseFile{Path: path, SHA256: hex.EncodeToString(h[:])}
}
func releaseTestCancel(t *testing.T, source *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan) (ReleaseFile, *databasepb.AccountRoleAdmissionRecovery) {
	t.Helper()
	seal := &base.RoleTradeSeal{ListingId: 11, PlayerId: 7, OwnerRevision: 1, SceneInstanceId: "11111111-1111-4111-8111-111111111111", ThroughGeneration: 12, SceneNodeId: "9", ZoneId: 17}
	begin := &dspb.AccountRoleTradeReceipt{ListingId: 11, PlayerId: 7, SellerAccount: "owned-release-fixture", SellerOwnerRevision: 1, State: dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_FREEZING, Holders: []*dspb.AccountRoleTradeHolder{{Seal: seal}}}
	reply := proto.Clone(begin).(*dspb.AccountRoleTradeReceipt)
	reply.State = dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED
	reply.Holders[0].Drained = true
	writeProto := func(name string, msg proto.Message) ReleaseFile {
		raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, raw, 0600))
		h := sha256.Sum256(raw)
		return ReleaseFile{Path: path, SHA256: hex.EncodeToString(h[:])}
	}
	unsigned, err := proto.Marshal(&dspb.CancelAccountRoleTradeRequest{ListingId: 11, PlayerId: 7, SellerAccount: begin.SellerAccount, ExpectedOwnerRevision: 1})
	require.NoError(t, err)
	sum := sha256.Sum256(unsigned)
	action := releaseTestFile(t, "cancel-action.json", map[string]any{"schema": "maintenance-release-trade-cancel-v1", "action": "cancel", "unsigned_request_proto": unsigned, "unsigned_request_sha256": hex.EncodeToString(sum[:]), "original_begin_receipt": writeProto("begin.json", begin), "reply": writeProto("reply.json", reply), "post_state": releaseTestFile(t, "state.json", map[string]any{"synthetic_unit_fixture": true}), "original_observation": releaseTestFile(t, "original.json", map[string]any{"synthetic_unit_fixture": true})})
	_, digest, err := PromotionDigest(source)
	require.NoError(t, err)
	request := &databasepb.AccountRoleAdmissionRecovery_Request{RequestId: bytes.Repeat([]byte{3}, 32), SellerAccount: begin.SellerAccount, Seal: seal, Admission: &base.RoleAdmissionIdentity{PlayerId: 7, SceneInstanceId: seal.SceneInstanceId, Generation: 12, OwnerRevision: 1, OwnerEpoch: 8, SceneNodeId: "9", SceneId: 19, ZoneId: 17, SessionId: 1, LoginGeneration: 1}, AdmissionRequestDigest: bytes.Repeat([]byte{4}, 32), ExpectedClosedThrough: 0, ExpectedRequiredOwnerEpoch: 8, ExpectedRequiredSaveSeq: 90, MaintenanceReceiptId: source.ReceiptId, MaintenanceRequestDigest: digest}
	raw, err := proto.Marshal(request)
	require.NoError(t, err)
	h := sha256.New()
	writePromotionPart(h, []byte("account-role-admission-recovery-v1"))
	writePromotionPart(h, raw)
	recovery := &databasepb.AccountRoleAdmissionRecovery{RequestId: request.RequestId, ListingId: 11, PlayerId: 7, SceneInstanceId: seal.SceneInstanceId, ThroughGeneration: 12, OwnerRevision: 1, RequestDigest: h.Sum(nil), RequestProto: raw, MaintenanceReceiptId: source.ReceiptId, MaintenanceRequestDigest: digest, RequiredOwnerEpoch: source.TargetOwner, RequiredSaveSeq: source.TargetSaveSeq, SqlProofDigest: bytes.Repeat([]byte{5}, 32), RecoveredMs: 1}
	return action, recovery
}
func releaseTestFixture(t *testing.T, same bool) (Restorer, MaintenanceReleasePlan, *MaintenanceReleasePermit, map[string][]releaseSQLRecord, *miniredis.Miniredis, *miniredis.Miniredis) {
	t.Helper()
	ctx := context.Background()
	r, assets, cache, state := promotionFixture(t)
	if same {
		// 同物理夹具必须从正式 promotion 前就用 DB0；不放宽 mapping 固定 DB0 契约。
		// 仅本 miniredis 替身装配，复制全部原键类型/值/TTL，不重构 R 或人工提升 SQL。
		for _, key := range cache.DB(r.Redis.Options().DB).Keys() {
			copied, err := r.Redis.Copy(ctx, key, key, 0, true).Result()
			require.NoError(t, err)
			require.EqualValues(t, 1, copied)
		}
		core := redis.NewClient(&redis.Options{Addr: r.Redis.Options().Addr, DB: 0, MaxRetries: -1})
		t.Cleanup(func() { core.Close() })
		core.AddHook(noSQLNetworkHook{t, state})
		r.Redis = core
		provisionMaintenanceRedisTest(t, r, "promotion", "player_store_1000000_db")
	}
	source, err := r.PreparePromotion(ctx, "player_store_1000000_db", 17, "维护工单", "7", bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	receipt, err := r.ApplyPromotion(ctx, source)
	require.NoError(t, err)
	originalMapping := r.mapping()
	mappingAddr := originalMapping.Options().Addr
	mappingCache := miniredis.RunT(t)
	if same {
		r.MappingRedis = r.Redis
	} else {
		r.MappingRedis = redis.NewClient(&redis.Options{Addr: mappingCache.Addr(), DB: 0, MaxRetries: -1})
		t.Cleanup(func() { r.MappingRedis.Close() })
	}
	provisionMaintenanceRedisTest(t, r, "release", r.RouteSchema)
	target := promotionRouteTargetForReleaseTest(r, source)
	require.NoError(t, r.Guard(ctx, target))
	source.MappingAddr = r.mapping().Options().Addr
	source.MappingDb = int32(r.mapping().Options().DB)
	raw, err := proto.Marshal(source)
	require.NoError(t, err)
	_, digest, err := PromotionDigest(source)
	require.NoError(t, err)
	receipt.PlanProto = raw
	receipt.RequestDigest = digest
	_ = mappingAddr
	cancel, recovery := releaseTestCancel(t, source)
	raw, err = proto.Marshal(source)
	require.NoError(t, err)
	recoveryRaw, err := proto.Marshal(recovery)
	require.NoError(t, err)
	member := MaintenanceReleaseMember{PlayerID: 7, RowSchema: source.RowSchema, PromotionProto: raw, RecoveryProtos: [][]byte{recoveryRaw}, CancelAction: cancel, FirstBusinessSHA256: hex.EncodeToString(source.FirstBusinessSha256), SecondBusinessSHA256: hex.EncodeToString(source.SecondBusinessSha256), Owner: source.TargetOwner, SaveSeq: source.TargetSaveSeq, Fence: source.RevokedFence}
	records := map[string][]releaseSQLRecord{}
	records["maintenance_recovery_target"] = []releaseSQLRecord{releaseTestRow("purpose", "release", "zone_id", 17, "schema_name", r.RouteSchema, "target_nonce", r.LocalTargetID, "physical_addr", r.MaintenanceSQLAddr)}
	records["route_meta"] = []releaseSQLRecord{releaseTestRow("id", 1, "generation", source.RouteGeneration, "writable", 0)}
	records["route_reconcile"] = []releaseSQLRecord{releaseTestRow("id", 1, "watermark_ms", source.RouteWatermark, "cache_verified_ms", 1000)}
	records["zone_merged_into"] = []releaseSQLRecord{}
	records["zone_merge_fence"] = []releaseSQLRecord{}
	records["player_route"] = []releaseSQLRecord{releaseTestRow("route_shard", 7, "player_id", 7, "home_zone", 17, "storage_id", source.StorageId, "placement_version", source.PlacementVersion, "frozen_run_id", nil, "rev", source.RouteRevision)}
	records["save_barrier_maintenance_receipt"] = []releaseSQLRecord{releaseTestRow("receipt_id", receipt.ReceiptId, "player_id", receipt.PlayerId, "request_digest", receipt.RequestDigest, "plan_proto", raw, "phase", int(receipt.Phase), "updated_ms", 1)}
	records["account_role_admission_recovery"] = []releaseSQLRecord{releaseTestRow("request_id", recovery.RequestId, "listing_id", 11, "player_id", 7, "scene_instance_id", recovery.SceneInstanceId, "through_generation", 12, "owner_revision", 1, "request_digest", recovery.RequestDigest, "request_proto", recovery.RequestProto, "maintenance_receipt_id", recovery.MaintenanceReceiptId, "maintenance_request_digest", recovery.MaintenanceRequestDigest, "required_owner_epoch", recovery.RequiredOwnerEpoch, "required_save_seq", recovery.RequiredSaveSeq, "sql_proof_digest", recovery.SqlProofDigest, "recovered_ms", 1)}
	records["account_role"] = []releaseSQLRecord{releaseTestRow("player_id", 7, "account", "owned-release-fixture", "role_proto", []byte{0, 1, 255}, "owner_revision", 1, "trade_listing_id", 0)}
	records["account_role_trade"] = []releaseSQLRecord{releaseTestRow("listing_id", 11, "player_id", 7, "seller_account", "owned-release-fixture", "seller_owner_revision", 1, "state", int(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED))}
	records["account_role_trade_holder"] = []releaseSQLRecord{releaseTestRow("listing_id", 11, "scene_instance_id", recovery.SceneInstanceId, "drained", 1, "ack_digest", recovery.RequestDigest)}
	records["account_role_admission"] = []releaseSQLRecord{releaseTestRow("player_id", 7, "scene_instance_id", recovery.SceneInstanceId, "highest_generation", 12, "closed_through", 12)}
	for i, name := range []string{"player_database", "player_database_1"} {
		row := releaseSQLRecord{}
		for j, col := range assets.columns {
			v := assets.rows[i][j]
			cell := releaseSQLCell{Name: col, Null: v == nil}
			if v != nil {
				cell.Bytes = bytes.Clone(v.([]byte))
			}
			row.Cells = append(row.Cells, cell)
		}
		records[name] = []releaseSQLRecord{row}
	}
	active := new(atomic.Int32)
	name := fmt.Sprintf("release-read-%d", driverID.Add(1))
	sql.Register(name, releaseReadDriver{records: records, active: active})
	db, err := sql.Open(name, "")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	r.DB = db
	review := releaseTestFile(t, "review-pin.json", map[string]any{"synthetic": "unit_store_boundary_only"})
	input := MaintenanceReleaseInput{Format: "maintenance-release-member-input-v1", Zone: 17, MaintenanceID: source.MaintenanceId, Members: []MaintenanceReleaseMember{member}}
	// 本helper仅隔离已由strict-reader单独测的外部permit适配；公开Prepare/Apply仍读全部SQL与执行真实Lua。
	targets, err := r.releaseTargets(ctx, 17, source.MaintenanceId)
	require.NoError(t, err)
	expected := []maintenanceReleaseReviewedRedisTarget{}
	for _, target := range targets {
		expected = append(expected, maintenanceReleaseReviewedRedisTarget{Role: target.Role, Addr: target.Addr, DB: target.DB, InstanceNonce: target.InstanceNonce, Zones: []uint32{17}})
	}
	permit := &MaintenanceReleasePermit{review: review, members: input, targetID: r.LocalTargetID, sqlAddr: r.MaintenanceSQLAddr, routeSchema: r.RouteSchema, deadline: time.Now().Add(50 * time.Second), expiresMS: time.Now().Add(50 * time.Second).UnixMilli(), redisTargets: expected}
	plan, err := r.PrepareMaintenanceRelease(ctx, 17, source.MaintenanceId, input.Members, permit)
	require.NoError(t, err)
	return r, plan, permit, records, cache, mappingCache
}
func promotionRouteTargetForReleaseTest(r Restorer, p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan) Plan {
	return Plan{Format: FormatV2, Schema: p.RowSchema, Zone: p.HomeZone, MaintenanceID: p.MaintenanceId, RedisAddr: r.Redis.Options().Addr, RedisDB: r.Redis.Options().DB, MappingAddr: r.mapping().Options().Addr, MappingDB: r.mapping().Options().DB, RoutingMode: RoutingModeDBFirst, RouteSchema: r.RouteSchema}
}

type releaseOnceHook struct {
	after bool
	done  bool
}

func (h *releaseOnceHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *releaseOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *releaseOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		apply := false
		if cmd.Name() == "eval" && len(args) > 3 {
			n, _ := strconv.Atoi(fmt.Sprint(args[2]))
			apply = len(args) > 3+n && fmt.Sprint(args[3+n]) == "apply"
		}
		if !apply || h.done {
			return next(ctx, cmd)
		}
		h.done = true
		if h.after {
			if err := next(ctx, cmd); err != nil {
				return err
			}
		}
		return errors.New("受控单次传输失败；after=true表示Lua已真实提交")
	}
}
func TestMaintenanceReleasePublicApplyCommittedPrefixAndLostReply(t *testing.T) {
	for _, kind := range []string{"same-physical", "core-committed-mapping-not-sent", "mapping-committed-reply-lost"} {
		t.Run(kind, func(t *testing.T) {
			r, p, permit, _, _, _ := releaseTestFixture(t, kind == "same-physical")
			ctx := context.Background()
			body, _ := json.MarshalIndent(p, "", "  ")
			sum := sha256.Sum256(append(body, '\n'))
			sha := hex.EncodeToString(sum[:])
			permit.planSHA = sha
			if kind != "same-physical" {
				r.mapping().AddHook(&releaseOnceHook{after: kind == "mapping-committed-reply-lost"})
				require.Error(t, r.ApplyMaintenanceRelease(ctx, p, sha, permit))
				require.False(t, r.Redis.Exists(ctx, MaintenanceKey(p.Zone)).Val() == 1)
				if kind == "core-committed-mapping-not-sent" {
					require.EqualValues(t, 1, r.mapping().Exists(ctx, MaintenanceKey(p.Zone)).Val())
				}
			}
			require.NoError(t, r.ApplyMaintenanceRelease(ctx, p, sha, permit))
			require.NoError(t, r.ApplyMaintenanceRelease(ctx, p, sha, permit))
			require.EqualValues(t, 0, r.mapping().Exists(ctx, MaintenanceKey(p.Zone), placement.MergeFenceKey(p.Zone)).Val())
			require.Equal(t, p.Members[0].Fence, r.Redis.HGet(ctx, savebarrier.Key(7), "handoff_fence").Val())
			require.Equal(t, savebarrier.Version(p.Members[0].Owner, p.Members[0].SaveSeq), r.Redis.HGet(ctx, savebarrier.Key(7), "pending").Val())
		})
	}
}
func TestMaintenanceReleaseRejectsDamagedTargetsWithoutPartialWrite(t *testing.T) {
	for _, kind := range []string{"nil-core", "nonce-ttl", "guard-ttl", "receipt-wrongtype", "receipt-plus-guard", "missing-one-guard", "pending-ahead", "outbox", "sql-unfrozen", "SQL-business-changed", "zone-member-added", "new-maintenance", "expired-window", "changed-plan", "route-generation", "route-watermark", "route-revision", "placement-version", "placement-NULL-pair", "duplicate-recovery", "missing-mapping-marker", "wrong-marker-scope", "wrong-physical-SQL", "SQL-marker-nonce"} {
		t.Run(kind, func(t *testing.T) {
			r, p, permit, records, _, _ := releaseTestFixture(t, true)
			ctx := context.Background()
			core := r.Redis
			body, _ := json.MarshalIndent(p, "", "  ")
			sum := sha256.Sum256(append(body, '\n'))
			sha := hex.EncodeToString(sum[:])
			permit.planSHA = sha
			switch kind {
			case "nil-core":
				r.Redis = nil
			case "nonce-ttl":
				require.NoError(t, r.Redis.Expire(ctx, p.Targets[0].Markers[0].Key+":nonce", time.Minute).Err())
			case "missing-mapping-marker":
				require.NoError(t, r.Redis.Del(ctx, p.Targets[0].Markers[1].Key).Err())
			case "wrong-marker-scope":
				require.NoError(t, r.Redis.Set(ctx, p.Targets[0].Markers[0].Key, "other-scope", 0).Err())
			case "wrong-physical-SQL":
				r.MaintenanceSQLAddr = "other.maintenance.example:4000"
			case "SQL-marker-nonce":
				records["maintenance_recovery_target"][0].Cells[3].Bytes = []byte(strings.Repeat("a", 32))
			case "guard-ttl":
				require.NoError(t, r.Redis.Expire(ctx, MaintenanceKey(p.Zone), time.Minute).Err())
			case "receipt-wrongtype":
				require.NoError(t, r.Redis.HSet(ctx, maintenanceReleaseReceiptKey(p.Zone, p.MaintenanceID), "bad", "type").Err())
			case "receipt-plus-guard":
				require.NoError(t, r.Redis.Set(ctx, maintenanceReleaseReceiptKey(p.Zone, p.MaintenanceID), "other", 0).Err())
			case "missing-one-guard":
				require.NoError(t, r.Redis.Del(ctx, placement.MergeFenceKey(p.Zone)).Err())
			case "pending-ahead":
				require.NoError(t, r.Redis.HSet(ctx, savebarrier.Key(7), "pending", savebarrier.Version(p.Members[0].Owner, 2)).Err())
			case "outbox":
				require.NoError(t, r.Redis.HSet(ctx, savebarrier.Key(7), "outbox_future_unknown", "keep-original").Err())
			case "sql-unfrozen":
				records["route_meta"][0] = releaseTestRow("id", 1, "generation", strings.Repeat("a", 32), "writable", 1)
			case "SQL-business-changed":
				records["player_database"][0].Cells[0].Bytes = []byte("other")
			case "zone-member-added":
				records["player_route"] = append(records["player_route"], records["player_route"][0])
			case "new-maintenance":
				p.MaintenanceID += "-new"
			case "expired-window":
				permit.deadline = time.Now().Add(-time.Second)
			case "changed-plan":
				sha = strings.Repeat("a", 64)
			case "route-generation":
				records["route_meta"][0].Cells[1].Bytes = []byte(strings.Repeat("c", 32))
			case "route-watermark":
				records["route_reconcile"][0].Cells[1].Bytes = []byte("1001")
			case "route-revision":
				records["player_route"][0].Cells[6].Bytes = []byte("999")
			case "placement-version":
				records["player_route"][0].Cells[4].Bytes = []byte("999")
			case "placement-NULL-pair":
				records["player_route"][0].Cells[3].Null = true
				records["player_route"][0].Cells[3].Bytes = nil
			case "duplicate-recovery":
				p.Members[0].RecoveryProtos = append(p.Members[0].RecoveryProtos, p.Members[0].RecoveryProtos[0])
				records["account_role_admission_recovery"] = append(records["account_role_admission_recovery"], records["account_role_admission_recovery"][0])
			}
			if kind == "duplicate-recovery" {
				permit.members.Members = p.Members
				body, _ = json.MarshalIndent(p, "", "  ")
				sum = sha256.Sum256(append(body, '\n'))
				sha = hex.EncodeToString(sum[:])
				permit.planSHA = sha
			}
			before, err := core.Dump(ctx, MaintenanceKey(p.Zone)).Result()
			require.NoError(t, err)
			require.NotPanics(t, func() { require.Error(t, r.ApplyMaintenanceRelease(ctx, p, sha, permit)) })
			after, err := core.Dump(ctx, MaintenanceKey(p.Zone)).Result()
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, p.Members[0].Fence, core.HGet(ctx, savebarrier.Key(7), "handoff_fence").Val())
		})
	}
}
func TestMaintenanceReleaseOriginalReaderRejectsFabricatedPassed(t *testing.T) {
	pin := releaseTestFile(t, "fake.json", map[string]any{"schema": "maintenance-release-reviewed-producers-v1", "run_id": "synthetic", "members_input": ReleaseFile{}, "producers": []any{}, "stable_facts": []any{}, "passed": true})
	_, err := ReadMaintenanceReleasePermit(context.Background(), pin, pin, "")
	require.Error(t, err)
	_, err = ReadMaintenanceReleaseMembers(pin)
	require.Error(t, err)
	wrong := pin
	wrong.SHA256 = strings.Repeat("a", 64)
	_, err = readReleaseBound(wrong, 1024)
	require.Error(t, err)
}

func TestMaintenanceReleasePermitBindsFreshCompleteAudits(t *testing.T) {
	for _, kind := range []string{"complete", "missing-target", "missing-partition", "stale-attempt", "failed-build", "source-diff", "issuer-omitted", "issuer-same-count-swap", "missing-raw-evidence", "revival-window-short", "wrong-target", "wrong-route", "pending-bucket-missing", "barrier-unconverged", "outbox", "same-count-key-swap"} {
		t.Run(kind, func(t *testing.T) {
			// 本例全部为显式synthetic运维原件；不冒真实停止、SQL或Redis验收。
			_, plan, _, _, _, _ := releaseTestFixture(t, true)
			issued := time.Now().Add(-3 * time.Second).UnixMilli()
			expires := issued + 40000
			run := "synthetic-release-operation"
			evidence := releaseTestFile(t, "raw-observation.json", map[string]any{"synthetic": "raw-operation-observation"})
			issuer := maintenanceReleaseIssuer{ID: "sm/1", Role: "scene-manager", Instance: "original-instance"}
			inv := releaseTestFile(t, "issuers.json", maintenanceReleaseIssuerInventory{Format: "maintenance-release-issuer-inventory-v1", RunID: run, TargetID: plan.LocalTarget, Zone: plan.Zone, RouteSchema: plan.RouteSchema, Issuers: []maintenanceReleaseIssuer{issuer}})
			rawEvidence := []ReleaseFile{evidence}
			if kind == "missing-raw-evidence" {
				rawEvidence = nil
			}
			retirementIssuer := issuer
			if kind == "issuer-same-count-swap" {
				retirementIssuer.Instance = "other-original"
			}
			retired := releaseTestFile(t, "retired.json", maintenanceReleaseRetirement{Format: "maintenance-release-issuer-retirement-v1", RunID: run, Issuer: retirementIssuer, PublicationClosedMS: issued - 200, StoppedMS: issued - 100, RevivalFenceID: "operator-maintenance-fence", Evidence: rawEvidence})
			retirements := []ReleaseFile{retired}
			if kind == "issuer-omitted" {
				retirements = nil
			}
			cutoff := releaseTestFile(t, "cutoff.json", map[string]any{"schema": "maintenance-release-issuer-retirements-v1", "operation_id": run, "issuer_inventory": inv, "retirements": retirements})
			inventory := releaseTestFile(t, "kafka-inventory.json", map[string]any{"schema": "maintenance-release-kafka-inventory-v1", "topics": []any{map[string]any{"name": "db-fixture", "partitions": 1}}, "subscriptions": []any{map[string]any{"group": "db-group", "topic": "db-fixture"}}, "internal_topics": []string{}})
			target := maintenanceReleaseReviewedRedisTarget{Role: plan.Targets[0].Role, Addr: plan.Targets[0].Addr, DB: plan.Targets[0].DB, InstanceNonce: plan.Targets[0].InstanceNonce, Zones: []uint32{plan.Zone}}
			epoch := strings.Repeat("a", 40)
			definition := releaseTestFile(t, "redis-definition.json", map[string]any{"schema": "maintenance-release-redis-inventory-v1", "targets": []any{map[string]any{"role": target.Role, "addr": target.Addr, "db": target.DB, "zones": target.Zones, "expected_run_id": epoch}}, "topics": []any{map[string]any{"name": "db-fixture", "partitions": 1}}})
			obs := []maintenanceReleaseRedisKeyObservation{}
			names := []string{}
			empty := sha256.Sum256(nil)
			add := func(key, kind, topic string) {
				h := sha256.Sum256([]byte(key))
				names = append(names, key)
				obs = append(obs, maintenanceReleaseRedisKeyObservation{Key: key, KeySHA: hex.EncodeToString(h[:]), Kind: kind, Topic: topic, Type: "none", DumpSHA: hex.EncodeToString(empty[:]), PTTL: -2, Count: 0})
			}
			for b := 0; b < 64; b++ {
				if kind == "pending-bucket-missing" && b == 63 {
					continue
				}
				add(fmt.Sprintf("player:save_pending:%d:%d", plan.Zone, b), "pending", "")
			}
			for _, p := range []struct{ prefix, kind string }{{"kafka:retry:queue:", "retry"}, {"kafka:dead:queue:", "dead"}, {"kafka:retry:processing:", "processing"}, {"kafka:retry:instances:", "instances"}, {"kafka:retry:instance_mode:", "modes"}} {
				add(p.prefix+"db-fixture", p.kind, "db-fixture")
			}
			add("kafka:retry:queue:db-fixture:p0", "retry", "db-fixture")
			if kind == "barrier-unconverged" || kind == "outbox" {
				add(savebarrier.Key(7), "barrier", "")
				i := len(obs) - 1
				obs[i].Type = "hash"
				obs[i].PTTL = -1
				obs[i].Count = 3
				obs[i].Members = []string{"pending", "player_database", "player_database_1"}
				v := savebarrier.Version(22, 1)
				obs[i].Values = []string{v, v, v}
				if kind == "barrier-unconverged" {
					obs[i].Values[0] = savebarrier.Version(23, 1)
				} else {
					obs[i].Count = 4
					obs[i].Members = append(obs[i].Members, "outbox_payload")
					obs[i].Values = append(obs[i].Values, "still-pending")
				}
			}
			if kind == "same-count-key-swap" {
				obs[0].Key = "player:save_pending:999:0"
				h := sha256.Sum256([]byte(obs[0].Key))
				obs[0].KeySHA = hex.EncodeToString(h[:])
				names[0] = obs[0].Key
			}
			stable := append([]maintenanceReleaseRedisKeyObservation{}, obs...)
			for i := range stable {
				stable[i].PTTL = 0
			}
			raw, _ := json.Marshal(stable)
			digest := sha256.Sum256(raw)
			targets := []any{map[string]any{"role": target.Role, "addr": target.Addr, "db": target.DB, "run_id": epoch, "key_count": len(obs), "key_names": names, "pending_buckets": 64, "blockers": []string{}, "observations": obs, "observations_sha256": hex.EncodeToString(digest[:])}}
			if kind == "missing-target" {
				targets = nil
			}
			start := issued + 100
			if kind == "stale-attempt" {
				start = issued - 100
			}
			queues := releaseTestFile(t, "redis.json", map[string]any{"schema": "maintenance-release-redis-queues-v1", "operation_id": run, "observed_start_ms": start, "observed_end_ms": issued + 200, "source_evidence": []ReleaseFile{evidence}, "definition": definition, "targets": targets})
			rows := []any{map[string]any{"group": "db-group", "topic": "db-fixture", "partition": 0, "earliest": 0, "committed": 2, "end": 2, "lag": 0}}
			if kind == "missing-partition" {
				rows = nil
			}
			kafka := releaseTestFile(t, "kafka.json", map[string]any{"schema": "maintenance-release-kafka-queues-v1", "operation_id": run, "observed_start_ms": issued + 100, "observed_end_ms": issued + 200, "source_evidence": []ReleaseFile{evidence}, "inventory": inventory, "topic_names": []string{"db-fixture"}, "group_names": []string{"db-group"}, "partition_rows": rows})
			input := releaseTestFile(t, "members.json", MaintenanceReleaseInput{Format: "maintenance-release-member-input-v1", Zone: plan.Zone, MaintenanceID: plan.MaintenanceID, Members: plan.Members})
			reviewBody := maintenanceReleaseReview{Format: "maintenance-release-operator-review-v1", RunID: run, TargetID: plan.LocalTarget, SQLAddr: plan.SQLAddr, RouteSchema: plan.RouteSchema, MembersInput: input, IssuerInventory: inv, StableFacts: []maintenanceReleaseFact{{Role: "issuer-cutoff", Actual: cutoff}, {Role: "trade-cancel/7", Actual: plan.Members[0].CancelAction}}, KafkaInventory: inventory, RedisTargets: []maintenanceReleaseReviewedRedisTarget{target}}
			if kind == "wrong-target" {
				reviewBody.TargetID = strings.Repeat("a", 32)
			}
			if kind == "wrong-route" {
				reviewBody.RouteSchema = "other_routes"
			}
			var review ReleaseFile
			if kind == "failed-build" || kind == "source-diff" {
				raw, _ := json.Marshal(reviewBody)
				var v map[string]any
				require.NoError(t, json.Unmarshal(raw, &v))
				v["producers"] = []any{map[string]any{"build_proof": evidence, "source": evidence}}
				review = releaseTestFile(t, "review-private-fields.json", v)
			} else {
				review = releaseTestFile(t, "review.json", reviewBody)
			}
			until := expires
			if kind == "revival-window-short" {
				until--
			}
			window := releaseTestFile(t, "window.json", maintenanceReleaseWindow{Format: "maintenance-release-window-v1", RunID: run, Review: review, IssuedMS: issued, ExpiresMS: expires, Issuers: []maintenanceReleaseIssuerWindow{{Issuer: issuer, Retirement: retired, ObservedMS: issued + 100, RevivalBlockedUntilMS: until, RevivalFenceID: "operator-maintenance-fence", Evidence: []ReleaseFile{evidence}}}})
			attempt := releaseTestFile(t, "attempt.json", maintenanceReleaseAttempt{Format: "maintenance-release-attempt-v1", Review: review, RunID: run, Window: window, Facts: []maintenanceReleaseFact{{Role: "redis-queues", Actual: queues}, {Role: "kafka-queues", Actual: kafka}}})
			permit, err := ReadMaintenanceReleasePermit(context.Background(), review, attempt, "")
			if kind == "complete" {
				require.NoError(t, err)
				require.NotNil(t, permit)
			} else {
				require.Error(t, err)
				require.Nil(t, permit)
			}
		})
	}
}

func roleReplace(role string) string { return strings.ReplaceAll(role, "/", "-") }

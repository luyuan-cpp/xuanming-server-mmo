package routetool

// freeze / unfreeze(第 1b-1 批)与就绪标记脚本的执行语义:miniredis + 假库,不连库。
// 解冻事务本身(先抬水位再翻 writable、锁序)由 store 包的契约用例与真库用例钉住;这里只钉
// 「标记怎么判、什么时候拒绝、交给库的最低水位是多少、退出码是几」。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"strconv"
	"strings"
	"testing"
	"time"

	"data_service/internal/store"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 编译期断言:装配层直接把这两样传进来。
var (
	_ readyMarkerRedis = (*redis.Redis)(nil)
	_ freezeDB         = (*store.RouteStore)(nil)
	_ unfreezeDB       = (*store.RouteStore)(nil)
	_ MaintenanceWork  = (*drainproof.Session)(nil)
)

const testGeneration = "0123456789abcdef0123456789abcdef"

// testIdentity 是「当前」mapping Redis 的身份(Identity 由用例注入,miniredis 的 INFO 不支持 server / replication 段)。
var testIdentity = RedisIdentity{RunID: strings.Repeat("ab", 20), MasterReplID: strings.Repeat("cd", 20)}

// fakeUnfreezeDB 是 unfreezeDB / freezeDB 的内存替身。
type fakeUnfreezeDB struct {
	meta          store.RouteMeta
	unfreezeCalls []uint64
	freezeCalls   int
	freezeChanged bool
	freezeErr     error
	deadline      bool // 解冻 / 冻结调用时 ctx 带不带截止时间
}

func (f *fakeUnfreezeDB) ReadRouteMeta(context.Context) (store.RouteMeta, error) { return f.meta, nil }

func (f *fakeUnfreezeDB) UnfreezeRouteWriters(ctx context.Context, minWatermarkMs uint64) (bool, error) {
	_, f.deadline = ctx.Deadline()
	f.unfreezeCalls = append(f.unfreezeCalls, minWatermarkMs)
	f.meta.Writable = 1
	return true, nil
}

func (f *fakeUnfreezeDB) FreezeRouteWriters(ctx context.Context) (bool, error) {
	_, f.deadline = ctx.Deadline()
	f.freezeCalls++
	return f.freezeChanged, f.freezeErr
}

func frozenUnfreezeDB() *fakeUnfreezeDB {
	return &fakeUnfreezeDB{meta: store.RouteMeta{Generation: testGeneration, Writable: 0}}
}

// readyMarkerFor 编出一份身份等于 id、generation 等于 gen 的合规标记。
func readyMarkerFor(t *testing.T, id RedisIdentity, gen string, watermarkMs uint64) string {
	t.Helper()
	raw, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{
		Generation:   gen,
		RedisRunID:   id.RunID,
		MasterReplID: id.MasterReplID,
		SetAtMs:      watermarkMs + 1,
		WatermarkMs:  watermarkMs,
		Leader:       "route_leader@it:0a1b2c3d",
	})
	require.NoError(t, err)
	return raw
}

// 以下只隔离工具的SQL控制调用，不证明实际Catalog授权或数据库锁；SQL验收另用真实owned fixture。
type unitMaintenanceCatalog struct {
	frozen   *pb.SqlWriterMaintenanceReceipt
	meta     func(context.Context) (store.RouteMeta, error)
	freezer  freezeDB
	releaser unfreezeDB
}

func unitReceipt(t *testing.T, kind pb.SqlWriterMaintenanceReceipt_Kind, floor uint64) *pb.SqlWriterMaintenanceReceipt {
	t.Helper()
	run := bytes.Repeat([]byte{1}, 16)
	plan, err := proto.MarshalOptions{Deterministic: true}.Marshal(&pb.SqlWriterMaintenanceReceipt_Plan{RunId: run, BeforeEpoch: 1, CatalogRevision: 1})
	require.NoError(t, err)
	manifest := sha256.Sum256(plan)
	request := &pb.SqlWriterMaintenanceReceipt{RequestId: bytes.Repeat([]byte{byte(kind) + 2}, 16), RunId: run, Generation: testGeneration, BeforeEpoch: 1, CatalogRevision: 1, Kind: kind, AttemptRevision: 1, PlanProto: plan, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: run, ManifestDigest: manifest[:]}}
	if kind == pb.SqlWriterMaintenanceReceipt_RELEASE {
		request.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{ReleaseReadiness: &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{OriginalFreezeReceiptId: bytes.Repeat([]byte{3}, 16), OriginalFreezeReceiptDigest: bytes.Repeat([]byte{4}, 32), RouteWatermarkFloorMs: floor}}
	}
	sealed, err := authoritysql.SealMaintenanceRequest(request)
	require.NoError(t, err)
	return sealed
}
func unitFrozenReceipt(t *testing.T) *pb.SqlWriterMaintenanceReceipt {
	r := unitReceipt(t, pb.SqlWriterMaintenanceReceipt_FREEZE, 0)
	r.AfterEpoch = 2
	r.Phase = pb.SqlWriterMaintenanceReceipt_FROZEN
	r.RunPhase = pb.SqlWriterDrainRun_SQL_FROZEN
	r, err := authoritysql.SealMaintenanceRequest(r)
	require.NoError(t, err)
	r.CommittedMs = 1
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	r.ReceiptDigest = sum[:]
	return r
}
func (c *unitMaintenanceCatalog) Freeze(ctx context.Context, ticket []byte, r *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if len(ticket) != 16 || r == nil || r.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE {
		return nil, errors.New("单测typed冻结输入不符")
	}
	if _, err := c.freezer.FreezeRouteWriters(ctx); err != nil {
		return nil, err
	}
	return proto.Clone(r).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (c *unitMaintenanceCatalog) Release(ctx context.Context, ticket []byte, r *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	if len(ticket) != 16 || r == nil || r.Kind != pb.SqlWriterMaintenanceReceipt_RELEASE || r.GetEvidence().GetReleaseReadiness() == nil {
		return nil, errors.New("单测typed释放输入不符")
	}
	if _, err := c.releaser.UnfreezeRouteWriters(ctx, r.GetEvidence().GetReleaseReadiness().RouteWatermarkFloorMs); err != nil {
		return nil, err
	}
	return proto.Clone(r).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (c *unitMaintenanceCatalog) MaintenanceReceipt(_ context.Context, id []byte) (*pb.SqlWriterMaintenanceReceipt, error) {
	if c.frozen == nil || !bytes.Equal(id, c.frozen.RequestId) {
		return nil, errors.New("单测冻结原件ID不同")
	}
	return proto.Clone(c.frozen).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (c *unitMaintenanceCatalog) CurrentMeta(ctx context.Context) (*pb.RouteMeta, error) {
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.RouteMeta{Generation: m.Generation, Writable: uint32(m.Writable), WriterEpoch: c.frozen.AfterEpoch, CatalogRevision: c.frozen.CatalogRevision, FreezeRunId: bytes.Clone(c.frozen.RunId)}, nil
}

// 准备固定P8输入，使用测试当前安排的原始Redis值；调用后不会随被测流程重封或重取输入。
func unitReleaseRequest(t *testing.T, rd readyMarkerRedis, db unfreezeDB) *pb.SqlWriterMaintenanceReceipt {
	t.Helper()
	var floor uint64
	m, err := db.ReadRouteMeta(context.Background())
	if err == nil && rd != nil && m.Frozen() {
		if raw, err := rd.GetCtx(context.Background(), placement.RouteReadyKey); err == nil {
			if marker, err := placement.DecodeRouteReadyMarker(raw); err == nil && marker.RedisRunID == testIdentity.RunID && marker.MasterReplID == testIdentity.MasterReplID {
				floor = marker.WatermarkMs
			}
		}
		if raw, err := rd.GetCtx(context.Background(), placement.RouteWatermarkKey); err == nil {
			if key, err := placement.DecodeRouteWatermark(raw); err == nil {
				floor = max(floor, key.FloorFor(m.Generation))
			}
		}
	}
	return unitReceipt(t, pb.SqlWriterMaintenanceReceipt_RELEASE, floor)
}

func runUnfreeze(t *testing.T, rd readyMarkerRedis, db unfreezeDB, ack bool) (UnfreezeReport, cliRun) {
	t.Helper()
	var out, errOut bytes.Buffer
	rep, err := Unfreeze(context.Background(), rd, db, UnfreezeOptions{
		Catalog: &unitMaintenanceCatalog{releaser: db}, TicketID: bytes.Repeat([]byte{7}, 16), Request: unitReleaseRequest(t, rd, db),
		CacheUnverifiedAcknowledged: ack,
		Timeout:                     store.RouteFreezeTimeoutMin,
		Identity:                    func(context.Context) (RedisIdentity, error) { return testIdentity, nil },
		Stdout:                      &out,
		Stderr:                      &errOut,
	})
	return rep, cliRun{code: UnfreezeExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
}

// TestUnfreeze_VerifiedMarkerUnfreezesWithItsWatermark:标记可解码、身份等于当前实例 → 解冻,最低水位取标记的 watermark_ms;
// 标记原样保留;解冻调用带截止时间。
func TestUnfreeze_VerifiedMarkerUnfreezesWithItsWatermark(t *testing.T) {
	mr, rd := newMappingRedis(t)
	raw := readyMarkerFor(t, testIdentity, testGeneration, 1_760_000_000_000)
	require.NoError(t, mr.Set(placement.RouteReadyKey, raw))
	db := frozenUnfreezeDB()

	rep, run := runUnfreeze(t, rd, db, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.Equal(t, []uint64{1_760_000_000_000}, db.unfreezeCalls)
	assert.True(t, db.deadline, "解冻事务必须带时限(store.RouteFreezeTimeoutMin)")
	assert.Equal(t, MarkerVerified, rep.Marker)
	assert.False(t, rep.GenerationRealigned)
	got, err := mr.Get(placement.RouteReadyKey)
	require.NoError(t, err)
	assert.Equal(t, raw, got, "可信标记不动")
	assert.Equal(t, "route_unfreeze changed=true already_writable=false marker=verified generation_realigned=false min_watermark_ms=1760000000000\n", run.stdout)
}

// TestUnfreeze_GenerationRealignedBeforeUnfreeze:可信标记的 generation 与库不等 → 先 compare-and-set 成库的 generation 再解冻;
// CAS 输了(读到之后被别人改过)→ 退出码 1、不解冻。
func TestUnfreeze_GenerationRealignedBeforeUnfreeze(t *testing.T) {
	mr, rd := newMappingRedis(t)
	require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, "ffff", 42)))
	db := frozenUnfreezeDB()

	rep, run := runUnfreeze(t, rd, db, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.True(t, rep.GenerationRealigned)
	got, err := mr.Get(placement.RouteReadyKey)
	require.NoError(t, err)
	m, err := placement.DecodeRouteReadyMarker(got)
	require.NoError(t, err)
	assert.Equal(t, testGeneration, m.Generation, "标记的 generation 对齐成库的")
	assert.Equal(t, uint64(42), m.WatermarkMs, "其余字段不变")
	assert.Equal(t, []uint64{42}, db.unfreezeCalls)

	t.Run("CAS输了_退出码1_不解冻", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, "ffff", 42)))
		db := frozenUnfreezeDB()
		raced := casRacingRedis{readyMarkerRedis: rd, mr: mr}

		_, run := runUnfreeze(t, raced, db, false)

		assert.Equal(t, ExitAttention, run.code, run.stderr)
		assert.Empty(t, db.unfreezeCalls)
		assert.Empty(t, run.stdout)
	})
}

// casRacingRedis 在每次 EVAL 之前把标记换成另一份合规值,模拟「读到之后被别人改过」。
type casRacingRedis struct {
	readyMarkerRedis
	mr *miniredis.Miniredis
}

func (c casRacingRedis) EvalCtx(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if err := c.mr.Set(placement.RouteReadyKey, `{"v":1,"changed":"by-someone-else"}`); err != nil {
		return nil, err
	}
	return c.readyMarkerRedis.EvalCtx(ctx, script, keys, args...)
}

// TestUnfreeze_RefusesWithoutVerifiedMarker:确认不了缓存(标记缺席 / 畸形 / 版本更新 / 不是 string / 身份不符)一律退出码 2、不解冻;
// 畸形的按原值撤掉,版本更新的与不是 string 的不动,身份不符的(没加确认时)不动。
func TestUnfreeze_RefusesWithoutVerifiedMarker(t *testing.T) {
	other := RedisIdentity{RunID: strings.Repeat("01", 20), MasterReplID: testIdentity.MasterReplID}
	for _, tc := range []struct {
		name      string
		preset    func(t *testing.T, mr *miniredis.Miniredis)
		wantState string
		wantKept  bool // 标记键是否还在
	}{
		{"缺席", func(*testing.T, *miniredis.Miniredis) {}, MarkerAbsent, false},
		{"畸形_撤掉", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set(placement.RouteReadyKey, "ready"))
		}, MarkerMalformedDropped, false},
		{"版本更新_不动", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set(placement.RouteReadyKey, `{"v":2,"whatever":true}`))
		}, MarkerNewerVersion, true},
		// 2026-10-18 修复评审:新版本改了已知字段的类型也只认「版本更新」,不能当畸形删掉。
		{"版本更新且字段改了类型_不动", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set(placement.RouteReadyKey, `{"v":2,"set_at_ms":"x"}`))
		}, MarkerNewerVersion, true},
		{"不是string_不动", func(t *testing.T, mr *miniredis.Miniredis) {
			_, err := mr.SAdd(placement.RouteReadyKey, "x")
			require.NoError(t, err)
		}, MarkerWrongType, true},
		{"身份不符_不动", func(t *testing.T, mr *miniredis.Miniredis) {
			require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, other, testGeneration, 42)))
		}, MarkerIdentityMismatch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, rd := newMappingRedis(t)
			tc.preset(t, mr)
			db := frozenUnfreezeDB()

			rep, run := runUnfreeze(t, rd, db, false)

			assert.Equal(t, ExitFailed, run.code, run.stderr)
			assert.Equal(t, tc.wantState, rep.Marker)
			assert.Empty(t, db.unfreezeCalls, "确认不了缓存就不许解冻")
			assert.Empty(t, run.stdout)
			assert.Contains(t, run.stderr, "§9.2")
			assert.Contains(t, run.stderr, "-cache-was-not-verified-acknowledged")
			assert.Equal(t, tc.wantKept, mr.Exists(placement.RouteReadyKey))
		})
	}
}

// TestUnfreeze_AcknowledgedProceedsWithZeroWatermark:显式确认之后照常解冻,最低水位 0(没有可信的「缓存截至」时刻);
// 身份不符的标记按原值撤掉(读方只看「存在」,不可信的标记不能活过解冻);版本更新的标记仍不动。
func TestUnfreeze_AcknowledgedProceedsWithZeroWatermark(t *testing.T) {
	t.Run("缺席", func(t *testing.T) {
		_, rd := newMappingRedis(t)
		db := frozenUnfreezeDB()
		rep, run := runUnfreeze(t, rd, db, true)
		assert.Equal(t, ExitClean, run.code, run.stderr)
		assert.Equal(t, []uint64{0}, db.unfreezeCalls)
		assert.Equal(t, MarkerAbsent, rep.Marker)
		assert.Contains(t, run.stderr, "WARN: ")
	})
	t.Run("身份不符_撤掉", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		other := RedisIdentity{RunID: testIdentity.RunID, MasterReplID: strings.Repeat("ef", 20)}
		require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, other, testGeneration, 42)))
		db := frozenUnfreezeDB()
		_, run := runUnfreeze(t, rd, db, true)
		assert.Equal(t, ExitClean, run.code, run.stderr)
		assert.Equal(t, []uint64{0}, db.unfreezeCalls, "不可信标记的水位不用")
		assert.False(t, mr.Exists(placement.RouteReadyKey))
	})
	t.Run("版本更新_仍不动", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, `{"v":2}`))
		db := frozenUnfreezeDB()
		_, run := runUnfreeze(t, rd, db, true)
		assert.Equal(t, ExitClean, run.code, run.stderr)
		assert.True(t, mr.Exists(placement.RouteReadyKey))
	})
}

// TestUnfreeze_AlreadyWritableIsNoop:开始时已经可写 → 退出码 0、不读标记、不调解冻。
func TestUnfreeze_AlreadyWritableIsNoop(t *testing.T) {
	_, rd := newMappingRedis(t)
	db := frozenUnfreezeDB()
	db.meta.Writable = 1

	rep, run := runUnfreeze(t, rd, db, false)

	assert.Equal(t, ExitClean, run.code, run.stderr)
	assert.True(t, rep.AlreadyWritable)
	assert.Empty(t, db.unfreezeCalls)
	assert.Contains(t, run.stdout, "already_writable=true")
}

// TestUnfreeze_RedisModeNeedsAckAndSkipsMarker(2026-10-18 修复评审):redis 模式下 unfreeze 是撤销冻结的唯一工具路径,
// 不读标记(不给 mapping Redis 与身份读取也能跑)、最低水位 0;没有显式确认就拒绝(退出码 2),连库都不读。
func TestUnfreeze_RedisModeNeedsAckAndSkipsMarker(t *testing.T) {
	run := func(db unfreezeDB, ack bool) (UnfreezeReport, cliRun, error) {
		var out, errOut bytes.Buffer
		rep, err := Unfreeze(context.Background(), nil, db, UnfreezeOptions{
			Catalog: &unitMaintenanceCatalog{releaser: db}, TicketID: bytes.Repeat([]byte{7}, 16), Request: unitReleaseRequest(t, nil, db),
			CacheUnverifiedAcknowledged: ack,
			RedisMode:                   true,
			Timeout:                     store.RouteFreezeTimeoutMin,
			Stdout:                      &out,
			Stderr:                      &errOut,
		})
		return rep, cliRun{code: UnfreezeExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}, err
	}

	db := frozenUnfreezeDB()
	_, r, err := run(db, false)
	assert.Equal(t, ExitFailed, r.code)
	assert.ErrorIs(t, err, ErrCacheNotVerified)
	assert.Empty(t, db.unfreezeCalls)
	assert.Empty(t, r.stdout)

	db = frozenUnfreezeDB()
	rep, r, err := run(db, true)
	require.NoError(t, err)
	assert.Equal(t, ExitClean, r.code, r.stderr)
	assert.Equal(t, []uint64{0}, db.unfreezeCalls, "redis 模式没有可信的「缓存截至」时刻")
	assert.Equal(t, MarkerNotReadRedisMode, rep.Marker)
	assert.True(t, db.deadline, "解冻写带时限")
	assert.Contains(t, r.stderr, "WARN: RouteStore.Mode=redis")
	assert.Equal(t, "route_unfreeze changed=true already_writable=false marker=not_read_redis_mode generation_realigned=false min_watermark_ms=0\n", r.stdout)

	_, err = Unfreeze(context.Background(), nil, frozenUnfreezeDB(), UnfreezeOptions{
		Timeout: store.RouteFreezeTimeoutMin, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	require.Error(t, err, "db_first 下没有 mapping Redis 与身份读取不许跑")
}

// TestFreeze_OutputAndRedisModeWarning:freeze 只碰库,带时限调用;stdout 一行统计;redis 模式打 WARN;
// 时限低于下限直接拒绝(不调库);库出错退出码 2、不出统计行。
func TestFreeze_OutputAndRedisModeWarning(t *testing.T) {
	run := func(db *fakeUnfreezeDB, redisMode bool, timeout time.Duration) cliRun {
		var out, errOut bytes.Buffer
		rep, err := Freeze(context.Background(), db, FreezeOptions{
			Catalog: &unitMaintenanceCatalog{freezer: db}, TicketID: bytes.Repeat([]byte{7}, 16), Request: unitReceipt(t, pb.SqlWriterMaintenanceReceipt_FREEZE, 0),
			Reason: "PITR 2026-10-18", Timeout: timeout, RedisMode: redisMode, Stdout: &out, Stderr: &errOut,
		})
		return cliRun{code: FreezeExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
	}

	db := &fakeUnfreezeDB{freezeChanged: true}
	r := run(db, false, store.RouteFreezeTimeoutMin)
	assert.Equal(t, ExitClean, r.code, r.stderr)
	assert.Equal(t, "route_freeze changed=true writable=0\n", r.stdout)
	assert.Contains(t, r.stderr, `FREEZE reason="PITR 2026-10-18"`)
	assert.NotContains(t, r.stderr, "Mode=redis")
	assert.True(t, db.deadline)

	r = run(&fakeUnfreezeDB{}, true, store.RouteFreezeTimeoutMin)
	assert.Equal(t, ExitClean, r.code)
	assert.Equal(t, "route_freeze changed=true writable=0\n", r.stdout) // 正式Catalog成功也涵盖同request永久收据重读，旧affected-rows布尔不再是API。
	assert.Contains(t, r.stderr, "WARN: RouteStore.Mode=redis")

	db = &fakeUnfreezeDB{}
	r = run(db, false, store.RouteFreezeTimeoutMin-time.Millisecond)
	assert.Equal(t, ExitFailed, r.code)
	assert.Zero(t, db.freezeCalls, "时限不够就不调库")

	r = run(&fakeUnfreezeDB{freezeErr: errors.New("Error 1205")}, false, store.RouteFreezeTimeoutMin)
	assert.Equal(t, ExitFailed, r.code)
	assert.Empty(t, r.stdout)
}

// TestParseRedisIdentity:按捕获的 Redis 7.2 INFO 文本(CRLF)取 run_id 与 master_replid;LF 也认;缺字段、长度不对、
// 大写一律报错(身份读不准时宁可确认不了)。
func TestParseRedisIdentity(t *testing.T) {
	server := "# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\nrun_id:" + testIdentity.RunID + "\r\ntcp_port:6379\r\n"
	replication := "# Replication\r\nrole:master\r\nconnected_slaves:0\r\nmaster_replid:" + testIdentity.MasterReplID +
		"\r\nmaster_replid2:0000000000000000000000000000000000000000\r\n"

	id, err := ParseRedisIdentity(server, replication)
	require.NoError(t, err)
	assert.Equal(t, testIdentity, id)

	id, err = ParseRedisIdentity(strings.ReplaceAll(server, "\r\n", "\n"), strings.ReplaceAll(replication, "\r\n", "\n"))
	require.NoError(t, err)
	assert.Equal(t, testIdentity, id, "LF 行尾也认")

	for _, tc := range []struct{ name, server, replication string }{
		{"缺run_id", "# Server\r\nredis_version:7.2.4\r\n", replication},
		{"缺master_replid", server, "# Replication\r\nrole:master\r\n"},
		{"run_id长度不对", strings.Replace(server, testIdentity.RunID, testIdentity.RunID[:39], 1), replication},
		{"run_id大写", strings.Replace(server, testIdentity.RunID, strings.ToUpper(testIdentity.RunID), 1), replication},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRedisIdentity(tc.server, tc.replication)
			assert.Error(t, err)
		})
	}
}

// TestReadyMarkerScriptsOnMiniredis:placement 的两个就绪标记脚本的执行语义 —— 当前值等于原值才改 / 删、回 1;
// 不等(含键不在)回 0、零写入。
func TestReadyMarkerScriptsOnMiniredis(t *testing.T) {
	mr, rd := newMappingRedis(t)
	ctx := context.Background()
	keys := []string{placement.RouteReadyKey}

	got, err := rd.EvalCtx(ctx, placement.RouteReadyCompareAndSetScript, keys, "a", "b")
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "键不在:CAS 落空")
	assert.False(t, mr.Exists(placement.RouteReadyKey))

	require.NoError(t, mr.Set(placement.RouteReadyKey, "a"))
	got, err = rd.EvalCtx(ctx, placement.RouteReadyCompareAndSetScript, keys, "x", "b")
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "原值不等:CAS 落空")
	got, err = rd.EvalCtx(ctx, placement.RouteReadyCompareAndSetScript, keys, "a", "b")
	require.NoError(t, err)
	assert.Equal(t, int64(1), got)
	v, _ := mr.Get(placement.RouteReadyKey)
	assert.Equal(t, "b", v)
	assert.Zero(t, mr.TTL(placement.RouteReadyKey), "CAS 写的值不带过期")

	got, err = rd.EvalCtx(ctx, placement.RouteReadyCompareAndDeleteScript, keys, "a")
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "原值不等:CAD 落空")
	assert.True(t, mr.Exists(placement.RouteReadyKey))
	got, err = rd.EvalCtx(ctx, placement.RouteReadyCompareAndDeleteScript, keys, "b")
	require.NoError(t, err)
	assert.Equal(t, int64(1), got)
	assert.False(t, mr.Exists(placement.RouteReadyKey))
}

// ── 2026-10-19 第 1b-2 批收尾:水位键的出口与身份解析的单一口径 ────────────────────────────────

// watermarkKeyFor 编出一份 mapping:route:watermark 的合规值。
func watermarkKeyFor(t *testing.T, gen string, watermarkMs uint64) string {
	t.Helper()
	raw, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: gen, WatermarkMs: watermarkMs})
	require.NoError(t, err)
	return raw
}

// runUnfreezeErr 同 runUnfreeze,另把 Unfreeze 的 error 带出来(按哨兵断言用)。
func runUnfreezeErr(t *testing.T, rd readyMarkerRedis, db unfreezeDB, ack bool) (UnfreezeReport, cliRun, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rep, err := Unfreeze(context.Background(), rd, db, UnfreezeOptions{
		Catalog: &unitMaintenanceCatalog{releaser: db}, TicketID: bytes.Repeat([]byte{7}, 16), Request: unitReleaseRequest(t, rd, db),
		CacheUnverifiedAcknowledged: ack,
		Timeout:                     store.RouteFreezeTimeoutMin,
		Identity:                    func(context.Context) (RedisIdentity, error) { return testIdentity, nil },
		Stdout:                      &out,
		Stderr:                      &errOut,
	})
	return rep, cliRun{code: UnfreezeExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}, err
}

// TestUnfreeze_RaisesFloorToWatermarkKey:解冻的最低水位取 max(可信标记的 watermark_ms, 世代相同的水位键的 watermark_ms)
// (规格 §2 M3:不抬的话解冻之后 leader 下一拍就按「水位键 > 库水位」再冻结);别的世代的水位键不约束本世代;
// 显式确认放行(没有可信标记)时水位键那一项照取。
func TestUnfreeze_RaisesFloorToWatermarkKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		markerWm uint64 // 0 = 没有标记(走显式确认)
		keyGen   string // "" = 水位键不在;"malformed" = 畸形值
		keyWm    uint64
		wantMin  uint64
		wantKey  string // 解冻之后水位键的处置
	}{
		{"水位键更高_抬到水位键", 1000, testGeneration, 5000, 5000, WatermarkKeyKept},
		{"标记更高_取标记_水位键跟上", 9000, testGeneration, 5000, 9000, WatermarkKeyCompareAndSet},
		{"别的世代_不约束", 1000, "ffff", 99999, 1000, WatermarkKeyCompareAndSet},
		{"显式确认_仍取水位键", 0, testGeneration, 7000, 7000, WatermarkKeyKept},
		{"显式确认_别的世代_水位0_按原值删", 0, "ffff", 7000, 0, WatermarkKeyDropped},
		{"水位键畸形_不给下限_改掉", 1000, "malformed", 0, 1000, WatermarkKeyCompareAndSet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, rd := newMappingRedis(t)
			if tc.markerWm > 0 {
				require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, testGeneration, tc.markerWm)))
			}
			switch tc.keyGen {
			case "":
			case "malformed":
				require.NoError(t, mr.Set(placement.RouteWatermarkKey, `{"v":1}`))
			default:
				require.NoError(t, mr.Set(placement.RouteWatermarkKey, watermarkKeyFor(t, tc.keyGen, tc.keyWm)))
			}
			db := frozenUnfreezeDB()

			rep, run, err := runUnfreezeErr(t, rd, db, tc.markerWm == 0)

			require.NoError(t, err, run.stderr)
			assert.Equal(t, ExitClean, run.code)
			assert.Equal(t, []uint64{tc.wantMin}, db.unfreezeCalls)
			assert.Equal(t, tc.wantMin, rep.MinWatermarkMs)
			assert.Equal(t, tc.wantKey, rep.WatermarkKey)
			assert.Contains(t, run.stdout, " min_watermark_ms="+strconv.FormatUint(tc.wantMin, 10)+"\n", "统计行格式不变")
			if tc.wantKey == WatermarkKeyDropped {
				assert.False(t, mr.Exists(placement.RouteWatermarkKey), "没有可写的正水位:按原值删掉,留给 leader 重建")
				return
			}
			got, err := mr.Get(placement.RouteWatermarkKey)
			require.NoError(t, err)
			assert.Equal(t, watermarkKeyFor(t, testGeneration, tc.wantMin), got, "解冻之后水位键 = {库世代, 最低水位}")
		})
	}
}

// orderUnfreezeDB 在解冻调用那一刻记下水位键的值:钉住「先翻 writable、再改水位键」的顺序。
type orderUnfreezeDB struct {
	*fakeUnfreezeDB
	mr           *miniredis.Miniredis
	keyAtFlip    string
	keyAtFlipSet bool
}

func (o *orderUnfreezeDB) UnfreezeRouteWriters(ctx context.Context, minWatermarkMs uint64) (bool, error) {
	o.keyAtFlip, _ = o.mr.Get(placement.RouteWatermarkKey)
	o.keyAtFlipSet = o.mr.Exists(placement.RouteWatermarkKey)
	return o.fakeUnfreezeDB.UnfreezeRouteWriters(ctx, minWatermarkMs)
}

// watermarkRacingRedis 在第一次对水位键的 EVAL / SETNX 之前,先把水位键换成 value(模拟读到之后被 leader / 别的写者改过)。
type watermarkRacingRedis struct {
	readyMarkerRedis
	mr    *miniredis.Miniredis
	value string
	raced bool
}

func (w *watermarkRacingRedis) race() error {
	if w.raced {
		return nil
	}
	w.raced = true
	return w.mr.Set(placement.RouteWatermarkKey, w.value)
}

func (w *watermarkRacingRedis) EvalCtx(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if len(keys) == 1 && keys[0] == placement.RouteWatermarkKey {
		if err := w.race(); err != nil {
			return nil, err
		}
	}
	return w.readyMarkerRedis.EvalCtx(ctx, script, keys, args...)
}

func (w *watermarkRacingRedis) SetnxCtx(ctx context.Context, key, value string) (bool, error) {
	if key == placement.RouteWatermarkKey {
		if err := w.race(); err != nil {
			return false, err
		}
	}
	return w.readyMarkerRedis.SetnxCtx(ctx, key, value)
}

// TestUnfreeze_ResetsWatermarkKeyAfterFlip:水位键在 writable 翻回 1 **之后**才改(规格 §13.1 第 4 条:同一流程、先翻后改);
// 不在就 SET NX;没有可写的正水位又不在就留给 leader。改输了:复读到本世代的值(leader 已推进它)算完成,
// 复读到别的世代 → 退出码 1(统计行照出,writable 已是 1),重跑走「已可写」分支、退出码 0(幂等)。
func TestUnfreeze_ResetsWatermarkKeyAfterFlip(t *testing.T) {
	t.Run("先翻writable再改水位键", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, testGeneration, 1000)))
		old := watermarkKeyFor(t, "ffff", 50)
		require.NoError(t, mr.Set(placement.RouteWatermarkKey, old))
		db := &orderUnfreezeDB{fakeUnfreezeDB: frozenUnfreezeDB(), mr: mr}

		rep, run, err := runUnfreezeErr(t, rd, db, false)

		require.NoError(t, err, run.stderr)
		assert.True(t, db.keyAtFlipSet)
		assert.Equal(t, old, db.keyAtFlip, "翻 writable 的那一刻水位键还是旧值")
		got, _ := mr.Get(placement.RouteWatermarkKey)
		assert.Equal(t, watermarkKeyFor(t, testGeneration, 1000), got)
		assert.Equal(t, WatermarkKeyCompareAndSet, rep.WatermarkKey)
		assert.Contains(t, run.stderr, "WATERMARK_KEY compare_and_set before=decoded ")
	})

	t.Run("不在_SETNX_统计行不变", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, testGeneration, 1000)))
		db := &orderUnfreezeDB{fakeUnfreezeDB: frozenUnfreezeDB(), mr: mr}

		rep, run, err := runUnfreezeErr(t, rd, db, false)

		require.NoError(t, err, run.stderr)
		assert.False(t, db.keyAtFlipSet, "翻 writable 之前不写水位键")
		got, _ := mr.Get(placement.RouteWatermarkKey)
		assert.Equal(t, watermarkKeyFor(t, testGeneration, 1000), got)
		assert.Zero(t, mr.TTL(placement.RouteWatermarkKey), "水位键不带过期")
		assert.Equal(t, WatermarkKeySetNX, rep.WatermarkKey)
		assert.Equal(t, "route_unfreeze changed=true already_writable=false marker=verified generation_realigned=false min_watermark_ms=1000\n", run.stdout)
	})

	t.Run("不在_也没有正水位_留给leader", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		rep, run, err := runUnfreezeErr(t, rd, frozenUnfreezeDB(), true)
		require.NoError(t, err, run.stderr)
		assert.Equal(t, WatermarkKeyLeftAbsent, rep.WatermarkKey)
		assert.False(t, mr.Exists(placement.RouteWatermarkKey))
	})

	t.Run("改输了_复读到本世代的值_算完成", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, testGeneration, 1000)))
		leaderValue := watermarkKeyFor(t, testGeneration, 1500)
		raced := &watermarkRacingRedis{readyMarkerRedis: rd, mr: mr, value: leaderValue}

		rep, run, err := runUnfreezeErr(t, raced, frozenUnfreezeDB(), false)

		require.NoError(t, err, run.stderr)
		assert.Equal(t, ExitClean, run.code)
		assert.Equal(t, WatermarkKeySuperseded, rep.WatermarkKey)
		got, _ := mr.Get(placement.RouteWatermarkKey)
		assert.Equal(t, leaderValue, got, "leader 推进过的值不覆盖")
	})

	t.Run("改输了_复读到别的世代_退出码1_重跑幂等", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		require.NoError(t, mr.Set(placement.RouteReadyKey, readyMarkerFor(t, testIdentity, testGeneration, 1000)))
		require.NoError(t, mr.Set(placement.RouteWatermarkKey, watermarkKeyFor(t, "ffff", 50)))
		raced := &watermarkRacingRedis{readyMarkerRedis: rd, mr: mr, value: watermarkKeyFor(t, "eeee", 60)}
		db := frozenUnfreezeDB()

		rep, run, err := runUnfreezeErr(t, raced, db, false)

		assert.ErrorIs(t, err, ErrWatermarkKeyNotReset)
		assert.Equal(t, ExitAttention, run.code)
		assert.Equal(t, WatermarkKeyNotReset, rep.WatermarkKey)
		assert.Equal(t, []uint64{1000}, db.unfreezeCalls, "writable 已经翻回 1")
		assert.Contains(t, run.stdout, "route_unfreeze changed=true ", "翻成功了,统计行照出")

		rep, run, err = runUnfreezeErr(t, rd, db, false)
		require.NoError(t, err, run.stderr)
		assert.Equal(t, ExitClean, run.code)
		assert.True(t, rep.AlreadyWritable, "重跑走「已可写」分支")
		assert.Len(t, db.unfreezeCalls, 1)
	})
}

// TestUnfreeze_WatermarkKeyNewerVersionRefused:水位键是更新格式版本写的 → 退出码 2,在任何写之前拒绝:不解冻、不动水位键,
// 连畸形标记的撤除都不做;显式确认也不放行(那只确认缓存,不认识的水位键下限规则本二进制不知道)。不是 string 类型同样退出码 2。
func TestUnfreeze_WatermarkKeyNewerVersionRefused(t *testing.T) {
	const newer = `{"v":2,"watermark_ms":"x"}`
	for _, ack := range []bool{false, true} {
		t.Run("ack="+strconv.FormatBool(ack), func(t *testing.T) {
			mr, rd := newMappingRedis(t)
			require.NoError(t, mr.Set(placement.RouteReadyKey, "malformed-marker"))
			require.NoError(t, mr.Set(placement.RouteWatermarkKey, newer))
			db := frozenUnfreezeDB()

			_, run, err := runUnfreezeErr(t, rd, db, ack)

			assert.ErrorIs(t, err, placement.ErrRouteWatermarkNewerVersion)
			assert.Equal(t, ExitFailed, run.code)
			assert.Empty(t, db.unfreezeCalls)
			assert.Empty(t, run.stdout)
			got, _ := mr.Get(placement.RouteWatermarkKey)
			assert.Equal(t, newer, got, "不动它")
			assert.True(t, mr.Exists(placement.RouteReadyKey), "拒绝在任何写之前:畸形标记也没撤")
		})
	}

	t.Run("不是string", func(t *testing.T) {
		mr, rd := newMappingRedis(t)
		_, err := mr.SAdd(placement.RouteWatermarkKey, "x")
		require.NoError(t, err)
		db := frozenUnfreezeDB()

		_, run, err := runUnfreezeErr(t, rd, db, true)

		assert.ErrorIs(t, err, ErrWatermarkKeyWrongType)
		assert.Equal(t, ExitFailed, run.code)
		assert.Empty(t, db.unfreezeCalls)
	})
}

// TestParseRedisIdentityAgreesWithPlacement:ParseRedisIdentity 委托 placement.ParseRedisInstanceIdentity(规格 §4.6 第 5 项),
// 对同一份 INFO 文本两者结论一致:同成同败,成功时 run_id / master_replid 相同。缺 role 现在两边都报错(相对 1b-1 的唯一收紧)。
func TestParseRedisIdentityAgreesWithPlacement(t *testing.T) {
	server := "# Server\r\nredis_version:7.2.4\r\nrun_id:" + testIdentity.RunID + "\r\ntcp_port:6379\r\n"
	replication := "# Replication\r\nrole:master\r\nconnected_slaves:0\r\nmaster_replid:" + testIdentity.MasterReplID +
		"\r\nmaster_replid2:0000000000000000000000000000000000000000\r\n"
	for _, tc := range []struct {
		name, server, replication string
		wantOK                    bool
	}{
		{"CRLF", server, replication, true},
		{"LF", strings.ReplaceAll(server, "\r\n", "\n"), strings.ReplaceAll(replication, "\r\n", "\n"), true},
		{"副本", server, strings.Replace(replication, "role:master", "role:slave", 1), true},
		{"缺run_id", "# Server\r\nredis_version:7.2.4\r\n", replication, false},
		{"缺master_replid", server, "# Replication\r\nrole:master\r\n", false},
		{"缺role", server, strings.Replace(replication, "role:master\r\n", "", 1), false},
		{"run_id大写", strings.Replace(server, testIdentity.RunID, strings.ToUpper(testIdentity.RunID), 1), replication, false},
		{"replid长度不对", server, strings.Replace(replication, testIdentity.MasterReplID, testIdentity.MasterReplID[:39], 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRedisIdentity(tc.server, tc.replication)
			want, perr := placement.ParseRedisInstanceIdentity(tc.server, tc.replication)
			assert.Equal(t, perr == nil, err == nil, "同成同败:routetool=%v placement=%v", err, perr)
			assert.Equal(t, tc.wantOK, err == nil, err)
			if err == nil {
				assert.Equal(t, RedisIdentity{RunID: want.RunID, MasterReplID: want.MasterReplID}, got)
				assert.Equal(t, testIdentity, got)
			}
		})
	}
}

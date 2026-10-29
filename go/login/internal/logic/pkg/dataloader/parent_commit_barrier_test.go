package dataloader

import (
	"context"
	"fmt"
	"login/internal/config"
	"login/internal/dispatcher"
	componentpb "proto/common/component"
	databasepb "proto/common/database"
	dbpb "proto/db"
	"shared/ownerepoch"
	"shared/savebarrier"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// 替身只提供 SQL 读回包，组装、校验、CAS 以及完成回调均走正式入口。
type coldSQLReads struct {
	callbacks map[string]dispatcher.ResultCallback
	read      func(*dbpb.DBTask) ([]byte, error)
	count     int
}

func (reads *coldSQLReads) Register(id string, _ time.Duration, callback dispatcher.ResultCallback) {
	if reads.callbacks == nil {
		reads.callbacks = make(map[string]dispatcher.ResultCallback)
	}
	reads.callbacks[id] = callback
}

// ReplyInstance 满足 readResultRegistrar;替身不经 Pub/Sub 投递,给一个合法的固定值即可。
func (reads *coldSQLReads) ReplyInstance() string { return "cold-sql-reads" }

// Cancel 满足 readResultRegistrar;替身的 SendTasks 不返回错误,正常不会走到这里。
func (reads *coldSQLReads) Cancel(ids ...string) {
	for _, id := range ids {
		delete(reads.callbacks, id)
	}
}

func (reads *coldSQLReads) SendTask(ctx context.Context, task *dbpb.DBTask, key string) error {
	return reads.SendTasks(ctx, []*dbpb.DBTask{task}, key)
}

func (reads *coldSQLReads) SendTasks(_ context.Context, tasks []*dbpb.DBTask, _ string) error {
	for _, task := range tasks {
		reads.count++
		data, err := reads.read(task)
		reads.callbacks[task.TaskId](&dbpb.TaskResult{Success: err == nil, Data: data, Error: fmt.Sprint(err)}, nil)
	}
	return nil
}

func sqlReadAtVersion(epoch, seq uint64) func(*dbpb.DBTask) ([]byte, error) {
	return func(task *dbpb.DBTask) ([]byte, error) {
		typ, err := protoregistry.GlobalTypes.FindMessageByURL(task.MsgType)
		if err != nil {
			return nil, err
		}
		sub := typ.New().Interface()
		if err := proto.Unmarshal(task.Body, sub); err != nil {
			return nil, err
		}
		switch row := sub.(type) {
		case *databasepb.PlayerDatabase:
			row.FenceOwnerEpoch, row.FenceSaveSeq = epoch, seq
		case *databasepb.PlayerDatabase_1:
			row.FenceOwnerEpoch, row.FenceSaveSeq = epoch, seq
		}
		return proto.Marshal(sub)
	}
}

func installCommittedBarrier(t *testing.T, rc *redis.Client, epoch, seq uint64) {
	t.Helper()
	ctx := context.Background()
	if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), epoch, 0).Err(); err != nil {
		t.Fatal(err)
	}
	v := savebarrier.Version(epoch, seq)
	// 此 fixture 明确表示同一实例已经核验并提交过 SQL；无须绕过正式的身份核验开关。
	if err := rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "pending", v, "player_database", v, "player_database_1", v, savebarrier.VerifiedIdentityField, "redis-test").Err(); err != nil {
		t.Fatal(err)
	}
}

func TestColdRecoveryRequiresBothSQLReceipts(t *testing.T) {
	for _, field := range []string{"player_database", "player_database_1"} {
		t.Run(field, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			installCommittedBarrier(t, rc, 2, 99)
			if err := rc.HSet(context.Background(), savebarrier.Key(seedTestPlayerID), field, savebarrier.Version(2, 98)).Err(); err != nil {
				t.Fatal(err)
			}
			calls := 0
			ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, nil, nil, seedTestPlayerID, 0, func(err error) {
				calls++
				if err == nil {
					t.Error("未提交的子表被放行")
				}
			})
			if calls != 1 || mr.Exists(seedParentKey(seedTestPlayerID)) {
				t.Fatal("未提交恢复写入了父键")
			}
		})
	}
}

func TestColdRecoveryIgnoresSubCachesAndCarriesSQLFloor(t *testing.T) {
	_, rc := newSeedTestRedis(t)
	installCommittedBarrier(t, rc, 2, 99)
	for _, sub := range seedSubMessages(seedTestPlayerID) {
		raw, _ := proto.Marshal(sub)
		if err := rc.Set(context.Background(), seedSubKey(sub, seedTestPlayerID), raw, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	reads := &coldSQLReads{read: sqlReadAtVersion(2, 99)}
	calls := 0
	ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		if err != nil {
			t.Error(err)
		}
	})
	parent, _ := readSeedParent(t, rc, seedTestPlayerID)
	if calls != 1 || reads.count != 2 || parent.LastSaveSeq != 99 || parent.GetPlayerDatabaseData().GetFenceSaveSeq() != 99 {
		t.Fatalf("恢复未沿 SQL 权威/播种水位: calls=%d reads=%d parent=%v", calls, reads.count, parent)
	}
}

func TestColdRecoveryDistinguishesOlderSQLAndAuthoritativeHistory(t *testing.T) {
	for _, test := range []struct {
		name       string
		install    bool
		epoch, seq uint64
		wantError  bool
	}{
		{"SQL较旧", true, 2, 98, true},
		{"SQL已换主", true, 3, 100, false},
		{"Redis归属历史丢失", false, 2, 99, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			if test.install {
				installCommittedBarrier(t, rc, 2, 99)
			}
			reads := &coldSQLReads{read: sqlReadAtVersion(test.epoch, test.seq)}
			calls := 0
			ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, reads, reads, seedTestPlayerID, 0, func(err error) {
				calls++
				if (err != nil) != test.wantError {
					t.Errorf("SQL 核验错误=%v，期望拒绝=%v", err, test.wantError)
				}
			})
			if calls != 1 || mr.Exists(seedParentKey(seedTestPlayerID)) == test.wantError {
				t.Fatal("SQL 核验的完成次数或父键终态不符")
			}
			if !test.wantError {
				parent, _ := readSeedParent(t, rc, seedTestPlayerID)
				if parent.LastSaveSeq != test.seq || parent.GetPlayerDatabaseData().GetFenceOwnerEpoch() != test.epoch || parent.GetPlayerDatabase_1Data().GetFenceSaveSeq() != test.seq || rc.Get(context.Background(), ownerepoch.OwnerEpochKey(seedTestPlayerID)).Val() != fmt.Sprint(test.epoch) {
					t.Fatal("SQL 权威恢复未精确采纳版本并抬高 owner")
				}
			}
		})
	}
}

func TestColdRecoveryCASPreservesNewOwnerOrNewerSnapshot(t *testing.T) {
	for _, mutation := range []string{"owner", "pending", "parent"} {
		t.Run(mutation, func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			installCommittedBarrier(t, rc, 2, 99)
			ctx := context.Background()
			key := seedParentKey(seedTestPlayerID)
			snapshot, err := beginParentLoad(ctx, rc, testParentLoadScripts, key, seedTestPlayerID, config.PersistSchemaRegressionWarn)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "owner":
				err = rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "3", 0).Err()
			case "pending":
				err = rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "pending", savebarrier.Version(2, 100)).Err()
			case "parent":
				err = rc.Set(ctx, key, "newer-parent", 0).Err()
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = snapshot.settle(ctx, rc, testParentLoadScripts, key, seedTestPlayerID, sqlParentAtVersion(2, 99), config.PersistSchemaRegressionWarn)
			if mutation == "parent" {
				if err != nil || rc.Get(ctx, key).Val() != "newer-parent" {
					t.Fatal("覆盖了并发新父键", err)
				}
			} else if err == nil || rc.Exists(ctx, key).Val() != 0 {
				t.Fatal("令牌变化未阻止恢复")
			}
		})
	}
}

func TestColdRecoveryFailsClosedForMalformedBarrierAndRedisFailure(t *testing.T) {
	for _, fault := range []string{"wrong-type", "overflow", "missing-pending", "redis-error"} {
		t.Run(fault, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			installCommittedBarrier(t, rc, 2, 99)
			ctx := context.Background()
			switch fault {
			case "wrong-type":
				mr.Del(savebarrier.Key(seedTestPlayerID))
				mr.Set(savebarrier.Key(seedTestPlayerID), "corrupt")
			case "overflow":
				_ = rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "pending", "18446744073709551616:00000000000000000099").Err()
			case "missing-pending":
				_ = rc.HDel(ctx, savebarrier.Key(seedTestPlayerID), "pending").Err()
			case "redis-error":
				mr.SetError("injected Redis unavailable")
			}
			snapshot, err := beginParentLoad(ctx, rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, config.PersistSchemaRegressionWarn)
			if err == nil && fault == "missing-pending" {
				// ACK 已存在而 pending 丢失会转 SQL 核验；SQL 空行不能洗掉这份已提交证据。
				_, err = snapshot.settle(ctx, rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, newPlayerAllDataSkeleton(seedTestPlayerID), config.PersistSchemaRegressionWarn)
			}
			if err == nil {
				t.Fatal("损坏/不可用屏障未拒绝")
			}
		})
	}
}

func TestUnversionedHistoryCannotBeMistakenForNewPlayer(t *testing.T) {
	t.Run("种子已分配过归属", func(t *testing.T) {
		_, rc := newSeedTestRedis(t)
		ctx := context.Background()
		if _, err := seedTestNewPlayerAllData(ctx, rc, seedTestPlayerID); err != nil {
			t.Fatal(err)
		}
		if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "1", 0).Err(); err != nil {
			t.Fatal(err)
		}
		snapshot, err := beginParentLoad(ctx, rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, config.PersistSchemaRegressionWarn)
		if err != nil {
			t.Fatal(err)
		}
		if route, _ := snapshot.route(false); snapshot.historyGap == nil || route != parentLoadVerify {
			t.Fatal("已分配归属的旧种子被当成新号")
		}
	})
	t.Run("历史零围栏资产", func(t *testing.T) {
		mr, rc := newSeedTestRedis(t)
		read := sqlReadAtVersion(0, 0)
		reads := &coldSQLReads{read: func(task *dbpb.DBTask) ([]byte, error) {
			raw, err := read(task)
			if task.MsgType != string((&databasepb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName()) {
				return raw, err
			}
			row := new(databasepb.PlayerDatabase)
			if err := proto.Unmarshal(raw, row); err != nil {
				return nil, err
			}
			row.LevelComponent = &componentpb.LevelComp{Level: 30}
			return proto.Marshal(row)
		}}
		calls := 0
		ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, reads, reads, seedTestPlayerID, 0, func(err error) {
			calls++
			if err == nil {
				t.Error("零围栏历史资产被当成新号")
			}
		})
		if calls != 1 || mr.Exists(seedParentKey(seedTestPlayerID)) {
			t.Fatal("无屏障历史资产写成了父键")
		}
	})
}

func TestUnownedIdentitySeedDoesNotAdmitAssetsOrUnknownFields(t *testing.T) {
	unknown := protowire.AppendVarint(protowire.AppendTag(nil, 1000000, protowire.VarintType), 1)
	for _, tc := range []struct {
		name      string
		mutate    func(*databasepb.PlayerAllData)
		wantError bool
	}{
		{"仅身份可重试", func(*databasepb.PlayerAllData) {}, false},
		{"一枚金币仍是历史资产", func(p *databasepb.PlayerAllData) {
			p.PlayerDatabaseData.Currency = &componentpb.CurrencyComp{Values: []uint64{1}}
		}, true},
		{"已注册不能冒充新号", func(p *databasepb.PlayerAllData) {
			p.PlayerDatabaseData.Uint64PbComponent = &componentpb.PlayerUint64Comp{RegistrationTimestamp: 1}
		}, true},
		{"顶层未知字段保留并拒绝", func(p *databasepb.PlayerAllData) { p.ProtoReflect().SetUnknown(unknown) }, true},
		{"身份未知字段保留并拒绝", func(p *databasepb.PlayerAllData) {
			p.PlayerDatabaseData.ProfileComponent.ProtoReflect().SetUnknown(unknown)
		}, true},
		{"职业未知字段保留并拒绝", func(p *databasepb.PlayerAllData) {
			p.PlayerDatabaseData.Uint32PbComponent.ProtoReflect().SetUnknown(unknown)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			parent := newPlayerAllDataSkeleton(seedTestPlayerID)
			parent.PlayerDatabaseData.Uint32PbComponent = &componentpb.PlayerUint32Comp{Class: 3}
			parent.PlayerDatabaseData.ProfileComponent = &componentpb.PlayerProfileComp{Name: "云中君", AppearanceId: "appearance-verified", Gender: 2}
			tc.mutate(parent)
			raw, err := proto.Marshal(parent)
			if err != nil {
				t.Fatal(err)
			}
			if err := rc.Set(context.Background(), seedParentKey(seedTestPlayerID), raw, time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := beginParentLoad(context.Background(), rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, config.PersistSchemaRegressionWarn)
			if err == nil {
				err = snapshot.historyGap
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("身份种子校验=%v，预期拒绝=%v", err, tc.wantError)
			}
			after, err := rc.Get(context.Background(), seedParentKey(seedTestPlayerID)).Bytes()
			if err != nil || string(after) != string(raw) {
				t.Fatal("只读检查修改了种子")
			}
		})
	}
}

// ── C4:父快照快路径的持久 schema 世代倒退判定(设计 §5.1、§5.4;交接 §3.3) ──────────────

const persistSchemaRegressionMetric = "login_persist_schema_regression_total"

// schemaRegressionKey 是 login_persist_schema_regression_total 的一条序列。
type schemaRegressionKey struct{ reason, mode string }

// allSchemaRegressionKeys 是全部合法序列。label 值写字面量而不引用常量:它们是告警表达式的契约,要单独钉住。
var allSchemaRegressionKeys = []schemaRegressionKey{
	{"regression", "warn"}, {"regression", "enforce"}, {"malformed", "warn"}, {"malformed", "enforce"},
}

// schemaRegressionCounts 读出各序列当前值,序列还没出现按 0。先 Enable:单测里 go-zero 的指标开关默认关着,
// 之前的写入都被丢弃。计数器是进程级累计值,用例一律比较前后差值;本包用例不加 t.Parallel()。
func schemaRegressionCounts(t *testing.T) map[schemaRegressionKey]float64 {
	t.Helper()
	gzprom.Enable()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	got := make(map[schemaRegressionKey]float64)
	for _, mf := range families {
		if mf.GetName() != persistSchemaRegressionMetric {
			continue
		}
		for _, m := range mf.GetMetric() {
			var key schemaRegressionKey
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "reason":
					key.reason = lp.GetValue()
				case "mode":
					key.mode = lp.GetValue()
				}
			}
			got[key] = m.GetCounter().GetValue()
		}
	}
	return got
}

// requireSchemaRegressionDelta 断言 want 这条序列恰好 +1、其余序列不变;want 为零值表示一条都不许增长。
// 同时要求不出现合法集合之外的序列(label 集合封闭)。
func requireSchemaRegressionDelta(t *testing.T, before map[schemaRegressionKey]float64, want schemaRegressionKey) {
	t.Helper()
	after := schemaRegressionCounts(t)
	legal := make(map[schemaRegressionKey]bool, len(allSchemaRegressionKeys))
	for _, key := range allSchemaRegressionKeys {
		legal[key] = true
		wantDelta := 0.0
		if key == want {
			wantDelta = 1
		}
		if delta := after[key] - before[key]; delta != wantDelta {
			t.Errorf("%s{reason=%q,mode=%q} delta = %v, want %v",
				persistSchemaRegressionMetric, key.reason, key.mode, delta, wantDelta)
		}
	}
	for key := range after {
		if !legal[key] {
			t.Errorf("%s has an unexpected series {reason=%q,mode=%q}", persistSchemaRegressionMetric, key.reason, key.mode)
		}
	}
}

// installFastPathParent 种下一份能通过身份、水位校验的父快照:屏障 2:99 已提交、父快照 LastSaveSeq=99,
// 两张表的戳按参数盖。withBarrierGen 时把 barrierGen 原样写进屏障 hash 的 schema_gen。
func installFastPathParent(t *testing.T, rc *redis.Client, gen, gen1 uint32, barrierGen string, withBarrierGen bool) {
	t.Helper()
	installCommittedBarrier(t, rc, 2, 99)
	parent := newPlayerAllDataSkeleton(seedTestPlayerID)
	parent.LastSaveSeq = 99
	parent.PlayerDatabaseData.PersistSchemaGen = gen
	parent.PlayerDatabase_1Data.PersistSchemaGen = gen1
	raw, err := proto.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := rc.Set(ctx, seedParentKey(seedTestPlayerID), raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if withBarrierGen {
		if err := rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "schema_gen", barrierGen).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// 快路径上的 C4:加载世代 = 两表戳最小值(0 按 1),低于屏障 schema_gen 即倒退;
// warn 照常返回快照,enforce 返回错误,两种都计数;屏障里没有 schema_gen 不比较。
func TestParentFastPathPersistSchemaRegression(t *testing.T) {
	warn, enforce := config.PersistSchemaRegressionWarn, config.PersistSchemaRegressionEnforce
	none := schemaRegressionKey{}
	regressionWarn := schemaRegressionKey{"regression", "warn"}
	regressionEnforce := schemaRegressionKey{"regression", "enforce"}
	for _, tc := range []struct {
		name           string
		gen, gen1      uint32
		barrierGen     string
		withBarrierGen bool
		mode           config.PersistSchemaRegressionMode
		wantErr        bool
		wantCount      schemaRegressionKey
	}{
		{"warn倒退照常返回快照并计数", 1, 1, "2", true, warn, false, regressionWarn},
		{"enforce倒退返回错误并计数", 1, 1, "2", true, enforce, true, regressionEnforce},
		{"屏障无schema_gen不比较", 1, 1, "", false, enforce, false, none},
		{"世代相等通过", 3, 3, "3", true, enforce, false, none},
		{"加载世代更高通过", 5, 5, "3", true, enforce, false, none},
		{"两表取最小值_player_database低", 2, 5, "3", true, enforce, true, regressionEnforce},
		{"两表取最小值_player_database_1低", 5, 2, "3", true, enforce, true, regressionEnforce},
		{"两表都不低于屏障才通过", 4, 3, "3", true, enforce, false, none},
		{"戳0按1_屏障1通过", 0, 0, "1", true, enforce, false, none},
		{"戳0按1_屏障2倒退", 0, 0, "2", true, enforce, true, regressionEnforce},
		{"一张表未盖戳把加载世代拉到1", 0, 3, "2", true, enforce, true, regressionEnforce},
		{"uint32上界合法", ^uint32(0), ^uint32(0), "4294967295", true, enforce, false, none},
		{"warn损坏照常返回快照并计数", 3, 3, "abc", true, warn, false, schemaRegressionKey{"malformed", "warn"}},
		{"enforce损坏返回错误并计数", 3, 3, "abc", true, enforce, true, schemaRegressionKey{"malformed", "enforce"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			installFastPathParent(t, rc, tc.gen, tc.gen1, tc.barrierGen, tc.withBarrierGen)
			before := schemaRegressionCounts(t)
			snapshot, err := beginParentLoad(context.Background(), rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, tc.mode)
			if (err != nil) != tc.wantErr {
				t.Fatalf("beginParentLoad err = %v, want error = %v", err, tc.wantErr)
			}
			if !tc.wantErr && (snapshot.raw == "" || snapshot.schemaGen != tc.barrierGen) {
				t.Fatalf("快路径快照未原样返回: raw=%d bytes schemaGen=%q, want schemaGen=%q",
					len(snapshot.raw), snapshot.schemaGen, tc.barrierGen)
			}
			requireSchemaRegressionDelta(t, before, tc.wantCount)
		})
	}
}

// 屏障 schema_gen 只认 scene 写入的规范十进制 uint32(>=1、无符号、无前导零、不超过 4294967295);
// 其余一律是损坏 = 倒退证据,不当成「不比较」。加载世代给到 uint32 上界:任何脏值都不会因为
// 「数值上不倒退」而漏判。
func TestParentFastPathMalformedBarrierSchemaGenIsRegressionEvidence(t *testing.T) {
	// "" 是"字段存在但值为空":与字段不存在分开判,按损坏处理(与 scene C3、go/db save_barrier_restore 同一口径)。
	for _, raw := range []string{
		"", "0", "00", "007", "01", "4294967296", "18446744073709551616", "-1", "+3", " 3", "3 ", "1.0", "0x10", "1e3", "abc", "３",
	} {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			installFastPathParent(t, rc, ^uint32(0), ^uint32(0), raw, true)
			before := schemaRegressionCounts(t)
			if _, err := beginParentLoad(context.Background(), rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID,
				config.PersistSchemaRegressionEnforce); err == nil {
				t.Fatalf("schema_gen=%q 未按损坏拒绝", raw)
			}
			requireSchemaRegressionDelta(t, before, schemaRegressionKey{"malformed", "enforce"})
		})
	}
}

// 起服预置:四条序列都要从起服起就存在,否则 `increase(...) > 0` 会漏掉第一次倒退。预置只写 0,不改变任何计数。
func TestPrimePersistSchemaRegressionMetricsExposesAllSeries(t *testing.T) {
	before := schemaRegressionCounts(t)
	PrimePersistSchemaRegressionMetrics()
	after := schemaRegressionCounts(t)
	for _, key := range allSchemaRegressionKeys {
		if _, ok := after[key]; !ok {
			t.Errorf("%s{reason=%q,mode=%q} missing after PrimePersistSchemaRegressionMetrics",
				persistSchemaRegressionMetric, key.reason, key.mode)
		}
	}
	requireSchemaRegressionDelta(t, before, schemaRegressionKey{})
}

// C4 排在身份、水位校验之后:父快照本来就会被这两项拒绝时,不能先计一次倒退、也不能把错误归因成 C4。
// 屏障 schema_gen 高于两表戳,同时父快照水位不符或身份不符,两种模式都应返回原来的错误且计数不变。
func TestParentSchemaRegressionCheckRunsAfterIdentityAndWatermark(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(parent *databasepb.PlayerAllData)
		wantErr string
	}{
		{"水位不符", func(parent *databasepb.PlayerAllData) { parent.LastSaveSeq = 98 }, "水位"},
		{"身份不符", func(parent *databasepb.PlayerAllData) { parent.PlayerDatabase_1Data.PlayerId = seedTestPlayerID + 1 }, "身份"},
	} {
		for _, mode := range []config.PersistSchemaRegressionMode{config.PersistSchemaRegressionWarn, config.PersistSchemaRegressionEnforce} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				_, rc := newSeedTestRedis(t)
				installFastPathParent(t, rc, 1, 1, "9", true)
				ctx := context.Background()
				parent := newPlayerAllDataSkeleton(seedTestPlayerID)
				parent.LastSaveSeq = 99
				parent.PlayerDatabaseData.PersistSchemaGen = 1
				parent.PlayerDatabase_1Data.PersistSchemaGen = 1
				tc.mutate(parent)
				raw, err := proto.Marshal(parent)
				if err != nil {
					t.Fatal(err)
				}
				if err := rc.Set(ctx, seedParentKey(seedTestPlayerID), raw, time.Hour).Err(); err != nil {
					t.Fatal(err)
				}
				before := schemaRegressionCounts(t)
				_, err = beginParentLoad(ctx, rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, mode)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("beginParentLoad err = %v, want an error mentioning %q", err, tc.wantErr)
				}
				requireSchemaRegressionDelta(t, before, schemaRegressionKey{})
			})
		}
	}
}

func TestParseBarrierSchemaGenAcceptsOnlyCanonicalUint32(t *testing.T) {
	for raw, want := range map[string]uint32{"1": 1, "2": 2, "10": 10, "4294967295": 4294967295} {
		if got, ok := parseBarrierSchemaGen(raw); !ok || got != want {
			t.Errorf("parseBarrierSchemaGen(%q) = (%d, %v), want (%d, true)", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0", "01", "4294967296", "-1", "+1", " 1", "1 "} {
		if got, ok := parseBarrierSchemaGen(raw); ok {
			t.Errorf("parseBarrierSchemaGen(%q) = (%d, true), want rejected", raw, got)
		}
	}
}

// 冷加载(父快照不存在)不判 C4:屏障 schema_gen 再高或已损坏,enforce 下也照常放行、不计数。
func TestColdLoadSkipsPersistSchemaRegression(t *testing.T) {
	for _, barrierGen := range []string{"9", "abc"} {
		t.Run(barrierGen, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			installCommittedBarrier(t, rc, 2, 99)
			if err := rc.HSet(context.Background(), savebarrier.Key(seedTestPlayerID), "schema_gen", barrierGen).Err(); err != nil {
				t.Fatal(err)
			}
			before := schemaRegressionCounts(t)
			snapshot, err := beginParentLoad(context.Background(), rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID,
				config.PersistSchemaRegressionEnforce)
			if err != nil || snapshot.raw != "" || mr.Exists(seedParentKey(seedTestPlayerID)) {
				t.Fatalf("冷加载被 C4 拦下或写了父键: err=%v raw=%d bytes", err, len(snapshot.raw))
			}
			requireSchemaRegressionDelta(t, before, schemaRegressionKey{})
		})
	}
}

// 冷恢复整条链在 enforce 下同样不判 C4:SQL 行未盖戳、屏障 schema_gen 更高,父键照常组装发布。
// 改写包级 config.AppConfig,本包用例不能加 t.Parallel()。
func TestColdRecoveryUnderEnforceDoesNotJudgeSchemaGen(t *testing.T) {
	previous := config.AppConfig.PersistSchemaRegressionMode
	t.Cleanup(func() { config.AppConfig.PersistSchemaRegressionMode = previous })
	config.AppConfig.PersistSchemaRegressionMode = string(config.PersistSchemaRegressionEnforce)

	_, rc := newSeedTestRedis(t)
	installCommittedBarrier(t, rc, 2, 99)
	if err := rc.HSet(context.Background(), savebarrier.Key(seedTestPlayerID), "schema_gen", "9").Err(); err != nil {
		t.Fatal(err)
	}
	before := schemaRegressionCounts(t)
	reads := &coldSQLReads{read: sqlReadAtVersion(2, 99)}
	calls := 0
	ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		if err != nil {
			t.Error(err)
		}
	})
	parent, _ := readSeedParent(t, rc, seedTestPlayerID)
	if calls != 1 || reads.count != 2 || parent.LastSaveSeq != 99 {
		t.Fatalf("冷恢复未完成: calls=%d reads=%d parent=%v", calls, reads.count, parent)
	}
	requireSchemaRegressionDelta(t, before, schemaRegressionKey{})
}

// EnsurePlayerAllDataInRedisAsync 从 config.AppConfig 取模式、归一化后传给 beginParentLoad:
// 空串 = warn 放行;enforce 拒绝倒退;非法取值不猜模式,即使数据不倒退本次入场也失败。
// 改写包级 config.AppConfig,本包用例不能加 t.Parallel()。
func TestEnsurePlayerAllDataPassesConfiguredRegressionMode(t *testing.T) {
	previous := config.AppConfig.PersistSchemaRegressionMode
	t.Cleanup(func() { config.AppConfig.PersistSchemaRegressionMode = previous })
	for _, tc := range []struct {
		name      string
		raw       string
		regressed bool
		wantErr   bool
	}{
		{"缺省按warn放行倒退", "", true, false},
		{"warn放行倒退", "warn", true, false},
		{"enforce拒绝倒退", "enforce", true, true},
		{"enforce放行不倒退", "enforce", false, false},
		{"非法取值失败", "Enforce", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.AppConfig.PersistSchemaRegressionMode = tc.raw
			_, rc := newSeedTestRedis(t)
			barrierGen := "1"
			if tc.regressed {
				barrierGen = "2"
			}
			installFastPathParent(t, rc, 1, 1, barrierGen, true)
			calls := 0
			// 快路径与失败路径都不碰 dispatcher / Kafka:传 nil,碰了就 panic。
			ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, nil, nil, seedTestPlayerID, 0, func(err error) {
				calls++
				if (err != nil) != tc.wantErr {
					t.Errorf("onComplete err = %v, want error = %v", err, tc.wantErr)
				}
			})
			if calls != 1 {
				t.Fatalf("onComplete calls = %d, want 1", calls)
			}
		})
	}
}

// 新号种子不盖戳(设计 §5.4):骨架两张表的戳都是 0,写进 Redis 的种子也是 0;
// 快路径在 enforce 下照常放行(新号没有屏障 hash,C4 不比较)。
// 反过来,盖了戳的「种子」不再等于骨架,isUnownedLoginSeed 判它不是新号 —— 判断本身不变,
// 这正是种子不能盖戳的原因:login 混版本运行时各 pod 盖的戳不同,新号首次入场会失败(评审 M3)。
func TestNewPlayerSeedStaysUnstamped(t *testing.T) {
	skeleton := newPlayerAllDataSkeleton(seedTestPlayerID)
	if skeleton.GetPlayerDatabaseData().GetPersistSchemaGen() != 0 || skeleton.GetPlayerDatabase_1Data().GetPersistSchemaGen() != 0 {
		t.Fatalf("骨架被盖了戳: player_database=%d player_database_1=%d",
			skeleton.GetPlayerDatabaseData().GetPersistSchemaGen(), skeleton.GetPlayerDatabase_1Data().GetPersistSchemaGen())
	}
	if !isUnownedLoginSeed(skeleton, seedTestPlayerID) {
		t.Fatal("未盖戳的骨架没被当成新号种子")
	}

	ctx := context.Background()
	_, rc := newSeedTestRedis(t)
	if _, err := seedTestNewPlayerAllData(ctx, rc, seedTestPlayerID); err != nil {
		t.Fatal(err)
	}
	seeded, _ := readSeedParent(t, rc, seedTestPlayerID)
	if seeded.GetPlayerDatabaseData().GetPersistSchemaGen() != 0 || seeded.GetPlayerDatabase_1Data().GetPersistSchemaGen() != 0 {
		t.Fatalf("写进 Redis 的种子带了戳: %v", seeded)
	}
	before := schemaRegressionCounts(t)
	snapshot, err := beginParentLoad(ctx, rc, testParentLoadScripts, seedParentKey(seedTestPlayerID), seedTestPlayerID, config.PersistSchemaRegressionEnforce)
	if err != nil || snapshot.raw == "" {
		t.Fatalf("新号种子在 enforce 下未走快路径: err=%v raw=%d bytes", err, len(snapshot.raw))
	}
	requireSchemaRegressionDelta(t, before, schemaRegressionKey{})

	for name, stamp := range map[string]func(*databasepb.PlayerAllData){
		"player_database":   func(p *databasepb.PlayerAllData) { p.PlayerDatabaseData.PersistSchemaGen = 1 },
		"player_database_1": func(p *databasepb.PlayerAllData) { p.PlayerDatabase_1Data.PersistSchemaGen = 1 },
	} {
		stamped := newPlayerAllDataSkeleton(seedTestPlayerID)
		stamp(stamped)
		if isUnownedLoginSeed(stamped, seedTestPlayerID) {
			t.Fatalf("%s 盖了戳的种子仍被当成新号", name)
		}
	}
}

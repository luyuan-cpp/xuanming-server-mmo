package dataloader

import (
	"context"
	"errors"
	"fmt"
	componentpb "proto/common/component"
	databasepb "proto/common/database"
	dbpb "proto/db"
	"shared/ownerepoch"
	"shared/savebarrier"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// miniredis 不实现 INFO；只替换实例身份读取，屏障读、判定和提交仍用正式脚本。
var testParentLoadScripts = newParentLoadScripts(`
local function instance_identity()
    return redis.call('GET', 'test:parent:identity') or 'redis-test', redis.call('GET', 'test:parent:role') or 'master'
end
`)

// 既有回归同样开启身份核验；只有具名兼容开关用例才关闭它。
func ensureTestPlayerAllDataInRedisAsync(ctx context.Context, rc redis.Cmdable, disp readResultRegistrar,
	producer dbTaskProducer, playerID uint64, ttl time.Duration, done func(error)) {
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, disp, producer, playerID, ttl, done)
}

func seedTestNewPlayerAllData(ctx context.Context, rc *redis.Client, playerID uint64) (SeedResult, error) {
	return seedNewPlayerAllData(ctx, rc, testParentLoadScripts, playerID)
}

func setTestParentIdentity(t *testing.T, rc *redis.Client, identity, stamp string) {
	t.Helper()
	ctx := context.Background()
	if err := rc.MSet(ctx, "test:parent:identity", identity, "test:parent:role", "master").Err(); err != nil {
		t.Fatal(err)
	}
	if stamp != "" {
		if err := rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), savebarrier.VerifiedIdentityField, stamp).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func sqlParentAtVersion(epoch, seq uint64) *databasepb.PlayerAllData {
	p := newPlayerAllDataSkeleton(seedTestPlayerID)
	p.PlayerDatabaseData.FenceOwnerEpoch, p.PlayerDatabaseData.FenceSaveSeq = epoch, seq
	p.PlayerDatabase_1Data.FenceOwnerEpoch, p.PlayerDatabase_1Data.FenceSaveSeq = epoch, seq
	return p
}

func TestParentLoadIdentityOrAckChangeAdoptsSQLBeforeFastPath(t *testing.T) {
	for _, trigger := range []string{"identity", "ack-ahead"} {
		t.Run(trigger, func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			ctx := context.Background()
			installCommittedBarrier(t, rc, 2, 99)
			old := sqlParentAtVersion(2, 99)
			old.LastSaveSeq = 99
			old.PlayerDatabaseData.LevelComponent = &componentpb.LevelComp{Level: 10}
			raw, err := proto.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			if err := rc.Set(ctx, seedParentKey(seedTestPlayerID), raw, time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			setTestParentIdentity(t, rc, "redis-new", "redis-old")
			if trigger == "ack-ahead" {
				setTestParentIdentity(t, rc, "redis-new", "redis-new")
				if err := rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "player_database", savebarrier.Version(3, 101), "player_database_1", savebarrier.Version(3, 101)).Err(); err != nil {
					t.Fatal(err)
				}
			}
			read := sqlReadAtVersion(3, 101)
			reads := &coldSQLReads{read: func(task *dbpb.DBTask) ([]byte, error) {
				data, err := read(task)
				if err != nil || task.MsgType != string((&databasepb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName()) {
					return data, err
				}
				row := new(databasepb.PlayerDatabase)
				if err := proto.Unmarshal(data, row); err != nil {
					return nil, err
				}
				row.LevelComponent = &componentpb.LevelComp{Level: 40}
				return proto.Marshal(row)
			}}
			calls := 0
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
				calls++
				if err != nil {
					t.Error(err)
				}
			})
			got, _ := readSeedParent(t, rc, seedTestPlayerID)
			if calls != 1 || reads.count != 2 || got.LastSaveSeq != 101 || got.GetPlayerDatabaseData().GetLevelComponent().GetLevel() != 40 {
				t.Fatalf("未采纳 SQL: calls=%d reads=%d parent=%v", calls, reads.count, got)
			}
			if rc.Get(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID)).Val() != "3" || rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), "pending").Val() != savebarrier.Version(3, 101) || rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), savebarrier.VerifiedIdentityField).Val() != "redis-new" {
				t.Fatal("SQL 采纳后 owner、屏障或身份戳未共同对齐")
			}
			// 已核验后再次加载才允许不碰 SQL 的快路径。
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, nil, nil, seedTestPlayerID, 0, func(err error) {
				calls++
				if err != nil {
					t.Error(err)
				}
			})
			if calls != 2 || reads.count != 2 {
				t.Fatal("核验后的快路径未恰好完成一次")
			}
		})
	}
}

func TestParentLoadVerifiedInflightWaitsWithoutSQLOrWrites(t *testing.T) {
	_, rc := newSeedTestRedis(t)
	ctx := context.Background()
	installCommittedBarrier(t, rc, 2, 99)
	p := sqlParentAtVersion(2, 99)
	p.LastSaveSeq = 100
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(ctx, seedParentKey(seedTestPlayerID), raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "pending", savebarrier.Version(2, 100)).Err(); err != nil {
		t.Fatal(err)
	}
	setTestParentIdentity(t, rc, "redis-live", "redis-live")
	calls := 0
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, nil, nil, seedTestPlayerID, 0, func(err error) {
		calls++
		if !errors.Is(err, errParentBarrierNotCommitted) {
			t.Errorf("在途屏障未等待: %v", err)
		}
	})
	if calls != 1 || rc.Get(ctx, seedParentKey(seedTestPlayerID)).Val() != string(raw) {
		t.Fatal("等待路径改写了父键或重复完成")
	}
}

// 首存前失败是可达活性缺口，但种子或空 SQL 都不是持久新号资格，不能冒险把历史丢失当新号。
// 保留原两个父键形态和全部状态检查，按安全合同都应拒绝；后续活性恢复需要正式 birth 原件。
func TestParentLoadOwnedSeedWithNoSQLHistoryFailsClosed(t *testing.T) {
	for _, parentMissing := range []bool{false, true} {
		t.Run(fmt.Sprintf("parentMissing=%t", parentMissing), func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			ctx := context.Background()
			if _, err := seedTestNewPlayerAllData(ctx, rc, seedTestPlayerID); err != nil {
				t.Fatal(err)
			}
			_, before := readSeedParent(t, rc, seedTestPlayerID)
			if parentMissing {
				mr.Del(seedParentKey(seedTestPlayerID))
			}
			if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "1", 0).Err(); err != nil {
				t.Fatal(err)
			}
			setTestParentIdentity(t, rc, "redis-live", "")
			beforeState := mr.Dump()
			beforeStamp := rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), savebarrier.VerifiedIdentityField).Val()
			reads := &coldSQLReads{read: sqlReadAtVersion(0, 0)}
			calls := 0
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
				calls++
				if err == nil || !strings.Contains(err.Error(), "历史") {
					t.Errorf("无持久新号资格的历史缺口被放行: %v", err)
				}
			})
			after, readErr := rc.Get(ctx, seedParentKey(seedTestPlayerID)).Bytes()
			if parentMissing {
				if !errors.Is(readErr, redis.Nil) {
					t.Fatal("拒绝后凭空建立了父键", readErr)
				}
			} else if readErr != nil || string(after) != string(before) {
				t.Fatal("拒绝后改写了原建角种子", readErr)
			}
			if calls != 1 || reads.count != 2 || mr.Dump() != beforeState {
				t.Fatalf("拒绝未保留完整原状态: calls=%d reads=%d", calls, reads.count)
			}
			if rc.Get(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID)).Val() != "1" || rc.HExists(ctx, savebarrier.Key(seedTestPlayerID), "pending").Val() {
				t.Fatal("拒绝历史缺口时改了 owner 或凭空写了 pending")
			}
			if rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), savebarrier.VerifiedIdentityField).Val() != beforeStamp {
				t.Fatal("拒绝后改写了原身份戳")
			}
		})
	}
}

// 历史缺口里父键不是建角种子(有资产却没有屏障)时，SQL 为空也不能放行，原样拒绝、零写入。
func TestParentLoadHistoryGapWithAssetsStillFailsClosed(t *testing.T) {
	_, rc := newSeedTestRedis(t)
	ctx := context.Background()
	p := newPlayerAllDataSkeleton(seedTestPlayerID)
	p.PlayerDatabaseData.Currency = &componentpb.CurrencyComp{Values: []uint64{1}}
	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(ctx, seedParentKey(seedTestPlayerID), raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	setTestParentIdentity(t, rc, "redis-live", "")
	reads := &coldSQLReads{read: sqlReadAtVersion(0, 0)}
	calls := 0
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		if err == nil || !strings.Contains(err.Error(), "历史") {
			t.Errorf("有资产而无屏障的父快照被放行: %v", err)
		}
	})
	if calls != 1 || rc.Get(ctx, seedParentKey(seedTestPlayerID)).Val() != string(raw) || rc.Exists(ctx, savebarrier.Key(seedTestPlayerID)).Val() != 0 {
		t.Fatal("历史缺口拒绝后写了父键或屏障")
	}
}

// 历史缺口而 SQL 两行有同一个非零围栏:不是「从未存盘」，按 Reconcile 采纳 SQL,不能把种子放行。
func TestParentLoadHistoryGapWithSQLHistoryAdoptsSQL(t *testing.T) {
	_, rc := newSeedTestRedis(t)
	ctx := context.Background()
	if _, err := seedTestNewPlayerAllData(ctx, rc, seedTestPlayerID); err != nil {
		t.Fatal(err)
	}
	if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	setTestParentIdentity(t, rc, "redis-live", "")
	reads := &coldSQLReads{read: sqlReadAtVersion(4, 7)}
	calls := 0
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		if err != nil {
			t.Error(err)
		}
	})
	got, _ := readSeedParent(t, rc, seedTestPlayerID)
	if calls != 1 || got.LastSaveSeq != 7 || got.GetPlayerDatabaseData().GetFenceOwnerEpoch() != 4 {
		t.Fatalf("SQL 有历史时没有采纳 SQL: calls=%d parent=%v", calls, got)
	}
	if rc.Get(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID)).Val() != "4" || rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), "pending").Val() != savebarrier.Version(4, 7) {
		t.Fatal("采纳后 owner 或 pending 未对齐 SQL")
	}
}

func TestParentLoadIdentityVerificationFailurePreservesRawParent(t *testing.T) {
	_, rc := newSeedTestRedis(t)
	ctx := context.Background()
	installFastPathParent(t, rc, 1, 1, "1", true)
	setTestParentIdentity(t, rc, "redis-new", "redis-old")
	before := rc.Get(ctx, seedParentKey(seedTestPlayerID)).Val()
	reads := &coldSQLReads{read: func(*dbpb.DBTask) ([]byte, error) { return nil, fmt.Errorf("injected SQL failure") }}
	calls := 0
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		if err == nil || !strings.Contains(err.Error(), "injected SQL failure") {
			t.Error("SQL 核验失败被伪装成功", err)
		}
	})
	if calls != 1 || reads.count != 2 || rc.Get(ctx, seedParentKey(seedTestPlayerID)).Val() != before || rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), savebarrier.VerifiedIdentityField).Val() != "redis-old" {
		t.Fatal("核验失败改写了父键或旧身份戳")
	}
}

// 兼容开关只略过实例身份检查，不能略过可独立判定的历史缺口与 ACK 回退证据。
func TestParentLoadCompatibilitySwitchKeepsHardEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot parentLoadSnapshot
		want     parentLoadRoute
	}{
		{"未核验且无持久版本", parentLoadSnapshot{raw: "parent"}, parentLoadVerify},
		{"历史缺口", parentLoadSnapshot{raw: "parent", historyGap: errors.New("history")}, parentLoadVerify},
		{"ACK倒退证据", parentLoadSnapshot{raw: "parent", state: savebarrier.BarrierAckAhead}, parentLoadVerify},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := tc.snapshot.route(false); got != tc.want {
				t.Fatalf("兼容路由=%v，期望=%v", got, tc.want)
			}
		})
	}
}

// 建角写种子时同时写上本实例身份戳:实例没换时，新号首次 EnterGame 不因为缺戳去读 SQL
// (new-player-first-enter-no-cold-read.md 的优化保持有效);实例换了才核验。
func TestSeedStampsIdentitySoFirstEnterSkipsSQL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		loginOn     string
		wantSQLRead int
	}{
		{"实例未换走快路径", "redis-live", 0},
		{"实例换了要核验", "redis-new", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			ctx := context.Background()
			setTestParentIdentity(t, rc, "redis-live", "")
			if res, err := seedCommittedNewPlayerAllData(ctx, rc, testParentLoadScripts, savebarrier.InitialPlayerData(seedTestPlayerID)); err != nil || res != SeedWritten {
				t.Fatalf("写种子: %v %v", res, err)
			}
			if got := rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), savebarrier.VerifiedIdentityField).Val(); got != "redis-live" {
				t.Fatalf("种子没有写身份戳: %q", got)
			}
			setTestParentIdentity(t, rc, tc.loginOn, "")
			reads := &coldSQLReads{read: initialCommitSQLRead}
			calls := 0
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
				calls++
				if err != nil {
					t.Error(err)
				}
			})
			if calls != 1 || reads.count != tc.wantSQLRead {
				t.Fatalf("calls=%d SQL 读=%d,期望 %d", calls, reads.count, tc.wantSQLRead)
			}
		})
	}
}

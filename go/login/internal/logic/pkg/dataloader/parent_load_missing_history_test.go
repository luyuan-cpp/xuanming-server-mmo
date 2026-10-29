package dataloader

import (
	"context"
	"fmt"
	dbpb "proto/db"
	"shared/ownerepoch"
	"shared/savebarrier"
	"strings"
	"testing"
)

// owner 已铸造不能证明从未存盘。父键、屏障和 SQL 行同时缺失时没有持久新号原件，必须拒绝。
// SQL 回包 Data 确实为空，而不是人为带 player_id 的零围栏行；仍走完整组装和提交调用面。
func TestParentLoadMissingRedisAndSQLHistoryWithMintedOwnerFailsClosed(t *testing.T) {
	mr, rc := newSeedTestRedis(t)
	ctx := context.Background()
	if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	setTestParentIdentity(t, rc, "redis-live", "")
	if mr.Exists(seedParentKey(seedTestPlayerID)) || mr.Exists(savebarrier.Key(seedTestPlayerID)) {
		t.Fatal("回归前提不是父键与屏障均缺失")
	}
	before := mr.Dump()
	reads := &coldSQLReads{read: func(*dbpb.DBTask) ([]byte, error) { return nil, nil }}
	calls := 0
	var gotErr error
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		gotErr = err
	})
	if calls != 1 || reads.count != 2 {
		t.Fatalf("没有精确完成两张 SQL 空行核验: calls=%d reads=%d", calls, reads.count)
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "历史") {
		t.Errorf("历史全缺不能被推断成新号: %v", gotErr)
	}
	if after := mr.Dump(); after != before {
		t.Fatal("拒绝路径改写了原键，或凭空建立父快照/屏障/身份戳")
	}
}

func TestParentLoadMissingEitherInitialSQLRowNeverPublishesParent(t *testing.T) {
	for _, table := range []string{"PlayerDatabase", "PlayerDatabase_1"} {
		t.Run(table, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			ctx := context.Background()
			if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "3", 0).Err(); err != nil {
				t.Fatal(err)
			}
			setTestParentIdentity(t, rc, "redis-live", "")
			before := mr.Dump()
			initial := savebarrier.InitialPlayerData(seedTestPlayerID)
			missingType := string(initial.PlayerDatabaseData.ProtoReflect().Descriptor().FullName())
			if table == "PlayerDatabase_1" {
				missingType = string(initial.PlayerDatabase_1Data.ProtoReflect().Descriptor().FullName())
			}
			missingReads := 0
			reads := &coldSQLReads{read: func(task *dbpb.DBTask) ([]byte, error) {
				if task.MsgType == missingType {
					missingReads++
					return nil, nil
				}
				return initialCommitSQLRead(task)
			}}
			calls := 0
			var result error
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) { calls++; result = err })
			if missingReads != 1 {
				t.Fatalf("缺行注入前提不成立: type=%q missingReads=%d", missingType, missingReads)
			}
			if calls != 1 || reads.count != 2 || result == nil || mr.Dump() != before {
				t.Fatalf("缺单行仍放行或写键: calls=%d reads=%d error=%v", calls, reads.count, result)
			}
		})
	}
}

func TestLegacyZeroSequenceSeedCannotBypassMissingSQLWithEitherSwitch(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprint(verify), func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			ctx := context.Background()
			setTestParentIdentity(t, rc, "redis-live", "")
			if _, err := seedTestNewPlayerAllData(ctx, rc, seedTestPlayerID); err != nil {
				t.Fatal(err)
			}
			before := mr.Dump()
			reads := &coldSQLReads{read: func(*dbpb.DBTask) ([]byte, error) { return nil, nil }}
			calls := 0
			var result error
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, verify, reads, reads, seedTestPlayerID, 0, func(err error) { calls++; result = err })
			if calls != 1 || reads.count != 2 || result == nil || mr.Dump() != before {
				t.Fatal("旧零序号种子或兼容开关绕过了真实 SQL 缺行")
			}
		})
	}
}

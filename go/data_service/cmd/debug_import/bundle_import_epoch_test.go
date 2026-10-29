package main

import (
	"context"
	"math"
	"strings"
	"testing"

	"data_service/cmd/debugutil"

	"shared/ownerepoch"
	"shared/savebarrier"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// r2-ds#2(2026-10-22)回归:重铸代次要盖过本地一切存盘痕迹;本地痕迹高过本地 owner(Redis 丢过状态)时拒绝导入。
// 修复前 E 只取 max(线上, 本地 owner)+1:本地 Redis 被 FLUSHALL 而 MySQL / Kafka 没清时,代次更大的残留 DBTask
// 冲掉导入的行,之后本地新登录的存盘被判 stale_owner。

func TestAdmitRejectsLocalTraceAheadOfOwner(t *testing.T) {
	bothRows := []string{"player_database", "player_database_1"}
	cases := []struct {
		name  string
		state *localPlayerState
	}{
		// 本地 Redis FLUSHALL 过:owner_epoch 没了,两张行还停在代次 9;Kafka 里可能压着更高代次的残留存盘。
		{"FLUSHALL 后行围栏高于空 owner", &localPlayerState{sqlTables: bothRows, sqlEpoch: 9}},
		// FLUSHALL 之后本地又进过一次场景:owner 从 0 INCR 到 1,仍低于行围栏。
		{"FLUSHALL 后重新登录过", &localPlayerState{sqlTables: bothRows, sqlEpoch: 9, ownerRaw: "1", ownerEpoch: 1,
			redisKeys: []string{"player:{1}:owner_epoch"}}},
		// owner_epoch 键被单删、屏障还在。
		{"屏障令牌高于 owner", &localPlayerState{redisKeys: []string{"player:{1}:save_barrier"}, barrierEpoch: 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.state.admit(true, 1)
			if err == nil {
				t.Fatal("本地存盘痕迹高于 owner 时导入被放行")
			}
			if !strings.Contains(err.Error(), "kafka-offset-reset") {
				t.Fatalf("拒绝信息应提示先清本地环境(含 Kafka 位点): %v", err)
			}
		})
	}

	consistent := &localPlayerState{sqlTables: bothRows, sqlEpoch: 9, ownerRaw: "9", ownerEpoch: 9, barrierEpoch: 9,
		redisKeys: []string{"player:{1}:owner_epoch"}}
	if err := consistent.admit(true, 1); err != nil {
		t.Fatalf("痕迹不超过 owner 的本地存档应可 -replace 覆盖: %v", err)
	}
	// 上一次导入在第 1 段之后中断:owner 已前进到 E,两张行还是旧代次 —— 带 -replace 重跑必须能收敛。
	interrupted := &localPlayerState{sqlTables: bothRows, sqlEpoch: 4, ownerRaw: "8", ownerEpoch: 8, barrierEpoch: 8,
		redisKeys: []string{"player:{1}:owner_epoch"}}
	if err := interrupted.admit(true, 1); err != nil {
		t.Fatalf("中断后的重跑被拒绝: %v", err)
	}
}

func TestReforgedEpochAboveEveryObservedEpoch(t *testing.T) {
	cases := []struct {
		name   string
		online uint64
		state  localPlayerState
		want   uint64
	}{
		{"线上最大", 20, localPlayerState{ownerEpoch: 9, sqlEpoch: 9, barrierEpoch: 9}, 21},
		{"本地 owner 最大", 3, localPlayerState{ownerEpoch: 12, sqlEpoch: 9, barrierEpoch: 11}, 13},
		{"本地行围栏最大", 3, localPlayerState{ownerEpoch: 4, sqlEpoch: 7}, 8},
		{"本地屏障令牌最大", 3, localPlayerState{ownerEpoch: 4, sqlEpoch: 2, barrierEpoch: 7}, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.state.reforgedEpoch(c.online)
			if err != nil || got != c.want {
				t.Fatalf("reforgedEpoch = %d, %v; want %d", got, err, c.want)
			}
		})
	}
	exhausted := localPlayerState{sqlEpoch: math.MaxUint64}
	if _, err := exhausted.reforgedEpoch(0); err == nil {
		t.Fatal("代次耗尽时必须拒绝,不能回绕成 0")
	}
}

// 本地屏障经 ReadRedisKeys(与 inspectLocalPlayer 同一条读路径)读出后,各版本令牌的代次都要算进来。
func TestLocalBarrierEpochReadsEveryVersionToken(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	const withOutbox, missing, malformed, wrongType = 5, 6, 7, 8
	mr.HSet(savebarrier.Key(withOutbox),
		"pending", savebarrier.Version(4, 10),
		"player_database", savebarrier.Version(3, 99),
		"player_database_1", savebarrier.Version(4, 10),
		"outbox_version", savebarrier.Version(6, 1),
		"outbox_topic", "db_task_zone_1")
	mr.HSet(savebarrier.Key(malformed), "pending", "not-a-version")
	mr.Set(savebarrier.Key(wrongType), "x")

	specs := []debugutil.RedisKeySpec{}
	for _, id := range []uint64{withOutbox, missing, malformed, wrongType} {
		specs = append(specs, debugutil.RedisKeySpec{Key: savebarrier.Key(id), Kind: debugutil.RedisKindHash})
	}
	entries, err := debugutil.ReadRedisKeys(context.Background(), client, specs)
	if err != nil {
		t.Fatal(err)
	}

	if epoch, err := localBarrierEpoch(&entries[0]); err != nil || epoch != 6 {
		t.Fatalf("补投意图的代次最大,应取 6: epoch=%d err=%v", epoch, err)
	}
	if epoch, err := localBarrierEpoch(&entries[1]); err != nil || epoch != 0 {
		t.Fatalf("屏障不存在应为 0: epoch=%d err=%v", epoch, err)
	}
	if _, err := localBarrierEpoch(&entries[2]); err == nil {
		t.Fatal("畸形令牌不能当成 0")
	}
	if _, err := localBarrierEpoch(&entries[3]); err == nil {
		t.Fatal("屏障键类型不对不能当成 0")
	}
}

// 2026-10-22 评审补充:inspectLocalPlayer 的 Redis 段(readLocalRedisState)必须把屏障令牌读进 barrierEpoch,
// 交给 admit 与 reforgedEpoch。删掉这一步读取时,本地补投意图高于 owner 的状态会被放行,第一段断言变红。
func TestReadLocalRedisStateFeedsBarrierEpochIntoAdmit(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	const id = 31
	mr.Set(ownerepoch.OwnerEpochKey(id), "3")
	mr.HSet(savebarrier.Key(id),
		"pending", savebarrier.Version(3, 10),
		"player_database", savebarrier.Version(3, 10),
		"player_database_1", savebarrier.Version(3, 10),
		"outbox_version", savebarrier.Version(6, 1))

	// owner_epoch 被单删后又从低位重建过:补投意图的代次 6 高于本地 owner 3,准入必须拒绝。
	ahead := &localPlayerState{}
	if err := ahead.readLocalRedisState(ctx, client, id); err != nil {
		t.Fatal(err)
	}
	if ahead.ownerEpoch != 3 || ahead.barrierEpoch != 6 {
		t.Fatalf("ownerEpoch=%d barrierEpoch=%d, want 3/6", ahead.ownerEpoch, ahead.barrierEpoch)
	}
	if err := ahead.admit(true, 1); err == nil || !strings.Contains(err.Error(), "kafka-offset-reset") {
		t.Fatalf("屏障令牌高于本地 owner 时准入必须拒绝: %v", err)
	}

	// owner 不低于全部屏障令牌:准入通过,重铸代次盖过 owner 与屏障里最大的代次。
	mr.Set(ownerepoch.OwnerEpochKey(id), "6")
	consistent := &localPlayerState{}
	if err := consistent.readLocalRedisState(ctx, client, id); err != nil {
		t.Fatal(err)
	}
	if err := consistent.admit(true, 1); err != nil {
		t.Fatalf("痕迹不超过 owner 的本地存档应可 -replace 覆盖: %v", err)
	}
	if epoch, err := consistent.reforgedEpoch(2); err != nil || epoch != 7 {
		t.Fatalf("reforgedEpoch = %d, %v; want 7", epoch, err)
	}

	// 会话键照样读进来:本地同号玩家会话未释放时一律拒绝。
	mr.Set(debugutil.LocatorSessionKey(id), "session")
	online := &localPlayerState{}
	if err := online.readLocalRedisState(ctx, client, id); err != nil {
		t.Fatal(err)
	}
	if err := online.admit(true, 1); err == nil || !strings.Contains(err.Error(), "在线") {
		t.Fatalf("本地会话未释放时准入必须拒绝: %v", err)
	}
}

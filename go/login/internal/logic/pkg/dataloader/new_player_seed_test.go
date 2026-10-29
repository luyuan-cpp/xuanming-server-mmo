package dataloader

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	comppb "proto/common/component"
	login_proto_database "proto/common/database"
	dbpb "proto/db"
	"shared/ownerepoch"
	"shared/savebarrier"
)

// 只提供已实际提交的两行，不能把空 Data 替换为带 id 的请求体。
func initialCommitSQLRead(task *dbpb.DBTask) ([]byte, error) {
	parent := savebarrier.InitialPlayerData(seedTestPlayerID)
	switch task.MsgType {
	case string(parent.PlayerDatabaseData.ProtoReflect().Descriptor().FullName()):
		return proto.Marshal(parent.PlayerDatabaseData)
	case string(parent.PlayerDatabase_1Data.ProtoReflect().Descriptor().FullName()):
		return proto.Marshal(parent.PlayerDatabase_1Data)
	default:
		return nil, fmt.Errorf("初始提交夹具收到未知 SQL 读类型")
	}
}

func TestCommittedSeedCarriesFullSQLVersionAndFirstEnterRemainsFast(t *testing.T) {
	_, rc := newSeedTestRedis(t)
	ctx := context.Background()
	parent := savebarrier.InitialPlayerData(seedTestPlayerID)
	result, err := seedCommittedNewPlayerAllData(ctx, rc, testParentLoadScripts, parent)
	requireSeedResult(t, result, err, SeedWritten)
	got, _ := readSeedParent(t, rc, seedTestPlayerID)
	if !proto.Equal(got, parent) || rc.Get(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID)).Val() != "1" {
		t.Fatal("初始缓存没有完整 SQL 原件和 owner 地板")
	}
	version := savebarrier.Version(1, 1)
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		if rc.HGet(ctx, savebarrier.Key(seedTestPlayerID), field).Val() != version {
			t.Fatal("初始提交屏障不完整")
		}
	}
	calls := 0
	ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, nil, nil, seedTestPlayerID, 0, func(err error) {
		calls++
		if err != nil {
			t.Error(err)
		}
	})
	if calls != 1 {
		t.Fatal("可信种子没有保留首次进场快路径")
	}
}

func TestCommittedSeedRefusesLateOwnerOrBarrierWithoutChangingKeys(t *testing.T) {
	for _, kind := range []string{"owner", "barrier", "wrongtype", "outbox"} {
		t.Run(kind, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			ctx := context.Background()
			switch kind {
			case "owner":
				_ = rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "3", 0).Err()
			case "barrier":
				_ = rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "pending", savebarrier.Version(2, 99)).Err()
			case "wrongtype":
				_ = rc.Set(ctx, savebarrier.Key(seedTestPlayerID), "bad", 0).Err()
			case "outbox":
				_ = rc.HSet(ctx, savebarrier.Key(seedTestPlayerID), "outbox_retry_ms", "1").Err()
			}
			before := mr.Dump()
			result, _ := seedCommittedNewPlayerAllData(ctx, rc, testParentLoadScripts, savebarrier.InitialPlayerData(seedTestPlayerID))
			if result == SeedWritten || result == SeedIdempotent || mr.Dump() != before {
				t.Fatal("迟到初始种子覆盖或更改了现有权威状态")
			}
		})
	}
}

func TestInitialSQLCommitRecoversFailedFirstLandingWithoutLoweringOwner(t *testing.T) {
	for _, owner := range []string{"", "3"} {
		t.Run("owner="+owner, func(t *testing.T) {
			_, rc := newSeedTestRedis(t)
			ctx := context.Background()
			if owner != "" {
				if err := rc.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), owner, 0).Err(); err != nil {
					t.Fatal(err)
				}
			}
			reads := &coldSQLReads{read: initialCommitSQLRead}
			calls := 0
			ensurePlayerAllDataInRedisAsync(ctx, rc, testParentLoadScripts, true, reads, reads, seedTestPlayerID, 0, func(err error) {
				calls++
				if err != nil {
					t.Error(err)
				}
			})
			got, _ := readSeedParent(t, rc, seedTestPlayerID)
			wantOwner := owner
			if wantOwner == "" {
				wantOwner = "1"
			}
			if calls != 1 || reads.count != 2 || !proto.Equal(got, savebarrier.InitialPlayerData(seedTestPlayerID)) || rc.Get(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID)).Val() != wantOwner {
				t.Fatal("初始提交恢复缺失、不完整或降低已有 owner")
			}
		})
	}
}

// 新号种子(docs/design/new-player-first-enter-no-cold-read.md)的单测。全部跑在 miniredis 上,
// 不需要 Kafka / dispatcher:种子命中后的快路径本来就不该碰它们,传 nil 正好把「碰了」变成 panic。

const seedTestPlayerID = uint64(424242)

// newSeedTestRedis 起一个 miniredis 与连到它的 go-redis 客户端,两者都随用例清理。
func newSeedTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// seedParentKey / seedSubKey 按消息全名拼键,与 scene、backfill、go/db 的拼法相同;不手写字面量,
// 也不复用被测的 buildParentKey,免得键名拼错时测试跟着一起错。
// seedParentKey 是父快照键 <全名>:{<pid>}(玩家域 hash tag);子表缓存键仍是 <全名>:<pid>(第 4 批改名)。
func seedParentKey(playerID uint64) string {
	return fmt.Sprintf("%s:{%d}", (&login_proto_database.PlayerAllData{}).ProtoReflect().Descriptor().FullName(), playerID)
}

func seedSubKey(msg proto.Message, playerID uint64) string {
	return fmt.Sprintf("%s:%d", msg.ProtoReflect().Descriptor().FullName(), playerID)
}

// seedSubMessages 是父键的两张子表在「库里没有这一行」时的内容,也就是冷读请求体本身。
func seedSubMessages(playerID uint64) []proto.Message {
	return []proto.Message{
		&login_proto_database.PlayerDatabase{PlayerId: playerID},
		&login_proto_database.PlayerDatabase_1{PlayerId: playerID},
	}
}

func readSeedParent(t *testing.T, rdb *redis.Client, playerID uint64) (*login_proto_database.PlayerAllData, []byte) {
	t.Helper()
	raw, err := rdb.Get(context.Background(), seedParentKey(playerID)).Bytes()
	if err != nil {
		t.Fatalf("read parent key: %v", err)
	}
	parent := &login_proto_database.PlayerAllData{}
	if err := proto.Unmarshal(raw, parent); err != nil {
		t.Fatalf("unmarshal parent key: %v", err)
	}
	return parent, raw
}

func requireSeedResult(t *testing.T, got SeedResult, err error, want SeedResult) {
	t.Helper()
	if want == SeedError {
		if got != SeedError || err == nil {
			t.Fatalf("SeedNewPlayerAllData = %v / err %v, want error with a non-nil err", got, err)
		}
		return
	}
	if got != want || err != nil {
		t.Fatalf("SeedNewPlayerAllData = %v / err %v, want %v / nil", got, err, want)
	}
}

// 种子 = 骨架(两张子表只带 player_id),不带身份、不带注册时间,TTL 与冷读组装的父键相同。
func TestSeedNewPlayerAllData_WritesSkeletonWithTTL(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()

	got, err := seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedWritten)

	parent, _ := readSeedParent(t, rdb, seedTestPlayerID)
	if !proto.Equal(parent, newPlayerAllDataSkeleton(seedTestPlayerID)) {
		t.Fatalf("seed = %v, want the cold-read skeleton", parent)
	}
	player := parent.GetPlayerDatabaseData()
	if player.GetPlayerId() != seedTestPlayerID || parent.GetPlayerDatabase_1Data().GetPlayerId() != seedTestPlayerID {
		t.Fatalf("seed player ids = %d / %d, want %d on both sub tables",
			player.GetPlayerId(), parent.GetPlayerDatabase_1Data().GetPlayerId(), seedTestPlayerID)
	}
	// 身份归 backfill 写;registration_timestamp 一旦非 0,scene 会跳过首次注册。
	if player.GetProfileComponent() != nil || player.GetUint32PbComponent() != nil {
		t.Fatalf("seed carries identity fields: profile=%v uint32=%v", player.GetProfileComponent(), player.GetUint32PbComponent())
	}
	if ts := player.GetUint64PbComponent().GetRegistrationTimestamp(); ts != 0 {
		t.Fatalf("seed registration_timestamp = %d, want 0 (scene must still run first-registration)", ts)
	}
	if ttl := mr.TTL(seedParentKey(seedTestPlayerID)); ttl <= 0 || ttl > playerAllDataParentTTL {
		t.Fatalf("seed TTL = %v, want (0, %v]", ttl, playerAllDataParentTTL)
	}
}

// go-redis 读超时后会重发已经在服务端执行过的 EVAL:第二次看到的是自己写的种子,必须算成功、不改值。
func TestSeedNewPlayerAllData_RerunIsIdempotent(t *testing.T) {
	_, rdb := newSeedTestRedis(t)
	ctx := context.Background()

	got, err := seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedWritten)
	_, first := readSeedParent(t, rdb, seedTestPlayerID)

	got, err = seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedIdempotent)
	if _, second := readSeedParent(t, rdb, seedTestPlayerID); !bytes.Equal(first, second) {
		t.Fatal("rerun changed the seeded parent key")
	}
}

// 父键已有真实存档(scene 存过盘:有等级、没有 TTL):一个字节都不能动,也不能给它加上 TTL。
func TestSeedNewPlayerAllData_NeverOverwritesExistingParent(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()
	existing, err := proto.Marshal(&login_proto_database.PlayerAllData{
		PlayerDatabaseData: &login_proto_database.PlayerDatabase{
			PlayerId:       seedTestPlayerID,
			LevelComponent: &comppb.LevelComp{Level: 30},
		},
	})
	if err != nil {
		t.Fatalf("marshal existing parent: %v", err)
	}
	if err := rdb.Set(ctx, seedParentKey(seedTestPlayerID), existing, 0).Err(); err != nil {
		t.Fatalf("seed existing parent: %v", err)
	}

	got, err := seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedParentExists)

	if _, after := readSeedParent(t, rdb, seedTestPlayerID); !bytes.Equal(existing, after) {
		t.Fatal("seed overwrote an existing parent key")
	}
	if ttl := mr.TTL(seedParentKey(seedTestPlayerID)); ttl != 0 {
		t.Fatalf("existing parent TTL = %v after the seed attempt, want none", ttl)
	}
}

// owner_epoch 在 = 玩家被派入过场景,scene 的存档才是真相:哪怕父键此刻不在(合服失效缓存),也不许写。
func TestSeedNewPlayerAllData_RefusesOnceOwnerEpochExists(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()
	if err := rdb.Set(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID), "3", 0).Err(); err != nil {
		t.Fatalf("seed owner_epoch: %v", err)
	}

	got, err := seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedOwnerEpochExists)

	if mr.Exists(seedParentKey(seedTestPlayerID)) {
		t.Fatal("seed written although player:{id}:owner_epoch exists")
	}
}

// 零 id、未接线的客户端:直接报错,不写任何键。
func TestSeedNewPlayerAllData_RejectsZeroIDAndNilClient(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()

	got, err := seedTestNewPlayerAllData(ctx, rdb, 0)
	requireSeedResult(t, got, err, SeedError)
	got, err = seedTestNewPlayerAllData(ctx, nil, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedError)

	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("rejected seeds left keys behind: %v", keys)
	}
}

// Redis 不可用:结论是 SeedError(建角侧据此退回冷读),不留下半个种子。
func TestSeedNewPlayerAllData_RedisDownIsError(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()
	// 先把连接建好,让注入的错误只打在种子脚本上,而不是握手上:结论一样,但失败形态更确定。
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	mr.SetError("simulated outage")
	t.Cleanup(func() { mr.SetError("") })

	got, err := seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedError)

	mr.SetError("")
	if mr.Exists(seedParentKey(seedTestPlayerID)) {
		t.Fatal("parent key exists after a failed seed")
	}
}

// 已提交种子与两张真实初始 SQL 行组装结果相同；旧子缓存不能替代 SQL 原件。
func TestCommittedSeedEqualsInitialSQLAssembly(t *testing.T) {
	ctx := context.Background()

	mr1, rdb1 := newSeedTestRedis(t)
	got, err := seedCommittedNewPlayerAllData(ctx, rdb1, testParentLoadScripts, savebarrier.InitialPlayerData(seedTestPlayerID))
	requireSeedResult(t, got, err, SeedWritten)

	mr2, rdb2 := newSeedTestRedis(t)
	for _, sub := range seedSubMessages(seedTestPlayerID) {
		raw, err := proto.Marshal(sub)
		if err != nil {
			t.Fatalf("marshal sub cache: %v", err)
		}
		if err := rdb2.Set(ctx, seedSubKey(sub, seedTestPlayerID), raw, time.Hour).Err(); err != nil {
			t.Fatalf("seed sub cache: %v", err)
		}
	}
	calls := 0
	var gotErr error
	reads := &coldSQLReads{read: initialCommitSQLRead}
	ensureTestPlayerAllDataInRedisAsync(ctx, rdb2, reads, reads, seedTestPlayerID, 0, func(err error) {
		calls++
		gotErr = err
	})
	if calls != 1 || gotErr != nil {
		t.Fatalf("cold-read assembly onComplete calls = %d, err = %v; want 1 / nil", calls, gotErr)
	}
	if reads.count != 2 {
		t.Fatalf("冷读使用了子缓存: SQL reads=%d", reads.count)
	}

	seeded, _ := readSeedParent(t, rdb1, seedTestPlayerID)
	assembled, _ := readSeedParent(t, rdb2, seedTestPlayerID)
	if !proto.Equal(seeded, assembled) {
		t.Fatalf("seed %v != cold-read assembly %v", seeded, assembled)
	}
	if ttl := mr1.TTL(seedParentKey(seedTestPlayerID)); ttl <= 0 {
		t.Fatalf("seed TTL = %v, want > 0", ttl)
	}
	if ttl := mr2.TTL(seedParentKey(seedTestPlayerID)); ttl <= 0 {
		t.Fatalf("cold-read assembled parent TTL = %v, want > 0", ttl)
	}
}

// 种子命中后的首次入场:EXISTS 快路径,回调同步跑且只跑一次;不发读任务(没有子表缓存回写、
// 没有 task:result 键)。disp / producer 为 nil,快路径之外的任何分支都会 panic。
func TestEnsurePlayerAllData_SeededParentTakesFastPath(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()
	got, err := seedCommittedNewPlayerAllData(ctx, rdb, testParentLoadScripts, savebarrier.InitialPlayerData(seedTestPlayerID))
	requireSeedResult(t, got, err, SeedWritten)

	calls := 0
	var gotErr error
	ensureTestPlayerAllDataInRedisAsync(ctx, rdb, nil, nil, seedTestPlayerID, 0, func(err error) {
		calls++
		gotErr = err
	})
	if calls != 1 || gotErr != nil {
		t.Fatalf("onComplete calls = %d, err = %v; want exactly one synchronous nil", calls, gotErr)
	}

	for _, sub := range seedSubMessages(seedTestPlayerID) {
		if key := seedSubKey(sub, seedTestPlayerID); mr.Exists(key) {
			t.Fatalf("sub cache %s exists: the fast path must not issue DB reads", key)
		}
	}
	for _, key := range mr.Keys() {
		if strings.HasPrefix(key, "task:result:") {
			t.Fatalf("found %s: the fast path must not register DB tasks", key)
		}
	}
	if parent, _ := readSeedParent(t, rdb, seedTestPlayerID); !proto.Equal(parent, savebarrier.InitialPlayerData(seedTestPlayerID)) {
		t.Fatalf("parent after the fast path = %v, want the untouched seed", parent)
	}
}

// 建角 6b' 的只读探测(设计 §10 P7):四种结论,且一个键都不改。键名独立拼(seedParentKey / ownerepoch),
// 不复用被测的 newPlayerStateKeys。两把都在时归 owner_epoch_exists,与种子脚本先判 owner_epoch 的顺序一致。
func TestProbeMintedPlayerState_Verdicts(t *testing.T) {
	plantParent := func(t *testing.T, rdb *redis.Client) {
		t.Helper()
		if err := rdb.Set(context.Background(), seedParentKey(seedTestPlayerID), "existing", 0).Err(); err != nil {
			t.Fatalf("plant parent key: %v", err)
		}
	}
	plantOwnerEpoch := func(t *testing.T, rdb *redis.Client) {
		t.Helper()
		if err := rdb.Set(context.Background(), ownerepoch.OwnerEpochKey(seedTestPlayerID), "3", 0).Err(); err != nil {
			t.Fatalf("plant owner_epoch: %v", err)
		}
	}
	cases := []struct {
		name  string
		plant []func(*testing.T, *redis.Client)
		want  MintedIDProbe
		label string
	}{
		{"两把键都不在", nil, MintedIDClear, "clear"},
		{"父键已在", []func(*testing.T, *redis.Client){plantParent}, MintedIDParentExists, "parent_exists"},
		{"owner_epoch 已在", []func(*testing.T, *redis.Client){plantOwnerEpoch}, MintedIDOwnerEpochExists, "owner_epoch_exists"},
		{"两把都在", []func(*testing.T, *redis.Client){plantParent, plantOwnerEpoch}, MintedIDOwnerEpochExists, "owner_epoch_exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr, rdb := newSeedTestRedis(t)
			for _, plant := range tc.plant {
				plant(t, rdb)
			}
			keysBefore := mr.Keys()

			got, err := ProbeMintedPlayerState(context.Background(), rdb, seedTestPlayerID)

			if got != tc.want || err != nil {
				t.Fatalf("ProbeMintedPlayerState = %v / err %v, want %v / nil", got, err, tc.want)
			}
			// label 值是告警表达式的契约,单独钉住字面量。
			if got.String() != tc.label {
				t.Fatalf("label = %q, want %q", got.String(), tc.label)
			}
			if keysAfter := mr.Keys(); !slices.Equal(keysBefore, keysAfter) {
				t.Fatalf("keys %v -> %v: the probe must not write", keysBefore, keysAfter)
			}
			if ttl := mr.TTL(seedParentKey(seedTestPlayerID)); ttl != 0 {
				t.Fatalf("parent TTL = %v after the probe, want none", ttl)
			}
		})
	}
}

// 探测与种子判的是同一组键:种子写下之后,探测必须看见它(否则探测放行的 id 只能在种子那一步才被拦下)。
func TestProbeMintedPlayerState_SeesTheSeedsParentKey(t *testing.T) {
	_, rdb := newSeedTestRedis(t)
	ctx := context.Background()
	got, err := seedTestNewPlayerAllData(ctx, rdb, seedTestPlayerID)
	requireSeedResult(t, got, err, SeedWritten)

	probe, err := ProbeMintedPlayerState(ctx, rdb, seedTestPlayerID)
	if probe != MintedIDParentExists || err != nil {
		t.Fatalf("ProbeMintedPlayerState after a seed = %v / err %v, want parent_exists / nil", probe, err)
	}
}

// 探不清的三种来源:零 id、未接线的客户端、Redis 不可用。结论一律 error 且带 err(建角据此 fail-closed),不写任何键。
func TestProbeMintedPlayerState_ErrorsAreNotClear(t *testing.T) {
	mr, rdb := newSeedTestRedis(t)
	ctx := context.Background()

	if got, err := ProbeMintedPlayerState(ctx, rdb, 0); got != MintedIDProbeError || err == nil {
		t.Fatalf("zero id: ProbeMintedPlayerState = %v / err %v, want error with a non-nil err", got, err)
	}
	if got, err := ProbeMintedPlayerState(ctx, nil, seedTestPlayerID); got != MintedIDProbeError || err == nil {
		t.Fatalf("nil client: ProbeMintedPlayerState = %v / err %v, want error with a non-nil err", got, err)
	}

	// 先把连接建好,让注入的错误只打在探测上,而不是握手上(同 TestSeedNewPlayerAllData_RedisDownIsError)。
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	mr.SetError("simulated outage")
	t.Cleanup(func() { mr.SetError("") })
	if got, err := ProbeMintedPlayerState(ctx, rdb, seedTestPlayerID); got != MintedIDProbeError || err == nil {
		t.Fatalf("redis down: ProbeMintedPlayerState = %v / err %v, want error with a non-nil err", got, err)
	}
	if got := MintedIDProbeError.String(); got != "error" {
		t.Fatalf("label = %q, want error", got)
	}

	mr.SetError("")
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("failed probes left keys behind: %v", keys)
	}
}

package sessionmanager

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 登录代次铸造的回归用例(docs/design/scene-enter-session-fence.md §3.2 / §10「铸造」)。
//
// 时间一律显式注入(mintNow 固定在 2025-10 前后的毫秒值),不读墙钟;miniredis 的 TTL 只在
// FastForward 时流逝,同样与墙钟无关。

const mintPlayerID = uint64(7001)

// mintNow 是一个真实量级的毫秒时间戳(约 1.76e12,远小于 2^53),让"时间下限"分支与线上同量级。
var mintNow = time.UnixMilli(1_760_000_000_000)

func newMintHarness(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func mustMint(t *testing.T, rdb *redis.Client, sessionFloor uint64, now time.Time) uint64 {
	t.Helper()
	gen, err := MintLoginGeneration(context.Background(), rdb, mintPlayerID, sessionFloor, now)
	if err != nil {
		t.Fatalf("MintLoginGeneration(floor=%d now=%d): %v", sessionFloor, now.UnixMilli(), err)
	}
	return gen
}

// 键名是跨进程约定(设计文档 §3.2):纯十进制 player_id,不带前导零、不带 hash tag。
func TestMintLoginGeneration_KeyName(t *testing.T) {
	if got := LoginGenerationKey(mintPlayerID); got != "player:{7001}:login_generation" {
		t.Fatalf("LoginGenerationKey = %q, want player:{7001}:login_generation", got)
	}
}

// 首次铸造(键不存在、无会话记录):取当前毫秒,且写回键里的就是返回值。
func TestMintLoginGeneration_FirstMintIsAtLeastNowMillis(t *testing.T) {
	rdb, mr := newMintHarness(t)

	gen := mustMint(t, rdb, 0, mintNow)
	if gen < uint64(mintNow.UnixMilli()) {
		t.Fatalf("first generation = %d, want >= now millis %d (time floor)", gen, mintNow.UnixMilli())
	}
	stored, err := mr.Get(LoginGenerationKey(mintPlayerID))
	if err != nil {
		t.Fatalf("read minted key: %v", err)
	}
	if stored != strconv.FormatUint(gen, 10) {
		t.Fatalf("stored value = %q, want %d in plain decimal", stored, gen)
	}
}

// 同一毫秒内连续登录(多 login 实例共享同一把 Redis):计数部分保证严格递增,不撞号(G2)。
func TestMintLoginGeneration_StrictlyIncreasing(t *testing.T) {
	rdb, _ := newMintHarness(t)

	prev := mustMint(t, rdb, 0, mintNow)
	for i := 0; i < 5; i++ {
		next := mustMint(t, rdb, 0, mintNow)
		if next <= prev {
			t.Fatalf("mint #%d = %d, want > previous %d", i+2, next, prev)
		}
		prev = next
	}
}

// 本机时钟落后于已存值(另一个 login 实例时钟偏快铸过):取已存值 + 1,不回退。
func TestMintLoginGeneration_ClockBehindStoredValue(t *testing.T) {
	rdb, mr := newMintHarness(t)
	stored := uint64(mintNow.UnixMilli()) + 5_000
	if err := mr.Set(LoginGenerationKey(mintPlayerID), strconv.FormatUint(stored, 10)); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	if gen := mustMint(t, rdb, 0, mintNow); gen != stored+1 {
		t.Fatalf("generation = %d, want stored+1 = %d", gen, stored+1)
	}
}

// 会话记录里的代次比键与时钟都大(键丢过、记录还在):取记录 + 1,保证提交点的严格递增校验能过(§3.4)。
func TestMintLoginGeneration_SessionFloorWins(t *testing.T) {
	rdb, _ := newMintHarness(t)
	floor := uint64(mintNow.UnixMilli()) + 10_000

	if gen := mustMint(t, rdb, floor, mintNow); gen != floor+1 {
		t.Fatalf("generation = %d, want sessionFloor+1 = %d", gen, floor+1)
	}
}

// 键带 7 天 TTL,且每次铸造都续满:"7 天无登录"才过期,活跃玩家的键不会中途消失。
func TestMintLoginGeneration_KeyHasSevenDayTTL(t *testing.T) {
	rdb, mr := newMintHarness(t)
	const want = 7 * 24 * time.Hour
	key := LoginGenerationKey(mintPlayerID)

	mustMint(t, rdb, 0, mintNow)
	if ttl := mr.TTL(key); ttl != want {
		t.Fatalf("TTL after first mint = %v, want %v", ttl, want)
	}

	mr.FastForward(24 * time.Hour)
	mustMint(t, rdb, 0, mintNow.Add(24*time.Hour))
	if ttl := mr.TTL(key); ttl != want {
		t.Fatalf("TTL after second mint = %v, want refreshed to %v", ttl, want)
	}
}

// 键丢失(FLUSHALL / 主从切换丢写 / TTL 到期)后再铸,仍大于丢失前发出的值 —— 纯 INCR 会从 1 重来,
// scene 上残留的实体就会一直拒新登录(§3.2「为什么带时间下限」)。两种下限分别验证:
//   - 记录下限:同一毫秒内重铸,只靠"会话记录代次 + 1"也必须更大;
//   - 时间下限:会话记录也没了(登出删会话),时间往前走就必须更大。
func TestMintLoginGeneration_KeyLossDoesNotRollBack(t *testing.T) {
	t.Run("session record floor at the same millisecond", func(t *testing.T) {
		rdb, mr := newMintHarness(t)
		var before uint64
		for i := 0; i < 3; i++ {
			before = mustMint(t, rdb, before, mintNow)
		}
		mr.FlushAll()

		if after := mustMint(t, rdb, before, mintNow); after <= before {
			t.Fatalf("after key loss = %d, want > %d (session record floor)", after, before)
		}
	})

	t.Run("time floor without session record", func(t *testing.T) {
		rdb, mr := newMintHarness(t)
		var before uint64
		for i := 0; i < 3; i++ {
			before = mustMint(t, rdb, 0, mintNow)
		}
		mr.FlushAll()

		if after := mustMint(t, rdb, 0, mintNow.Add(time.Second)); after <= before {
			t.Fatalf("after key loss = %d, want > %d (time floor)", after, before)
		}
	})
}

// Redis 出错 = 铸造失败,返回错误、不返回 0 让调用方带着继续(§3.2「失败处理」)。
func TestMintLoginGeneration_RedisErrorFails(t *testing.T) {
	rdb, mr := newMintHarness(t)
	// 用不可重试的 ERR 前缀:LOADING / READONLY 之类会触发 go-redis 的退避重试,只会拖慢用例。
	mr.SetError("ERR injected redis failure")

	gen, err := MintLoginGeneration(context.Background(), rdb, mintPlayerID, 0, mintNow)
	if err == nil {
		t.Fatalf("want an error when Redis fails, got generation %d", gen)
	}
	if gen != 0 {
		t.Fatalf("generation on error = %d, want 0", gen)
	}
}

// 接缝上的输入校验:没接 Redis、player_id 为 0、没给时间(会丢掉时间下限)一律报错,且不写键。
func TestMintLoginGeneration_RejectsInvalidInput(t *testing.T) {
	rdb, mr := newMintHarness(t)
	ctx := context.Background()

	if _, err := MintLoginGeneration(ctx, nil, mintPlayerID, 0, mintNow); err == nil {
		t.Error("nil redis client: want error")
	}
	if _, err := MintLoginGeneration(ctx, rdb, 0, 0, mintNow); err == nil {
		t.Error("player_id 0: want error")
	}
	if _, err := MintLoginGeneration(ctx, rdb, mintPlayerID, 0, time.Time{}); err == nil {
		t.Error("zero now: want error (the time floor would silently disappear)")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("keys written on rejected input: %v", keys)
	}
}

// 镜像结构与 proto 之间的代次映射:NewOnlineSession 写入、toProto 带出、fromProto 读回。
// fromProto 漏了 → 下一次铸造的下限恒为 0;toProto 漏了 → 会话记录恒为"无代次"。两处都是静默的。
func TestLoginGeneration_SessionMirrorRoundTrip(t *testing.T) {
	const gen = uint64(1_760_000_000_321)
	existing := &PlayerSession{PlayerID: mintPlayerID, SessionVersion: 4, LoginGeneration: gen - 5}

	session := NewOnlineSession(existing, OnlineSessionInput{
		PlayerID:        mintPlayerID,
		SessionID:       9,
		Now:             mintNow,
		LoginGeneration: gen,
	})
	if session.LoginGeneration != gen {
		t.Fatalf("NewOnlineSession LoginGeneration = %d, want the input %d (not the existing record's)", session.LoginGeneration, gen)
	}

	pb := toProto(session)
	if pb.GetLoginGeneration() != gen {
		t.Fatalf("toProto login_generation = %d, want %d", pb.GetLoginGeneration(), gen)
	}
	if back := fromProto(pb); back.LoginGeneration != gen {
		t.Fatalf("fromProto LoginGeneration = %d, want %d", back.LoginGeneration, gen)
	}
}

// 脚本应答的解析:解析失败、0、不大于会话记录代次(双精度丢位等)都必须当失败,不得带着继续。
func TestParseMintedLoginGeneration(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		floor   uint64
		want    uint64
		wantErr string
	}{
		{name: "plain decimal", raw: "1760000000001", floor: 0, want: 1760000000001},
		{name: "above floor", raw: "42", floor: 41, want: 42},
		{name: "not a number", raw: "1.76e+12", wantErr: "unparsable"},
		{name: "empty", raw: "", wantErr: "unparsable"},
		{name: "zero", raw: "0", wantErr: "zero"},
		{name: "equal to floor", raw: "41", floor: 41, wantErr: "not above"},
		{name: "below floor", raw: "40", floor: 41, wantErr: "not above"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMintedLoginGeneration(tc.raw, tc.floor)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error mentioning %q, got generation %d", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("generation = %d, want %d", got, tc.want)
			}
		})
	}
}

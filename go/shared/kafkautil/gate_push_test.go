package kafkautil

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
)

// broadcastTargetVectors 是 testdata/broadcast_target_vectors.json 的结构。
//
// 语言中立:C++ 侧的 BroadcastTargetVectors.GateDecodesGoEncodedPairs 读同一份文件,用 expect 构造
// BroadcastToPlayersEvent,经 gate 的 broadcast_targets::ForEach 解码,断言得到的 (session, player) 对
// 等于 targets 按 session 稳定升序、同一 session 保留首个之后的结果;Go 侧断言编码器逐字段产出 expect。
// 两端都过 = Go 生产者编进去的每一个 player_id,gate 都会拿去核对它对应的那个 session。
type broadcastTargetVectors struct {
	Schema  string                  `json:"schema"`
	Vectors []broadcastTargetVector `json:"vectors"`
}

type broadcastTargetVector struct {
	Name    string `json:"name"`
	Targets []struct {
		SessionID uint32 `json:"session_id"`
		PlayerID  uint64 `json:"player_id"`
	} `json:"targets"`
	Expect struct {
		SessionList       []uint32 `json:"session_list"`
		SessionBitmapBase uint32   `json:"session_bitmap_base"`
		SessionBitmapHex  string   `json:"session_bitmap_hex"`
		PlayerList        []uint64 `json:"player_list"`
	} `json:"expect"`
}

func (v broadcastTargetVector) targets() []BroadcastTarget {
	out := make([]BroadcastTarget, len(v.Targets))
	for i, target := range v.Targets {
		out[i] = BroadcastTarget{SessionID: target.SessionID, PlayerID: target.PlayerID}
	}
	return out
}

func loadBroadcastTargetVectors(t *testing.T) []broadcastTargetVector {
	t.Helper()
	data, err := os.ReadFile("testdata/broadcast_target_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var file broadcastTargetVectors
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("vector file has no vectors")
	}
	return file.Vectors
}

// gateDecodeOrder 按 gate 的解码契约(broadcast_target_codec.h 的 ForEach)列出 session 的投递顺序:
// bitmap 非空走 bitmap(按字节序、字节内低位在前逐个置位),否则按 session_list 顺序。
// player_list[i] 对应这里的第 i 个 session。
func gateDecodeOrder(sessionList []uint32, bitmapBase uint32, bitmap []byte) []uint32 {
	if len(bitmap) == 0 {
		return slices.Clone(sessionList)
	}
	var sessions []uint32
	for i, b := range bitmap {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) != 0 {
				sessions = append(sessions, bitmapBase+uint32(i*8+bit))
			}
		}
	}
	return sessions
}

// pairTargets 把解码出的 session 与 player_list 逐项配对。长度不齐时 gate 整条拒绝,这里直接判失败。
func pairTargets(t *testing.T, sessions []uint32, players []uint64) []BroadcastTarget {
	t.Helper()
	if len(players) != len(sessions) {
		t.Fatalf("player_list has %d entries for %d targets; gate rejects a misaligned list", len(players), len(sessions))
	}
	out := make([]BroadcastTarget, len(sessions))
	for i := range sessions {
		out[i] = BroadcastTarget{SessionID: sessions[i], PlayerID: players[i]}
	}
	return out
}

// sortedUniqueTargets 是顺序契约的直接陈述:按 session 稳定升序,同一 session 保留输入里的第一个。
// 刻意用 sort.SliceStable 加显式去重来写,不与实现共用 slices.SortStableFunc / CompactFunc。
func sortedUniqueTargets(targets []BroadcastTarget) []BroadcastTarget {
	ordered := append([]BroadcastTarget(nil), targets...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].SessionID < ordered[j].SessionID })
	var out []BroadcastTarget
	for _, target := range ordered {
		if len(out) > 0 && out[len(out)-1].SessionID == target.SessionID {
			continue
		}
		out = append(out, target)
	}
	return out
}

// TestEncodeBroadcastTargetsMatchesSharedVectors 是与 C++ gate 的对拍本体:编码器对每条向量的
// 四个目标字段都必须逐字段等于 expect。不一致时保留 -v 全文回报,以机器结果为准,不直接改表。
func TestEncodeBroadcastTargetsMatchesSharedVectors(t *testing.T) {
	vectors := loadBroadcastTargetVectors(t)
	var sawBitmap, sawSparseList, sawDuplicate bool
	for _, v := range vectors {
		sawBitmap = sawBitmap || v.Expect.SessionBitmapHex != ""
		sawSparseList = sawSparseList || (v.Expect.SessionBitmapHex == "" && len(v.Expect.SessionList) >= BitmapThreshold)
		sawDuplicate = sawDuplicate || len(v.Expect.PlayerList) < len(v.Targets)

		t.Run(v.Name, func(t *testing.T) {
			targets := v.targets()
			input := slices.Clone(targets)

			sessionList, bitmapBase, bitmap, playerList := EncodeBroadcastTargets(targets)

			if !slices.Equal(sessionList, v.Expect.SessionList) {
				t.Errorf("session_list = %v, want %v", sessionList, v.Expect.SessionList)
			}
			if bitmapBase != v.Expect.SessionBitmapBase {
				t.Errorf("session_bitmap_base = %d, want %d", bitmapBase, v.Expect.SessionBitmapBase)
			}
			if got := hex.EncodeToString(bitmap); got != v.Expect.SessionBitmapHex {
				t.Errorf("session_bitmap = %q, want %q", got, v.Expect.SessionBitmapHex)
			}
			if !slices.Equal(playerList, v.Expect.PlayerList) {
				t.Errorf("player_list = %v, want %v", playerList, v.Expect.PlayerList)
			}
			if !slices.Equal(targets, input) {
				t.Error("EncodeBroadcastTargets mutated its input slice")
			}
		})
	}
	// 向量文件自身的覆盖面:bitmap 形态、≥ 阈值仍退回 list 的稀疏形态、含重复 session,缺一样就红。
	if !sawBitmap || !sawSparseList || !sawDuplicate {
		t.Fatalf("vector file lost coverage: bitmap=%v sparse_list=%v duplicate_session=%v", sawBitmap, sawSparseList, sawDuplicate)
	}
}

// TestBroadcastTargetVectorsDecodeToSortedTargets 校验向量文件本身:按 gate 的解码契约还原 expect,
// 必须正好是 targets 稳定升序去重之后的 (session, player) 对 —— 这正是 C++ 侧用例的断言。
// 向量写错时先在这里红,而不是让两端对着一份错表各自变绿。
func TestBroadcastTargetVectorsDecodeToSortedTargets(t *testing.T) {
	for _, v := range loadBroadcastTargetVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			usesBitmap := v.Expect.SessionBitmapHex != ""
			if usesBitmap == (len(v.Expect.SessionList) > 0) {
				t.Fatalf("expect must use exactly one form: session_list=%v session_bitmap_hex=%q",
					v.Expect.SessionList, v.Expect.SessionBitmapHex)
			}
			if !usesBitmap && v.Expect.SessionBitmapBase != 0 {
				t.Fatalf("list form must leave session_bitmap_base 0, got %d", v.Expect.SessionBitmapBase)
			}
			if v.Expect.SessionBitmapHex != strings.ToLower(v.Expect.SessionBitmapHex) {
				t.Fatalf("session_bitmap_hex must be lowercase: %q", v.Expect.SessionBitmapHex)
			}
			bitmap, err := hex.DecodeString(v.Expect.SessionBitmapHex)
			if err != nil {
				t.Fatalf("decode session_bitmap_hex: %v", err)
			}

			got := pairTargets(t, gateDecodeOrder(v.Expect.SessionList, v.Expect.SessionBitmapBase, bitmap), v.Expect.PlayerList)
			if want := sortedUniqueTargets(v.targets()); !slices.Equal(got, want) {
				t.Errorf("decoded pairs = %v, want %v", got, want)
			}
		})
	}
}

// TestEncodeBroadcastTargetsFormBoundaries 钉住形态选择的边界(与 C++ Encode 同口径):阈值按去重后的
// 目标数算;bitmap 只在「字节数 + 6 < 目标数 × 3」时选用;跨度按 64 位算,不因 uint32 回绕选出空 bitmap。
// 每种输入都必须能按 gate 契约还原成稳定升序去重之后的目标。
func TestEncodeBroadcastTargetsFormBoundaries(t *testing.T) {
	const sessionBase = uint32(5 << 17) // gate node 5 的会话段
	dense := func(n int) []BroadcastTarget {
		out := make([]BroadcastTarget, n)
		for i := range out {
			// player_id 与 session 反向,独立排序 player_list 的错误实现会被发现。
			out[i] = BroadcastTarget{SessionID: sessionBase + uint32(i), PlayerID: uint64(10_000 - i)}
		}
		return out
	}
	fullSpan := []BroadcastTarget{{SessionID: math.MaxUint32, PlayerID: 1}}
	for s := uint32(0); s < BitmapThreshold-1; s++ {
		fullSpan = append(fullSpan, BroadcastTarget{SessionID: s, PlayerID: 100 + uint64(s)})
	}

	cases := []struct {
		name        string
		targets     []BroadcastTarget
		bitmapBytes int // 0 = 期望 list 形态
	}{
		{"31 dense targets stay below the threshold", dense(31), 0},
		{"32 dense targets reach the threshold", dense(32), 4},
		{"32 inputs with one duplicate are 31 targets", append(dense(31), BroadcastTarget{SessionID: sessionBase, PlayerID: 9}), 0},
		{"33 inputs with one duplicate are 32 targets", append(dense(32), BroadcastTarget{SessionID: sessionBase + 5, PlayerID: 9}), 4},
		{"span 712 keeps bitmap (89+6 < 96)", append(dense(31), BroadcastTarget{SessionID: sessionBase + 711, PlayerID: 9}), 89},
		{"span 713 falls back to list (90+6 = 96)", append(dense(31), BroadcastTarget{SessionID: sessionBase + 712, PlayerID: 9}), 0},
		{"full uint32 span falls back to list", fullSpan, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionList, bitmapBase, bitmap, playerList := EncodeBroadcastTargets(tc.targets)

			if len(bitmap) != tc.bitmapBytes {
				t.Fatalf("bitmap has %d bytes, want %d (0 = list form)", len(bitmap), tc.bitmapBytes)
			}
			if tc.bitmapBytes > 0 && len(sessionList) != 0 {
				t.Errorf("bitmap form must leave session_list empty, got %d entries", len(sessionList))
			}
			if tc.bitmapBytes == 0 && (bitmapBase != 0 || bitmap != nil) {
				t.Errorf("list form must leave the bitmap fields zero, got base=%d bitmap=%x", bitmapBase, bitmap)
			}
			got := pairTargets(t, gateDecodeOrder(sessionList, bitmapBase, bitmap), playerList)
			if want := sortedUniqueTargets(tc.targets); !slices.Equal(got, want) {
				t.Errorf("decoded pairs = %v, want %v", got, want)
			}
		})
	}

	if sessionList, bitmapBase, bitmap, playerList := EncodeBroadcastTargets(nil); sessionList != nil || bitmapBase != 0 || bitmap != nil || playerList != nil {
		t.Errorf("empty input = (%v, %d, %x, %v), want all zero values", sessionList, bitmapBase, bitmap, playerList)
	}
}

// errStopBeforeKafka 让 recordingBuilder 在写 Kafka 之前停下:*kafkago.Writer 是具体类型,单测里起不出来。
var errStopBeforeKafka = errors.New("stop before kafka")

// recordingBuilder 记下共享收口交给 builder 的参数;stop 非 nil 时每个 Build* 都返回它。
type recordingBuilder struct {
	stop           error
	pushPlayerIDs  []uint64
	broadcastCalls [][]BroadcastTarget
}

func (b *recordingBuilder) BuildPushCommand(_ uint32, _ uint32, playerID uint64, _ string, _ uint32, _ []byte) ([]byte, error) {
	b.pushPlayerIDs = append(b.pushPlayerIDs, playerID)
	return nil, b.stop
}

func (b *recordingBuilder) BuildBroadcastCommand(_ uint32, targets []BroadcastTarget, _ string, _ uint32, _ []byte) ([]byte, error) {
	b.broadcastCalls = append(b.broadcastCalls, slices.Clone(targets))
	return nil, b.stop
}

func (b *recordingBuilder) BuildBroadcastToSceneCommand(uint32, uint64, string, uint32, []byte) ([]byte, error) {
	return nil, b.stop
}

func (b *recordingBuilder) BuildBroadcastToAllCommand(uint32, string, uint32, []byte) ([]byte, error) {
	return nil, b.stop
}

// TestPushToPlayerRejectsZeroPlayerID:player_id 为 0 的推送必须在构造命令之前被拒 ——
// 0 在 gate 侧是「不校验」的兼容位,放出去等于关掉身份栅栏。
func TestPushToPlayerRejectsZeroPlayerID(t *testing.T) {
	builder := &recordingBuilder{stop: errStopBeforeKafka}
	info := PlayerGateInfo{SessionID: 42, GateID: "5", GateInstanceID: "gate-5-uuid"}

	err := PushToPlayer(context.Background(), nil, builder, info, 1, []byte("body"))
	if err == nil || !strings.Contains(err.Error(), "zero player_id") {
		t.Fatalf("PushToPlayer(player 0) error = %v, want a zero player_id rejection", err)
	}
	if len(builder.pushPlayerIDs) != 0 {
		t.Fatalf("builder called %d times for a zero player_id push", len(builder.pushPlayerIDs))
	}

	// 对照:非 0 的 player_id 原样交给 builder(随后停在写 Kafka 之前)。
	info.PlayerID = 1001
	if err := PushToPlayer(context.Background(), nil, builder, info, 1, []byte("body")); !errors.Is(err, errStopBeforeKafka) {
		t.Fatalf("PushToPlayer(player 1001) error = %v, want the builder stop sentinel", err)
	}
	if !slices.Equal(builder.pushPlayerIDs, []uint64{1001}) {
		t.Fatalf("builder received player ids %v, want [1001]", builder.pushPlayerIDs)
	}
}

// TestBroadcastToPlayersDropsZeroPlayerID:player_id 为 0 的目标逐个丢弃并报错,其余照常交给 builder。
func TestBroadcastToPlayersDropsZeroPlayerID(t *testing.T) {
	t.Run("all invalid writes nothing", func(t *testing.T) {
		// writer 传 nil、builder 不设停止哨兵:全部无效时只要走到写 Kafka 就会 nil 解引用 panic。
		builder := &recordingBuilder{}
		players := []PlayerGateInfo{
			{SessionID: 11, GateID: "5", GateInstanceID: "gate-5-uuid"},
			{SessionID: 12, GateID: "5", GateInstanceID: "gate-5-uuid"},
		}

		err := BroadcastToPlayers(context.Background(), nil, builder, players, 1, []byte("body"))
		if err == nil || !strings.Contains(err.Error(), "zero player_id") {
			t.Fatalf("BroadcastToPlayers error = %v, want a zero player_id rejection", err)
		}
		if len(builder.broadcastCalls) != 0 {
			t.Fatalf("builder called %d times although every target was invalid", len(builder.broadcastCalls))
		}
	})

	t.Run("valid targets still go out", func(t *testing.T) {
		builder := &recordingBuilder{stop: errStopBeforeKafka}
		players := []PlayerGateInfo{
			{PlayerID: 1001, SessionID: 11, GateID: "5", GateInstanceID: "gate-5-uuid"},
			{SessionID: 12, GateID: "5", GateInstanceID: "gate-5-uuid"},
			{PlayerID: 1003, SessionID: 13, GateID: "5", GateInstanceID: "gate-5-uuid"},
		}

		err := BroadcastToPlayers(context.Background(), nil, builder, players, 1, []byte("body"))
		// 首个错误是丢弃 0 号目标那条:它在分组阶段就记下,早于 builder 的停止哨兵。
		if err == nil || !strings.Contains(err.Error(), "zero player_id") {
			t.Fatalf("BroadcastToPlayers error = %v, want the zero player_id drop reported first", err)
		}
		want := []BroadcastTarget{{SessionID: 11, PlayerID: 1001}, {SessionID: 13, PlayerID: 1003}}
		if len(builder.broadcastCalls) != 1 || !slices.Equal(builder.broadcastCalls[0], want) {
			t.Fatalf("builder received %v, want exactly one call with %v", builder.broadcastCalls, want)
		}
	})
}

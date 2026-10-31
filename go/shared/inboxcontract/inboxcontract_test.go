package inboxcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	inboxpb "proto/player_inbox"
)

// TestContentSHA256Golden 用固定输入与固定十六进制钉住摘要布局(持久合同:收件箱 content_sha256 列与 Match 的回执核对)。
// 期望值是对同一串字节分别用 sha256sum 与 .NET SHA256 两个独立实现算出的:
//   - 00 00 00 04 | 01 02 03 04 05 06 07 08 | 08 01  (Battle 类型、每字节不同的签发时刻,钉住两段都是 big-endian)
//   - 00 00 00 01 | 00 00 00 00 00 00 03 e8          (空载荷只哈希 12 字节头)
func TestContentSHA256Golden(t *testing.T) {
	cases := []struct {
		name    string
		msgType uint32
		issued  uint64
		payload []byte
		want    string
	}{
		{"Battle类型+全字节签发时刻+两字节载荷", uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT), 0x0102030405060708, []byte{0x08, 0x01},
			"f719478459441aff1901145cc52f3d68fb76c81781022620b7e9f988e45ec43b"},
		{"空载荷", uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN), 1000, nil,
			"071fb8d284782f437b2e09b7a933ce5fd614252c2e0c16abfd5be3ad6454e193"},
	}
	for _, tc := range cases {
		if got := hex.EncodeToString(ContentSHA256(tc.msgType, tc.issued, tc.payload)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	if !bytes.Equal(ContentSHA256(1, 1000, nil), ContentSHA256(1, 1000, []byte{})) {
		t.Error("nil 与空载荷的摘要必须相同(收件箱把 nil 载荷按空字节写库)")
	}
}

// TestContentSHA256MatchesExplicitLayout 与「12 字节 big-endian 头 + 载荷原字节」逐字节对照:这正是收件箱 decide.go
// 与 Match completion.go 两处旧实现的写法,合并成一个来源之后结果不能变。
func TestContentSHA256MatchesExplicitLayout(t *testing.T) {
	msgType := uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT)
	issued := uint64(1_793_000_000_123)
	payload := []byte("battle-settlement-original")
	layout := []byte{
		byte(msgType >> 24), byte(msgType >> 16), byte(msgType >> 8), byte(msgType),
		byte(issued >> 56), byte(issued >> 48), byte(issued >> 40), byte(issued >> 32),
		byte(issued >> 24), byte(issued >> 16), byte(issued >> 8), byte(issued),
	}
	want := sha256.Sum256(append(layout, payload...))
	if got := ContentSHA256(msgType, issued, payload); !bytes.Equal(got, want[:]) {
		t.Fatalf("摘要与显式布局不符: got %x, want %x", got, want)
	}
	for name, other := range map[string][]byte{
		"类型不同":   ContentSHA256(msgType+1, issued, payload),
		"签发时刻不同": ContentSHA256(msgType, issued+1, payload),
		"载荷不同":   ContentSHA256(msgType, issued, append([]byte(nil), payload[1:]...)),
	} {
		if bytes.Equal(other, want[:]) {
			t.Errorf("%s:摘要相同", name)
		}
	}
}

// TestMaxPayloadBytesReadsProtoOption:上限只从 proto 选项读;Battle 结算是 16KiB(Match 的 COMPLETE 准入、
// 收件箱的原件校验与 scene 执行器共用这一个数)。未登记的值返回 false,调用方据此拒绝。
func TestMaxPayloadBytesReadsProtoOption(t *testing.T) {
	for typ, want := range map[inboxpb.PlayerInboxMessageType]int{
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN:   64,
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN: 64,
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT:  1024,
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT:        16 * 1024,
	} {
		if got, ok := MaxPayloadBytes(typ); !ok || got != want {
			t.Errorf("%v: got (%d, %v), want (%d, true)", typ, got, ok, want)
		}
	}
	for _, typ := range []inboxpb.PlayerInboxMessageType{
		inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_UNSPECIFIED,
		inboxpb.PlayerInboxMessageType(99),
	} {
		if got, ok := MaxPayloadBytes(typ); ok || got != 0 {
			t.Errorf("%v 没有规格,应返回 (0, false),got (%d, %v)", typ, got, ok)
		}
	}
}

// TestBattlePayloadBoundary 钉住调用方的判定口径 len(payload) > max:16384 字节在界内,16385 越界。
// 两个调用方(Match validateTerminalPayload、收件箱 validateBattleOriginal)另有各自的边界用例。
func TestBattlePayloadBoundary(t *testing.T) {
	limit, ok := MaxPayloadBytes(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT)
	if !ok {
		t.Fatal("Battle 结算必须有载荷上限")
	}
	if within := 16384; within > limit {
		t.Fatalf("16384 字节应在界内,上限 %d", limit)
	}
	if over := 16385; !(over > limit) {
		t.Fatalf("16385 字节应越界,上限 %d", limit)
	}
}

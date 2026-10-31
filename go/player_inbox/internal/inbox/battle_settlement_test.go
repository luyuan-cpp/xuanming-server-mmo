package inbox

// Battle 结算域的纯逻辑用例(2026-10-30,评审 B-1 / B-3):运维提示不能把值班引向被拒绝的作废,
// 载荷上限与内容摘要只认 shared/inboxcontract 这一个来源。

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"player_inbox/internal/constants"

	battlepb "proto/battle"
	inboxpb "proto/player_inbox"
	"shared/inboxcontract"

	"google.golang.org/protobuf/proto"
)

// TestDeliveryWarnAdviceNeverSuggestsVoidForBattle:Battle 结算域的 Void 一律被拒、Ack 只认 APPLIED,
// 「投递多次未确认」的 ERROR 不能再建议 VoidItem,要指向 runbook 的 Battle 卡住小节;GM 域仍建议隔离。
func TestDeliveryWarnAdviceNeverSuggestsVoidForBattle(t *testing.T) {
	battle := deliveryWarnAdvice(inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT)
	if strings.Contains(battle, "consider VoidItem") || !strings.Contains(battle, "section 6.1") ||
		!strings.Contains(battle, "never edit SQL") {
		t.Fatalf("Battle 域提示不对: %q", battle)
	}
	gm := deliveryWarnAdvice(inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION)
	if !strings.Contains(gm, "VoidItem(QUARANTINED)") {
		t.Fatalf("GM 域仍应建议隔离: %q", gm)
	}
}

// battleSettlementOfSize 造一份确定性序列化后恰好 size 字节的合法 Battle 原件:每项 {item_table_id:1,count:1}
// 的掉落占 6 字节;余下不足 6 字节的部分把前几项的 count 改成 128(varint 1 字节变 2 字节,整项多 1 字节)逐字节补齐。
func battleSettlementOfSize(t *testing.T, playerID, battleID uint64, size int) []byte {
	t.Helper()
	p := &battlepb.BattleSettlementData{PlayerId: playerID, BattleId: battleID, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN}
	marshal := func() []byte {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := len(marshal())
	entries, gap := (size-base)/6, (size-base)%6
	if entries < gap {
		t.Fatalf("size=%d 太小,补不齐", size)
	}
	for i := 0; i < entries; i++ {
		count := uint64(1)
		if i < gap {
			count = 128
		}
		p.ItemsGained = append(p.ItemsGained, &battlepb.BattleItemEntry{ItemTableId: 1, Count: count})
	}
	b := marshal()
	if len(b) != size {
		t.Fatalf("构造的原件 %d 字节,want %d", len(b), size)
	}
	return b
}

// TestBattlePayloadBoundaryUsesProtoLimit:Battle 原件的上限只来自 proto 类型规格(16KiB):
// 恰好 16384 字节通过,16385 字节在 Match 专用入口按永久原件冲突拒绝、在注册表按超大拒绝。
func TestBattlePayloadBoundaryUsesProtoLimit(t *testing.T) {
	battleType := inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT
	limit, ok := inboxcontract.MaxPayloadBytes(battleType)
	if !ok || limit != 16384 {
		t.Fatalf("Battle 载荷上限 = (%d, %v), want (16384, true)", limit, ok)
	}
	original := func(payload []byte) *inboxpb.BattleSettlementInboxRequest {
		return &inboxpb.BattleSettlementInboxRequest{PlayerId: 11, BattleId: 21, ExpectedOwnerAccount: "original",
			ExpectedOwnerRevision: 3, IssuedAtMs: 7, SettlementPayload: payload}
	}
	within, over := battleSettlementOfSize(t, 11, 21, limit), battleSettlementOfSize(t, 11, 21, limit+1)
	if err := validateBattleOriginal(original(within)); err != nil {
		t.Fatalf("恰好 %d 字节应通过: %v", limit, err)
	}
	if err := validateBattleOriginal(original(over)); !errors.Is(err, ErrBattleSettlementOriginal) {
		t.Fatalf("%d 字节应按永久原件冲突拒绝: %v", limit+1, err)
	}
	r := mustRegistry(t)
	allowed := map[inboxpb.PlayerInboxMessageType]struct{}{battleType: {}}
	if _, code := r.ValidatePayload(battleType, allowed, within); code != 0 {
		t.Fatalf("注册表对 %d 字节应放行,got %d", limit, code)
	}
	if _, code := r.ValidatePayload(battleType, allowed, over); code != constants.ErrPayloadTooLarge {
		t.Fatalf("注册表对 %d 字节应回 ErrPayloadTooLarge,got %d", limit+1, code)
	}
}

// TestContentSHA256UsesSharedContract:收件箱的摘要入口与 shared/inboxcontract 的 golden 值逐字节相同
// (同一串输入在 inboxcontract_test.go 里钉住);库里存量条目的判重语义不因合并来源而变化。
func TestContentSHA256UsesSharedContract(t *testing.T) {
	got := hex.EncodeToString(ContentSHA256(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT, 0x0102030405060708, []byte{0x08, 0x01}))
	if want := "f719478459441aff1901145cc52f3d68fb76c81781022620b7e9f988e45ec43b"; got != want {
		t.Fatalf("ContentSHA256 = %s, want %s", got, want)
	}
}

package playerredis

import (
	"context"
	"errors"
	"testing"
)

// 单实例没有 slot:SlotKeys 不假装列出一个 slot 的键,调用方必须改用 ScanEachMaster。
func TestSlotKeysRejectsStandaloneAndBadArguments(t *testing.T) {
	ctx := context.Background()
	_, c := newStandalone(t)
	if _, err := SlotKeys(ctx, c, 7, 1000); !errors.Is(err, ErrUnsupportedClient) {
		t.Fatalf("单实例应返回 ErrUnsupportedClient,得到 %v", err)
	}
	if _, err := SlotKeys(ctx, c, 16384, 1000); err == nil {
		t.Fatal("slot 越界应报错")
	}
	if _, err := SlotKeys(ctx, c, 7, slotKeysSlack); err == nil {
		t.Fatal("limit 不大于余量时一次列举放不下任何键,应报错")
	}
}

// 持有判定只认 myself 行自己的 slot 段:不在段里、或节点上有迁移标记,都不能当作「这一刻持有」。
func TestSlotOwnershipRanges(t *testing.T) {
	nodes := "67ed 127.0.0.1:30002@31002 myself,master - 0 1 2 connected 5461-10922 12000\n"
	self, err := parseMyselfLine(nodes)
	if err != nil {
		t.Fatalf("parseMyselfLine: %v", err)
	}
	for _, slot := range []uint16{5461, 8000, 10922, 12000} {
		if !rangesContain(self.ranges, slot) {
			t.Errorf("slot %d 在 %v 里", slot, self.ranges)
		}
	}
	for _, slot := range []uint16{0, 5460, 10923, 11999, 12001, 16383} {
		if rangesContain(self.ranges, slot) {
			t.Errorf("slot %d 不在 %v 里", slot, self.ranges)
		}
	}
}

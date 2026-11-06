package inbox

// 物品获取封禁运维查询(item_gain_blocks.go)的纯逻辑单测。读库那一半在 store_integration_test.go
// 的 TestIntegrationItemGainBlocksListsPendingItemSanctions(-tags integration + 真库)。

import (
	"context"
	"strings"
	"testing"

	"player_inbox/internal/constants"

	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx/logtest"
)

// 两种物品获取处置类型取自注册表、同在 GM 域:封禁与解封按同一条流的 seq 定先后(设计 §2.2)。
func TestItemGainSanctionDomainIsTheGmDomain(t *testing.T) {
	s := &Store{registry: mustRegistry(t)}
	if d, err := s.itemGainSanctionDomain(); err != nil || d != gmDomain {
		t.Fatalf("itemGainSanctionDomain = %v, %v; want %v", d, err, gmDomain)
	}
	if _, err := (&Store{registry: &Registry{}}).itemGainSanctionDomain(); err == nil {
		t.Fatal("注册表不认识这两种类型时必须报错,不能默认成某个域")
	}
}

// player_id 为 0 在碰库与读存档之前就拒绝;没装配读取器是装配缺陷,回错误(适配层折成 StoreUnavailable)。
func TestItemGainBlocksRejectsBeforeTouchingStores(t *testing.T) {
	s := &Store{registry: mustRegistry(t)}
	resp, err := s.ItemGainBlocks(context.Background(), 0, nil)
	if err != nil || resp.GetErrorMessage().GetId() != constants.ErrInvalidParameter {
		t.Fatalf("player_id=0 = %+v, %v; want kInvalidParameter", resp, err)
	}
	if _, err := s.ItemGainBlocks(context.Background(), 42, nil); err == nil {
		t.Fatal("没有封禁表读取器必须报错,不能回一个空表(会被读成「没封」)")
	}
}

// 载荷解出配置号,条目各列原样带出;解不开或配置号为 0 的(入箱校验过,只可能是数据损坏)记 0、照样列出并打 ERROR。
func TestItemGainPendingViews(t *testing.T) {
	logs := logtest.NewCollector(t)
	rows := []*inboxpb.PlayerInboxItemRow{
		{
			Domain:        uint32(gmDomain),
			Seq:           4,
			MsgType:       uint32(gmBlockItem),
			RequestId:     11,
			Payload:       marshal(t, &inboxpb.GmItemGainPayload{ItemConfigId: 30045}),
			Directive:     uint32(inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_VOID),
			DeliveryCount: 2,
			CreatedMs:     1000,
			OperatorName:  "ops_a",
			Note:          "工单 #1",
		},
		{Domain: uint32(gmDomain), Seq: 6, MsgType: uint32(gmUnblockItem), RequestId: 12, Payload: []byte{0xff}},
		{Domain: uint32(gmDomain), Seq: 7, MsgType: uint32(gmBlockItem), RequestId: 13},
	}
	views := itemGainPendingViews(context.Background(), 42, rows)
	if len(views) != len(rows) {
		t.Fatalf("应逐行列出 %d 条,got %d", len(rows), len(views))
	}
	first := views[0]
	if first.GetSeq() != 4 || first.GetMsgType() != gmBlockItem || first.GetItemConfigId() != 30045 ||
		first.GetRequestId() != 11 || first.GetDirective() != inboxpb.PlayerInboxDirective_PLAYER_INBOX_DIRECTIVE_VOID ||
		first.GetDeliveryCount() != 2 || first.GetCreatedMs() != 1000 || first.GetOperatorName() != "ops_a" || first.GetNote() != "工单 #1" {
		t.Fatalf("第 1 条 = %+v", first)
	}
	for _, v := range views[1:] {
		if v.GetItemConfigId() != 0 || v.GetSeq() == 0 || v.GetRequestId() == 0 {
			t.Fatalf("坏载荷应记配置号 0 并保留其余列: %+v", v)
		}
	}
	if out := logs.String(); strings.Count(out, "undecodable payload") != 2 {
		t.Fatalf("两条坏载荷各打一条 ERROR:\n%s", out)
	}
}

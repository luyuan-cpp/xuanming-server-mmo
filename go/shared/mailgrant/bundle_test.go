package mailgrant

import (
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	basepb "proto/common/base"
	mailpb "proto/mail"
	"testing"
)

func TestMailGrantRoundTripAndRejectsExistingInstances(t *testing.T) {
	in := &mailpb.MailAttachments{Currencies: []*mailpb.MailCurrency{{CurrencyType: 0, Amount: 1}, {CurrencyType: 2, Amount: 1 << 40}}, Items: []*mailpb.MailItem{{ConfigId: 42, Count: 2}}}
	bundle, err := Bundle(in, func(id uint32) bool { return id == 42 })
	if err != nil {
		t.Fatal(err)
	}
	out, err := View(bundle)
	if err != nil || !proto.Equal(in, out) {
		t.Fatalf("常规附件转换失败: %v", err)
	}
	for _, bad := range []*assetpb.AssetBundle{{ItemUuids: []uint64{123}}, {PetId: 123}, {ItemInstances: []*assetpb.AssetItemInstance{{}}}, {PetInstance: &assetpb.AssetPetInstance{}}} {
		if _, err = View(bad); err == nil {
			t.Fatal("已有实例不得转换为空的新奖励")
		}
	}
}
func TestMailGrantRejectsMalformedBeforeDelivery(t *testing.T) {
	unknownCurrency := uint32(len(basepb.CurrencyType_name))
	for _, bad := range []*mailpb.MailAttachments{{Currencies: []*mailpb.MailCurrency{{CurrencyType: unknownCurrency, Amount: 1}}}, {Currencies: []*mailpb.MailCurrency{{Amount: 0}}}, {Currencies: []*mailpb.MailCurrency{{Amount: 1}, {Amount: 2}}}, {Items: []*mailpb.MailItem{{ConfigId: 42, Count: 1}}}} {
		if _, err := Bundle(bad, func(uint32) bool { return false }); err == nil {
			t.Fatal("坏附件不能进入可靠意图")
		}
	}
}

func TestMailGrantTotalItemBudgetMatchesScene(t *testing.T) {
	for _, in := range []*mailpb.MailAttachments{{Items: []*mailpb.MailItem{{ConfigId: 1, Count: 4097}}}, {Items: []*mailpb.MailItem{{ConfigId: 1, Count: 4096}, {ConfigId: 2, Count: 1}}}} {
		if _, err := Bundle(in, func(uint32) bool { return true }); err == nil {
			t.Fatal("总授予数越过scene上限仍进入奖励意图")
		}
	}
	if _, err := Bundle(&mailpb.MailAttachments{Items: []*mailpb.MailItem{{ConfigId: 1, Count: 4096}}}, func(uint32) bool { return true }); err != nil {
		t.Fatal(err)
	}
}

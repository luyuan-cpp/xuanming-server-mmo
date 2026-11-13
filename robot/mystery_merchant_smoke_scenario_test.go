package main

// merchant-smoke 里纯取值 / 对账函数的单测:不连服务器,只喂构造出来的应答。约定同 equip_forge_smoke_scenario_test.go:
// 冒烟要等整套服务起来才跑得到,断言函数若写反了(该红不红)联机时看不出来,所以每个函数都要有
// 「合法形状放行 / 每种坏形状各自被拦」两面。字符串字面量一律 ASCII。

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"proto/common/component"
	"proto/scene"
)

const (
	merchantTestRound    uint64 = 1700000000
	merchantTestGoods    uint32 = 7
	merchantTestCurrency uint32 = 1
	merchantTestItem     uint32 = 3001
)

func merchantTestQuote() merchantQuote {
	return merchantQuote{roundId: merchantTestRound, goodsId: merchantTestGoods, price: 800,
		currencyType: merchantTestCurrency, itemConfigId: merchantTestItem, itemCount: 2}
}

// merchantTestReply 造一份成功的购买应答:在商人那条线、货架上目标已买 bought 份(限购 5)、旁边还有一件别的商品;
// 背包里有 items 个目标物品和一件无关物品;货币 0 号槽是别的币种,1 号槽是本次用的货币。
func merchantTestReply(balance uint64, items, bought uint32) *scene.BuyMysteryMerchantGoodsResponse {
	return &scene.BuyMysteryMerchantGoodsResponse{
		Info: &scene.MysteryMerchantInfo{
			Active: true, RoundId: merchantTestRound, Here: true,
			Goods: []*scene.MysteryMerchantGoodsInfo{
				{GoodsId: 9, BoughtCount: 1, LimitCount: 5},
				{GoodsId: merchantTestGoods, BoughtCount: bought, LimitCount: 5},
			},
		},
		Bag: &scene.BagInfo{
			Currency: &component.CurrencyComp{Values: []uint64{42, balance}},
			Items: []*scene.BagItemInfo{
				{ItemId: 1, ConfigId: merchantTestItem, Count: items},
				{ItemId: 2, ConfigId: 1, Count: 3},
			},
		},
	}
}

// merchantTestReplayOf 按契约演一次重放:与 last 同一份背包与货架,标 replayed。
func merchantTestReplayOf(last *scene.BuyMysteryMerchantGoodsResponse) *scene.BuyMysteryMerchantGoodsResponse {
	replay := proto.Clone(last).(*scene.BuyMysteryMerchantGoodsResponse)
	replay.Replayed = true
	return replay
}

func merchantTestGoodsOf(reply *scene.BuyMysteryMerchantGoodsResponse) *scene.MysteryMerchantGoodsInfo {
	return merchantSmokeFindGoods(reply.GetInfo(), merchantTestGoods)
}

// 余额按币种取槽位,槽位不在(数组短、currency 或整个背包缺失)时报「没有」而不是当成 0;物品数按 config 汇总多堆。
func TestMerchantSmokeBalanceAndItemTotalReadTheSnapshot(t *testing.T) {
	reply := merchantTestReply(99200, 2, 1)
	if balance, ok := merchantSmokeBalance(reply.GetBag(), merchantTestCurrency); !ok || balance != 99200 {
		t.Fatalf("balance = %d, %t; want 99200, true", balance, ok)
	}
	if balance, ok := merchantSmokeBalance(reply.GetBag(), 0); !ok || balance != 42 {
		t.Fatalf("other currency = %d, %t; want 42, true", balance, ok)
	}
	for _, test := range []struct {
		name string
		bag  *scene.BagInfo
	}{
		{"slot beyond the values", &scene.BagInfo{Currency: &component.CurrencyComp{Values: []uint64{42}}}},
		{"no currency", &scene.BagInfo{}},
		{"no bag", nil},
	} {
		if _, ok := merchantSmokeBalance(test.bag, merchantTestCurrency); ok {
			t.Fatalf("%s: a missing balance slot read as present", test.name)
		}
	}
	split := merchantTestReply(99200, 2, 1)
	split.Bag.Items = append(split.Bag.Items, &scene.BagItemInfo{ItemId: 3, ConfigId: merchantTestItem, Count: 5})
	if total := merchantSmokeItemTotal(split.GetBag(), merchantTestItem); total != 7 {
		t.Fatalf("item total over two stacks = %d, want 7", total)
	}
	if total := merchantSmokeItemTotal(split.GetBag(), 4242); total != 0 {
		t.Fatalf("item total of an absent config = %d, want 0", total)
	}
	if total := merchantSmokeItemTotal(nil, merchantTestItem); total != 0 {
		t.Fatalf("item total without a bag = %d, want 0", total)
	}
}

// 重放的合法形状:同一条连接上紧跟着重发;重登后不在商人那条线(看不到货架);已换轮(货架是新一轮的);
// 物品换了堆法但总数不变。
func TestMerchantSmokeCheckReplayAcceptsTheFirstResult(t *testing.T) {
	quote := merchantTestQuote()
	for _, test := range []struct {
		name   string
		mutate func(replay *scene.BuyMysteryMerchantGoodsResponse)
	}{
		{"resent on the same connection", func(replay *scene.BuyMysteryMerchantGoodsResponse) {}},
		{"relogged on another channel", func(replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Info.Here, replay.Info.Goods = false, nil
		}},
		{"round changed since", func(replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Info.RoundId++
			merchantTestGoodsOf(replay).BoughtCount = 0
		}},
		{"stacks regrouped", func(replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Bag.Items[0].Count = 1
			replay.Bag.Items = append(replay.Bag.Items, &scene.BagItemInfo{ItemId: 3, ConfigId: merchantTestItem, Count: 1})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			last := merchantTestReply(99200, 2, 1)
			replay := merchantTestReplayOf(last)
			test.mutate(replay)
			if err := merchantSmokeCheckReplay(last, replay, quote.roundId, quote); err != nil {
				t.Fatalf("legal replay rejected: %v", err)
			}
		})
	}
}

// 重放的每种坏形状各自被拦。基线:上一份成功应答 99200 银两、背包里 2 个目标物品、已购 1 份。
func TestMerchantSmokeCheckReplayRejectsEveryBrokenShape(t *testing.T) {
	quote := merchantTestQuote()
	for _, test := range []struct {
		name   string
		mutate func(last, replay *scene.BuyMysteryMerchantGoodsResponse)
	}{
		{"not marked replayed", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) { replay.Replayed = false }},
		{"charged again", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Bag.Currency.Values[merchantTestCurrency] -= quote.price
		}},
		{"granted again", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Bag.Items[0].Count += quote.itemCount
		}},
		{"limit slot taken again", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) {
			merchantTestGoodsOf(replay).BoughtCount++
		}},
		{"goods missing from the shelf", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Info.Goods = replay.Info.Goods[:1]
		}},
		{"replayed reply without a bag", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) { replay.Bag = nil }},
		{"replayed reply without the balance slot", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) {
			replay.Bag.Currency.Values = replay.Bag.Currency.Values[:1]
		}},
		{"last reply without the balance slot", func(last, replay *scene.BuyMysteryMerchantGoodsResponse) {
			last.Bag.Currency = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			last := merchantTestReply(99200, 2, 1)
			replay := merchantTestReplayOf(last)
			test.mutate(last, replay)
			if err := merchantSmokeCheckReplay(last, replay, quote.roundId, quote); err == nil {
				t.Fatal("broken replay accepted")
			}
		})
	}
}

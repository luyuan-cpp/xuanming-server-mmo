package main

// equip-forge-smoke 里纯取值 / 对账函数的单测:不连服务器,只喂构造出来的快照。
// 约定同 equip_smoke_scenario_test.go:冒烟要等整套服务起来才跑得到,断言函数若写反了(该红不红)联机时看不出来,
// 所以每个函数都要有「合法形状放行 / 每种坏形状各自被拦」两面。字符串字面量一律 ASCII。
// 夹具里的强化数值(每级 +500 万分比、金币 1000、强化石 2 个、宝石 +30)是测试自定的,与真表无关;
// 只有 equipForgeWeaponBaseDamage 等顶部常量是冒烟镜像的表值。

import (
	"math"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"proto/common/component"
	"proto/scene"
)

const (
	equipForgeTestTargetId uint64 = 11
	equipForgeTestStoneId  uint64 = 21
	equipForgeTestGemId    uint64 = 22
	equipForgeTestFillerId uint64 = 23

	equipForgeTestCostGold      uint64 = 1000
	equipForgeTestMaterialCount uint32 = 2
	equipForgeTestGemValue      uint64 = 30
)

// 测试自定的强化表:第 N 级的总加成 = N × 500 万分比。
func equipForgeTestBonus(level uint32) uint32 {
	return level * 500
}

// equipForgeTestWeapon 造一把自洽的 +level 的 1101:基础属性按测试强化表放大,未满级带下一级预览(必成),两个空孔。
func equipForgeTestWeapon(itemId uint64, level uint32) *scene.BagItemInfo {
	item := equipTestWeapon(itemId)
	item.EnhanceLevel = level
	item.EnhanceMaxLevel = equipForgeMaxEnhanceLevel
	item.EnhanceBonusPermyriad = equipForgeTestBonus(level)
	line := item.BaseAttrs[0]
	line.Value = equipForgeScale(equipForgeWeaponBaseDamage, equipForgeTestBonus(level))
	line.EnhanceBonus = line.Value - equipForgeWeaponBaseDamage
	if level < equipForgeMaxEnhanceLevel {
		item.EnhanceNext = &scene.EquipEnhancePreview{
			NextLevel: level + 1, SuccessRate: equipForgePermyriadBase, CostGold: equipForgeTestCostGold,
			MaterialConfigId: equipForgeStone, MaterialName: "stone", MaterialCount: equipForgeTestMaterialCount,
			BonusPermyriad: equipForgeTestBonus(level + 1),
		}
		line.EnhanceNextValue = equipForgeScale(equipForgeWeaponBaseDamage, equipForgeTestBonus(level+1))
	}
	item.GemSockets = []*scene.EquipGemSocketInfo{
		{Socket: 0, GemType: 1, Name: "attack socket"},
		{Socket: 1, GemType: 0, Name: "any socket"},
	}
	return item
}

func equipForgeTestStack(itemId uint64, configId, count uint32) *scene.BagItemInfo {
	return &scene.BagItemInfo{ItemId: itemId, ConfigId: configId, Count: count, MaxStack: 999, Name: "stack"}
}

func equipForgeTestGemAttr() *scene.EquipAttrLineInfo {
	return &scene.EquipAttrLineInfo{AttrId: equipAttrDamage, Name: "attr-1", Value: equipForgeTestGemValue}
}

func equipForgeTestGem(itemId uint64, count uint32) *scene.BagItemInfo {
	gem := equipForgeTestStack(itemId, equipForgeGemAttackL1, count)
	gem.GemType = equipForgeGemTypeAttack
	gem.GemAttr = equipForgeTestGemAttr()
	return gem
}

// equipForgeTestSetGold 给两份快照写同一份 currency(服务器的 BuildSnapshot 把同一个 CurrencyComp 拷进每份 BagInfo)。
// 第二个槽位是别的货币,对账不该碰它。
func equipForgeTestSetGold(bags equipForgeBags, gold uint64) {
	for _, bag := range []*scene.BagInfo{bags.inventory, bags.equipment} {
		bag.Currency = &component.CurrencyComp{Values: []uint64{gold, 5}}
	}
}

// equipForgeTestBags 造一对自洽的快照:人物背包里有强化石(0 号物品格之外)、宝石和一件无关物品;
// target 按 worn 放进装备栏的武器槽或人物背包的 0 号格。stones / gems 为 0 时不放那一堆。
func equipForgeTestBags(target *scene.BagItemInfo, worn bool, gold uint64, stones, gems uint32) equipForgeBags {
	placed := map[uint32]*scene.BagItemInfo{
		7: {ItemId: equipForgeTestFillerId, ConfigId: 1, Count: 3, MaxStack: 99},
	}
	if stones > 0 {
		placed[1] = equipForgeTestStack(equipForgeTestStoneId, equipForgeStone, stones)
	}
	if gems > 0 {
		placed[2] = equipForgeTestGem(equipForgeTestGemId, gems)
	}
	equipped := map[uint32]*scene.BagItemInfo{}
	if worn {
		equipped[equipSmokeSlotWeapon] = target
	} else {
		placed[0] = target
	}
	bags := equipForgeBags{
		inventory: equipTestBag(equipSmokeBagInventory, 28, placed),
		equipment: equipTestBag(equipSmokeBagEquipment, 10, equipped),
	}
	equipForgeTestSetGold(bags, gold)
	return bags
}

func equipForgeTestClone(bags equipForgeBags) equipForgeBags {
	return equipForgeBags{
		inventory: proto.Clone(bags.inventory).(*scene.BagInfo),
		equipment: proto.Clone(bags.equipment).(*scene.BagInfo),
	}
}

// equipForgeTestTarget 取夹具里的目标(改它 = 改快照)。
func equipForgeTestTarget(bags equipForgeBags) *scene.BagItemInfo {
	target, _ := bags.item(equipForgeTestTargetId)
	return target
}

// equipForgeTestReplace 把两个包里 item_id 相同的那件换成 item(槽位不动)。
func equipForgeTestReplace(bags equipForgeBags, item *scene.BagItemInfo) {
	for _, bag := range []*scene.BagInfo{bags.inventory, bags.equipment} {
		for index, existing := range bag.Items {
			if existing.ItemId == item.ItemId {
				bag.Items[index] = item
			}
		}
	}
}

// equipForgeTestSetCount 改人物背包里某一堆的数量;改成 0 = 整堆连同格子一起回收(服务器 MergeAndCompact 之后的样子)。
func equipForgeTestSetCount(bags equipForgeBags, itemId uint64, count uint32) {
	if count == 0 {
		equipTestRemove(bags.inventory, itemId)
		return
	}
	equipBagItem(bags.inventory, itemId).Count = count
}

// equipForgeTestEnhanced 按契约演一次被受理的强化:扣测试表里的金币与强化石,目标变成 +level(等级没变就原样留着)。
func equipForgeTestEnhanced(before equipForgeBags, level uint32) equipForgeBags {
	after := equipForgeTestClone(before)
	equipForgeTestSetGold(after, equipForgeGold(before.inventory)-equipForgeTestCostGold)
	stones := uint32(equipForgeConfigCount(before.inventory, equipForgeStone))
	equipForgeTestSetCount(after, equipForgeTestStoneId, stones-equipForgeTestMaterialCount)
	if level != equipForgeTestTarget(before).GetEnhanceLevel() {
		equipForgeTestReplace(after, equipForgeTestWeapon(equipForgeTestTargetId, level))
	}
	return after
}

// equipForgeTestInlaid 按契约演一次被受理的镶嵌:0 号孔嵌上测试宝石,人物背包里那一堆少 1。
func equipForgeTestInlaid(before equipForgeBags) equipForgeBags {
	after := equipForgeTestClone(before)
	socket := equipForgeTestTarget(after).GemSockets[0]
	socket.GemConfigId = equipForgeGemAttackL1
	socket.GemName = "attack gem"
	socket.Attr = equipForgeTestGemAttr()
	gems := uint32(equipForgeConfigCount(before.inventory, equipForgeGemAttackL1))
	equipForgeTestSetCount(after, equipForgeTestGemId, gems-1)
	return after
}

// 放大算法与服务器的 ScaleByPermyriad 同锚点:增量向下取整、0 严格恒等、饱和不回绕。
func TestEquipForgeScaleMirrorsServerAnchors(t *testing.T) {
	for _, test := range []struct {
		base      uint64
		permyriad uint32
		want      uint64
	}{
		{40, 500, 42},
		{12, 500, 12},
		{19999, 5000, 29998},
		{math.MaxUint64, 1, math.MaxUint64},
		{math.MaxUint64, 0, math.MaxUint64},
		{40, 0, 40},
		{0, 8000, 0},
		{40, 8000, 72},
		{1620, 2600, 2041},
	} {
		if got := equipForgeScale(test.base, test.permyriad); got != test.want {
			t.Fatalf("scale(%d, %d) = %d, want %d", test.base, test.permyriad, got, test.want)
		}
	}
}

func TestEquipForgeLevelAfterFailureStopsAtZero(t *testing.T) {
	for _, test := range []struct{ level, loss, want uint32 }{
		{5, 0, 5}, {5, 2, 3}, {2, 2, 0}, {1, 3, 0}, {0, 1, 0},
	} {
		if got := equipForgeLevelAfterFailure(test.level, test.loss); got != test.want {
			t.Fatalf("level %d after losing %d = %d, want %d", test.level, test.loss, got, test.want)
		}
	}
}

// 目标 = item_id 最小且没到上限的 1101;满级的、别的 config、强化未开放的都挑不中。
func TestEquipForgePickTargetTakesLowestUnmaxedWeapon(t *testing.T) {
	maxed := equipForgeTestWeapon(10, equipForgeMaxEnhanceLevel)
	bag := equipTestBag(equipSmokeBagInventory, 28, map[uint32]*scene.BagItemInfo{
		0: equipForgeTestWeapon(30, 0), 1: maxed, 2: equipForgeTestWeapon(20, 4), 3: equipTestShoes(5),
	})
	if got := equipForgePickTarget(bag); got == nil || got.ItemId != 20 {
		t.Fatalf("picked %v, want the unmaxed weapon with the lowest item id (20)", got.GetItemId())
	}
	onlyMaxed := equipTestBag(equipSmokeBagInventory, 28, map[uint32]*scene.BagItemInfo{0: maxed, 1: equipTestShoes(5)})
	if got := equipForgePickTarget(onlyMaxed); got != nil {
		t.Fatalf("picked item %d although every weapon is at the cap", got.ItemId)
	}
	closed := equipForgeTestWeapon(40, 0)
	closed.EnhanceMaxLevel = 0
	if equipForgePickTarget(equipTestBag(equipSmokeBagInventory, 28, map[uint32]*scene.BagItemInfo{0: closed})) != nil {
		t.Fatal("picked a weapon although enhancement is closed (max level 0)")
	}
	if equipForgePickTarget(equipTestBag(equipSmokeBagInventory, 28, nil)) != nil {
		t.Fatal("picked something from an empty bag")
	}
}

// 金币读 currency.values[0](缺组件 / 列表短都按 0);数量按 config 汇总;「缺多少」只算缺口。
func TestEquipForgeGoldCountsAndShortfallReadTheSnapshot(t *testing.T) {
	bags := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 300, 1, 2)
	if got := equipForgeGold(bags.inventory); got != 300 {
		t.Fatalf("gold = %d, want 300", got)
	}
	// 同一种材料分两堆
	equipTestPlace(bags.inventory, 9, equipForgeTestStack(31, equipForgeStone, 4))
	if got := equipForgeConfigCount(bags.inventory, equipForgeStone); got != 5 {
		t.Fatalf("stones = %d, want 5 summed over two stacks", got)
	}
	if equipForgeConfigCount(bags.inventory, equipForgeGemAttackL1) != 2 ||
		equipForgeConfigCount(bags.inventory, equipForgeWeapon) != 1 ||
		equipForgeConfigCount(bags.inventory, equipForgeGemNeverGranted) != 0 {
		t.Fatal("per-config counts are wrong")
	}
	next := &scene.EquipEnhancePreview{CostGold: 1000, MaterialConfigId: equipForgeStone, MaterialCount: 7}
	if gold, material := equipForgeShortfall(bags.inventory, next); gold != 700 || material != 2 {
		t.Fatalf("shortfall = %d gold / %d material, want 700 / 2", gold, material)
	}
	equipForgeTestSetGold(bags, 1000)
	next.MaterialCount = 5
	if gold, material := equipForgeShortfall(bags.inventory, next); gold != 0 || material != 0 {
		t.Fatalf("shortfall = %d gold / %d material although the bag covers the cost exactly", gold, material)
	}
	// 不要材料(material_config_id = 0)时数量不作数
	if gold, material := equipForgeShortfall(bags.inventory, &scene.EquipEnhancePreview{MaterialCount: 9}); gold != 0 || material != 0 {
		t.Fatalf("shortfall = %d gold / %d material for a free row", gold, material)
	}
	bags.inventory.Currency = &component.CurrencyComp{}
	if equipForgeGold(bags.inventory) != 0 {
		t.Fatal("empty currency list must read as 0 gold")
	}
	bags.inventory.Currency = nil
	if equipForgeGold(bags.inventory) != 0 || equipForgeGold(nil) != 0 {
		t.Fatal("missing currency must read as 0 gold")
	}
}

func TestEquipForgeCountsDiffTreatsMissingAsZero(t *testing.T) {
	want := map[uint32]uint64{1: 2, 2: 0}
	if diff := equipForgeCountsDiff(want, map[uint32]uint64{1: 2}); len(diff) != 0 {
		t.Fatalf("a missing key and a zero count must compare equal: %v", diff)
	}
	diff := equipForgeCountsDiff(want, map[uint32]uint64{1: 3, 5: 1})
	if len(diff) != 2 || !strings.Contains(diff[0], "config_id=1 ") || !strings.Contains(diff[1], "config_id=5 ") {
		t.Fatalf("expected the two differing configs in ascending order, got %v", diff)
	}
}

// 空堆(count = 0)必须已回收。它不影响按 config 汇总的对账,所以要靠这一条单独拦。
func TestEquipForgeNoEmptyStacksRejectsZeroCountEntries(t *testing.T) {
	bags := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 1500, 2, 1)
	if err := checkEquipForgeNoEmptyStacks(bags.inventory); err != nil {
		t.Fatalf("clean snapshot rejected: %v", err)
	}
	if err := checkEquipForgeNoEmptyStacks(nil); err != nil {
		t.Fatalf("missing snapshot is not this check's business: %v", err)
	}
	after := equipForgeTestEnhanced(bags, 1)
	// 材料扣到 0 却没回收:把那一堆以 count = 0 留在包里
	equipTestPlace(after.inventory, 1, equipForgeTestStack(equipForgeTestStoneId, equipForgeStone, 0))
	if err := checkEquipForgeEnhance(bags, after, equipForgeTestTargetId, true, 1); err != nil {
		t.Fatalf("reconciliation goes by per-config totals and must not depend on the empty stack: %v", err)
	}
	err := checkEquipForgeNoEmptyStacks(after.inventory)
	if err == nil || !strings.Contains(err.Error(), "item_id=21") {
		t.Fatalf("drained stack left in the bag was not reported: %v", err)
	}
}

// 宝石物品:类型对、带合规的 gem_attr、可叠加的非装备,且不带装备才有的字段。
func TestEquipForgeGemItemRequiresTypeAttrAndNoEquipFields(t *testing.T) {
	if err := checkEquipForgeGemItem(equipForgeTestGem(1, 3), equipForgeGemTypeAttack); err != nil {
		t.Fatalf("well-formed gem rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(gem *scene.BagItemInfo)
	}{
		{"not a gem", func(gem *scene.BagItemInfo) { gem.GemType = 0 }},
		{"wrong type", func(gem *scene.BagItemInfo) { gem.GemType = 2 }},
		{"no attr", func(gem *scene.BagItemInfo) { gem.GemAttr = nil }},
		{"zero value", func(gem *scene.BagItemInfo) { gem.GemAttr.Value = 0 }},
		{"attr without id", func(gem *scene.BagItemInfo) { gem.GemAttr.AttrId = 0 }},
		{"attr without name", func(gem *scene.BagItemInfo) { gem.GemAttr.Name = "" }},
		{"attr with tier", func(gem *scene.BagItemInfo) { gem.GemAttr.Tier = equipTierBlue }},
		{"attr with cap", func(gem *scene.BagItemInfo) { gem.GemAttr.Cap = 30 }},
		{"attr with seq", func(gem *scene.BagItemInfo) { gem.GemAttr.Seq = 1 }},
		{"missing name", func(gem *scene.BagItemInfo) { gem.Name = "" }},
		{"not stackable", func(gem *scene.BagItemInfo) { gem.MaxStack = 1 }},
		{"is equipment", func(gem *scene.BagItemInfo) { gem.EquipKind = 11 }},
		{"carries enhance level", func(gem *scene.BagItemInfo) { gem.EnhanceLevel = 1 }},
		{"carries max level", func(gem *scene.BagItemInfo) { gem.EnhanceMaxLevel = equipForgeMaxEnhanceLevel }},
		{"carries sockets", func(gem *scene.BagItemInfo) {
			gem.GemSockets = []*scene.EquipGemSocketInfo{{Socket: 0, Name: "socket"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			gem := equipForgeTestGem(1, 3)
			test.mutate(gem)
			if err := checkEquipForgeGemItem(gem, equipForgeGemTypeAttack); err == nil {
				t.Fatal("broken gem item accepted")
			}
		})
	}
}

// 孔位:与定义逐个对上;空孔不带宝石数据;「已镶但不生效」(有 config 没 attr)是合法状态。
func TestEquipForgeSocketsMustMirrorTheDefinitions(t *testing.T) {
	build := func() []*scene.EquipGemSocketInfo {
		return equipForgeTestWeapon(1, 0).GemSockets
	}
	if err := checkEquipForgeSockets(build(), equipForgeWeaponSockets); err != nil {
		t.Fatalf("two empty sockets rejected: %v", err)
	}
	inlaid := build()
	inlaid[0].GemConfigId, inlaid[0].GemName, inlaid[0].Attr = equipForgeGemAttackL1, "attack gem", equipForgeTestGemAttr()
	if err := checkEquipForgeSockets(inlaid, equipForgeWeaponSockets); err != nil {
		t.Fatalf("inlaid socket rejected: %v", err)
	}
	inlaid[0].Attr = nil
	if err := checkEquipForgeSockets(inlaid, equipForgeWeaponSockets); err != nil {
		t.Fatalf("inlaid-but-ineffective socket is a legal state: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo
	}{
		{"no sockets", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo { return nil }},
		{"missing socket", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo { return sockets[:1] }},
		{"extra socket", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			return append(sockets, &scene.EquipGemSocketInfo{Socket: 2, Name: "extra"})
		}},
		{"not ascending", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			sockets[0], sockets[1] = sockets[1], sockets[0]
			return sockets
		}},
		{"wrong accepted type", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			sockets[0].GemType = 2
			return sockets
		}},
		{"missing name", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			sockets[1].Name = ""
			return sockets
		}},
		{"empty socket with attr", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			sockets[1].Attr = equipForgeTestGemAttr()
			return sockets
		}},
		{"empty socket with gem name", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			sockets[1].GemName = "ghost"
			return sockets
		}},
		{"inlaid with broken attr", func(sockets []*scene.EquipGemSocketInfo) []*scene.EquipGemSocketInfo {
			sockets[0].GemConfigId, sockets[0].Attr = equipForgeGemAttackL1, &scene.EquipAttrLineInfo{AttrId: 1, Name: "attr-1"}
			return sockets
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := checkEquipForgeSockets(test.mutate(build()), equipForgeWeaponSockets); err == nil {
				t.Fatal("broken sockets accepted")
			}
		})
	}
}

// 目标的显示字段:任何等级的自洽形状都放行(含满级不带预览);每种坏形状各自被拦。
func TestEquipForgeTargetDisplayAcceptsEveryLevelAndRejectsBrokenFields(t *testing.T) {
	for level := uint32(0); level <= equipForgeMaxEnhanceLevel; level++ {
		if err := checkEquipForgeTargetDisplay(equipForgeTestWeapon(1, level)); err != nil {
			t.Fatalf("well-formed +%d weapon rejected: %v", level, err)
		}
	}
	for _, test := range []struct {
		name   string
		level  uint32
		mutate func(item *scene.BagItemInfo)
	}{
		{"wrong config", 3, func(item *scene.BagItemInfo) { item.ConfigId = equipSmokeWeaponL80 }},
		{"not equipment", 3, func(item *scene.BagItemInfo) { item.EquipKind = 0 }},
		{"stacked", 3, func(item *scene.BagItemInfo) { item.Count = 2 }},
		{"enhancement closed", 3, func(item *scene.BagItemInfo) { item.EnhanceMaxLevel = 0 }},
		{"cap differs from mirrored table", 3, func(item *scene.BagItemInfo) { item.EnhanceMaxLevel = equipForgeMaxEnhanceLevel + 1 }},
		{"level above cap", 12, func(item *scene.BagItemInfo) { item.EnhanceLevel = equipForgeMaxEnhanceLevel + 1 }},
		{"bonus at level zero", 0, func(item *scene.BagItemInfo) { item.EnhanceBonusPermyriad = 500 }},
		{"preview missing below cap", 3, func(item *scene.BagItemInfo) {
			item.EnhanceNext = nil
			item.BaseAttrs[0].EnhanceNextValue = 0
		}},
		{"preview present at cap", 12, func(item *scene.BagItemInfo) {
			item.EnhanceNext = &scene.EquipEnhancePreview{NextLevel: 13, SuccessRate: 1, BonusPermyriad: 9000}
		}},
		{"preview skips a level", 3, func(item *scene.BagItemInfo) { item.EnhanceNext.NextLevel = 5 }},
		{"preview repeats the level", 3, func(item *scene.BagItemInfo) { item.EnhanceNext.NextLevel = 3 }},
		{"success rate zero", 3, func(item *scene.BagItemInfo) { item.EnhanceNext.SuccessRate = 0 }},
		{"success rate above base", 3, func(item *scene.BagItemInfo) { item.EnhanceNext.SuccessRate = equipForgePermyriadBase + 1 }},
		{"preview bonus shrinks", 3, func(item *scene.BagItemInfo) {
			item.EnhanceNext.BonusPermyriad = 1000
			item.BaseAttrs[0].EnhanceNextValue = equipForgeScale(equipForgeWeaponBaseDamage, 1000)
		}},
		{"table value drifted", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].Value++ }},
		{"bonus larger than value", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].EnhanceBonus = 1000 }},
		{"bonus does not follow permyriad", 3, func(item *scene.BagItemInfo) {
			item.BaseAttrs[0].Value++
			item.BaseAttrs[0].EnhanceBonus++
		}},
		{"bonus not applied", 3, func(item *scene.BagItemInfo) {
			item.BaseAttrs[0].Value = equipForgeWeaponBaseDamage
			item.BaseAttrs[0].EnhanceBonus = 0
		}},
		{"next value wrong", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].EnhanceNextValue++ }},
		{"next value missing", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].EnhanceNextValue = 0 }},
		{"next value at cap", 12, func(item *scene.BagItemInfo) { item.BaseAttrs[0].EnhanceNextValue = 99 }},
		{"wrong base attr", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].AttrId = equipAttrDefense }},
		{"no base attrs", 3, func(item *scene.BagItemInfo) { item.BaseAttrs = nil }},
		{"colored base tier", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].Tier = equipTierBlue }},
		{"base cap shown", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].Cap = 40 }},
		{"base name missing", 3, func(item *scene.BagItemInfo) { item.BaseAttrs[0].Name = "" }},
		{"uninitialized instance has no sockets", 3, func(item *scene.BagItemInfo) { item.GemSockets = nil }},
		{"socket type wrong", 3, func(item *scene.BagItemInfo) { item.GemSockets[1].GemType = 3 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := equipForgeTestWeapon(1, test.level)
			test.mutate(item)
			if err := checkEquipForgeTargetDisplay(item); err == nil {
				t.Fatal("broken target display accepted")
			}
		})
	}
}

// 强化对账的合法形状:成功(背包里 / 穿着)、材料恰好扣光、不要材料的行、失败不掉级、失败掉级、掉级不低于 0。
func TestEquipForgeEnhanceAcceptsEveryLegalOutcome(t *testing.T) {
	for _, worn := range []bool{false, true} {
		before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), worn, 1500, 5, 1)
		if err := checkEquipForgeEnhance(before, equipForgeTestEnhanced(before, 1), equipForgeTestTargetId, true, 1); err != nil {
			t.Fatalf("successful enhance (worn=%t) rejected: %v", worn, err)
		}
	}
	// 材料恰好扣到 0:那一堆整个消失
	drained := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 1000, 2, 0)
	if err := checkEquipForgeEnhance(drained, equipForgeTestEnhanced(drained, 1), equipForgeTestTargetId, true, 1); err != nil {
		t.Fatalf("enhance that drains gold and the material stack rejected: %v", err)
	}
	// 不要材料的行:只扣金币
	free := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 1500, 0, 1)
	equipForgeTestTarget(free).EnhanceNext.MaterialConfigId, equipForgeTestTarget(free).EnhanceNext.MaterialCount = 0, 0
	freeAfter := equipForgeTestClone(free)
	equipForgeTestSetGold(freeAfter, 500)
	equipForgeTestReplace(freeAfter, equipForgeTestWeapon(equipForgeTestTargetId, 1))
	if err := checkEquipForgeEnhance(free, freeAfter, equipForgeTestTargetId, true, 1); err != nil {
		t.Fatalf("enhance on a row without material rejected: %v", err)
	}
	// 概率行:成功与失败都接受;失败照扣、等级按 fail_level_loss 走
	for _, test := range []struct {
		name               string
		level, loss, after uint32
		success            bool
	}{
		{"risky success", 4, 0, 5, true},
		{"failure keeps level", 4, 0, 4, false},
		{"failure drops levels", 4, 2, 2, false},
		{"failure stops at zero", 1, 3, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, test.level), true, 1500, 5, 1)
			next := equipForgeTestTarget(before).EnhanceNext
			next.SuccessRate, next.FailLevelLoss = 6000, test.loss
			after := equipForgeTestEnhanced(before, test.after)
			if err := checkEquipForgeEnhance(before, after, equipForgeTestTargetId, test.success, test.after); err != nil {
				t.Fatalf("legal outcome rejected: %v", err)
			}
		})
	}
}

// 强化对账的每种坏形状各自被拦。基线:背包里 +0 → +1 成功,强化前 1500 金币 / 5 个强化石 / 1 颗宝石。
func TestEquipForgeEnhanceRejectsEveryBrokenLedger(t *testing.T) {
	for _, test := range []struct {
		name    string
		success bool
		level   uint32
		mutate  func(before, after equipForgeBags)
	}{
		{"gold not deducted", true, 1, func(before, after equipForgeBags) { equipForgeTestSetGold(after, 1500) }},
		{"gold over-deducted", true, 1, func(before, after equipForgeBags) { equipForgeTestSetGold(after, 100) }},
		{"material not deducted", true, 1, func(before, after equipForgeBags) {
			equipForgeTestSetCount(after, equipForgeTestStoneId, 5)
		}},
		{"material over-deducted", true, 1, func(before, after equipForgeBags) {
			equipForgeTestSetCount(after, equipForgeTestStoneId, 2)
		}},
		{"gem consumed", true, 1, func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestGemId, 0) }},
		{"bystander lost", true, 1, func(before, after equipForgeBags) { equipTestRemove(after.inventory, equipForgeTestFillerId) }},
		{"level not written", true, 1, func(before, after equipForgeBags) {
			equipForgeTestReplace(after, equipForgeTestWeapon(equipForgeTestTargetId, 0))
		}},
		{"response level disagrees", true, 2, func(before, after equipForgeBags) {}},
		{"guaranteed roll reported as failure", false, 0, func(before, after equipForgeBags) {
			equipForgeTestReplace(after, equipForgeTestWeapon(equipForgeTestTargetId, 0))
		}},
		{"affix rerolled", true, 1, func(before, after equipForgeBags) { equipForgeTestTarget(after).Affixes[0].Value = 1 }},
		{"socket touched", true, 1, func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[0].GemConfigId = equipForgeGemAttackL1
		}},
		{"name changed", true, 1, func(before, after equipForgeBags) { equipForgeTestTarget(after).Name = "other" }},
		{"table value drifted", true, 1, func(before, after equipForgeBags) { equipForgeTestTarget(after).BaseAttrs[0].Value++ }},
		{"bonus does not follow permyriad", true, 1, func(before, after equipForgeBags) {
			line := equipForgeTestTarget(after).BaseAttrs[0]
			line.Value++
			line.EnhanceBonus++
		}},
		{"base attr swapped", true, 1, func(before, after equipForgeBags) {
			equipForgeTestTarget(after).BaseAttrs[0].AttrId = equipAttrDefense
		}},
		{"base attr row lost", true, 1, func(before, after equipForgeBags) { equipForgeTestTarget(after).BaseAttrs = nil }},
		// 600 与 500 万分比在表值 40 上都是 +2:每行的值对得上,只有「总加成 == 预览」这一条拦得住
		{"total bonus differs from preview", true, 1, func(before, after equipForgeBags) {
			equipForgeTestTarget(after).EnhanceBonusPermyriad = 600
		}},
		{"value differs from preview", true, 1, func(before, after equipForgeBags) {
			equipForgeTestTarget(before).BaseAttrs[0].EnhanceNextValue = 43
		}},
		{"moved into equipment", true, 1, func(before, after equipForgeBags) {
			target := equipForgeTestTarget(after)
			equipTestRemove(after.inventory, equipForgeTestTargetId)
			equipTestPlace(after.equipment, equipSmokeSlotWeapon, target)
		}},
		{"moved to another slot", true, 1, func(before, after equipForgeBags) {
			for _, slot := range after.inventory.Layout.Slots {
				if slot.ItemId == equipForgeTestTargetId {
					slot.Slot = 9
				}
			}
		}},
		{"instance duplicated", true, 1, func(before, after equipForgeBags) {
			equipTestPlace(after.equipment, equipSmokeSlotWeapon, equipCloneItem(equipForgeTestTarget(after)))
		}},
		{"instance lost", true, 1, func(before, after equipForgeBags) { equipTestRemove(after.inventory, equipForgeTestTargetId) }},
		{"equipment bystander appeared", true, 1, func(before, after equipForgeBags) {
			equipTestPlace(after.equipment, equipSmokeSlotShoes, equipTestShoes(40))
		}},
		{"slot defs dropped", true, 1, func(before, after equipForgeBags) { after.equipment.Layout.EquipSlots = nil }},
		{"snapshots disagree on gold", true, 1, func(before, after equipForgeBags) {
			after.equipment.Currency = &component.CurrencyComp{Values: []uint64{1}}
		}},
		{"no preview before", true, 1, func(before, after equipForgeBags) { equipForgeTestTarget(before).EnhanceNext = nil }},
		{"preview level inconsistent", true, 1, func(before, after equipForgeBags) {
			equipForgeTestTarget(before).EnhanceNext.NextLevel = 2
		}},
		{"accepted without enough material", true, 1, func(before, after equipForgeBags) {
			equipForgeTestSetCount(before, equipForgeTestStoneId, 1)
		}},
		{"accepted without enough gold", true, 1, func(before, after equipForgeBags) { equipForgeTestSetGold(before, 999) }},
		{"target absent before", true, 1, func(before, after equipForgeBags) { equipTestRemove(before.inventory, equipForgeTestTargetId) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 1500, 5, 1)
			after := equipForgeTestEnhanced(before, 1)
			test.mutate(before, after)
			if err := checkEquipForgeEnhance(before, after, equipForgeTestTargetId, test.success, test.level); err == nil {
				t.Fatal("broken enhance accepted")
			}
		})
	}
	// 概率行失败:等级不许涨;等级没变时显示数据一个字段都不许变
	before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 4), true, 1500, 5, 1)
	equipForgeTestTarget(before).EnhanceNext.SuccessRate = 6000
	raised := equipForgeTestEnhanced(before, 5)
	if err := checkEquipForgeEnhance(before, raised, equipForgeTestTargetId, false, 5); err == nil {
		t.Fatal("level raised although the roll failed")
	}
	kept := equipForgeTestEnhanced(before, 4)
	equipForgeTestTarget(kept).Affixes[0].Value = 1
	if err := checkEquipForgeEnhance(before, kept, equipForgeTestTargetId, false, 4); err == nil {
		t.Fatal("display changed although the level did not")
	}
}

// equipForgeTestAccepted 把一对快照包成一次被受理的强化结果。
func equipForgeTestAccepted(bags equipForgeBags, success bool, level uint32, replayed bool) equipForgeChange {
	return equipForgeChange{
		equipChange: equipChange{bag: bags.inventory, equipment: bags.equipment},
		success:     success,
		level:       level,
		replayed:    replayed,
	}
}

// 同键重发的合法形状:首次成功与首次失败(等级没变)两种,重放都回首次的结果、标 replayed、快照与首次应答相同。
func TestEquipForgeReplayAcceptsTheFirstOutcome(t *testing.T) {
	before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 1500, 5, 1)
	for _, test := range []struct {
		name    string
		success bool
		level   uint32
	}{
		{"first enhance succeeded", true, 1},
		{"first roll failed and kept the level", false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			enhanced := equipForgeTestEnhanced(before, test.level)
			first := equipForgeTestAccepted(enhanced, test.success, test.level, false)
			replay := equipForgeTestAccepted(equipForgeTestClone(enhanced), test.success, test.level, true)
			if err := checkEquipForgeReplay(first, replay); err != nil {
				t.Fatalf("legal replay rejected: %v", err)
			}
		})
	}
}

// 同键重发的每种坏形状各自被拦。基线:背包里 +0 → +1 成功,首次之后 500 金币 / 3 个强化石 / 1 颗宝石。
func TestEquipForgeReplayRejectsEveryBrokenShape(t *testing.T) {
	before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), false, 1500, 5, 1)
	enhanced := equipForgeTestEnhanced(before, 1)
	first := equipForgeTestAccepted(enhanced, true, 1, false)
	for _, test := range []struct {
		name   string
		mutate func(replay *equipForgeChange)
	}{
		// 改之前的行为:没有键时按等级比较,重发被拒成「等级已变化」,客户端会以为第一次没成
		{"refused like a stale-level enhance", func(replay *equipForgeChange) {
			replay.tip, replay.bag, replay.equipment = equipForgeTipEnhanceLevelChanged, nil, nil
		}},
		{"not marked replayed", func(replay *equipForgeChange) { replay.replayed = false }},
		{"roll outcome differs", func(replay *equipForgeChange) { replay.success = false }},
		{"level differs", func(replay *equipForgeChange) { replay.level = 2 }},
		{"charged again", func(replay *equipForgeChange) {
			equipForgeTestSetGold(equipForgeBags{inventory: replay.bag, equipment: replay.equipment}, 0)
		}},
		{"material consumed again", func(replay *equipForgeChange) {
			equipForgeTestSetCount(equipForgeBags{inventory: replay.bag, equipment: replay.equipment}, equipForgeTestStoneId, 1)
		}},
		{"target enhanced again", func(replay *equipForgeChange) {
			equipForgeTestReplace(equipForgeBags{inventory: replay.bag, equipment: replay.equipment},
				equipForgeTestWeapon(equipForgeTestTargetId, 2))
		}},
		{"equipment changed", func(replay *equipForgeChange) {
			equipTestPlace(replay.equipment, equipSmokeSlotShoes, equipTestShoes(40))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			replay := equipForgeTestAccepted(equipForgeTestClone(enhanced), true, 1, true)
			test.mutate(&replay)
			if err := checkEquipForgeReplay(first, replay); err == nil {
				t.Fatal("broken replay accepted")
			}
		})
	}
}

// 镶嵌 / 取下对账的合法形状:穿着与背包里、宝石剩一堆与恰好用完、取下后退回成新的一堆。
func TestEquipForgeInlayAndRemoveAcceptLegalMoves(t *testing.T) {
	for _, worn := range []bool{false, true} {
		before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 3), worn, 200, 0, 2)
		inlaid := equipForgeTestInlaid(before)
		if err := checkEquipForgeInlay(before, inlaid, equipForgeTestTargetId, equipForgeSocketAttack, equipForgeGemAttackL1); err != nil {
			t.Fatalf("inlay (worn=%t) rejected: %v", worn, err)
		}
		// 取下 = 回到镶嵌前
		if err := checkEquipForgeRemove(inlaid, equipForgeTestClone(before), equipForgeTestTargetId, equipForgeSocketAttack); err != nil {
			t.Fatalf("remove (worn=%t) rejected: %v", worn, err)
		}
	}
	// 最后一颗:镶嵌后那一堆消失;取下时铸出一堆新的(item_id 与格子都可以不同)
	last := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 0), true, 0, 0, 1)
	inlaid := equipForgeTestInlaid(last)
	if err := checkEquipForgeInlay(last, inlaid, equipForgeTestTargetId, equipForgeSocketAttack, equipForgeGemAttackL1); err != nil {
		t.Fatalf("inlaying the last gem rejected: %v", err)
	}
	removed := equipForgeTestClone(last)
	equipTestRemove(removed.inventory, equipForgeTestGemId)
	equipTestPlace(removed.inventory, 5, equipForgeTestGem(99, 1))
	if err := checkEquipForgeRemove(inlaid, removed, equipForgeTestTargetId, equipForgeSocketAttack); err != nil {
		t.Fatalf("gem returned as a freshly minted stack rejected: %v", err)
	}
}

// 镶嵌对账的每种坏形状各自被拦。基线:穿着的 +3 武器,0 号孔嵌测试宝石,背包里 2 颗 → 1 颗。
func TestEquipForgeInlayRejectsEveryBrokenMove(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(before, after equipForgeBags)
	}{
		{"gem not consumed", func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestGemId, 2) }},
		{"two gems consumed", func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestGemId, 0) }},
		{"socket still empty", func(before, after equipForgeBags) {
			equipForgeTestReplace(after, equipCloneItem(equipForgeTestTarget(before)))
		}},
		{"wrong gem in socket", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[0].GemConfigId = equipForgeGemDefenseL1
		}},
		{"gem not effective", func(before, after equipForgeBags) { equipForgeTestTarget(after).GemSockets[0].Attr = nil }},
		{"attr value differs from the gem item", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[0].Attr.Value++
		}},
		{"attr id differs from the gem item", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[0].Attr.AttrId = equipAttrDefense
		}},
		{"attr carries a tier", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[0].Attr.Tier = equipTierBlue
		}},
		{"gem name missing", func(before, after equipForgeBags) { equipForgeTestTarget(after).GemSockets[0].GemName = "" }},
		{"socket type rewritten", func(before, after equipForgeBags) { equipForgeTestTarget(after).GemSockets[0].GemType = 0 }},
		{"socket renamed", func(before, after equipForgeBags) { equipForgeTestTarget(after).GemSockets[0].Name = "other" }},
		{"other socket touched", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[1].GemConfigId = equipForgeGemAttackL1
		}},
		{"enhance level changed", func(before, after equipForgeBags) { equipForgeTestTarget(after).EnhanceLevel++ }},
		{"base attr changed", func(before, after equipForgeBags) { equipForgeTestTarget(after).BaseAttrs[0].Value++ }},
		{"gold charged", func(before, after equipForgeBags) { equipForgeTestSetGold(after, 100) }},
		{"bystander lost", func(before, after equipForgeBags) { equipTestRemove(after.inventory, equipForgeTestFillerId) }},
		{"taken off", func(before, after equipForgeBags) {
			target := equipForgeTestTarget(after)
			equipTestRemove(after.equipment, equipForgeTestTargetId)
			equipTestPlace(after.inventory, 0, target)
		}},
		{"socket occupied before", func(before, after equipForgeBags) {
			equipForgeTestTarget(before).GemSockets[0].GemConfigId = equipForgeGemDefenseL1
		}},
		{"gem not owned before", func(before, after equipForgeBags) { equipTestRemove(before.inventory, equipForgeTestGemId) }},
		{"target absent before", func(before, after equipForgeBags) { equipTestRemove(before.equipment, equipForgeTestTargetId) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 3), true, 200, 0, 2)
			after := equipForgeTestInlaid(before)
			test.mutate(before, after)
			if err := checkEquipForgeInlay(before, after, equipForgeTestTargetId, equipForgeSocketAttack, equipForgeGemAttackL1); err == nil {
				t.Fatal("broken inlay accepted")
			}
		})
	}
	// 武器没有的孔:不该被受理
	before := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 3), true, 200, 0, 2)
	if err := checkEquipForgeInlay(before, equipForgeTestInlaid(before), equipForgeTestTargetId,
		equipForgeAbsentSocket, equipForgeGemAttackL1); err == nil {
		t.Fatal("inlay into a socket the weapon does not have accepted")
	}
}

// 取下对账的每种坏形状各自被拦。基线:穿着的 +3 武器 0 号孔里有测试宝石、背包里 1 颗 → 取下后 2 颗。
func TestEquipForgeRemoveRejectsEveryBrokenMove(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(before, after equipForgeBags)
	}{
		{"gem not returned", func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestGemId, 1) }},
		{"two gems returned", func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestGemId, 3) }},
		{"socket still occupied", func(before, after equipForgeBags) {
			equipForgeTestReplace(after, equipCloneItem(equipForgeTestTarget(before)))
		}},
		{"attr left behind", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[0].Attr = equipForgeTestGemAttr()
		}},
		{"gem name left behind", func(before, after equipForgeBags) { equipForgeTestTarget(after).GemSockets[0].GemName = "attack gem" }},
		{"socket dropped from the list", func(before, after equipForgeBags) {
			target := equipForgeTestTarget(after)
			target.GemSockets = target.GemSockets[1:]
		}},
		{"socket type rewritten", func(before, after equipForgeBags) { equipForgeTestTarget(after).GemSockets[0].GemType = 0 }},
		{"other socket touched", func(before, after equipForgeBags) {
			equipForgeTestTarget(after).GemSockets[1].GemConfigId = equipForgeGemAttackL1
		}},
		{"enhance level changed", func(before, after equipForgeBags) { equipForgeTestTarget(after).EnhanceLevel-- }},
		{"gold charged", func(before, after equipForgeBags) { equipForgeTestSetGold(after, 100) }},
		{"a different gem returned", func(before, after equipForgeBags) {
			equipForgeTestSetCount(after, equipForgeTestGemId, 1)
			equipTestPlace(after.inventory, 5, equipForgeTestStack(98, equipForgeGemDefenseL1, 1))
		}},
		{"socket empty before", func(before, after equipForgeBags) {
			socket := equipForgeTestTarget(before).GemSockets[0]
			socket.GemConfigId, socket.GemName, socket.Attr = 0, "", nil
		}},
		{"target absent before", func(before, after equipForgeBags) { equipTestRemove(before.equipment, equipForgeTestTargetId) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			empty := equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 3), true, 200, 0, 2)
			before := equipForgeTestInlaid(empty)
			after := equipForgeTestClone(empty)
			test.mutate(before, after)
			if err := checkEquipForgeRemove(before, after, equipForgeTestTargetId, equipForgeSocketAttack); err == nil {
				t.Fatal("broken remove accepted")
			}
		})
	}
}

// 重登:目标仍穿着且逐字段相同,金币 / 按 config 汇总的数量 / 装备栏不变;同种材料换了分堆不算变化。
func TestEquipForgeRestoredRequiresSameTargetGoldCountsAndEquipment(t *testing.T) {
	build := func() equipForgeBags {
		bags := equipForgeTestInlaid(equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 3), true, 200, 5, 2))
		equipTestPlace(bags.inventory, 9, equipForgeTestStack(31, equipForgeStone, 4))
		return bags
	}
	before := build()
	if err := checkEquipForgeRestored(before, equipForgeTestClone(before), equipForgeTestTargetId); err != nil {
		t.Fatalf("identical snapshots rejected: %v", err)
	}
	merged := equipForgeTestClone(before)
	equipTestRemove(merged.inventory, 31)
	equipForgeTestSetCount(merged, equipForgeTestStoneId, 9)
	if err := checkEquipForgeRestored(before, merged, equipForgeTestTargetId); err != nil {
		t.Fatalf("same totals in different stacks rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(before, after equipForgeBags)
	}{
		{"enhance level lost", func(before, after equipForgeBags) {
			equipForgeTestReplace(after, equipForgeTestWeapon(equipForgeTestTargetId, 0))
		}},
		{"gem lost", func(before, after equipForgeBags) {
			socket := equipForgeTestTarget(after).GemSockets[0]
			socket.GemConfigId, socket.GemName, socket.Attr = 0, "", nil
		}},
		{"affix lost", func(before, after equipForgeBags) { equipForgeTestTarget(after).Affixes = nil }},
		{"back in the bag", func(before, after equipForgeBags) {
			target := equipForgeTestTarget(after)
			equipTestRemove(after.equipment, equipForgeTestTargetId)
			equipTestPlace(after.inventory, 0, target)
		}},
		{"duplicated", func(before, after equipForgeBags) {
			equipTestPlace(after.inventory, 0, equipCloneItem(equipForgeTestTarget(after)))
		}},
		{"gone", func(before, after equipForgeBags) { equipTestRemove(after.equipment, equipForgeTestTargetId) }},
		{"gold rolled back", func(before, after equipForgeBags) { equipForgeTestSetGold(after, 1200) }},
		{"material rolled back", func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestStoneId, 7) }},
		{"gem refunded", func(before, after equipForgeBags) { equipForgeTestSetCount(after, equipForgeTestGemId, 2) }},
		{"slot defs dropped", func(before, after equipForgeBags) { after.equipment.Layout.EquipSlots = nil }},
		{"moved to another equip slot", func(before, after equipForgeBags) {
			after.equipment.Layout.Slots[0].Slot = equipSmokeSlotHat
		}},
		{"not worn before", func(before, after equipForgeBags) {
			target := equipForgeTestTarget(before)
			equipTestRemove(before.equipment, equipForgeTestTargetId)
			equipTestPlace(before.inventory, 0, target)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := build()
			after := equipForgeTestClone(before)
			test.mutate(before, after)
			if err := checkEquipForgeRestored(before, after, equipForgeTestTargetId); err == nil {
				t.Fatal("changed state after relogin accepted")
			}
		})
	}
}

// 面板:物伤 / 法伤恰好各变 delta(可为负、可为 0),其余一项都不许动。
func TestEquipForgePanelShiftRequiresExactAttackDelta(t *testing.T) {
	shifted := func(delta int64) *scene.AttributePanelInfo {
		panel := equipTestPanel()
		panel.Derived.PhysicalAttack = uint64(int64(panel.Derived.PhysicalAttack) + delta)
		panel.Derived.MagicAttack = uint64(int64(panel.Derived.MagicAttack) + delta)
		return panel
	}
	for _, delta := range []int64{30, 0, -2} {
		if err := checkEquipForgePanelShift(equipTestPanel(), shifted(delta), delta); err != nil {
			t.Fatalf("exact shift of %d rejected: %v", delta, err)
		}
	}
	// 当前气血 / 法力不参与比较(穿着的装备变了会按比例保持,取整可能差 1)
	healed := shifted(30)
	healed.Derived.Health, healed.Derived.Mana = 1, 1
	if err := checkEquipForgePanelShift(equipTestPanel(), healed, 30); err != nil {
		t.Fatalf("current health / mana must not count: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(panel *scene.AttributePanelInfo)
	}{
		{"physical short by one", func(panel *scene.AttributePanelInfo) { panel.Derived.PhysicalAttack-- }},
		{"magic not raised", func(panel *scene.AttributePanelInfo) { panel.Derived.MagicAttack -= 30 }},
		{"raised too much", func(panel *scene.AttributePanelInfo) { panel.Derived.PhysicalAttack++ }},
		{"defense changed", func(panel *scene.AttributePanelInfo) { panel.Derived.Defense++ }},
		{"max health changed", func(panel *scene.AttributePanelInfo) { panel.Derived.MaxHealth-- }},
		{"dimension changed", func(panel *scene.AttributePanelInfo) { panel.Dimensions[0].Bonus++ }},
		{"combat row changed", func(panel *scene.AttributePanelInfo) { panel.Combat[3].Value++ }},
		{"combat row gone", func(panel *scene.AttributePanelInfo) { panel.Combat = panel.Combat[:14] }},
		{"no derived block", func(panel *scene.AttributePanelInfo) { panel.Derived = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			panel := shifted(30)
			test.mutate(panel)
			if err := checkEquipForgePanelShift(equipTestPanel(), panel, 30); err == nil {
				t.Fatal("panel that is not an exact attack shift accepted")
			}
		})
	}
	if err := checkEquipForgePanelShift(equipTestPanel(), equipTestPanel(), -1000); err == nil {
		t.Fatal("a drop larger than the attack itself accepted")
	}
	if err := checkEquipForgePanelShift(&scene.AttributePanelInfo{}, equipTestPanel(), 0); err == nil {
		t.Fatal("baseline without derived stats accepted")
	}
}

// 小取值函数:孔位查找与日志串、基础属性当前值、物品所在格、「去掉目标」的副本。
func TestEquipForgeItemHelpersReadSocketsBaseValueAndSlot(t *testing.T) {
	bags := equipForgeTestInlaid(equipForgeTestBags(equipForgeTestWeapon(equipForgeTestTargetId, 3), true, 200, 0, 2))
	target := equipForgeTestTarget(bags)
	if socket := equipForgeSocketOf(target, equipForgeSocketAttack); socket == nil || socket.GemConfigId != equipForgeGemAttackL1 {
		t.Fatal("socket lookup is wrong")
	}
	if equipForgeSocketOf(target, equipForgeAbsentSocket) != nil || equipForgeSocketOf(nil, 0) != nil {
		t.Fatal("absent socket reported as present")
	}
	if text := equipForgeSocketsText(target); text != "[0:t1=2101 1:t0=0]" {
		t.Fatalf("sockets text = %q", text)
	}
	target.GemSockets[0].Attr = nil
	if text := equipForgeSocketsText(target); text != "[0:t1=2101! 1:t0=0]" {
		t.Fatalf("ineffective gem is not marked: %q", text)
	}
	// +3 = 1500 万分比:40 → 46
	if equipForgeBaseValue(target, equipAttrDamage) != 46 || equipForgeBaseValue(target, equipAttrSpeed) != 0 {
		t.Fatal("base value lookup is wrong")
	}
	if equipForgeItemSlot(bags.equipment, equipForgeTestTargetId) != int64(equipSmokeSlotWeapon) ||
		equipForgeItemSlot(bags.inventory, equipForgeTestTargetId) != -1 {
		t.Fatal("item slot lookup is wrong")
	}
	stripped := equipForgeWithoutItem(bags.equipment, equipForgeTestTargetId)
	if len(stripped.Items) != 0 || stripped.Currency != nil || len(stripped.Layout.Slots) != 1 || len(bags.equipment.Items) != 1 {
		t.Fatal("stripping must drop the item and currency from a copy and keep the layout")
	}
	if equipForgeWithoutItem(nil, 1) != nil {
		t.Fatal("stripping a missing snapshot must stay nil")
	}
	worn, isWorn := bags.item(equipForgeTestTargetId)
	gem, gemWorn := bags.item(equipForgeTestGemId)
	missing, _ := bags.item(12345)
	if worn == nil || !isWorn || gem == nil || gemWorn || missing != nil {
		t.Fatal("two-bag item lookup is wrong")
	}
}

// 两条冒烟不能共用账号(强化不可逆,见 equipForgeAccount 的注释);失败提示要点名该换哪个账号。
func TestEquipForgeAccountIsSeparateAndHintsNameIt(t *testing.T) {
	if equipForgeAccount == equipSmokeAccount {
		t.Fatal("equip-forge-smoke must not share its account with equip-smoke")
	}
	if !strings.HasPrefix(equipForgeAccount, "robot_") {
		t.Fatalf("account %q does not hit the dev password auth prefix", equipForgeAccount)
	}
	for _, hint := range []string{equipForgeResetHint(), equipForgeDisplayHint()} {
		if !strings.Contains(hint, equipForgeAccount) {
			t.Fatalf("hint %q does not name the account to replace", hint)
		}
	}
	if equipForgeOutcomeText(true) == equipForgeOutcomeText(false) {
		t.Fatal("success and failure must read differently in the result line")
	}
}

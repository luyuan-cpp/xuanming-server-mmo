package main

// equip-smoke 里纯校验函数的单测:不连服务器,只喂构造出来的快照。
// 冒烟本身要等整套服务起来才跑得到,而它的断言函数若写反了(该红不红),联机时是看不出来的 ——
// 所以每个校验函数都要有「合法形状放行 / 每种坏形状各自被拦」两面。
// 字符串字面量一律 ASCII;属性名用 attr-<id> / combat-<id>,与真表无关。

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"proto/common/base"
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/logic/handler"
)

func equipTestLine(attrId, tier, seq uint32, value, limit uint64) *scene.EquipAttrLineInfo {
	return &scene.EquipAttrLineInfo{
		AttrId: attrId, Name: fmt.Sprintf("attr-%d", attrId), Tier: tier, Seq: seq, Value: value, Cap: limit,
	}
}

// 蓝 2 条 + 粉 1 条(与第一条蓝是同一个属性,规则允许)+ 黄 1 条。
func equipTestAffixes() []*scene.EquipAttrLineInfo {
	return []*scene.EquipAttrLineInfo{
		equipTestLine(3, 1, 0, 2, 2),
		equipTestLine(12, 1, 1, 1, 2),
		equipTestLine(3, 2, 0, 1, 2),
		equipTestLine(14, 3, 0, 2, 2),
	}
}

func equipTestWeapon(itemId uint64) *scene.BagItemInfo {
	return &scene.BagItemInfo{
		ItemId: itemId, ConfigId: equipSmokeWeaponL1, Count: 1, MaxStack: 1, EquipKind: 11,
		Name: "wooden sword", EquipLevel: 1,
		BaseAttrs: []*scene.EquipAttrLineInfo{equipTestLine(equipAttrDamage, equipTierBase, 0, 40, 0)},
		Affixes:   equipTestAffixes(),
	}
}

func equipTestShoes(itemId uint64) *scene.BagItemInfo {
	return &scene.BagItemInfo{
		ItemId: itemId, ConfigId: equipSmokeShoesL1, Count: 1, MaxStack: 1, EquipKind: 14,
		Name: "straw shoes", EquipLevel: 1,
		BaseAttrs: []*scene.EquipAttrLineInfo{
			equipTestLine(equipAttrDefense, equipTierBase, 0, 12, 0),
			equipTestLine(equipAttrSpeed, equipTierBase, 1, 12, 0),
		},
		Affixes: []*scene.EquipAttrLineInfo{equipTestLine(equipAttrDefense, 1, 0, 3, 6)},
	}
}

// equipTestBag 按「槽号 → 物品」造一份自洽的包快照;装备栏额外带上 3–6 号槽的定义。
func equipTestBag(bagType, capacity uint32, placed map[uint32]*scene.BagItemInfo) *scene.BagInfo {
	bag := &scene.BagInfo{Layout: &scene.BagLayoutInfo{BagType: bagType, Capacity: capacity}}
	for slot, item := range placed {
		bag.Items = append(bag.Items, item)
		bag.Layout.Slots = append(bag.Layout.Slots,
			&scene.BagSlotInfo{Slot: slot, ItemId: item.ItemId, Width: 1, Height: 1})
	}
	if bagType == equipSmokeBagEquipment {
		for _, slot := range equipSmokeSlots {
			bag.Layout.EquipSlots = append(bag.Layout.EquipSlots,
				&scene.EquipSlotInfo{Slot: slot, EquipKind: slot + 8, Name: fmt.Sprintf("slot-%d", slot)})
		}
	}
	return bag
}

// equipTestRemove 把一件物品连同引用它的槽位从快照里拿掉。
func equipTestRemove(bag *scene.BagInfo, itemId uint64) {
	var items []*scene.BagItemInfo
	for _, item := range bag.Items {
		if item.ItemId != itemId {
			items = append(items, item)
		}
	}
	bag.Items = items
	var slots []*scene.BagSlotInfo
	for _, slot := range bag.Layout.Slots {
		if slot.ItemId != itemId {
			slots = append(slots, slot)
		}
	}
	bag.Layout.Slots = slots
}

// equipTestPlace 往快照的指定槽里放一件物品(调用方保证该槽空着)。
func equipTestPlace(bag *scene.BagInfo, slot uint32, item *scene.BagItemInfo) {
	bag.Items = append(bag.Items, item)
	bag.Layout.Slots = append(bag.Layout.Slots,
		&scene.BagSlotInfo{Slot: slot, ItemId: item.ItemId, Width: 1, Height: 1})
}

func equipTestPanel() *scene.AttributePanelInfo {
	panel := &scene.AttributePanelInfo{
		Level: 1,
		Derived: &scene.DerivedAttributeInfo{
			MaxHealth: 550, MaxMana: 840, PhysicalAttack: 50, MagicAttack: 40, Speed: 276, Defense: 180,
			Health: 550, Mana: 840,
		},
		Dimensions: []*scene.AttributeDimensionInfo{
			{DimensionId: 101, Value: 1}, {DimensionId: 103, Value: 1},
		},
	}
	for id := uint32(1); id <= equipSmokeCombatRows; id++ {
		panel.Combat = append(panel.Combat, &scene.CombatAttributeInfo{
			CombatId: id, Name: fmt.Sprintf("combat-%d", id), Percent: id != 6, Sort: id * 10,
		})
	}
	return panel
}

func equipClonePanel(panel *scene.AttributePanelInfo) *scene.AttributePanelInfo {
	return proto.Clone(panel).(*scene.AttributePanelInfo)
}

func equipCloneItem(item *scene.BagItemInfo) *scene.BagItemInfo {
	return proto.Clone(item).(*scene.BagItemInfo)
}

// 合法形状:只有一条蓝;蓝 + 粉 + 黄齐全且粉与蓝重复同一属性;各档都顶到上限(蓝 3 + 粉 1 + 黄 1)。
func TestEquipAffixLinesAcceptLegalShapes(t *testing.T) {
	if err := checkEquipAffixLines([]*scene.EquipAttrLineInfo{equipTestLine(3, 1, 0, 1, 2)}); err != nil {
		t.Fatalf("single blue row rejected: %v", err)
	}
	if err := checkEquipAffixLines(equipTestAffixes()); err != nil {
		t.Fatalf("blue + pink + yellow rejected: %v", err)
	}
	full := []*scene.EquipAttrLineInfo{
		equipTestLine(3, 1, 0, 1, 2), equipTestLine(12, 1, 1, 1, 2), equipTestLine(14, 1, 2, 1, 2),
		equipTestLine(12, 2, 0, 2, 2), equipTestLine(3, 3, 0, 2, 2),
	}
	if err := checkEquipAffixLines(full); err != nil {
		t.Fatalf("blue 3 + pink 1 + yellow 1 rejected: %v", err)
	}
}

// 每种坏形状各自被拦:条数越界、档位越界、值越界、缺名称 / id、蓝属性重复、seq 不从 0 连续、档位乱序、
// 各档条数越界。最后四个用例的总行数、seq、蓝属性去重全都合法,只有「各档条数」那一条拦得住。
func TestEquipAffixLinesRejectMalformedRows(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo
	}{
		{"no rows", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo { return nil }},
		{"too many rows", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			return append(lines, equipTestLine(15, 3, 1, 1, 2), equipTestLine(17, 3, 2, 1, 1))
		}},
		{"base tier", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0].Tier = equipTierBase
			return lines
		}},
		{"green tier", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[3].Tier = 4
			return lines
		}},
		{"zero value", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].Value = 0
			return lines
		}},
		{"value above cap", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].Value = 3
			return lines
		}},
		{"missing name", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0].Name = ""
			return lines
		}},
		{"missing attr id", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0].AttrId = 0
			return lines
		}},
		{"duplicate blue attr", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].AttrId = lines[0].AttrId
			return lines
		}},
		{"seq starts at one", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[2].Seq = 1
			return lines
		}},
		{"seq gap", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].Seq = 2
			return lines
		}},
		{"tiers out of order", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0], lines[3] = lines[3], lines[0]
			return lines
		}},
		{"four blue rows", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			return []*scene.EquipAttrLineInfo{
				equipTestLine(3, 1, 0, 1, 2), equipTestLine(12, 1, 1, 1, 2),
				equipTestLine(14, 1, 2, 1, 2), equipTestLine(15, 1, 3, 1, 2),
			}
		}},
		{"two pink rows", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			return []*scene.EquipAttrLineInfo{
				equipTestLine(3, 1, 0, 1, 2), equipTestLine(12, 2, 0, 1, 2), equipTestLine(14, 2, 1, 1, 2),
			}
		}},
		{"two yellow rows", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			return []*scene.EquipAttrLineInfo{
				equipTestLine(3, 1, 0, 1, 2), equipTestLine(12, 3, 0, 1, 2), equipTestLine(14, 3, 1, 1, 2),
			}
		}},
		{"pink without blue", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			return []*scene.EquipAttrLineInfo{equipTestLine(3, 2, 0, 1, 2)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := checkEquipAffixLines(test.mutate(equipTestAffixes())); err == nil {
				t.Fatal("malformed affix rows accepted")
			}
		})
	}
}

// 基础属性必须与表逐行一致(顺序、id、值),且 tier = 0、cap = 0、seq 从 0 递增、有名称。
func TestEquipBaseLinesMustMirrorTableRowsInOrder(t *testing.T) {
	want := []equipBaseAttr{{equipAttrDefense, 12}, {equipAttrSpeed, 12}}
	if err := checkEquipBaseLines(equipTestShoes(1).BaseAttrs, want); err != nil {
		t.Fatalf("table-shaped base rows rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo
	}{
		{"missing row", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo { return lines[:1] }},
		{"swapped rows", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0].AttrId, lines[1].AttrId = lines[1].AttrId, lines[0].AttrId
			return lines
		}},
		{"wrong value", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].Value = 13
			return lines
		}},
		{"colored tier", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0].Tier = equipTierBlue
			return lines
		}},
		{"cap shown", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[0].Cap = 12
			return lines
		}},
		{"seq not increasing", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].Seq = 0
			return lines
		}},
		{"missing name", func(lines []*scene.EquipAttrLineInfo) []*scene.EquipAttrLineInfo {
			lines[1].Name = ""
			return lines
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := checkEquipBaseLines(test.mutate(equipTestShoes(1).BaseAttrs), want); err == nil {
				t.Fatal("base rows that differ from the table accepted")
			}
		})
	}
}

// tooltip 整体:名称、部位、不可叠加、佩戴等级任一不对都要拦。
func TestEquipItemDisplayRequiresNameKindLevelAndSingleStack(t *testing.T) {
	want := []equipBaseAttr{{equipAttrDamage, 40}}
	if err := checkEquipItemDisplay(equipTestWeapon(1), 1, want); err != nil {
		t.Fatalf("well-formed weapon rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(item *scene.BagItemInfo)
	}{
		{"missing name", func(item *scene.BagItemInfo) { item.Name = "" }},
		{"not equipment", func(item *scene.BagItemInfo) { item.EquipKind = 0 }},
		{"stackable", func(item *scene.BagItemInfo) { item.MaxStack = 99 }},
		{"stacked", func(item *scene.BagItemInfo) { item.Count = 2 }},
		{"wrong level", func(item *scene.BagItemInfo) { item.EquipLevel = 20 }},
		{"no affixes", func(item *scene.BagItemInfo) { item.Affixes = nil }},
		{"no base attrs", func(item *scene.BagItemInfo) { item.BaseAttrs = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := equipTestWeapon(1)
			test.mutate(item)
			if err := checkEquipItemDisplay(item, 1, want); err == nil {
				t.Fatal("broken tooltip accepted")
			}
		})
	}
}

// 槽位定义:要的槽都在、有部位有名称、升序、不重复。
func TestEquipSlotDefsRequireNamedAscendingUniqueSlots(t *testing.T) {
	layout := equipTestBag(equipSmokeBagEquipment, 10, nil).Layout
	defs, err := checkEquipSlotDefs(layout, equipSmokeSlots...)
	if err != nil || len(defs) != len(equipSmokeSlots) || defs[equipSmokeSlotShoes].GetEquipKind() != 14 {
		t.Fatalf("well-formed slot defs rejected or misread: defs=%d err=%v", len(defs), err)
	}
	for _, test := range []struct {
		name   string
		mutate func(layout *scene.BagLayoutInfo)
	}{
		{"missing slot", func(layout *scene.BagLayoutInfo) { layout.EquipSlots = layout.EquipSlots[:3] }},
		{"missing name", func(layout *scene.BagLayoutInfo) { layout.EquipSlots[1].Name = "" }},
		{"missing kind", func(layout *scene.BagLayoutInfo) { layout.EquipSlots[2].EquipKind = 0 }},
		{"not ascending", func(layout *scene.BagLayoutInfo) {
			layout.EquipSlots[0], layout.EquipSlots[1] = layout.EquipSlots[1], layout.EquipSlots[0]
		}},
		{"duplicate slot", func(layout *scene.BagLayoutInfo) { layout.EquipSlots[1].Slot = layout.EquipSlots[0].Slot }},
		{"no defs at all", func(layout *scene.BagLayoutInfo) { layout.EquipSlots = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			broken := equipTestBag(equipSmokeBagEquipment, 10, nil).Layout
			test.mutate(broken)
			if _, err := checkEquipSlotDefs(broken, equipSmokeSlots...); err == nil {
				t.Fatal("broken slot defs accepted")
			}
		})
	}
}

// 穿脱后的两个包:正常形状放行;重掷、放错槽、实例被复制、实例丢失、缺槽位定义、多穿了一件各自被拦。
func TestEquipLayoutCheckCatchesRerollDuplicationLossAndMisplacement(t *testing.T) {
	weaponA, weaponB, shoes := equipTestWeapon(11), equipTestWeapon(12), equipTestShoes(13)
	// 一件与冒烟无关、不在 inBag 名单里的普通物品。有了它才造得出「总数恰好守恒」的复制 / 错位
	// (少一件无关物品、多一份装备)—— 那种情形下数量检查是瞎的,见本用例末尾三段。
	filler := &scene.BagItemInfo{ItemId: 14, ConfigId: 1, Count: 3, MaxStack: 99}
	build := func() (*scene.BagInfo, *scene.BagInfo) {
		bag := equipTestBag(equipSmokeBagInventory, 28, map[uint32]*scene.BagItemInfo{
			0: equipCloneItem(weaponB), 5: equipCloneItem(shoes), 7: equipCloneItem(filler),
		})
		equipment := equipTestBag(equipSmokeBagEquipment, 10, map[uint32]*scene.BagItemInfo{
			equipSmokeSlotWeapon: equipCloneItem(weaponA),
		})
		return bag, equipment
	}
	check := func(bag, equipment *scene.BagInfo) error {
		return checkEquipLayout(bag, equipment, 4,
			map[uint32]*scene.BagItemInfo{equipSmokeSlotWeapon: weaponA}, weaponB, shoes)
	}
	bag, equipment := build()
	if err := check(bag, equipment); err != nil {
		t.Fatalf("well-formed layout rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(bag, equipment *scene.BagInfo)
	}{
		{"affix rerolled on move", func(bag, equipment *scene.BagInfo) { equipment.Items[0].Affixes[0].Value = 1 }},
		{"affix lost on move", func(bag, equipment *scene.BagInfo) { equipment.Items[0].Affixes = nil }},
		{"wrong slot", func(bag, equipment *scene.BagInfo) { equipment.Layout.Slots[0].Slot = equipSmokeSlotHat }},
		{"instance duplicated, total grows", func(bag, equipment *scene.BagInfo) {
			equipTestPlace(bag, 9, equipCloneItem(weaponA))
		}},
		{"instance lost", func(bag, equipment *scene.BagInfo) {
			bag.Items, bag.Layout.Slots = nil, nil
		}},
		{"dangling layout slot", func(bag, equipment *scene.BagInfo) { bag.Layout.Slots[0].ItemId = 999 }},
		{"slot defs missing", func(bag, equipment *scene.BagInfo) { equipment.Layout.EquipSlots = nil }},
		{"wrong bag type", func(bag, equipment *scene.BagInfo) { equipment.Layout.BagType = equipSmokeBagInventory }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bag, equipment := build()
			test.mutate(bag, equipment)
			if err := check(bag, equipment); err == nil {
				t.Fatal("broken layout accepted")
			}
		})
	}
	// 以下三段的两个包物品总数都恰好守恒为 4,数量检查看不出来,得靠各自那一条。
	//
	// 1) 已穿上的 A 在人物背包里还留着一份,顶掉了那件无关物品。总数、装备栏件数、3 号槽、应在背包的两件全都
	//    对得上,只有「已穿上的 guid 还留在人物背包里」这一条拦得住 —— 把那条检查删掉,这里就会被放行。
	bag, equipment = build()
	equipTestRemove(bag, filler.ItemId)
	equipTestPlace(bag, 7, equipCloneItem(weaponA))
	if err := check(bag, equipment); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("item_id=%d", weaponA.ItemId)) {
		t.Fatalf("copy of the equipped instance left in the bag was not reported: %v", err)
	}
	// 2) 反方向的复制:应在背包里的 B 在装备栏里多出一份(背包里少的是那件无关物品)。装备栏件数不对。
	bag, equipment = build()
	equipTestRemove(bag, filler.ItemId)
	equipTestPlace(equipment, 0, equipCloneItem(weaponB))
	if err := check(bag, equipment); err == nil {
		t.Fatal("copy of a bag instance inside the equipment bag accepted")
	}
	// 3) 错位而不是复制:应在背包里的 B 整件跑到了装备栏。同样红在装备栏件数。
	bag, equipment = build()
	equipTestRemove(bag, weaponB.ItemId)
	equipTestPlace(equipment, 0, equipCloneItem(weaponB))
	if err := check(bag, equipment); err == nil {
		t.Fatal("extra equipped item accepted")
	}
}

// display-* 的失败提示要分清「复用的旧实例」与「本轮新铸的」,并且两种都点名该换哪个账号。
func TestEquipDisplayHintSeparatesReusedFromFreshlyGrantedInstances(t *testing.T) {
	old, fresh := equipTestWeapon(11), equipTestWeapon(12)
	reused := map[uint64]bool{old.ItemId: true}
	oldHint, freshHint := equipDisplayHint(old, reused), equipDisplayHint(fresh, reused)
	if !strings.Contains(oldHint, "reused=true") || !strings.Contains(freshHint, "reused=false") {
		t.Fatalf("hints do not tell reused from freshly granted: %q / %q", oldHint, freshHint)
	}
	for _, hint := range []string{oldHint, freshHint} {
		if !strings.Contains(hint, equipSmokeAccount) {
			t.Fatalf("hint %q does not name the account to replace", hint)
		}
	}
	// 名单为空 = 发物之前人物背包里什么都没有(全新账号):哪一件都不是复用的
	if !strings.Contains(equipDisplayHint(old, nil), "reused=false") {
		t.Fatal("instance reported as reused although the bag was empty before granting")
	}
}

// 面板比较:当前气血 / 法力不参与;六项上限、维度 value / bonus、战斗属性任一变化都要报出来。
func TestEquipPanelDiffCoversDerivedDimensionsAndCombatButNotCurrentHealth(t *testing.T) {
	bare := equipTestPanel()
	same := equipClonePanel(bare)
	same.Derived.Health, same.Derived.Mana = 1, 1
	if diff := equipPanelDiff(bare, same); len(diff) != 0 {
		t.Fatalf("current health / mana must not count: %v", diff)
	}
	for _, test := range []struct {
		name   string
		mutate func(panel *scene.AttributePanelInfo)
	}{
		{"max health", func(panel *scene.AttributePanelInfo) { panel.Derived.MaxHealth++ }},
		{"max mana", func(panel *scene.AttributePanelInfo) { panel.Derived.MaxMana++ }},
		{"physical attack", func(panel *scene.AttributePanelInfo) { panel.Derived.PhysicalAttack++ }},
		{"magic attack", func(panel *scene.AttributePanelInfo) { panel.Derived.MagicAttack++ }},
		{"speed", func(panel *scene.AttributePanelInfo) { panel.Derived.Speed++ }},
		{"defense", func(panel *scene.AttributePanelInfo) { panel.Derived.Defense++ }},
		{"dimension value", func(panel *scene.AttributePanelInfo) { panel.Dimensions[1].Value++ }},
		{"dimension bonus", func(panel *scene.AttributePanelInfo) { panel.Dimensions[0].Bonus++ }},
		{"combat value", func(panel *scene.AttributePanelInfo) { panel.Combat[14].Value++ }},
		{"combat row gone", func(panel *scene.AttributePanelInfo) { panel.Combat = panel.Combat[:14] }},
		{"dimension gone", func(panel *scene.AttributePanelInfo) { panel.Dimensions = panel.Dimensions[:1] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := equipClonePanel(bare)
			test.mutate(changed)
			if diff := equipPanelDiff(bare, changed); len(diff) == 0 {
				t.Fatal("changed panel reported as identical")
			}
		})
	}
	raised := equipClonePanel(bare)
	raised.Derived.Defense += 12
	raised.Combat[0].Value += 2
	if drops := equipPanelDrops(bare, raised); len(drops) != 0 {
		t.Fatalf("raised stats reported as drops: %v", drops)
	}
	if drops := equipPanelDrops(raised, bare); len(drops) != 2 {
		t.Fatalf("expected exactly the two lowered stats, got %v", drops)
	}
}

// 面板形状:战斗属性必须 15 行全量、id 不重复、有名称、按 sort 升序。
func TestEquipPanelShapeRequiresFullNamedSortedCombatRows(t *testing.T) {
	if err := checkEquipPanelShape(equipTestPanel()); err != nil {
		t.Fatalf("well-formed panel rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(panel *scene.AttributePanelInfo)
	}{
		{"no derived", func(panel *scene.AttributePanelInfo) { panel.Derived = nil }},
		{"row missing", func(panel *scene.AttributePanelInfo) { panel.Combat = panel.Combat[:14] }},
		{"no combat rows", func(panel *scene.AttributePanelInfo) { panel.Combat = nil }},
		{"duplicate id", func(panel *scene.AttributePanelInfo) { panel.Combat[3].CombatId = panel.Combat[2].CombatId }},
		{"zero id", func(panel *scene.AttributePanelInfo) { panel.Combat[0].CombatId = 0 }},
		{"missing name", func(panel *scene.AttributePanelInfo) { panel.Combat[7].Name = "" }},
		{"not sorted", func(panel *scene.AttributePanelInfo) { panel.Combat[5].Sort = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			panel := equipTestPanel()
			test.mutate(panel)
			if err := checkEquipPanelShape(panel); err == nil {
				t.Fatal("broken panel shape accepted")
			}
		})
	}
}

// 加成下界:基础 + 随机的「伤害 / 防御 / 速度」都要算进去;同名战斗属性行要涨;任何一项下降都不行。
func TestEquipGainRequiresFlatAttrsSameNameCombatRowsAndNoDrops(t *testing.T) {
	bare := equipTestPanel()
	weapon := equipTestWeapon(1)
	// 蓝:伤害 +6;蓝:与面板 combat-1 同名的战斗属性 +2
	combatLine := equipTestLine(12, 1, 1, 2, 2)
	combatLine.Name = "combat-1"
	weapon.Affixes = []*scene.EquipAttrLineInfo{equipTestLine(equipAttrDamage, 1, 0, 6, 20), combatLine}
	shoes := equipTestShoes(2) // 基础 防御 12 / 速度 12,蓝:防御 +3

	dressed := func() *scene.AttributePanelInfo {
		panel := equipClonePanel(bare)
		panel.Derived.PhysicalAttack += 46
		panel.Derived.MagicAttack += 46
		panel.Derived.Defense += 15
		panel.Derived.Speed += 12
		panel.Combat[0].Value += 2
		return panel
	}
	if err := checkEquipGain(bare, dressed(), weapon, shoes); err != nil {
		t.Fatalf("panel with every bonus applied rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(panel *scene.AttributePanelInfo)
	}{
		{"damage affix not applied", func(panel *scene.AttributePanelInfo) { panel.Derived.PhysicalAttack -= 6 }},
		{"damage not applied to magic", func(panel *scene.AttributePanelInfo) { panel.Derived.MagicAttack -= 46 }},
		{"defense affix not applied", func(panel *scene.AttributePanelInfo) { panel.Derived.Defense -= 3 }},
		{"base speed not applied", func(panel *scene.AttributePanelInfo) { panel.Derived.Speed -= 12 }},
		{"combat affix not applied", func(panel *scene.AttributePanelInfo) { panel.Combat[0].Value -= 2 }},
		{"unrelated stat dropped", func(panel *scene.AttributePanelInfo) { panel.Derived.MaxHealth-- }},
		{"dimension dropped", func(panel *scene.AttributePanelInfo) { panel.Dimensions[0].Value-- }},
	} {
		t.Run(test.name, func(t *testing.T) {
			panel := dressed()
			test.mutate(panel)
			if err := checkEquipGain(bare, panel, weapon, shoes); err == nil {
				t.Fatal("missing bonus accepted")
			}
		})
	}
	// 百分比行的终值按 100 封顶:穿前 99、装备 +2,面板显示 100 就算加到位
	capped := equipTestPanel()
	capped.Combat[0].Value = 99
	cappedNow := equipClonePanel(capped)
	cappedNow.Derived.PhysicalAttack += 46
	cappedNow.Derived.MagicAttack += 46
	cappedNow.Combat[0].Value = 100
	if err := checkEquipGain(capped, cappedNow, weapon); err != nil {
		t.Fatalf("percent row clamped at 100 rejected: %v", err)
	}
}

// 新响应类型进 RecordFeatureBody 之后:全零 / 缺包的响应是失败,响应体 tip 被带出来,完整响应原样保留。
func TestEquipFeatureBodiesRejectZeroResponsesAndSurfaceBodyTips(t *testing.T) {
	inventory := equipTestBag(equipSmokeBagInventory, 28, nil)
	equipment := equipTestBag(equipSmokeBagEquipment, 10, nil)
	rejected := &base.TipInfoMessage{Id: 7}
	for _, test := range []struct {
		name     string
		id       uint32
		zero     proto.Message
		partial  proto.Message
		rejected proto.Message
		complete proto.Message
	}{
		{"equip", game.SceneBagClientPlayerEquipItemMessageId,
			&scene.EquipItemResponse{},
			&scene.EquipItemResponse{Bag: inventory},
			&scene.EquipItemResponse{ErrorMessage: rejected},
			&scene.EquipItemResponse{Bag: inventory, Equipment: equipment}},
		{"unequip", game.SceneBagClientPlayerUnequipItemMessageId,
			&scene.UnequipItemResponse{},
			&scene.UnequipItemResponse{Equipment: equipment},
			&scene.UnequipItemResponse{ErrorMessage: rejected},
			&scene.UnequipItemResponse{Bag: inventory, Equipment: equipment}},
		{"gm grant", game.SceneBagClientPlayerGmGrantItemMessageId,
			&scene.GmGrantItemResponse{},
			&scene.GmGrantItemResponse{Bag: &scene.BagInfo{}},
			&scene.GmGrantItemResponse{ErrorMessage: rejected},
			&scene.GmGrantItemResponse{Bag: inventory}},
	} {
		t.Run(test.name, func(t *testing.T) {
			player := gameobject.NewPlayer(1)
			for _, incomplete := range []proto.Message{test.zero, test.partial} {
				cursor := player.FeatureSequence()
				handler.RecordFeatureBody(player, test.id, incomplete)
				got, ok := player.FeatureResponse(test.id, cursor)
				if !ok || got.TipID != 0 || got.Failure == "" || got.Response != nil {
					t.Fatal("all-zero or partial response counted as success")
				}
			}
			cursor := player.FeatureSequence()
			handler.RecordFeatureBody(player, test.id, test.rejected)
			got, ok := player.FeatureResponse(test.id, cursor)
			if !ok || got.TipID != 7 || got.Response != nil {
				t.Fatal("body tip was not surfaced")
			}
			cursor = player.FeatureSequence()
			handler.RecordFeatureBody(player, test.id, test.complete)
			got, ok = player.FeatureResponse(test.id, cursor)
			if !ok || got.TipID != 0 || got.Failure != "" || !proto.Equal(got.Response, test.complete) {
				t.Fatal("complete response was not retained")
			}
		})
	}
}

// handler.HandleFeatureMessage(features-smoke 的收包路径)也登记了这三个消息号:信封错误先于解包、
// 空 body 不算成功、完整响应按来源消息号留存。equip-smoke 自己不走这条路(它用 prepareBehaviorClient 的
// 通用分发),这里只是不让那三条 case 成为没有任何用例碰到的代码。
func TestEquipFeatureMessagesHonorEnvelopeErrorsAndKeepTheirSource(t *testing.T) {
	inventory := equipTestBag(equipSmokeBagInventory, 28, nil)
	equipment := equipTestBag(equipSmokeBagEquipment, 10, nil)
	for _, test := range []struct {
		name     string
		id       uint32
		complete proto.Message
	}{
		{"equip", game.SceneBagClientPlayerEquipItemMessageId,
			&scene.EquipItemResponse{Bag: inventory, Equipment: equipment}},
		{"unequip", game.SceneBagClientPlayerUnequipItemMessageId,
			&scene.UnequipItemResponse{Bag: inventory, Equipment: equipment}},
		{"gm grant", game.SceneBagClientPlayerGmGrantItemMessageId,
			&scene.GmGrantItemResponse{Bag: inventory}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := proto.Marshal(test.complete)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			player := gameobject.NewPlayer(1)
			cursor := player.FeatureSequence()
			if !handler.HandleFeatureMessage(player, &base.MessageContent{
				MessageId: test.id, ErrorMessage: &base.TipInfoMessage{Id: 42}, SerializedMessage: body,
			}) {
				t.Fatal("message id is not routed as a feature response")
			}
			got, ok := player.FeatureResponse(test.id, cursor)
			if !ok || got.TipID != 42 || got.Response != nil || got.Failure == "" {
				t.Fatal("envelope error became a success")
			}
			cursor = player.FeatureSequence()
			handler.HandleFeatureMessage(player, &base.MessageContent{MessageId: test.id})
			got, ok = player.FeatureResponse(test.id, cursor)
			if !ok || got.TipID != 0 || got.Failure == "" || got.Response != nil {
				t.Fatal("empty body counted as success")
			}
			cursor = player.FeatureSequence()
			handler.HandleFeatureMessage(player, &base.MessageContent{MessageId: test.id, SerializedMessage: body})
			got, ok = player.FeatureResponse(test.id, cursor)
			if !ok || got.MessageID != test.id || got.TipID != 0 || got.Failure != "" ||
				!proto.Equal(got.Response, test.complete) {
				t.Fatal("complete response was not retained under its own message id")
			}
		})
	}
}

// 「第一把 / 第二把」按 item_id 升序定,与线上顺序无关;空位 = 容量 - 已占槽。
func TestEquipBagHelpersOrderInstancesAndCountFreeSlots(t *testing.T) {
	bag := equipTestBag(equipSmokeBagInventory, 5, map[uint32]*scene.BagItemInfo{
		0: equipTestWeapon(30), 1: equipTestWeapon(10), 2: equipTestShoes(15), 4: equipTestWeapon(20),
	})
	weapons := equipBagItemsOfConfig(bag, equipSmokeWeaponL1)
	if len(weapons) != 3 || weapons[0].ItemId != 10 || weapons[1].ItemId != 20 || weapons[2].ItemId != 30 {
		t.Fatal("instances are not ordered by ascending item id")
	}
	if len(equipBagItemsOfConfig(bag, equipSmokeWeaponL80)) != 0 {
		t.Fatal("absent config reported as present")
	}
	if equipBagFreeSlots(bag) != 1 {
		t.Fatalf("free slots = %d, want 1", equipBagFreeSlots(bag))
	}
	if equipSlotOccupant(bag, 2) != 15 || equipSlotOccupant(bag, 3) != 0 {
		t.Fatal("slot occupant lookup is wrong")
	}
	if equipBagItem(bag, 20) == nil || equipBagItem(bag, 21) != nil {
		t.Fatal("item lookup is wrong")
	}
}

// 失败日志里的 tip 带枚举名;0 与未知码不编造名字。
func TestEquipTipTextNamesGeneratedEnums(t *testing.T) {
	if text := equipTipText(tipEquipLevelNotEnough); !strings.Contains(text, "kEquipLevelNotEnough") {
		t.Fatalf("tip text %q does not carry the enum name", text)
	}
	// 第 8 步 EquipItem(0) 认的那个通用段码也得叫得出名字(它不在 equip_error 段里)
	if text := equipTipText(tipInvalidParameter); !strings.Contains(text, "kInvalidParameter") {
		t.Fatalf("tip text %q does not carry the common-segment enum name", text)
	}
	if equipTipText(0) != "0" || equipTipText(4000000000) != "4000000000" {
		t.Fatal("zero or unknown tip was given a made-up name")
	}
}

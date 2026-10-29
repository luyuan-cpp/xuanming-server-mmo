package main

// equip-forge-smoke 场景:单机器人对本地服务端做「装备强化 + 宝石镶嵌 / 取下」端到端冒烟。
//
// 兑现 docs/design/equipment-enhance-inlay.md §7 的冒烟设计(协议 §3.3、编排 §4.3、不变量 §5),逐条断言:
//   0. 预备(让脚本可重复运行):装备栏里穿着的全部卸下;目标 = 人物背包里 item_id 最小且还没强化到上限的 1101,
//      没有就 GmGrantItem 发一把(空位不够直接判红并提示清号);目标各孔里的宝石全部取下;人物背包里保证至少一颗 2101。
//      金币与强化材料不在这里发 —— 第 3 步按当时那一级的预览「缺多少补多少」;
//   1. 显示:目标 enhance_max_level = 12、两个孔(0 号只收类型 1、1 号任意,名称非空,此刻都是空孔)、
//      enhance_next.next_level = enhance_level + 1;基础属性行 value − enhance_bonus = 表值 40、
//      enhance_bonus = 表值 × 当前万分比 / 10000(向下取整)、enhance_next_value = 下一级的预览值;
//      背包里的 2101 带 gem_type = 1 与 gem_attr(属性「伤害」);
//   2. 拒绝码(只测能确定造出来的,每条之后重新读两个包、要求原样不变):
//      嵌强化石 → kEquipGemNotGem;嵌 7 号孔 → kEquipGemSocketInvalid;0 号孔嵌防御宝石 → kEquipGemTypeMismatch;
//      0 号孔嵌从不发放的 2103 → kEquipGemNotOwned;取空孔 → kEquipGemSocketEmpty;
//      EnhanceEquip(0)、InlayGem(gem_config_id = 0)→ kInvalidParameter;
//   3. 强化两次 —— 先在背包里、穿上后再来一次。对账一律以**强化前那份快照**里的 enhance_next 为准:
//      金币差 = cost_gold、材料差 = material_count、成功则等级 +1、失败则 max(0, 等级 − fail_level_loss);
//      success_rate = 10000 必须成功,其余两种结果都接受(强化失败不是错误,见 §5 不变量 10)。
//      穿着且等级变了:服务器推属性面板,物伤 / 法伤恰好各变了基础属性行的差值;等级没变:面板不变;
//   4. 镶嵌:0 号孔嵌 2101 → 该孔 gem_config_id = 2101 且带 attr、背包里 2101 少 1、面板物伤 / 法伤各 + 宝石值;
//      同一个孔再嵌 → kEquipGemSocketOccupied,两个包不变;
//   5. 下线重登:强化等级与宝石都在(目标的显示数据逐字段相同),金币 / 背包数量 / 面板与下线前一致;
//   6. 取下:该孔变回空孔、2101 回到镶嵌前的数量、面板恰好回到镶嵌前;打 EQUIP_FORGE_SMOKE_OK。
//
// 每份强化 / 镶嵌 / 取下的响应快照都断言没有 count = 0 的条目(扣到 0 的空堆必须已回收,§4.3 强化第 13 步)。
//
// 可重复性:强化不可逆,机器人也没有删物手段。目标到 +12 之后,下一轮的第 0 步会自动换一把新的 1101
// (背包因此每十几轮多一把;满了判红并提示清号)。kEquipEnhanceUnavailable / kEquipGemBagFull 机器人造不出来,
// 由 C++ 单测覆盖;kEquipEnhanceMaxLevel 只在「第一次强化恰好把目标推到 +12」的那一轮顺带验一次(见第 3 步)。
//
// 结果约定(供外层脚本消费):
//   全过 → 日志一行 `EQUIP_FORGE_SMOKE_OK …`,退出码 0;
//   任一步失败 → `EQUIP_FORGE_SMOKE_FAIL step=… reason=…`,退出码 1。

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧的 DevPasswordAuth(开发用密码认证)。
// 9104 接在 equip-smoke 的 9103 之后。两条冒烟**不能共用账号**:强化不可逆,而 equip-smoke 第 1 步按表值断言
// 1101 的基础属性恰为「伤害 40」,包里留一把 +N 的 1101 就会让它红在 display-weapon。
const equipForgeAccount = "robot_9104"

// 冒烟镜像的表值 —— **策划改表要同步改这里**(设计文档 §2.1–§2.4 的初始数据)。
// 其余数值(每级的金币 / 材料 / 成功率 / 万分比、宝石属性值、孔位名)一律读服务器下发的快照,不在这里抄。
const (
	equipForgeWeapon           uint32 = 1101 // 目标装备:1 级武器(Item 表);部位 = 武器,带两个孔
	equipForgeWeaponBaseDamage uint64 = 40   // 1101 基础属性「伤害」的表值(未强化时的 value;与 equip-smoke 第 1 步同值)
	equipForgeStone            uint32 = 2001 // 强化石:Item 表里有、**不是宝石** —— 拿它造 kEquipGemNotGem
	equipForgeGemAttackL1      uint32 = 2101 // 攻击宝石·一级:类型 1、属性「伤害」—— 嵌进 0 号孔
	equipForgeGemDefenseL1     uint32 = 2201 // 防御宝石·一级:类型 2 —— 嵌 0 号孔(只收类型 1)造 kEquipGemTypeMismatch
	equipForgeGemNeverGranted  uint32 = 2103 // 攻击宝石·三级:类型对得上 0 号孔,但本冒烟**从不发放** —— 造 kEquipGemNotOwned
	equipForgeGemTypeAttack    uint32 = 1    // EquipGem.gem_type:1 = 攻击
	equipForgeMaxEnhanceLevel  uint32 = 12   // 强化上限 = EquipEnhance 表从 1 起连续的行数
	equipForgeSocketAttack     uint32 = 0    // 武器 0 号孔(攻击孔)
	equipForgeAbsentSocket     uint32 = 7    // 孔位号合法(< kMaxGemSockets = 8)但武器没有这个孔 —— 造 kEquipGemSocketInvalid
)

// equipForgeSocketDef 是孔位定义的一行(EquipGemSocket 表):孔位号 + 接纳的宝石类型(0 = 任意)。
type equipForgeSocketDef struct {
	socket  uint32
	gemType uint32
}

// 武器的孔位定义(EquipGemSocket 表里 equip_kind = 11 的两行,socket 升序)。同属上面「策划改表要同步」的镜像值。
var equipForgeWeaponSockets = []equipForgeSocketDef{
	{socket: equipForgeSocketAttack, gemType: equipForgeGemTypeAttack}, // 攻击孔
	{socket: 1, gemType: 0}, // 通用孔
}

// 万分比的分母(equiprules::kPermyriadBase)。success_rate 到这个值即必成。
const equipForgePermyriadBase uint32 = 10000

// GmGrantItem 单次数量上限,镜像 PlayerEquipSystem::kGmGrantMaxCount。补材料是「缺多少补多少」,
// 一级最多要 10 个强化石,远在上限之内;真超了说明表改得很大,明说好过被服务器回一个 kEquipGrantInvalid。
const equipForgeGrantMaxCount uint64 = 99

// 本期新增的 tip。一律读导表器生成的枚举,**不写字面量**(AGENTS.md §7 不变量 5);
// 复用的码(kInvalidParameter / kEquipNotEquipped / kEquipBagFull / kEquipGrantInvalid)用 equip_smoke_scenario.go 里那一份。
var (
	equipForgeTipEnhanceMaxLevel   = uint32(tiptable.EquipError_kEquipEnhanceMaxLevel)
	equipForgeTipGemNotGem         = uint32(tiptable.EquipError_kEquipGemNotGem)
	equipForgeTipGemNotOwned       = uint32(tiptable.EquipError_kEquipGemNotOwned)
	equipForgeTipGemSocketInvalid  = uint32(tiptable.EquipError_kEquipGemSocketInvalid)
	equipForgeTipGemSocketOccupied = uint32(tiptable.EquipError_kEquipGemSocketOccupied)
	equipForgeTipGemSocketEmpty    = uint32(tiptable.EquipError_kEquipGemSocketEmpty)
	equipForgeTipGemTypeMismatch   = uint32(tiptable.EquipError_kEquipGemTypeMismatch)
	equipForgeTipGemBagFull        = uint32(tiptable.EquipError_kEquipGemBagFull)
)

// equipForgeBags 是同一时刻的人物背包 + 装备栏两份全量快照。
type equipForgeBags struct {
	inventory *scene.BagInfo
	equipment *scene.BagInfo
}

// item 在两个包里找一件物品;第二个返回值 = 它是否穿在身上(在装备栏里)。两个包都没有时返回 (nil, false)。
func (b equipForgeBags) item(itemId uint64) (*scene.BagItemInfo, bool) {
	if item := equipBagItem(b.equipment, itemId); item != nil {
		return item, true
	}
	return equipBagItem(b.inventory, itemId), false
}

// equipForgeChange 是一次强化 / 镶嵌 / 取下请求的完整结果。
type equipForgeChange struct {
	equipChange        // bag / equipment / tip / panelSeq,语义同穿脱(业务拒绝时两个包为 nil)
	success     bool   // 仅强化:本次掷骰是否成功。false 不是错误 —— tip 仍为 0、两个包照常带回
	level       uint32 // 仅强化:操作后的强化等级
}

// RunEquipForgeSmoke 是 main.go `mode: equip-forge-smoke` 的入口。
func RunEquipForgeSmoke(cfg *config.Config) {
	loginTestCfg = cfg

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}

	var session *equipSmokeSession
	cleanup := func() {
		session.close() // nil 安全、可重复调
	}
	fail := func(step, format string, args ...any) {
		zap.L().Error(fmt.Sprintf("EQUIP_FORGE_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}
	getPanel := func() (*scene.AttributePanelInfo, error) {
		return session.attributePanel(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
			&scene.GetAttributePanelRequest{}, &scene.GetAttributePanelResponse{})
	}

	// ---- 登录进场 ----
	opened, err := openEquipForgeSession(cfg, stats)
	if err != nil {
		fail("login", "%v", err)
	}
	session = opened
	zap.L().Info("[equip-forge-smoke] in scene", zap.Uint64("player", session.gc.PlayerId))

	// ---- 步骤 0:预备,把账号拉回已知起点(同账号可反复跑) ----
	// 上一轮结束时目标是穿着的(第 3 步之后不再卸),中途失败也可能留着:无条件全卸,第 1–2 步都按「目标在背包里」写。
	bags, err := equipForgeReadBags(session)
	if err != nil {
		fail("prep-bags", "%v", err)
	}
	for _, worn := range bags.equipment.GetItems() {
		change, err := session.unequip(worn.GetItemId())
		if err != nil {
			fail("prep-unequip", "item_id=%d %v", worn.GetItemId(), err)
		}
		switch change.tip {
		case 0, tipEquipNotEquipped:
			// 卸下了,或者它已经不在装备栏里 —— 两种都是目标状态
		case tipEquipBagFull:
			fail("prep-bag-full", "人物背包已满,卸不下上一轮留在身上的装备 item_id=%d。%s",
				worn.GetItemId(), equipForgeResetHint())
		default:
			fail("prep-unequip", "卸下 item_id=%d 被拒: tip=%s", worn.GetItemId(), equipTipText(change.tip))
		}
	}
	bags, err = equipForgeReadBags(session)
	if err != nil {
		fail("prep-bags", "%v", err)
	}
	if len(bags.equipment.GetItems()) != 0 {
		fail("prep-equipment-not-empty", "全部卸下之后装备栏仍有 %d 件", len(bags.equipment.GetItems()))
	}
	// kEquipGemNotOwned 靠「从不发放 2103」来造。包里真有的话,第 2 步那条请求会把它嵌上去 —— 在动手之前就拦住。
	if owned := equipForgeConfigCount(bags.inventory, equipForgeGemNeverGranted); owned != 0 {
		fail("prep-unexpected-gem", "人物背包里有 %d 颗 config_id=%d:本冒烟从不发放它,靠它造 kEquipGemNotOwned。%s",
			owned, equipForgeGemNeverGranted, equipForgeResetHint())
	}
	grantedWeapon := false
	target := equipForgePickTarget(bags.inventory)
	if target == nil {
		// 包里已有 1101 却一把都挑不中,而且强化上限是 0:强化没开放(EquipEnhance 表没导,或 scene 不是同批产物)。
		// 这时再发一把新的同样挑不中,只会让这个删不掉东西的账号每重跑一次多一把 —— 直接判红,不发。
		if weapons := equipBagItemsOfConfig(bags.inventory, equipForgeWeapon); len(weapons) > 0 && weapons[0].GetEnhanceMaxLevel() == 0 {
			fail("prep-target", "人物背包里已有 %d 把 config_id=%d,但 enhance_max_level = 0(强化未开放:EquipEnhance 表没导,"+
				"或 scene 不是用同批产物重启的),不再发新装备", len(weapons), equipForgeWeapon)
		}
		// 没有还能强化的 1101(新账号,或者上一把已经 +12):发一把新的。先看放不放得下,发不进去的失败信息不如这条直白
		if free := equipBagFreeSlots(bags.inventory); free < 1 {
			fail("prep-bag-space", "人物背包没有空位再发一把 config_id=%d(容量 %d;强化到上限的旧装备删不掉,会越攒越多)。%s",
				equipForgeWeapon, bags.inventory.GetLayout().GetCapacity(), equipForgeResetHint())
		}
		bag, err := equipForgeGrant(session, equipForgeWeapon, 1)
		if err != nil {
			fail("prep-grant-weapon", "%v", err)
		}
		grantedWeapon = true
		if target = equipForgePickTarget(bag); target == nil {
			fail("prep-target", "新发了一把 config_id=%d,人物背包里仍没有 enhance_level < enhance_max_level 的:"+
				"多半是 enhance_max_level = 0(EquipEnhance 表没导,或 scene 不是用同批产物重启的)", equipForgeWeapon)
		}
	}
	targetId := target.GetItemId()
	// 上一轮若死在「嵌上之后、取下之前」,宝石还在孔里:全部取下(退回人物背包),第 1–4 步都从空孔起步。
	removedGems := 0
	for _, socket := range target.GetGemSockets() {
		if socket.GetGemConfigId() == 0 {
			continue
		}
		change, err := equipForgeRemove(session, targetId, socket.GetSocket())
		if err != nil {
			fail("prep-remove-gem", "socket=%d %v", socket.GetSocket(), err)
		}
		switch change.tip {
		case 0:
			removedGems++
		case equipForgeTipGemBagFull:
			fail("prep-bag-full", "人物背包已满,取不下上一轮留在 %d 号孔里的宝石 config_id=%d。%s",
				socket.GetSocket(), socket.GetGemConfigId(), equipForgeResetHint())
		default:
			fail("prep-remove-gem", "取下 %d 号孔的宝石被拒: tip=%s", socket.GetSocket(), equipTipText(change.tip))
		}
	}
	if removedGems > 0 || grantedWeapon {
		if bags, err = equipForgeReadBags(session); err != nil {
			fail("prep-bags", "%v", err)
		}
	}
	grantedGem := false
	if equipForgeConfigCount(bags.inventory, equipForgeGemAttackL1) == 0 {
		if _, err := equipForgeGrant(session, equipForgeGemAttackL1, 1); err != nil {
			fail("prep-grant-gem", "%v", err)
		}
		grantedGem = true
		if bags, err = equipForgeReadBags(session); err != nil {
			fail("prep-bags", "%v", err)
		}
	}
	target, worn := bags.item(targetId)
	if target == nil || worn {
		fail("prep-target", "预备后目标 item_id=%d 应在人物背包里(找到=%t,穿着=%t)", targetId, target != nil, worn)
	}
	startLevel := target.GetEnhanceLevel()
	zap.L().Info("[equip-forge-smoke] step 0: prepared",
		zap.Uint64("target", targetId), zap.Uint32("enhance_level", startLevel),
		zap.Bool("granted_weapon", grantedWeapon), zap.Bool("granted_gem", grantedGem),
		zap.Int("removed_gems", removedGems), zap.Uint64("gold", equipForgeGold(bags.inventory)),
		zap.Int("bag_items", len(bags.inventory.GetItems())),
		zap.Uint32("bag_capacity", bags.inventory.GetLayout().GetCapacity()))

	// ---- 步骤 1:显示(强化预览 / 孔位 / 宝石物品) ----
	if err := checkEquipForgeTargetDisplay(target); err != nil {
		fail("display-target", "config_id=%d item_id=%d: %v。%s", target.GetConfigId(), targetId, err, equipForgeDisplayHint())
	}
	for _, socket := range target.GetGemSockets() {
		if socket.GetGemConfigId() != 0 {
			fail("display-sockets-not-empty", "预备段取下全部宝石之后,%d 号孔仍显示 gem_config_id=%d",
				socket.GetSocket(), socket.GetGemConfigId())
		}
	}
	gems := equipBagItemsOfConfig(bags.inventory, equipForgeGemAttackL1)
	if len(gems) == 0 {
		fail("display-gem", "预备后人物背包里没有 config_id=%d", equipForgeGemAttackL1)
	}
	for _, gem := range gems {
		if err := checkEquipForgeGemItem(gem, equipForgeGemTypeAttack); err != nil {
			fail("display-gem", "config_id=%d item_id=%d: %v", gem.GetConfigId(), gem.GetItemId(), err)
		}
	}
	// 第 4 步要断言「物伤 / 法伤各 + 宝石值」:只有「伤害」这个属性是两项攻击各加 N(一期设计文档 §2.1)
	if attrId := gems[0].GetGemAttr().GetAttrId(); attrId != equipAttrDamage {
		fail("display-gem-attr", "config_id=%d 的 gem_attr.attr_id=%d,应为 %d(伤害)。EquipGem 表改了的话要同步改本文件顶部的常量",
			equipForgeGemAttackL1, attrId, equipAttrDamage)
	}
	gemValue := gems[0].GetGemAttr().GetValue()
	zap.L().Info("[equip-forge-smoke] step 1: display ok",
		zap.String("target_name", target.GetName()), zap.Uint32("enhance_level", startLevel),
		zap.Uint32("enhance_max_level", target.GetEnhanceMaxLevel()),
		zap.String("sockets", equipForgeSocketsText(target)),
		zap.String("base_attrs", equipLinesText(target.GetBaseAttrs())),
		zap.String("gem_name", gems[0].GetName()), zap.Uint64("gem_value", gemValue))

	// ---- 步骤 2:拒绝码 ----
	// 每条之后都重新读两个包并要求原样不变(含金币:每份 BagInfo 都带 currency)。
	// 判定顺序(§4.3 镶嵌步骤表):是不是宝石 → 孔位 / 占用 / 类型 → 持有量。所以 2201、2103 都不必真的持有。
	rejections := []struct {
		step string
		what string
		want uint32
		send func() (equipForgeChange, error)
	}{
		{"reject-not-gem", "InlayGem(强化石)", equipForgeTipGemNotGem, func() (equipForgeChange, error) {
			return equipForgeInlay(session, targetId, equipForgeSocketAttack, equipForgeStone)
		}},
		{"reject-socket-invalid", "InlayGem(武器没有的孔)", equipForgeTipGemSocketInvalid, func() (equipForgeChange, error) {
			return equipForgeInlay(session, targetId, equipForgeAbsentSocket, equipForgeGemAttackL1)
		}},
		{"reject-type-mismatch", "InlayGem(攻击孔嵌防御宝石)", equipForgeTipGemTypeMismatch, func() (equipForgeChange, error) {
			return equipForgeInlay(session, targetId, equipForgeSocketAttack, equipForgeGemDefenseL1)
		}},
		{"reject-not-owned", "InlayGem(背包里没有的宝石)", equipForgeTipGemNotOwned, func() (equipForgeChange, error) {
			return equipForgeInlay(session, targetId, equipForgeSocketAttack, equipForgeGemNeverGranted)
		}},
		{"reject-socket-empty", "RemoveGem(空孔)", equipForgeTipGemSocketEmpty, func() (equipForgeChange, error) {
			return equipForgeRemove(session, targetId, equipForgeSocketAttack)
		}},
		// 下面两条是 handler 守护段的参数粗检(§4.4),到不了 PlayerEquipSystem
		{"reject-enhance-zero", "EnhanceEquip(item_id=0)", tipInvalidParameter, func() (equipForgeChange, error) {
			return equipForgeEnhance(session, 0)
		}},
		{"reject-inlay-zero-gem", "InlayGem(gem_config_id=0)", tipInvalidParameter, func() (equipForgeChange, error) {
			return equipForgeInlay(session, targetId, equipForgeSocketAttack, 0)
		}},
	}
	for _, rejection := range rejections {
		change, err := rejection.send()
		if err != nil {
			fail(rejection.step, "%v", err)
		}
		if change.tip != rejection.want {
			fail(rejection.step, "%s 应回 %s,实际 tip=%s(0 = 没有被拒,账号状态已被改动;下一轮的预备段会收拾)",
				rejection.what, equipTipText(rejection.want), equipTipText(change.tip))
		}
		if err := session.expectBagsUnchanged(bags.inventory, bags.equipment); err != nil {
			fail(rejection.step+"-unchanged", "%s 被拒之后: %v", rejection.what, err)
		}
	}
	zap.L().Info("[equip-forge-smoke] step 2: rejections ok", zap.Int("cases", len(rejections)))

	// ---- 步骤 3a:在背包里强化一次 ----
	if bags, err = equipForgeFund(session, bags, targetId); err != nil {
		fail("enhance-bag-fund", "%v", err)
	}
	forged, err := equipForgeEnhance(session, targetId)
	if err != nil {
		fail("enhance-bag", "%v", err)
	}
	if forged.tip != 0 {
		fail("enhance-bag", "强化背包里的装备被拒: tip=%s(强化前 +%d)", equipTipText(forged.tip), startLevel)
	}
	after := equipForgeBags{inventory: forged.bag, equipment: forged.equipment}
	if err := checkEquipForgeEnhance(bags, after, targetId, forged.success, forged.level); err != nil {
		fail("enhance-bag-reconcile", "%v", err)
	}
	target, _ = after.item(targetId)
	if err := checkEquipForgeTargetDisplay(target); err != nil {
		fail("enhance-bag-display", "强化后 item_id=%d: %v", targetId, err)
	}
	// 响应里的两个包是客户端直接拿来覆盖本地缓存的,必须就是 GetBag 读到的全量真相
	if err := session.expectBagsUnchanged(after.inventory, after.equipment); err != nil {
		fail("enhance-bag-snapshot", "强化响应里的快照与随后 GetBag 读到的不一致: %v", err)
	}
	bags = after
	bagOutcome := equipForgeOutcomeText(forged.success)
	bagLevel := forged.level
	zap.L().Info("[equip-forge-smoke] step 3a: enhanced in bag",
		zap.String("outcome", bagOutcome), zap.Uint32("level_before", startLevel), zap.Uint32("level_after", bagLevel),
		zap.Uint64("gold_left", equipForgeGold(bags.inventory)),
		zap.String("base_attrs", equipLinesText(target.GetBaseAttrs())))

	// ---- 步骤 3b:穿上(整份实例换包,强化等级不许丢) ----
	wear, err := session.equip(targetId)
	if err != nil {
		fail("equip-target", "%v", err)
	}
	if wear.tip != 0 {
		fail("equip-target", "穿上目标被拒: tip=%s", equipTipText(wear.tip))
	}
	if err := checkEquipSameItem(wear.equipment, target); err != nil {
		fail("equip-target-kept", "穿上后强化等级 / 孔位不该变: %v", err)
	}
	if equipBagItem(wear.bag, targetId) != nil {
		fail("equip-target-kept", "item_id=%d 已穿上,却还留在人物背包里(实例被复制)", targetId)
	}
	panel, err := session.panelAfter(wear)
	if err != nil {
		fail("equip-target-panel", "%v", err)
	}
	bags = equipForgeBags{inventory: wear.bag, equipment: wear.equipment}
	wornAttack := panel.GetDerived().GetPhysicalAttack()

	// ---- 步骤 3c:穿着再强化一次 ----
	wornOutcome := "max-level"
	if target.GetEnhanceNext() == nil {
		// 3a 恰好把目标推到了上限(每十几轮才遇到一次):没有下一级可强化。这一轮改验 kEquipEnhanceMaxLevel ——
		// 平时机器人造不出这个状态(强化不可逆、没有删物手段),碰上了就别浪费。
		forged, err = equipForgeEnhance(session, targetId)
		if err != nil {
			fail("enhance-worn-max", "%v", err)
		}
		if forged.tip != equipForgeTipEnhanceMaxLevel {
			fail("enhance-worn-max", "目标已是 +%d(上限 %d),再强化应回 %s,实际 tip=%s",
				target.GetEnhanceLevel(), target.GetEnhanceMaxLevel(),
				equipTipText(equipForgeTipEnhanceMaxLevel), equipTipText(forged.tip))
		}
		if err := session.expectBagsUnchanged(bags.inventory, bags.equipment); err != nil {
			fail("enhance-worn-max-unchanged", "%v", err)
		}
	} else {
		if bags, err = equipForgeFund(session, bags, targetId); err != nil {
			fail("enhance-worn-fund", "%v", err)
		}
		forged, err = equipForgeEnhance(session, targetId)
		if err != nil {
			fail("enhance-worn", "%v", err)
		}
		if forged.tip != 0 {
			fail("enhance-worn", "强化穿着的装备被拒: tip=%s(强化前 +%d)", equipTipText(forged.tip), target.GetEnhanceLevel())
		}
		after = equipForgeBags{inventory: forged.bag, equipment: forged.equipment}
		if err := checkEquipForgeEnhance(bags, after, targetId, forged.success, forged.level); err != nil {
			fail("enhance-worn-reconcile", "%v", err)
		}
		enhanced, _ := after.item(targetId)
		if err := checkEquipForgeTargetDisplay(enhanced); err != nil {
			fail("enhance-worn-display", "强化后 item_id=%d: %v", targetId, err)
		}
		if forged.level != target.GetEnhanceLevel() {
			// 穿着且等级变了:服务器重算并推面板。武器的基础属性是「伤害」,物伤 / 法伤恰好各变这一行的差值
			delta := int64(equipForgeBaseValue(enhanced, equipAttrDamage)) - int64(equipForgeBaseValue(target, equipAttrDamage))
			now, err := session.panelAfter(forged.equipChange)
			if err != nil {
				fail("enhance-worn-panel", "强化穿着的装备且等级变了(+%d -> +%d): %v", target.GetEnhanceLevel(), forged.level, err)
			}
			if err := checkEquipForgePanelShift(panel, now, delta); err != nil {
				fail("enhance-worn-panel-shift", "%v", err)
			}
			panel = now
		} else {
			// 等级没变就不重算、不推送 —— 这里**不能**等推送(会等满超时)。主动取一份,要求与强化前逐项相等
			now, err := getPanel()
			if err != nil {
				fail("enhance-worn-panel", "%v", err)
			}
			if diff := equipPanelDiff(panel, now); len(diff) != 0 {
				fail("enhance-worn-panel-changed", "强化失败且等级没变,面板却变了: %s", strings.Join(diff, "; "))
			}
		}
		target = enhanced
		bags = after
		wornOutcome = equipForgeOutcomeText(forged.success)
	}
	zap.L().Info("[equip-forge-smoke] step 3: enhanced while worn",
		zap.String("outcome", wornOutcome), zap.Uint32("level", target.GetEnhanceLevel()),
		zap.Uint64("physical_attack_worn", wornAttack),
		zap.Uint64("physical_attack_now", panel.GetDerived().GetPhysicalAttack()))

	// ---- 步骤 4:镶嵌 ----
	gemsBeforeInlay := equipForgeConfigCount(bags.inventory, equipForgeGemAttackL1)
	forged, err = equipForgeInlay(session, targetId, equipForgeSocketAttack, equipForgeGemAttackL1)
	if err != nil {
		fail("inlay", "%v", err)
	}
	if forged.tip != 0 {
		fail("inlay", "%d 号孔嵌 config_id=%d 被拒: tip=%s", equipForgeSocketAttack, equipForgeGemAttackL1, equipTipText(forged.tip))
	}
	after = equipForgeBags{inventory: forged.bag, equipment: forged.equipment}
	if err := checkEquipForgeInlay(bags, after, targetId, equipForgeSocketAttack, equipForgeGemAttackL1); err != nil {
		fail("inlay-reconcile", "%v", err)
	}
	// 镶嵌 / 取下穿着的装备一定重算并推面板
	inlaidPanel, err := session.panelAfter(forged.equipChange)
	if err != nil {
		fail("inlay-panel", "镶嵌穿着的装备之后: %v", err)
	}
	if err := checkEquipForgePanelShift(panel, inlaidPanel, int64(gemValue)); err != nil {
		fail("inlay-panel-shift", "%v", err)
	}
	bags = after
	forged, err = equipForgeInlay(session, targetId, equipForgeSocketAttack, equipForgeGemAttackL1)
	if err != nil {
		fail("inlay-occupied", "%v", err)
	}
	if forged.tip != equipForgeTipGemSocketOccupied {
		fail("inlay-occupied", "已镶的孔再嵌应回 %s,实际 tip=%s",
			equipTipText(equipForgeTipGemSocketOccupied), equipTipText(forged.tip))
	}
	// 同时核对「镶嵌响应里的两个包」就是 GetBag 读到的全量
	if err := session.expectBagsUnchanged(bags.inventory, bags.equipment); err != nil {
		fail("inlay-occupied-unchanged", "%v", err)
	}
	target, _ = bags.item(targetId)
	zap.L().Info("[equip-forge-smoke] step 4: gem inlaid",
		zap.String("sockets", equipForgeSocketsText(target)), zap.Uint64("gem_value", gemValue),
		zap.Uint64("physical_attack_before", panel.GetDerived().GetPhysicalAttack()),
		zap.Uint64("physical_attack_after", inlaidPanel.GetDerived().GetPhysicalAttack()),
		zap.Uint64("magic_attack_after", inlaidPanel.GetDerived().GetMagicAttack()))

	// ---- 步骤 5:下线重登,强化等级与宝石往返 ----
	// 强化等级与宝石都在 EquipInstanceData 里,随 ItemEntry.equip 整份落库;加成不落库,加载时按实例重算。
	reborn, err := equipForgeRelogin(session, cfg)
	if err != nil {
		fail("relogin", "%v", err)
	}
	session = reborn
	restored, err := equipForgeReadBags(session)
	if err != nil {
		fail("relogin-bags", "%v", err)
	}
	if err := checkEquipForgeRestored(bags, restored, targetId); err != nil {
		fail("relogin-restored", "%v", err)
	}
	restoredPanel, err := getPanel()
	if err != nil {
		fail("relogin-panel", "%v", err)
	}
	if diff := equipPanelDiff(inlaidPanel, restoredPanel); len(diff) != 0 {
		fail("relogin-panel-mismatch", "重登后面板与下线前不一致(强化与宝石的加成应在加载时重算出同样的值): %s",
			strings.Join(diff, "; "))
	}
	bags = restored
	zap.L().Info("[equip-forge-smoke] step 5: enhance level and gem survived relogin",
		zap.Uint64("player", session.gc.PlayerId), zap.Uint32("enhance_level", target.GetEnhanceLevel()),
		zap.Uint64("physical_attack", restoredPanel.GetDerived().GetPhysicalAttack()))

	// ---- 步骤 6:取下 ----
	forged, err = equipForgeRemove(session, targetId, equipForgeSocketAttack)
	if err != nil {
		fail("remove", "%v", err)
	}
	if forged.tip != 0 {
		fail("remove", "取下 %d 号孔的宝石被拒: tip=%s", equipForgeSocketAttack, equipTipText(forged.tip))
	}
	after = equipForgeBags{inventory: forged.bag, equipment: forged.equipment}
	if err := checkEquipForgeRemove(bags, after, targetId, equipForgeSocketAttack); err != nil {
		fail("remove-reconcile", "%v", err)
	}
	if got := equipForgeConfigCount(after.inventory, equipForgeGemAttackL1); got != gemsBeforeInlay {
		fail("remove-gem-count", "取下后人物背包里 config_id=%d 应回到镶嵌前的 %d 颗,实际 %d 颗",
			equipForgeGemAttackL1, gemsBeforeInlay, got)
	}
	finalPanel, err := session.panelAfter(forged.equipChange)
	if err != nil {
		fail("remove-panel", "取下穿着的装备上的宝石之后: %v", err)
	}
	if diff := equipPanelDiff(panel, finalPanel); len(diff) != 0 {
		fail("remove-not-reversible", "取下后面板应恰好回到镶嵌前(镶嵌前 -> 现在): %s", strings.Join(diff, "; "))
	}
	zap.L().Info("[equip-forge-smoke] step 6: gem removed, panel back to before inlay")

	zap.L().Info(fmt.Sprintf(
		"EQUIP_FORGE_SMOKE_OK player=%d steps=6 target=%d granted_weapon=%t enhance_level=%d->%d->%d "+
			"enhance_in_bag=%s enhance_worn=%s gem=%d value=%d physical_attack=%d->%d->%d",
		session.gc.PlayerId, targetId, grantedWeapon, startLevel, bagLevel, target.GetEnhanceLevel(),
		bagOutcome, wornOutcome, equipForgeGemAttackL1, gemValue,
		panel.GetDerived().GetPhysicalAttack(), inlaidPanel.GetDerived().GetPhysicalAttack(),
		finalPanel.GetDerived().GetPhysicalAttack()))
	cleanup()
	_ = zap.L().Sync()
}

// ---------------------------------------------------------------------------
// 会话辅助
// ---------------------------------------------------------------------------

// openEquipForgeSession 取 gate、用 equipForgeAccount 登录进场(账号没有角色时自动建角),返回新会话。
//
// 会话类型直接用 equipSmokeSession:call / getBag / gmGrant / 同号节流 / close 都在它上面,不另造一份。
// 唯独「登哪个账号」借不了 —— openEquipSmokeSession 把账号写死成 equipSmokeAccount,而两条冒烟必须分号
// (见 equipForgeAccount)。
func openEquipForgeSession(cfg *config.Config, stats *metrics.Stats) (*equipSmokeSession, error) {
	return openEquipSessionAs(cfg, stats, equipForgeAccount)
}

// equipForgeRelogin 下线后用同一账号重新登录进场,返回新会话(旧会话此后不可再用)。
// **不能调 session.relogin**:它内部走 openEquipSmokeSession,重登上来的会是 equip-smoke 的账号。
// 中间等 2s 的理由与它相同:让 scene 的异步存盘落到 db,否则重登可能读到旧档。
func equipForgeRelogin(s *equipSmokeSession, cfg *config.Config) (*equipSmokeSession, error) {
	stats := s.stats
	s.close()
	time.Sleep(2 * time.Second)
	return openEquipForgeSession(cfg, stats)
}

// equipForgeReadBags 读人物背包 + 装备栏两份全量快照。
func equipForgeReadBags(s *equipSmokeSession) (equipForgeBags, error) {
	inventory, err := s.getBag(equipSmokeBagInventory)
	if err != nil {
		return equipForgeBags{}, err
	}
	equipment, err := s.getBag(equipSmokeBagEquipment)
	if err != nil {
		return equipForgeBags{}, err
	}
	return equipForgeBags{inventory: inventory, equipment: equipment}, nil
}

// equipForgeRequest 发一次强化 / 镶嵌 / 取下并等**它自己的**响应(认领方式见 equipSmokeSession.call)。
// 业务拒绝不是 error:结果里 tip != 0、两个包为 nil。被受理的响应必须带回两份全量快照,
// 且每份都先过 checkEquipForgeNoEmptyStacks(空堆必须已回收)再过快照自洽检查。
func equipForgeRequest(s *equipSmokeSession, messageId uint32, request proto.Message) (equipForgeChange, error) {
	// 发请求之前记下面板序号:改的是穿着的装备时服务器会推一份新面板,panelAfter 靠这个游标认出它
	_, panelSeq := s.player.GetAttributePanel()
	body, tip, err := s.call(messageId, request)
	if err != nil {
		return equipForgeChange{}, err
	}
	change := equipForgeChange{equipChange: equipChange{tip: tip, panelSeq: panelSeq}}
	if tip != 0 {
		return change, nil
	}
	response, ok := body.(equipBagsResponse)
	if !ok {
		return equipForgeChange{}, fmt.Errorf("message_id=%d 的响应类型不符: %T", messageId, body)
	}
	for _, snapshot := range []struct {
		bag     *scene.BagInfo
		bagType uint32
	}{
		{response.GetBag(), equipSmokeBagInventory},
		{response.GetEquipment(), equipSmokeBagEquipment},
	} {
		if err := checkEquipForgeNoEmptyStacks(snapshot.bag); err != nil {
			return equipForgeChange{}, fmt.Errorf("message_id=%d 响应里 bag_type=%d 的快照: %w", messageId, snapshot.bagType, err)
		}
		if err := checkEquipBagSnapshot(snapshot.bag, snapshot.bagType); err != nil {
			return equipForgeChange{}, err
		}
	}
	change.bag, change.equipment = response.GetBag(), response.GetEquipment()
	if enhanced, ok := body.(*scene.EnhanceEquipResponse); ok {
		change.success, change.level = enhanced.GetSuccess(), enhanced.GetEnhanceLevel()
	}
	return change, nil
}

func equipForgeEnhance(s *equipSmokeSession, itemId uint64) (equipForgeChange, error) {
	return equipForgeRequest(s, game.SceneBagClientPlayerEnhanceEquipMessageId, &scene.EnhanceEquipRequest{ItemId: itemId})
}

func equipForgeInlay(s *equipSmokeSession, itemId uint64, socket, gemConfigId uint32) (equipForgeChange, error) {
	return equipForgeRequest(s, game.SceneBagClientPlayerInlayGemMessageId,
		&scene.InlayGemRequest{ItemId: itemId, Socket: socket, GemConfigId: gemConfigId})
}

func equipForgeRemove(s *equipSmokeSession, itemId uint64, socket uint32) (equipForgeChange, error) {
	return equipForgeRequest(s, game.SceneBagClientPlayerRemoveGemMessageId,
		&scene.RemoveGemRequest{ItemId: itemId, Socket: socket})
}

// equipForgeGrant 用 GmGrantItem 往人物背包发 count 个 configId(装备 = count 个独立实例;材料 / 宝石 = 叠加数量),
// 返回发完之后的人物背包全量。任何拒绝都翻成带处置建议的 error。
func equipForgeGrant(s *equipSmokeSession, configId uint32, count uint64) (*scene.BagInfo, error) {
	if count == 0 || count > equipForgeGrantMaxCount {
		return nil, fmt.Errorf("GmGrantItem(config_id=%d) 要发 %d 个,超出单次上限 %d(表里这一级的材料数量改大了?)",
			configId, count, equipForgeGrantMaxCount)
	}
	bag, tip, err := s.gmGrant(configId, uint32(count))
	if err != nil {
		return nil, fmt.Errorf("GmGrantItem(config_id=%d, count=%d): %w", configId, count, err)
	}
	if tip == tipEquipGrantInvalid {
		return nil, fmt.Errorf("GmGrantItem(config_id=%d) 回 kEquipGrantInvalid:Item 表里没有这一行,"+
			"需先导表(Item 表 2000 段的强化石 / 宝石新行 + 导表器)并重启 scene", configId)
	}
	if tip != 0 {
		return nil, fmt.Errorf("GmGrantItem(config_id=%d, count=%d) 被拒: tip=%s(若是背包放不下:%s)",
			configId, count, equipTipText(tip), equipForgeResetHint())
	}
	return bag, nil
}

// equipForgeAddGold 用 GmAddCurrency 加金币。
// gmAddGold 是各冒烟共用的 helper,不走 equipSmokeSession.call,所以这里手动过一次同号节流(同一张表、同一个间隔)。
// gate GM 闸(信封)、scene GM 闸 / 冻结(响应体 tip)与超时,gmAddGold 都直接返回 error,这里原样带上去。
// 返回的 balance_after 不看:对账以随后重新读的背包快照为准(每份 BagInfo 都带 currency),见 equipForgeFund。
func equipForgeAddGold(s *equipSmokeSession, amount int64) error {
	s.throttle(game.SceneCurrencyClientPlayerGmAddCurrencyMessageId)
	if _, err := gmAddGold(s.gc, s.player, s.stats, amount, equipSmokeRpcTimeout); err != nil {
		return fmt.Errorf("gold +%d: %w", amount, err)
	}
	return nil
}

// equipForgeFund 让人物背包够强化 itemId 一次:按 bags 里目标的 enhance_next「缺多少补多少」
// (金币走 GmAddCurrency,材料走 GmGrantItem),返回补完之后**重新读**的两份快照 —— 强化对账的「强化前」必须是它:
// 每份 BagInfo 都带 currency,补金币会让两份快照都变。恰好补齐而不多发,账号上的金币与材料就不会越跑越多。
func equipForgeFund(s *equipSmokeSession, bags equipForgeBags, itemId uint64) (equipForgeBags, error) {
	target, _ := bags.item(itemId)
	next := target.GetEnhanceNext()
	if target == nil || next == nil {
		return equipForgeBags{}, fmt.Errorf("item_id=%d 不在两个包里,或不带 enhance_next(没有下一级可强化)", itemId)
	}
	gold, material := equipForgeShortfall(bags.inventory, next)
	if gold == 0 && material == 0 {
		return bags, nil
	}
	if gold > math.MaxInt64 {
		return equipForgeBags{}, fmt.Errorf("enhance_next.cost_gold=%d 超出 GmAddCurrency 的 int64 金额", next.GetCostGold())
	}
	if gold > 0 {
		if err := equipForgeAddGold(s, int64(gold)); err != nil {
			return equipForgeBags{}, err
		}
	}
	if material > 0 {
		if _, err := equipForgeGrant(s, next.GetMaterialConfigId(), material); err != nil {
			return equipForgeBags{}, err
		}
	}
	funded, err := equipForgeReadBags(s)
	if err != nil {
		return equipForgeBags{}, err
	}
	// GM 指令被拒(gate / scene 的 GM 闸)在上面就已经是 error;走到这里是两条指令都回了成功,背包快照却没补够
	if gold, material := equipForgeShortfall(funded.inventory, next); gold != 0 || material != 0 {
		return equipForgeBags{}, fmt.Errorf("补完之后仍缺 金币 %d / 材料(config_id=%d)%d 个:GM 指令回了成功,"+
			"背包快照里却没补够(scene 发物 / 加金币没落到人物背包?)", gold, next.GetMaterialConfigId(), material)
	}
	return funded, nil
}

// equipForgeResetHint 是「账号被占满 / 被污染,脚本自己收拾不了」时的统一处置建议。
func equipForgeResetHint() string {
	return fmt.Sprintf("请清号后重跑:清掉 %s 的背包存档,或把 equipForgeAccount 换成一个新账号", equipForgeAccount)
}

// equipForgeDisplayHint 给第 1 步(display-target)的失败信息补一句怎么修。
// enhance_max_level / enhance_next / 孔位定义每次按表现算,导表并重启 scene 即生效;
// 但「实例未初始化」(一期掷值挂点没装上的 scene 铸出来的,服务器对它不下发 enhance_next 与 gem_sockets)
// 不会自己变好,而预备段又是「已有就复用」,所以那种情况修好服务端之后还得换号。
func equipForgeDisplayHint() string {
	return fmt.Sprintf("红在 enhance_max_level / enhance_next / 孔位的话,先确认 EquipEnhance / EquipGem / EquipGemSocket 三张表已导、"+
		"scene 是用同批产物重启的;若这把装备是在掷值挂点没生效的 scene 上铸的(实例未初始化:不带 enhance_next 与 gem_sockets),"+
		"修好之后还要把 equipForgeAccount(现为 %s)换成新账号、或清掉该账号的背包存档再重跑", equipForgeAccount)
}

func equipForgeOutcomeText(success bool) string {
	if success {
		return "success"
	}
	return "fail"
}

// ---------------------------------------------------------------------------
// 取值与对账(纯函数,单测见 equip_forge_smoke_scenario_test.go)
// ---------------------------------------------------------------------------

// equipForgeGold 读一份背包快照里的金币(bag.currency.values[金币])。
// repeated 可以比货币种类短,玩家还没有货币组件时整个 currency 都不带:两种都按 0。
func equipForgeGold(bag *scene.BagInfo) uint64 {
	values := bag.GetCurrency().GetValues()
	if int(currencyTypeGold) >= len(values) {
		return 0
	}
	return values[currencyTypeGold]
}

// equipForgeConfigCounts 按 config 汇总一个包里的数量。材料 / 宝石可能分几堆,服务器也是按 config 扣的,所以对账只认汇总。
func equipForgeConfigCounts(bag *scene.BagInfo) map[uint32]uint64 {
	counts := make(map[uint32]uint64)
	for _, item := range bag.GetItems() {
		counts[item.GetConfigId()] += uint64(item.GetCount())
	}
	return counts
}

func equipForgeConfigCount(bag *scene.BagInfo, configId uint32) uint64 {
	return equipForgeConfigCounts(bag)[configId]
}

// equipForgeCountsDiff 返回两份「config → 数量」不相等的项(空 = 一致)。缺键与 0 等价。
func equipForgeCountsDiff(want, got map[uint32]uint64) []string {
	seen := make(map[uint32]bool, len(want)+len(got))
	ids := make([]uint32, 0, len(want)+len(got))
	for _, counts := range []map[uint32]uint64{want, got} {
		for id := range counts {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var diff []string
	for _, id := range ids {
		if want[id] != got[id] {
			diff = append(diff, fmt.Sprintf("config_id=%d 应有 %d 实际 %d", id, want[id], got[id]))
		}
	}
	return diff
}

// equipForgeScale 镜像 equiprules::ScaleByPermyriad(设计文档 §4.2):基础值按万分比放大,
// 增量向下取整、bonus = 0 严格恒等、饱和不回绕。拆成「整万 + 余数」两段再乘,中间值不溢出。
// 锚点:(40, 500) = 42;(12, 500) = 12;(19999, 5000) = 29998。
func equipForgeScale(base uint64, permyriad uint32) uint64 {
	bonus := uint64(permyriad)
	whole := base / 10000
	increase := uint64(math.MaxUint64)
	if bonus == 0 || whole <= math.MaxUint64/bonus {
		increase = whole * bonus
	}
	// 余数段最大 9999 × (2^32 − 1),不溢出
	if part := base % 10000 * bonus / 10000; increase > math.MaxUint64-part {
		increase = math.MaxUint64
	} else {
		increase += part
	}
	if base > math.MaxUint64-increase {
		return math.MaxUint64
	}
	return base + increase
}

// equipForgeLevelAfterFailure 镜像 equiprules::LevelAfterFailure:强化失败后的等级,最多掉到 0。
func equipForgeLevelAfterFailure(level, failLevelLoss uint32) uint32 {
	if level > failLevelLoss {
		return level - failLevelLoss
	}
	return 0
}

// equipForgePickTarget 挑强化目标:人物背包里 item_id 最小、且还没强化到上限的 1101;没有返回 nil。
// 按 item_id 定序是为了同一个账号每一轮都挑到同一把(BagInfo.items 的顺序没有语义)。
// enhance_max_level = 0(强化未开放)时谁也挑不中。
func equipForgePickTarget(inventory *scene.BagInfo) *scene.BagItemInfo {
	for _, weapon := range equipBagItemsOfConfig(inventory, equipForgeWeapon) {
		if weapon.GetEnhanceLevel() < weapon.GetEnhanceMaxLevel() {
			return weapon
		}
	}
	return nil
}

// equipForgeShortfall 算「照 next 强化一次,人物背包还缺多少」:缺的金币、缺的材料个数(材料是 next.material_config_id)。
func equipForgeShortfall(inventory *scene.BagInfo, next *scene.EquipEnhancePreview) (gold, material uint64) {
	if have := equipForgeGold(inventory); have < next.GetCostGold() {
		gold = next.GetCostGold() - have
	}
	if next.GetMaterialConfigId() != 0 {
		need := uint64(next.GetMaterialCount())
		if have := equipForgeConfigCount(inventory, next.GetMaterialConfigId()); have < need {
			material = need - have
		}
	}
	return gold, material
}

// equipForgeSocketOf 返回装备上某个孔位的下发记录;该部位没有这个孔(或实例未初始化)时返回 nil。
func equipForgeSocketOf(item *scene.BagItemInfo, socket uint32) *scene.EquipGemSocketInfo {
	for _, info := range item.GetGemSockets() {
		if info.GetSocket() == socket {
			return info
		}
	}
	return nil
}

// equipForgeSocketsText 把孔位压成一行日志:孔位号:t类型=宝石config(0 = 空孔),不生效的宝石后面带 !。
func equipForgeSocketsText(item *scene.BagItemInfo) string {
	parts := make([]string, 0, len(item.GetGemSockets()))
	for _, socket := range item.GetGemSockets() {
		part := fmt.Sprintf("%d:t%d=%d", socket.GetSocket(), socket.GetGemType(), socket.GetGemConfigId())
		if socket.GetGemConfigId() != 0 && socket.GetAttr() == nil {
			part += "!"
		}
		parts = append(parts, part)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// equipForgeBaseValue 返回装备基础属性里某个属性的当前值(已含强化增量);没有这一行返回 0。
func equipForgeBaseValue(item *scene.BagItemInfo, attrId uint32) uint64 {
	var total uint64
	for _, line := range item.GetBaseAttrs() {
		if line.GetAttrId() == attrId {
			total += line.GetValue()
		}
	}
	return total
}

// equipForgeItemSlot 返回一件物品在包里占的槽号;没摆放(或不在这个包里)返回 -1。
func equipForgeItemSlot(bag *scene.BagInfo, itemId uint64) int64 {
	for _, entry := range bag.GetLayout().GetSlots() {
		if entry.GetItemId() == itemId {
			return int64(entry.GetSlot())
		}
	}
	return -1
}

// equipForgeWithoutItem 返回去掉某件物品的显示数据与 currency 之后的包快照副本(布局原样保留)。
// 用来比「这件之外的一切」:它自己的字段另有专门的断言,金币另算。
func equipForgeWithoutItem(bag *scene.BagInfo, itemId uint64) *scene.BagInfo {
	if bag == nil {
		return nil
	}
	stripped := proto.Clone(bag).(*scene.BagInfo)
	var others []*scene.BagItemInfo
	for _, item := range stripped.GetItems() {
		if item.GetItemId() != itemId {
			others = append(others, item)
		}
	}
	stripped.Items = others
	stripped.Currency = nil
	return stripped
}

// equipForgeEnhanceBlind 返回抹掉「随强化等级变化的字段」之后的副本:强化只许动这几个字段,其余必须逐字段相同。
func equipForgeEnhanceBlind(item *scene.BagItemInfo) *scene.BagItemInfo {
	blind := proto.Clone(item).(*scene.BagItemInfo)
	blind.EnhanceLevel = 0
	blind.EnhanceBonusPermyriad = 0
	blind.EnhanceNext = nil
	blind.BaseAttrs = nil
	return blind
}

// equipForgeSocketBlind 返回抹掉某个孔位之后的副本:镶嵌 / 取下只许动那一个孔,其余必须逐字段相同。
func equipForgeSocketBlind(item *scene.BagItemInfo, socket uint32) *scene.BagItemInfo {
	blind := proto.Clone(item).(*scene.BagItemInfo)
	var others []*scene.EquipGemSocketInfo
	for _, info := range blind.GetGemSockets() {
		if info.GetSocket() != socket {
			others = append(others, info)
		}
	}
	blind.GemSockets = others
	return blind
}

// checkEquipForgeNoEmptyStacks 要求快照里没有 count = 0 的条目。
// 强化扣材料、镶嵌扣宝石都可能把一堆扣到 0;服务器必须在回包前用 MergeAndCompact(kMergeOnly) 回收,
// 否则客户端背包里会多出一个数量为 0 的格子(设计文档 §4.3 强化第 13 步 / 镶嵌第 9 步)。
func checkEquipForgeNoEmptyStacks(bag *scene.BagInfo) error {
	for _, item := range bag.GetItems() {
		if item.GetCount() == 0 {
			return fmt.Errorf("item_id=%d config_id=%d 的 count = 0:扣到 0 的空堆没有回收", item.GetItemId(), item.GetConfigId())
		}
	}
	return nil
}

// checkEquipForgeGemAttr 校验一条宝石属性行(孔位上的 attr / 宝石物品的 gem_attr,设计文档 §3.3):
// 有属性 id 与名称、value > 0;tier / cap / seq 恒 0(宝石不分颜色档、没有上限、不排序)。
func checkEquipForgeGemAttr(attr *scene.EquipAttrLineInfo) error {
	if attr.GetAttrId() == 0 || attr.GetName() == "" {
		return fmt.Errorf("attr_id=%d name=%q:id 与名称都不能为空", attr.GetAttrId(), attr.GetName())
	}
	if attr.GetValue() == 0 {
		return fmt.Errorf("attr_id=%d 的 value = 0", attr.GetAttrId())
	}
	if attr.GetTier() != 0 || attr.GetCap() != 0 || attr.GetSeq() != 0 {
		return fmt.Errorf("attr_id=%d tier=%d cap=%d seq=%d,宝石属性行这三项应恒为 0",
			attr.GetAttrId(), attr.GetTier(), attr.GetCap(), attr.GetSeq())
	}
	return nil
}

// checkEquipForgeGemItem 校验人物背包里的一堆宝石物品:可叠加的非装备、有名称、gem_type 对、带 gem_attr,
// 且不带任何装备才有的强化 / 孔位字段。
func checkEquipForgeGemItem(item *scene.BagItemInfo, wantType uint32) error {
	if item.GetEquipKind() != 0 || item.GetMaxStack() <= 1 {
		return fmt.Errorf("宝石应是可叠加的非装备物品: equip_kind=%d max_stack=%d", item.GetEquipKind(), item.GetMaxStack())
	}
	if item.GetName() == "" {
		return fmt.Errorf("name 为空(Item 表的宝石行没有数据,需先导表)")
	}
	if item.GetGemType() != wantType {
		return fmt.Errorf("gem_type=%d,应为 %d(0 = 服务器不认为它是宝石:EquipGem 表没导?)", item.GetGemType(), wantType)
	}
	if item.GetGemAttr() == nil {
		return fmt.Errorf("不带 gem_attr(EquipGem 表这一行的属性不存在或不被规则层接受)")
	}
	if err := checkEquipForgeGemAttr(item.GetGemAttr()); err != nil {
		return fmt.Errorf("gem_attr: %w", err)
	}
	if item.GetEnhanceLevel() != 0 || item.GetEnhanceMaxLevel() != 0 || item.GetEnhanceBonusPermyriad() != 0 ||
		item.GetEnhanceNext() != nil || len(item.GetGemSockets()) != 0 {
		return fmt.Errorf("非装备物品却带着强化 / 孔位字段: enhance_level=%d enhance_max_level=%d 孔位 %d 个",
			item.GetEnhanceLevel(), item.GetEnhanceMaxLevel(), len(item.GetGemSockets()))
	}
	return nil
}

// checkEquipForgeSockets 校验一件装备下发的孔位(设计文档 §3.3 gem_sockets):与该部位的孔位定义逐个对上
// (个数、孔位号升序、接纳类型),名称非空;空孔不带宝石名与属性;已镶的孔若带属性,属性行要合规。
// 「已镶但不带 attr」是合法状态(已镶但当前不生效),生效与否由镶嵌那一步的对账另行断言。
func checkEquipForgeSockets(sockets []*scene.EquipGemSocketInfo, want []equipForgeSocketDef) error {
	if len(sockets) != len(want) {
		return fmt.Errorf("应恰有 %d 个孔,实际 %d 个(0 个 = 实例未初始化,或 EquipGemSocket 表没导)", len(want), len(sockets))
	}
	for index, socket := range sockets {
		def := want[index]
		if socket.GetSocket() != def.socket || socket.GetGemType() != def.gemType {
			return fmt.Errorf("第 %d 个孔是 socket=%d gem_type=%d,应为 socket=%d gem_type=%d(按 socket 升序下发)",
				index, socket.GetSocket(), socket.GetGemType(), def.socket, def.gemType)
		}
		if socket.GetName() == "" {
			return fmt.Errorf("%d 号孔的名称为空(EquipGemSocket 表的 name 列没有数据?)", socket.GetSocket())
		}
		if socket.GetGemConfigId() == 0 {
			if socket.GetAttr() != nil || socket.GetGemName() != "" {
				return fmt.Errorf("%d 号孔是空孔(gem_config_id = 0),却带着宝石名 %q 或属性", socket.GetSocket(), socket.GetGemName())
			}
			continue
		}
		if socket.GetAttr() != nil {
			if err := checkEquipForgeGemAttr(socket.GetAttr()); err != nil {
				return fmt.Errorf("%d 号孔的 attr: %w", socket.GetSocket(), err)
			}
		}
	}
	return nil
}

// checkEquipForgeBaseLines 校验强化之后的基础属性行(设计文档 §3.3 / §4.3 FillItemDisplay / §7 第 3 步):
//
//	与表里的基础属性逐行对上(顺序、属性 id);tier = 0、cap = 0、seq 从 0 递增、名称非空;
//	value − enhance_bonus = 表值(强化只加增量,不改表值);
//	enhance_bonus = 表值 × enhance_bonus_permyriad / 10000 向下取整(= ScaleByPermyriad 的增量);
//	带 enhance_next 时 enhance_next_value = 按下一级万分比放大后的值,不带时为 0。
func checkEquipForgeBaseLines(item *scene.BagItemInfo, want []equipBaseAttr) error {
	lines := item.GetBaseAttrs()
	if len(lines) != len(want) {
		return fmt.Errorf("应恰有 %d 行,实际 %d 行", len(want), len(lines))
	}
	next := item.GetEnhanceNext()
	for index, line := range lines {
		expected := want[index]
		if line.GetAttrId() != expected.attrId {
			return fmt.Errorf("第 %d 行应是 attr_id=%d,实际 attr_id=%d", index, expected.attrId, line.GetAttrId())
		}
		if line.GetTier() != equipTierBase || line.GetCap() != 0 || line.GetSeq() != uint32(index) || line.GetName() == "" {
			return fmt.Errorf("第 %d 行 tier=%d cap=%d seq=%d name=%q,基础属性应为 tier=0 cap=0、seq 从 0 递增、名称非空",
				index, line.GetTier(), line.GetCap(), line.GetSeq(), line.GetName())
		}
		if line.GetValue() < line.GetEnhanceBonus() || line.GetValue()-line.GetEnhanceBonus() != expected.value {
			return fmt.Errorf("第 %d 行 value=%d enhance_bonus=%d,value − enhance_bonus 应等于表值 %d",
				index, line.GetValue(), line.GetEnhanceBonus(), expected.value)
		}
		if bonus := equipForgeScale(expected.value, item.GetEnhanceBonusPermyriad()) - expected.value; line.GetEnhanceBonus() != bonus {
			return fmt.Errorf("第 %d 行 enhance_bonus=%d,应为 表值 %d × %d / 10000(向下取整)= %d",
				index, line.GetEnhanceBonus(), expected.value, item.GetEnhanceBonusPermyriad(), bonus)
		}
		preview := uint64(0)
		if next != nil {
			preview = equipForgeScale(expected.value, next.GetBonusPermyriad())
		}
		if line.GetEnhanceNextValue() != preview {
			return fmt.Errorf("第 %d 行 enhance_next_value=%d,应为 %d(带 enhance_next 时 = 表值按下一级万分比放大,不带时为 0)",
				index, line.GetEnhanceNextValue(), preview)
		}
	}
	return nil
}

// checkEquipForgeTargetDisplay 校验目标装备(1101)的强化 / 镶嵌显示字段(设计文档 §7 第 1 步;强化之后复查用的也是它):
//
//	enhance_max_level = 12、enhance_level 不超过上限、0 级的总加成万分比为 0;
//	未满级必带 enhance_next:next_level = enhance_level + 1、success_rate 在 1..10000、总加成不比当前小;满级不带;
//	基础属性恰一行「伤害」且按表值 40 对得上(checkEquipForgeBaseLines);
//	两个孔:0 号只收类型 1、1 号任意,名称非空(checkEquipForgeSockets)。
func checkEquipForgeTargetDisplay(item *scene.BagItemInfo) error {
	if item.GetConfigId() != equipForgeWeapon || item.GetEquipKind() == 0 || item.GetCount() != 1 {
		return fmt.Errorf("目标应是一件 config_id=%d 的装备: config_id=%d equip_kind=%d count=%d",
			equipForgeWeapon, item.GetConfigId(), item.GetEquipKind(), item.GetCount())
	}
	level, limit := item.GetEnhanceLevel(), item.GetEnhanceMaxLevel()
	if limit != equipForgeMaxEnhanceLevel {
		return fmt.Errorf("enhance_max_level=%d,应为 %d(0 = 强化未开放;EquipEnhance 表的行数改了的话要同步改本文件顶部的常量)",
			limit, equipForgeMaxEnhanceLevel)
	}
	if level > limit {
		return fmt.Errorf("enhance_level=%d 超过 enhance_max_level=%d(下发的等级应已按表上限夹取)", level, limit)
	}
	if level == 0 && item.GetEnhanceBonusPermyriad() != 0 {
		return fmt.Errorf("未强化的装备 enhance_bonus_permyriad=%d,应为 0", item.GetEnhanceBonusPermyriad())
	}
	next := item.GetEnhanceNext()
	if level >= limit {
		if next != nil {
			return fmt.Errorf("已满级(+%d)却带着 enhance_next(next_level=%d)", level, next.GetNextLevel())
		}
	} else {
		if next == nil {
			return fmt.Errorf("+%d 未满级却不带 enhance_next(实例未初始化,或 EquipEnhance 第 %d 行缺失 / success_rate = 0)", level, level+1)
		}
		if next.GetNextLevel() != level+1 {
			return fmt.Errorf("enhance_next.next_level=%d,应为 enhance_level + 1 = %d", next.GetNextLevel(), level+1)
		}
		if next.GetSuccessRate() < 1 || next.GetSuccessRate() > equipForgePermyriadBase {
			return fmt.Errorf("enhance_next.success_rate=%d 不在 1..%d 之内", next.GetSuccessRate(), equipForgePermyriadBase)
		}
		if next.GetBonusPermyriad() < item.GetEnhanceBonusPermyriad() {
			return fmt.Errorf("enhance_next.bonus_permyriad=%d 比当前的 %d 还小(总加成应随等级单调不减)",
				next.GetBonusPermyriad(), item.GetEnhanceBonusPermyriad())
		}
	}
	if err := checkEquipForgeBaseLines(item, []equipBaseAttr{{equipAttrDamage, equipForgeWeaponBaseDamage}}); err != nil {
		return fmt.Errorf("base_attrs %s: %w", equipLinesText(item.GetBaseAttrs()), err)
	}
	if err := checkEquipForgeSockets(item.GetGemSockets(), equipForgeWeaponSockets); err != nil {
		return fmt.Errorf("gem_sockets %s: %w", equipForgeSocketsText(item), err)
	}
	return nil
}

// checkEquipForgeLedger 核对一次强化 / 镶嵌 / 取下前后「目标自己的字段之外」的一切:
//
//	目标没丢、没被复制、没换包、没挪格(这三个操作都是原地改实例);
//	金币恰好少了 goldSpent,且两份快照带的金币数一致(只比金币这一个数:其它币种、欠款、封禁列表不在断言范围);
//	人物背包按 config 汇总的数量 == wantCounts(调用方在操作前的汇总上加减好本次应有的变化);
//	装备栏里目标之外的实例、全部槽位与槽位定义逐字段不变。
func checkEquipForgeLedger(before, after equipForgeBags, itemId uint64, wantCounts map[uint32]uint64, goldSpent uint64) error {
	if equipBagItem(after.inventory, itemId) != nil && equipBagItem(after.equipment, itemId) != nil {
		return fmt.Errorf("item_id=%d 同时出现在人物背包与装备栏(实例被复制)", itemId)
	}
	was, wornBefore := before.item(itemId)
	now, wornNow := after.item(itemId)
	if was == nil || now == nil {
		return fmt.Errorf("item_id=%d 操作前后必须都在(操作前找到=%t,操作后找到=%t)", itemId, was != nil, now != nil)
	}
	if wornBefore != wornNow {
		return fmt.Errorf("item_id=%d 换了包(操作前穿着=%t,操作后穿着=%t)", itemId, wornBefore, wornNow)
	}
	homeBefore, homeAfter := before.inventory, after.inventory
	if wornBefore {
		homeBefore, homeAfter = before.equipment, after.equipment
	}
	if slotBefore, slotAfter := equipForgeItemSlot(homeBefore, itemId), equipForgeItemSlot(homeAfter, itemId); slotBefore != slotAfter {
		return fmt.Errorf("item_id=%d 从 %d 号格挪到了 %d 号格(-1 = 没摆放)", itemId, slotBefore, slotAfter)
	}
	goldBefore, goldAfter := equipForgeGold(before.inventory), equipForgeGold(after.inventory)
	if goldBefore < goldSpent || goldBefore-goldSpent != goldAfter {
		return fmt.Errorf("金币应恰好少 %d:操作前 %d,操作后 %d", goldSpent, goldBefore, goldAfter)
	}
	if other := equipForgeGold(after.equipment); other != goldAfter {
		return fmt.Errorf("同一次响应里两份快照的金币不一致:人物背包 %d,装备栏 %d", goldAfter, other)
	}
	if diff := equipForgeCountsDiff(wantCounts, equipForgeConfigCounts(after.inventory)); len(diff) != 0 {
		return fmt.Errorf("人物背包按 config 汇总的数量不对: %s", strings.Join(diff, "; "))
	}
	if !featureBagsEqual(equipForgeWithoutItem(before.equipment, itemId), equipForgeWithoutItem(after.equipment, itemId)) {
		return fmt.Errorf("装备栏里目标之外的内容变了(其它装备 / 槽位 / 槽位定义): 操作前 %d 件,操作后 %d 件",
			len(before.equipment.GetItems()), len(after.equipment.GetItems()))
	}
	return nil
}

// checkEquipForgeEnhance 给一次**被受理**的强化对账(设计文档 §7 第 3 步)。一律以 before(强化前那份快照)里目标的
// enhance_next 为准 —— 强化面板显示给玩家的就是它,服务器实扣的若与它不符,玩家看到的价格 / 结果就是错的:
//
//	金币差 = cost_gold;材料差 = material_count;成功 → 等级 +1,失败 → max(0, 等级 − fail_level_loss);
//	success_rate = 10000 必须成功,其余两种结果都接受(概率失败是成功的响应,不是错误);
//	响应里的 enhance_level 与快照里目标的 enhance_level 都等于上面算出的等级;
//	等级没变 → 目标的显示数据逐字段不变;等级变了 → 只许强化相关字段变,表值不变、增量按新万分比对得上,
//	成功时新的总加成与每行的值恰好等于强化前预览的 bonus_permyriad / enhance_next_value;
//	其余物品、目标所在的包与格子都不变(checkEquipForgeLedger;金币以外的货币它不看)。
func checkEquipForgeEnhance(before, after equipForgeBags, itemId uint64, success bool, level uint32) error {
	was, _ := before.item(itemId)
	if was == nil {
		return fmt.Errorf("强化前的快照里没有 item_id=%d", itemId)
	}
	next := was.GetEnhanceNext()
	if next == nil {
		return fmt.Errorf("强化前的快照里 item_id=%d 不带 enhance_next(满级 / 未开放),这次强化不该被受理", itemId)
	}
	current := was.GetEnhanceLevel()
	if next.GetNextLevel() != current+1 {
		return fmt.Errorf("强化前 enhance_next.next_level=%d,应为 enhance_level + 1 = %d", next.GetNextLevel(), current+1)
	}
	if !success && next.GetSuccessRate() >= equipForgePermyriadBase {
		return fmt.Errorf("enhance_next.success_rate=%d(必成)却回了 success=false", next.GetSuccessRate())
	}
	want := current + 1
	if !success {
		want = equipForgeLevelAfterFailure(current, next.GetFailLevelLoss())
	}
	if level != want {
		return fmt.Errorf("响应的 enhance_level=%d,应为 %d(强化前 +%d,success=%t,fail_level_loss=%d)",
			level, want, current, success, next.GetFailLevelLoss())
	}
	wantCounts := equipForgeConfigCounts(before.inventory)
	if materialId, count := next.GetMaterialConfigId(), uint64(next.GetMaterialCount()); materialId != 0 && count > 0 {
		if wantCounts[materialId] < count {
			return fmt.Errorf("强化前人物背包里 config_id=%d 只有 %d 个,不够 enhance_next 要的 %d 个,这次强化不该被受理",
				materialId, wantCounts[materialId], count)
		}
		wantCounts[materialId] -= count
	}
	if err := checkEquipForgeLedger(before, after, itemId, wantCounts, next.GetCostGold()); err != nil {
		return err
	}
	now, _ := after.item(itemId)
	if now.GetEnhanceLevel() != want {
		return fmt.Errorf("快照里 item_id=%d 的 enhance_level=%d,应为 %d(与响应的 enhance_level 不一致)",
			itemId, now.GetEnhanceLevel(), want)
	}
	if want == current {
		if !proto.Equal(was, now) {
			return fmt.Errorf("强化失败且等级没变(+%d),item_id=%d 的显示数据却变了: base_attrs 之前 %s,现在 %s",
				current, itemId, equipLinesText(was.GetBaseAttrs()), equipLinesText(now.GetBaseAttrs()))
		}
		return nil
	}
	if !proto.Equal(equipForgeEnhanceBlind(was), equipForgeEnhanceBlind(now)) {
		return fmt.Errorf("强化改了等级之外的字段(随机属性 / 孔位 / 名称 / 上限 …): affixes 之前 %s,现在 %s;孔位 之前 %s,现在 %s",
			equipLinesText(was.GetAffixes()), equipLinesText(now.GetAffixes()), equipForgeSocketsText(was), equipForgeSocketsText(now))
	}
	if len(now.GetBaseAttrs()) != len(was.GetBaseAttrs()) {
		return fmt.Errorf("基础属性行数变了: %d -> %d", len(was.GetBaseAttrs()), len(now.GetBaseAttrs()))
	}
	for index, line := range now.GetBaseAttrs() {
		previous := was.GetBaseAttrs()[index]
		if line.GetAttrId() != previous.GetAttrId() ||
			line.GetValue() < line.GetEnhanceBonus() || previous.GetValue() < previous.GetEnhanceBonus() ||
			line.GetValue()-line.GetEnhanceBonus() != previous.GetValue()-previous.GetEnhanceBonus() {
			return fmt.Errorf("基础属性第 %d 行的表值(value − enhance_bonus)变了: 之前 attr_id=%d %d − %d,现在 attr_id=%d %d − %d",
				index, previous.GetAttrId(), previous.GetValue(), previous.GetEnhanceBonus(),
				line.GetAttrId(), line.GetValue(), line.GetEnhanceBonus())
		}
		base := line.GetValue() - line.GetEnhanceBonus()
		if bonus := equipForgeScale(base, now.GetEnhanceBonusPermyriad()) - base; line.GetEnhanceBonus() != bonus {
			return fmt.Errorf("基础属性第 %d 行 enhance_bonus=%d,应为 表值 %d × %d / 10000(向下取整)= %d",
				index, line.GetEnhanceBonus(), base, now.GetEnhanceBonusPermyriad(), bonus)
		}
		if success && line.GetValue() != previous.GetEnhanceNextValue() {
			return fmt.Errorf("基础属性第 %d 行强化成功后 value=%d,而强化前预览的 enhance_next_value=%d",
				index, line.GetValue(), previous.GetEnhanceNextValue())
		}
	}
	if success && now.GetEnhanceBonusPermyriad() != next.GetBonusPermyriad() {
		return fmt.Errorf("强化成功后 enhance_bonus_permyriad=%d,而强化前预览的 bonus_permyriad=%d",
			now.GetEnhanceBonusPermyriad(), next.GetBonusPermyriad())
	}
	return nil
}

// checkEquipForgeInlay 给一次**被受理**的镶嵌对账(设计文档 §7 第 4 步):
//
//	镶嵌前那个孔存在且是空的、人物背包里确有这种宝石(否则不该被受理);
//	镶嵌后该孔 gem_config_id = 宝石 config、带宝石名、带 attr(= 生效),attr 与背包里那颗宝石自报的 gem_attr 同属性同值;
//	孔位定义(接纳类型、名称)不变,目标的其余字段逐字段不变;
//	人物背包里这种宝石恰好少 1,金币与其余物品不变(checkEquipForgeLedger)。
func checkEquipForgeInlay(before, after equipForgeBags, itemId uint64, socket, gemConfigId uint32) error {
	was, _ := before.item(itemId)
	if was == nil {
		return fmt.Errorf("镶嵌前的快照里没有 item_id=%d", itemId)
	}
	hole := equipForgeSocketOf(was, socket)
	if hole == nil {
		return fmt.Errorf("镶嵌前 item_id=%d 没有 %d 号孔,这次镶嵌不该被受理", itemId, socket)
	}
	if hole.GetGemConfigId() != 0 {
		return fmt.Errorf("镶嵌前 %d 号孔里已有 config_id=%d,这次镶嵌不该被受理", socket, hole.GetGemConfigId())
	}
	wantCounts := equipForgeConfigCounts(before.inventory)
	if wantCounts[gemConfigId] == 0 {
		return fmt.Errorf("镶嵌前人物背包里没有 config_id=%d,这次镶嵌不该被受理", gemConfigId)
	}
	wantCounts[gemConfigId]--
	if err := checkEquipForgeLedger(before, after, itemId, wantCounts, 0); err != nil {
		return err
	}
	now, _ := after.item(itemId)
	filled := equipForgeSocketOf(now, socket)
	if filled == nil || filled.GetGemConfigId() != gemConfigId {
		return fmt.Errorf("镶嵌后 %d 号孔应是 gem_config_id=%d,实际孔位 %s", socket, gemConfigId, equipForgeSocketsText(now))
	}
	if filled.GetGemType() != hole.GetGemType() || filled.GetName() != hole.GetName() {
		return fmt.Errorf("镶嵌改了 %d 号孔的定义: gem_type %d -> %d,name %q -> %q",
			socket, hole.GetGemType(), filled.GetGemType(), hole.GetName(), filled.GetName())
	}
	if filled.GetGemName() == "" {
		return fmt.Errorf("镶嵌后 %d 号孔的 gem_name 为空(宝石的 Item 行查不到?)", socket)
	}
	attr := filled.GetAttr()
	if attr == nil {
		return fmt.Errorf("镶嵌后 %d 号孔不带 attr:宝石嵌上了但不计入加成(EquipGem 行 / 属性行不可用?)", socket)
	}
	if err := checkEquipForgeGemAttr(attr); err != nil {
		return fmt.Errorf("镶嵌后 %d 号孔的 attr: %w", socket, err)
	}
	// wantCounts 判过数量非 0,所以这种宝石在镶嵌前的背包里至少有一堆
	source := equipBagItemsOfConfig(before.inventory, gemConfigId)[0].GetGemAttr()
	if attr.GetAttrId() != source.GetAttrId() || attr.GetValue() != source.GetValue() {
		return fmt.Errorf("镶嵌后 %d 号孔的 attr 是 attr_id=%d value=%d,而背包里这颗宝石自报的 gem_attr 是 attr_id=%d value=%d",
			socket, attr.GetAttrId(), attr.GetValue(), source.GetAttrId(), source.GetValue())
	}
	if !proto.Equal(equipForgeSocketBlind(was, socket), equipForgeSocketBlind(now, socket)) {
		return fmt.Errorf("镶嵌改了 %d 号孔之外的字段(强化等级 / 基础属性 / 随机属性 / 别的孔): 孔位 之前 %s,现在 %s;base_attrs 之前 %s,现在 %s",
			socket, equipForgeSocketsText(was), equipForgeSocketsText(now),
			equipLinesText(was.GetBaseAttrs()), equipLinesText(now.GetBaseAttrs()))
	}
	return nil
}

// checkEquipForgeRemove 给一次**被受理**的取下对账(设计文档 §7 第 6 步):
//
//	取下前那个孔里确有宝石(否则不该被受理);取下后该孔仍在(作为空孔继续下发)、不带宝石 config / 名称 / 属性;
//	孔位定义不变,目标的其余字段逐字段不变;
//	人物背包里那种宝石恰好多 1(原样退回),金币与其余物品不变(取下免费)。
func checkEquipForgeRemove(before, after equipForgeBags, itemId uint64, socket uint32) error {
	was, _ := before.item(itemId)
	if was == nil {
		return fmt.Errorf("取下前的快照里没有 item_id=%d", itemId)
	}
	hole := equipForgeSocketOf(was, socket)
	if hole == nil || hole.GetGemConfigId() == 0 {
		return fmt.Errorf("取下前 item_id=%d 的 %d 号孔里没有宝石(孔位 %s),这次取下不该被受理", itemId, socket, equipForgeSocketsText(was))
	}
	wantCounts := equipForgeConfigCounts(before.inventory)
	wantCounts[hole.GetGemConfigId()]++
	if err := checkEquipForgeLedger(before, after, itemId, wantCounts, 0); err != nil {
		return err
	}
	now, _ := after.item(itemId)
	emptied := equipForgeSocketOf(now, socket)
	if emptied == nil {
		return fmt.Errorf("取下后 %d 号孔不见了(它仍在孔位定义里,应作为空孔继续下发): 孔位 %s", socket, equipForgeSocketsText(now))
	}
	if emptied.GetGemConfigId() != 0 || emptied.GetAttr() != nil || emptied.GetGemName() != "" {
		return fmt.Errorf("取下后 %d 号孔应是空孔,实际 gem_config_id=%d gem_name=%q 带属性=%t",
			socket, emptied.GetGemConfigId(), emptied.GetGemName(), emptied.GetAttr() != nil)
	}
	if emptied.GetGemType() != hole.GetGemType() || emptied.GetName() != hole.GetName() {
		return fmt.Errorf("取下改了 %d 号孔的定义: gem_type %d -> %d,name %q -> %q",
			socket, hole.GetGemType(), emptied.GetGemType(), hole.GetName(), emptied.GetName())
	}
	if !proto.Equal(equipForgeSocketBlind(was, socket), equipForgeSocketBlind(now, socket)) {
		return fmt.Errorf("取下改了 %d 号孔之外的字段(强化等级 / 基础属性 / 随机属性 / 别的孔): 孔位 之前 %s,现在 %s;base_attrs 之前 %s,现在 %s",
			socket, equipForgeSocketsText(was), equipForgeSocketsText(now),
			equipLinesText(was.GetBaseAttrs()), equipLinesText(now.GetBaseAttrs()))
	}
	return nil
}

// checkEquipForgeRestored 核对下线重登前后(设计文档 §7 第 5 步):目标仍穿在身上,显示数据逐字段相同
// (强化等级与宝石随实例落库往返,任何一个丢了都会体现在这里);金币、人物背包按 config 汇总的数量、装备栏的其余部分都不变。
// 人物背包不逐实例比:读盘时同种材料的堆是否保持原样不属于本冒烟的断言范围。
func checkEquipForgeRestored(before, after equipForgeBags, itemId uint64) error {
	was, worn := before.item(itemId)
	if was == nil || !worn {
		return fmt.Errorf("下线前 item_id=%d 应穿在身上(找到=%t,穿着=%t)", itemId, was != nil, worn)
	}
	now := equipBagItem(after.equipment, itemId)
	if now == nil {
		return fmt.Errorf("重登后 item_id=%d 不在装备栏里(人物背包里有=%t)", itemId, equipBagItem(after.inventory, itemId) != nil)
	}
	if equipBagItem(after.inventory, itemId) != nil {
		return fmt.Errorf("重登后 item_id=%d 同时出现在人物背包与装备栏(实例被复制)", itemId)
	}
	if !proto.Equal(was, now) {
		return fmt.Errorf("重登后 item_id=%d 的显示数据变了: 强化 +%d -> +%d;孔位 %s -> %s;base_attrs %s -> %s",
			itemId, was.GetEnhanceLevel(), now.GetEnhanceLevel(), equipForgeSocketsText(was), equipForgeSocketsText(now),
			equipLinesText(was.GetBaseAttrs()), equipLinesText(now.GetBaseAttrs()))
	}
	if goldBefore, goldAfter := equipForgeGold(before.inventory), equipForgeGold(after.inventory); goldBefore != goldAfter {
		return fmt.Errorf("重登后金币变了: %d -> %d", goldBefore, goldAfter)
	}
	if diff := equipForgeCountsDiff(equipForgeConfigCounts(before.inventory), equipForgeConfigCounts(after.inventory)); len(diff) != 0 {
		return fmt.Errorf("重登后人物背包按 config 汇总的数量变了: %s", strings.Join(diff, "; "))
	}
	// 目标自己上面逐字段比过了,这里比装备栏的其余部分:别的装备、目标所在的槽、槽位定义。
	// currency 不整份比(只比上面的金币数值):读盘后 values 的长度是否与下线前逐位相同不属于本冒烟的断言范围。
	if !featureBagsEqual(equipForgeWithoutItem(before.equipment, itemId), equipForgeWithoutItem(after.equipment, itemId)) {
		return fmt.Errorf("重登后装备栏与下线前不一致(别的装备 / 目标所在的槽 / 槽位定义): 下线前 %d 件 / %d 个槽位定义,现在 %d 件 / %d 个槽位定义",
			len(before.equipment.GetItems()), len(before.equipment.GetLayout().GetEquipSlots()),
			len(after.equipment.GetItems()), len(after.equipment.GetLayout().GetEquipSlots()))
	}
	return nil
}

// checkEquipForgePanelShift 要求 after 相对 before 只有物伤与法伤各变了 delta,其余(六项二级属性上限、
// 一级维度、战斗属性)逐项相等。delta 可以为负(强化失败掉级)。
//
// 用「恰好」而不是 equip-smoke 的「至少」:这里前后两份面板之间只差同一件穿着的装备上「伤害」这一种平加值
// (武器基础属性的强化增量,或一颗伤害宝石),它在取整之前以整数加进物伤 / 法伤,别的输入都没动。
func checkEquipForgePanelShift(before, after *scene.AttributePanelInfo, delta int64) error {
	if before.GetDerived() == nil || after.GetDerived() == nil {
		return fmt.Errorf("面板缺二级属性(before 有=%t,after 有=%t)", before.GetDerived() != nil, after.GetDerived() != nil)
	}
	want := proto.Clone(before).(*scene.AttributePanelInfo)
	for _, attack := range []*uint64{&want.Derived.PhysicalAttack, &want.Derived.MagicAttack} {
		if delta < 0 && *attack < uint64(-delta) {
			return fmt.Errorf("变化量 %d 比变化前的攻击 %d 还大", delta, *attack)
		}
		*attack = uint64(int64(*attack) + delta)
	}
	if diff := equipPanelDiff(want, after); len(diff) != 0 {
		return fmt.Errorf("面板应只有物伤 / 法伤各变 %+d、其余不变(预期 -> 实际): %s", delta, strings.Join(diff, "; "))
	}
	return nil
}

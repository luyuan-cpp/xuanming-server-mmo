package main

// equip-smoke 场景:单机器人对本地服务端做「装备属性(问道式随机属性 + 穿脱)」端到端冒烟。
//
// 兑现 docs/design/equipment-attributes.md 的服务端契约(§2.6 样例数据 / §3.2 协议 / §4.3 穿脱编排 /
// §4.4 属性计算 / §5 不变量),逐条断言:
//   0. 预备(让脚本可重复运行):等级归 1;装备栏里穿着的全部卸下;人物背包里凑齐 2 把 1101(1 级武器)
//      + 1 把 1105(80 级武器)+ 1 件 1401(1 级鞋)—— 已有就复用,缺几件 GmGrantItem 补几件,所以同账号
//      反复跑背包不会涨;空位不够直接判红并提示清号;
//   1. tooltip 字段:1101 名称非空、equip_level = 1、基础属性恰 1 行(伤害 40,tier 0,cap 0);
//      随机属性 1..5 行(蓝 1..3、粉 / 黄各至多 1),每行 tier ∈ {1 蓝, 2 粉, 3 黄}、1 <= value <= cap、蓝属性互不重复、
//      同档 seq 从 0 连续、整体按 (tier, seq) 有序;1401 的基础属性恰 2 行(防御 12、速度 12);
//   2. 装备栏快照带全部槽位定义:3 / 4 / 5 / 6 号槽都在、名称非空、slot 升序;
//   3. 等级不足:1 级角色穿 1105 → kEquipLevelNotEnough,两个包原样不变;
//   4. 穿武器 A(第一把 1101):响应里人物背包不再有 A、装备栏 3 号槽是 A 且显示数据与穿前逐字段相同(不重掷);
//      服务器主动推了属性面板,且与随后主动取的一致;物伤 / 法伤至少 +40(基础伤害)再加 A 上的「伤害」随机属性,
//      六项二级属性、一级属性点、战斗属性都不低于穿前;A 上与面板「战斗属性」同名的随机属性至少加到那一行;
//   5. 替换:再穿武器 B(第二把 1101)→ 3 号槽变成 B,A 回到人物背包且属性不变;面板按「只穿 B」核对;
//   6. 再穿鞋 1401 → 6 号槽;面板速度、防御各至少 +12;
//   7. 下线重登:装备栏里 B 与鞋仍在原槽、显示数据逐字段相同;面板六项 / 一级属性点 / 战斗属性与下线前一致
//      (装备实例落库往返 + 加载时重算装备加成 —— 战斗属性不落库,加载漏重算就会在这里归零);
//   8. 卸 B → 面板按「只穿鞋」核对;卸鞋 → 装备栏空,面板**恰好**回到第 4 步之前(穿脱可逆);
//      再卸一次 B → kEquipNotEquipped;EquipItem(0)→ kInvalidParameter(handler 粗检)或 kEquipItemNotFound;
//      EquipItem(不存在的 guid)→ kEquipItemNotFound;三次被拒之后两个包不变;
//   9. 打 EQUIP_SMOKE_OK。
//
// 结果约定(供外层脚本消费):
//   全过 → 日志一行 `EQUIP_SMOKE_OK …`,退出码 0;
//   任一步失败 → `EQUIP_SMOKE_FAIL step=… reason=…`,退出码 1。

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"google.golang.org/protobuf/proto"

	"proto/scene"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

// 账号前缀必须是 robot_ 才会命中 login 侧的 DevPasswordAuth(开发用密码认证)。
// 9103 避开压测区段、battle-smoke 的 9001/9002、attribute-smoke 的 9101 与 pet-smoke 的 9102。
const equipSmokeAccount = "robot_9103"

// 单次 RPC 的等待预算。背包 / 穿脱是纯内存 + 表查询,正常 <50ms;给 10s 是覆盖 gate→scene 首次路由建立。
const equipSmokeRpcTimeout = 10 * time.Second

// 同一个消息号两次发送之间至少隔这么久。
//
// gate 的 MessageLimiter 按「消息号 + 整秒时间戳」计数,一个时间戳要过了 time_window(1 秒)**之后**才出窗,
// 所以额度 N 实际管的是「相邻两个整秒里合计 N 个」。700ms 的间隔保证任意相邻两秒里同号至多 3 个 ——
// 表里没登记的消息吃的默认档(3 次/窗口)也过得去,穿 / 脱 / 发物的 5 次/秒、GetBag 的 10 次/秒更不用说。
// 这样「MessageLimiter 还没加行」不会把一条验装备的冒烟染红;那件事对真实客户端(连点)仍然是上线前置,
// 写在 etc/equip_smoke.yaml 的文件头里。代价:同号连发(预备段连脱、成对读两个包)每次多等不到 1 秒。
const equipSmokeSameIdGap = 700 * time.Millisecond

// 包类型,与 cpp/libs/modules/bag/bag_system.h 的 BagType、player_bag.proto 的注释对齐。
const (
	equipSmokeBagInventory uint32 = 0 // 人物背包
	equipSmokeBagEquipment uint32 = 2 // 装备栏
)

// 样例装备与槽位,与 data/Item.xlsx / data/EquipSlot.xlsx 对齐(设计文档 §2.6)。
const (
	equipSmokeWeaponL1  uint32 = 1101 // 1 级武器:基础属性 伤害 40
	equipSmokeWeaponL80 uint32 = 1105 // 80 级武器:基础属性 伤害 1620;1 级角色穿不上
	equipSmokeShoesL1   uint32 = 1401 // 1 级鞋:基础属性 防御 12、速度 12

	equipSmokeSlotWeapon  uint32 = 3
	equipSmokeSlotHat     uint32 = 4
	equipSmokeSlotClothes uint32 = 5
	equipSmokeSlotShoes   uint32 = 6
)

// 装备栏快照必须带出来的槽位定义(含空槽)。
var equipSmokeSlots = []uint32{equipSmokeSlotWeapon, equipSmokeSlotHat, equipSmokeSlotClothes, equipSmokeSlotShoes}

// 冒烟用到的装备与各自需要的件数。预备段按「缺几件补几件」发,已有的直接复用。
var equipSmokeKit = []struct {
	configId uint32
	count    int
}{
	{equipSmokeWeaponL1, 2},  // 两把:一把穿上、一把用来替换
	{equipSmokeWeaponL80, 1}, // 等级不足的反例
	{equipSmokeShoesL1, 1},   // 第二个部位
}

// 属性 id,与 data/EquipAttribute.xlsx 对齐(设计文档 §2.1,id 一经使用不再改义)。
// 只镜像样例装备的**基础属性**用到的三行 —— 断言里要写「伤害 40 / 防御 12 / 速度 12」,绕不开。
// 其余随机属性一律不按 id 解释(机器人和客户端一样零配表),见 checkEquipGain。
const (
	equipAttrDamage  uint32 = 1  // 伤害:物伤与法伤各 +N
	equipAttrDefense uint32 = 8  // 防御 +N
	equipAttrSpeed   uint32 = 11 // 速度 +N
)

// 颜色档(EquipAttrLineInfo.tier,设计文档 §3.2):0 基础 / 1 蓝 / 2 粉 / 3 黄 / 4 绿(预留,本期不掷)。
const (
	equipTierBase   uint32 = 0
	equipTierBlue   uint32 = 1
	equipTierPink   uint32 = 2
	equipTierYellow uint32 = 3
)

// 各档随机属性的条数范围,来自 EquipAffixRule 行 1(样例装备的 affix_rule 都是它,设计文档 §2.4 / §2.6):
// 蓝 blue_min..blue_max = 1..3 条;粉、黄各按概率「掷出 1 条」,所以至多 1 条(这一条是掷值规则本身的形状,
// 与概率填多少无关)。只限总条数不够:5 条全蓝、或两条粉都在 1..5 之内。
const (
	equipSmokeMinBlueAffixes   = 1
	equipSmokeMaxBlueAffixes   = 3
	equipSmokeMaxPinkAffixes   = 1
	equipSmokeMaxYellowAffixes = 1

	equipSmokeMinAffixes = equipSmokeMinBlueAffixes
	equipSmokeMaxAffixes = equipSmokeMaxBlueAffixes + equipSmokeMaxPinkAffixes + equipSmokeMaxYellowAffixes
)

// 面板「战斗属性」的行数 = CombatAttributes 的字段数(设计文档 §3.3;§3.2 约定 15 项全量下发)。
const equipSmokeCombatRows = 15

// tip id 一律读导表器生成的枚举,**不写字面量**(AGENTS.md §7 不变量 5;教训见 pet_smoke_scenario.go 同名注释)。
var (
	tipEquipItemNotFound   = uint32(tiptable.EquipError_kEquipItemNotFound)
	tipEquipLevelNotEnough = uint32(tiptable.EquipError_kEquipLevelNotEnough)
	tipEquipNotEquipped    = uint32(tiptable.EquipError_kEquipNotEquipped)
	tipEquipBagFull        = uint32(tiptable.EquipError_kEquipBagFull)
	tipEquipGrantInvalid   = uint32(tiptable.EquipError_kEquipGrantInvalid)
	// 通用段的「参数无效」:scene handler 守护段对 item_id = 0 的粗检回的是它(见第 8 步)
	tipInvalidParameter = uint32(tiptable.CommonError_kInvalidParameter)
)

// equipSmokeSession 是一个已登录进场的机器人会话。
type equipSmokeSession struct {
	gc       *pkg.GameClient
	player   *gameobject.Player
	stats    *metrics.Stats
	lastSent map[uint32]time.Time // 消息号 → 上次发送时刻(同号节流用,见 equipSmokeSameIdGap)
}

// equipChange 是一次穿 / 脱请求的完整结果。
type equipChange struct {
	bag       *scene.BagInfo // 人物背包全量(仅 tip == 0 时有)
	equipment *scene.BagInfo // 装备栏全量(仅 tip == 0 时有)
	tip       uint32         // 0 = 成功;否则是响应体 error_message 里的业务拒绝码
	panelSeq  uint64         // 发请求前的属性面板序号:成功后服务器推的那份面板,序号必然比它大
}

// equipBaseAttr 是表里的一条基础属性(Item.base_attr)。
type equipBaseAttr struct {
	attrId uint32
	value  uint64
}

// RunEquipSmoke 是 main.go `mode: equip-smoke` 的入口。
func RunEquipSmoke(cfg *config.Config) {
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
		zap.L().Error(fmt.Sprintf("EQUIP_SMOKE_FAIL step=%s reason=%s", step, fmt.Sprintf(format, args...)))
		cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	}

	// ---- 登录进场 ----
	opened, err := openEquipSmokeSession(cfg, stats)
	if err != nil {
		fail("login", "%v", err)
	}
	session = opened
	zap.L().Info("[equip-smoke] in scene", zap.Uint64("player", session.gc.PlayerId))

	// ---- 步骤 0:预备,把账号拉回已知起点(同账号可反复跑) ----
	// 等级归 1:第 3 步要用「1 级穿不上 80 级武器」;GmSetPlayerLevel 先推面板再回包,attributePanel 按来源认领
	if _, err := session.attributePanel(game.SceneAttributeClientPlayerGmSetPlayerLevelMessageId,
		&scene.GmSetPlayerLevelRequest{Level: 1}); err != nil {
		fail("prep-level", "%v", err)
	}
	// 上一轮若在穿上之后中途失败,账号会带着装备进来:无条件全卸,否则第 4 步的面板基线就不是裸装,
	// 「卸下后恰好还原」那条断言也无从谈起。
	equipment, err := session.getBag(equipSmokeBagEquipment)
	if err != nil {
		fail("prep-equipment", "%v", err)
	}
	for _, worn := range equipment.GetItems() {
		change, err := session.unequip(worn.GetItemId())
		if err != nil {
			fail("prep-unequip", "item_id=%d %v", worn.GetItemId(), err)
		}
		switch change.tip {
		case 0, tipEquipNotEquipped:
			// 卸下了,或者它已经不在装备栏里 —— 两种都是目标状态
		case tipEquipBagFull:
			fail("prep-bag-full", "人物背包已满,卸不下上一轮留在身上的装备 item_id=%d。请清号后重跑:"+
				"清掉 %s 的背包存档,或把 equipSmokeAccount 换成一个新账号", worn.GetItemId(), equipSmokeAccount)
		default:
			fail("prep-unequip", "卸下 item_id=%d 被拒: tip=%s", worn.GetItemId(), equipTipText(change.tip))
		}
	}
	equipment, err = session.getBag(equipSmokeBagEquipment)
	if err != nil {
		fail("prep-equipment", "%v", err)
	}
	if len(equipment.GetItems()) != 0 {
		fail("prep-equipment-not-empty", "全部卸下之后装备栏仍有 %d 件", len(equipment.GetItems()))
	}
	inventory, err := session.getBag(equipSmokeBagInventory)
	if err != nil {
		fail("prep-inventory", "%v", err)
	}
	// 记下发物之前就在包里的实例:第 1 步失败时要说清红的那件是复用的旧实例还是本轮新铸的(见 equipDisplayHint)
	reused := make(map[uint64]bool, len(inventory.GetItems()))
	for _, item := range inventory.GetItems() {
		reused[item.GetItemId()] = true
	}
	// 先算清要补几件、放不放得下,再动手发:发到一半才发现满了,会留下一个凑不齐的半成品账号
	missing := 0
	for _, kit := range equipSmokeKit {
		if have := len(equipBagItemsOfConfig(inventory, kit.configId)); have < kit.count {
			missing += kit.count - have
		}
	}
	if free := equipBagFreeSlots(inventory); free < missing {
		fail("prep-bag-space", "人物背包空位不足:还要发 %d 件装备,只剩 %d 格(容量 %d)。请清号后重跑:"+
			"清掉 %s 的背包存档,或把 equipSmokeAccount 换成一个新账号",
			missing, free, inventory.GetLayout().GetCapacity(), equipSmokeAccount)
	}
	granted := 0
	for _, kit := range equipSmokeKit {
		have := len(equipBagItemsOfConfig(inventory, kit.configId))
		if have >= kit.count {
			continue
		}
		bag, tip, err := session.gmGrant(kit.configId, uint32(kit.count-have))
		if err != nil {
			fail("prep-grant", "config_id=%d %v", kit.configId, err)
		}
		if tip == tipEquipGrantInvalid {
			fail("prep-grant-table", "GmGrantItem(config_id=%d) 回 kEquipGrantInvalid:Item 表里没有这一行,"+
				"需先导表(tools/scripts/equip_xlsx_patch.py + 导表器)并重启 scene", kit.configId)
		}
		if tip != 0 {
			fail("prep-grant", "GmGrantItem(config_id=%d, count=%d) 被拒: tip=%s",
				kit.configId, kit.count-have, equipTipText(tip))
		}
		if got := len(equipBagItemsOfConfig(bag, kit.configId)); got != kit.count {
			fail("prep-grant-count", "GmGrantItem(config_id=%d, count=%d) 之后人物背包里应有 %d 件,实际 %d 件",
				kit.configId, kit.count-have, kit.count, got)
		}
		inventory = bag
		granted += kit.count - have
	}
	if granted > 0 {
		// 响应里的包是客户端直接拿来覆盖本地缓存的,必须就是全量真相
		fresh, err := session.getBag(equipSmokeBagInventory)
		if err != nil {
			fail("prep-inventory", "%v", err)
		}
		if !featureBagsEqual(inventory, fresh) {
			fail("prep-grant-snapshot", "GmGrantItem 响应里的人物背包与随后 GetBag 读到的不一致(响应应是全量快照)")
		}
		inventory = fresh
	}
	weapons := equipBagItemsOfConfig(inventory, equipSmokeWeaponL1)
	bigWeapons := equipBagItemsOfConfig(inventory, equipSmokeWeaponL80)
	shoesList := equipBagItemsOfConfig(inventory, equipSmokeShoesL1)
	if len(weapons) < 2 || len(bigWeapons) < 1 || len(shoesList) < 1 {
		fail("prep-pick", "预备后人物背包里的装备不齐: %d 把 %d / %d 把 %d / %d 件 %d",
			len(weapons), equipSmokeWeaponL1, len(bigWeapons), equipSmokeWeaponL80, len(shoesList), equipSmokeShoesL1)
	}
	// 「第一把 / 第二把」按 item_id 升序定:BagInfo.items 的顺序没有语义
	weaponA, weaponB, bigWeapon, shoes := weapons[0], weapons[1], bigWeapons[0], shoesList[0]
	// 两个包的物品总数:穿脱只是同一 guid 换包,之后每一步都必须守恒
	totalItems := len(inventory.GetItems()) + len(equipment.GetItems())
	zap.L().Info("[equip-smoke] step 0: prepared",
		zap.Uint64("weapon_a", weaponA.GetItemId()), zap.Uint64("weapon_b", weaponB.GetItemId()),
		zap.Uint64("weapon_l80", bigWeapon.GetItemId()), zap.Uint64("shoes", shoes.GetItemId()),
		zap.Int("granted", granted), zap.Int("bag_items", len(inventory.GetItems())),
		zap.Uint32("bag_capacity", inventory.GetLayout().GetCapacity()))

	// ---- 步骤 1:tooltip 字段(名称 / 要求 / 基础属性 / 随机属性的形状) ----
	for _, weapon := range []*scene.BagItemInfo{weaponA, weaponB} {
		if err := checkEquipItemDisplay(weapon, 1, []equipBaseAttr{{equipAttrDamage, 40}}); err != nil {
			fail("display-weapon", "config_id=%d item_id=%d: %v%s",
				weapon.GetConfigId(), weapon.GetItemId(), err, equipDisplayHint(weapon, reused))
		}
	}
	if err := checkEquipItemDisplay(shoes, 1,
		[]equipBaseAttr{{equipAttrDefense, 12}, {equipAttrSpeed, 12}}); err != nil {
		fail("display-shoes", "config_id=%d item_id=%d: %v%s",
			shoes.GetConfigId(), shoes.GetItemId(), err, equipDisplayHint(shoes, reused))
	}
	// 80 级武器顺带核一遍:上限按 80 级那一档取,value 仍须落在 1..cap 内
	if err := checkEquipItemDisplay(bigWeapon, 80, []equipBaseAttr{{equipAttrDamage, 1620}}); err != nil {
		fail("display-weapon-l80", "config_id=%d item_id=%d: %v%s",
			bigWeapon.GetConfigId(), bigWeapon.GetItemId(), err, equipDisplayHint(bigWeapon, reused))
	}
	zap.L().Info("[equip-smoke] step 1: tooltip fields ok",
		zap.String("weapon_name", weaponA.GetName()),
		zap.String("affixes_a", equipLinesText(weaponA.GetAffixes())),
		zap.String("affixes_b", equipLinesText(weaponB.GetAffixes())),
		zap.String("affixes_shoes", equipLinesText(shoes.GetAffixes())))

	// ---- 步骤 2:装备栏快照带全部槽位定义 ----
	slotDefs, err := checkEquipSlotDefs(equipment.GetLayout(), equipSmokeSlots...)
	if err != nil {
		fail("equip-slots", "%v", err)
	}
	// 槽位的部位必须与样例装备的部位对得上,否则后面「武器进 3 号槽、鞋进 6 号槽」不成立
	if slotDefs[equipSmokeSlotWeapon].GetEquipKind() != weaponA.GetEquipKind() ||
		slotDefs[equipSmokeSlotShoes].GetEquipKind() != shoes.GetEquipKind() {
		fail("equip-slot-kinds", "槽位部位与装备部位对不上: %d 号槽 kind=%d / 武器 kind=%d,%d 号槽 kind=%d / 鞋 kind=%d",
			equipSmokeSlotWeapon, slotDefs[equipSmokeSlotWeapon].GetEquipKind(), weaponA.GetEquipKind(),
			equipSmokeSlotShoes, slotDefs[equipSmokeSlotShoes].GetEquipKind(), shoes.GetEquipKind())
	}
	zap.L().Info("[equip-smoke] step 2: equip slots ok",
		zap.Int("slot_defs", len(slotDefs)), zap.Uint32("equipment_capacity", equipment.GetLayout().GetCapacity()))

	// ---- 步骤 3:等级不足 ----
	change, err := session.equip(bigWeapon.GetItemId())
	if err != nil {
		fail("level-gate", "%v", err)
	}
	if change.tip != tipEquipLevelNotEnough {
		fail("level-gate", "1 级角色穿 80 级武器应回 kEquipLevelNotEnough(%d),实际 tip=%s",
			tipEquipLevelNotEnough, equipTipText(change.tip))
	}
	if err := session.expectBagsUnchanged(inventory, equipment); err != nil {
		fail("level-gate-unchanged", "%v", err)
	}
	zap.L().Info("[equip-smoke] step 3: level requirement enforced")

	// ---- 步骤 4:穿武器 A ----
	bare, err := session.attributePanel(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{})
	if err != nil {
		fail("panel-bare", "%v", err)
	}
	if err := checkEquipPanelShape(bare); err != nil {
		fail("panel-shape", "%v", err)
	}
	if bare.GetLevel() != 1 {
		fail("panel-level", "预备后应为 1 级,面板 level=%d", bare.GetLevel())
	}
	change, err = session.equip(weaponA.GetItemId())
	if err != nil {
		fail("equip-a", "%v", err)
	}
	if change.tip != 0 {
		fail("equip-a", "穿 1 级武器被拒: tip=%s", equipTipText(change.tip))
	}
	if err := checkEquipLayout(change.bag, change.equipment, totalItems,
		map[uint32]*scene.BagItemInfo{equipSmokeSlotWeapon: weaponA}, weaponB, bigWeapon, shoes); err != nil {
		fail("equip-a-layout", "%v", err)
	}
	withA, err := session.panelAfter(change)
	if err != nil {
		fail("equip-a-panel", "%v", err)
	}
	if err := checkEquipGain(bare, withA, weaponA); err != nil {
		fail("equip-a-gain", "%v", err)
	}
	zap.L().Info("[equip-smoke] step 4: weapon equipped",
		zap.Uint64("physical_attack_before", bare.GetDerived().GetPhysicalAttack()),
		zap.Uint64("physical_attack_after", withA.GetDerived().GetPhysicalAttack()),
		zap.Uint64("magic_attack_after", withA.GetDerived().GetMagicAttack()))

	// ---- 步骤 5:替换(同部位已占,旧的回背包) ----
	change, err = session.equip(weaponB.GetItemId())
	if err != nil {
		fail("equip-b", "%v", err)
	}
	if change.tip != 0 {
		fail("equip-b", "穿第二把武器(替换)被拒: tip=%s", equipTipText(change.tip))
	}
	if err := checkEquipLayout(change.bag, change.equipment, totalItems,
		map[uint32]*scene.BagItemInfo{equipSmokeSlotWeapon: weaponB}, weaponA, bigWeapon, shoes); err != nil {
		fail("equip-b-layout", "%v", err)
	}
	withB, err := session.panelAfter(change)
	if err != nil {
		fail("equip-b-panel", "%v", err)
	}
	// 基线仍是裸装:A 已经换下来了,身上只有 B
	if err := checkEquipGain(bare, withB, weaponB); err != nil {
		fail("equip-b-gain", "%v", err)
	}
	zap.L().Info("[equip-smoke] step 5: weapon replaced, old one back in bag",
		zap.Uint64("physical_attack", withB.GetDerived().GetPhysicalAttack()))

	// ---- 步骤 6:再穿鞋(第二个部位) ----
	change, err = session.equip(shoes.GetItemId())
	if err != nil {
		fail("equip-shoes", "%v", err)
	}
	if change.tip != 0 {
		fail("equip-shoes", "穿 1 级鞋被拒: tip=%s", equipTipText(change.tip))
	}
	dressed := map[uint32]*scene.BagItemInfo{equipSmokeSlotWeapon: weaponB, equipSmokeSlotShoes: shoes}
	if err := checkEquipLayout(change.bag, change.equipment, totalItems, dressed, weaponA, bigWeapon); err != nil {
		fail("equip-shoes-layout", "%v", err)
	}
	withBoth, err := session.panelAfter(change)
	if err != nil {
		fail("equip-shoes-panel", "%v", err)
	}
	// 基线是「只穿 B」:鞋的基础属性 防御 12 / 速度 12 必须原样加上去
	if err := checkEquipGain(withB, withBoth, shoes); err != nil {
		fail("equip-shoes-gain", "%v", err)
	}
	zap.L().Info("[equip-smoke] step 6: shoes equipped",
		zap.Uint64("defense_before", withB.GetDerived().GetDefense()),
		zap.Uint64("defense_after", withBoth.GetDerived().GetDefense()),
		zap.Uint64("speed_before", withB.GetDerived().GetSpeed()),
		zap.Uint64("speed_after", withBoth.GetDerived().GetSpeed()))

	// ---- 步骤 7:下线重登,装备与加成往返 ----
	// 覆盖三件事:ItemEntry.equip 的存盘 / 读盘(bag_marshal 漏字段就丢属性)、装备栏的槽位还原、
	// 加载时按装备重算(战斗属性与装备加成都不落库,InitializeOnLoad 漏了就退回裸装数值)。
	reborn, err := session.relogin(cfg)
	if err != nil {
		fail("relogin", "%v", err)
	}
	session = reborn
	equipment, err = session.getBag(equipSmokeBagEquipment)
	if err != nil {
		fail("relogin-equipment", "%v", err)
	}
	inventory, err = session.getBag(equipSmokeBagInventory)
	if err != nil {
		fail("relogin-inventory", "%v", err)
	}
	if err := checkEquipLayout(inventory, equipment, totalItems, dressed, weaponA, bigWeapon); err != nil {
		fail("relogin-layout", "%v", err)
	}
	restored, err := session.attributePanel(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{})
	if err != nil {
		fail("relogin-panel", "%v", err)
	}
	if diff := equipPanelDiff(withBoth, restored); len(diff) != 0 {
		fail("relogin-panel-mismatch", "重登后面板与下线前不一致(装备加成应在加载时重算出同样的值): %s",
			strings.Join(diff, "; "))
	}
	zap.L().Info("[equip-smoke] step 7: equipment and bonuses survived relogin",
		zap.Uint64("player", session.gc.PlayerId),
		zap.Uint64("physical_attack", restored.GetDerived().GetPhysicalAttack()),
		zap.Uint64("defense", restored.GetDerived().GetDefense()))

	// ---- 步骤 8:卸下,面板恰好还原;各类拒绝 ----
	change, err = session.unequip(weaponB.GetItemId())
	if err != nil {
		fail("unequip-b", "%v", err)
	}
	if change.tip != 0 {
		fail("unequip-b", "卸下武器被拒: tip=%s", equipTipText(change.tip))
	}
	if err := checkEquipLayout(change.bag, change.equipment, totalItems,
		map[uint32]*scene.BagItemInfo{equipSmokeSlotShoes: shoes}, weaponA, weaponB, bigWeapon); err != nil {
		fail("unequip-b-layout", "%v", err)
	}
	shoesOnly, err := session.panelAfter(change)
	if err != nil {
		fail("unequip-b-panel", "%v", err)
	}
	// 卸一件只撤一件的加成:身上还剩鞋,相对裸装仍须带着鞋的那份
	if err := checkEquipGain(bare, shoesOnly, shoes); err != nil {
		fail("unequip-b-gain", "%v", err)
	}
	change, err = session.unequip(shoes.GetItemId())
	if err != nil {
		fail("unequip-shoes", "%v", err)
	}
	if change.tip != 0 {
		fail("unequip-shoes", "卸下鞋被拒: tip=%s", equipTipText(change.tip))
	}
	if err := checkEquipLayout(change.bag, change.equipment, totalItems,
		map[uint32]*scene.BagItemInfo{}, weaponA, weaponB, bigWeapon, shoes); err != nil {
		fail("unequip-shoes-layout", "%v", err)
	}
	bareAgain, err := session.panelAfter(change)
	if err != nil {
		fail("unequip-shoes-panel", "%v", err)
	}
	if diff := equipPanelDiff(bare, bareAgain); len(diff) != 0 {
		fail("unequip-not-reversible", "全部卸下后面板应恰好回到穿之前(穿脱可逆): %s", strings.Join(diff, "; "))
	}
	finalInventory, finalEquipment := change.bag, change.equipment

	change, err = session.unequip(weaponB.GetItemId())
	if err != nil {
		fail("unequip-twice", "%v", err)
	}
	if change.tip != tipEquipNotEquipped {
		fail("unequip-twice", "卸一件不在装备栏里的装备应回 kEquipNotEquipped(%d),实际 tip=%s",
			tipEquipNotEquipped, equipTipText(change.tip))
	}
	change, err = session.equip(0)
	if err != nil {
		fail("equip-zero", "%v", err)
	}
	// 设计文档 §4.6 只说「参数粗检」,没规定 item_id = 0 回哪个码。现在的 handler 守护段回 kInvalidParameter;
	// 粗检若被拿掉,0 会落进 PlayerEquipSystem::Equip、按「人物背包里没有这个 guid」回 kEquipItemNotFound。
	// 这两个都是对的拒绝,认其一;其余一律判红(含 tip = 0,即根本没被拒)—— 只告警的话,真回归会混在
	// 每次全绿运行都有的那条 Warn 里看不出来。
	if change.tip != tipInvalidParameter && change.tip != tipEquipItemNotFound {
		fail("equip-zero", "EquipItem(item_id=0) 应回 kInvalidParameter(%d)或 kEquipItemNotFound(%d),实际 tip=%s(0 = 没有被拒)",
			tipInvalidParameter, tipEquipItemNotFound, equipTipText(change.tip))
	}
	zeroTip := change.tip
	// 比两个包里现有的 guid 都大,保证不存在
	var unknownItemId uint64
	for _, bag := range []*scene.BagInfo{finalInventory, finalEquipment} {
		for _, item := range bag.GetItems() {
			if item.GetItemId() > unknownItemId {
				unknownItemId = item.GetItemId()
			}
		}
	}
	unknownItemId += 999983
	change, err = session.equip(unknownItemId)
	if err != nil {
		fail("equip-unknown", "%v", err)
	}
	if change.tip != tipEquipItemNotFound {
		fail("equip-unknown", "穿一个不存在的 guid 应回 kEquipItemNotFound(%d),实际 tip=%s",
			tipEquipItemNotFound, equipTipText(change.tip))
	}
	// 同时核对「卸下响应里的两个包」就是 GetBag 读到的全量(客户端拿响应直接覆盖缓存)
	if err := session.expectBagsUnchanged(finalInventory, finalEquipment); err != nil {
		fail("rejections-unchanged", "%v", err)
	}
	zap.L().Info("[equip-smoke] step 8: unequip is reversible, rejections ok",
		zap.String("equip_zero_tip", equipTipText(zeroTip)))

	// ---- 步骤 9:通过 ----
	zap.L().Info(fmt.Sprintf(
		"EQUIP_SMOKE_OK player=%d steps=9 weapon_a=%d weapon_b=%d shoes=%d affixes=%d/%d/%d granted=%d "+
			"physical_attack=%d->%d defense=%d->%d speed=%d->%d",
		session.gc.PlayerId, weaponA.GetItemId(), weaponB.GetItemId(), shoes.GetItemId(),
		len(weaponA.GetAffixes()), len(weaponB.GetAffixes()), len(shoes.GetAffixes()), granted,
		bare.GetDerived().GetPhysicalAttack(), withBoth.GetDerived().GetPhysicalAttack(),
		bare.GetDerived().GetDefense(), withBoth.GetDerived().GetDefense(),
		bare.GetDerived().GetSpeed(), withBoth.GetDerived().GetSpeed()))
	cleanup()
	_ = zap.L().Sync()
}

// ---------------------------------------------------------------------------
// 会话辅助
// ---------------------------------------------------------------------------

// openEquipSmokeSession 取 gate、登录进场(账号没有角色时自动建角),返回新会话。首次登录与重登共用。
func openEquipSmokeSession(cfg *config.Config, stats *metrics.Stats) (*equipSmokeSession, error) {
	host, portStr, tokenPayload, tokenSig, err := resolveGateAddrLocal(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve gate: %w", err)
	}
	port, _ := strconv.Atoi(portStr)
	gc, player, err := prepareBehaviorClient(host, port, equipSmokeAccount, cfg.Password, stats, tokenPayload, tokenSig)
	if err != nil {
		return nil, fmt.Errorf("account=%s: %w", equipSmokeAccount, err)
	}
	return &equipSmokeSession{gc: gc, player: player, stats: stats, lastSent: make(map[uint32]time.Time)}, nil
}

// close 下线(LeaveGame + Disconnect)并关连接。nil 安全、可重复调:失败路径上 cleanup 可能在 relogin
// 已经关过旧会话之后再调一次。
func (s *equipSmokeSession) close() {
	if s == nil || s.gc == nil {
		return
	}
	_ = leaveGame(s.gc, s.stats)
	sendDisconnectBestEffort(s.gc)
	gameobject.PlayerList.Delete(s.gc.PlayerId)
	s.gc.Close()
	s.gc = nil
}

// relogin 下线后用同一账号重新登录进场,返回新会话(旧会话此后不可再用)。
// 中间等 2s 让 scene 的异步存盘落到 db,否则重登可能读到旧档
// (HandlePlayerAsyncSaved 会把过快的重登判成"重连取代登出")。
func (s *equipSmokeSession) relogin(cfg *config.Config) (*equipSmokeSession, error) {
	stats := s.stats
	s.close()
	time.Sleep(2 * time.Second)
	return openEquipSmokeSession(cfg, stats)
}

// throttle 保证同一个消息号两次发送之间至少隔 equipSmokeSameIdGap(原因见该常量)。
func (s *equipSmokeSession) throttle(messageId uint32) {
	if last, ok := s.lastSent[messageId]; ok {
		if wait := equipSmokeSameIdGap - time.Since(last); wait > 0 {
			time.Sleep(wait)
		}
	}
	s.lastSent[messageId] = time.Now()
}

// call 发一个背包 / 装备 RPC 并等**它自己的**响应,按「来源消息号 + 序号游标」认领:
// 游标在发送前取,只认序号更新、来源等于本次消息号的那一份(handler.RecordFeatureBody 落的快照)。
//
//	成功          → (响应体, 0, nil)
//	业务拒绝      → (nil, tip, nil):tip 来自响应体 error_message,是不是预期内由调用方判断
//	其它          → (nil, 0, err):发送失败 / 超时 / 全零或残缺的响应
//
// 「全零响应」专门说一句:gate 级拒绝(限流、GM 闸、超长包)把错误码写在信封 MessageContent.error_message、
// body 为空。prepareBehaviorClient 的收包回调目前只把属性消息的信封错误翻译成 tip,背包这几条会被照常分发成
// 一份全零响应 —— 没有 tip、也没有包。这里必须把它判成失败,否则穿 / 脱会被当成「成功但两个包都是空的」。
// 真实的拒绝码看紧挨着的那条 `gate envelope error` 日志。
func (s *equipSmokeSession) call(messageId uint32, request proto.Message) (proto.Message, uint32, error) {
	s.throttle(messageId)
	cursor := s.player.FeatureSequence()
	if err := s.gc.SendRequest(messageId, request); err != nil {
		return nil, 0, fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	s.stats.MsgSent()
	deadline := time.Now().Add(equipSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if snapshot, ok := s.player.FeatureResponse(messageId, cursor); ok {
			if snapshot.TipID != 0 {
				return nil, snapshot.TipID, nil
			}
			if snapshot.Failure != "" || snapshot.Response == nil {
				return nil, 0, fmt.Errorf("message_id=%d 回了全零 / 残缺的响应(%s):多半是 gate 级拒绝(限流 / GM 闸),"+
					"拒绝码见紧挨着的 `gate envelope error` 日志", messageId, snapshot.Failure)
			}
			return snapshot.Response, 0, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, 0, fmt.Errorf("等响应超时 message_id=%d(scene 没有这个方法,或 robot 不是用 proto-gen 之后的产物重编的?)", messageId)
}

// getBag 读一个包的全量快照,并复核快照自身一致(布局引用的实例都存在、不重复、不悬空)。
func (s *equipSmokeSession) getBag(bagType uint32) (*scene.BagInfo, error) {
	body, tip, err := s.call(game.SceneBagClientPlayerGetBagMessageId, &scene.GetBagRequest{BagType: bagType})
	if err != nil {
		return nil, err
	}
	if tip != 0 {
		return nil, fmt.Errorf("GetBag(bag_type=%d) 被拒: tip=%s", bagType, equipTipText(tip))
	}
	response, ok := body.(*scene.GetBagResponse)
	if !ok {
		return nil, fmt.Errorf("GetBag 的响应类型不符: %T", body)
	}
	if err := checkEquipBagSnapshot(response.GetBag(), bagType); err != nil {
		return nil, err
	}
	return response.GetBag(), nil
}

// gmGrant 往人物背包发 count 件 configId(不可叠加物品 = count 个独立实例,各掷一次随机属性)。
// 返回 (人物背包全量, 0, nil) 或 (nil, tip, nil)。
func (s *equipSmokeSession) gmGrant(configId, count uint32) (*scene.BagInfo, uint32, error) {
	body, tip, err := s.call(game.SceneBagClientPlayerGmGrantItemMessageId,
		&scene.GmGrantItemRequest{ConfigId: configId, Count: count})
	if err != nil || tip != 0 {
		return nil, tip, err
	}
	response, ok := body.(*scene.GmGrantItemResponse)
	if !ok {
		return nil, 0, fmt.Errorf("GmGrantItem 的响应类型不符: %T", body)
	}
	if err := checkEquipBagSnapshot(response.GetBag(), equipSmokeBagInventory); err != nil {
		return nil, 0, err
	}
	return response.GetBag(), 0, nil
}

// equipBagsResponse 是穿 / 脱两种响应的公共形状(protoc-gen-go 生成的 getter)。
type equipBagsResponse interface {
	GetBag() *scene.BagInfo
	GetEquipment() *scene.BagInfo
}

func (s *equipSmokeSession) equip(itemId uint64) (equipChange, error) {
	return s.changeEquip(game.SceneBagClientPlayerEquipItemMessageId, &scene.EquipItemRequest{ItemId: itemId})
}

func (s *equipSmokeSession) unequip(itemId uint64) (equipChange, error) {
	return s.changeEquip(game.SceneBagClientPlayerUnequipItemMessageId, &scene.UnequipItemRequest{ItemId: itemId})
}

// changeEquip 发一次穿 / 脱。业务拒绝不是 error:结果里 tip != 0、两个包为 nil。
func (s *equipSmokeSession) changeEquip(messageId uint32, request proto.Message) (equipChange, error) {
	// 发请求之前记下面板序号:成功后服务器会推一份新面板,panelAfter 靠这个游标认出它
	_, panelSeq := s.player.GetAttributePanel()
	body, tip, err := s.call(messageId, request)
	if err != nil {
		return equipChange{}, err
	}
	if tip != 0 {
		return equipChange{tip: tip, panelSeq: panelSeq}, nil
	}
	response, ok := body.(equipBagsResponse)
	if !ok {
		return equipChange{}, fmt.Errorf("message_id=%d 的响应类型不符: %T", messageId, body)
	}
	if err := checkEquipBagSnapshot(response.GetBag(), equipSmokeBagInventory); err != nil {
		return equipChange{}, err
	}
	if err := checkEquipBagSnapshot(response.GetEquipment(), equipSmokeBagEquipment); err != nil {
		return equipChange{}, err
	}
	return equipChange{bag: response.GetBag(), equipment: response.GetEquipment(), panelSeq: panelSeq}, nil
}

// expectBagsUnchanged 重新读两个包,要求与给定的两份快照完全一致(比较时忽略 repeated 的线上顺序)。
func (s *equipSmokeSession) expectBagsUnchanged(inventory, equipment *scene.BagInfo) error {
	nowInventory, err := s.getBag(equipSmokeBagInventory)
	if err != nil {
		return err
	}
	nowEquipment, err := s.getBag(equipSmokeBagEquipment)
	if err != nil {
		return err
	}
	if !featureBagsEqual(inventory, nowInventory) {
		return fmt.Errorf("人物背包与预期的快照不一致: 预期 %d 件,现在 %d 件",
			len(inventory.GetItems()), len(nowInventory.GetItems()))
	}
	if !featureBagsEqual(equipment, nowEquipment) {
		return fmt.Errorf("装备栏与预期的快照不一致: 预期 %d 件 / %d 个槽位定义,现在 %d 件 / %d 个槽位定义",
			len(equipment.GetItems()), len(equipment.GetLayout().GetEquipSlots()),
			len(nowEquipment.GetItems()), len(nowEquipment.GetLayout().GetEquipSlots()))
	}
	return nil
}

// attributePanel 发一个属性 RPC 并等它带回的面板。只认「来源 = 本次请求消息号」的那一份:
// GmSetPlayerLevel 会先推一份(NotifyAttributePanelChanged)再回响应,不看来源就会把推送当成响应消费。
// 属性消息的 gate 信封错误已由 prepareBehaviorClient 翻译成 tip,这里按 tip 报错即可。
func (s *equipSmokeSession) attributePanel(messageId uint32, request proto.Message) (*scene.AttributePanelInfo, error) {
	s.throttle(messageId)
	s.player.SetAttributePanel(nil, 0, 0) // 只清上一次留下的 tip;面板传 nil 时序号不动
	_, cursor := s.player.GetAttributePanel()
	if err := s.gc.SendRequest(messageId, request); err != nil {
		return nil, fmt.Errorf("send message_id=%d: %w", messageId, err)
	}
	s.stats.MsgSent()
	deadline := time.Now().Add(equipSmokeRpcTimeout)
	for time.Now().Before(deadline) {
		if tip := s.player.GetAttributeLastTip(); tip != 0 {
			return nil, fmt.Errorf("属性请求 message_id=%d 被拒: tip=%s", messageId, equipTipText(tip))
		}
		panel, seq := s.player.GetAttributePanel()
		if panel != nil && seq > cursor && s.player.GetAttributePanelSource() == messageId {
			return panel, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("等属性响应超时 message_id=%d", messageId)
}

// panelAfter 取一次成功穿 / 脱之后的面板。两件事都验(设计文档 §4.3:成功后 Recalculate + PushPanel):
//  1. 服务器主动推了面板(来源 = NotifyAttributePanelChanged、序号比发请求前新)——
//     客户端开着的属性窗只靠这条推送刷新,漏推就是「换了装备数值不动」;
//  2. 再主动取一份作权威值,且与推送那份数值一致(推的不是中间态)。
func (s *equipSmokeSession) panelAfter(change equipChange) (*scene.AttributePanelInfo, error) {
	var pushed *scene.AttributePanelInfo
	deadline := time.Now().Add(equipSmokeRpcTimeout)
	for pushed == nil && time.Now().Before(deadline) {
		panel, seq := s.player.GetAttributePanel()
		if panel != nil && seq > change.panelSeq &&
			s.player.GetAttributePanelSource() == game.SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId {
			pushed = panel
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pushed == nil {
		return nil, fmt.Errorf("穿 / 脱成功后 %s 内没收到属性面板推送(NotifyAttributePanelChanged)", equipSmokeRpcTimeout)
	}
	fetched, err := s.attributePanel(game.SceneAttributeClientPlayerGetAttributePanelMessageId,
		&scene.GetAttributePanelRequest{})
	if err != nil {
		return nil, err
	}
	if diff := equipPanelDiff(pushed, fetched); len(diff) != 0 {
		return nil, fmt.Errorf("推送的面板与随后主动取的不一致(推送 -> 主动取): %s", strings.Join(diff, "; "))
	}
	return fetched, nil
}

// ---------------------------------------------------------------------------
// 背包取值与校验(纯函数,单测见 equip_smoke_scenario_test.go)
// ---------------------------------------------------------------------------

// equipTipText 把 tip id 写成「数字(枚举名)」,失败日志里不用再去翻表。枚举名来自导表器生成的 *_name 映射;
// tip 码全仓一条数轴,三张映射之间不会撞号。
func equipTipText(tip uint32) string {
	if tip == 0 {
		return "0"
	}
	for _, names := range []map[int32]string{tiptable.EquipError_name, tiptable.BagError_name, tiptable.CommonError_name} {
		if name, ok := names[int32(tip)]; ok {
			return fmt.Sprintf("%d(%s)", tip, name)
		}
	}
	return strconv.FormatUint(uint64(tip), 10)
}

func equipBagItem(bag *scene.BagInfo, itemId uint64) *scene.BagItemInfo {
	for _, item := range bag.GetItems() {
		if item.GetItemId() == itemId {
			return item
		}
	}
	return nil
}

// equipBagItemsOfConfig 返回包里某个 config 的全部实例,按 item_id 升序 —— BagInfo.items 的顺序没有语义,
// 「第一把 / 第二把」必须自己定序,否则同一个账号两次运行可能挑到不同的实例。
func equipBagItemsOfConfig(bag *scene.BagInfo, configId uint32) []*scene.BagItemInfo {
	var result []*scene.BagItemInfo
	for _, item := range bag.GetItems() {
		if item.GetConfigId() == configId {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].GetItemId() < result[j].GetItemId() })
	return result
}

// equipSlotOccupant 返回占着某个槽的 item_id,空槽返回 0。
func equipSlotOccupant(bag *scene.BagInfo, slot uint32) uint64 {
	for _, entry := range bag.GetLayout().GetSlots() {
		if entry.GetSlot() == slot {
			return entry.GetItemId()
		}
	}
	return 0
}

// equipBagFreeSlots 返回包里的空位数。人物背包是扁平布局、一件占一格(checkEquipBagSnapshot 已核对宽高都是 1)。
func equipBagFreeSlots(bag *scene.BagInfo) int {
	free := int(bag.GetLayout().GetCapacity()) - len(bag.GetLayout().GetSlots())
	if free < 0 {
		return 0
	}
	return free
}

// equipLinesText 把属性行压成一行日志:attr_id:t档#序号=值/上限。
func equipLinesText(lines []*scene.EquipAttrLineInfo) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, fmt.Sprintf("%d:t%d#%d=%d/%d",
			line.GetAttrId(), line.GetTier(), line.GetSeq(), line.GetValue(), line.GetCap()))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// checkEquipBagSnapshot 复核一份包快照:类型是要的那个包,且布局与实例互相对得上
// (featureBagInventory:每个槽引用一个存在且唯一的实例、没有未摆放的实例)。
func checkEquipBagSnapshot(bag *scene.BagInfo, bagType uint32) error {
	if bag.GetLayout() == nil {
		return fmt.Errorf("bag_type=%d 的快照没有 layout", bagType)
	}
	if bag.GetLayout().GetBagType() != bagType {
		return fmt.Errorf("要的是 bag_type=%d,快照里是 bag_type=%d", bagType, bag.GetLayout().GetBagType())
	}
	if _, err := featureBagInventory(bag); err != nil {
		return fmt.Errorf("bag_type=%d 的快照不自洽: %w", bagType, err)
	}
	return nil
}

// checkEquipAffixLines 校验一件装备的随机属性行(设计文档 §2.2 / §2.4 / §3.2):
//
//	条数在 equipSmokeMinAffixes..equipSmokeMaxAffixes,且各档各自在范围内(蓝 1..3、粉至多 1、黄至多 1);
//	tier ∈ {1 蓝, 2 粉, 3 黄};attr_id 与名称非空;
//	1 <= value <= cap;蓝属性之间 attr_id 互不重复(粉 / 黄允许与蓝重复);
//	同一档内 seq 从 0 连续,整体按 (tier, seq) 升序 —— 显示顺序只认这两个字段,不认 repeated 下标。
func checkEquipAffixLines(lines []*scene.EquipAttrLineInfo) error {
	if len(lines) < equipSmokeMinAffixes || len(lines) > equipSmokeMaxAffixes {
		return fmt.Errorf("应有 %d..%d 行,实际 %d 行", equipSmokeMinAffixes, equipSmokeMaxAffixes, len(lines))
	}
	blueAttrs := make(map[uint32]bool)
	tierRows := make(map[uint32]int) // 档 → 该档的行数
	for index, line := range lines {
		tier := line.GetTier()
		if tier < equipTierBlue || tier > equipTierYellow {
			return fmt.Errorf("第 %d 行 tier=%d,应在 %d(蓝)..%d(黄)之间", index, tier, equipTierBlue, equipTierYellow)
		}
		tierRows[tier]++
		if line.GetAttrId() == 0 || line.GetName() == "" {
			return fmt.Errorf("第 %d 行 attr_id=%d name=%q:id 与名称都不能为空(EquipAttribute 表缺行?需先导表)",
				index, line.GetAttrId(), line.GetName())
		}
		if line.GetValue() < 1 || line.GetValue() > line.GetCap() {
			return fmt.Errorf("第 %d 行 value=%d 不在 1..cap(%d) 之内", index, line.GetValue(), line.GetCap())
		}
		wantSeq := uint32(0)
		if index > 0 {
			previous := lines[index-1]
			if tier < previous.GetTier() {
				return fmt.Errorf("第 %d 行 tier=%d 排在 tier=%d 之后:应按 (tier, seq) 升序", index, tier, previous.GetTier())
			}
			if tier == previous.GetTier() {
				wantSeq = previous.GetSeq() + 1
			}
		}
		if line.GetSeq() != wantSeq {
			return fmt.Errorf("第 %d 行(tier=%d)seq=%d,应为 %d:同档 seq 从 0 连续", index, tier, line.GetSeq(), wantSeq)
		}
		if tier == equipTierBlue {
			if blueAttrs[line.GetAttrId()] {
				return fmt.Errorf("蓝属性 attr_id=%d 重复(蓝属性之间不放回抽取)", line.GetAttrId())
			}
			blueAttrs[line.GetAttrId()] = true
		}
	}
	// 各档条数:总数在 1..5 之内并不够 —— 4 条蓝、两条粉、只有粉没有蓝,都是掷值规则掷不出来的形状
	blue, pink, yellow := tierRows[equipTierBlue], tierRows[equipTierPink], tierRows[equipTierYellow]
	if blue < equipSmokeMinBlueAffixes || blue > equipSmokeMaxBlueAffixes {
		return fmt.Errorf("蓝属性应有 %d..%d 条,实际 %d 条(粉 %d 条、黄 %d 条)",
			equipSmokeMinBlueAffixes, equipSmokeMaxBlueAffixes, blue, pink, yellow)
	}
	if pink > equipSmokeMaxPinkAffixes || yellow > equipSmokeMaxYellowAffixes {
		return fmt.Errorf("粉属性至多 %d 条、黄属性至多 %d 条,实际 粉 %d 条、黄 %d 条",
			equipSmokeMaxPinkAffixes, equipSmokeMaxYellowAffixes, pink, yellow)
	}
	return nil
}

// checkEquipBaseLines 校验基础属性行:与表里的 Item.base_attr 逐行一致(顺序 = 表里的顺序),
// tier = 0、cap = 0(基础属性不显示「/上限」)、seq 从 0 递增、名称非空。
func checkEquipBaseLines(lines []*scene.EquipAttrLineInfo, want []equipBaseAttr) error {
	if len(lines) != len(want) {
		return fmt.Errorf("应恰有 %d 行,实际 %d 行", len(want), len(lines))
	}
	for index, line := range lines {
		expected := want[index]
		if line.GetAttrId() != expected.attrId || line.GetValue() != expected.value {
			return fmt.Errorf("第 %d 行应是 attr_id=%d value=%d,实际 attr_id=%d value=%d",
				index, expected.attrId, expected.value, line.GetAttrId(), line.GetValue())
		}
		if line.GetTier() != equipTierBase || line.GetCap() != 0 {
			return fmt.Errorf("第 %d 行 tier=%d cap=%d,基础属性应为 tier=0 cap=0", index, line.GetTier(), line.GetCap())
		}
		if line.GetSeq() != uint32(index) {
			return fmt.Errorf("第 %d 行 seq=%d,应从 0 递增", index, line.GetSeq())
		}
		if line.GetName() == "" {
			return fmt.Errorf("第 %d 行(attr_id=%d)名称为空(EquipAttribute 表缺行?需先导表)", index, line.GetAttrId())
		}
	}
	return nil
}

// checkEquipItemDisplay 校验一件装备的 tooltip 字段:名称、不可叠加、佩戴等级、基础属性、随机属性。
func checkEquipItemDisplay(item *scene.BagItemInfo, wantLevel uint32, wantBase []equipBaseAttr) error {
	if item.GetName() == "" {
		return fmt.Errorf("name 为空(Item 表的新列没有数据,需先导表)")
	}
	if item.GetEquipKind() == 0 {
		return fmt.Errorf("equip_kind = 0,不是装备")
	}
	if item.GetCount() != 1 || item.GetMaxStack() != 1 {
		return fmt.Errorf("装备必须不可叠加: count=%d max_stack=%d", item.GetCount(), item.GetMaxStack())
	}
	if item.GetEquipLevel() != wantLevel {
		return fmt.Errorf("equip_level=%d,应为 %d", item.GetEquipLevel(), wantLevel)
	}
	if err := checkEquipBaseLines(item.GetBaseAttrs(), wantBase); err != nil {
		return fmt.Errorf("base_attrs %s: %w", equipLinesText(item.GetBaseAttrs()), err)
	}
	if err := checkEquipAffixLines(item.GetAffixes()); err != nil {
		return fmt.Errorf("affixes %s: %w", equipLinesText(item.GetAffixes()), err)
	}
	return nil
}

// equipDisplayHint 给第 1 步(display-*)的失败信息补一句:红的这件实例是复用的还是本轮新铸的、修好之后怎么重跑。
//
// 随机属性只在新铸时掷一次,之后任何路径都不重掷(设计文档 §5 不变量 1);预备段又是「已有就复用」。
// 两条合起来:一件在导表不全 / 掷值挂点没装上的 scene 上铸出来的装备会一直带着当时的结果(0 条随机属性),
// 服务端修好之后同账号重跑,红的还是它 —— 不提示就会被误判成「没修好」。
// 名称 / 佩戴等级 / 基础属性不在此列:它们每次按表现算(不变量 2),导表并重启 scene 即生效。
//
// reused:发物之前就已经在人物背包里的 item_id。
func equipDisplayHint(item *scene.BagItemInfo, reused map[uint64]bool) string {
	origin := "本轮 GmGrantItem 新铸的,随机属性(affixes)不对就出在当前这批 scene / 配表上" +
		"(查 scene 日志里的 [PlayerEquip] ERROR),而且它会留在背包里被下一轮复用"
	if reused[item.GetItemId()] {
		origin = "复用的旧实例(本轮没有新发),随机属性(affixes)是它当初铸出来时的结果"
	}
	return fmt.Sprintf(" [reused=%t] 这件是%s。随机属性只在新铸时掷一次、不重掷:红在 affixes 的话,修好服务端之后"+
		"还得把 equipSmokeAccount(现为 %s)换成新账号、或清掉该账号的背包存档再重跑;"+
		"红在 name / equip_level / base_attrs 的话不用换号(它们每次按表现算,导表并重启 scene 即可)",
		reused[item.GetItemId()], origin, equipSmokeAccount)
}

// checkEquipSlotDefs 校验装备栏快照里的槽位定义(BagLayoutInfo.equip_slots):slot 升序、不重复;
// wantSlots 里的槽都在,且有部位、有名称。返回 槽号 → 定义。
func checkEquipSlotDefs(layout *scene.BagLayoutInfo, wantSlots ...uint32) (map[uint32]*scene.EquipSlotInfo, error) {
	defs := make(map[uint32]*scene.EquipSlotInfo, len(layout.GetEquipSlots()))
	for index, def := range layout.GetEquipSlots() {
		if _, duplicated := defs[def.GetSlot()]; duplicated {
			return nil, fmt.Errorf("equip_slots 里 %d 号槽重复", def.GetSlot())
		}
		if index > 0 && def.GetSlot() < layout.GetEquipSlots()[index-1].GetSlot() {
			return nil, fmt.Errorf("equip_slots 没按 slot 升序:%d 排在 %d 之后",
				def.GetSlot(), layout.GetEquipSlots()[index-1].GetSlot())
		}
		defs[def.GetSlot()] = def
	}
	for _, slot := range wantSlots {
		def, ok := defs[slot]
		if !ok {
			return nil, fmt.Errorf("equip_slots 里没有 %d 号槽(一共下发了 %d 个槽;EquipSlot 表缺行,需先导表)", slot, len(defs))
		}
		if def.GetEquipKind() == 0 || def.GetName() == "" {
			return nil, fmt.Errorf("%d 号槽 equip_kind=%d name=%q:部位与名称都不能为空(EquipSlot 表的 name 列没有数据,需先导表)",
				slot, def.GetEquipKind(), def.GetName())
		}
	}
	return defs, nil
}

// checkEquipSameItem 要求 want 这件装备在 bag 里,且与 want(穿脱之前记下的同一 guid 的快照)逐字段相同。
// 穿脱是「同一 guid 换包」:名称、基础属性、随机属性都不许变(设计文档 §5 不变量 1:不重掷)。
func checkEquipSameItem(bag *scene.BagInfo, want *scene.BagItemInfo) error {
	got := equipBagItem(bag, want.GetItemId())
	if got == nil {
		return fmt.Errorf("item_id=%d(config_id=%d)不在 bag_type=%d 的包里",
			want.GetItemId(), want.GetConfigId(), bag.GetLayout().GetBagType())
	}
	if !proto.Equal(got, want) {
		return fmt.Errorf("item_id=%d 换包后显示数据变了(随机属性被重掷或丢了?):affixes 之前 %s,现在 %s",
			want.GetItemId(), equipLinesText(want.GetAffixes()), equipLinesText(got.GetAffixes()))
	}
	return nil
}

// checkEquipLayout 断言一次穿 / 脱(或重登)之后两个包的形状:
//
//	equipped:槽号 → 应在该槽的装备(穿脱之前的快照);装备栏里恰好只有这些;
//	inBag:   应在人物背包里的装备(穿脱之前的快照);
//	wantTotal:两个包的物品总数(穿脱不产生也不销毁实例)。
//
// 装备栏的每份快照都必须带槽位定义:客户端拿穿 / 脱响应直接覆盖装备栏缓存,少了它整排槽位就画不出来。
func checkEquipLayout(bag, equipment *scene.BagInfo, wantTotal int,
	equipped map[uint32]*scene.BagItemInfo, inBag ...*scene.BagItemInfo) error {
	if err := checkEquipBagSnapshot(bag, equipSmokeBagInventory); err != nil {
		return err
	}
	if err := checkEquipBagSnapshot(equipment, equipSmokeBagEquipment); err != nil {
		return err
	}
	if _, err := checkEquipSlotDefs(equipment.GetLayout(), equipSmokeSlots...); err != nil {
		return err
	}
	if total := len(bag.GetItems()) + len(equipment.GetItems()); total != wantTotal {
		return fmt.Errorf("两个包的物品总数应守恒为 %d,实际 %d(人物背包 %d + 装备栏 %d)",
			wantTotal, total, len(bag.GetItems()), len(equipment.GetItems()))
	}
	if len(equipment.GetItems()) != len(equipped) {
		return fmt.Errorf("装备栏应恰有 %d 件,实际 %d 件", len(equipped), len(equipment.GetItems()))
	}
	slots := make([]uint32, 0, len(equipped))
	for slot := range equipped {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	for _, slot := range slots {
		want := equipped[slot]
		if occupant := equipSlotOccupant(equipment, slot); occupant != want.GetItemId() {
			return fmt.Errorf("装备栏 %d 号槽应是 item_id=%d,实际 %d(0 = 空槽)", slot, want.GetItemId(), occupant)
		}
		if err := checkEquipSameItem(equipment, want); err != nil {
			return err
		}
		// 总数守恒时数量检查是瞎的:背包里别的东西少了一件、同时多出一份已穿上的 guid,只有这一条拦得住
		if equipBagItem(bag, want.GetItemId()) != nil {
			return fmt.Errorf("item_id=%d 已穿上,却还留在人物背包里(实例被复制)", want.GetItemId())
		}
	}
	// 反方向(应在背包里的 guid 同时出现在装备栏)不需要再查一遍:走到这里,装备栏已被前面的检查钉死为
	// 「恰好 len(equipped) 件、且就是 equipped 里那几件」(快照自洽 = 一槽一件互不重复 + 件数相等 + 逐槽核对),
	// 多出来的任何一件都已经在「装备栏应恰有 N 件」那一步红了(单测里的第 2 / 3 段)。
	for _, want := range inBag {
		if err := checkEquipSameItem(bag, want); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 面板取值与校验(纯函数)
// ---------------------------------------------------------------------------

// equipDerivedStat 是面板二级属性里的一项**上限**。
type equipDerivedStat struct {
	name string
	get  func(*scene.DerivedAttributeInfo) uint64
}

// 六项二级属性上限。不含当前气血 / 法力:穿脱按比例保持当前值(设计文档 §4.4,穿脱不能当治疗),
// 取整会让它们差 1,不属于「可逆 / 一致」的断言范围。
var equipDerivedStats = []equipDerivedStat{
	{"max_health", (*scene.DerivedAttributeInfo).GetMaxHealth},
	{"max_mana", (*scene.DerivedAttributeInfo).GetMaxMana},
	{"physical_attack", (*scene.DerivedAttributeInfo).GetPhysicalAttack},
	{"magic_attack", (*scene.DerivedAttributeInfo).GetMagicAttack},
	{"speed", (*scene.DerivedAttributeInfo).GetSpeed},
	{"defense", (*scene.DerivedAttributeInfo).GetDefense},
}

func equipCombatValues(panel *scene.AttributePanelInfo) map[uint32]uint64 {
	values := make(map[uint32]uint64, len(panel.GetCombat()))
	for _, row := range panel.GetCombat() {
		values[row.GetCombatId()] = row.GetValue()
	}
	return values
}

func equipDimension(panel *scene.AttributePanelInfo, dimensionId uint32) *scene.AttributeDimensionInfo {
	for _, dimension := range panel.GetDimensions() {
		if dimension.GetDimensionId() == dimensionId {
			return dimension
		}
	}
	return nil
}

// equipPanelCompare 逐项比较两份面板里「会被装备影响」的全部数值:六项二级属性上限、每个一级维度的
// value / bonus、每行战斗属性的 value。holds(was, is) 为假的项各产出一条「名称 was -> is」。
func equipPanelCompare(before, after *scene.AttributePanelInfo, holds func(was, is uint64) bool) []string {
	var broken []string
	note := func(name string, beforeValue, afterValue uint64) {
		if !holds(beforeValue, afterValue) {
			broken = append(broken, fmt.Sprintf("%s %d -> %d", name, beforeValue, afterValue))
		}
	}
	for _, stat := range equipDerivedStats {
		note(stat.name, stat.get(before.GetDerived()), stat.get(after.GetDerived()))
	}
	for _, dimension := range before.GetDimensions() {
		other := equipDimension(after, dimension.GetDimensionId())
		if other == nil {
			broken = append(broken, fmt.Sprintf("dimension[%d] 不见了", dimension.GetDimensionId()))
			continue
		}
		note(fmt.Sprintf("dimension[%d].value", dimension.GetDimensionId()), dimension.GetValue(), other.GetValue())
		note(fmt.Sprintf("dimension[%d].bonus", dimension.GetDimensionId()),
			uint64(dimension.GetBonus()), uint64(other.GetBonus()))
	}
	afterCombat := equipCombatValues(after)
	for _, row := range before.GetCombat() {
		value, ok := afterCombat[row.GetCombatId()]
		if !ok {
			broken = append(broken, fmt.Sprintf("combat[%d] 不见了", row.GetCombatId()))
			continue
		}
		note(fmt.Sprintf("combat[%d]", row.GetCombatId()), row.GetValue(), value)
	}
	if len(before.GetDimensions()) != len(after.GetDimensions()) || len(before.GetCombat()) != len(after.GetCombat()) {
		broken = append(broken, fmt.Sprintf("行数变了: dimensions %d -> %d, combat %d -> %d",
			len(before.GetDimensions()), len(after.GetDimensions()), len(before.GetCombat()), len(after.GetCombat())))
	}
	return broken
}

// equipPanelDiff 返回两份面板不相等的项(空 = 完全一致)。
func equipPanelDiff(want, got *scene.AttributePanelInfo) []string {
	return equipPanelCompare(want, got, func(was, is uint64) bool { return is == was })
}

// equipPanelDrops 返回 after 比 before **变小**的项(空 = 没有任何一项下降)。
func equipPanelDrops(before, after *scene.AttributePanelInfo) []string {
	return equipPanelCompare(before, after, func(was, is uint64) bool { return is >= was })
}

// checkEquipPanelShape 校验面板里与装备相关的字段形状(设计文档 §3.2):二级属性在、
// 战斗属性 15 项全量下发、combat_id 不重复、名称非空(客户端逐行照显)、按 sort 升序。
func checkEquipPanelShape(panel *scene.AttributePanelInfo) error {
	if panel.GetDerived() == nil || panel.GetDerived().GetMaxHealth() == 0 {
		return fmt.Errorf("二级属性缺失或气血上限为 0")
	}
	if len(panel.GetCombat()) != equipSmokeCombatRows {
		return fmt.Errorf("面板的战斗属性应全量下发 %d 行,实际 %d 行(scene 没按 §4.4 填 AttributePanelInfo.combat?)",
			equipSmokeCombatRows, len(panel.GetCombat()))
	}
	seen := make(map[uint32]bool, len(panel.GetCombat()))
	for index, row := range panel.GetCombat() {
		if row.GetCombatId() == 0 || seen[row.GetCombatId()] {
			return fmt.Errorf("战斗属性第 %d 行 combat_id=%d 为 0 或重复", index, row.GetCombatId())
		}
		seen[row.GetCombatId()] = true
		if row.GetName() == "" {
			return fmt.Errorf("战斗属性 combat_id=%d 名称为空(EquipAttribute 表缺 effect=5 的行?需先导表)", row.GetCombatId())
		}
		if index > 0 && row.GetSort() < panel.GetCombat()[index-1].GetSort() {
			return fmt.Errorf("战斗属性没按 sort 升序:combat_id=%d(sort=%d)排在 sort=%d 之后",
				row.GetCombatId(), row.GetSort(), panel.GetCombat()[index-1].GetSort())
		}
	}
	return nil
}

// checkEquipGain 核对「在 base 这个状态上再穿 added 这几件」之后的面板 now。机器人和客户端一样零配表,
// 所以只断言不查表就能确定的事:
//
//  1. 没有任何一项下降:六项二级属性上限、每个一级维度的 value / bonus、每行战斗属性(装备只加不减);
//  2. 基础属性用到的三种属性按 id 给出精确下界(设计文档 §2.1):
//     「伤害」→ 物伤与法伤各至少 +N;「防御」→ 防御至少 +N;「速度」→ 速度至少 +N。
//     N 把 added 上该属性的基础行与随机行都算进去 —— 只加了基础属性、漏了随机属性会在这里露出来;
//  3. 战斗类随机属性按**名称**对上面板的「战斗属性」行:面板那一行的名字就是从 EquipAttribute 表同一行
//     反查出来的(§4.3 DescribeCombatStat),所以同名即同一条属性,该行至少要涨这么多;
//     百分比行的终值按 100 封顶来比。
//
// 用「至少」而不是「恰好」:一级属性点(力量 / 体质 …)也会抬高二级属性,换算系数机器人不知道。
// 「恰好」那一半由第 8 步的可逆断言(全部卸下后与裸装逐项相等)补上。
func checkEquipGain(base, now *scene.AttributePanelInfo, added ...*scene.BagItemInfo) error {
	if drops := equipPanelDrops(base, now); len(drops) != 0 {
		return fmt.Errorf("穿上装备后有数值下降: %s", strings.Join(drops, "; "))
	}
	var damage, defense, speed uint64
	gainByName := make(map[string]uint64)
	for _, item := range added {
		for _, lines := range [][]*scene.EquipAttrLineInfo{item.GetBaseAttrs(), item.GetAffixes()} {
			for _, line := range lines {
				switch line.GetAttrId() {
				case equipAttrDamage:
					damage += line.GetValue()
				case equipAttrDefense:
					defense += line.GetValue()
				case equipAttrSpeed:
					speed += line.GetValue()
				}
				if line.GetName() != "" {
					gainByName[line.GetName()] += line.GetValue()
				}
			}
		}
	}
	bounds := []struct {
		name string
		get  func(*scene.DerivedAttributeInfo) uint64
		gain uint64
	}{
		{"physical_attack", (*scene.DerivedAttributeInfo).GetPhysicalAttack, damage},
		{"magic_attack", (*scene.DerivedAttributeInfo).GetMagicAttack, damage},
		{"defense", (*scene.DerivedAttributeInfo).GetDefense, defense},
		{"speed", (*scene.DerivedAttributeInfo).GetSpeed, speed},
	}
	for _, bound := range bounds {
		before, after := bound.get(base.GetDerived()), bound.get(now.GetDerived())
		if after < before+bound.gain {
			return fmt.Errorf("%s 至少应 +%d(装备上的基础 + 随机属性),实际 %d -> %d",
				bound.name, bound.gain, before, after)
		}
	}
	nowCombat := equipCombatValues(now)
	for _, row := range base.GetCombat() {
		gain := gainByName[row.GetName()]
		if row.GetName() == "" || gain == 0 {
			continue
		}
		want := row.GetValue() + gain
		if row.GetPercent() && want > 100 {
			want = 100
		}
		if got := nowCombat[row.GetCombatId()]; got < want {
			return fmt.Errorf("战斗属性「%s」(combat_id=%d)至少应到 %d(穿前 %d + 装备 %d),实际 %d",
				row.GetName(), row.GetCombatId(), want, row.GetValue(), gain, got)
		}
	}
	return nil
}

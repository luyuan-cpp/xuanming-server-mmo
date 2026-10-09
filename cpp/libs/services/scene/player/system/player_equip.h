#pragma once

#include <cstdint>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

#include "engine/core/type_define/type_define.h"  // Guid

#include "equip_attribute_rules.h"

class ItemComp;
class BagItemInfo;
class BagLayoutInfo;

// 装备系统(新装备掷随机属性 + 穿脱 + 装备加成汇总 + tooltip 显示数据),设计文档
// docs/design/equipment-attributes.md §4.3。
//
// 分层(AGENTS §11.6):
//   * 容器层(modules/bag)不认识「属性」,只搬整份实例;本类是唯一认识 EquipAttribute /
//     EquipAttributeCap / EquipAffixPool / EquipAffixRule 四张表与 Item 表装备列的地方。
//   * 「怎么掷、怎么累加」是纯规则,在 equip_attribute_rules.h(equiprules);本类只负责把表行 /
//     实例翻译成规则的输入,并编排容器原语。
//   * 一件装备「是否穿着」= 它在不在 bags[kEquipment];实例上没有「已装备」标记。
//
// 不变量(设计文档 §5):
//   1. 随机属性只在实例「新铸」时掷一次;还原、穿脱、邮件回流不重掷(ItemComp.has_equip() 守护)。
//   2. 上限、名称、基础属性不入库:显示与加成每次按表现查,改表立即对存量装备生效,不洗存档。
//   3. 装备加成不落库:本类不写 PlayerAttributeComp.bonus_values,也不写 BaseAttributesComp。
//   4. 穿脱是「同一 guid 换包」:不写获得 / 销毁流水,不过发放封禁闸。
//
// 返回值:kSuccess(table/proto/tip/common_error_tip.pb.h)以外一律为 tip id —— 以
// table/proto/tip/equip_error_tip.pb.h 的 kEquip* 为主,个别情形透传 common / bag 域的码(各函数注释
// 里列明)。handler 直接搬进 TipInfoMessage。
//
// 写入口(Equip / Unequip / GmGrantItem)统一前置,与 PetSystem / PlayerAttributeSystem 的
// CheckWritable 同口径:实体无效 → kEntityIsNull;跨 zone 冻结(PlayerFrozenComp)→ kEquipFrozen;
// 战斗在途(PlayerBattleSystem::IsInBattle)→ kEquipInBattle。快照已出之后换装会让局内数值与面板分叉。
//
// 线程模型:全部方法都在 scene 节点 loop 线程调用。
class PlayerEquipSystem {
public:
	// ---- 启动 ----

	// 把 RollNewInstance 装成 BagService 的「新铸实例初始化回调」(BagService::SetItemInstanceInitializer),
	// 并做一遍装备相关的表校验。
	//
	// 调用时机:scene 进程启动、配表加载完成之后、任何入包流量(玩家加载 / 掉落 / 邮件)之前调一次。
	//   幂等:重复调用只是重装同一个回调、重做一遍校验。**刻意不带「已安装」标志** —— 每次调用都真的
	//   重新 SetItemInstanceInitializer:回调会被别人卸掉(背包单测夹具每个用例的 SetUp / TearDown 都卸),
	//   靠标志跳过安装的话,卸过之后就再也装不上,此后新装备没有属性且零报错。
	//   安装本身不带同步(见 BagService 的注释),只许在 scene loop 线程调,且不要在处理入包的调用栈里调。
	// 表校验见 ValidateTables:只打 ERROR、不拒启、不改表。
	// scene 的调用点是配表加载完成回调(cpp/nodes/scene/main.cpp 的 TableLoadHandler::OnLoaded):
	//   启动时表一加载完就装;以后若有整批重载(ReloadTables),同一回调会再跑一遍,回调重装、表重新校验。
	//   本类自己不订阅热更 —— 绕开那条回调的单表 Load() 不会触发重新校验。
	static void InstallItemInitializer();

	// 装备相关的表校验:逐行检查、逐条打 ERROR(报全,不在第一条就停),返回发现的问题条数(0 = 自洽)。
	// 计数口径:一行里互不相干的问题各计一条;同一个根因只计一次(下面标了「只报前者」的地方)。
	// 只读:不改表、不拒启、不装回调。InstallItemInitializer 调它;单测也直接调,把「导出的表自洽」钉成用例。
	// 调用前这几张表必须已加载:Item / EquipAttribute / EquipAttributeCap / EquipAffixPool /
	// EquipAffixRule / AttributeDimension / AttributePool(没加载的表按空表算,引用它的行会被报成问题;
	// EquipAttribute 自己没加载时,下面「15 项各有显示行」会报满 15 条)。
	//   * EquipAttribute:effect / effect_param 必须被 equiprules::ApplyEffect 接受(effect 是 1..5;
	//     effect = 3 时 param 是 1..6 的 DerivedStat;effect = 5 时 param 是 1..15 的 CombatStat);
	//     effect = 1 时 param 还必须是 AttributeDimension 里存在的维度,且该维度属于角色池
	//     (判定只有一份:PlayerAttributeSystem::IsPlayerDimension)—— 属性重算只对角色池维度取装备点,
	//     指向宝宝池维度的点数没人读;
	//   * 战斗属性 1..15(equiprules::CombatStat)每一项都要有显示行:EquipAttribute 里 effect = 5 且
	//     effect_param = 该项、name 非空的行(判据就是 DescribeCombatStat 返回的 name 非空,与属性面板
	//     决定「这一行出不出」同源)。缺一项计一条 —— 面板会静默少这一行,别处没有任何报错。
	//     常见成因是两行填了同一个 effect_param(各自都合法,上一条查不出来)。
	//     同一根因只计一次:effect = 5 但 effect_param 越界的行已在上一条报过,它本该承载的那一项
	//     不再另计(只报前者;按项号从小到大,每条被拒的行抵掉一项缺行);
	//   * EquipAttributeCap:1 <= min_value <= cap;
	//   * EquipAffixPool:attr_id 在 EquipAttribute 里、weight > 0、该属性在 EquipAttributeCap 里至少有一档
	//     (attr_id 不存在时上限表里必然也没有它,只报前者);
	//   * EquipAffixRule:blue_min <= blue_max,pink_rate / yellow_rate 不超过 10000;
	//   * Item(只看 equip_kind != 0 的行;以下三条都没有导表期外键):max_stack_size == 1
	//     (可堆叠会把不同属性的实例并成一堆);affix_pool != 0 时 EquipAffixPool 里有该池的行;
	//     affix_rule != 0 时 EquipAffixRule 里有该行;base_attr 的每个 base_attr_id 在 EquipAttribute 里。
	static uint32_t ValidateTables();

	// ---- 新铸实例 ----

	// 给一件**新铸**的物品实例掷随机属性(生产随机源 tlsRandom)。它就是装进 BagService 的那个回调,
	// 所以受 ItemInstanceInitializer 的全部约束(item_system.h):不许失败、不许抛异常、不许在里面再入包、
	// 只许写 item.equip 段(item_id / config_id / size / acquire_seq 一律不碰)。
	//
	// 行为(任何分支都不返回错误):
	//   * item.has_equip() 已为真 → 原样返回(幂等,绝不重掷);
	//   * config 查不到表 / 不是装备(IsEquipment 为假)→ 不动 item(has_equip() 保持为假);
	//   * 是装备但表配成了可堆叠(max_stack_size != 1)→ 同样不动:置位会让它从此不能并堆,
	//     掷了属性也会被并堆吃掉(启动表校验会报这一行);
	//   * 是装备 → 一定 mutable_equip() 置位(「已完成初始化」),哪怕一条都没掷出:
	//       - affix_pool == 0 或 affix_rule == 0 → 0 条(策划配的「不掷」,不打日志);
	//       - 池 / 规则行查不到 → 0 条并打 ERROR(表不一致;启动表校验也会报);
	//       - 否则按 EquipAffixRule + EquipAffixPool + EquipAttributeCap(按 equip_level 取档)调
	//         equiprules::RollAffixes,结果逐条写成 EquipAffix{attr_id, tier, value, seq};
	//         池里指向不存在属性的条目先被排除(打 ERROR),不会掷出查不到定义的属性。
	// 不写流水、不推任何消息、不触发属性重算(实例此刻还没进任何包)。
	// 调用时机:只由 BagService 的三个 AddItem(s) 入口在实例进 ItemStore 之前调;单测可直接调。
	static void RollNewInstance(ItemComp& item);

	// ---- 写入口 ----

	// 穿上:itemId 必须在人物背包(bags[kInventory])里。
	//
	// 槽位规则:取装备栏里接受该部位的槽(Bag::SlotsAcceptingKind,槽号升序)中**第一个空槽**;
	// 都占着就**替换槽号最小的那个**,旧装备回人物背包(新装备刚腾出一格,必然放得下)。
	//
	// 校验顺序(全部纯校验,在任何修改之前完成)与返回的 tip:
	//   统一前置(见类注释)                             kEntityIsNull / kEquipFrozen / kEquipInBattle
	//   玩家没有 PlayerBagsComp(尚未完成加载)          kEquipInternalError
	//   itemId 不在人物背包                              kEquipItemNotFound
	//   config 查不到表、不是装备、或被配成了可堆叠      kEquipNotEquipment
	//   实例 size != 1(被扣光还没回收的空实例)         kEquipItemNotFound
	//   角色等级 < Item.equip_level                      kEquipLevelNotEnough
	//   Item.equip_class != 0 且不等于角色职业           kEquipClassMismatch
	//   装备栏没有任何槽接受该部位                       kEquipNoSlot
	//   (替换时)目标槽的占用者搬不动(数据 / 表损坏)   kEquipInternalError
	// 搬运顺序:人物背包 TakeInstance(新)→(替换时)装备栏 TakeInstance(旧)→ 装备栏
	//   PutInstance(新, 槽)→(替换时)人物背包 PutInstance(旧)。任何一步失败都按相反顺序放回
	//   (先试原槽,原槽放不回就自动找位),打 ERROR 并返回 kEquipInternalError;放回也失败 = 实例丢失,
	//   打一条带固定标签 [INSTANCE_LOST] 的 ERROR,内含整份实例(guid / config / 全部随机属性)供人工补偿。
	//   不用 LOG_FATAL:它会 abort 整个 scene 进程。
	//   回滚后装备栏的内容若与调用前不同(某件装备没能放回装备栏),同样重算属性并推面板,
	//   数值不会停在「还穿着它」的状态;放回成功(哪怕换了槽)不重算。Unequip 的回滚同此。
	//
	// 成功后:PlayerAttributeSystem::Recalculate(player, RecalcReason::kEquipmentChanged)
	//   + PlayerAttributeSystem::PushPanel(player)。当前气血 / 法力按比例保持(穿脱不能当治疗)。
	// 不写流水(同一 guid 换包)、不过发放封禁闸、不重掷属性、不推背包(由 handler 回两个包的全量)。
	static uint32_t Equip(entt::entity player, Guid itemId);

	// 卸下:itemId 必须在装备栏(bags[kEquipment])里,回到人物背包。
	//
	// 校验顺序(全部纯校验,在任何修改之前完成)与返回的 tip:
	//   统一前置(见类注释)                             kEntityIsNull / kEquipFrozen / kEquipInBattle
	//   玩家没有 PlayerBagsComp                          kEquipInternalError
	//   itemId 不在装备栏                                kEquipNotEquipped
	//   穿着的这件搬不动(查不到表 / 可堆叠 / size != 1) kEquipInternalError
	//   人物背包没有空位                                 kEquipBagFull
	//   人物背包有空位却不收它(准入 / 表行异常)        kEquipInternalError
	// 搬运顺序:装备栏 TakeInstance → 人物背包 PutInstance;后一步失败则原槽放回,打 ERROR 并返回
	//   kEquipInternalError(预检过空位仍失败 = 编程错误)。
	//
	// 成功后的属性重算 / 面板推送、以及「不写流水 / 不过封禁闸 / 不重掷 / 不推背包」与 Equip 相同。
	static uint32_t Unequip(entt::entity player, Guid itemId);

	// GM 发物(dev / test 才开;「prod 关掉」由 gate / scene 的 GM 闸负责,本函数**不做鉴权**)。
	// 往人物背包(bags[kInventory])发 count 个 configId:不可叠加物品 = count 个独立实例
	// (装备各自掷一次随机属性),可叠加物品 = 数量 count。
	//
	// 返回的 tip:
	//   统一前置(见类注释)                             kEntityIsNull / kEquipFrozen / kEquipInBattle
	//   configId == 0、Item 表里没有该行、count == 0 或 count > kGmGrantMaxCount   kEquipGrantInvalid
	//   玩家没有 PlayerBagsComp                          kEquipInternalError
	//   其余(背包满、发放封禁等)                        原样透传 BagService::AddItems 的返回码
	// 走 BagService::AddItems(txType = TX_GM_GRANT):写获得流水、过发放封禁闸、过异常检测 ——
	// 与正式发放同一条路,所以新装备的掷属性挂点(ItemInstanceInitializer)自然生效。
	// 不触发属性重算(东西进的是背包,不是装备栏)、不推背包(由 handler 回人物背包全量)。
	static uint32_t GmGrantItem(entt::entity player, uint32_t configId, uint32_t count);

	// GmGrantItem 单次请求的数量上限:它是开发工具,不需要更大;同时兜住「count 传成一个巨数」的手滑。
	static constexpr uint32_t kGmGrantMaxCount = 99;

	// ---- 只读 ----

	// 汇总 player 当前穿着的全部装备的加成:只读 bags[kEquipment],每件 = Item.base_attr(基础属性)
	// + 实例的 equip.affixes(随机属性),逐条经 equiprules::ApplyEffect 累加。
	//
	// out 在函数开头被重置为空,再累加(调用方不必预清)。
	// 实体无效 / 没有 PlayerBagsComp / 装备栏为空 → out 保持为空,不是错误、不打日志。
	// 穿着即生效:这里不复核等级 / 职业要求(只在穿上那一刻校验)。
	// 随机属性的计入值与 FillItemDisplay 的显示值**同口径**(两处共用同一个取值函数):超过当前表上限的
	// 按上限计(改表下调上限立即生效、不洗存档);该属性在这件装备的等级上一档上限都没有时按 0 计并打
	// WARN(tooltip 上同样显示 0 / 上限 0),存档原值不动,表补回来即恢复。
	// 「哪些行算数」也与 FillItemDisplay 同口径 —— 不计入加成的行,tooltip 上也不显示:
	//   * Item 行查不到,或 equip_kind == 0(表里已不是装备,却还躺在装备栏):整件不计并打 ERROR;
	//   * 表里查不到的属性 id、ApplyEffect 拒绝的 effect / param、非法 tier:跳过该条并打 ERROR,不影响其余条目。
	//
	// 纯读、无副作用:不写任何组件、不推消息。组件一律 try_get —— PlayerAttributeSystem::Recalculate
	// 每次重算都调它(属性同步路径,禁 get_or_emplace)。
	static void CollectBonus(entt::entity player, equiprules::EquipBonus& out);

	// 填一件物品的显示字段(客户端零配表,名称 / 上限全靠这里下发)。只写 BagItemInfo 的 6–13 号字段:
	//   所有物品:name / description / icon_key(来自 Item 表;缺配为空串,不编造);
	//   仅装备:  equip_level / equip_class / equip_class_name(职业表没有名称列或查不到时为空)、
	//            base_attrs(顺序 = 表里的基础属性顺序,tier = 0,seq 从 0 递增,cap = 0)、
	//            affixes(按 (tier, seq) 升序排好;value 按当前表上限夹取,cap = 当前表上限)。
	// 1–5 号字段(item_id / config_id / count / max_stack / equip_kind)由 PlayerBagSystem::BuildSnapshot
	// 自己填,这里不碰、也不清 info。
	// config 查不到表 → 什么都不写。表里查不到的属性 id / 规则层不接受的 effect、effect_param / 非法 tier
	// → 跳过该行并打 ERROR(与 CollectBonus 跳过的是同一批行:不计入加成的数值不上 tooltip)。
	// 纯读:只依赖 item 与配表,不看玩家(同一件装备谁看都一样)。
	static void FillItemDisplay(const ItemComp& item, ::BagItemInfo& info);

	// 往 layout.equip_slots 追加 EquipSlot 表里的槽位定义(含空槽),按 slot 升序、同槽号去重:
	// {slot = 行 id(即槽号), equip_kind, name(缺配为空串)}。两条过滤,都按数据判、不写死槽号:
	//   * **只下发槽号 < layout.capacity() 的行**(与 Bag::SlotsAcceptingKind 的「在容量内」同口径:
	//     客户端画出来的槽必须是真能穿的槽);
	//   * **name 为空的槽只在当前被占用时才下发**。没有部位名 = 不对玩家展示的槽(表里 0–2 号是单测
	//     夹具槽,name 留空),空着就不画;但身上穿着的每一件都必须有槽可画,否则它在客户端无处显示、
	//     也就卸不下来 —— 所以被占着的无名槽照常下发(name 为空,显示什么由客户端兜底)。
	//     这只管「下发给客户端的槽位定义」:无名槽照样能穿(SlotsAcceptingKind 不看 name)。
	// 所以调用方必须先填好 layout.capacity 与 layout.slots(占用关系从后者读)再调。
	// 只追加、不清 layout 的其它字段;纯读,不看玩家。调用方只在 bag_type == kEquipment 时调。
	static void FillEquipSlots(::BagLayoutInfo& layout);

	// 属性面板「战斗属性」区一行的显示信息。
	struct CombatStatDisplay {
		std::string name;
		bool percent{false};
		uint32_t sort{0};
	};

	// 反查 EquipAttribute 表里 effect == 5 且 effect_param == stat 的行,取它的 name / percent / sort。
	// 多行命中取 sort 最小的(再并列取 id 最小的);没有命中(含 stat 为 kNone / kCount)→ 返回
	// name 为空、percent = false、sort = 0,由调用方决定该项显示与否。纯读。
	static CombatStatDisplay DescribeCombatStat(equiprules::CombatStat stat);

	// 该 config 是不是装备:Item 表有此行且 equip_kind != 0。查不到表返回 false,不打日志(静默查询)。
	// 纯读;供本类各入口、表校验与单测共用,别处不要自己再写一遍 equip_kind 判断。
	static bool IsEquipment(uint32_t configId);
};

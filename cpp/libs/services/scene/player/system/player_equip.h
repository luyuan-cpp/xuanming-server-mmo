#pragma once

#include <cstdint>
#include <functional>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

#include "engine/core/type_define/type_define.h"  // Guid

#include "equip_attribute_rules.h"

class ItemComp;
class BagItemInfo;
class BagLayoutInfo;

// 装备系统(新装备掷随机属性 + 穿脱 + 强化 + 宝石镶嵌 / 取下 + 装备加成汇总 + tooltip 显示数据),设计文档
// docs/design/equipment-attributes.md §4.3(一期:随机属性 + 穿脱)与
// docs/design/equipment-enhance-inlay.md §4.3(二期:强化 + 镶嵌;下文「二期 §N」指它)。
//
// 分层(AGENTS §11.6):
//   * 容器层(modules/bag)不认识「属性」「强化」「宝石」,只搬整份实例;本类是唯一认识 EquipAttribute /
//     EquipAttributeCap / EquipAffixPool / EquipAffixRule / EquipEnhance / EquipGem / EquipGemSocket
//     七张表与 Item 表装备列的地方。
//   * 「怎么掷、怎么累加、怎么放大、孔位收不收」是纯规则,在 equip_attribute_rules.h(equiprules);
//     本类只负责把表行 / 实例翻译成规则的输入,并编排容器原语。
//   * 一件装备「是否穿着」= 它在不在 bags[kEquipment];实例上没有「已装备」标记。
//
// 不变量(1–4 见一期设计文档 §5;5–8 归纳自二期 §5,编号是本注释自己的):
//   1. 随机属性只在实例「新铸」时掷一次;还原、穿脱、邮件回流不重掷(ItemComp.has_equip() 守护)。
//   2. 上限、名称、基础属性不入库:显示与加成每次按表现查,改表立即对存量装备生效,不洗存档。
//   3. 装备加成不落库:本类不写 PlayerAttributeComp.bonus_values,也不写 BaseAttributesComp。
//   4. 穿脱是「同一 guid 换包」:不写获得 / 销毁流水,不过发放封禁闸。
//   5. 强化与宝石入库的只有「+几」和「哪个孔嵌了哪种宝石」(EquipInstanceData.enhance_level / gems),
//      数值每次按表现查:等级按表上限夹取,宝石按表现查是否生效。0 级、无宝石的装备与一期逐位一致。
//   6. 加成与 tooltip 同口径:基础属性(含下一级预览)只从同一个放大函数取,宝石属性只从同一个现查函数取;
//      不计入加成的宝石不显示属性。「孔位是否占用」是另一件事,只看实例上的原始记录。
//   7. 只有 has_equip() 的实例能被强化 / 镶嵌:绝不对未初始化的实例调 mutable_equip(),不对非装备写 equip 段。
//   8. 被扣的材料 / 宝石 config 必须不是装备(按 config 扣数量不看 has_equip(),会把装备实例当材料销毁):
//      表校验报、写入口挡,两道都在本类。
//
// 返回值:kSuccess(table/proto/tip/common_error_tip.pb.h)以外一律为 tip id —— 以
// table/proto/tip/equip_error_tip.pb.h 的 kEquip* 为主,个别情形透传 common / bag 域的码(各函数注释
// 里列明)。handler 直接搬进 TipInfoMessage。
//
// 写入口(Equip / Unequip / GmGrantItem / Enhance / InlayGem / RemoveGem)统一前置,与 PetSystem /
// PlayerAttributeSystem 的 CheckWritable 同口径:实体无效 → kEntityIsNull;跨 zone 冻结(PlayerFrozenComp)
// → kEquipFrozen;战斗在途(PlayerBattleSystem::IsInBattle)→ kEquipInBattle。快照已出之后换装 / 强化会让
// 局内数值与面板分叉;强化 / 镶嵌还是 scene 侧第一批玩家可触发的背包扣减入口,战斗闸必须在这一层(D48)。
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
	// EquipAffixRule / EquipEnhance / EquipGem / EquipGemSocket / AttributeDimension / AttributePool
	// (没加载的表按空表算,引用它的行会被报成问题;EquipAttribute 自己没加载时,下面「15 项各有显示行」
	// 会报满 15 条。二期三张表自己没加载 = 空表 = 强化未开放 / 没有宝石 / 没有孔,不计问题)。
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
	// 二期三张表(二期 §4.3「表校验」;下面每个分号隔开的条件恰好计一条):
	//   * EquipEnhance(逐行):id == 0 或 id > MaxEnhanceLevel()(不在从 1 起的连续等级里);
	//     success_rate 不在 1..10000;连续等级内 bonus_permyriad 小于上一级;cost_gold > INT64_MAX;
	//     material_id != 0 且 Item 行不存在(该行不再查后两项);material_id != 0、Item 行存在且是装备;
	//     material_id != 0、Item 行存在且 battle_usable != 0(强化石不能是战斗道具:战斗结算按 config 扣物,
	//     与强化扣材料会在重登窗口里交错);material_id != 0 且 material_count == 0;
	//     material_id == 0 且 material_count != 0。
	//   * EquipGem(逐行):Item 行不存在(该行不再查后两项);Item 行存在且是装备;Item 行存在且
	//     battle_usable != 0;gem_type == 0;属性行不存在;value == 0。属性行存在但规则层不接受它的 effect
	//     **不计**(EquipAttribute 段已经报过那一行,同一根因)。
	//   * EquipGemSocket:equip_kind == 0(逐行);socket >= equiprules::kMaxGemSockets(逐行);
	//     同部位内重复的 socket,每多出一行计一条;按部位把 socket 去重后不是 0..n-1,每个部位计一条。
	//     同一根因只计一次的两处取舍:equip_kind == 0 的行不属于任何部位,不参加后两项;越界的 socket 已逐行
	//     报过,它留下的缺口不另计 ——「不是 0..n-1」只在界内的缺口数多于越界行数时才报
	//     (否则把某个孔误填成 9 会被计两条)。
	// 校验只打日志不拒启、单表热更也不重跑,所以写入口对缺行与被配成装备的材料 / 宝石行另有 fail-closed
	// (见 Enhance / InlayGem / IsGem)。
	static uint32_t ValidateTables();

	// ---- 新铸实例 ----

	// 给一件**新铸**的物品实例掷随机属性(生产随机源 tlsRandom;单测可用 SetRandomSourceForTest 换掉)。
	// 它就是装进 BagService 的那个回调,所以受 ItemInstanceInitializer 的全部约束(item_system.h):
	// 不许失败、不许抛异常、不许在里面再入包、只许写 item.equip 段(item_id / config_id / size /
	// acquire_seq 一律不碰)。
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

	// ---- 写入口:强化 / 镶嵌 / 取下(二期 §4.3)----
	//
	// 三个入口共同的约定:
	//   * 对象 itemId 可以在人物背包(bags[kInventory])或装备栏(bags[kEquipment])里 —— 先找背包、后找
	//     装备栏;仓库 / 临时格里的不行。金币、材料、宝石的扣与退只认人物背包。
	//   * 定位装备的拒绝码(顺序同 Equip):找不到 → kEquipItemNotFound;config 查不到表 / 不是装备 /
	//     被配成了可堆叠 → kEquipNotEquipment;实例 size != 1 → kEquipItemNotFound。
	//   * 实例未初始化(!has_equip())是一期定义的合法状态(「它本来就没有」),不是故障:三个入口各自回一个
	//     业务码并打 WARN,绝不对它调 mutable_equip()。
	//   * 先校验后提交:下面「校验顺序」里的每一条都是纯校验,任何一条失败什么都没动;提交段排成
	//     「后面的步骤不会再失败」,它的失败分支只为留下可对账的 ERROR。
	//   * 提交段里凡是写实例之前都重新按 guid 取指针(ItemStore 的指针在 Insert / Erase / 整理之后失效)。
	//   * 改的是穿着的那件 → PlayerAttributeSystem::Recalculate(kEquipmentChanged)+ PushPanel:当前气血 /
	//     法力按比例保持(强化加血装不回血)。不推背包(由 handler 回两个包的全量)。

	// Enhance 的结果。概率失败也返回 kSuccess(金币与材料照扣,不是错误),靠 success 区分。
	struct EnhanceOutcome {
		bool success{false};  // 本次掷骰是否成功
		uint32_t level{0};    // 操作后的强化等级
	};

	// 强化:按 EquipEnhance 表第「有效等级 + 1」行掷一次成功率。成功等级 +1;失败掉 fail_level_loss 级
	// (最多掉到 0,表里默认 0 = 不掉)。**成败都扣**该行的金币与材料。
	//
	// 校验顺序(全部纯校验,在任何修改之前完成)与返回的 tip:
	//   统一前置(见类注释)                                    kEntityIsNull / kEquipFrozen / kEquipInBattle
	//   玩家没有 PlayerBagsComp                                 kEquipInternalError
	//   定位装备(见上)                                        kEquipItemNotFound / kEquipNotEquipment
	//   实例未初始化(打 WARN)                                 kEquipEnhanceUnavailable
	//   强化未开放(MaxEnhanceLevel() == 0)                     kEquipEnhanceUnavailable
	//   有效等级(存档值按表上限夹取)已到上限                   kEquipEnhanceMaxLevel
	//   下一级的行查不到(打 ERROR)                             kEquipInternalError
	//   下一级的行 success_rate == 0(必败的行不收费)           kEquipEnhanceUnavailable
	//   要材料,但材料的 Item 行不存在或是装备(打 ERROR)       kEquipInternalError
	//   要材料,人物背包里的数量不够                             kEquipEnhanceMaterialNotEnough
	//   要材料,只有算上带更新版本字段的堆才够(打 WARN)         kFeatureUnavailable(版本错位,可重试)
	//   要金币,余额不够(cost_gold > INT64_MAX 一律算不够)     kEquipEnhanceGoldNotEnough
	//   「要材料」= material_id != 0 且 material_count > 0;「要金币」= cost_gold > 0。
	//   材料按 Bag::CheckQuantities 判,只计 ItemStore::IsCountableByConfig 的堆,与提交段扣材料同一口径
	//   (混版本设计 §4.4 第 6 批):判完才扣金币,扣材料不会因版本错位失败,也就不会出现 ENHANCE_PARTIAL。
	// 提交顺序:掷骰(equiprules::RollEnhanceSuccess,恰好消耗一次 rand(10000);先掷后扣是为了让流水带上成败)
	//   → 扣金币(CurrencySystem::DeductCurrency,TX_EQUIP_ENHANCE,correlation = itemId)
	//   → 扣材料(BagService::RemoveItemsExact,TX_EQUIP_ENHANCE,correlation = itemId,
	//      extra = {"from":<强化前等级>,"success":true|false})
	//   → 重新按 guid 取实例,写 enhance_level → 回收材料抽空的堆(MergeAndCompact(kMergeOnly),放在写实例
	//      之后:「已扣」与「已写」之间不夹任何会销毁实例的步骤)→ 等级变了且穿着 → 重算属性并推面板。
	//   提交段任何一步失败都返回 kEquipInternalError 并打 ERROR:扣金币失败 = 什么都没动;扣材料失败带标签
	//   [PlayerEquip][ENHANCE_PARTIAL](金币已扣;没有货币退款原语,只留日志供人工对账);写等级时取不到实例
	//   带标签 [PlayerEquip][INSTANCE_LOST]。正常情况下这三个分支都走不到。
	//
	// outcome 在函数开头被重置;只有返回 kSuccess 时有意义。强化不是幂等操作:调用方不得自动重试。
	static uint32_t Enhance(entt::entity player, Guid itemId, EnhanceOutcome& outcome);

	// 镶嵌:消耗人物背包里 1 颗 gemConfigId(按 config 扣,不指定哪一堆),记到装备实例的 socket 号孔上。
	// 孔里已有宝石就拒绝,要先取下(不做「顶替并退回」)。
	//
	// 校验顺序(全部纯校验,在任何修改之前完成)与返回的 tip:
	//   统一前置(见类注释)                                    kEntityIsNull / kEquipFrozen / kEquipInBattle
	//   玩家没有 PlayerBagsComp                                 kEquipInternalError
	//   定位装备(见上)                                        kEquipItemNotFound / kEquipNotEquipment
	//   实例未初始化(打 WARN)                                 kEquipGemSocketInvalid
	//   gemConfigId 不是宝石(!IsGem)                          kEquipGemNotGem
	//   socket 不在该部位的孔位定义(EquipGemSocket 表)里       kEquipGemSocketInvalid
	//   实例上已有该 socket 的记录                              kEquipGemSocketOccupied
	//   宝石类型与孔位类型不符(孔位类型 0 = 任意)             kEquipGemTypeMismatch
	//   实例上的宝石记录已达 equiprules::kMaxGemSockets 条      kEquipInternalError(脏数据,打 ERROR)
	//   人物背包里没有这种宝石                                  kEquipGemNotOwned
	//   只有带更新版本字段的堆里有这种宝石(打 WARN)            kFeatureUnavailable(版本错位,可重试)
	//   「已有记录」看实例上的**全部**原始记录,不管那条记录此刻是否计入加成(equiprules::CheckGemFit 的
	//   判定顺序:孔不存在 → 已占用 → 类型不符);占着孔的是一条不生效的记录时另打一条 WARN。
	//   唯一的例外:gem_config_id == 0 的记录视同不存在(协议里 0 就是空孔,没有东西可退;当成占用的话
	//   这个孔对玩家永久作废)—— 镶嵌、取下、加成、tooltip 四处同口径。
	// 提交顺序:扣 1 颗宝石(BagService::RemoveItemsExact,TX_EQUIP_INLAY,correlation = itemId,
	//   extra = {"socket":<N>})→ 重新按 guid 取实例,追加 {socket, gemConfigId} → 回收抽空的堆
	//   → 装备穿着 → 重算属性并推面板。
	//   扣宝石失败 = 什么都没动,kEquipInternalError;写实例时取不到实例带标签 [PlayerEquip][GEM_LOST]
	//   (宝石已销毁;日志含 gem config),kEquipInternalError。
	static uint32_t InlayGem(entt::entity player, Guid itemId, uint32_t socket, uint32_t gemConfigId);

	// 取下:把 socket 号孔里的宝石原样退回人物背包(免费,退 1 颗同 config 的宝石物品),并从实例上抹掉。
	// **不要求**该孔仍在 EquipGemSocket 表里、也不要求宝石仍在 EquipGem 表里:策划删孔 / 删宝石行之后照样取得
	// 下来。宝石的 Item 行被删、或被配成了装备,则退不回去,宝石留在装备上(fail-closed)。
	//
	// 校验顺序(全部纯校验,在任何修改之前完成)与返回的 tip:
	//   统一前置(见类注释)                                    kEntityIsNull / kEquipFrozen / kEquipInBattle
	//   玩家没有 PlayerBagsComp                                 kEquipInternalError
	//   定位装备(见上)                                        kEquipItemNotFound / kEquipNotEquipment
	//   实例上没有该 socket 的记录(含未初始化的实例,后者打 WARN;gem_config_id == 0 的记录视同没有)  kEquipGemSocketEmpty
	//   宝石的 Item 行不存在,或被配成了装备(打 ERROR;后者退回去会铸出一件装备) kEquipInternalError
	//   人物背包没有空格、也没有哪一堆同种宝石还塞得下(静默预检,判据同容器层的并堆)  kEquipGemBagFull
	//   取的是一颗当前不计入加成的宝石时照常放行,提交成功之后另打一条 WARN。
	// 提交顺序(先发物、成功后才抹宝石:任何失败都不会凭空少宝石):
	//   BagService::AddItems(TX_EQUIP_GEM_REMOVE,correlation = itemId,extra = {"socket":<N>};过发放封禁闸、
	//   写获得流水、计入异常获得统计)—— 返回 kBagItemNotStacked / kBagAddItemBagFull → kEquipGemBagFull;
	//   其余失败(发放封禁、Item 行被删等)→ 打 ERROR,kEquipInternalError,宝石仍在装备上
	//   → 重新按 guid 取实例,删掉该 socket 的**全部**记录(退回的是第一条 gem_config_id != 0 的;脏数据里
	//   同孔的其余记录每删一条打一条 ERROR,它们被无偿清掉了)→ 装备穿着 → 重算属性并推面板。
	//   抹宝石时取不到实例带标签 [PlayerEquip][INSTANCE_LOST],kEquipInternalError(宝石已退回背包)。
	static uint32_t RemoveGem(entt::entity player, Guid itemId, uint32_t socket);

	// ---- 只读 ----

	// 汇总 player 当前穿着的全部装备的加成:只读 bags[kEquipment],每件 = Item.base_attr(基础属性,
	// 按这件的有效强化等级放大)+ 实例的 equip.affixes(随机属性)+ 实例的 equip.gems(当前生效的宝石),
	// 逐条经 equiprules::ApplyEffect 累加。
	//
	// 强化与宝石(二期 §4.3 消费侧)—— 入包边界整份信任 equip 段,所以这里每次现查、对脏数据静默跳过
	// (本函数与 FillItemDisplay 在每次重算 / 每次开包都跑,为一条脏记录打日志就是全服洪泛):
	//   * 有效强化等级 = min(实例.enhance_level, MaxEnhanceLevel());基础属性 = 表值按该级 bonus_permyriad
	//     放大(equiprules::ScaleByPermyriad;0 级严格恒等)。**只放大基础属性**,随机属性与宝石不放大。
	//   * 宝石只看实例上前 equiprules::kMaxGemSockets 条记录;同一 socket 只认第一条,这一条不合格则该孔
	//     整个不计(不回退到同孔的后一条)。合格 = EquipGem 表有该宝石、该 socket 在这件装备部位的孔位定义里、
	//     宝石的属性行存在且 effect 被规则层接受。不复核宝石类型与孔位类型(那是嵌入时的规则)。
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

	// 填一件物品的显示字段(客户端零配表,名称 / 上限 / 强化预览 / 孔位全靠这里下发)。只写 BagItemInfo 的
	// 6–20 号字段:
	//   所有物品:name / description / icon_key(来自 Item 表;缺配为空串,不编造);
	//   仅宝石物品(IsGem;不是装备):gem_type(18);属性行存在且被规则层接受时 gem_attr(19,只写
	//            attr_id / name / value / percent,tier / cap / seq 恒 0)。不是宝石的非装备与一期逐位一致;
	//   仅装备:  equip_level / equip_class / equip_class_name(职业表没有名称列或查不到时为空)、
	//            base_attrs(顺序 = 表里的基础属性顺序,tier = 0,seq 从 0 递增,cap = 0;value = 表值按有效
	//            强化等级放大后的值,enhance_bonus = value − 表值;带 enhance_next 时另写
	//            enhance_next_value = 表值按下一级放大后的值)、
	//            affixes(按 (tier, seq) 升序排好;value 按当前表上限夹取,cap = 当前表上限)、
	//            enhance_level(14,有效等级)/ enhance_max_level(15)/ enhance_bonus_permyriad(20)、
	//            enhance_next(16):仅当实例已初始化、有效等级 < 上限、且下一级的行存在并 success_rate > 0
	//            (与 Enhance 的判据相同)时写;材料名 / 图标取材料的 Item 行(查不到留空),不要材料时
	//            material_config_id / material_count 为 0、
	//            gem_sockets(17):仅实例已初始化时写。按该部位的孔位定义逐孔下发(含空孔),socket 升序、
	//            同号去重。**「占用」按原始记录**:实例上有该 socket 的记录(取第一条)→ gem_config_id =
	//            记录里的 config,gem_name / gem_icon_key 取宝石的 Item 行(查不到留空);**「生效」按
	//            CollectBonus 的口径**:计入加成的才带 attr。gem_config_id != 0 却不带 attr = 已镶但当前
	//            不生效(仍可取下)。孔位定义里没有的 socket 上的记录不下发(仍可用 RemoveGem 按孔位号取下)。
	// 1–5 号字段(item_id / config_id / count / max_stack / equip_kind)由 PlayerBagSystem::BuildSnapshot
	// 自己填,这里不碰、也不清 info。
	// config 查不到表 → 什么都不写。表里查不到的属性 id / 规则层不接受的 effect、effect_param / 非法 tier
	// → 跳过该行并打 ERROR(与 CollectBonus 跳过的是同一批行:不计入加成的数值不上 tooltip)。
	// 强化 / 宝石的脏数据(等级超上限、宝石行或孔位定义不在表里)一律静默处理,不打日志(同 CollectBonus)。
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

	// 强化上限:EquipEnhance 表里从 1 起**连续**的最大 id(第 N 行 = 「从 +(N−1) 强化到 +N」)。
	// 表为空、或没有 id = 1 的行 → 0 = 强化未开放。中间缺一行则上限停在缺口之前(缺口之后的行由表校验报)。
	// 纯读、静默;每次现算(几次哈希查找),不缓存 —— 改表 / 热更立即生效,存量装备的等级按它夹取。
	static uint32_t MaxEnhanceLevel();

	// 该 config 是不是宝石:EquipGem 表有这一行,且它的 Item 行存在、不是装备(equip_kind == 0)。
	// 后一半不能省:镶嵌 / 取下按 config 扣发数量,指向装备的「宝石」嵌进去吃掉的是一件装备实例,取下来
	// 铸出的是一件重新掷属性的新装备。纯读、静默;InlayGem 与 FillItemDisplay 用的是同一个判据。
	static bool IsGem(uint32_t configId);

	// ---- 仅供单测 ----

	// 替换本文件全部掷骰(RollNewInstance 的掷属性与 Enhance 的强化判定)用的随机源。rand(n) 返回 [0, n);
	// 传空恢复生产随机源(tlsRandom)。
	// 进程级静态、无锁:只许在测试的 SetUp / TearDown 里调(同 BagService::SetItemInstanceInitializer 的
	// 约定),用完必须复原。没有它,编排层的强化失败分支(照扣、掉级)只能靠概率撞,写不出确定的用例。
	static void SetRandomSourceForTest(std::function<uint64_t(uint64_t)> rand);
};

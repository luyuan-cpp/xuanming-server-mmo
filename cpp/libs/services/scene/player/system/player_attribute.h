#pragma once

#include <cstdint>
#include <map>
#include <string>

#include <entt/src/entt/entity/entity.hpp>

#include "player_level_rules.h"

class AttributePanelInfo;

// 角色属性加点系统(属性点池;2026-09-14 删相性点 / 仙魔点池),设计文档
// docs/design/player-attribute-allocation.md。
//
// 唯一入口不变量:PlayerAttributeComp 的任何写入只经本类静态函数;
// 每个写操作成功后都 Recalculate(重算 DerivedAttributesComp)。
// 返回值:0 / kSuccess 语义以外一律为 tip id(table/proto/tip/attribute_error_tip.pb.h),
// handler 直接搬进 TipInfoMessage。
//
// 跨 zone 冻结(PlayerFrozenComp)期间所有写操作拒绝(cross-zone-readiness-audit.md §11.1);
// 战斗在途(InBattleComp)期间同样拒绝:快照已出,改属性会让局内数值与面板分叉。
class PlayerAttributeSystem {
public:
	// 从 DB 加载 / 首登后调用:补默认方案、清理不存在的维度、重算二级属性。
	static void InitializeOnLoad(entt::entity player);

	// 重算的触发原因:决定当前 HP/MP 怎么跟随上限变化(评审 2026-09-04 堵住"降后再升"白嫖回血):
	//   kLevelChanged  升级:上限抬高按绝对增量补当前值(升级手感);降级只夹。
	//   其它(加载/加点/切方案/洗点/换装):按比例保持 hp = hp × newMax / oldMax,一升一降往返不净得
	//                 (极低血量因"活着至少留 1"最多回升到 oldMax/newMax,有上界且不累积),
	//                 切方案 / 洗点 / 重新加点都不能成为回血手段(turn-based-battle-server.md D4 残血带出战斗)。
	//   kEquipmentChanged  穿上 / 卸下装备(PlayerEquipSystem::Equip / Unequip 成功后):与加点同走「按比例保持」,
	//                 **不是**补增量 —— 否则脱下加血装再穿回就是一次免费回血(equipment-attributes.md §4.4、
	//                 §5 不变量 4)。也不触发下面的「已分配 > 总量」收敛:换装不改等级、不改总点数,
	//                 那个收敛只属于加载与等级变化。
	// 只许在末尾追加:player_event_handler / player_equip 等调用方按名字引用这些值。
	enum class RecalcReason : uint8_t { kLoad, kLevelChanged, kAllocate, kSchemeSwitch, kReset, kEquipmentChanged };

	// 重算二级属性(DerivedAttributesComp + BaseAttributesComp.speed / armor(职业初值)),按 reason 处理当前 HP/MP;不落库。
	// kLoad / kLevelChanged 还会先做"已分配 > 总量"的收敛(降级 / 改表后整池清零返还),防止低等级号带着高等级面板。
	//
	// 装备加成(equipment-attributes.md §4.4)每次都在这里从装备栏现算(PlayerEquipSystem::CollectBonus,纯读),
	// 所以任何触发原因的重算都自带装备,不存在「某条路径忘了算装备」:
	//   * 一级属性点(单项 + 所有属性)并进自然部分,按每点固定系数换算 —— 不进加点公式的 n,也不进
	//     「85 级标准基础属性」(否则同样的加点收益会随装备浮动);
	//   * 二级属性平加(气血 / 法力 / 物伤 / 法伤 / 速度 / 防御)在向下取整之前加进六项;
	//   * 战斗类属性(必杀 / 连击 / 抗性 … 15 项)整块覆盖写进 DerivedAttributesComp.combat。
	// 装备数值不写 PlayerAttributeComp(bonus_values 是落库字段,留给丹药等),也不写 BaseAttributesComp 的
	// 成长字段。唯一的例外是既有的 speed 镜像:BaseAttributesComp.speed = 重算出的速度(含装备),它每次加载
	// 都被这里覆盖、从不作为输入,落库的那份只是缓存。
	// 调用约定:装备栏的内容一变就必须调一次(reason = kEquipmentChanged),否则二级属性停在换装之前。
	static void Recalculate(entt::entity player, RecalcReason reason = RecalcReason::kLoad);

	// 面板全量(客户端零配表,维度名/说明/上限/剩余点都在这里)。
	// 一级属性的 value 含装备点,bonus = bonus_values + 装备点(已含在 value 内);combat = 战斗类属性的终值
	// (必杀率含角色基础暴击率,抗物理 / 抗法术含基础抗性;百分比项夹到 100),按 sort 升序(sort 相同按 combat_id)。
	// 取数口径:输入(等级 / 已分配 / 装备点)现读,产出(六项二级属性与战斗类加成)读上一次 Recalculate 的结果。
	// combat 是**显示值**,有三处与实战取值不是一回事(都只影响面板,DerivedAttributesComp.combat 里的加成原值不动):
	//   * 百分比项夹到 100:对必杀率,这与回合引擎的夹取一致;
	//   * 抗物理 / 抗法术显示的是「基础抗性 + 加成」,**不反映**它与护甲 / 防御共用的 60% 常驻减伤封顶
	//     (combat_damage_rules.h 的 kMaxPassiveReduction)—— 防御已堆满的角色,实际生效的减伤低于这里的数;
	//   * EquipAttribute 表里没有显示行(effect = 5 且 effect_param = 该项)的项不下发:没有名字,客户端无从显示。
	//     表完整时恒为 15 项。缺行不在这里打日志(每次开面板都会刷一遍);它由启动时的装备表校验
	//     (PlayerEquipSystem::ValidateTables 的「战斗属性 1..15 各有显示行」)报 ERROR。
	static void BuildPanel(entt::entity player, AttributePanelInfo& panel);

	// 确认加点:target 为该池"目标已分配"(全量、幂等;缺省维度视为不变)
	static uint32_t Allocate(entt::entity player, uint32_t poolId,
							 const std::map<uint32_t, uint32_t>& target);

	// 重置(洗点):清空当前方案里该池全部分配;按 AttributePool 表扣金币
	static uint32_t Reset(entt::entity player, uint32_t poolId);

	// 自动加点:按 AttributeAutoPlan(职业 → 通用兜底)算建议"目标已分配",只算不落
	static uint32_t AutoAllocate(entt::entity player, uint32_t poolId,
								 std::map<uint32_t, uint32_t>& suggested);

	static uint32_t CreateScheme(entt::entity player, const std::string& name, uint32_t& schemeId);
	static uint32_t SwitchScheme(entt::entity player, uint32_t schemeId);
	static uint32_t RenameScheme(entt::entity player, uint32_t schemeId, const std::string& name);

	// GM:设等级(1..kMaxLevel),触发 PlayerUpgradeEvent → 重算 + 推面板
	static uint32_t GmSetLevel(entt::entity player, uint32_t level);

	// 主动推面板(升级 / GM / 外部加成变化)
	static void PushPanel(entt::entity player);

	// 该维度是不是「角色池」的维度:AttributeDimension 里有这一行,且它所属的池 AttributePool.owner_type == 0
	// (0 = 角色,1 = 宝宝)。维度不存在、或它的池查不到 → false。
	// 「哪些维度算角色的」只有这一个出处:Recalculate / BuildPanel 只对它答 true 的维度取点(含装备的一级属性点),
	// PlayerEquipSystem::ValidateTables 用它核对「一级属性点」类装备属性指向的维度 —— 别处不要再抄一份
	// owner_type 判断,两份迟早一个改了一个没改。
	// 纯读、不打日志、不看玩家。AttributeDimension / AttributePool 两张表没加载时按「不存在」答 false。
	static bool IsPlayerDimension(uint32_t dimensionId);

	// 角色等级上限:唯一真相在 player_level_rules.h(playerlevel::kMaxLevel),这里只是别名
	static constexpr uint32_t kMaxLevel = playerlevel::kMaxLevel;
};

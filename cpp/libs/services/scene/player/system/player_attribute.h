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
//
// 组件归属:PlayerAttributeComp / BaseAttributesComp / LevelComp / PlayerUint32Comp / PlayerBagsComp
// 由加载(PlayerDatabaseMessageFieldsUnmarshal)挂上,DerivedAttributesComp 由 InitializeOnLoad 挂上;
// 面板、重算、写入口与 AutoAllocate 统一要求这六项就绪,只取不建。缺必需组件是服务端故障:
// 写操作、AutoAllocate 与 BuildPanel 返回 kServiceUnavailable(写操作在任何扣费 / 改动之前返回),
// Recalculate 跳过,PushPanel 不推;都记 ERROR。错误码口径与 player_mission.cpp / skill.cpp 一致:
// 实体无效 → kEntityIsNull(本类既有口径),实体有效但缺必需组件 → kServiceUnavailable。
class PlayerAttributeSystem {
public:
	// 从 DB 加载 / 首登后调用(PlayerAttributeComp 已由 Unmarshal 挂上):补默认方案、
	// 挂上全新的 DerivedAttributesComp 并重算二级属性。存档组件缺失时只记错误,不补建。
	// 已分配点原样携带,不清理、不返还:本版本表里查不到 / 归不到角色的维度是未知键(不生效、计入已用),
	// 超额的池整池按 0 生效(加点统一规则,attribute_allocation_rules.h;
	// docs/design/mixed-version-persist-field-protection.md §12.1 D1 / D2)。
	static void InitializeOnLoad(entt::entity player);

	// 重算的触发原因:决定当前 HP/MP 怎么跟随上限变化(评审 2026-09-04 堵住"降后再升"白嫖回血):
	//   kLevelChanged  升级:上限抬高按绝对增量补当前值(升级手感);降级只夹。
	//   其它(加载/加点/切方案/洗点/换装):按比例保持 hp = hp × newMax / oldMax,一升一降往返不净得
	//                 (极低血量因"活着至少留 1"最多回升到 oldMax/newMax,有上界且不累积),
	//                 切方案 / 洗点 / 重新加点都不能成为回血手段(turn-based-battle-server.md D4 残血带出战斗)。
	//   kEquipmentChanged  穿上 / 卸下装备(PlayerEquipSystem::Equip / Unequip 成功后):与加点同走「按比例保持」,
	//                 **不是**补增量 —— 否则脱下加血装再穿回就是一次免费回血(equipment-attributes.md §4.4、
	//                 §5 不变量 4)。
	//   kSkillChanged  学习 / 修习了被动职业技能(PlayerSkillSystem::UpgradeSkill):与换装同走「按比例保持」
	//                 (docs/design/class-skills.md §7)。
	// reason 只决定 HP/MP 怎么跟随上限,不决定加点怎么处理:任何原因的重算都不改写已分配点(见 Recalculate)。
	// 只许在末尾追加:player_event_handler / player_equip 等调用方按名字引用这些值。
	enum class RecalcReason : uint8_t { kLoad, kLevelChanged, kAllocate, kSchemeSwitch, kReset, kEquipmentChanged, kSkillChanged };

	// 重算二级属性(DerivedAttributesComp + BaseAttributesComp.speed / armor(职业初值)),按 reason 处理当前 HP/MP;不落库。
	// 只读 PlayerAttributeComp,不改写、不返还已分配点(以前 kLoad / kLevelChanged 会把超额的池整池清零返还,
	// 回滚 / 降级一次就删数据,mixed-version-persist-field-protection.md §12.1 D2)。现在按加点统一规则
	// (attribute_allocation_rules.h)只决定生效值:超过本版本总量或单项上限的池整池按 0 换算,本版本归不到角色的
	// 未知键永不生效 —— 低等级号照样带不着高等级面板进战斗快照,而等级回来了同一份加点原样生效。
	// kLoad 时对不生效的池 / 带未知键的方案各打一条 WARN(metric=allocation_not_effective),不写数据。
	//
	// 装备加成(equipment-attributes.md §4.4)每次都在这里从装备栏现算(PlayerEquipSystem::CollectBonus,纯读),
	// 所以任何触发原因的重算都自带装备,不存在「某条路径忘了算装备」:
	//   * 一级属性点(单项 + 所有属性)并进自然部分,按每点固定系数换算 —— 不进加点公式的 n,也不进
	//     「85 级标准基础属性」(否则同样的加点收益会随装备浮动);
	//   * 二级属性平加(气血 / 法力 / 物伤 / 法伤 / 速度 / 防御)在向下取整之前加进六项;
	//   * 战斗类属性(必杀 / 连击 / 抗性 … 15 项)整块覆盖写进 DerivedAttributesComp.combat。
	// 职业技能被动(docs/design/class-skills.md §7)同样每次现算(PlayerSkillSystem::CollectPassiveBonus,纯读):
	//   六项二级属性在装备平加之后按比例放大;暴击被动的百分点加到 combat 的物理 / 法术必杀率上。
	//   scene 是这几项被动唯一的消费者,战斗快照带的已经是放大后的值,引擎不再叠加。
	// 装备数值不写 PlayerAttributeComp(bonus_values 是落库字段,留给丹药等),也不写 BaseAttributesComp 的
	// 成长字段。唯一的例外是既有的 speed 镜像:BaseAttributesComp.speed = 重算出的速度(含装备),它每次加载
	// 都被这里覆盖、从不作为输入,落库的那份只是缓存。
	// 调用约定:装备栏的内容一变,**或穿着的装备的实例数据一变**(强化等级、镶嵌 / 取下宝石,见
	// docs/design/equipment-enhance-inlay.md §4.3),就必须调一次(reason = kEquipmentChanged),否则二级属性
	// 停在变化之前。
	static void Recalculate(entt::entity player, RecalcReason reason = RecalcReason::kLoad);

	// 面板全量(客户端零配表,维度名/说明/上限/剩余点都在这里)。
	// 一级属性的 allocated 下发存储值;value 只算生效的已分配(所属池不生效时按 0),剩余点与洗点费用按同一个
	// 生效判定给出(不生效的池剩余 0、洗点免费)。
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
	// 返回值:kSuccess;实体无效 kEntityIsNull;缺上述任一持久 / 派生输入 kServiceUnavailable。
	// 非 kSuccess 时 panel 是清空的 —— 调用方必须回错误码,不能把空面板当成功发给客户端
	// (客户端按整体覆盖处理面板,空面板等于把真实数据抹掉)。只读,不改变实体的组件集合。
	static uint32_t BuildPanel(entt::entity player, AttributePanelInfo& panel);

	// 确认加点:target 为该池"目标已分配"(全量、幂等;缺省维度视为不变)。目标只能是本池已知维度;
	// 只写真正变化的键(不落 0 值新键)。剩余点扣掉未知键,池不生效时剩余为 0(回「点数不足」)。
	static uint32_t Allocate(entt::entity player, uint32_t poolId,
							 const std::map<uint32_t, uint32_t>& target);

	// 重置(洗点):清空当前方案里该池的已知维度,未知键原样保留;按 AttributePool 表扣金币。
	// 本池已知维度一点没加(含只剩未知键)→ kAttributeNothingToChange 且不扣费;池不生效 → 免费并打 WARN
	// (metric=allocation_reset_free_ineffective)。费用与面板 reset_cost_gold 同一口径。
	static uint32_t Reset(entt::entity player, uint32_t poolId);

	// 自动加点:按 AttributeAutoPlan(职业 → 通用兜底)算建议"目标已分配",只算不落
	static uint32_t AutoAllocate(entt::entity player, uint32_t poolId,
								 std::map<uint32_t, uint32_t>& suggested);

	static uint32_t CreateScheme(entt::entity player, const std::string& name, uint32_t& schemeId);
	static uint32_t SwitchScheme(entt::entity player, uint32_t schemeId);
	static uint32_t RenameScheme(entt::entity player, uint32_t schemeId, const std::string& name);

	// GM:设等级(1..kMaxLevel,绝对设值),直接重算并推面板与宠物列表。降级不清加点:超额的池按 0 生效,
	// 设回原等级后原样生效。存储等级原本超上限时会被覆写,打 WARN 留痕(GM 只在 dev/test 开放)。
	static uint32_t GmSetLevel(entt::entity player, uint32_t level);

	// 主动推面板(升级 / GM / 外部加成变化);BuildPanel 失败时不推。
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

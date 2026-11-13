#pragma once
// 手写头(player_data_loader.h 是生成器产物,重生成会覆盖,不能在里面加声明)。
//
// 玩家基础属性的「初始化 / 阵亡复活」纯规则:
//   1) 全新号(建角只写账号级 class_id,不建完整属性数据)→ BaseAttributesComp 全 0,
//      按 ClassTable 的 init_* 赋全套初始属性,给世界与战斗一致的起点;
//   2) 已初始化但阵亡的号(health=0、其余属性在)→ 只回满 HP/MP,不动成长属性
//      (基础复活:玩家不会永久卡在 0 血;产品完整死亡/复活流程未定前的基线);
//   3) 活着(health>0)→ 一律不动 —— 残血带出战斗(设计 D4)靠这条保住。
//
// 「回满」回到哪里:优先用调用方传进来的 maxHealth/maxMana(即属性加点系统算出的
// DerivedAttributesComp 上限);传 0 表示上限暂不可知,退回 ClassTable 的 1 级初值。
// 这个参数不是可选装饰:20 级玩家 max_health 约 1100,只回 500 等于把等级成长吞掉,
// 而登录路径的 Recalculate 只向下夹不向上补(oldMaxHealth 每次登录都是 0),补不回来。
//
// 规则本身不碰 ECS 也不查表管理器(取哪一行、上限多少都由调用方决定),所以能在不链接
// scene.lib 的测试工程里直接验证:cpp/tests/turn_battle_engine_test/player_revive_rule_test.cpp。
// 取哪一行:class_id 目前未随 PlayerAllData 下发到 scene,各职业初值相同,取首行即可
// (与 player_attribute.cpp 的 ResolveClassRow 同一口径);待 class_id 打通后按 class 取行。
//
// 存档里只有当前血蓝(2026-11-12):力量、护甲、抗性、暴击、速度只有职业表这一个来源,存盘时清掉
// (PlayerDatabaseMessageFieldsMarshal),每次加载由 ApplyClassConstants 按职业表写回。所以从存档读出来的组件里
// 力量、速度恒为 0,规则 1) 的「全新号」判定在加载路径上等价于「血量为 0」:新号和阵亡后下线的号走同一支,
// 两者要做的事本来就一样(回满血蓝)。规则 2) 现在只在运行时(战斗结算把人打到 0 血)才走得到。

#include "proto/common/component/actor_comp.pb.h"
#include "table/proto/class_table.pb.h"

// 规则对本次调用做了什么(调用方据此打日志、决定要不要按真实上限回满;测试据此断言分支)。
enum class PlayerReviveOutcome {
	kUntouched,    // 活着,一个字段都没动
	kInitialized,  // 全新号:全套初始属性 + 回满 HP/MP
	kRevived,      // 阵亡:仅回满 HP/MP,成长属性保持
};

// 判定「全新号」:health/strength/speed 三项同时为 0。
// 只有 health=0 而成长属性还在 → 是阵亡,不是新号(否则复活会把成长属性重置回初值)。
inline bool IsUninitializedBaseAttributes(const BaseAttributesComp& attrs) {
	return attrs.health() == 0 && attrs.strength() == 0 && attrs.speed() == 0;
}

// 职业常量:力量、护甲、抗性、暴击取职业表,速度取职业表的起点(随后由 PlayerAttributeSystem::Recalculate 覆盖成
// 算上加点和装备的值)。不碰当前血蓝。这些字段不落库,每次加载都调一次,所以改职业表对已经建好的角色同样生效。
// 要做「每个角色各自成长」的基础值,把输入(已分配点、外部加成)放进 PlayerAttributeComp、在重算里产出;
// 直接改这几个字段的话,下次加载就被职业表盖掉。
inline void ApplyClassConstants(BaseAttributesComp& attrs, const ClassTable& cls) {
	attrs.set_strength(cls.init_strength());
	attrs.set_armor(cls.init_armor());
	attrs.set_resistance(cls.init_resistance());
	attrs.set_critchance(cls.init_critchance());
	attrs.set_speed(cls.init_speed());
}

// maxHealth / maxMana 传 0 = 上限未知,退回 ClassTable 初值。
inline PlayerReviveOutcome ApplyClassInitialAttributesOrRevive(
	BaseAttributesComp& attrs, const ClassTable& cls,
	uint64_t maxHealth = 0, uint64_t maxMana = 0) {
	if (attrs.health() != 0) {
		return PlayerReviveOutcome::kUntouched;  // 活着(含残血)一律不动
	}
	const bool uninitialized = IsUninitializedBaseAttributes(attrs);
	if (uninitialized) {
		ApplyClassConstants(attrs, cls);
	}
	// 初始化与复活都回满:有真实上限就回到真实上限,否则退回职业初值
	attrs.set_health(maxHealth > 0 ? maxHealth : cls.init_health());
	attrs.set_mana(maxMana > 0 ? maxMana : cls.init_mana());
	return uninitialized ? PlayerReviveOutcome::kInitialized : PlayerReviveOutcome::kRevived;
}

// 阵亡基础复活(ECS 侧入口,定义在 player_database_loader.cpp:取 ClassTable 行后套用上面的规则)。
// 注:同名的 player_data_loader.h 才是生成器产物;loader 的 .cpp 生成器只写到 tools/generated/temp/,
// 源码树里那份 player_database_loader.cpp 是手写文件,regen 不会覆盖它。
// health==0 时回满 HP/MP,否则不动;maxHealth/maxMana 由调用方从 DerivedAttributesComp 取,
// 取不到传 0 退回职业初值。回合制战斗结算把玩家打到 0 血后调用,否则 0 血玩家可以再次排队、
// 被快照进新局、引擎开局即判负(2026-09-02 跨 zone 冒烟实测)。
void ReviveBaseAttributesIfDead(BaseAttributesComp& attrs, uint64_t maxHealth = 0, uint64_t maxMana = 0);

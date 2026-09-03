#pragma once

// 战斗配表指纹(设计文档 docs/design/cross-zone-matchmaking.md §10)。
//
// 目的:回合引擎是确定性的,但确定性的前提是参战各方(出快照的 scene 节点、
// 开局的 battle 节点)读的是同一份战斗配表。跨 zone 匹配时不同 zone 可能滚动
// 发布到不同表版本,这里把六张战斗表的解析结果做成一个短指纹随快照携带,
// match 比对全员一致、battle 开局时再与自身比对(不一致按配置 warn/enforce)。
//
// 算法:对六张已加载的 proto 表对象(Skill/Buff/Cooldown/SkillPermission/Dungeon/Monster)
// 逐张做确定性序列化(CodedOutputStream::SetSerializationDeterministic(true),
// map 字段按 key 排序),按固定表序拼接(每段前缀表名 + 段长,防止"表 A 尾巴 +
// 表 B 头部"错位碰撞),整体 sha256 后取 hex 前 32 位(16 字节)。
// 只看表内容,不看文件路径/json 与 pb 二进制格式/加载顺序:同一份数据无论从
// skill.json 还是 skill.pb 加载,指纹相同。
//
// 缓存:Current() 进程内只算一次(六张表全量序列化,scene 每次备战都算不划算)。
// 注意:generated/table/code/all_table.h 的 ReloadTables() 目前全仓无人调用;
// 若日后接线热重载,必须在重载完成回调里调用 Refresh(),否则指纹与内存表脱节。

#include <cstddef>
#include <string>

class SkillTableData;
class BuffTableData;
class CooldownTableData;
class SkillPermissionTableData;
class DungeonTableData;
class MonsterTableData;

namespace turnbattle
{

	class BattleTableFingerprint
	{
	public:
		// 指纹长度:sha256 hex(64 位)的前 32 位 = 16 字节熵,碰撞概率对"版本判别"足够
		static constexpr size_t kHexLength = 32;

		// 从全局表管理器的当前快照计算(不缓存)。必须在表加载完成后调用,
		// 表未加载时六张表都是空对象,算出的是"空表指纹"。
		static std::string Compute();

		// 对给定的六个表对象计算(纯函数,单测直接构造表对象验证)。
		static std::string ComputeFrom(const SkillTableData& skill,
									   const BuffTableData& buff,
									   const CooldownTableData& cooldown,
									   const SkillPermissionTableData& skillPermission,
									   const DungeonTableData& dungeon,
									   const MonsterTableData& monster);

		// 进程内缓存值:首次调用时 Compute() 并缓存。调用时机必须晚于表加载完成
		// (scene/battle 的 TableLoadHandler::OnLoaded 里主动 Refresh 一次即可保证)。
		static const std::string& Current();

		// 重算并覆盖缓存(表加载完成 / 热重载后调用),返回新值。
		static const std::string& Refresh();
	};

} // namespace turnbattle

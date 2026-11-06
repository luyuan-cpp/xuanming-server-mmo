#pragma once

#include <cstdint>
#include <string>

// 玩家持久数据消息 player_database 顶层的未知字段,以及加载时的覆盖世代
// (docs/design/mixed-version-persist-field-protection.md §3.3、§4.2 L1)。
// 2026-11-05 之前还有对应 player_database_1 的 PlayerDatabase1CarryComp,随那个消息删除
// (docs/design/player-save-direct-commit.md §1.2)。
//
// 持久数据类(docs/design/ecs-component-access-rules.md):只在手写的 PlayerDatabaseMessageFieldsUnmarshal 里创建,
// 之后不替换、不摘除;存盘入口缺它就整份拒存,不补默认值 —— 补一个空组件等于把新版本写进来的整列抹掉。
//
// 顶层未知字段是旧版本完全没有对应逻辑的整列,旧代码不会删除或重置它们,所以原样带回不会出现
// 「旧对象的新字段复活到同键新对象上」的问题(设计 §4.1)。按 ID 引用其他实体的新列另由 R5 约束。
struct PlayerDatabaseCarryComp
{
	// unknown_field_carry::Capture 的字节;全网同版本时为空,不占内存。
	std::string topLevelUnknown;
	// 加载时本表 persist_schema_gen 的原值;0 = 未盖戳(机制上线前写入或新号种子),比较时按 1 处理。
	uint32_t loadedSchemaGen{0};
};

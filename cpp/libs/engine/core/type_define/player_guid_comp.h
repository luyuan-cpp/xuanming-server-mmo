#pragma once

#include "type_define.h"

// 玩家实体的业务身份,由登录加载创建,通过存盘消息里的 player_id 还原。
// player_id 只由发号器发,只当不透明 ID 使用,不得由字段名推断位布局(AGENTS.md §7 第 1 条)。
// Guid 仍是业务 ID 类型别名;组件用具名类型隔离,避免与其他 uint64_t 共用 ECS 池。
struct PlayerGuidComp
{
    Guid value{};
};

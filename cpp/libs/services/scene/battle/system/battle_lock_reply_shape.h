#pragma once

// zone Redis 应答 → battle_lock::ReplyShape 的唯一适配(scene 登录钩子读战斗锁用)。
//
// 单独成头是为了让 battle_lock_rules.h 保持零依赖:判定函数只认 ReplyShape,单测不用拼 redisReply 也能盯住;
// 这里是唯一碰 hiredis 类型的地方,空应答 → kNoReply 这一条映射也由单测直接盯住
// (cpp/tests/routing_identity_test,BattleLockReplyShape.*)。

#include <hiredis/hiredis.h>

#include "battle_lock_rules.h"

namespace battle_lock
{
	inline ReplyShape ReplyShapeOf(const redisReply *reply)
	{
		if (reply == nullptr)
		{
			return ReplyShape::kNoReply;
		}
		switch (reply->type)
		{
		case REDIS_REPLY_NIL:
			return ReplyShape::kNil;
		case REDIS_REPLY_STRING:
			return ReplyShape::kString;
		case REDIS_REPLY_STATUS:
			return ReplyShape::kStatus;
		case REDIS_REPLY_INTEGER:
			return ReplyShape::kInteger;
		case REDIS_REPLY_ARRAY:
			return ReplyShape::kArray;
		case REDIS_REPLY_ERROR:
			return ReplyShape::kError;
		default:
			return ReplyShape::kOther;
		}
	}
} // namespace battle_lock

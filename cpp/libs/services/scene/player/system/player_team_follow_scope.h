#pragma once

#include <cstdint>
#include <functional>
#include <memory>
#include <string_view>

#include "entt/src/entt/entity/entity.hpp"

struct redisReply;
namespace muduo::net { class EventLoop; }

// Scene 主 loop 持有。构造、启动、停止和析构均在该 loop，每个 loop 只绑定一个作用域。
class PlayerTeamFollowScope final
{
public:
	using RedisReplyHandler = std::function<void(const redisReply*)>;
	// 提交及回调均在所属 loop，回调可在提交内重入。返回 REDIS_OK 才承诺一次回调。
	// 键只借用到提交返回，回复只借用到回调返回。
	using RedisGet = std::function<int(std::string_view, RedisReplyHandler)>;

	explicit PlayerTeamFollowScope(muduo::net::EventLoop& loop);
	PlayerTeamFollowScope(muduo::net::EventLoop& loop, std::function<bool()> redisReady, RedisGet get);
	~PlayerTeamFollowScope();
	PlayerTeamFollowScope(const PlayerTeamFollowScope&) = delete;
	PlayerTeamFollowScope& operator=(const PlayerTeamFollowScope&) = delete;

	void Stop();

private:
	friend class PlayerTeamSystem;
	static void Start(entt::entity player, uint64_t playerId, uint64_t leaderId);

	struct State;
	static thread_local std::weak_ptr<State> current_;
	std::shared_ptr<State> state_;
};

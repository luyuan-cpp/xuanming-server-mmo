#pragma once

#include <algorithm>
#include <chrono>

// 世界频道推送的每台 gate 转发预算(docs/design/chat-world-channel-fanout.md §4)。
//
// 推送开销是「世界消息数 × 本机在线人数」:全服聊天量一涨(刷屏、脚本),不设上限每台 gate 的下行就按平方增长。预算按 gate 计、
// 不按会话计:本机所有会话收的是同一条广播流,超出预算的那几条对本机所有会话一起不推,每台 gate 的推送开销只与本机在线人数成正比。
// 不推的消息仍在 chat 的历史里,客户端打开面板、切频道或重连时拉得到。
//
// 每秒 kPerSecond 条、最多连发 kBurst 条,是一个人读得过来的量级;全服的发送量另由 chat 的世界频道发言间隔约束(设计稿 §5),
// 两边一起保证推送量有上界。
//
// 算法是 GCRA(通用信元速率算法,与令牌桶等价):只记「理论到达时刻」tat,每推一条往后挪一个标准间隔;tat 领先当前时刻超过
// (kBurst - 1) 个间隔就拒绝。全用整数时长,没有浮点累计误差;拒绝不改状态。
//
// 线程:只在 gate 主循环上用(Kafka 回调经 queueInLoop 投递到主循环,KafkaManager::StartBackgroundPolling),不加锁。
// 时间由调用方传入单调时钟读数,测试注入;读数倒退(调用方传错)时只会多拒绝,不会多放行。
class WorldChatPushBudget
{
public:
	static constexpr int kPerSecond = 10;
	static constexpr int kBurst = 20;
	// 相邻两条之间的标准间隔。
	static constexpr std::chrono::steady_clock::duration kInterval =
		std::chrono::duration_cast<std::chrono::steady_clock::duration>(std::chrono::seconds(1)) / kPerSecond;

	// 取一个推送名额。取不到返回 false:这一条对本机全部会话一起不推。
	bool TryTake(std::chrono::steady_clock::time_point now)
	{
		const std::chrono::steady_clock::time_point tat = std::max(tat_, now);
		if (tat - now > kInterval * (kBurst - 1))
		{
			return false;
		}
		tat_ = tat + kInterval;
		return true;
	}

private:
	// 理论到达时刻。起步是时钟零点,第一条一定放行,并且立刻有一个完整的突发可用。
	std::chrono::steady_clock::time_point tat_{};
};

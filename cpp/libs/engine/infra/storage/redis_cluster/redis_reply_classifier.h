#pragma once

// 应答归类(docs/design/redis-player-store-cluster.md §5.6),纯逻辑。
//
// 两类错误应答意味着命令**没有执行**:重定向(MOVED / ASK / TRYAGAIN / CLUSTERDOWN)与服务端暂不可用
// (NOREPLICAS / LOADING / MASTERDOWN / BUSY / OOM / MISCONF / READONLY)。路由层按时间预算重试它们,
// 绝不以 REDIS_REPLY_ERROR 交给业务:业务把错误应答当业务结论处理(AsyncLoad 遇 ERROR 直接 fail-closed、
// 凭证条件写遇 ERROR 不重试),未执行类错误漏过去就成了错误结论。其余错误应答(ERR、WRONGTYPE、NOSCRIPT、
// 脚本自己的 error_reply)是业务结论,原样交给业务。
//
// 匹配只看错误文本的第一个词且必须完全相等:BUSYGROUP、BUSYKEY 这些是业务错误,不能按前缀算进 BUSY。

#include <hiredis/hiredis.h>

#include <cstddef>
#include <cstdint>
#include <iterator>
#include <optional>
#include <string_view>

#include "redis_node_endpoint.h"
#include "redis_slot_queues.h"

namespace redis_cluster
{
	enum class NotExecutedError : uint8_t
	{
		kMoved,
		kAsk,
		kTryAgain,
		kClusterDown,
		kNoReplicas,
		kLoading,
		kMasterDown,
		kBusy,
		kOom,
		kMisconf,
		kReadOnly,

		kCount,
	};

	inline constexpr std::string_view kNotExecutedErrorTokens[] = {
		"MOVED",
		"ASK",
		"TRYAGAIN",
		"CLUSTERDOWN",
		"NOREPLICAS",
		"LOADING",
		"MASTERDOWN",
		"BUSY",
		"OOM",
		"MISCONF",
		"READONLY",
	};
	static_assert(std::size(kNotExecutedErrorTokens) == static_cast<std::size_t>(NotExecutedError::kCount),
		"kNotExecutedErrorTokens must list every NotExecutedError");

	inline std::string_view NotExecutedErrorToken(NotExecutedError error)
	{
		return kNotExecutedErrorTokens[static_cast<std::size_t>(error)];
	}

	// 这条命令能否跟随重定向。只有 cluster 模式、发往 slot 主节点的命令可以;
	// standalone 与「任一已连接节点」的命令只能把 MOVED / ASK 当普通的未执行错误退避。
	enum class RedirectPolicy : uint8_t
	{
		kFollow,
		kNoFollow,
	};

	struct ReplyDisposition
	{
		std::optional<NotExecutedError> notExecuted;   // 空 = 最终应答,交给业务
		RetryMode retryMode = RetryMode::kBackoff;     // notExecuted 非空时有效
		std::optional<NodeEndpoint> movedTo;           // MOVED:更新 slot 表,重发到这里
		std::optional<NodeEndpoint> askTarget;         // ASK:只这一次发到这里,先发 ASKING,不改 slot 表
		bool refreshTopology = false;                  // 本地拓扑可能过时:MOVED、CLUSTERDOWN
	};

	// 错误文本的第一个词是否属于「没有执行」。
	std::optional<NotExecutedError> FindNotExecutedError(std::string_view errorText);

	// errorText 不含开头的 '-'(hiredis 已剥掉)。重定向目标不是合法的 IP 端点时(主机名、缺字段)跟随不了,按退避处理。
	ReplyDisposition ClassifyErrorText(std::string_view errorText, RedirectPolicy policy);

	ReplyDisposition ClassifyReply(const redisReply& reply, RedirectPolicy policy);

	// 形状与 hiredis::Hiredis::NotExecutedPredicate 相同,供维护前沿记账。空应答不算(结果未知)。
	bool IsNotExecutedReply(const redisReply* reply);
}

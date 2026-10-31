#include "redis_reply_classifier.h"

namespace redis_cluster
{
	namespace
	{
		// "<TOKEN> <slot> <host:port>"。
		std::optional<NodeEndpoint> ParseRedirectTarget(std::string_view errorText)
		{
			const std::size_t tokenEnd = errorText.find(' ');
			if (tokenEnd == std::string_view::npos)
			{
				return std::nullopt;
			}
			const std::size_t slotEnd = errorText.find(' ', tokenEnd + 1);
			if (slotEnd == std::string_view::npos)
			{
				return std::nullopt;
			}
			return ParseNodeEndpoint(errorText.substr(slotEnd + 1));
		}
	}

	std::optional<NotExecutedError> FindNotExecutedError(std::string_view errorText)
	{
		const std::string_view token = errorText.substr(0, errorText.find(' '));
		for (std::size_t i = 0; i < std::size(kNotExecutedErrorTokens); ++i)
		{
			if (kNotExecutedErrorTokens[i] == token)
			{
				return static_cast<NotExecutedError>(i);
			}
		}
		return std::nullopt;
	}

	ReplyDisposition ClassifyErrorText(std::string_view errorText, RedirectPolicy policy)
	{
		ReplyDisposition disposition;
		disposition.notExecuted = FindNotExecutedError(errorText);
		if (!disposition.notExecuted)
		{
			return disposition;
		}
		const NotExecutedError error = *disposition.notExecuted;
		disposition.refreshTopology = error == NotExecutedError::kMoved || error == NotExecutedError::kClusterDown;
		if (policy == RedirectPolicy::kFollow && (error == NotExecutedError::kMoved || error == NotExecutedError::kAsk))
		{
			std::optional<NodeEndpoint> target = ParseRedirectTarget(errorText);
			if (target)
			{
				disposition.retryMode = RetryMode::kImmediate;
				if (error == NotExecutedError::kMoved)
				{
					disposition.movedTo = std::move(target);
				}
				else
				{
					disposition.askTarget = std::move(target);
				}
			}
		}
		return disposition;
	}

	ReplyDisposition ClassifyReply(const redisReply& reply, RedirectPolicy policy)
	{
		if (reply.type != REDIS_REPLY_ERROR || reply.str == nullptr)
		{
			return {};
		}
		return ClassifyErrorText(std::string_view(reply.str, reply.len), policy);
	}

	bool IsNotExecutedReply(const redisReply* reply)
	{
		return reply != nullptr && reply->type == REDIS_REPLY_ERROR && reply->str != nullptr
			&& FindNotExecutedError(std::string_view(reply->str, reply->len)).has_value();
	}
}

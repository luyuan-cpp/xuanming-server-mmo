// RedisClusterClient 的纯逻辑单测(docs/design/redis-player-store-cluster.md §5.6):不碰网络,时间由用例传入。
// 覆盖键解析、同槽校验、停等顺序、每类错误的处置、预算耗尽收尾、断连收尾、排队上限、拓扑解析。
// 接到真实连接之后的行为见 redis_cluster_client_test.cpp。

#include <gtest/gtest.h>

#include <hiredis/hiredis.h>

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <initializer_list>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "engine/infra/storage/redis_cluster/redis_cluster_topology.h"
#include "engine/infra/storage/redis_cluster/redis_command_route.h"
#include "engine/infra/storage/redis_cluster/redis_node_endpoint.h"
#include "engine/infra/storage/redis_cluster/redis_reply_classifier.h"
#include "engine/infra/storage/redis_cluster/redis_slot.h"
#include "engine/infra/storage/redis_cluster/redis_slot_queues.h"

namespace
{
	using namespace redis_cluster;
	using namespace std::chrono_literals;

	// 用例的时间起点。只做差,不依赖真实时钟。
	const TimePoint kT0{};

	std::string Resp(std::initializer_list<std::string_view> args)
	{
		std::string out = "*" + std::to_string(args.size()) + "\r\n";
		for (const std::string_view arg : args)
		{
			out += "$" + std::to_string(arg.size()) + "\r\n";
			out += arg;
			out += "\r\n";
		}
		return out;
	}

	CommandRoute RouteOf(const std::string& formatted, std::optional<std::string_view> routingKey = std::nullopt)
	{
		return ResolveCommandRoute(formatted, routingKey);
	}

	// ---- 命令解析 ----

	TEST(RedisCommandRouteTest, ParsesFormattedCommandBinarySafely)
	{
		const std::string binary("a\r\nb\0c", 6);
		const std::string formatted = Resp({"SET", "k", binary});
		const auto args = ParseFormattedCommand(formatted);
		ASSERT_TRUE(args.has_value());
		ASSERT_EQ(args->size(), 3u);
		EXPECT_EQ((*args)[0], "SET");
		EXPECT_EQ((*args)[1], "k");
		EXPECT_EQ((*args)[2], binary);
	}

	TEST(RedisCommandRouteTest, ParsesWhatHiredisActuallyFormats)
	{
		char* raw = nullptr;
		const int length = ::redisFormatCommand(&raw, "EVAL %s 2 %s %s %d %b", "return 1", "a:{5}", "b:{5}", 7, "x\0y",
			static_cast<std::size_t>(3));
		ASSERT_GT(length, 0);
		const std::string formatted(raw, static_cast<std::size_t>(length));
		::redisFreeCommand(raw);

		const auto args = ParseFormattedCommand(formatted);
		ASSERT_TRUE(args.has_value());
		ASSERT_EQ(args->size(), 7u);
		EXPECT_EQ((*args)[0], "EVAL");
		EXPECT_EQ((*args)[3], "a:{5}");
		EXPECT_EQ((*args)[5], "7");
		EXPECT_EQ((*args)[6], std::string_view("x\0y", 3));

		const CommandRoute route = RouteOf(formatted);
		EXPECT_EQ(route.kind, RouteKind::kSlot);
		EXPECT_EQ(route.slot, redis_slot::HashSlot("5"));
	}

	TEST(RedisCommandRouteTest, RejectsMalformedBuffers)
	{
		const std::string_view malformed[] = {
			"",
			"PING\r\n",
			"*0\r\n",
			"*1\r\n",
			"*1\r\n$4\r\nPIN",
			"*1\r\n$4\r\nPINGxx",
			"*1\r\n$4\r\nPING\r\nextra",
			"*2\r\n$4\r\nPING\r\n",
			"*99999999999\r\n",
			"*1\r\n$99999999999999999999\r\nPING\r\n",
			"*1\r\n$-1\r\n",
			"*x\r\n",
		};
		for (const std::string_view buffer : malformed)
		{
			EXPECT_FALSE(ParseFormattedCommand(buffer).has_value()) << std::string(buffer);
			EXPECT_EQ(ResolveCommandRoute(buffer).reason, RouteRejectReason::kMalformedCommand) << std::string(buffer);
		}
	}

	// ---- 键位置 ----

	TEST(RedisCommandRouteTest, SingleKeyCommandsRouteByFirstArgument)
	{
		const std::string_view names[] = {"GET", "SET", "HGET", "HSET", "HMGET", "HGETALL", "ZADD", "ZRANGE", "ZREM", "EXPIRE", "get", "hGetAll"};
		for (const std::string_view name : names)
		{
			const std::string formatted = Resp({name, "plain:key", "other:argument"});
			const CommandRoute route = RouteOf(formatted);
			EXPECT_EQ(route.kind, RouteKind::kSlot) << std::string(name);
			EXPECT_EQ(route.slot, redis_slot::HashSlot("plain:key")) << std::string(name);
			EXPECT_EQ(route.commandName, name);
		}
	}

	TEST(RedisCommandRouteTest, SingleKeyDoesNotNeedHashTag)
	{
		// 缺省策略只查多键。玩家域句柄用 kAllKeys,见下一条。
		EXPECT_EQ(RouteOf(Resp({"GET", "player:42:location"})).kind, RouteKind::kSlot);
		EXPECT_EQ(RouteOf(Resp({"EVAL", "return 1", "1", "player:42:location"})).kind, RouteKind::kSlot);
		EXPECT_EQ(RouteOf(Resp({"MGET", "player:42:location"})).kind, RouteKind::kSlot);
	}

	TEST(RedisCommandRouteTest, AllKeysPolicyRejectsUntaggedKeysAndRoutingKeys)
	{
		const auto route = [](const std::string& formatted, std::optional<std::string_view> routingKey = std::nullopt) {
			return ResolveCommandRoute(formatted, routingKey, HashTagPolicy::kAllKeys);
		};

		const std::string untaggedGet = Resp({"GET", "world_channel_directory:zone:1:2"});
		const CommandRoute untagged = route(untaggedGet);
		EXPECT_EQ(untagged.reason, RouteRejectReason::kMissingHashTag);
		EXPECT_EQ(untagged.offendingKey, "world_channel_directory:zone:1:2");
		EXPECT_EQ(route(Resp({"EVAL", "return 1", "1", "player:42:location"})).reason, RouteRejectReason::kMissingHashTag);
		EXPECT_EQ(route(Resp({"MGET", "player:42:location"})).reason, RouteRejectReason::kMissingHashTag);
		EXPECT_EQ(route(Resp({"PING"}), std::string_view("untagged")).reason, RouteRejectReason::kMissingHashTag);

		const CommandRoute tagged = route(Resp({"GET", "player:{42}:location"}));
		EXPECT_EQ(tagged.kind, RouteKind::kSlot);
		EXPECT_EQ(tagged.slot, redis_slot::PlayerSlot(42));
		const CommandRoute ping = route(Resp({"PING"}), std::string_view("player:{42}:x"));
		EXPECT_EQ(ping.kind, RouteKind::kSlot);
		EXPECT_EQ(ping.slot, redis_slot::PlayerSlot(42));

		// 多键的既有检查不变;SCRIPT LOAD 没有键,照旧发往任一节点。
		EXPECT_EQ(route(Resp({"MGET", "a:{42}", "b:{43}"})).reason, RouteRejectReason::kCrossSlot);
		EXPECT_EQ(route(Resp({"SCRIPT", "LOAD", "return 1"})).kind, RouteKind::kAnyNode);
	}

	TEST(RedisCommandRouteTest, CommandMissingItsKeyIsMalformed)
	{
		for (const std::string_view name : {std::string_view("GET"), std::string_view("MGET"), std::string_view("DEL")})
		{
			EXPECT_EQ(RouteOf(Resp({name})).reason, RouteRejectReason::kMalformedCommand) << std::string(name);
		}
		EXPECT_EQ(RouteOf(Resp({"EVAL", "return 1"})).reason, RouteRejectReason::kMalformedCommand);
	}

	TEST(RedisCommandRouteTest, EvalTakesKeysByNumKeysAndIgnoresArgv)
	{
		const std::string formatted = Resp({"EVAL", "return 1", "3", "PlayerAllData:{42}", "player:{42}:owner_epoch",
			"player:{42}:location", "argv:{999}", "more"});
		const CommandRoute route = RouteOf(formatted);
		EXPECT_EQ(route.kind, RouteKind::kSlot);
		EXPECT_EQ(route.slot, 8000);

		const CommandRoute sha = RouteOf(Resp({"EVALSHA", "abc", "2", "a:{7}", "b:{7}", "argv"}));
		EXPECT_EQ(sha.kind, RouteKind::kSlot);
		EXPECT_EQ(sha.slot, redis_slot::HashSlot("7"));
	}

	TEST(RedisCommandRouteTest, EvalWithBadNumKeysIsRejected)
	{
		const std::string_view bad[] = {"2", "-1", "abc", "", "1x"};
		for (const std::string_view numKeys : bad)
		{
			EXPECT_EQ(RouteOf(Resp({"EVAL", "return 1", numKeys, "a:{7}"})).reason, RouteRejectReason::kBadNumKeys)
				<< std::string(numKeys);
		}
	}

	// ---- 同槽校验 ----

	TEST(RedisCommandRouteTest, MultiKeyCommandsMustShareSlotAndCarryTags)
	{
		const std::string_view names[] = {"MGET", "DEL", "EXISTS"};
		for (const std::string_view name : names)
		{
			const CommandRoute ok = RouteOf(Resp({name, "x:{7}", "y:{7}"}));
			EXPECT_EQ(ok.kind, RouteKind::kSlot) << std::string(name);
			EXPECT_EQ(ok.slot, redis_slot::HashSlot("7")) << std::string(name);

			const std::string crossSlot = Resp({name, "x:{7}", "y:{8}"});
			const CommandRoute cross = RouteOf(crossSlot);
			EXPECT_EQ(cross.kind, RouteKind::kRejected) << std::string(name);
			EXPECT_EQ(cross.reason, RouteRejectReason::kCrossSlot) << std::string(name);
			EXPECT_EQ(cross.offendingKey, "y:{8}") << std::string(name);

			// 恰好同 slot 但没带 tag 也不行:靠巧合同槽的键迁移后没有任何保证。
			const std::string noTag = Resp({name, "same", "same"});
			EXPECT_EQ(RouteOf(noTag).reason, RouteRejectReason::kMissingHashTag) << std::string(name);
		}
	}

	TEST(RedisCommandRouteTest, EveryKeyMustCarryTagNotJustTheFirst)
	{
		// "7" 与 "x:{7}" 同 slot,第二个键没带 tag。
		ASSERT_EQ(redis_slot::HashSlot("7"), redis_slot::HashSlot("x:{7}"));
		const std::string formatted = Resp({"EVAL", "return 1", "2", "x:{7}", "7"});
		const CommandRoute route = RouteOf(formatted);
		EXPECT_EQ(route.reason, RouteRejectReason::kMissingHashTag);
		EXPECT_EQ(route.offendingKey, "7");
	}

	TEST(RedisCommandRouteTest, PlayerKeyAndItsSlotIndexTagMayShareACommand)
	{
		// 两个 tag 字面不同,但按构造同 slot(§5.2 的 slot-tag)。
		const std::string playerKey = "player:" + redis_slot::PlayerTag(42) + ":owner_epoch";
		const std::string indexKey = "player:index:" + redis_slot::PlayerIndexTag(42);
		const CommandRoute route = RouteOf(Resp({"EVAL", "return 1", "2", playerKey, indexKey}));
		EXPECT_EQ(route.kind, RouteKind::kSlot);
		EXPECT_EQ(route.slot, redis_slot::PlayerSlot(42));
	}

	// ---- 无键命令与显式路由键 ----

	TEST(RedisCommandRouteTest, KeylessCommandNeedsExplicitRoutingKey)
	{
		EXPECT_EQ(RouteOf(Resp({"PING"})).reason, RouteRejectReason::kKeylessNeedsRoutingKey);
		EXPECT_EQ(RouteOf(Resp({"EVAL", "return 1", "0"})).reason, RouteRejectReason::kKeylessNeedsRoutingKey);

		const std::string_view routingKey = "player:{42}:x";
		const CommandRoute ping = RouteOf(Resp({"PING"}), routingKey);
		EXPECT_EQ(ping.kind, RouteKind::kSlot);
		EXPECT_EQ(ping.slot, 8000);
		const CommandRoute eval = RouteOf(Resp({"EVAL", "return 1", "0"}), routingKey);
		EXPECT_EQ(eval.kind, RouteKind::kSlot);
		EXPECT_EQ(eval.slot, 8000);
	}

	TEST(RedisCommandRouteTest, RoutingKeyCannotBypassTheGuards)
	{
		const std::string_view routingKey = "player:{42}:x";
		EXPECT_EQ(RouteOf(Resp({"GET", "other:{43}:y"}), routingKey).reason, RouteRejectReason::kRoutingKeyMismatch);
		EXPECT_EQ(RouteOf(Resp({"MGET", "a:{42}", "b:{43}"}), routingKey).reason, RouteRejectReason::kCrossSlot);
		EXPECT_EQ(RouteOf(Resp({"PUBLISH", "channel", "m"}), routingKey).reason, RouteRejectReason::kUnknownCommand);
		EXPECT_EQ(RouteOf(Resp({"PING"}), std::string_view()).reason, RouteRejectReason::kEmptyRoutingKey);

		const CommandRoute same = RouteOf(Resp({"GET", "other:{42}:y"}), routingKey);
		EXPECT_EQ(same.kind, RouteKind::kSlot);
		EXPECT_EQ(same.slot, 8000);
	}

	TEST(RedisCommandRouteTest, ScriptLoadGoesToAnyNodeOtherSubcommandsAreRejected)
	{
		EXPECT_EQ(RouteOf(Resp({"SCRIPT", "LOAD", "return 1"})).kind, RouteKind::kAnyNode);
		EXPECT_EQ(RouteOf(Resp({"script", "load", "return 1"})).kind, RouteKind::kAnyNode);
		EXPECT_EQ(RouteOf(Resp({"SCRIPT", "FLUSH"})).reason, RouteRejectReason::kUnknownCommand);
		EXPECT_EQ(RouteOf(Resp({"SCRIPT", "LOAD"})).reason, RouteRejectReason::kUnknownCommand);

		const CommandRoute routed = RouteOf(Resp({"SCRIPT", "LOAD", "return 1"}), std::string_view("player:{42}:x"));
		EXPECT_EQ(routed.kind, RouteKind::kSlot);
		EXPECT_EQ(routed.slot, 8000);
	}

	TEST(RedisCommandRouteTest, UnknownAndConnectionStateCommandsFailClosed)
	{
		const std::string_view names[] = {"PUBLISH", "AUTH", "SELECT", "MULTI", "SUBSCRIBE", "ASKING", "KEYS", "SCAN"};
		for (const std::string_view name : names)
		{
			EXPECT_EQ(RouteOf(Resp({name, "x"})).reason, RouteRejectReason::kUnknownCommand) << std::string(name);
		}
	}

	TEST(RedisCommandRouteTest, RejectReasonNames)
	{
		EXPECT_EQ(RouteRejectReasonName(RouteRejectReason::kCrossSlot), "cross_slot");
		EXPECT_EQ(RouteRejectReasonName(RouteRejectReason::kMissingHashTag), "missing_hash_tag");
		EXPECT_EQ(RouteRejectReasonName(RouteRejectReason::kRoutingKeyMismatch), "routing_key_mismatch");
	}

	// ---- 端点 ----

	TEST(RedisNodeEndpointTest, OnlyIpLiteralsAreAccepted)
	{
		EXPECT_TRUE(IsIpLiteral("127.0.0.1"));
		EXPECT_TRUE(IsIpLiteral("10.96.0.15"));
		EXPECT_TRUE(IsIpLiteral("255.255.255.255"));
		EXPECT_TRUE(IsIpLiteral("::1"));
		EXPECT_TRUE(IsIpLiteral("fd00:10:96::a"));

		EXPECT_FALSE(IsIpLiteral(""));
		EXPECT_FALSE(IsIpLiteral("redis-0"));
		EXPECT_FALSE(IsIpLiteral("redis.infra.svc"));
		EXPECT_FALSE(IsIpLiteral("abc"));
		EXPECT_FALSE(IsIpLiteral("256.0.0.1"));
		EXPECT_FALSE(IsIpLiteral("1.2.3"));
		EXPECT_FALSE(IsIpLiteral("1.2.3.4.5"));
		EXPECT_FALSE(IsIpLiteral("01.2.3.4"));
		EXPECT_FALSE(IsIpLiteral("1.2.3."));
	}

	TEST(RedisNodeEndpointTest, ParsesHostPort)
	{
		const auto v4 = ParseNodeEndpoint("10.0.0.2:7000");
		ASSERT_TRUE(v4.has_value());
		EXPECT_EQ(v4->ip, "10.0.0.2");
		EXPECT_EQ(v4->port, 7000);
		EXPECT_FALSE(v4->IsIpv6());

		const auto v6 = ParseNodeEndpoint("::1:6381");
		ASSERT_TRUE(v6.has_value());
		EXPECT_EQ(v6->ip, "::1");
		EXPECT_EQ(v6->port, 6381);
		EXPECT_TRUE(v6->IsIpv6());

		const std::string_view bad[] = {"", "10.0.0.2", ":6379", "10.0.0.2:", "10.0.0.2:0", "10.0.0.2:65536", "redis-0:6379", "10.0.0.2:x"};
		for (const std::string_view text : bad)
		{
			EXPECT_FALSE(ParseNodeEndpoint(text).has_value()) << std::string(text);
		}
	}

	TEST(RedisNodeEndpointTest, SplitHostPortAcceptsHostnames)
	{
		// 部署配置里的种子可以写主机名,启动时解析一次;运行期的 MOVED 只认 IP(上一条)。
		const auto name = SplitHostPort("redis.infra:6379");
		ASSERT_TRUE(name.has_value());
		EXPECT_EQ(name->host, "redis.infra");
		EXPECT_EQ(name->port, 6379);

		const auto v4 = SplitHostPort("10.0.0.2:7000");
		ASSERT_TRUE(v4.has_value());
		EXPECT_EQ(v4->host, "10.0.0.2");
		EXPECT_EQ(v4->port, 7000);

		const auto v6 = SplitHostPort("::1:6381");
		ASSERT_TRUE(v6.has_value());
		EXPECT_EQ(v6->host, "::1");
		EXPECT_EQ(v6->port, 6381);

		const std::string_view bad[] = {"", "redis", ":6379", "redis:", "redis:0", "redis:65536", "redis:x", "redis:6379x"};
		for (const std::string_view text : bad)
		{
			EXPECT_FALSE(SplitHostPort(text).has_value()) << std::string(text);
		}
	}

	// ---- 错误归类:每类错误的处置 ----

	TEST(RedisReplyClassifierTest, MovedUpdatesOwnerAndResendsImmediately)
	{
		const ReplyDisposition d = ClassifyErrorText("MOVED 3999 127.0.0.1:6381", RedirectPolicy::kFollow);
		EXPECT_EQ(d.notExecuted, NotExecutedError::kMoved);
		EXPECT_EQ(d.retryMode, RetryMode::kImmediate);
		ASSERT_TRUE(d.movedTo.has_value());
		EXPECT_EQ(d.movedTo->ip, "127.0.0.1");
		EXPECT_EQ(d.movedTo->port, 6381);
		EXPECT_FALSE(d.askTarget.has_value());
		EXPECT_TRUE(d.refreshTopology);
	}

	TEST(RedisReplyClassifierTest, AskResendsImmediatelyWithoutTouchingOwner)
	{
		const ReplyDisposition d = ClassifyErrorText("ASK 3999 10.0.0.2:7000", RedirectPolicy::kFollow);
		EXPECT_EQ(d.notExecuted, NotExecutedError::kAsk);
		EXPECT_EQ(d.retryMode, RetryMode::kImmediate);
		ASSERT_TRUE(d.askTarget.has_value());
		EXPECT_EQ(d.askTarget->ip, "10.0.0.2");
		EXPECT_EQ(d.askTarget->port, 7000);
		EXPECT_FALSE(d.movedTo.has_value());
		EXPECT_FALSE(d.refreshTopology);
	}

	TEST(RedisReplyClassifierTest, UnfollowableRedirectBacksOff)
	{
		const std::string_view unfollowable[] = {
			"MOVED 3999 redis-0:6381",      // 主机名
			"MOVED 3999 :6381",             // cluster-preferred-endpoint-type unknown-endpoint
			"MOVED 3999",                   // 缺字段
			"MOVED",
			"ASK 3999 redis-0:6381",
		};
		for (const std::string_view text : unfollowable)
		{
			const ReplyDisposition d = ClassifyErrorText(text, RedirectPolicy::kFollow);
			EXPECT_TRUE(d.notExecuted.has_value()) << std::string(text);
			EXPECT_EQ(d.retryMode, RetryMode::kBackoff) << std::string(text);
			EXPECT_FALSE(d.movedTo.has_value()) << std::string(text);
			EXPECT_FALSE(d.askTarget.has_value()) << std::string(text);
		}
		// 拓扑过时才会收到 MOVED:跟随不了也要刷新。
		EXPECT_TRUE(ClassifyErrorText("MOVED 3999 redis-0:6381", RedirectPolicy::kFollow).refreshTopology);
	}

	TEST(RedisReplyClassifierTest, NoFollowPolicyTurnsRedirectsIntoBackoff)
	{
		const ReplyDisposition moved = ClassifyErrorText("MOVED 3999 127.0.0.1:6381", RedirectPolicy::kNoFollow);
		EXPECT_EQ(moved.notExecuted, NotExecutedError::kMoved);
		EXPECT_EQ(moved.retryMode, RetryMode::kBackoff);
		EXPECT_FALSE(moved.movedTo.has_value());

		const ReplyDisposition ask = ClassifyErrorText("ASK 3999 127.0.0.1:6381", RedirectPolicy::kNoFollow);
		EXPECT_EQ(ask.notExecuted, NotExecutedError::kAsk);
		EXPECT_EQ(ask.retryMode, RetryMode::kBackoff);
		EXPECT_FALSE(ask.askTarget.has_value());
	}

	struct BackoffErrorCase
	{
		std::string_view text;
		NotExecutedError error;
		bool refreshTopology;
	};

	const BackoffErrorCase kBackoffErrors[] = {
		{"TRYAGAIN Multiple keys request during rehashing of slot", NotExecutedError::kTryAgain, false},
		{"CLUSTERDOWN The cluster is down", NotExecutedError::kClusterDown, true},
		{"CLUSTERDOWN Hash slot not served", NotExecutedError::kClusterDown, true},
		{"NOREPLICAS Not enough good replicas to write.", NotExecutedError::kNoReplicas, false},
		{"LOADING Redis is loading the dataset in memory", NotExecutedError::kLoading, false},
		{"MASTERDOWN Link with MASTER is down and replica-serve-stale-data is set to 'no'.", NotExecutedError::kMasterDown, false},
		{"BUSY Redis is busy running a script.", NotExecutedError::kBusy, false},
		{"OOM command not allowed when used memory > 'maxmemory'.", NotExecutedError::kOom, false},
		{"MISCONF Redis is configured to save RDB snapshots", NotExecutedError::kMisconf, false},
		{"READONLY You can't write against a read only replica.", NotExecutedError::kReadOnly, false},
	};

	TEST(RedisReplyClassifierTest, EveryUnavailableErrorBacksOffAndNeverReachesBusiness)
	{
		for (const BackoffErrorCase& testCase : kBackoffErrors)
		{
			const ReplyDisposition d = ClassifyErrorText(testCase.text, RedirectPolicy::kFollow);
			EXPECT_EQ(d.notExecuted, testCase.error) << std::string(testCase.text);
			EXPECT_EQ(d.retryMode, RetryMode::kBackoff) << std::string(testCase.text);
			EXPECT_EQ(d.refreshTopology, testCase.refreshTopology) << std::string(testCase.text);
			EXPECT_FALSE(d.movedTo.has_value());
			EXPECT_FALSE(d.askTarget.has_value());
		}
	}

	TEST(RedisReplyClassifierTest, BusinessErrorsAreDeliveredAsIs)
	{
		const std::string_view business[] = {
			"ERR unknown command",
			"WRONGTYPE Operation against a key holding the wrong kind of value",
			"NOSCRIPT No matching script",
			"CROSSSLOT Keys in request don't hash to the same slot",
			"BUSYGROUP Consumer Group name already exists",
			"BUSYKEY Target key name already exists.",
			"NOAUTH Authentication required.",
			"HANDOFF register shape",
			"HANDOFF_SQL record",
			"LOADINGX",
			"moved 3999 127.0.0.1:6381",
			"",
		};
		for (const std::string_view text : business)
		{
			const ReplyDisposition d = ClassifyErrorText(text, RedirectPolicy::kFollow);
			EXPECT_FALSE(d.notExecuted.has_value()) << std::string(text);
			EXPECT_FALSE(d.refreshTopology) << std::string(text);
		}
	}

	TEST(RedisReplyClassifierTest, ClassifiesRealReplyStructs)
	{
		std::string text = "LOADING Redis is loading the dataset in memory";
		redisReply error{};
		error.type = REDIS_REPLY_ERROR;
		error.str = text.data();
		error.len = text.size();
		EXPECT_EQ(ClassifyReply(error, RedirectPolicy::kFollow).notExecuted, NotExecutedError::kLoading);
		EXPECT_TRUE(IsNotExecutedReply(&error));

		// 同样的文本放在字符串应答里是业务数据,不是错误。
		redisReply stringReply = error;
		stringReply.type = REDIS_REPLY_STRING;
		EXPECT_FALSE(ClassifyReply(stringReply, RedirectPolicy::kFollow).notExecuted.has_value());
		EXPECT_FALSE(IsNotExecutedReply(&stringReply));

		std::string businessText = "ERR boom";
		redisReply business{};
		business.type = REDIS_REPLY_ERROR;
		business.str = businessText.data();
		business.len = businessText.size();
		EXPECT_FALSE(IsNotExecutedReply(&business));

		// 空应答 = 结果未知,不是「没有执行」。
		EXPECT_FALSE(IsNotExecutedReply(nullptr));
	}

	// ---- 重试预算 ----

	TEST(RedisRetryBudgetTest, BackoffDoublesFromFiftyMillisecondsAndCapsAtOneSecond)
	{
		const RetryPolicy policy;
		HeadRetryBudget budget(policy, kT0);
		const Duration expected[] = {50ms, 100ms, 200ms, 400ms, 800ms, 1000ms, 1000ms};
		TimePoint now = kT0;
		for (const Duration delay : expected)
		{
			const RetryDecision decision = budget.Next(policy, RetryMode::kBackoff, now);
			ASSERT_EQ(decision.kind, RetryDecision::Kind::kRetryAfter);
			EXPECT_EQ(decision.delay, delay);
			now += decision.delay;
		}
	}

	TEST(RedisRetryBudgetTest, BudgetIsTenSecondsAndTheLastDelayIsTruncatedToIt)
	{
		const RetryPolicy policy;
		HeadRetryBudget budget(policy, kT0);
		TimePoint now = kT0;
		int retries = 0;
		for (;;)
		{
			const RetryDecision decision = budget.Next(policy, RetryMode::kBackoff, now);
			if (decision.kind == RetryDecision::Kind::kExhausted)
			{
				break;
			}
			ASSERT_EQ(decision.kind, RetryDecision::Kind::kRetryAfter);
			ASSERT_GT(decision.delay, Duration::zero());
			now += decision.delay;
			ASSERT_LT(++retries, 100);
		}
		EXPECT_EQ(now - kT0, Duration(10s)) << "最后一次重试恰好落在截止时刻,此后耗尽";
		// 50+100+200+400+800 = 1550ms,8 次 1s 到 9550ms,最后截断成 450ms。
		EXPECT_EQ(retries, 14);
	}

	TEST(RedisRetryBudgetTest, ImmediateRedirectsAreCappedThenFallBackToBackoff)
	{
		const RetryPolicy policy;
		HeadRetryBudget budget(policy, kT0);
		for (uint32_t i = 0; i < policy.maxImmediateRedirects; ++i)
		{
			EXPECT_EQ(budget.Next(policy, RetryMode::kImmediate, kT0).kind, RetryDecision::Kind::kResendNow);
		}
		const RetryDecision capped = budget.Next(policy, RetryMode::kImmediate, kT0);
		EXPECT_EQ(capped.kind, RetryDecision::Kind::kRetryAfter);
		EXPECT_EQ(capped.delay, Duration(50ms));
	}

	TEST(RedisRetryBudgetTest, ImmediateRedirectIsAlsoBoundByTheBudget)
	{
		const RetryPolicy policy;
		HeadRetryBudget budget(policy, kT0);
		EXPECT_EQ(budget.Next(policy, RetryMode::kImmediate, kT0 + 10s).kind, RetryDecision::Kind::kExhausted);
		EXPECT_EQ(budget.Next(policy, RetryMode::kBackoff, kT0 + 11s).kind, RetryDecision::Kind::kExhausted);
	}

	// ---- 停等队列 ----

	using TestQueues = SlotCommandQueues<int>;

	TEST(RedisSlotQueuesTest, SameQueueIsStopAndWaitInCallOrder)
	{
		TestQueues queues{RetryPolicy{}, QueueLimits{}};
		EXPECT_EQ(queues.Submit(7, 1, kT0), SubmitResult::kDispatchNow);
		EXPECT_EQ(queues.Submit(7, 2, kT0), SubmitResult::kQueued);
		EXPECT_EQ(queues.Submit(7, 3, kT0), SubmitResult::kQueued);
		ASSERT_NE(queues.Head(7), nullptr);
		EXPECT_EQ(*queues.Head(7), 1);
		EXPECT_EQ(queues.QueuedTotal(), 2u);

		// 第一条没拿到最终应答,不管过多久第二条都不会被放出来。
		queues.MarkInFlight(7);
		EXPECT_EQ(queues.State(7), HeadState::kInFlight);
		EXPECT_EQ(*queues.Head(7), 1);

		EXPECT_TRUE(queues.CompleteHead(7, kT0));
		EXPECT_EQ(*queues.Head(7), 2);
		EXPECT_EQ(queues.State(7), HeadState::kPending);
		EXPECT_EQ(queues.QueuedTotal(), 1u);

		EXPECT_TRUE(queues.CompleteHead(7, kT0));
		EXPECT_EQ(*queues.Head(7), 3);
		EXPECT_EQ(queues.QueuedTotal(), 0u);

		EXPECT_FALSE(queues.CompleteHead(7, kT0));
		EXPECT_EQ(queues.Head(7), nullptr);
		EXPECT_FALSE(queues.Busy(7));
		EXPECT_FALSE(queues.State(7).has_value());

		// 队列回到空闲:下一条立刻成为队头。
		EXPECT_EQ(queues.Submit(7, 4, kT0), SubmitResult::kDispatchNow);
	}

	TEST(RedisSlotQueuesTest, DifferentQueuesDoNotBlockEachOther)
	{
		TestQueues queues{RetryPolicy{}, QueueLimits{}};
		EXPECT_EQ(queues.Submit(1, 10, kT0), SubmitResult::kDispatchNow);
		EXPECT_EQ(queues.Submit(2, 20, kT0), SubmitResult::kDispatchNow);
		EXPECT_EQ(queues.Submit(kAnyNodeQueue, 30, kT0), SubmitResult::kDispatchNow);
		EXPECT_EQ(queues.Submit(1, 11, kT0), SubmitResult::kQueued);
		EXPECT_EQ(queues.BusyQueues().size(), 3u);

		EXPECT_FALSE(queues.CompleteHead(2, kT0));
		EXPECT_EQ(*queues.Head(1), 10);
		EXPECT_EQ(*queues.Head(kAnyNodeQueue), 30);
	}

	TEST(RedisSlotQueuesTest, EveryNotExecutedErrorKeepsTheHeadAndNeverReleasesTheNextCommand)
	{
		const std::string_view redirects[] = {"MOVED 1 127.0.0.1:6381", "ASK 1 127.0.0.1:6381"};
		for (const std::string_view text : redirects)
		{
			TestQueues queues{RetryPolicy{}, QueueLimits{}};
			queues.Submit(1, 10, kT0);
			queues.Submit(1, 11, kT0);
			queues.MarkInFlight(1);
			const RetryDecision decision = queues.OnNotExecuted(1, ClassifyErrorText(text, RedirectPolicy::kFollow).retryMode, kT0);
			EXPECT_EQ(decision.kind, RetryDecision::Kind::kResendNow) << std::string(text);
			EXPECT_EQ(queues.State(1), HeadState::kPending) << std::string(text);
			EXPECT_EQ(*queues.Head(1), 10) << std::string(text);
			EXPECT_EQ(queues.QueuedIn(1), 1u) << std::string(text);
		}
		for (const BackoffErrorCase& testCase : kBackoffErrors)
		{
			TestQueues queues{RetryPolicy{}, QueueLimits{}};
			queues.Submit(1, 10, kT0);
			queues.Submit(1, 11, kT0);
			queues.MarkInFlight(1);
			const RetryDecision decision = queues.OnNotExecuted(1, ClassifyErrorText(testCase.text, RedirectPolicy::kFollow).retryMode, kT0);
			EXPECT_EQ(decision.kind, RetryDecision::Kind::kRetryAfter) << std::string(testCase.text);
			EXPECT_EQ(decision.delay, Duration(50ms)) << std::string(testCase.text);
			EXPECT_EQ(queues.State(1), HeadState::kBackoff) << std::string(testCase.text);
			EXPECT_EQ(*queues.Head(1), 10) << std::string(testCase.text);
			EXPECT_EQ(queues.QueuedIn(1), 1u) << std::string(testCase.text);
		}
	}

	TEST(RedisSlotQueuesTest, BudgetStartsWhenCommandBecomesHeadNotWhenItWasQueued)
	{
		TestQueues queues{RetryPolicy{}, QueueLimits{}};
		queues.Submit(1, 10, kT0);
		queues.Submit(1, 11, kT0);
		queues.MarkInFlight(1);

		// 第二条排队等了 9s 才成为队头。
		const TimePoint promotedAt = kT0 + 9s;
		EXPECT_TRUE(queues.CompleteHead(1, promotedAt));
		queues.MarkInFlight(1);
		EXPECT_EQ(queues.OnNotExecuted(1, RetryMode::kBackoff, promotedAt + 9s).kind, RetryDecision::Kind::kRetryAfter);
		queues.MarkInFlight(1);
		EXPECT_EQ(queues.OnNotExecuted(1, RetryMode::kBackoff, promotedAt + 10s).kind, RetryDecision::Kind::kExhausted);
	}

	TEST(RedisSlotQueuesTest, ExhaustedBudgetAbortsHeadAndQueuedInCallOrder)
	{
		TestQueues queues{RetryPolicy{}, QueueLimits{}};
		queues.Submit(5, 1, kT0);
		queues.Submit(5, 2, kT0);
		queues.Submit(5, 3, kT0);
		queues.Submit(6, 9, kT0);
		queues.Submit(6, 10, kT0);
		queues.MarkInFlight(5);

		const RetryDecision decision = queues.OnNotExecuted(5, RetryMode::kBackoff, kT0 + 10s);
		EXPECT_EQ(decision.kind, RetryDecision::Kind::kExhausted);
		EXPECT_EQ(queues.Abort(5), (std::vector<int>{1, 2, 3}));
		EXPECT_FALSE(queues.Busy(5));
		EXPECT_EQ(queues.QueuedTotal(), 1u) << "别的队列不受影响";
		EXPECT_EQ(*queues.Head(6), 9);
		EXPECT_EQ(queues.Submit(5, 4, kT0 + 10s), SubmitResult::kDispatchNow) << "收尾之后可以接受新命令";
	}

	TEST(RedisSlotQueuesTest, DisconnectAbortsInFlightHeadAndQueuedInCallOrder)
	{
		TestQueues queues{RetryPolicy{}, QueueLimits{}};
		queues.Submit(5, 1, kT0);
		queues.Submit(5, 2, kT0);
		queues.MarkInFlight(5);
		EXPECT_EQ(queues.Abort(5), (std::vector<int>{1, 2}));
		EXPECT_EQ(queues.QueuedTotal(), 0u);
		EXPECT_TRUE(queues.Abort(5).empty()) << "空闲队列收尾是空操作";
	}

	TEST(RedisSlotQueuesTest, DefaultLimitsAre256PerQueueAnd65536Total)
	{
		const QueueLimits limits;
		EXPECT_EQ(limits.perQueue, 256u);
		EXPECT_EQ(limits.total, 65536u);
	}

	TEST(RedisSlotQueuesTest, PerQueueLimitCountsQueuedNotHead)
	{
		QueueLimits limits;
		limits.perQueue = 2;
		TestQueues queues{RetryPolicy{}, limits};
		EXPECT_EQ(queues.Submit(1, 0, kT0), SubmitResult::kDispatchNow);
		EXPECT_EQ(queues.Submit(1, 1, kT0), SubmitResult::kQueued);
		EXPECT_EQ(queues.Submit(1, 2, kT0), SubmitResult::kQueued);
		EXPECT_EQ(queues.Submit(1, 3, kT0), SubmitResult::kQueueFull);
		EXPECT_EQ(queues.QueuedIn(1), 2u);
		EXPECT_EQ(queues.Submit(2, 4, kT0), SubmitResult::kDispatchNow) << "每队列上限不影响别的队列";

		EXPECT_TRUE(queues.CompleteHead(1, kT0));
		EXPECT_EQ(queues.Submit(1, 5, kT0), SubmitResult::kQueued) << "腾出位置后可以再排";
	}

	TEST(RedisSlotQueuesTest, TotalLimitSpansAllQueuesAndIsReleasedByCompletionAndAbort)
	{
		QueueLimits limits;
		limits.total = 3;
		TestQueues queues{RetryPolicy{}, limits};
		for (QueueIndex queue = 0; queue < 3; ++queue)
		{
			EXPECT_EQ(queues.Submit(queue, 0, kT0), SubmitResult::kDispatchNow);
			EXPECT_EQ(queues.Submit(queue, 1, kT0), SubmitResult::kQueued);
		}
		EXPECT_EQ(queues.QueuedTotal(), 3u);
		EXPECT_EQ(queues.Submit(0, 2, kT0), SubmitResult::kTotalQueueFull);
		EXPECT_EQ(queues.Submit(3, 0, kT0), SubmitResult::kDispatchNow) << "队头不占排队额度";
		EXPECT_EQ(queues.Submit(3, 1, kT0), SubmitResult::kTotalQueueFull);

		EXPECT_TRUE(queues.CompleteHead(0, kT0));
		EXPECT_EQ(queues.QueuedTotal(), 2u);
		queues.Abort(1);
		EXPECT_EQ(queues.QueuedTotal(), 1u);
		EXPECT_EQ(queues.Submit(3, 1, kT0), SubmitResult::kQueued);
	}

	// ---- 拓扑解析 ----

	// 按 hiredis 的 redisReply 形状搭应答树,只用来喂解析函数。节点放在 deque 里,地址稳定。
	class ReplyTree
	{
	public:
		const redisReply* Integer(long long value)
		{
			redisReply& reply = New(REDIS_REPLY_INTEGER);
			reply.integer = value;
			return &reply;
		}

		const redisReply* String(std::string_view text)
		{
			std::string& stored = strings_.emplace_back(text);
			redisReply& reply = New(REDIS_REPLY_STRING);
			reply.str = stored.data();
			reply.len = stored.size();
			return &reply;
		}

		const redisReply* Nil()
		{
			return &New(REDIS_REPLY_NIL);
		}

		const redisReply* Array(std::initializer_list<const redisReply*> elements)
		{
			std::vector<redisReply*>& stored = arrays_.emplace_back();
			for (const redisReply* element : elements)
			{
				// hiredis 的 element 字段不带 const,解析函数只读。
				stored.push_back(const_cast<redisReply*>(element));
			}
			redisReply& reply = New(REDIS_REPLY_ARRAY);
			reply.elements = stored.size();
			reply.element = stored.data();
			return &reply;
		}

		// [first, last, [ip, port, id]]
		const redisReply* Range(long long first, long long last, std::string_view ip, long long port)
		{
			return Array({Integer(first), Integer(last), Array({String(ip), Integer(port), String("nodeid")})});
		}

	private:
		redisReply& New(int type)
		{
			redisReply& reply = replies_.emplace_back();
			reply.type = type;
			return reply;
		}

		std::deque<redisReply> replies_;
		std::deque<std::string> strings_;
		std::deque<std::vector<redisReply*>> arrays_;
	};

	TEST(RedisClusterTopologyTest, ParsesClusterSlotsAndIgnoresReplicasAndMetadata)
	{
		ReplyTree tree;
		const redisReply* reply = tree.Array({
			tree.Array({
				tree.Integer(0), tree.Integer(5460),
				tree.Array({tree.String("10.0.0.1"), tree.Integer(7000), tree.String("id-a"), tree.Array({})}),
				tree.Array({tree.String("10.0.0.4"), tree.Integer(7003), tree.String("id-d")}),
			}),
			tree.Range(5461, 10922, "10.0.0.2", 7001),
			tree.Range(10923, 16383, "10.0.0.3", 7002),
		});
		std::string error;
		const auto topology = ParseClusterSlots(*reply, error);
		ASSERT_TRUE(topology.has_value()) << error;
		ASSERT_EQ(topology->ranges.size(), 3u);
		EXPECT_EQ(topology->ranges[0].first, 0);
		EXPECT_EQ(topology->ranges[0].last, 5460);
		EXPECT_EQ(topology->ranges[0].master, (NodeEndpoint{"10.0.0.1", 7000}));
		EXPECT_EQ(topology->ranges[2].first, 10923);
		EXPECT_EQ(topology->ranges[2].last, 16383);
		EXPECT_EQ(topology->ranges[2].master, (NodeEndpoint{"10.0.0.3", 7002}));
	}

	TEST(RedisClusterTopologyTest, AnyInvalidEntryInvalidatesTheWholeReply)
	{
		ReplyTree tree;
		const redisReply* good = tree.Range(0, 100, "10.0.0.1", 7000);
		const redisReply* bad[] = {
			tree.Range(200, 100, "10.0.0.1", 7000),        // first > last
			tree.Range(-1, 100, "10.0.0.1", 7000),
			tree.Range(0, 16384, "10.0.0.1", 7000),        // 超出 slot 范围
			tree.Range(0, 100, "redis-0.infra.svc", 7000), // 主机名
			tree.Range(0, 100, "", 7000),                  // 未知端点
			tree.Range(0, 100, "10.0.0.1", 0),
			tree.Range(0, 100, "10.0.0.1", 65536),
			tree.Array({tree.Integer(0), tree.Integer(100)}),                                           // 缺主节点
			tree.Array({tree.Integer(0), tree.Integer(100), tree.Array({tree.Nil(), tree.Integer(7000)})}), // NULL 端点
			tree.Array({tree.Integer(0), tree.Integer(100), tree.Array({tree.String("10.0.0.1")})}),    // 缺端口
			tree.Array({tree.String("0"), tree.Integer(100), tree.Array({tree.String("10.0.0.1"), tree.Integer(7000)})}),
			tree.Integer(1),
		};
		for (const redisReply* entry : bad)
		{
			std::string error;
			EXPECT_FALSE(ParseClusterSlots(*tree.Array({good, entry}), error).has_value());
			EXPECT_FALSE(error.empty());
		}
	}

	TEST(RedisClusterTopologyTest, NonArrayAndEmptyRepliesAreInvalid)
	{
		ReplyTree tree;
		std::string error;
		EXPECT_FALSE(ParseClusterSlots(*tree.Array({}), error).has_value()) << "空数组:节点还没加入集群,不能拿来清空路由表";
		EXPECT_FALSE(error.empty());
		error.clear();
		EXPECT_FALSE(ParseClusterSlots(*tree.Integer(1), error).has_value());
		EXPECT_FALSE(error.empty());
	}

	TEST(RedisClusterTopologyTest, OwnerTableAppliesTopologyAndMoved)
	{
		const NodeEndpoint a{"10.0.0.1", 7000};
		const NodeEndpoint b{"10.0.0.2", 7001};
		const NodeEndpoint c{"10.0.0.3", 7002};

		SlotOwnerTable table;
		EXPECT_EQ(table.Owner(0), nullptr) << "拓扑到达前没有主";

		ClusterTopology topology;
		topology.ranges.push_back(SlotRange{0, 100, a});
		topology.ranges.push_back(SlotRange{200, 16383, b});
		table.ApplyTopology(topology);
		ASSERT_NE(table.Owner(0), nullptr);
		EXPECT_EQ(*table.Owner(0), a);
		EXPECT_EQ(*table.Owner(100), a);
		EXPECT_EQ(table.Owner(101), nullptr) << "拓扑空洞没有主";
		EXPECT_EQ(table.Owner(199), nullptr);
		EXPECT_EQ(*table.Owner(200), b);
		EXPECT_EQ(*table.Owner(16383), b);
		EXPECT_EQ(table.Owner(16384), nullptr);
		EXPECT_TRUE(table.IsMaster(a));
		EXPECT_TRUE(table.IsMaster(b));
		EXPECT_FALSE(table.IsMaster(c));

		table.ApplyMoved(50, c);
		EXPECT_EQ(*table.Owner(50), c);
		EXPECT_EQ(*table.Owner(49), a) << "MOVED 只改这一个 slot";
		EXPECT_EQ(*table.Owner(51), a);
		EXPECT_TRUE(table.IsMaster(c));

		// 整份拓扑替换之后,之前的归属与节点都不留痕迹。
		ClusterTopology next;
		next.ranges.push_back(SlotRange{0, 16383, b});
		table.ApplyTopology(next);
		EXPECT_EQ(*table.Owner(50), b);
		EXPECT_FALSE(table.IsMaster(a));
		EXPECT_FALSE(table.IsMaster(c));
		EXPECT_EQ(table.Masters().size(), 1u);
	}

	TEST(RedisClusterTopologyTest, OwnerTableAssignAllForStandalone)
	{
		const NodeEndpoint a{"10.0.0.1", 7000};
		SlotOwnerTable table;
		table.AssignAll(a);
		EXPECT_EQ(*table.Owner(0), a);
		EXPECT_EQ(*table.Owner(16383), a);
		EXPECT_EQ(table.Masters().size(), 1u);
	}

	TEST(RedisClusterTopologyTest, RefreshPolicyLimitsRateAndKeepsPeriod)
	{
		TopologyRefreshPolicy policy(30s, 1s);
		EXPECT_FALSE(policy.ShouldStart(kT0)) << "没有请求、也没开始过,不刷新";

		policy.Request();
		EXPECT_TRUE(policy.ShouldStart(kT0));
		policy.OnStarted(kT0);
		EXPECT_FALSE(policy.ShouldStart(kT0 + 2s)) << "同一时刻只有一次在途";

		// 在途期间的请求不丢:那次应答可能早于触发它的事件。
		policy.Request();
		policy.OnFinished(true);
		EXPECT_FALSE(policy.ShouldStart(kT0 + 500ms)) << "限速:两次开始至少隔 1s";
		EXPECT_TRUE(policy.ShouldStart(kT0 + 1s));
		policy.OnStarted(kT0 + 1s);
		policy.OnFinished(true);

		EXPECT_FALSE(policy.ShouldStart(kT0 + 30s)) << "没有请求时只按周期刷新";
		EXPECT_TRUE(policy.ShouldStart(kT0 + 31s));
	}

	TEST(RedisClusterTopologyTest, RefreshPolicyRetriesAfterFailure)
	{
		TopologyRefreshPolicy policy(30s, 1s);
		policy.Request();
		policy.OnStarted(kT0);
		policy.OnFinished(false);
		EXPECT_FALSE(policy.ShouldStart(kT0 + 500ms));
		EXPECT_TRUE(policy.ShouldStart(kT0 + 1s));
	}

	TEST(RedisClusterTopologyTest, ReconnectJitterStaysWithinTheConfiguredWindow)
	{
		EXPECT_DOUBLE_EQ(JitteredReconnectIntervalSec(3.0, 1.0, 0.0), 3.0);
		EXPECT_DOUBLE_EQ(JitteredReconnectIntervalSec(3.0, 1.0, 0.5), 3.5);
		EXPECT_LT(JitteredReconnectIntervalSec(3.0, 1.0, 0.999999), 4.0);
		EXPECT_DOUBLE_EQ(JitteredReconnectIntervalSec(3.0, 0.0, 0.5), 3.0) << "抖动关闭";
		EXPECT_DOUBLE_EQ(JitteredReconnectIntervalSec(3.0, 1.0, 1.5), 3.0) << "越界的随机数收回区间";
		EXPECT_DOUBLE_EQ(JitteredReconnectIntervalSec(3.0, 1.0, -0.5), 3.0);
	}
}

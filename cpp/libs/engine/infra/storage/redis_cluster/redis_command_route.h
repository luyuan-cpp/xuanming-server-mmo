#pragma once

// 命令路由判定(docs/design/redis-player-store-cluster.md §5.2、§5.6),纯逻辑:解析 hiredis 格式化好的命令缓冲,
// 算出它该发往哪个 slot,或者当场拒绝。standalone 模式走同一套检查,开发环境就能拦下上线后才会炸的命令。
//
// 只认下面这些命令(按当前 C++ 代码实际用到的核对),其它一律 fail-closed:不知道键在哪,就不知道该发给谁。
// 新增命令在 .cpp 的 kCommandKeySpecs 里加一行。
//   键在第 1 个参数:GET SET HGET HSET HMGET HGETALL ZADD ZRANGE ZREM EXPIRE
//                    SPUBLISH(分片频道按频道名路由;普通 PUBLISH 不登记,Cluster 下它广播到全部节点,
//                    不随节点数扩展,docs/design/redis-cluster-all-domains-20261110.md §4.6)
//   全部参数都是键:DEL EXISTS MGET
//   按 numkeys 取键:EVAL EVALSHA
//   无键,必须显式给路由键:PING
//   发往任一已连接节点:SCRIPT LOAD
//
// 除了「哪些键必须带 tag」(HashTagPolicy),还按句柄所属的域检查 tag 的形状(KeyDomain,同上设计 §3.1):
// 控制集群的族 tag 不能发到玩家域句柄,玩家域的 tag 也不能发到控制集群句柄。

#include <cstddef>
#include <cstdint>
#include <iterator>
#include <optional>
#include <string_view>
#include <vector>

namespace redis_cluster
{
	// 解析 "*N\r\n" + N 个 "$len\r\n<bytes>\r\n"。返回的 string_view 指向入参缓冲;
	// 没有参数、长度对不上、结尾有多余字节都算格式错误,返回空。
	std::optional<std::vector<std::string_view>> ParseFormattedCommand(std::string_view formatted);

	enum class RouteKind : uint8_t
	{
		kSlot,       // 发往 slot 的主节点,走同 slot 停等
		kAnyNode,    // 发往任一已连接节点
		kRejected,   // 当场拒绝:返回 REDIS_ERR 并打 ERROR
	};

	enum class RouteRejectReason : uint8_t
	{
		kNone,
		kMalformedCommand,          // 不是合法的命令缓冲,或缺少必需的参数
		kUnknownCommand,            // 未登记的命令
		kBadNumKeys,                // EVAL / EVALSHA 的 numkeys 不是非负整数,或超过实际参数个数
		kKeylessNeedsRoutingKey,    // 无键命令没有显式给路由键
		kEmptyRoutingKey,           // 路由键是空串(CRC16 恰好算到 slot 0,不能让它悄悄路由过去)
		kMissingHashTag,            // 多键命令有键没带 hash tag
		kCrossSlot,                 // 多键命令的键不同 slot
		kRoutingKeyMismatch,        // 显式路由键与命令自身的键不同 slot
		kWrongDomainTag,            // 键或路由键的 tag 属于另一个 Redis 域(KeyDomain)
		kMixedTags,                 // 控制集群的多键命令带着不同的 tag(碰巧同 slot 也拒绝,不让代码依赖哈希巧合)

		kCount,
	};

	inline constexpr std::string_view kRouteRejectReasonNames[] = {
		"none",
		"malformed_command",
		"unknown_command",
		"bad_numkeys",
		"keyless_needs_routing_key",
		"empty_routing_key",
		"missing_hash_tag",
		"cross_slot",
		"routing_key_mismatch",
		"wrong_domain_tag",
		"mixed_tags",
	};
	static_assert(std::size(kRouteRejectReasonNames) == static_cast<std::size_t>(RouteRejectReason::kCount),
		"kRouteRejectReasonNames must list every RouteRejectReason");

	inline std::string_view RouteRejectReasonName(RouteRejectReason reason)
	{
		return kRouteRejectReasonNames[static_cast<std::size_t>(reason)];
	}

	struct CommandRoute
	{
		RouteKind kind = RouteKind::kRejected;
		uint16_t slot = 0;                                   // kind == kSlot 时有效
		RouteRejectReason reason = RouteRejectReason::kNone; // kind == kRejected 时有效
		std::string_view commandName;                        // 诊断用,指向入参缓冲
		std::string_view offendingKey;                       // 诊断用,指向入参缓冲或路由键
	};

	// 哪些键必须带 hash tag。
	enum class HashTagPolicy : uint8_t
	{
		kMultiKeyOnly,   // 只查多键命令:多键要同 slot,没有 tag 同不了
		kAllKeys,        // 单键命令与显式路由键也查。这是玩家域的接缝规则(设计 §5.1):控制域的键发到玩家域句柄当场拒绝。
		                 // 单实例开发时两个句柄指向同一个实例,不查的话这种错要到上集群才暴露,而且表现是读写落在两边
	};

	// 键属于哪个 Redis 域,按 hash tag 的形状判(redis_slot::IsControlTag),与 Go 的 clusterredis.KeyPolicy 同一口径。
	// 与 HashTagPolicy 正交:HashTagPolicy 管「哪些键必须带 tag」,KeyDomain 管「带着的 tag 必须是哪一种」。不带 tag 的键不归它管。
	enum class KeyDomain : uint8_t
	{
		kUnrestricted,   // 不看 tag 的形状(缺省)
		kPlayer,         // 玩家域与登录域:tag 不得是控制集群的族 tag。与 HashTagPolicy::kAllKeys 合用
		kControl,        // 控制集群:tag 只能是登记过的族 tag;多键命令的每个键都带 tag,且是同一个 tag。与 kMultiKeyOnly 合用
	};

	// routingKey 为空表示按命令自身的键路由(command());非空表示调用方显式指定(commandForKey())。
	// 带键的命令同样过多键检查,且路由键必须与键同 slot、同样按域检查,显式路由不能绕开守卫。
	CommandRoute ResolveCommandRoute(std::string_view formatted, std::optional<std::string_view> routingKey = std::nullopt,
		HashTagPolicy tagPolicy = HashTagPolicy::kMultiKeyOnly, KeyDomain keyDomain = KeyDomain::kUnrestricted);
}

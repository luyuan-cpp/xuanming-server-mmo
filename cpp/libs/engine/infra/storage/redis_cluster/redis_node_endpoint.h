#pragma once

// Redis 节点端点:IP 字面量 + 端口(docs/design/redis-player-store-cluster.md §5.6)。
// 运行期不做阻塞 DNS,所以路由层只接受 IP 字面量;主机名一律视为无效,由调用方按「这条路由不可用」处理。
// 只做纯计算,无 I/O。

#include <charconv>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <optional>
#include <string>
#include <string_view>
#include <system_error>

namespace redis_cluster
{
	struct NodeEndpoint
	{
		std::string ip;
		uint16_t port = 0;

		bool operator==(const NodeEndpoint&) const = default;

		bool IsIpv6() const { return ip.find(':') != std::string::npos; }

		// 只用于日志与展示,不是解析的输入。
		std::string ToString() const { return ip + ":" + std::to_string(port); }
	};

	struct NodeEndpointHash
	{
		std::size_t operator()(const NodeEndpoint& endpoint) const
		{
			return std::hash<std::string>{}(endpoint.ip) ^ (static_cast<std::size_t>(endpoint.port) << 1);
		}
	};

	namespace detail
	{
		// 严格的点分十进制:恰好 4 段,每段 0-255,不接受前导零(010 在不同解析器里可能是八进制)。
		inline bool IsIpv4Literal(std::string_view text)
		{
			int parts = 0;
			std::size_t pos = 0;
			for (;;)
			{
				const std::size_t dot = text.find('.', pos);
				const std::string_view part =
					dot == std::string_view::npos ? text.substr(pos) : text.substr(pos, dot - pos);
				if (part.empty() || part.size() > 3 || (part.size() > 1 && part.front() == '0'))
				{
					return false;
				}
				int value = 0;
				for (const char c : part)
				{
					if (c < '0' || c > '9')
					{
						return false;
					}
					value = value * 10 + (c - '0');
				}
				if (value > 255)
				{
					return false;
				}
				++parts;
				if (dot == std::string_view::npos)
				{
					break;
				}
				pos = dot + 1;
			}
			return parts == 4;
		}

		// 这里的目的只是排除主机名(主机名不含冒号),不是完整的 IPv6 语法校验;写法非法时由建连处的 inet_pton 报错。
		inline bool IsIpv6Literal(std::string_view text)
		{
			int colons = 0;
			for (const char c : text)
			{
				const bool hex = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
				if (c == ':')
				{
					++colons;
				}
				else if (!hex && c != '.')
				{
					return false;
				}
			}
			return colons >= 2;
		}
	}

	inline bool IsIpLiteral(std::string_view host)
	{
		return detail::IsIpv4Literal(host) || detail::IsIpv6Literal(host);
	}

	inline std::optional<NodeEndpoint> MakeNodeEndpoint(std::string_view host, long long port)
	{
		if (!IsIpLiteral(host) || port <= 0 || port > 65535)
		{
			return std::nullopt;
		}
		return NodeEndpoint{std::string(host), static_cast<uint16_t>(port)};
	}

	struct HostPort
	{
		std::string_view host;   // 指向入参缓冲。可能是主机名,要不要解析由调用方决定
		uint16_t port = 0;
	};

	// 拆 "host:port":host 非空,端口 1-65535。端口取最后一个冒号之后,所以不带方括号的 IPv6 也能拆(Redis 的 MOVED 就是这种写法)。
	inline std::optional<HostPort> SplitHostPort(std::string_view hostPort)
	{
		const std::size_t colon = hostPort.rfind(':');
		if (colon == std::string_view::npos || colon == 0)
		{
			return std::nullopt;
		}
		const std::string_view portText = hostPort.substr(colon + 1);
		long long port = 0;
		const char* first = portText.data();
		const char* last = first + portText.size();
		const std::from_chars_result parsed = std::from_chars(first, last, port);
		if (parsed.ec != std::errc() || parsed.ptr != last || port <= 0 || port > 65535)
		{
			return std::nullopt;
		}
		return HostPort{hostPort.substr(0, colon), static_cast<uint16_t>(port)};
	}

	// 解析运行期拿到的 "host:port"(MOVED、CLUSTER SLOTS),host 必须是 IP 字面量。
	inline std::optional<NodeEndpoint> ParseNodeEndpoint(std::string_view hostPort)
	{
		const std::optional<HostPort> parts = SplitHostPort(hostPort);
		if (!parts)
		{
			return std::nullopt;
		}
		return MakeNodeEndpoint(parts->host, parts->port);
	}
}

#pragma once

// 发布轨道(stable / canary)的 C++ 定义,以及客户端请求链路上按轨道选实例的规则
// (docs/design/release-track-gray-canary.md §2.1、§2.4)。
//
// 轨道是进程的部署属性:部署经环境变量 MMORPG_RELEASE_TRACK 注入,不设等于 stable,进程存续期间不变。
// 节点在首次发布 NodeInfo 之前解析一次(Node::InitRpcServer),取值非法就拒启,所以注册进 etcd 的
// NodeInfo.release_track 只会是 "stable" 或 "canary";别的节点没写这个字段(空串)也按 stable 算。
//
// 选实例的规则(Select):
//   - 调用方是 canary:候选里有 canary 就只在 canary 里选,没有就退回 stable;
//   - 调用方是 stable:只在 stable 里选,永远不进 canary。
// 只用在客户端请求链路上(gate 选路由服);zone 内类型按 zone 选,服务间调用只连 stable(设计文档 §2.5)。
//
// Go 的同一份规则在 go/shared/releasetrack;两边单测用同一组样例
// (cpp/tests/client_endpoint_test/release_track_test.cpp 与 go/shared/releasetrack/releasetrack_test.go)。
//
// 纯头文件,不依赖 muduo / proto,可在启动早期调用。

#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <iterator>
#include <optional>
#include <string_view>
#include <vector>

namespace release_track
{
	enum class Track : uint8_t
	{
		kStable,
		kCanary,
		kCount,
	};

	// 写进 NodeInfo.release_track 与 ForwardRequest.release_track 的取值,下标即 Track。
	inline constexpr std::string_view kTrackNames[] = {
		"stable",
		"canary",
	};
	static_assert(std::size(kTrackNames) == static_cast<std::size_t>(Track::kCount),
				  "kTrackNames must name every Track");

	// 部署注入本进程轨道的环境变量名;Go 服务读同一个名字。
	inline constexpr const char *kEnvVar = "MMORPG_RELEASE_TRACK";

	// 解析一个轨道取值。空串是 stable;其余只认 "stable"、"canary",区分大小写、不去空白:
	// 部署写出的值必须原样就是这两个之一,宽松匹配会让拼错的配置悄悄落到某条轨道上。认不出返回 nullopt。
	inline std::optional<Track> Parse(std::string_view raw)
	{
		if (raw.empty())
		{
			return Track::kStable;
		}
		for (std::size_t index = 0; index < std::size(kTrackNames); ++index)
		{
			if (raw == kTrackNames[index])
			{
				return static_cast<Track>(index);
			}
		}
		return std::nullopt;
	}

	// 轨道的取值名。kCount 不是合法轨道,返回一个 Parse 不认的串,写出去会被消费方拒掉,不会被当成 stable。
	inline std::string_view Name(Track track)
	{
		const auto index = static_cast<std::size_t>(track);
		return index < std::size(kTrackNames) ? kTrackNames[index] : std::string_view{"invalid-track"};
	}

	// 读本进程的轨道:环境变量不设或为空是 stable;取值非法返回 nullopt,调用方必须拒启。
	// 带着认不出的轨道注册,节点会被所有选点方当作无效节点跳过,不报错,却也收不到流量。
	inline std::optional<Track> FromEnv()
	{
		const char *raw = std::getenv(kEnvVar);
		return Parse(raw == nullptr ? std::string_view{} : std::string_view{raw});
	}

	// 按文件头的规则,从 candidates 里留下调用方 caller 能用的那部分,保持原有相对顺序;不改 candidates。
	// trackOf(candidate) 返回候选的轨道,必须是 Parse 成功的值:轨道无效的候选应在调用之前就剔掉。
	template <typename T, typename TrackOf>
	std::vector<T> Select(Track caller, const std::vector<T> &candidates, TrackOf trackOf)
	{
		const auto withTrack = [&candidates, &trackOf](Track want)
		{
			std::vector<T> kept;
			kept.reserve(candidates.size());
			for (const auto &candidate : candidates)
			{
				if (trackOf(candidate) == want)
				{
					kept.push_back(candidate);
				}
			}
			return kept;
		};
		if (caller == Track::kCanary)
		{
			if (auto canary = withTrack(Track::kCanary); !canary.empty())
			{
				return canary;
			}
		}
		return withTrack(Track::kStable);
	}
}

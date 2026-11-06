#include <gtest/gtest.h>

#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "node/system/node/release_track.h"

// release_track 的单元测试(docs/design/release-track-gray-canary.md §2.1、§2.4)。
//
// 下面两张样例表与 Go go/shared/releasetrack/releasetrack_test.go 逐行相同,两边的解析与选点规则
// 必须给出同样的结果。改一边时同步改另一边。读环境变量的 FromEnv 只是对 Parse 的一层包装,
// 拒启本身(FatalStartupConfig)不在单测里跑,由交付说明里的负例冒烟验证。
// 和 client_endpoint、open_file_limit 同在一个工程:都是节点启动期的纯判定。

using release_track::Name;
using release_track::Parse;
using release_track::Select;
using release_track::Track;

TEST(ReleaseTrackParse, Samples)
{
	EXPECT_EQ(std::optional<Track>(Track::kStable), Parse(""));
	EXPECT_EQ(std::optional<Track>(Track::kStable), Parse("stable"));
	EXPECT_EQ(std::optional<Track>(Track::kCanary), Parse("canary"));
	EXPECT_EQ(std::nullopt, Parse("Canary"));
	EXPECT_EQ(std::nullopt, Parse("STABLE"));
	EXPECT_EQ(std::nullopt, Parse(" stable"));
	EXPECT_EQ(std::nullopt, Parse("stable\n"));
	EXPECT_EQ(std::nullopt, Parse("beta"));
}

TEST(ReleaseTrackParse, NameRoundTrips)
{
	for (const auto track : {Track::kStable, Track::kCanary})
	{
		EXPECT_EQ(std::optional<Track>(track), Parse(Name(track))) << Name(track);
	}
}

// kCount 不是合法轨道:它的名字不能是 "stable",否则写出去会被当成 stable。
TEST(ReleaseTrackParse, CountIsNotAValidTrack)
{
	EXPECT_EQ(std::nullopt, Parse(Name(Track::kCount)));
}

namespace
{
	struct Candidate
	{
		std::string name;
		Track track;
	};

	std::vector<std::string> Names(const std::vector<Candidate> &candidates)
	{
		std::vector<std::string> names;
		for (const auto &candidate : candidates)
		{
			names.push_back(candidate.name);
		}
		return names;
	}

	std::vector<std::string> SelectNames(Track caller, const std::vector<Candidate> &candidates)
	{
		return Names(Select(caller, candidates, [](const Candidate &candidate) { return candidate.track; }));
	}

	const Candidate s1{"s1", Track::kStable};
	const Candidate s2{"s2", Track::kStable};
	const Candidate c1{"c1", Track::kCanary};
	const Candidate c2{"c2", Track::kCanary};
}

TEST(ReleaseTrackSelect, CanaryCallerUsesOnlyCanaryWhenPresent)
{
	EXPECT_EQ((std::vector<std::string>{"c1", "c2"}), SelectNames(Track::kCanary, {s1, c1, s2, c2}));
}

TEST(ReleaseTrackSelect, CanaryCallerFallsBackToStable)
{
	EXPECT_EQ((std::vector<std::string>{"s1", "s2"}), SelectNames(Track::kCanary, {s1, s2}));
}

TEST(ReleaseTrackSelect, StableCallerUsesOnlyStable)
{
	EXPECT_EQ((std::vector<std::string>{"s1", "s2"}), SelectNames(Track::kStable, {s1, c1, s2}));
}

TEST(ReleaseTrackSelect, StableCallerNeverEntersCanary)
{
	EXPECT_TRUE(SelectNames(Track::kStable, {c1, c2}).empty());
}

TEST(ReleaseTrackSelect, NoCandidates)
{
	EXPECT_TRUE(SelectNames(Track::kCanary, {}).empty());
	EXPECT_TRUE(SelectNames(Track::kStable, {}).empty());
}

TEST(ReleaseTrackSelect, DoesNotModifyCandidates)
{
	const std::vector<Candidate> candidates{s1, c1};
	(void)SelectNames(Track::kCanary, candidates);
	EXPECT_EQ((std::vector<std::string>{"s1", "c1"}), Names(candidates));
}

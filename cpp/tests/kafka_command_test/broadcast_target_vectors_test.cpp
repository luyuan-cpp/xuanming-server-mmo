#include <gtest/gtest.h>

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <optional>
#include <string>
#include <string_view>
#include <system_error>
#include <utility>
#include <vector>

#include <yaml-cpp/yaml.h>

#include "network/broadcast_target_codec.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/gate/gate_service.pb.h"

// ---------------------------------------------------------------------------
// broadcast_target_vectors_test — 指名广播 (session_id, player_id) 编码的跨语言契约(C++ 侧)。
// 契约本体见 cpp/libs/engine/core/network/broadcast_target_codec.h 文件头。
//
// Go 生产者(go/shared/kafkautil 的 EncodeBroadcastTargets)把目标编进 BroadcastToPlayersEvent,gate 用
// broadcast_target_codec.h 的 ForEach 解码;player_list[i] 按位置对应「按 session_id 升序」的第 i 个目标,
// 两侧错开一格就是把每个人的身份都比对成别人的,编译器与集成测试都看不见。所以两侧读同一份向量
// go/shared/kafkautil/testdata/broadcast_target_vectors.json(Go 侧 TestEncodeBroadcastTargetsMatchesSharedVectors
// 拿真实实现对拍):
//   1) gate 解码:按向量的 expect 构造 Kafka 事件,ForEach 出的 (session, player) 对必须等于 targets
//      按 session 稳定升序、同 session 保留第一个之后的结果;
//   2) scene 编码:C++ Encode(targets) 必须逐字段等于 expect —— scene 与 Go 两个编码器由此钉在同一份输出上。
//
// 确定、可重复:只读向量文件、只调纯函数。向量对不上时以机器结果为准回报,不直接改向量文件。
// main() 在 kafka_command_test.cpp 里,本文件不再定义。
// ---------------------------------------------------------------------------

namespace {

namespace fs = std::filesystem;

constexpr const char *kBroadcastVectorRelativePath =
    "go/shared/kafkautil/testdata/broadcast_target_vectors.json";

// 从若干起点逐级向上(最多 8 层)找向量文件:
//   - 当前工作目录:main() 找到配置后会把 cwd 换到 bin/(仓库根的下一级);
//   - 本源文件所在目录(MSBuild 以绝对路径传源文件,__FILE__ 即绝对路径):cwd 不在仓库内时兜底。
fs::path FindBroadcastVectorFile()
{
    std::vector<fs::path> startPoints;
    std::error_code cwdEc;
    const fs::path cwd = fs::current_path(cwdEc);
    if (!cwdEc)
    {
        startPoints.push_back(cwd);
    }
    const fs::path sourceDir = fs::path(__FILE__).parent_path();
    if (sourceDir.is_absolute())
    {
        startPoints.push_back(sourceDir);
    }

    for (const auto &start : startPoints)
    {
        fs::path dir = start;
        for (int depth = 0; depth < 8; ++depth)
        {
            const fs::path candidate = dir / kBroadcastVectorRelativePath;
            std::error_code existsEc;
            if (fs::exists(candidate, existsEc))
            {
                return candidate;
            }
            if (!dir.has_parent_path() || dir.parent_path() == dir)
            {
                break;
            }
            dir = dir.parent_path();
        }
    }
    return {};
}

using TargetPair = std::pair<uint32_t, uint64_t>;

struct BroadcastVector
{
    std::string name;
    std::vector<broadcast_targets::Target> targets;
    // expect:Go 编码器的输出(四个目标字段)。
    std::vector<uint32_t> sessionList;
    uint32_t bitmapBase = 0;
    std::string bitmap; // 已从小写十六进制还原成字节
    std::vector<uint64_t> playerList;
};

// 契约是小写十六进制;奇数长度或非法字符返回空(向量文件坏了)。
std::optional<std::string> DecodeLowerHex(std::string_view hex)
{
    if (hex.size() % 2 != 0)
    {
        return std::nullopt;
    }
    const auto nibble = [](char c) -> int
    {
        if (c >= '0' && c <= '9')
        {
            return c - '0';
        }
        if (c >= 'a' && c <= 'f')
        {
            return c - 'a' + 10;
        }
        return -1;
    };
    std::string bytes;
    bytes.reserve(hex.size() / 2);
    for (std::size_t i = 0; i < hex.size(); i += 2)
    {
        const int high = nibble(hex[i]);
        const int low = nibble(hex[i + 1]);
        if (high < 0 || low < 0)
        {
            return std::nullopt;
        }
        bytes.push_back(static_cast<char>((high << 4) | low));
    }
    return bytes;
}

// JSON 的 [] 与 null(Go 的 nil 切片)、缺键都当空列表;其他形状说明向量文件坏了,返回 false。
template <typename T>
bool ReadList(const YAML::Node &node, std::vector<T> &out)
{
    out.clear();
    if (!node || node.IsNull())
    {
        return true;
    }
    if (!node.IsSequence())
    {
        return false;
    }
    for (const auto &item : node)
    {
        out.push_back(item.as<T>());
    }
    return true;
}

// 读整份向量文件。找不到文件或形状不对时记失败并返回已读到的部分;调用方据空列表 ASSERT。
std::vector<BroadcastVector> LoadBroadcastVectors()
{
    const fs::path vectorFile = FindBroadcastVectorFile();
    if (vectorFile.empty())
    {
        ADD_FAILURE() << "找不到共享向量文件 " << kBroadcastVectorRelativePath
                      << ";它是 C++/Go 广播目标编码对拍的唯一凭据,缺了就等于没测。";
        return {};
    }

    const YAML::Node root = YAML::LoadFile(vectorFile.string());
    const YAML::Node vectors = root["vectors"];
    if (!vectors || !vectors.IsSequence())
    {
        ADD_FAILURE() << "向量文件缺 vectors 数组: " << vectorFile.string();
        return {};
    }

    std::vector<BroadcastVector> out;
    std::size_t index = 0;
    for (const auto &node : vectors)
    {
        BroadcastVector entry;
        entry.name = node["name"] ? node["name"].as<std::string>() : "#" + std::to_string(index);
        ++index;

        const YAML::Node targets = node["targets"];
        if (!targets || !targets.IsSequence())
        {
            ADD_FAILURE() << "vector " << entry.name << " 缺 targets 数组";
            continue;
        }
        for (const auto &target : targets)
        {
            entry.targets.push_back(
                broadcast_targets::Target{target["session_id"].as<uint32_t>(), target["player_id"].as<uint64_t>()});
        }

        const YAML::Node expect = node["expect"];
        if (!expect || !expect.IsMap())
        {
            ADD_FAILURE() << "vector " << entry.name << " 缺 expect";
            continue;
        }
        if (!ReadList(expect["session_list"], entry.sessionList) || !ReadList(expect["player_list"], entry.playerList))
        {
            ADD_FAILURE() << "vector " << entry.name << " 的 session_list / player_list 不是数组";
            continue;
        }
        const YAML::Node base = expect["session_bitmap_base"];
        entry.bitmapBase = (base && !base.IsNull()) ? base.as<uint32_t>() : 0;
        const YAML::Node hex = expect["session_bitmap_hex"];
        const auto bitmap = DecodeLowerHex((hex && !hex.IsNull()) ? hex.as<std::string>() : std::string{});
        if (!bitmap)
        {
            ADD_FAILURE() << "vector " << entry.name << " 的 session_bitmap_hex 不是小写十六进制";
            continue;
        }
        entry.bitmap = *bitmap;
        out.push_back(std::move(entry));
    }
    return out;
}

// 契约里「第 i 个目标」的定义:按 session 稳定升序,同一 session 只保留输入里的第一个。
// 故意不调 Encode 来算期望 —— 被测的正是它的排序与去重,独立写一遍才算对照。
std::vector<TargetPair> ContractOrder(const std::vector<broadcast_targets::Target> &targets)
{
    std::vector<TargetPair> pairs;
    pairs.reserve(targets.size());
    for (const auto &target : targets)
    {
        pairs.emplace_back(target.sessionId, target.playerId);
    }
    std::stable_sort(pairs.begin(), pairs.end(),
                     [](const TargetPair &a, const TargetPair &b) { return a.first < b.first; });
    pairs.erase(std::unique(pairs.begin(), pairs.end(),
                            [](const TargetPair &a, const TargetPair &b) { return a.first == b.first; }),
                pairs.end());
    return pairs;
}

} // namespace

// ---------------------------------------------------------------------------
// 1. gate 解码 Go 编码的事件
// ---------------------------------------------------------------------------

TEST(BroadcastTargetVectors, GateDecodesGoEncodedPairs)
{
    const auto vectors = LoadBroadcastVectors();
    ASSERT_FALSE(vectors.empty()) << "没有可用的向量";

    std::size_t bitmapForms = 0;
    std::size_t listForms = 0;
    std::size_t withDuplicateSessions = 0;
    for (const auto &entry : vectors)
    {
        SCOPED_TRACE("vector=" + entry.name);
        EXPECT_TRUE(entry.sessionList.empty() || entry.bitmap.empty())
            << "两种形态二选一:session_list 与 bitmap 不能同时出现";

        contracts::kafka::BroadcastToPlayersEvent event;
        for (const auto sessionId : entry.sessionList)
        {
            event.add_session_list(sessionId);
        }
        event.set_session_bitmap_base(entry.bitmapBase);
        event.set_session_bitmap(entry.bitmap);
        for (const auto playerId : entry.playerList)
        {
            event.add_player_list(playerId);
        }

        EXPECT_TRUE(broadcast_targets::PlayerListAligned(event))
            << "Go 编出的 player_list 与目标数不齐,gate 会整条拒绝";

        std::vector<TargetPair> decoded;
        broadcast_targets::ForEach(event, [&decoded](uint32_t sessionId, uint64_t playerId)
                                   { decoded.emplace_back(sessionId, playerId); });
        const auto expected = ContractOrder(entry.targets);
        EXPECT_EQ(decoded, expected)
            << "gate 解出的 (session, player) 对与契约顺序不一致 —— 身份会被比对成别人的";

        if (entry.bitmap.empty())
        {
            ++listForms;
        }
        else
        {
            ++bitmapForms;
        }
        if (expected.size() != entry.targets.size())
        {
            ++withDuplicateSessions;
        }
    }

    // 覆盖面:两种形态与重复 session 都必须出现,否则这份对拍只证明了契约的一部分。
    EXPECT_GT(bitmapForms, 0u) << "向量缺 bitmap 形态(去重后 ≥ 32 个密集目标)";
    EXPECT_GT(listForms, 0u) << "向量缺 session_list 形态";
    EXPECT_GT(withDuplicateSessions, 0u) << "向量缺重复 session 的用例";
}

// ---------------------------------------------------------------------------
// 2. scene 的 C++ 编码器与 Go 编码器产出同一份字段
// ---------------------------------------------------------------------------

// 两边是同一条契约的两个实现:同阈值、同体积判据、同去重规则。用 RPC 请求类型编码 —— 那是 scene 实际发给
// gate 的消息;编解码模板对两种消息是同一份实现,Kafka 事件的同型断言见 routing_identity_test。
TEST(BroadcastTargetVectors, SceneEncoderMatchesGoEncoder)
{
    const auto vectors = LoadBroadcastVectors();
    ASSERT_FALSE(vectors.empty()) << "没有可用的向量";

    for (const auto &entry : vectors)
    {
        SCOPED_TRACE("vector=" + entry.name);
        auto targets = entry.targets;
        BroadcastToPlayersRequest request;
        broadcast_targets::Encode(request, targets);

        EXPECT_EQ(std::vector<uint32_t>(request.session_list().begin(), request.session_list().end()),
                  entry.sessionList);
        EXPECT_EQ(request.session_bitmap_base(), entry.bitmapBase);
        EXPECT_EQ(request.session_bitmap(), entry.bitmap);
        EXPECT_EQ(std::vector<uint64_t>(request.player_list().begin(), request.player_list().end()),
                  entry.playerList);
    }
}

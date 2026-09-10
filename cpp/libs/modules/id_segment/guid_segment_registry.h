#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "guid_segment_client.h"

// ─────────────────────────────────────────────────────────────────────────
// 永久 guid 种类注册表:一种 GUID 一个 GuidSegmentClient 实例(§7.5 第 3 条)。
//
// 加一种永久 guid(pet / guild …)只要两处:
//   1. 下面的 GuidKind 加一个枚举项,并在 kGuidKindNames(.cpp)里加对应的一行名字
//      —— 两者数量不一致会被 static_assert 当场拦下,漏改编译不过;
//   2. bin/etc/base_deploy_config.yaml 的 IdSegments 里加一块(Kind: pet + step 三元组),
//      以及 go 侧 id_segment 表的一行种子(迁移显式创建,生产不自动补种,§7.5 第 7 条)。
// 业务侧用 tlsGuidSegmentRegistry.Get(GuidKind::kPet).TryNext(out) 取号即可。
//
// 名字既是注册表里的种类名,也是 id_segment.biz_tag —— 两边同一个字符串,免得配置里再对一遍表。
//
// 底层类型 uint8_t 数的是**种类个数**(现在 3 种),不是 guid 的位宽 ——
// guid 的值是 Guid(uint64),见 guid_segment_client.h,二者无关。
// 它只用来给种类编号、索引下面按种类排的数组;纯内存使用,不进协议不落库。
// kCount 必须始终是最后一项:它既是种类数,也是所有按种类索引的数组的长度。
// ─────────────────────────────────────────────────────────────────────────
enum class GuidKind : uint8_t
{
    kItem,
    kTxLog,
    kSnapshot,

    kCount
};

constexpr std::size_t kGuidKindCount = static_cast<std::size_t>(GuidKind::kCount);

// 种类名 = biz_tag。越界返回 "?"。
const char *GuidKindName(GuidKind kind);

// 按名字(配置里的 Kind 字段)找种类;不认识返回 false 且不改 out。
bool ParseGuidKind(std::string_view name, GuidKind &out);

class GuidSegmentRegistry
{
public:
    GuidSegmentRegistry() = default;
    GuidSegmentRegistry(const GuidSegmentRegistry &) = delete;
    GuidSegmentRegistry &operator=(const GuidSegmentRegistry &) = delete;

    [[nodiscard]] GuidSegmentClient &Get(GuidKind kind);
    [[nodiscard]] const GuidSegmentClient &Get(GuidKind kind) const;

    template <typename Fn>
    void ForEach(Fn &&fn)
    {
        for (std::size_t i = 0; i < kGuidKindCount; ++i)
        {
            fn(static_cast<GuidKind>(i), clients_[i]);
        }
    }

    // 已启用的种类数(配置里 Enabled=true 且 Enable 成功的)。
    [[nodiscard]] std::size_t EnabledCount() const;

    // 每个**已启用**的种类都至少领到了一段。没启用的种类不算 —— 它们按设计永远 fail-closed,
    // 不该拦住节点启动;一种都没启用时恒 true(scene main 会另外 WARN)。
    // scene 的 DependencyGate 条件 "id segments ready" 就是它。
    [[nodiscard]] bool AllEnabledReady() const;

    // "item,snapshot" 这样列出已启用但还没段的种类;都就绪返回空串。启动等待日志用。
    [[nodiscard]] std::string DescribeNotReady() const;

    // 对每个已启用的实例调 Warm(幂等)。DataService 节点连上 / SetAfterStart 时各催一次。
    void WarmAll();

    // 关机:全部停止续段,手里的号照常发完。
    void ShutdownAll();

    // 全部回到未启用。单测隔离用;生产不调。
    void ResetAll();

private:
    std::array<GuidSegmentClient, kGuidKindCount> clients_;
};

// 每线程一份(与旧 tlsSnowflakeManager 同形态):只有 scene 的主循环线程启用其中的实例。
extern thread_local GuidSegmentRegistry tlsGuidSegmentRegistry;

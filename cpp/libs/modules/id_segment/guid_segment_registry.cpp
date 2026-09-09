#include "guid_segment_registry.h"

#include "muduo/base/Logging.h"

thread_local GuidSegmentRegistry tlsGuidSegmentRegistry;

namespace
{
    constexpr const char *kGuidKindNames[] = {
#define GUID_SEGMENT_KIND_NAME_ENTRY(name, tag) tag,
        GUID_SEGMENT_KIND_LIST(GUID_SEGMENT_KIND_NAME_ENTRY)
#undef GUID_SEGMENT_KIND_NAME_ENTRY
    };
    static_assert(sizeof(kGuidKindNames) / sizeof(kGuidKindNames[0]) == kGuidKindCount,
                  "GUID_SEGMENT_KIND_LIST drives both the enum and the name table");

    std::size_t IndexOf(GuidKind kind)
    {
        const auto index = static_cast<std::size_t>(kind);
        if (index >= kGuidKindCount)
        {
            // 只可能来自把整数硬转成 GuidKind 的代码错误;越界访问数组比崩溃更难查。
            LOG_FATAL << "[idsegment] GuidKind out of range: " << index;
        }
        return index;
    }
} // namespace

const char *GuidKindName(GuidKind kind)
{
    const auto index = static_cast<std::size_t>(kind);
    return index < kGuidKindCount ? kGuidKindNames[index] : "?";
}

bool ParseGuidKind(std::string_view name, GuidKind &out)
{
    for (std::size_t i = 0; i < kGuidKindCount; ++i)
    {
        if (name == kGuidKindNames[i])
        {
            out = static_cast<GuidKind>(i);
            return true;
        }
    }
    return false;
}

GuidSegmentClient &GuidSegmentRegistry::Get(GuidKind kind)
{
    return clients_[IndexOf(kind)];
}

const GuidSegmentClient &GuidSegmentRegistry::Get(GuidKind kind) const
{
    return clients_[IndexOf(kind)];
}

std::size_t GuidSegmentRegistry::EnabledCount() const
{
    std::size_t count = 0;
    for (const auto &client : clients_)
    {
        count += client.IsEnabled() ? 1 : 0;
    }
    return count;
}

bool GuidSegmentRegistry::AllEnabledReady() const
{
    for (const auto &client : clients_)
    {
        if (client.IsEnabled() && !client.IsReady())
        {
            return false;
        }
    }
    return true;
}

std::string GuidSegmentRegistry::DescribeNotReady() const
{
    std::string out;
    for (std::size_t i = 0; i < kGuidKindCount; ++i)
    {
        const auto &client = clients_[i];
        if (client.IsEnabled() && !client.IsReady())
        {
            if (!out.empty())
            {
                out += ",";
            }
            out += kGuidKindNames[i];
        }
    }
    return out;
}

void GuidSegmentRegistry::WarmAll()
{
    for (auto &client : clients_)
    {
        client.Warm();
    }
}

void GuidSegmentRegistry::ShutdownAll()
{
    for (auto &client : clients_)
    {
        client.Shutdown();
    }
}

void GuidSegmentRegistry::ResetAll()
{
    for (auto &client : clients_)
    {
        client.Reset();
    }
}

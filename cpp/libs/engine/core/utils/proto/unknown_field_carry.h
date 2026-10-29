#pragma once

#include <string>
#include <string_view>

#include <google/protobuf/descriptor.h>
#include <google/protobuf/message.h>
#include <google/protobuf/unknown_field_set.h>

#include "persist_schema/persist_schema_gen.h" // IsRetiredField:台账退役字段(生成产物)

// 未知字段跟着对象走(docs/design/mixed-version-persist-field-protection.md §4.2)。
//
// 新版本给持久消息加的字段,旧版本解析时落进消息本层的 UnknownFieldSet。直接以 PB 形式存放的组件(emplace 拷贝、
// CopyFrom 带回)天然保留它;由手写代码把 PB 拆成运行时结构、存盘时再逐字段拼回的地方,要用这一对函数把本层未知字段
// 存在运行时对象里,存盘时并回去。对象被删除或重建,未知字段跟着消失 —— 这是 protobuf 本身的语义,不会把旧对象的
// 新字段「复活」到同键的新对象上(设计 §4.1)。
//
// 存成序列化字节而不是 UnknownFieldSet:后者不可拷贝,而运行时结构要能放进 unordered_map、被拷贝。
namespace unknown_field_carry
{

// 取出消息本层(不含子消息)的未知字段;没有时返回空串,平时不占内存。
[[nodiscard]] inline std::string Capture(const google::protobuf::Message &message)
{
    const auto &unknown = message.GetReflection()->GetUnknownFields(message);
    std::string bytes;
    if (!unknown.empty())
    {
        (void)unknown.SerializeToString(&bytes);
    }
    return bytes;
}

// 先解析、后写入:调用方可以在动消息之前确认携带的字节完好。返回 false = 字节解析失败(只可能是内存被破坏)。
[[nodiscard]] inline bool Parse(const std::string &bytes, google::protobuf::UnknownFieldSet &out)
{
    out.Clear();
    return bytes.empty() || out.ParseFromString(bytes);
}

// 用携带的未知字段替换消息本层的未知字段。本层原有的先清掉:携带的这份就是本层的全部未知字段,
// 并入两份会让重复字段翻倍。
inline void Replace(const google::protobuf::UnknownFieldSet &carried, google::protobuf::Message &message)
{
    auto *unknown = message.GetReflection()->MutableUnknownFields(&message);
    unknown->Clear();
    unknown->MergeFrom(carried);
}

// Parse + Replace。返回 false 时消息不动,调用方必须拒绝这次写入。
[[nodiscard]] inline bool Restore(const std::string &bytes, google::protobuf::Message &message)
{
    google::protobuf::UnknownFieldSet parsed;
    if (!Parse(bytes, parsed))
    {
        return false;
    }
    Replace(parsed, message);
    return true;
}

// 一组未知字段里有没有"更新版本才有的字段":字段号不在 messageFullName 这个类型的退役清单里。
// 退役字段(台账 retired 行)的残留字节在之后的每个版本里都是未知字段,照样原样携带,但它们不代表"有更新版本的
// 语义"(设计 §3.1)。只看"有没有未知字段",带残留的对象就会被永远当成更新版本的数据:永远不能合堆、合并、按数量扣除。
// isRetired(fieldNumber) 判字段号是否已退役;拆成参数只为单测能给出非空的退役清单(生产清单来自生成产物)。
template <typename IsRetired>
[[nodiscard]] bool ContainsUnretiredFields(const google::protobuf::UnknownFieldSet &unknown, IsRetired &&isRetired)
{
    for (int i = 0; i < unknown.field_count(); ++i)
    {
        if (!isRetired(unknown.field(i).number()))
        {
            return true;
        }
    }
    return false;
}

[[nodiscard]] inline bool ContainsNewerVersionFields(std::string_view messageFullName,
                                                     const google::protobuf::UnknownFieldSet &unknown)
{
    return ContainsUnretiredFields(unknown, [messageFullName](int fieldNumber)
                                   { return persist_schema::IsRetiredField(messageFullName, fieldNumber); });
}

// 消息本层是否带着更新版本才有的字段(本版本不认识、也不是已退役字段)。旧代码对这类对象不做合堆、合并、
// 按数量扣除等会改变新字段语义的操作(设计 §4.4)。守卫一律用它,不要直接看 UnknownFieldSet 是否为空。
[[nodiscard]] inline bool CarriesNewerVersionFields(const google::protobuf::Message &message)
{
    const auto &unknown = message.GetReflection()->GetUnknownFields(message);
    if (unknown.empty())
    {
        return false;
    }
    const auto fullName = message.GetDescriptor()->full_name();
    return ContainsNewerVersionFields(std::string_view(fullName.data(), fullName.size()), unknown);
}

// 同上,对象是运行时结构、本层未知字段按 Capture 存成了字节(例如欠款 CurrencyDebt.unknownFields)。
// messageFullName 是这些字节原属的消息类型全名。字节解析失败(只可能是内存被破坏)按"带着"处理:守卫拒绝即 fail-closed。
[[nodiscard]] inline bool CarriesNewerVersionFields(std::string_view messageFullName, const std::string &capturedBytes)
{
    if (capturedBytes.empty())
    {
        return false;
    }
    google::protobuf::UnknownFieldSet parsed;
    if (!Parse(capturedBytes, parsed))
    {
        return true;
    }
    return ContainsNewerVersionFields(messageFullName, parsed);
}

} // namespace unknown_field_carry

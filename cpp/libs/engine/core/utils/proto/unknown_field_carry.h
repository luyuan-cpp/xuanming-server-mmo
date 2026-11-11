#pragma once

// Windows SDK 的同名宏会改写 protobuf 反射方法；仅在本头内屏蔽，结束后恢复调用方环境。
#pragma push_macro("GetMessage")
#undef GetMessage

#include <string>
#include <string_view>
#include <vector>

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

// 整棵消息树里有没有更新版本才有的字段(2026-11-11,混版本设计 §12「载荷新字段」)。
//
// 给读进来、执行完就丢的命令载荷用:收件箱载荷、资产通道请求。它们不像持久对象那样让未知字段跟着对象走 ——
// 旧版本照常执行,新字段的语义就丢了,还会记成已生效。所以调用方见到 true 一律按版本错位处理:不执行、不记终态,
// 等落到认识这些字段的版本再执行。
//
// 逐层看每个已设置的子消息(含 repeated 与 map 的条目)。bytes 字段不往里解:命令里按设计原样携带的持久对象
// (资产包的 instance_proto)放在 bytes 里,它们的未知字段是要带走的数据,不是命令的新语义(§4.2)。
//
// 退役判据:字段号落在该层消息的 reserved 段里,或在持久 schema 台账的退役清单里。两者是同一件事的两种表示 ——
// 删字段必须写 reserved(AGENTS §4),台账的 retired 行就是据此记下的;载荷类消息不进台账,只能看 reserved 段。
// 退役字段的残留不代表更新版本的语义,不算。
//
// where 非空时填第一个新字段的位置「消息全名#字段号」,只进日志。
[[nodiscard]] inline bool TreeCarriesNewerVersionFields(const google::protobuf::Message &message, std::string *where = nullptr)
{
    const auto *descriptor = message.GetDescriptor();
    const auto *reflection = message.GetReflection();
    const auto &unknown = reflection->GetUnknownFields(message);
    if (!unknown.empty())
    {
        const auto fullName = descriptor->full_name();
        const std::string_view name(fullName.data(), fullName.size());
        for (int i = 0; i < unknown.field_count(); ++i)
        {
            const int number = unknown.field(i).number();
            if (descriptor->IsReservedNumber(number) || persist_schema::IsRetiredField(name, number))
            {
                continue;
            }
            if (where != nullptr)
            {
                *where = std::string(name) + "#" + std::to_string(number);
            }
            return true;
        }
    }

    std::vector<const google::protobuf::FieldDescriptor *> fields;
    reflection->ListFields(message, &fields);
    for (const auto *field : fields)
    {
        if (field->cpp_type() != google::protobuf::FieldDescriptor::CPPTYPE_MESSAGE)
        {
            continue;
        }
        if (!field->is_repeated())
        {
            if (TreeCarriesNewerVersionFields(reflection->GetMessage(message, field), where))
            {
                return true;
            }
            continue;
        }
        const int size = reflection->FieldSize(message, field);
        for (int i = 0; i < size; ++i)
        {
            if (TreeCarriesNewerVersionFields(reflection->GetRepeatedMessage(message, field, i), where))
            {
                return true;
            }
        }
    }
    return false;
}

} // namespace unknown_field_carry

#pragma pop_macro("GetMessage")

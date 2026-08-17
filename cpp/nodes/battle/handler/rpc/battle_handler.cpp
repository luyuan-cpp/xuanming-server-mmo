#include "battle_handler.h"

#include "muduo/base/Logging.h"

// 引用一个 battle_node.proto 的消息类型,保证该文件的描述符已注册进
// generated_pool(FindServiceByName 依赖静态注册,直接引用消息最稳妥)。
#include "proto/battle/battle_node.pb.h"

const ::google::protobuf::ServiceDescriptor *BattleHandler::GetDescriptor()
{
    // 先触发 battle_node.proto 描述符注册,再按全名查服务描述符。
    // battle_node.proto 无 package,服务全名即 "BattleNode"。
    (void)::CreateBattleRequest::default_instance();
    const auto *descriptor =
        ::google::protobuf::DescriptorPool::generated_pool()->FindServiceByName("BattleNode");
    if (descriptor == nullptr)
    {
        LOG_FATAL << "battle 占位 handler 找不到 BattleNode 服务描述符,battle_node.proto 生成产物缺失";
    }
    return descriptor;
}

void BattleHandler::CallMethod(const ::google::protobuf::MethodDescriptor *method,
                               ::google::protobuf::RpcController *controller,
                               const ::google::protobuf::Message * /*request*/,
                               ::google::protobuf::Message * /*response*/,
                               ::google::protobuf::Closure *done)
{
    // battle 不提供 muduo TCP RPC 业务;正常拓扑下不可能走到这里
    // (IsTcpNodeType 不含 BattleNodeService,无节点会向 battle 发 TCP RPC)。
    // full_name() 在 protobuf 35.x 返回 absl::string_view,muduo LogStream 无对应
    // operator<<,须显式转 std::string
    LOG_ERROR << "battle 节点收到意外的 TCP RPC 调用: "
              << (method != nullptr ? std::string(method->full_name()) : std::string("<null>"));
    if (controller != nullptr)
    {
        controller->SetFailed("battle 节点不提供 muduo TCP RPC 服务");
    }
    if (done != nullptr)
    {
        done->Run();
    }
}

const ::google::protobuf::Message &BattleHandler::GetRequestPrototype(
    const ::google::protobuf::MethodDescriptor *method) const
{
    return *::google::protobuf::MessageFactory::generated_factory()->GetPrototype(method->input_type());
}

const ::google::protobuf::Message &BattleHandler::GetResponsePrototype(
    const ::google::protobuf::MethodDescriptor *method) const
{
    return *::google::protobuf::MessageFactory::generated_factory()->GetPrototype(method->output_type());
}

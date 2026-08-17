#pragma once

#include <google/protobuf/descriptor.h>
#include <google/protobuf/message.h>
#include <google/protobuf/service.h>

// battle 节点的 muduo TCP RPC 占位 handler。
//
// battle 是纯 gRPC 协议节点(设计文档 D2/D6):不进 IsTcpNodeType 白名单,
// 没有任何节点会经 muduo TCP RPC 向它握手或发业务消息。但 Node 框架的入口
// 模板(RunSimpleNodeMainWithOwnedContext)要求一个 protobuf Service 作为
// reply service,这里提供一个"永远失败"的最小实现兜底。
//
// 刻意不依赖 cc_generic_services 生成的 protobuf 泛型服务类:
// battle 的 proto 要走 gRPC C++ 插件生成(该插件拒绝 cc_generic_services=true,
// 见 proto/scene_manager/scene_node_service.proto 头部注释的同类先例),
// 因此描述符从 generated_pool 里取(服务描述符与 cc_generic_services 无关,
// 始终随 battle_node.pb.cc 注册)。
class BattleHandler : public ::google::protobuf::Service
{
public:
    const ::google::protobuf::ServiceDescriptor *GetDescriptor() override;

    // 任何经 TCP RPC 打进来的调用都直接判失败:battle 不提供 muduo TCP RPC 业务。
    void CallMethod(const ::google::protobuf::MethodDescriptor *method,
                    ::google::protobuf::RpcController *controller,
                    const ::google::protobuf::Message *request,
                    ::google::protobuf::Message *response,
                    ::google::protobuf::Closure *done) override;

    const ::google::protobuf::Message &GetRequestPrototype(
        const ::google::protobuf::MethodDescriptor *method) const override;

    const ::google::protobuf::Message &GetResponsePrototype(
        const ::google::protobuf::MethodDescriptor *method) const override;
};

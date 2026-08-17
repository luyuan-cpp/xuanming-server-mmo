#include <unordered_map>
#include <memory>
#include <google/protobuf/service.h>

extern std::unordered_map<std::string, std::unique_ptr<::google::protobuf::Service>> gNodeService;

// battle 是纯 gRPC 节点,不向 muduo TCP RPC server 注册任何业务服务
// (gRPC 服务在 main.cpp 里经 Node::RegisterGrpcService 注册)。
void InitServiceHandler()
{
}

#pragma once

// 服务端方法表(docs/design/grpc-muduo-transport.md §15.4.2、§15.4.3、§15.5.4)。
//
// 路由按 :path 精确匹配:scene 是 "/scene_node.SceneNodeGrpc/<M>",battle 是 "/BattleNode/<M>"(无 package)。
// 生成的 <Svc>Impl 实现 Service,在 Server::Start 之前把自己的方法登记进来;Start 之后表只读。
// 本文件不依赖 protobuf / grpc:请求与应答在这一层只是字节,解析由 core 的 GrpcUnaryCall 负责。
//
// 线程:登记与查询都在 Server 所属的 loop 线程上(Start 之前登记,之后只读),不加锁。

#include <cstddef>
#include <functional>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

namespace muduo_grpc
{

class ServerCall;

// requestBytes 只在本次调用内有效。只在 loop 线程、Server 的派发泵里调用,绝不在 nghttp2 回调栈里;
// 调用前框架已确认调用仍有效、未到期。handler 必须让 call 恰好完成一次(同步 Finish*,或留着句柄稍后完成)。
using UnaryMethodHandler = std::function<void(ServerCall call, std::string_view requestBytes)>;

// "/pkg.Service/Method" -> {"pkg.Service","Method"};必须以 '/' 开头、恰好两段、两段都非空。
// 返回的 string_view 指向 path 本身,生命周期跟随 path。
std::optional<std::pair<std::string_view, std::string_view>> SplitMethodPath(std::string_view path);

class MethodTable
{
public:
    // 路径非法(SplitMethodPath 拆不开)、重复、或 handler 为空:LOG_ERROR,valid_ 置 false,返回 false。
    // 失败不影响已经登记的方法;Server 在 Start 前检查 valid(),任何一次失败都应当阻止启动。
    bool AddUnary(std::string path, UnaryMethodHandler handler);
    // 没有返回 nullptr。返回的指针在表不再修改期间有效(Start 之后只读)。
    const UnaryMethodHandler *Find(std::string_view path) const;
    // fullServiceName 即 SplitMethodPath 拆出的第一段;用来区分两种 UNIMPLEMENTED 文案(§15.5.3)。
    bool HasService(std::string_view fullServiceName) const;
    bool Valid() const { return valid; }
    size_t size() const { return methods.size(); }
    // 已登记的全部路径,升序。Server::Start 据此冻结自己的派发表(下标 → handler)与按方法的统计维度
    // (§15.4.3 server.h 的 methods_);§15.4.3 草稿之外的补充。
    std::vector<std::string> Paths() const;

private:
    // 透明哈希:Find / HasService 用 string_view 查找,不为每次查询构造 std::string。
    struct StringHash
    {
        using is_transparent = void;
        size_t operator()(std::string_view value) const { return std::hash<std::string_view>{}(value); }
    };

    std::unordered_map<std::string, UnaryMethodHandler, StringHash, std::equal_to<>> methods;
    std::unordered_set<std::string, StringHash, std::equal_to<>> services;
    bool valid{true};
};

class Service
{
public:
    virtual ~Service() = default;
    virtual void RegisterMethods(MethodTable &table) = 0;
};

} // namespace muduo_grpc

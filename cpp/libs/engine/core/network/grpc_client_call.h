#pragma once
#include <chrono>
#include <cstdint>
#include <functional>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace google::protobuf { class Message; }

// 手工维护的 gRPC 客户端调用契约:tools/proto_generator 的 grpc_async_client / service_header 模板
// 生成的代码引用这里的类型。现行接口与完成契约见 docs/design/grpc-muduo-transport.md §16。
//
// 与传输无关:不含任何官方 gRPC 类型。2026-10-15 第 3 批起生成客户端走 muduo_grpc::Channel,第 5 批删除官方运行时。

// C++ 节点 unary gRPC 调用的内置 deadline(毫秒)。bin/etc/base_deploy_config.yaml 的 GrpcClient.CallDeadlineMs
// 没配到的目标节点类型用它(grpc_call_deadline::Apply)。取值须大于各 Go 服务 zrpc 服务端 Timeout 的最大值
// (scene_manager 8000)加 2000 余量:下游先超时,调用方才收得到带真实错误码的应答(上游比下游宽)。
inline constexpr uint32_t kDefaultGrpcCallDeadlineMs = 10000;

// gRPC metadata:有序的键值对。随请求发出的按 Send 传入的原值保存(应用层 Base64 之前);
// 应答里的按收到的值保存(`-bin` 键已解掉线上那一层 base64,与官方客户端的 GetServerInitialMetadata 同义)。
using GrpcMetadata = std::vector<std::pair<std::string, std::string>>;
using GrpcSentMetadata = GrpcMetadata;

// 与 grpc::StatusCode 同值(线上 grpc-status 的数值)。只列调用方需要按名字判断的几个。
inline constexpr uint32_t kGrpcStatusOk = 0;
inline constexpr uint32_t kGrpcStatusCancelled = 1;
inline constexpr uint32_t kGrpcStatusDeadlineExceeded = 4;
inline constexpr uint32_t kGrpcStatusUnimplemented = 12;
inline constexpr uint32_t kGrpcStatusInternal = 13;
inline constexpr uint32_t kGrpcStatusUnavailable = 14;

inline const std::string* FindGrpcMetadata(const GrpcMetadata& metadata, std::string_view key) {
	for (const auto& [entryKey, entryValue] : metadata) {
		if (entryKey == key) {
			return &entryValue;
		}
	}
	return nullptr;
}

// 一次 unary 调用的结果。code 与 grpc::StatusCode 同值,0 = OK。
// 非 OK = 结果未知,不代表服务端没执行。截止、断线与取消之后,服务端仍可能完成业务。
// 完成回调只能释放本地在途状态、提示客户端或重试幂等请求;裁决对端状态仍靠看门狗与权威数据。
struct GrpcCallOutcome {
	uint32_t code{kGrpcStatusOk};
	std::string message;
	// 服务端回写的响应头 / trailers 里的自定义 metadata;失败时可能为空(连接断开、本地拒绝)。
	GrpcMetadata initialMetadata;
	GrpcMetadata trailingMetadata;

	bool ok() const { return code == kGrpcStatusOk; }

	// 按 key 找响应头里的 metadata;没有返回 nullptr。路由模式的 gate 按回显会话认领。
	const std::string* FindInitialMetadata(std::string_view key) const { return FindGrpcMetadata(initialMetadata, key); }
};

// 每次调用的完成回调,契约(docs/design/grpc-muduo-transport.md §16.1):
// - 恰好调用一次;只在节点所属 EventLoop 线程调用,从不在 Send 栈内同步调用(本地拒绝也延后投递)。
// - reply 只在 outcome.ok() 时非空,只在回调期间有效;要留用就拷贝。
// - done 必填;传入空回调是调用方错误,生成客户端会 LOG_FATAL。
// - 节点摘除 / 停机先以非 OK 完成在途调用,再拆节点;回调里不要假设节点仍可用。摘节点时回调在实体销毁之后才执行,
//   不要捕获实体句柄或组件指针。
// - 不要按 code 区分「停机」与「传输失败」:同一停机场景旧官方 CQ 给 CANCELLED、muduo_grpc 通道给 UNAVAILABLE,
//   换传输后会变。非 OK 一律当结果未知,只释放本地在途状态。
template <typename Reply>
using GrpcDone = std::function<void(const GrpcCallOutcome&, const Reply* reply)>;

// 通用 RPC 元数据发送器使用同一完成契约;响应只在回调期间有效。
using GrpcMessageDone = std::function<void(const GrpcCallOutcome&, const google::protobuf::Message* reply)>;

// 每次调用的可选参数。deadline <= 0 用该 proto 文件的默认 deadline(Set<File>CallDeadline);
// metadata 填原值,发出前由生成代码做 Base64;需要在完成时使用的请求上下文由闭包捕获。
struct GrpcCallOptions {
	std::chrono::milliseconds deadline{0};
	GrpcSentMetadata metadata;
};

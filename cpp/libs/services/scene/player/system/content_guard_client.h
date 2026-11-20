#pragma once

#include <cstdint>
#include <functional>
#include <string>

#include "proto/content_guard/content_guard.pb.h"

// 内容安全服务(go/content_guard)的调用端:scene 收到玩家填写的文字(角色改名、宠物名、属性方案名)时先送检
// (docs/design/rename-and-content-safety.md §6.6)。Go 侧的同一份口径在 go/shared/contentguard。
//
// 口径收在这里,调用方不各写一遍:
//   - 服务不可达、超时、回了不认识的结局,一律不放行(kUnavailable / kReject);
//   - 被检文字不进日志。
//
// 判定是异步的:结果在主循环线程上通过回调给出,从不在 Check 的调用栈里同步调用(生成的 gRPC 客户端的契约)。
// 回调到来时玩家可能已经下线或换了实体,调用方要自己复核(按 player_id 重新找实体)。
class ContentGuardClient
{
public:
	enum class Verdict : uint8_t
	{
		kPass,		  // 原文可用
		kMasked,	  // 用 maskedText 代替原文。只有聊天场景会出现;scene 送检的都是名字类,收到它按拒绝处理
		kReject,	  // 命中屏蔽词
		kUnavailable, // 内容安全服务不可达或超时:按不可用处理,回可重试的提示
	};

	struct Result
	{
		Verdict verdict{Verdict::kUnavailable};
		std::string maskedText;
		uint32_t category{0};

		// 名字类场景只有 kPass 才算能用。
		bool Passed() const { return verdict == Verdict::kPass; }
	};

	using Done = std::function<void(const Result &)>;

	// 送检一段文字。
	// 返回 false = 没发出去(没有就绪的内容安全节点),done 不会被调用,调用方当场按不可用处理;
	// 返回 true  = 已发出,done 恰好调用一次。
	// 空文字不该送检,由调用方自己挡(这里照发,服务会放行)。
	static bool Check(content_guard::TextScene scene, uint64_t playerId, const std::string &text, Done done);

	// 把一次调用的结局换成 Result。只认 PASS 与 MASKED;调用失败、回包为空、其余取值(含将来新增的)一律不放行。
	// MASKED 却没带打码后的正文是服务的缺陷,同样不放行。
	static Result FromReply(bool callOk, const content_guard::CheckTextResponse *reply);

	// 测试注入点:传 nullptr 恢复默认实现(挑一个 READY 的 ContentGuardNodeService 节点发出)。生产代码不调用。
	using SendFn = bool (*)(const content_guard::CheckTextRequest &, Done);
	static void SetTransportForTest(SendFn send);
};

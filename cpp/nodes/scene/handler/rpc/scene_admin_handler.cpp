
#include "scene_admin_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <muduo/base/Logging.h>
#include <node/system/node/node.h>
#include <proto/common/component/player_comp.pb.h>
#include <proto/common/database/mysql_database_table.pb.h>
#include <thread_context/ecs_context.h>
#include <utils/registry/describe_entity.h>

#include <algorithm>
#include <string>
#include <unordered_map>

// GM 面调用方鉴权。与 Gate.GmGracefulShutdown 共用同一份实现 —— 信封格式、
// canonical 串、时间窗与 nonce 去重的完整定义都在这个头里,两侧必须逐字节一致,
// 所以绝不能在这里另写一份。
//
// ⚠️ 跨节点相对包含是**临时形态**:这个头当初放在 cpp/nodes/gate/ 下是因为只有
// gate 一个使用者,现在 scene 成了第二个,它的正确归宿是 cpp/libs/engine/core/
// (gate 与 scene 的 include 目录都已经包含该路径)。没有在本次一并搬迁,是因为
// 搬迁要连带改 gate 的三处 include、gate.vcxproj / core.vcxproj 的 ClInclude、
// 那份刻意脱离引擎独立编译的 gate_security_test.cpp 的构建命令,以及 SECURITY.md
// 的路径引用 —— 九个文件的重构挂在一个安全修复上,而本仓 Claude 不执行编译
// (CLAUDE.md §10.1),验证不了。搬迁另开一件事做。
#include "../../../gate/gate_security.h"

namespace
{
// 值只开放给已鉴权的 GM 调用；普通 C++ 持久组件仍只列类型，不新增序列化适配。
const std::unordered_map<entt::id_type, std::string (*)(const void*)> kPersistedValueFormatters{
    {entt::type_hash<Transform>::value(), [](const void* value) { return static_cast<const Transform*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerUint64Comp>::value(), [](const void* value) { return static_cast<const PlayerUint64Comp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerSkillListComp>::value(), [](const void* value) { return static_cast<const PlayerSkillListComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerUint32Comp>::value(), [](const void* value) { return static_cast<const PlayerUint32Comp*>(value)->ShortDebugString(); }},
    {entt::type_hash<BaseAttributesComp>::value(), [](const void* value) { return static_cast<const BaseAttributesComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<LevelComp>::value(), [](const void* value) { return static_cast<const LevelComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerAttributeComp>::value(), [](const void* value) { return static_cast<const PlayerAttributeComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerPetComp>::value(), [](const void* value) { return static_cast<const PlayerPetComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerProfileComp>::value(), [](const void* value) { return static_cast<const PlayerProfileComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerAssetOpLedgerComp>::value(), [](const void* value) { return static_cast<const PlayerAssetOpLedgerComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<PlayerInboxLedgerComp>::value(), [](const void* value) { return static_cast<const PlayerInboxLedgerComp*>(value)->ShortDebugString(); }},
    {entt::type_hash<CurrencyComp>::value(), [](const void* value) { return static_cast<const CurrencyComp*>(value)->ShortDebugString(); }},
};
}
///<<< END WRITING YOUR CODE

void SceneSceneHandler::Test(::google::protobuf::RpcController* controller, const ::GameSceneTest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	///<<< END WRITING YOUR CODE

}

void SceneSceneHandler::GmGracefulShutdown(::google::protobuf::RpcController* controller, const ::GmGracefulShutdownRequest* request,
	::GmGracefulShutdownResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE

	// GM 面必须鉴权。这条 RPC 会把本 scene 节点排队停机 —— 它持有的是**玩家权威
	// 数据**,停机会触发全量存盘 drain,被人恶意连发就是一次可重复的全服抖动。
	// 在此之前唯一的"身份"是请求体里自报的 operator 明文字符串:任何能连到本节点
	// RPC 端口的进程发一条 GmGracefulShutdown 就能停服,无凭据、无审计。
	//
	// Gate 侧的同名 RPC 早就接了这套校验(gate_service_handler.cpp),但 scene 侧
	// 一直是裸奔的 —— 鉴权原语、SECURITY.md、单测都齐了,只差这一处接线。
	// gate_security.h 的 canonical 串里把 method 绑了进去,正是为了防止把 Scene
	// 这条 GM RPC 的签名搬去调 Gate,所以这里的 method 名必须是
	// "Scene.GmGracefulShutdown",不能照抄 Gate 那个。
	//
	// 密钥只从环境变量 GATE_GM_ADMIN_SECRET 读,未配置时一律拒绝(fail-closed),
	// 不设"开发模式免签"的口子:开发环境要停 scene,Ctrl+C / SIGTERM 就够了。
	//
	// 签名绑本节点 node_id:nonce 去重表是进程内的,不绑就意味着抓到一条合法停机
	// 请求后可以在时间窗内挨个重放给全区每一个 scene —— 一次抓包换全服停机。
	const std::string selfNodeId = std::to_string(gNode->GetNodeId());
	const auto gmAuth = gate_security::VerifyGmRequestFromEnv(
		"Scene.GmGracefulShutdown", selfNodeId, request->operator_(), request->reason());
	if (gmAuth.result != gate_security::GmAuthResult::kOk)
	{
		// 拒绝也要留痕:这是入侵检测的唯一信号源。operator / reason 都是攻击者
		// 可控输入(不含任何秘密),照原样打有助于事后比对来源 —— 但必须截断,
		// 否则对方发一条几 MB 的 operator 就能把日志盘和 IO 线程当放大器用,
		// 而这条路径连鉴权都还没过。substr 的 count 超长会自动截到 size,不会抛。
		constexpr size_t kMaxLoggedFieldLength = 128;
		LOG_ERROR << "GM graceful shutdown REJECTED, reason="
				  << gate_security::GmAuthResultName(gmAuth.result)
				  << " raw_operator=" << request->operator_().substr(0, kMaxLoggedFieldLength)
				  << " request_reason=" << request->reason().substr(0, kMaxLoggedFieldLength);
		// 被拒以 RPC_ERROR(HANDLER_FAILED) 回给调用方(kRemoteFailed),与「鉴权通过但没有可关的对象」区分开;
		// 线上只带错误码,不回拒绝原因,原因只在本地日志。与 Gate.GmGracefulShutdown 同一口径。
		controller->SetFailed("GM graceful shutdown rejected");
		return;
	}

	LOG_INFO << "GM graceful shutdown requested by operator=" << gmAuth.operatorName
			 << " reason=" << request->reason();

	// 1. 只读统计当前玩家。唯一的存盘/退出入口在 Node before-shutdown
	// hook,它会先复制实体列表,避免 dirty-save 快路径会边遍历边销毁实体。
	const auto view = tlsEcs.actorRegistry.view<Player>();
	const uint32_t count = static_cast<uint32_t>(view.size());

	LOG_INFO << "GM graceful shutdown: scheduling persistence drain for " << count << " players.";
	response->set_affected_count(count);

	// 2. 应答由框架负责发送:本 handler 不调 done->Run(),也不接管 done(不调 RpcDoneCallback::Adopt)。
	//
	// RpcChannel::CallServiceMethod 在 handler 同步返回之后立即序列化 response 并回包
	// (调用方带非 0 调用 id 时必回,affected_count 为 0 也回;id 为 0 是单向调用,永不回包)。
	// 第 3 步依赖这条时序:回包先于 RequestShutdown。若改成 Adopt 延后完成,回包就可能
	// 落在停机之后,调用方只能等到超时或断连。
	//
	// 历史:旧框架传进来的 done 恒为 nullptr,旧代码 `done->Run()` 是确定性的空指针解引用 ——
	// GM 一敲停机,进程当场崩在这一行:玩家已经收不到任何东西,而第 3 步的优雅停机
	// (存盘 barrier、租约注销)根本没机会执行,"优雅停机"退化成硬杀。
	//
	// 3. 停机必须延后到本次 RPC 应答发出之后。直接在这里调 RequestShutdown 会在
	// handler 返回前就开始拆运行时,框架随后再去发应答就可能发不出去。queueInLoop
	// 把它排到当前事件循环回合的末尾:此时 handler 已返回、应答已写进发送缓冲。
	// 仍然是非阻塞投递 —— RPC handler 跑在 loop 线程上,不能同步等整个 barrier 完成:
	// gRPC 排空与停机收尾都靠 loop 推进,在 loop 上等就永远等不到。
	gNode->GetLoop()->queueInLoop([] { gNode->RequestShutdown(); });
	return;

	///<<< END WRITING YOUR CODE

}

void SceneSceneHandler::GmDescribeEntity(::google::protobuf::RpcController* controller, const ::GmDescribeEntityRequest* request,
	::GmDescribeEntityResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
    constexpr std::size_t kMaxLoggedFieldLength = 128;
    constexpr std::size_t kMaxValueLength = 4 * 1024;
    constexpr std::size_t kMaxDescriptionLength = 64 * 1024;
    const std::string selfNodeId = std::to_string(gNode->GetNodeId());
    const auto gmAuth = gate_security::VerifyGmRequestFromEnv(
        "Scene.GmDescribeEntity", selfNodeId, request->operator_(), request->reason());
    if (gmAuth.result != gate_security::GmAuthResult::kOk)
    {
        LOG_ERROR << "GM describe entity REJECTED, reason="
                  << gate_security::GmAuthResultName(gmAuth.result)
                  << " raw_operator=" << request->operator_().substr(0, kMaxLoggedFieldLength)
                  << " request_reason=" << request->reason().substr(0, kMaxLoggedFieldLength);
        controller->SetFailed("GM describe entity rejected");
        return;
    }
    // reason 是签名覆盖的目标，拒绝拿合法签名换 player_id 重放。
    if (request->reason() != std::to_string(request->player_id()))
    {
        LOG_ERROR << "GM describe entity REJECTED, reason=target_mismatch"
                  << " operator=" << gmAuth.operatorName.substr(0, kMaxLoggedFieldLength)
                  << " player_id=" << request->player_id()
                  << " request_reason=" << request->reason().substr(0, kMaxLoggedFieldLength);
        controller->SetFailed("GM describe entity rejected");
        return;
    }

    const auto player = tlsEcs.GetPlayer(request->player_id());
    auto& registry = tlsEcs.actorRegistry;
    if (!registry.valid(player))
    {
        response->set_description("player not on this node");
        LOG_INFO << "GM describe entity operator=" << gmAuth.operatorName
                 << " player_id=" << request->player_id() << " components=0";
        return;
    }

    auto description = DescribeEntity(registry, player);
    description.resize(std::min(description.size(), kMaxDescriptionLength));
    std::size_t componentCount = 0;
    for (auto [id, storage] : registry.storage())
    {
        if (!storage.contains(player))
        {
            continue;
        }
        ++componentCount;
        if (!request->include_values() || description.size() == kMaxDescriptionLength)
        {
            continue;
        }
        const auto formatter = kPersistedValueFormatters.find(storage.info().hash());
        if (formatter == kPersistedValueFormatters.end())
        {
            continue;
        }
        const void* value = storage.value(player);
        if (value == nullptr)
        {
            continue;
        }
        const auto line = "\n" + std::string(storage.info().name()) + "=" +
            formatter->second(value).substr(0, kMaxValueLength);
        description.append(line, 0, kMaxDescriptionLength - description.size());
    }
    response->set_description(description);
    // 值只在本次 RPC 应答中出现，审计日志不记录组件内容。
    LOG_INFO << "GM describe entity operator=" << gmAuth.operatorName
             << " player_id=" << request->player_id() << " components=" << componentCount;
///<<< END WRITING YOUR CODE}

}

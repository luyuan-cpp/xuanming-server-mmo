
#include "scene_admin_handler.h"

///<<< BEGIN WRITING YOUR CODE
#include <muduo/base/Logging.h>
#include <node/system/node/node.h>
#include <proto/common/component/player_comp.pb.h>
#include <thread_context/ecs_context.h>
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

	LOG_INFO << "GM graceful shutdown requested by operator=" << request->operator_()
			 << " reason=" << request->reason();

	// 1. 只读统计当前玩家。唯一的存盘/退出入口在 Node before-shutdown
	// hook,它会先复制实体列表,避免 dirty-save 快路径会边遍历边销毁实体。
	const auto view = tlsEcs.actorRegistry.view<Player>();
	const uint32_t count = static_cast<uint32_t>(view.size());

	LOG_INFO << "GM graceful shutdown: scheduling persistence drain for " << count << " players.";
	response->set_affected_count(count);

	// 2. 应答由框架负责发送,这里**不能**碰 done。
	//
	// GameChannel::CallMethod 传进来的 done 恒为 nullptr(见 game_channel.cpp:451),
	// 应答是在 CallMethod 返回之后由框架序列化 response 再发出的。旧代码 `done->Run()`
	// 是确定性的空指针解引用 —— GM 一敲停机,进程当场崩在这一行:玩家已经收不到
	// 任何东西,而第 3 步的优雅停机(存盘 barrier、租约注销)根本没机会执行,
	// "优雅停机"退化成硬杀。
	//
	// 3. 停机必须延后到本次 RPC 应答发出之后。直接在这里调 RequestShutdown 会在
	// handler 返回前就开始拆运行时,框架随后再去发应答就可能发不出去。queueInLoop
	// 把它排到当前事件循环回合的末尾:此时 CallMethod 已返回、应答已写进发送缓冲。
	// 仍然是非阻塞投递 —— RPC handler 不能等整个 barrier 完成,否则 gRPC
	// Server::Shutdown 等 handler 退出、handler 又等 Shutdown 完成,会闭环。
	gNode->GetLoop()->queueInLoop([] { gNode->RequestShutdown(); });
	return;

	///<<< END WRITING YOUR CODE
}

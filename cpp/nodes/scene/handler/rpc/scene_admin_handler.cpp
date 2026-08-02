
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

	// 2. Reply to GM before shutting down so the caller gets the response.
	done->Run();

	// 3. 非阻塞投递停机。RPC handler 不能等整个 barrier 完成,否则
	// gRPC Server::Shutdown 等 handler 退出、handler 又等 Shutdown 完成,会闭环。
	gNode->RequestShutdown();
	return;

	///<<< END WRITING YOUR CODE
}

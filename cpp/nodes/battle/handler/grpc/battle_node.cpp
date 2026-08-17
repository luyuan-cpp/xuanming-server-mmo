#include "battle_node.h"
#include <future>

#include "logic/battle_room_manager.h"
#include "muduo/base/Logging.h"

BattleNodeImpl::BattleNodeImpl(muduo::net::EventLoop& loop)
    : loop_(loop)
{
}

void BattleNodeImpl::HandleCreateBattle(const ::CreateBattleRequest* request,
    ::CreateBattleResponse* response)
{
    // 业务逻辑全部在手写类 BattleRoomManager(cpp/nodes/battle/logic/),此处只做委托
    BattleRoomManager::Instance().HandleCreateBattle(*request, *response);
}

void BattleNodeImpl::HandleDestroyBattle(const ::DestroyBattleRequest* request)
{
    // 幂等销毁,只解绑不结算(补偿/回滚路径),委托手写房间管理器
    BattleRoomManager::Instance().HandleDestroyBattle(*request);
}

grpc::Status BattleNodeImpl::CreateBattle(grpc::ServerContext* /*context*/,
    const ::CreateBattleRequest* request,
    ::CreateBattleResponse* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, response, &promise]
                    {
        HandleCreateBattle(request, response);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status BattleNodeImpl::DestroyBattle(grpc::ServerContext* /*context*/,
    const ::DestroyBattleRequest* request,
    ::Empty* /*response*/)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, &promise]
                    {
        HandleDestroyBattle(request);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

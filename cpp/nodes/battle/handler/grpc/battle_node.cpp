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

void BattleNodeImpl::HandleAddObserver(const ::AddObserverRequest* request,
    ::AddObserverResponse* response)
{
    // 观战接入(设计文档 §10.5),错误经 response tip 返回,status 恒 OK
    BattleRoomManager::Instance().HandleAddObserver(*request, *response);
}

void BattleNodeImpl::HandleRemoveObserver(const ::RemoveObserverRequest* request)
{
    // 幂等清退(match 互斥路径),委托手写房间管理器
    BattleRoomManager::Instance().HandleRemoveObserver(*request);
}

grpc::Status BattleNodeImpl::AddObserver(grpc::ServerContext* /*context*/,
    const ::AddObserverRequest* request,
    ::AddObserverResponse* response)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, response, &promise]
                    {
        HandleAddObserver(request, response);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status BattleNodeImpl::RemoveObserver(grpc::ServerContext* /*context*/,
    const ::RemoveObserverRequest* request,
    ::Empty* /*response*/)
{
    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, &promise]
                    {
        HandleRemoveObserver(request);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

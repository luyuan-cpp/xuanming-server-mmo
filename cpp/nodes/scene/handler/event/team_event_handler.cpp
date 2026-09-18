#include "team_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "player/system/player_team.h"
///<<< END WRITING YOUR CODE
void TeamEventHandler::Register()
{
    tlsEcs.dispatcher.sink<PlayerTeamRefreshEvent>().connect<&TeamEventHandler::PlayerTeamRefreshEventHandler>();
}

void TeamEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<PlayerTeamRefreshEvent>().disconnect<&TeamEventHandler::PlayerTeamRefreshEventHandler>();
}
void TeamEventHandler::PlayerTeamRefreshEventHandler(const PlayerTeamRefreshEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // team 服务(match 进程)--Kafka SceneCommand{DispatchEvent}--> DispatchProtoEvent 进程内分发到此(loop 线程)。
    // 只是"去刷新一下"的信号,不带权威数据:PlayerTeamSystem 自己读 SharedRedis 刷新 TeamId,
    // 本节点找不到玩家实体就丢弃(team-system.md §F.2)。
    PlayerTeamSystem::OnRefreshEvent(event);
///<<< END WRITING YOUR CODE
}

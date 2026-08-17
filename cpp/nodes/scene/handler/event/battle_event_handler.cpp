#include "battle_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "battle/system/player_battle.h"
///<<< END WRITING YOUR CODE
void BattleEventHandler::Register()
{
    tlsEcs.dispatcher.sink<BattleSettlementEvent>().connect<&BattleEventHandler::BattleSettlementEventHandler>();
}

void BattleEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<BattleSettlementEvent>().disconnect<&BattleEventHandler::BattleSettlementEventHandler>();
}
void BattleEventHandler::BattleSettlementEventHandler(const BattleSettlementEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // battle --Kafka SceneCommand{DispatchEvent}--> DispatchProtoEvent 进程内分发到此(loop 线程)。
    // scene 是结算的唯一应用者:battle_id 匹配校验、离线 pending 暂存、锁清理与
    // 客户端通知全部在 PlayerBattleSystem::ApplySettlement 内闭环。
    PlayerBattleSystem::ApplySettlement(event);
///<<< END WRITING YOUR CODE
}

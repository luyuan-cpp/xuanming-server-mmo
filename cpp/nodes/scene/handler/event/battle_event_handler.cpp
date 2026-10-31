#include "battle_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
#include "battle/system/player_battle.h"
///<<< END WRITING YOUR CODE
void BattleEventHandler::BattleConfirmedEventHandler(const BattleConfirmedEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
    // battle CreateBattle 成功后每玩家一条(同一 Kafka 通道进来,loop 线程)。
    // PREPARING -> FIGHTING、作废期限切正式 deadline、battle:lock 条件续期,幂等处理全在系统内。
    PlayerBattleSystem::ConfirmBattle(event);
///<<< END WRITING YOUR CODE
}

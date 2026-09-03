#include "match_event_handler.h"
#include "thread_context/ecs_context.h"

///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
void MatchEventHandler::Register()
{
    tlsEcs.dispatcher.sink<contracts::kafka::BattleResultTeam>().connect<&MatchEventHandler::BattleResultTeamHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BattleResultEvent>().connect<&MatchEventHandler::BattleResultEventHandler>();
}

void MatchEventHandler::UnRegister()
{
    tlsEcs.dispatcher.sink<contracts::kafka::BattleResultTeam>().disconnect<&MatchEventHandler::BattleResultTeamHandler>();
    tlsEcs.dispatcher.sink<contracts::kafka::BattleResultEvent>().disconnect<&MatchEventHandler::BattleResultEventHandler>();
}
void MatchEventHandler::BattleResultTeamHandler(const contracts::kafka::BattleResultTeam& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}
void MatchEventHandler::BattleResultEventHandler(const contracts::kafka::BattleResultEvent& event)
{
///<<< BEGIN WRITING YOUR CODE
///<<< END WRITING YOUR CODE
}

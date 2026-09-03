#pragma once

#include "proto/contracts/kafka/match_event.pb.h"

class MatchEventHandler
{
public:
    static void Register();

    static void UnRegister();
    static void BattleResultTeamHandler(const contracts::kafka::BattleResultTeam& event);
    static void BattleResultEventHandler(const contracts::kafka::BattleResultEvent& event);
};

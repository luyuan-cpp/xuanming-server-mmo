#include "event_handler.h"

#include "gate_event_handler.h"
#include "match_event_handler.h"
#include "player_event_handler.h"

void RegisterNodeEvents() { EventHandler::Register(); }

void EventHandler::Register()
{
GateEventHandler::Register();
MatchEventHandler::Register();
PlayerEventHandler::Register();

}

void EventHandler::UnRegister()
{
GateEventHandler::UnRegister();
MatchEventHandler::UnRegister();
PlayerEventHandler::UnRegister();

}
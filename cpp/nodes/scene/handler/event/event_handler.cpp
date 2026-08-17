#include "event_handler.h"

#include "actor_combat_state_event_handler.h"
#include "actor_event_handler.h"
#include "battle_event_handler.h"
#include "buff_event_handler.h"
#include "combat_event_handler.h"
#include "mission_event_handler.h"
#include "node_event_handler.h"
#include "npc_event_handler.h"
#include "player_event_handler.h"
#include "player_migration_event_handler.h"
#include "scene_event_handler.h"
#include "server_event_handler.h"
#include "skill_event_handler.h"

void RegisterNodeEvents() { EventHandler::Register(); }

void EventHandler::Register()
{
ActorCombatStateEventHandler::Register();
ActorEventHandler::Register();
BattleEventHandler::Register();
BuffEventHandler::Register();
CombatEventHandler::Register();
MissionEventHandler::Register();
NodeEventHandler::Register();
NpcEventHandler::Register();
PlayerEventHandler::Register();
PlayerMigrationEventHandler::Register();
SceneEventHandler::Register();
ServerEventHandler::Register();
SkillEventHandler::Register();

}

void EventHandler::UnRegister()
{
ActorCombatStateEventHandler::UnRegister();
ActorEventHandler::UnRegister();
BattleEventHandler::UnRegister();
BuffEventHandler::UnRegister();
CombatEventHandler::UnRegister();
MissionEventHandler::UnRegister();
NodeEventHandler::UnRegister();
NpcEventHandler::UnRegister();
PlayerEventHandler::UnRegister();
PlayerMigrationEventHandler::UnRegister();
SceneEventHandler::UnRegister();
ServerEventHandler::UnRegister();
SkillEventHandler::UnRegister();

}
#pragma once

#include "proto/battle/player_battle.pb.h"

namespace turnbattle {

// 票据身份只授权本人及本人的出战宝宝；同队不代表有指令权。
// 旧客户端仅允许省略人物的 actor_id / round_index，宝宝始终要求回合号。
inline bool CanCommandBattleActor(const BattleStateS2C& state, uint64_t playerId,
                                 const SubmitBattleActionRequest& request) {
    if (playerId == 0 || state.battle_id() != request.battle_id()) return false;
    if (request.round_index() != 0 && request.round_index() != state.round_index()) return false;
    const auto actorId = request.actor_id() == 0 ? playerId : request.actor_id();
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() != actorId) continue;
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) return actorId == playerId;
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_PET || actor.owner_player_id() != playerId
            || request.round_index() == 0) return false;
        const auto type = request.action().action_type();
        return type == BATTLE_ACTION_ATTACK || type == BATTLE_ACTION_SKILL || type == BATTLE_ACTION_DEFEND;
    }
    return false;
}

} // namespace turnbattle

#pragma once
#include <cstdint>

enum eActorCombatState : uint32_t {
    kActorCombatStateNone,
    kActorCombatStateSilence,
    kActorMaxCombatStateType,
};

// 与 eActionStateMode 及 combat_ai::StateMode 统一编码: 0=互斥拒绝, 1=允许, 2=打断。
enum eCombatStateMode : uint32_t {
    kCombatStateMutualExclusion = 0, // Mutually exclusive, blocked
    kCombatStatePermitted = 1,       // Permitted
    kCombatStateInterrupt = 2,       // Interrupt current state and execute
};

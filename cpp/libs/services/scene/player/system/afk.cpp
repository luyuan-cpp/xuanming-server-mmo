#include "afk.h"

#include "muduo/base/Logging.h"

#include "core/constants/fps.h"
#include "frame/manager/frame_time.h"
#include "player/comp/afk_comp.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/team_follow_comp.h"  // TeamFollowerComp:随从挂机照样被队形驱动,不停步
#include "proto/common/component/player_comp.pb.h"
#include "spatial/system/movement.h"  // HaltMotion:挂机即停步
#include "thread_context/ecs_context.h"

// A player with no client messages for this many frames is marked AFK.
// 30 seconds * 20 fps = 600 frames.
constexpr uint32_t kAfkInactivityFrames = 30 * kTargetFPS;

void AfkSystem::Update(double delta)
{
    auto& registry = tlsEcs.actorRegistry;
    const uint32_t currentFrame = tlsFrameTimeManager.frameTime.current_frame();

    // 1. Check active players: if idle too long, mark AFK.
    //    Movement, AOI, and attribute sync systems use exclude<AfkComp>,
    //    so AFK players skip expensive per-tick processing automatically.
    //
    //    PlayerFrozenComp exclude: players in an ownership handoff (zone
    //    travel / cross-node scene switch) are NOT idle in the AFK sense —
    //    they're held write-disabled until scene_manager's EnterScene reply
    //    (or the travel watchdog) decides whether this node keeps them.
    //    Without this exclude a slow handoff (>30s) would flag them AFK and
    //    the AFK-cleanup path could destroy them mid-handoff, defeating
    //    Frozen's whole purpose. cross-zone-scene-travel.md §11.2.
    auto activeView = registry.view<Player, LastActiveFrameComp>(
        entt::exclude<AfkComp, PlayerFrozenComp>);
    for (auto &&[entity, player, lastActive] : activeView.each())
    {
        if (currentFrame - lastActive.frame >= kAfkInactivityFrames)
        {
            registry.emplace<AfkComp>(entity, AfkComp{currentFrame, 0});
            // 挂机即停步:MovementSystem 从这一刻起不再积分他的位移。速度留着的话,旁人按最后一条移动一直外推,
            // 摘掉挂机后服务器也会从原地按旧速度接着走。组队随从例外:挂机照样被队形驱动(TeamFollowSystem 不看挂机标记)。
            // 只改当前实体的 Velocity 与脏位组件,不碰本 view 遍历的组件,可以在遍历中做。
            if (!registry.any_of<TeamFollowerComp>(entity))
            {
                MovementSystem::HaltMotion(entity);
            }
            LOG_INFO << "Player entity " << entt::to_integral(entity)
                     << " marked AFK after " << kAfkInactivityFrames << " frames of inactivity";
        }
    }

    // 2. Check AFK players: if they received new input, remove AFK.
    //    Frozen players' client input is rejected by the business systems'
    //    PlayerFrozenComp gates (player_frozen_comp.h), so they cannot transition
    //    out of AFK while frozen.
    //    Excluding them here is defensive — the activeView gate above
    //    already prevents them entering AFK in the first place.
    auto afkView = registry.view<Player, AfkComp, LastActiveFrameComp>(
        entt::exclude<PlayerFrozenComp>);
    for (auto &&[entity, player, afkComp, lastActive] : afkView.each())
    {
        if (lastActive.frame > afkComp.afkStartFrame)
        {
            LOG_INFO << "Player entity " << entt::to_integral(entity)
                     << " resumed activity, removing AFK state";
            registry.erase<AfkComp>(entity);
        }
    }
}

#pragma once

#include <cstdint>
#include <chrono>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <vector>
#include "engine/core/type_define/type_define.h"
#include "entt/src/entt/entity/entity.hpp"
#include "proto/common/base/role_admission.pb.h"
#include "proto/data_service/data_service.pb.h"

struct PlayerEnterContext;
class PlayerAllData;
class SealAccountRoleTradeRequest;
struct RoleAdmissionState;

// scene逻辑线程唯一所有者。SQL登记成功才进入原加载链；封存永远按原实例/修订/世代。
// 普通销毁只关闭本地持有并保留SQL下限，交易ACK还必须等存盘落定。
class RoleAdmissionSystem {
public:
    using Clock = std::chrono::steady_clock;
    struct LocalNode { std::string instanceId, nodeId; uint32_t zoneId{0}; };
    using RegisterReply = std::function<void(bool, const data_service::RegisterRoleAdmissionResponse*)>;
    using FenceReply = std::function<void(bool, std::optional<std::string>)>;
    using CloseReply = std::function<void(bool, const data_service::CloseRoleAdmissionsResponse*)>;
    // 依赖只在当前逻辑线程使用；true表示已接管回调，false不得再回调。可同步完成。
    // 未替换的依赖沿正式HMAC/RPC/Redis适配器执行，不存在默认允许的授权分支。
    struct Dependencies {
        std::function<Clock::time_point()> now;
        std::function<LocalNode()> localNode;
        std::function<bool(data_service::RegisterRoleAdmissionRequest, RegisterReply)> registerAdmission;
        std::function<bool(data_service::CloseRoleAdmissionsRequest, CloseReply)> closeAdmissions;
        std::function<bool(data_service::AckRoleTradeDrainRequest, std::function<void(bool)>)> ackDrain;
        // expectedPrior 首次读取后固定；重试保留同一 candidate 与 prior，不从当前身份补资格。
        std::function<bool(Guid, std::string, std::optional<std::string>, std::function<bool(const std::string&)>, FenceReply)> registerHandoffFence;
        std::function<bool(Guid, std::string, std::string, std::string, std::function<void(bool)>)> revokeHandoffFence;
        std::function<void(Guid, PlayerEnterContext)> activate;
        std::function<void(Guid)> freeze;
        std::function<void(Guid, uint32_t)> reject;
        std::function<bool(Guid)> hasPendingLoad;
        std::function<bool(Guid)> hasUnsettledSave;
        // 直接测生命周期的宿主显式提供 SQL 已确认原件；默认空，不能覆盖授权判定。
        // 状态机回归不填此项，必须经过 RequestEnter 与登记回包。
        std::vector<common::base::RoleAdmissionIdentity> confirmedAdmissions;
    };
    class ScopedDependencies {
    public:
        explicit ScopedDependencies(Dependencies dependencies);
        ~ScopedDependencies();
        ScopedDependencies(const ScopedDependencies&) = delete;
        ScopedDependencies& operator=(const ScopedDependencies&) = delete;
    private:
        std::shared_ptr<RoleAdmissionState> previous_;
    };
    static void RequestEnter(Guid playerId, PlayerEnterContext context);
    static bool Allows(Guid playerId, uint64_t generation, uint64_t ownerRevision);
    static bool IsSealed(Guid playerId);
    static std::optional<common::base::RoleAdmissionIdentity> ConfirmedIdentity(Guid playerId);
    // 只确认调用方原入场上下文；不能用当前 identity 给旧异步调用补新世代或新会话。
    static std::optional<common::base::RoleAdmissionIdentity> ConfirmedIdentityForContext(
        Guid playerId, const PlayerEnterContext& context);
    // 在存盘提交前捕获原身份与不可变payload；返回的钩子只在该次提交成功后调用，
    // 且必须先于全局saved回调。失去原实体/入场资格就不登记，不读取新ctx补身份。
    // loop线程调用；不持连接，不拥有payload。
    static std::function<void()> CaptureSnapshotCommit(entt::entity player,
        std::shared_ptr<const PlayerAllData> source, uint64_t ownerEpoch, uint64_t saveSeq);
    // 这份快照是不是本实体当前入场、当前代次的提交。快照只在提交之后才替换(规格 §6),核对身份就够,不需要再读证据。
    // 输入必须是本实体当前快照,两子表player相同;E/q来自原不可变提交钩子且非零。只在scene loop线程调用。
    static bool IsSnapshotCommitted(entt::entity player, const PlayerAllData& snapshot);
    static bool Seal(const common::base::RoleTradeSeal& seal);
    static bool AcceptSeal(const SealAccountRoleTradeRequest& request);
    static void ObserveSave(Guid playerId, uint64_t ownerEpoch, uint64_t saveSeq);
    // 只对冻结发起之后提交的快照调用；旧加载/旧SQL/冻结之前在途的存盘不能补此证据。
    static void ObserveTradeSaveQueued(Guid playerId, uint64_t ownerEpoch, uint64_t saveSeq);
    // 这一轮封存已经发起冻结存盘。存盘提交时用「提交那一刻」的值决定它算不算排空证据。
    static void SetTradeFreezeSaving(Guid playerId, bool saving);
    static bool IsTradeFreezeSaving(Guid playerId);
    static void OnDestroyed(Guid playerId);
    static void BeginCallback(Guid playerId);
    static void EndCallback(Guid playerId);
    static void Tick();
    // 本线程对该玩家还有没结清的入场登记:SQL 登记 / handoff_fence 登记在途或待重发、登记撤销在途或待发、或登记相关回调未到。
    // 会话结束凭证(PlayerLifecycleSystem::HandleSessionEndedRelease)据此判"本节点可能正在建立持有":这段时间里
    // save_barrier.handoff_fence 上可能已是这条登记刚写的 H,不能把它当成已结束的资格去消费。只读,scene 逻辑线程调用。
    static bool HasAdmissionInFlight(Guid playerId);
    // 本进程实例号,即本系统登记 handoff_fence 时写入的 S(与 RequestEnter 同一个来源,测试依赖注入同样生效)。
    static std::string LocalInstanceId();
};

// 非持久运行时授权，只由成功入场写入；scene自发换图沿用原修订，不能反查买家修订。
struct PlayerRoleAdmissionComp {
    uint64_t generation{0};
    uint64_t ownerRevision{0};
};

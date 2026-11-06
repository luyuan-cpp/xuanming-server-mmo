#pragma once

// scene 节点按控制面的期望状态对账本地场景归属(缺口清单第 33 条)。
// 设计见 docs/design/scene-node-ownership-reconcile.md,判定规则在 scene_ownership_rules.h。
//
// 解决的问题:节点与 etcd 单边失联超过租约加再入屏障,scene_manager 已把它判死,拆掉了它名下实例的映射、把世界频道
// 改派到别的节点;它回来后按原 node_id 重新注册,进程里却还留着这些实体(玩家与新频道里的玩家互相看不见,
// 实例谁也回收不了)。做法同 kubelet 回到集群后停掉 API server 上已删除的 Pod:身份确认有效时定期读
// scene:{id}:node,不再指向本节点的实体走与 DestroyScene 相同的路径排空、销毁。
//
// 线程:全部只在 scene 节点的 EventLoop 线程上调用(tlsEcs、tlsSceneControlRedis 与定时器都属于它)。

#include <array>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <iterator>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

#include "engine/core/utils/registry/scene_entity.h"
#include "time/comp/timer_task_comp.h"
#include "world/scene_ownership_rules.h"

struct redisReply;

namespace hiredis
{
class Hiredis;
}

// 场景实体上的归属核对状态。按需状态(docs/design/ecs-component-access-rules.md):唯一创建入口是
// scene_ownership::CollectChecks 第一次核对到这个实体;SceneLifecycle::ReuseExistingScene 把它整个摘掉,下一轮从头算
// 宽限期;随实体销毁。缺失 = 还没核对过,不是错误。只在内存里,不进任何存盘、快照与协议。
struct SceneOwnershipCheckComp
{
	// 每次创建换一个新号。在途的读带着它,应答回来时对不上,说明期间组件被摘掉重建过(CreateScene 又命中了这个实体),
	// 这次读数作废。
	std::uint64_t trackingId{0};
	scene_ownership::CheckState state;
	// 已判定不归本节点、发起过排空。只决定日志级别与计数;判定本身每次都按读数重来。
	bool releaseStarted{false};
};

namespace scene_ownership
{
// 一次读发出时记下的东西:应答回来按它找回实体,并核对实体与核对状态都没换过。
struct PendingCheck
{
	SceneEntity scene{entt::null};
	std::uint64_t sceneId{0};
	std::uint64_t trackingId{0};
};

// 收集本地全部场景;给第一次核对到的实体登记核对状态,宽限起点记为 now。scene_id 为 0 的实体跳过(入口不会建出这种实体)。
std::vector<PendingCheck> CollectChecks(Clock::time_point now);

// 摘掉实体的核对状态:下一轮从头算宽限期,连击数清零。只给 SceneLifecycle::ReuseExistingScene 用。无效句柄 no-op。
void RestartCheck(SceneEntity scene);

// 把一条 MGET 应答拆成逐个场景的读数,下标与那一批 PendingCheck 对齐。
// 空应答(断线)、错误应答、不是数组、元素个数对不上:整批 kUnreadable。元素是 nil 为 kMissing,字符串为 kValue,其余 kUnreadable。
std::vector<OwnerReading> ReadingsOf(const redisReply *reply, std::size_t sceneCount);

struct ApplyOutcome
{
	std::size_t releaseStarted{0}; // 本批第一次判定不归本节点、发起排空的实体数
	std::size_t destroyed{0};      // 本批销毁掉的实体数
};

// 一批读数回来:逐个分类、推进核对状态;判定 kRelease 的调 SceneLifecycle::DestroyScene(先把住户改派回大世界,
// 没有绑定了才销毁;有绑定时实体留着,下一轮仍不归本节点就再调一次)。
// 读的途中实体已销毁、核对状态号变了(CreateScene 又命中了它)、scene_id 对不上的,这次读数作废。
// readings 与 checks 长度不等时整批作废。selfNodeId 为空时一律按读不到处理。
// 调用方负责:调用前确认本节点身份仍确认有效、selfNodeId 与发读时相同;issuedAt 是这批读发出的时刻。
ApplyOutcome ApplyReadings(const std::vector<PendingCheck> &checks, const std::vector<OwnerReading> &readings,
						   std::string_view selfNodeId, Clock::time_point issuedAt);

// 一轮核对的去向。
enum class SweepOutcome : std::uint8_t
{
	kSent,                // 读已发出
	kNoScenes,            // 本地没有场景
	kIdentityUnconfirmed, // 本节点 etcd 身份当前不确认有效:不核对(失联期间分不清 etcd 整体故障与单边失联)
	kRedisUnavailable,    // 控制域 Redis 没连上
	kPreviousInFlight,    // 上一轮还有应答没回来:不叠加
	kSendFailed,          // 命令没被 hiredis 收下
	kCount
};

// 指标用的名字,下标与 SweepOutcome 一一对应。长度由初始化列表推导,加了枚举项没加名字时 static_assert 失败。
inline constexpr std::string_view kSweepOutcomeNames[] = {
	"sent",                 // SweepOutcome::kSent
	"no_scenes",            // SweepOutcome::kNoScenes
	"identity_unconfirmed", // SweepOutcome::kIdentityUnconfirmed
	"redis_unavailable",    // SweepOutcome::kRedisUnavailable
	"previous_in_flight",   // SweepOutcome::kPreviousInFlight
	"send_failed",          // SweepOutcome::kSendFailed
};
static_assert(std::size(kSweepOutcomeNames) == static_cast<std::size_t>(SweepOutcome::kCount),
			  "kSweepOutcomeNames 必须与 SweepOutcome 一一对应");

// 进程级计数,只在 EventLoop 线程读写(指标采集回调也在这条线程上)。
struct Stats
{
	std::array<std::uint64_t, static_cast<std::size_t>(SweepOutcome::kCount)> sweeps{};
	std::array<std::uint64_t, static_cast<std::size_t>(Observation::kCount)> observations{};
	std::uint64_t releaseStarted{0};
	std::uint64_t destroyed{0};
};
const Stats &ReadStats();
void ResetStatsForTest();
} // namespace scene_ownership

// 把核对计数挂到 Prometheus 出口(engine/core/metrics;设了 MMORPG_METRICS_PORT 才有出口)。全部只有低基数标签:
//   scene_ownership_sweeps_total{outcome}        counter,outcome 取 kSweepOutcomeNames
//   scene_ownership_observations_total{result}   counter,result 取 kObservationNames
//   scene_ownership_releases_total{stage}        counter,stage = drain_started(判定不归本节点)/ destroyed(实体已销毁)
//   scene_ownership_releasing_scenes             gauge,  此刻已判定不归本节点、仍在排空的本地实体数
// scene 启动时调用一次;重复调用只打一行 ERROR(注册表按名字拒绝)。
void RegisterSceneOwnershipMetrics();

// 定时核对的宿主。归节点入口的运行期上下文所有(nodes/scene/main.cpp 的 SceneRuntimeContext),Start / Stop 成对。
// 定时器与 Redis 回调只捕获内部 Session 的 weak_ptr(AGENTS.md §11.7):Stop 或析构之后到达的应答一律丢弃。
class SceneOwnershipReconciler
{
public:
	using IdentityProbe = std::function<bool()>;

	SceneOwnershipReconciler() = default;
	~SceneOwnershipReconciler();

	SceneOwnershipReconciler(const SceneOwnershipReconciler &) = delete;
	SceneOwnershipReconciler &operator=(const SceneOwnershipReconciler &) = delete;
	SceneOwnershipReconciler(SceneOwnershipReconciler &&) = delete;
	SceneOwnershipReconciler &operator=(SceneOwnershipReconciler &&) = delete;

	// 在节点 SetAfterStart 里调:那时 node_id 已是终值。identityConfirmed = 本节点 etcd 身份当前确认有效
	// (EtcdService::IsIdentityConfirmedFresh);它捕获的对象必须活到 Stop。空探针 = 永远不确认,不核对(fail-closed)。
	// 重复调用先 Stop 再起。
	void Start(IdentityProbe identityConfirmed);

	// 普通停机、身份冲突关停、拆除前都调;幂等。之后到达的应答一律丢弃,不再动任何实体,也不再调探针。
	void Stop();

	// 一轮核对。定时器每 kSweepInterval 调一次;用例可直接调。没 Start 时回 kIdentityUnconfirmed、不计数。
	scene_ownership::SweepOutcome SweepOnce();

private:
	struct Session;
	static scene_ownership::SweepOutcome Sweep(const std::shared_ptr<Session> &session);
	// 发一条 MGET 读这一批场景的属主;hiredis 收下回 true(应答一定会回来,断线时是空应答)。
	static bool SendOwnerRead(hiredis::Hiredis &redis, const std::shared_ptr<Session> &session,
							  std::vector<scene_ownership::PendingCheck> batch, const std::string &selfNodeId,
							  scene_ownership::Clock::time_point issuedAt);

	std::shared_ptr<Session> session_;
	TimerTaskComp timer_;
};

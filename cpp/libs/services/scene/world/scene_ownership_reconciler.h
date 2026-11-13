#pragma once

// scene 节点按控制面的期望状态对账本地场景归属(缺口清单第 33 条)。
// 设计见 docs/design/scene-node-ownership-reconcile.md,判定规则在 scene_ownership_rules.h。
//
// 解决的问题:节点与 etcd 单边失联超过租约加再入屏障,scene_manager 已把它判死,拆掉了它名下实例的映射、把世界频道
// 改派到别的节点;它的号还可能被新进程接走。失联期间与回来之后,进程里都还留着这些实体(玩家与新频道里的玩家互相看不见,
// 实例谁也回收不了)。做法同 kubelet 按 API server 的期望状态停掉本地多余的 Pod:每 10 秒读一次本地场景的 scene:{id}:node
// 与本节点号的持有者,不归本进程的实体走与 DestroyScene 相同的路径排空、销毁,按住户预算限速。
//
// 线程:全部只在 scene 节点的 EventLoop 线程上调用(tlsEcs、tlsControlRedis 与定时器都属于它)。

#include <array>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <iterator>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "engine/core/utils/registry/scene_entity.h"
#include "time/comp/timer_task_comp.h"
#include "world/scene_ownership_rules.h"

struct redisReply;

class RedisClusterClient;

// 场景实体上的归属核对状态。按需状态(docs/design/ecs-component-access-rules.md):唯一创建入口是
// scene_ownership::CollectChecks 第一次核对到这个实体;SceneLifecycle::ReuseExistingScene 把它整个摘掉,下一轮从头算
// 宽限期;随实体销毁。缺失 = 还没核对过,不是错误。只在内存里,不进任何存盘、快照与协议。
struct SceneOwnershipCheckComp
{
	// 每次创建换一个新号。在途的读带着它,应答回来时对不上,说明期间组件被摘掉重建过(CreateScene 又命中了这个实体),
	// 这次读数作废。
	std::uint64_t trackingId{0};
	scene_ownership::CheckState state;
	// 已判定不归本进程、发起过排空。只决定日志级别、计数与指标;判定本身每次都按读数重来。
	bool releaseStarted{false};
	// 已判定不归本进程,但本轮住户预算不够、还没开始排空(§4.7)。开始排空或映射回到本节点时清掉。
	bool releaseDeferred{false};
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

// 一条 MGET 的读数:第一个键是本节点号的持有者,其后是这一批场景的属主,下标与那一批 PendingCheck 对齐。
struct BatchReadings
{
	OwnerReading holder;
	std::vector<OwnerReading> owners;
};

// 拆 MGET 应答。空应答(断线)、错误应答、不是数组、元素个数不是 1 + sceneCount:整批 kUnreadable。
// 元素是 nil 为 kMissing,字符串为 kValue,其余 kUnreadable。
BatchReadings ReadBatch(const redisReply *reply, std::size_t sceneCount);

struct ApplyOutcome
{
	std::size_t releaseStarted{0}; // 本批第一次开始排空的实体数
	std::size_t destroyed{0};      // 本批销毁掉的实体数
	std::size_t deferred{0};       // 本批已判定不归本进程、但住户预算不够、留到下一轮的实体数
};

// 一批读数回来:逐个分类、推进核对状态;判定 kRelease 的在住户预算内调 SceneLifecycle::DestroyScene(先把住户改派回大世界,
// 没有绑定了才销毁;有绑定时实体留着,下一轮仍不归本进程就再调一次,已在排空的不再占预算)。
// slotHolder == kAnotherProcess 时整批读数都按 kSuperseded,不看映射。
// 读的途中实体已销毁、核对状态号变了(CreateScene 又命中了它)、scene_id 对不上的,这次读数作废。
// owners 与 checks 长度不等时整批作废。selfNodeId 为空时一律按读不到处理。
// 调用方负责:selfNodeId 与发读时相同;issuedAt 是这批读发出的时刻;budget 是本轮共用的预算。
ApplyOutcome ApplyReadings(const std::vector<PendingCheck> &checks, const std::vector<OwnerReading> &owners,
						   std::string_view selfNodeId, SlotHolder slotHolder, Clock::time_point issuedAt,
						   ReleaseBudget &budget);

// 一轮核对的去向。
enum class SweepOutcome : std::uint8_t
{
	kSent,             // 读已发出
	kNoScenes,         // 本地没有场景
	kRedisUnavailable, // 控制集群没连上
	kPreviousInFlight, // 上一轮还有应答没回来:不叠加
	kSendFailed,       // 命令没被集群客户端收下(守卫拒绝、slot 没有可用连接、排队满)
	kCount
};

// 指标用的名字,下标与 SweepOutcome 一一对应。长度由初始化列表推导,加了枚举项没加名字时 static_assert 失败。
inline constexpr std::string_view kSweepOutcomeNames[] = {
	"sent",               // SweepOutcome::kSent
	"no_scenes",          // SweepOutcome::kNoScenes
	"redis_unavailable",  // SweepOutcome::kRedisUnavailable
	"previous_in_flight", // SweepOutcome::kPreviousInFlight
	"send_failed",        // SweepOutcome::kSendFailed
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
	bool superseded{false};     // 最近一次读到的持有者判定:本节点号已被别的进程接走
	bool ownHolderKnown{false}; // 本进程读到过自己的持有者记录(HolderTracker::ownRevision != 0);没读到时认不出被接走
};
const Stats &ReadStats();
void ResetStatsForTest();

// 一次持有者读数:交给 holder 判定本节点号是不是已被别的进程接走,并更新进程级状态 ——「读到过本进程自己的记录」,
// 与「被接走」(读不到持有者记录时不改上一次的判定);「被接走」变化时打日志。返回判定,交给 ApplyReadings。
SlotHolder ObserveHolder(HolderTracker &holder, const OwnerReading &reading, std::string_view selfNodeId,
						 std::string_view selfUuid, bool identityConfirmed);
} // namespace scene_ownership

// 把核对计数挂到 Prometheus 出口(engine/core/metrics;设了 MMORPG_METRICS_PORT 才有出口)。全部只有低基数标签,
// 每个取值在启动时都以 0 导出(告警的 increase() 抓得到第一次):
//   scene_ownership_sweeps_total{outcome}         counter,outcome 取 kSweepOutcomeNames
//   scene_ownership_observations_total{result}    counter,result 取 kObservationNames
//   scene_ownership_releases_total{stage}         counter,stage = drain_started(开始排空)/ destroyed(实体已销毁)
//   scene_ownership_tracked_scenes                gauge,  此刻登记了核对状态的本地实体数
//   scene_ownership_releasing_scenes              gauge,  此刻已开始排空、还在等住户离开的本地实体数
//   scene_ownership_release_deferred_scenes       gauge,  此刻已判定不归本进程、等住户预算的本地实体数
//   scene_ownership_superseded                    gauge,  1 = 最近一次读到本节点号已被别的进程接走
//   scene_ownership_own_holder_known              gauge,  1 = 本进程读到过自己的持有者记录;托管着场景却一直是 0 时认不出被接走
// scene 启动时调用一次;重复调用只打一行 ERROR(注册表按名字拒绝)。告警在 deploy/k8s/scene-ownership-alerts.yaml。
void RegisterSceneOwnershipMetrics();

// 定时核对的宿主。归节点入口的运行期上下文所有(nodes/scene/main.cpp 的 SceneRuntimeContext),Start / Stop 成对。
// 定时器与 Redis 回调只捕获内部 Session 与本轮预算的 shared_ptr / weak_ptr(AGENTS.md §11.7):Stop 或析构之后到达的应答
// 一律丢弃。
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

	// 在节点 SetAfterStart 里调:那时 node_id 与 node_uuid 已是终值。identityConfirmed = 本节点 etcd 身份当前确认有效
	// (EtcdService::IsIdentityConfirmedFresh),只用来解读持有者记录(确认有效时别人的记录只能是旧值,见 HolderTracker);
	// 核对本身不看它,与 etcd 失联期间照常核对(§4.5)。它捕获的对象必须活到 Stop。空探针 = 永远不确认。
	// 重复调用先 Stop 再起。
	void Start(IdentityProbe identityConfirmed);

	// 普通停机、身份冲突关停、拆除前都调;幂等。之后到达的应答一律丢弃,不再动任何实体,也不再调探针。
	void Stop();

	// 一轮核对。定时器每 kSweepInterval 调一次;用例可直接调。没 Start 时回 nullopt、不计数。
	std::optional<scene_ownership::SweepOutcome> SweepOnce();

private:
	struct Session;
	static scene_ownership::SweepOutcome Sweep(const std::shared_ptr<Session> &session);
	// 发一条 MGET 读本节点号的持有者与这一批场景的属主;集群客户端收下回 true(应答一定会回来,断线时是空应答)。
	static bool SendOwnerRead(RedisClusterClient &redis, const std::shared_ptr<Session> &session,
							  std::vector<scene_ownership::PendingCheck> batch, const std::string &holderKey,
							  const std::string &selfNodeId, const std::string &selfUuid,
							  scene_ownership::Clock::time_point issuedAt,
							  const std::shared_ptr<scene_ownership::ReleaseBudget> &budget);

	std::shared_ptr<Session> session_;
	TimerTaskComp timer_;
};

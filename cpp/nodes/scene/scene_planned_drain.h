#pragma once

// scene 节点的计划排空编排(docs/design/scene-node-planned-drain.md §3、§4)。
//
// 阶段本身归 Node(Node::SetPlannedDrain:触发、预算、到期进普通停机),这里只管 scene 的业务:
//   Begin       关建场景准入;开始在 etcd 发布 admission = CLOSED(scene_manager 随即不再往本节点放新频道、新副本、新玩家,
//               并把本节点托着的世界频道迁走 —— 迁走之后发来的 DestroyScene 触发住户疏散);打开疏散限速。
//   每 100ms     按限速发起疏散;每秒对账一次 admission、打一行进度;到自疏散时刻把还没走的人排进疏散队列
//               (副本没打完、scene_manager 没赶上迁频道);记下有没有能接住疏散的同类。
//   IsComplete  本地没有玩家、每一次改派都有了结论;或连续 kNoTargetGrace 看不到能接住疏散的同类(整 zone 停机、
//               全部同类都在排空 —— 等下去只是空耗宽限期,剩下的人走普通停机的存盘下线)。
//
// Agones 形态的触发是排空标签:lifecycle 读到 mmorpg.io/drain 就拒绝新的分配许可,这里每秒看一眼,看到就请 Node
// 进计划排空,并记下「这是进程自己决定的终止」—— 停机时要先请 Agones 删掉本 GameServer(SDK /shutdown),
// 而 SIGTERM 触发的排空不能这样做(SIGTERM 本来就是 Agones 删 Pod 发的,见 main.cpp exitAllPlayers 的注释)。
//
// 线程模型:全部在节点 EventLoop 线程。对象由 SceneRuntimeContext 持有,先于 Node 构造、晚于 Node 析构;定时器回调捕获
// this 与 Node 引用,所以三条停机路径与 Node 析构之前都要 Stop()。

#include <chrono>
#include <cstddef>
#include <functional>
#include <optional>

#include "node/system/node/node_admission_advertise.h"
#include "time/comp/timer_task_comp.h"

class Node;

class ScenePlannedDrain
{
public:
	// 计划排空开始(Node::SetPlannedDrain 的 begin 钩子)。closeCreation:关建场景准入(SceneNodeGrpcImpl::CloseCreationAdmission)。
	void Begin(Node &node, const std::function<void()> &closeCreation);
	// 计划排空是否完成(Node::SetPlannedDrain 的 isComplete 钩子,Node 每 200ms 问一次)。
	bool IsComplete(Node &node);
	// 取消定时器。停机钩子、身份冲突关停与 Node 析构之前调用;幂等。
	void Stop();

	// Agones 形态:每秒看一眼 lifecycle 是否读到了排空标签,读到就进计划排空。SetAfterStart 里 lifecycle 启动之后调用。
	void WatchAgonesDrainLabel(Node &node);
	// 本次计划排空是否由 Agones 排空标签触发(停机时据此请 Agones 删掉本 GameServer)。
	bool TriggeredByAgonesLabel() const { return triggeredByAgonesLabel_; }

private:
	void Tick(Node &node);
	void ReconcileAdmission(Node &node);
	void LogProgress(Node &node);
	bool HasOpenEvacuationTarget(Node &node) const;

	TimerTaskComp tickTimer_;
	TimerTaskComp agonesLabelTimer_;
	node_admission::AdvertisePolicy advertisePolicy_;
	bool started_{false};
	bool triggeredByAgonesLabel_{false};
	bool selfEvacuationStarted_{false};
	bool noTargetLogged_{false};
	std::chrono::steady_clock::time_point selfEvacuateAt_{};
	std::chrono::steady_clock::time_point lastSecondTick_{};
	std::optional<std::chrono::steady_clock::time_point> noTargetSince_;
	std::size_t lastLoggedPlayers_{static_cast<std::size_t>(-1)};
	std::size_t lastLoggedQueued_{static_cast<std::size_t>(-1)};
};

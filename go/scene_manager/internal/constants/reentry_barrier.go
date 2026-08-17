package constants

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 场景所有权「再入屏障」(scene owner re-entry barrier)
//
// 核心不变量:**老节点最晚可能写入的时刻 < 新节点最早被允许接管的时刻**。
//
// 为什么需要它:etcd 租约到期时,Go SceneManager 与 C++ scene 节点对同一个信号
// **各自独立**反应,两条时间线之间没有任何互相等待:
//
//	T0          节点与 etcd 失联(进程仍在跑、仍在服务玩家、仍能写 Redis)
//	T0+TTL      etcd 租约到期
//	            ├─ etcd 发 DELETE → Go 立刻判死 → resolveScene / rebalance /
//	            │  孤儿收尾 改写 scene:{id}:node 并让新节点 CreateScene
//	            └─ C++ 老节点健康检查(每秒)发现租约过期 →
//	               BeginEmergencyRelocateAll:抄会话 → SavePlayerToRedis → drain
//	T0+TTL ~ +drain  老节点仍在写玩家最终态,而新节点已经 load 了更旧的快照
//	                 → 同一玩家双写 / 回档
//
// 所以 Go 在**看到 DELETE 之后**还必须再等 `C++ drain 预算 + 时钟余量`,才允许
// 把场景/玩家改派到新节点。TTL 那一段在信号到达时已经花掉了,增量屏障只有
// drain + skew 这一截。
//
// 硬崩溃(SIGKILL/OOM/段错误)不走优雅 drain,老节点直接不写了,屏障只是让
// 自愈晚一个屏障时长 —— 代价有界且可自愈。真正危险的是「老节点还活着还能写
// Redis,但 Go 已经判死改派」,那正是本屏障要挡住的分区情形。
//
// 完整设计见 docs/design/scene-owner-reentry-barrier.md §3.2。
// ---------------------------------------------------------------------------

// CppNodeDrainBudget 是 C++ scene 节点丢租约后的紧急疏散 drain 预算。
//
// ⚠️ **跨语言硬依赖,改一边必须改另一边**:该值在 C++ 侧是硬编码常量
// kDrainBudget(cpp/libs/engine/core/node/system/node/node.cpp,
// StartConflictDrainWatchdog 里),当前 = 15s。cpp/ 不属于本模块,Go 侧运行期
// 读不到它,只能在这里镜像一份并把依赖显式写出来。
//
// 后果不对称,所以宁可镜像值偏大:
//   - C++ 调大 drain 而这里没跟着调 → 屏障短于老节点实际停笔时刻,
//     双写窗口重新打开,而且是**静默的**(没有任何日志会报这件事);
//   - 这里调大而 C++ 没动 → 只是改派多等几秒,可自愈。
//
// 治本做法是把 drain 预算提到 bin/etc/base_deploy_config.yaml,让 Go 与 C++
// 读同一个值(理由同 pkg/placement 把脑裂常数集中成单一派生入口)。在那之前,
// 这里是 Go 侧的**唯一入口**,别在别处再写第二个 15。
const CppNodeDrainBudget = 15 * time.Second

// ReentryBarrierClockSkewMargin 是时钟 / 调度余量。
//
// death_at 由 Go 在**观察到 etcd DELETE 时**打点,而 drain 由 C++ 从**它自己
// 发现租约过期时**开始算,两者不是同一个时钟上的同一个点:
//   - etcd 事件投递、watch 重连、Go 侧 Redis 写入都有延迟 —— 让 death_at 偏晚,
//     屏障结束点跟着偏晚,方向是**安全**的;
//   - C++ 健康检查每秒一次,它发现租约过期本身就晚于真实过期点 —— 同样偏安全;
//   - 但 Go 与 C++ 分处不同 Pod,墙钟可能有偏差,方向**不定**,这是唯一不安全项。
//
// 5s 覆盖墙钟偏差并给 Redis 写入抖动留余地。
const ReentryBarrierClockSkewMargin = 5 * time.Second

// SceneReentryBarrier = C++ drain 预算 + 时钟余量 = 20s。
//
// **全模块唯一入口**:所有「老属主刚死,现在能不能接管」的判定都读它
// (或读它派生出的 ResolveSceneReentryBarrier 结果),不要在调用点写字面量。
const SceneReentryBarrier = CppNodeDrainBudget + ReentryBarrierClockSkewMargin

// NodeDeathMarkTTL 是 node:zone:{z}:{n}:death_at 标记的存活时间。
//
// 语义上「没有标记」= 没有观察到该节点近期死亡 = 允许接管。所以 TTL **只能
// 偏长不能偏短**:偏长只是让一条早已无意义的键多留一会儿(屏障早过了,判定结果
// 一样是允许);偏短则会在屏障还没走完时把证据抹掉,直接把双写窗口放开。
//
// 10 分钟能盖住 SceneManager 自身的短暂重启与 etcd watch 重连,同时远小于
// node_id 被回收复用的现实时间尺度。节点重新注册(etcd PUT)时标记会被显式
// 清掉,不依赖 TTL 过期。
const NodeDeathMarkTTL = 10 * time.Minute

// ValidateSceneReentryBarrier 机械校验屏障常数之间的关系,供进程启动时调用。
//
// 这不是运行期防御,是**防止后来者把常数改劈叉**:上面三个值互相有约束,
// 但 Go 没有编译期断言,单独看每一行都像是「一个合理的数字」。
func ValidateSceneReentryBarrier() error {
	if SceneReentryBarrier < CppNodeDrainBudget {
		return fmt.Errorf(
			"再入屏障 %v 小于 C++ drain 预算 %v:老节点停笔前新节点就能接管,双写窗口是敞开的",
			SceneReentryBarrier, CppNodeDrainBudget)
	}
	if NodeDeathMarkTTL <= SceneReentryBarrier {
		return fmt.Errorf(
			"death_at 标记 TTL %v 不大于再入屏障 %v:标记会在屏障走完前过期,等于屏障失效",
			NodeDeathMarkTTL, SceneReentryBarrier)
	}
	return nil
}

// ResolveSceneReentryBarrier 把配置里的屏障秒数解析成生效时长。
//
// 配置**只能把屏障调高,不能调低**:调低意味着 Go 敢在 C++ 还可能写 Redis 时
// 就改派,那是配置改不出来的正确性,不是运维偏好。configuredSeconds <= 0 表示
// 不覆盖,用常数。低于常数时钳回常数并返回 error —— 调用方负责把这条 error
// 记出来(启动日志里能看见),而不是让进程带着一个假的屏障值跑。
func ResolveSceneReentryBarrier(configuredSeconds int64) (time.Duration, error) {
	if configuredSeconds <= 0 {
		return SceneReentryBarrier, nil
	}
	d := time.Duration(configuredSeconds) * time.Second
	if d < SceneReentryBarrier {
		return SceneReentryBarrier, fmt.Errorf(
			"SceneReentryBarrierSeconds=%d(%v)低于安全下限 %v(= C++ drain %v + 时钟余量 %v),已钳回下限",
			configuredSeconds, d, SceneReentryBarrier, CppNodeDrainBudget, ReentryBarrierClockSkewMargin)
	}
	return d, nil
}

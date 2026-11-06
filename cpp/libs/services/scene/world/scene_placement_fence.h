#pragma once

// 世界频道的放置代次栅栏:scene 节点按代次拒收过期的 CreateScene / DestroyScene(纯规则,不碰 ECS、不碰网络)。
// 设计见 docs/design/world-channel-placement-generation.md §4。
//
// scene_manager 每次把一个世界频道放到某个节点上(换节点的四条路径),先从本 zone 的计数器分一个代次(>= 1),
// 这次尝试的建场景、落空回收、迁走后拆老实体都带它;init 向属主补建时带频道当前的代次。网络分区恢复后才送到的旧请求
// 按代次比较:
//   - 拆场景(代次 g)只拆不比 g 新的实体:本节点已按更新的代次(> g)建过这个场景,说明之后有过一次合法的放置,
//     这次拆除是过期的,忽略。
//   - 建场景(代次 g)不复活已拆掉的放置:本节点已按 >= g 的代次拆过这个场景,这次建场景是过期的,拒收。
// 代次 0 = 不带代次(副本、镜像,以及世界频道的退役拆除:排空、孤儿清理)。代次 0 的建场景按最旧的代次判,同样受「已拆」挡;
// 代次 0 的拆场景无条件执行,也不改记录,行为与没有代次时完全一样。退役拆除不留「已退役」墓碑:墓碑一旦记错(频道之后以
// 新的代次被放回来),这个场景就永远建不回这个节点;不留时最坏是退役之后晚到的旧建场景建出没有映射指向的孤儿,由节点的
// 归属对账收(docs/design/scene-node-ownership-reconcile.md)。
//
// 只有带过代次(>= 1)的场景才有记录,副本与镜像不占内存。记录活到进程退出、不清理:条数 = 本进程生命期内带代次放置到
// 本节点的世界频道数。gRPC 请求绑定连接,旧进程连接上的请求不会送到新进程,重启之后不需要旧记录。
//
// 线程:实例由 SceneLifecycle::PlacementFence() 按线程持有,只在节点 EventLoop 线程用,不加锁。

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <unordered_map>

class ScenePlacementFence
{
public:
	struct Record
	{
		// 放行过的最新建场景代次;0 = 没有按代次建过。
		std::uint64_t newestCreate = 0;
		// 放行过的最新拆场景代次;0 = 没按代次拆过。
		std::uint64_t destroyedThrough = 0;
	};

	// 判定一次建场景。返回 false 表示过期:调用方不建、不复用已有实体,回空应答(scene_manager 按建场景失败处理)。
	// 返回 true 时已记下这个代次;调用方随后必须建成或复用实体,所以要放在参数、许可等其它拒收理由之后调用。
	[[nodiscard]] bool AdmitCreate(std::uint64_t sceneId, std::uint64_t generation)
	{
		const auto it = records_.find(sceneId);
		if (it != records_.end() && IsDestroyedAt(it->second, generation))
		{
			return false;
		}
		if (generation == 0)
		{
			return true;
		}
		Record& record = (it != records_.end()) ? it->second : records_[sceneId];
		record.newestCreate = std::max(record.newestCreate, generation);
		return true;
	}

	// 判定一次拆场景。返回 false 表示过期:调用方忽略这次拆除,照常回 OK。代次 >= 1 放行时已记下这个代次;
	// 节点上没有这个实体也记,挡住同一次尝试晚到的建场景(gRPC 建场景要先等 Agones 许可,拆场景可能先被处理)。
	// 代次 0 一律放行,不改记录。
	[[nodiscard]] bool AdmitDestroy(std::uint64_t sceneId, std::uint64_t generation)
	{
		if (generation == 0)
		{
			return true;
		}
		Record& record = records_[sceneId];
		if (record.newestCreate > generation)
		{
			return false;
		}
		record.destroyedThrough = std::max(record.destroyedThrough, generation);
		return true;
	}

	// 这个场景的记录,没有时为 nullptr。供日志与用例。
	[[nodiscard]] const Record* Find(std::uint64_t sceneId) const
	{
		const auto it = records_.find(sceneId);
		return it == records_.end() ? nullptr : &it->second;
	}

	[[nodiscard]] std::size_t Size() const { return records_.size(); }

private:
	// 已按 >= generation 的代次拆过。代次 0 的建场景遇到任何一次按代次的拆除记录都算。
	static bool IsDestroyedAt(const Record& record, std::uint64_t generation)
	{
		return record.destroyedThrough != 0 && generation <= record.destroyedThrough;
	}

	std::unordered_map<std::uint64_t, Record> records_;
};

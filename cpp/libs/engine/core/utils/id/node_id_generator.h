#pragma once

#include <cstddef>
#include <cstdint>
#include <limits>

#include "muduo/base/Logging.h"

// IDs are transient; invalidated on server restart
//
// 位布局:[node : sizeof(T)*8 - kNodeBit][seq : kNodeBit]。
//
// ⚠️ 两条边界必须守住,否则 node 段会被污染,`node_id(guid)` 解出**别人的** node_id:
//   ① seq 必须掩码到 kNodeBit 位。seq 自增到 2^kNodeBit 就会进位溢进 node 段 ——
//      载体 uint32 + kNodeBit=17 时,单进程累计 131072 次 Generate 即触发,
//      而 gate 的 session id 正是每个连接自增一次,13 万次连接完全够得着。
//      掩码后 seq 回绕复用旧号,这与本类"transient id"的语义一致(调用方本就要自查活跃集合去重);
//      而溢出会把号发到别的 node 的地盘上,跨进程撞号且无法自查。两害相权取回绕。
//   ② node_id 必须能装进 node 段。超出即被 `<< kNodeBit` 截断成另一个 node_id,
//      是静默的跨节点撞号,这里 fail-closed 拒绝。
template <class T, std::size_t kNodeBit>
class TransientNodeCompositeIdGenerator
{
public:
	static std::size_t node_bit() { return kNodeBit; }

	// seq 段掩码:只保留低 kNodeBit 位,进位不得溢进 node 段。
	static constexpr T kSeqMask = static_cast<T>((static_cast<T>(1) << kNodeBit) - 1);
	// node 段能表达的最大值。载体宽度减去 seq 段宽度即为 node 段宽度。
	static constexpr T kMaxNodeId =
		static_cast<T>(std::numeric_limits<T>::max() >> kNodeBit);

	void set_node_id(T node_id)
	{
		if (node_id > kMaxNodeId) {
			// fail-closed:截断后会与别的节点撞号,宁可不发号也不发错号。
			LOG_FATAL << "TransientNodeCompositeIdGenerator: node id " << node_id
				<< " exceeds max " << kMaxNodeId << " for a " << (sizeof(T) * 8 - kNodeBit)
				<< "-bit node segment";
			return;
		}
		node_id_ = static_cast<T>(node_id << kNodeBit);
	}

	T node_id(T guid)
	{
		return guid >> kNodeBit;
	}

	T Generate()
	{
		seq_ = static_cast<T>((seq_ + 1) & kSeqMask);
		return node_id_ | seq_;
	}

	T node_id_prefix() const { return node_id_; }

	T LastId()
	{
		return  node_id_ | seq_;
	}
private:
	T node_id_{ 0 };
	T seq_{ 0 };
};

using TransientNode12BitCompositeIdGenerator  = TransientNodeCompositeIdGenerator<uint32_t, 12>;

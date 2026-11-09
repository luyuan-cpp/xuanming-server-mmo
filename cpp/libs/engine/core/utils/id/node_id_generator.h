#pragma once

#include <cstdint>
#include <limits>

#include "muduo/base/Logging.h"

// IDs are transient; invalidated on server restart
//
// 位布局:[node : sizeof(T)*8 - kSequenceBits][seq : kSequenceBits]。
//
// ⚠️ 两条边界必须守住,否则 node 段会被污染,`node_id(guid)` 解出**别人的** node_id:
//   ① seq 必须掩码到 kSequenceBits 位。seq 自增到 2^kSequenceBits 就会进位溢进 node 段 ——
//      载体 uint32 + kSequenceBits=17 时,单进程累计 131072 次 Generate 即触发,
//      而 gate 的 session id 正是每个连接自增一次,13 万次连接完全够得着。
//      掩码后 seq 回绕复用旧号,这与本类"transient id"的语义一致(调用方本就要自查活跃集合去重);
//      而溢出会把号发到别的 node 的地盘上,跨进程撞号且无法自查。两害相权取回绕。
//   ② node_id 必须能装进 node 段。超出即被 `<< kSequenceBits` 截断成另一个 node_id,
//      是静默的跨节点撞号,这里 fail-closed 拒绝。
template <class T, std::size_t kSequenceBits>
class TransientNodeCompositeIdGenerator
{
public:
	static std::size_t sequence_bit_count() { return kSequenceBits; }

	// seq 段掩码:只保留低 kSequenceBits 位,进位不得溢进 node 段。
	static constexpr T kSeqMask = static_cast<T>((static_cast<T>(1) << kSequenceBits) - 1);
	// node 段能表达的最大值。载体宽度减去 seq 段宽度即为 node 段宽度。
	static constexpr T kMaxNodeId =
		static_cast<T>(std::numeric_limits<T>::max() >> kSequenceBits);

	void set_node_id(T node_id)
	{
		if (node_id > kMaxNodeId) {
			// fail-closed:截断后会与别的节点撞号,宁可不发号也不发错号。
			LOG_FATAL << "TransientNodeCompositeIdGenerator: node id " << node_id
				<< " exceeds max " << kMaxNodeId << " for a " << (sizeof(T) * 8 - kSequenceBits)
				<< "-bit node segment";
			return;
		}
		node_id_ = static_cast<T>(node_id << kSequenceBits);
	}

	T node_id(T guid)
	{
		return guid >> kSequenceBits;
	}

	T Generate()
	{
		seq_ = static_cast<T>((seq_ + 1) & kSeqMask);
		return node_id_ | seq_;
	}

	// 对外短期身份不能回绕复用:node 未分配或序号耗尽时拒绝,耗尽后保持饱和。
	// 原 Generate 的回绕契约保留,只有明确要求不复用的号源使用本入口。
	bool TryGenerateWithoutReuse(T& value)
	{
		value = 0;
		if (node_id_ == 0 || seq_ == kSeqMask) {
			return false;
		}
		++seq_;
		value = node_id_ | seq_;
		return true;
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

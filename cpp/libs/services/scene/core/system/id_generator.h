#pragma once

#include "core/utils/id/node_id_generator.h"
#include "player/system/save_seq.h"

using TransientNode32BitCompositeIdGenerator = TransientNodeCompositeIdGenerator<uint64_t, 32>;

class ThreadLocalIdGeneratorManager {
public:
    ThreadLocalIdGeneratorManager() = default;

    ThreadLocalIdGeneratorManager(const ThreadLocalIdGeneratorManager&) = delete;
    ThreadLocalIdGeneratorManager& operator=(const ThreadLocalIdGeneratorManager&) = delete;

    void SetNodeId(uint32_t nodeId) {
        buffIdGenerator.set_node_id(nodeId);
        skillIdGenerator.set_node_id(nodeId);
        actorNetIdGenerator.set_node_id(nodeId);
    }

	TransientNode32BitCompositeIdGenerator buffIdGenerator;
	TransientNode32BitCompositeIdGenerator skillIdGenerator;
	TransientNode32BitCompositeIdGenerator actorNetIdGenerator;
	// 玩家存盘序号 save_seq 的进程级发号器(docs/design/db-write-fencing-mysql.md §7.2)。与 node_id 无关,SetNodeId 不碰它;
	// 加载玩家时 Observe 播种(InitPlayerFromAllData),确定要写盘时 Next(SavePlayerImpl)。只在 scene 逻辑线程上用。
	save_seq::SaveSeqAllocator saveSeq;
};

extern thread_local ThreadLocalIdGeneratorManager tlsIdGeneratorManager;

#pragma once

// 单独一个头文件、且**不 include rdkafkacpp.h**:node.h / kafka_manager.h 都要按值
// 传这个结构体(带默认实参,必须是完整类型),而它们被几乎所有 TU 包含;
// 把 rdkafka 的 C++ 包装层拖进去会连带 LIBRDKAFKA_STATICLIB 的定义顺序要求
// (见 kafka_consumer.h 顶部的长注释)。

#include <cstdint>

// 显式分区消费的策略(控制面命令 topic 用;见
// docs/design/control-plane-topic-partitioning-20260908.md)。
// 只在 KafkaConsumer::init() 的 partitions 非空时生效,默认值 = 保持改造前的行为。
struct KafkaPartitionAssignPolicy {
	// >0:启动时向 broker 核对 topic 的分区数。broker **答了一个不同的数字**就起不来。
	//
	// 这是分区契约的门禁,不是洁癖:broker 开着 auto.create.topics.enable 且
	// num.partitions=1,任何生产者抢在 topic-init 之前发一条消息,topic 就被建成
	// 1 分区;此时 partition = node_id % 256 算出来的 200 号分区根本不存在,
	// assign 之后消费者永远收不到任何东西,而 Kafka 一个错都不报 ——
	// 症状是"登录卡在分配场景",排查成本极高。宁可起不来。
	//
	// 边界:**查不到**(broker 不可达 / 元数据还没广播开)只记 ERROR 继续,不拒绝启动。
	// 改造前的 subscribe() 在 broker 不可达时同样成功、由后台重连兜底;把它变成启动
	// 致命,等于让"Kafka 晚起一会儿"直接打死整个 gate/scene。
	int32_t expectedTopicPartitions = 0;

	// true:assign 时显式定位到分区末尾(OFFSET_END),不读历史。
	//
	// 一个分区上坐着几百个节点,历史里绝大多数是**别人的**命令;而发给本进程的
	// 命令不可能出现在本进程启动之前(target_instance_id 是启动时新生成的 uuid)。
	// 所以重放历史只有成本没有收益:白读几十万条再全部丢弃。
	bool startAtEnd = false;

	// true:关掉 auto.commit。
	//
	// 必须与 startAtEnd 一起用。共享分区意味着同一个 (group, topic, partition)
	// 会被几百个进程写同一个 offset,谁都不能代表别人的进度;提交上去只会互相覆盖,
	// 并在重启后按别人的 offset 起跳、漏掉自己的命令。既然每次启动都从末尾开始,
	// offset 就没有任何持久化的意义。
	bool ownOffsets = false;
};

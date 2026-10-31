// LIBRDKAFKA_STATICLIB 必须在包含 rdkafka.h 之前定义,与 kafka_producer.h / kafka_consumer.h 同一口径
// (那边的长注释解释了为什么本仓库按 STATICLIB 声明 librdkafka 符号)。本文件只用 C 接口,
// 这些函数无论是否带 dllimport 都经 lib/rdkafka.lib 导入库链接到 rdkafka.dll。
#ifndef LIBRDKAFKA_STATICLIB
#define LIBRDKAFKA_STATICLIB
#endif

// rdkafka.h 在 Windows 上会 include <winsock2.h>,放在最前面,免得别的头先把 <windows.h>
// (连带旧的 winsock.h)拖进来造成重定义。
#include "librdkafka/src/rdkafka.h"

#include <gtest/gtest.h>

#include <cstddef>
#include <iomanip>
#include <sstream>
#include <string>

// ---------------------------------------------------------------------------
// kafka_partition_contract_test — KafkaProducer 钉住的默认分区器(consistent_random)。
//
// 同一业务实体的 key 落同一分区是 AGENTS §7.3 的前提。要盯住两件事:
//   1) 运行时加载的 librdkafka 就是 vendored 版本,且它的默认分区器是 consistent_random;
//   2) KafkaProducer::createProducer 在 CONF_GLOBAL 上钉的 partitioner 确实落到默认 topic conf
//      (否则显式钉值是空操作)。
//
// 确定、可重复:只调 librdkafka 的 conf 对象,不建 rd_kafka_t、不连 broker、不起线程、不读墙钟。
//
// main() 在 kafka_command_test.cpp 里,本文件不再定义。
// ---------------------------------------------------------------------------

namespace {

constexpr const char *kContractPartitioner = "consistent_random";

// 读 topic conf 上的字符串属性;读不到返回空串(断言处会把空串当失败打印出来)。
std::string GetTopicConfString(const rd_kafka_topic_conf_t *tconf, const char *name)
{
    char buf[128] = {};
    size_t size = sizeof(buf);
    if (rd_kafka_topic_conf_get(tconf, name, buf, &size) != RD_KAFKA_CONF_OK)
    {
        return {};
    }
    return std::string(buf);
}

std::string HexVersion(int version)
{
    std::ostringstream os;
    os << "0x" << std::hex << std::setw(8) << std::setfill('0') << version;
    return os.str();
}

} // namespace

// 本工程链的是 rdkafka.dll 的导入库(lib/rdkafka.lib),头文件取自 third_party/librdkafka。
// 两者版本不一致时,本套件证明的是运行时 DLL 而不是 vendored 源码 —— 所以先把
// 「运行时 == vendored」本身变成被测条件,并把运行时版本串记进测试报告。
TEST(KafkaPartitionerContract, VendoredDefaultPartitionerIsConsistentRandom)
{
    const char *runtimeVersionStr = rd_kafka_version_str();
    const std::string runtimeVersion = runtimeVersionStr != nullptr ? runtimeVersionStr : "(null)";
    RecordProperty("librdkafka_runtime_version", runtimeVersion);
    EXPECT_EQ(rd_kafka_version(), RD_KAFKA_VERSION)
        << "运行时 rdkafka.dll 版本 " << HexVersion(rd_kafka_version()) << " (" << runtimeVersion
        << ") 与 vendored 头文件 RD_KAFKA_VERSION " << HexVersion(RD_KAFKA_VERSION)
        << " 不一致:本套件证明的是运行时 DLL 的行为,先让 bin/rdkafka.dll 与 third_party/librdkafka 对齐再看结果。";

    rd_kafka_topic_conf_t *tconf = rd_kafka_topic_conf_new();
    ASSERT_NE(tconf, nullptr);
    const std::string partitioner = GetTopicConfString(tconf, "partitioner");
    rd_kafka_topic_conf_destroy(tconf);

    EXPECT_EQ(partitioner, kContractPartitioner)
        << "librdkafka 的默认分区器不再是 consistent_random。KafkaProducer::createProducer 已显式钉值,"
           "但凡是没走它的生产者都会换分区;升级 librdkafka 前先核对。";
}

// 与 createProducer 的设置路径一致:RdKafka::Conf(CONF_GLOBAL)::set 直接调 rd_kafka_conf_set
// (src-cpp/ConfImpl.cpp),partitioner 是 topic 级属性,全局作用域不认识它,librdkafka 会
// fallthru 到默认 topic conf(没有就现建一个)。这条断言失败 = 显式钉值成了空操作。
TEST(KafkaPartitionerContract, ExplicitPartitionerOnGlobalConfReachesDefaultTopicConf)
{
    rd_kafka_conf_t *conf = rd_kafka_conf_new();
    ASSERT_NE(conf, nullptr);

    char errstr[512] = {};
    const rd_kafka_conf_res_t res =
        rd_kafka_conf_set(conf, "partitioner", kContractPartitioner, errstr, sizeof(errstr));
    EXPECT_EQ(res, RD_KAFKA_CONF_OK) << errstr;

    // 返回的 topic conf 归 conf 所有,不得单独 destroy;用 EXPECT 而不是 ASSERT,保证下面能释放 conf。
    const rd_kafka_topic_conf_t *defaultTopicConf = rd_kafka_conf_get_default_topic_conf(conf);
    EXPECT_NE(defaultTopicConf, nullptr)
        << "在全局 conf 上设 partitioner 之后没有生成默认 topic conf:显式钉值不会作用到任何 topic";
    if (defaultTopicConf != nullptr)
    {
        EXPECT_EQ(GetTopicConfString(defaultTopicConf, "partitioner"), kContractPartitioner);
    }

    rd_kafka_conf_destroy(conf);
}

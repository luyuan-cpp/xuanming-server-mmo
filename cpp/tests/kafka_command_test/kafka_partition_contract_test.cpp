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
#include <cstdint>
#include <exception>
#include <filesystem>
#include <iomanip>
#include <limits>
#include <set>
#include <sstream>
#include <string>
#include <system_error>
#include <vector>

#include <yaml-cpp/yaml.h>

#include "player/constants/player.h"
#include "proto/db/db_task.pb.h"

// ---------------------------------------------------------------------------
// kafka_partition_contract_test — 玩家存盘 DBTask 的 Kafka 分区契约(C++ 写侧)。
// 契约文档:docs/design/db-task-kafka-partition-contract.md「分区函数契约」。
//
// 契约:key = 玩家 id 的无符号十进制 ASCII(无前导零);partition = CRC-32/IEEE(key) mod N,
// 按 uint32 无符号取模 —— 即 librdkafka consistent_random 对非空 key 的
// rd_crc32(key) % partition_cnt。login 发读请求用 Go 的 kafkautil.DBTaskPartition /
// NewDBTaskPartitioner(sarama NewConsistentCRCHashPartitioner),scene 写存盘用 librdkafka。
// 任何一侧漂移,同一玩家的读写就不在同一分区:go/db 的读后写屏障静默失效,写换分区还会被判
// orderingIncomparable 送进死信。编译器和集成测试都看不见,所以两侧读同一份 golden 向量
// go/shared/kafkautil/testdata/dbtask_partition_vectors.json,各自拿本侧真实实现对拍。
//
// 要盯住四件事:
//   1) 运行时加载的 librdkafka 就是 vendored 版本,且它的默认分区器是 consistent_random;
//   2) KafkaProducer::createProducer 在 CONF_GLOBAL 上钉的 partitioner 确实落到默认 topic conf
//      (否则显式钉值是空操作);
//   3) librdkafka consistent_random 对每条向量算出的分区与 Go 侧一致;
//   4) DbTaskKafkaKey 的字节与 Go 的 strconv.FormatUint(id, 10) 一致(key 字节就是分区函数的输入)。
//
// 确定、可重复:只调 librdkafka 的纯函数与 conf 对象,不建 rd_kafka_t、不连 broker、不起线程、
// 不读墙钟。向量对不上时以机器结果为准回报,不直接改向量文件。
//
// main() 在 kafka_command_test.cpp 里,本文件不再定义。
// ---------------------------------------------------------------------------

namespace {

namespace fs = std::filesystem;

constexpr const char *kDbTaskVectorRelativePath =
    "go/shared/kafkautil/testdata/dbtask_partition_vectors.json";

// 契约分区器名。与 kafka_producer.cpp createProducer 里显式设置的值必须相同。
constexpr const char *kContractPartitioner = "consistent_random";

// 向量必须覆盖的分区数:常用的 7/10/16/20,外加 INT32_MAX —— 后者那一条能把
// 「uint32 无符号取模」与「有符号取模取反」「sarama referenceAbs」三种写法区分开。
constexpr int32_t kRequiredPartitionCounts[] = {
    7, 10, 16, 20, (std::numeric_limits<int32_t>::max)()};

// 从若干起点逐级向上(最多 8 层)找向量文件。起点:
//   - 当前工作目录:run_cpp_tests.ps1 以仓库根为 cwd 启动;main() 找到配置后会把 cwd 换到
//     bin/(仓库根的下一级),上溯一级即可;
//   - 本源文件所在目录(MSBuild 以绝对路径传源文件,__FILE__ 即绝对路径):cwd 不在仓库内时兜底。
// 不复用 kafka_command_test.cpp 的 FindVectorFile:它写死的是另一份向量的相对路径,
// 为复用它去改签名属于无关重构。
fs::path FindDbTaskVectorFile()
{
    std::vector<fs::path> startPoints;
    std::error_code cwdEc;
    const fs::path cwd = fs::current_path(cwdEc);
    if (!cwdEc)
    {
        startPoints.push_back(cwd);
    }
    const fs::path sourceDir = fs::path(__FILE__).parent_path();
    if (sourceDir.is_absolute())
    {
        startPoints.push_back(sourceDir);
    }

    for (const auto &start : startPoints)
    {
        fs::path dir = start;
        for (int depth = 0; depth < 8; ++depth)
        {
            const fs::path candidate = dir / kDbTaskVectorRelativePath;
            std::error_code existsEc;
            if (fs::exists(candidate, existsEc))
            {
                return candidate;
            }
            if (!dir.has_parent_path() || dir.parent_path() == dir)
            {
                break;
            }
            dir = dir.parent_path();
        }
    }
    return {};
}

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

// 对非空 key 调 librdkafka 的 consistent_random。rkt 传 nullptr 是安全的:非空 key 走
// rd_kafka_msg_partitioner_consistent(只算 rd_crc32(key) % partition_cnt,不碰 rkt);
// 空 key 才会走随机分区并访问 rkt —— 所以调用方必须先断言 key 非空、partitionCount > 0。
int32_t LibrdkafkaPartition(const std::string &key, int32_t partitionCount)
{
    return rd_kafka_msg_partitioner_consistent_random(
        nullptr, key.data(), key.size(), partitionCount, nullptr, nullptr);
}

std::string HexVersion(int version)
{
    std::ostringstream os;
    os << "0x" << std::hex << std::setw(8) << std::setfill('0') << version;
    return os.str();
}

} // namespace

// ---------------------------------------------------------------------------
// 1. 运行时库就是 vendored 版本,且默认分区器 = 契约分区器
// ---------------------------------------------------------------------------

// 本工程链的是 rdkafka.dll 的导入库(lib/rdkafka.lib),头文件取自 third_party/librdkafka。
// 两者版本不一致时,本套件证明的是运行时 DLL 而不是 vendored 源码 —— 所以先把
// 「运行时 == vendored」本身变成被测条件,并把运行时版本串记进测试报告。
TEST(DbTaskPartitionContract, VendoredDefaultPartitionerIsConsistentRandom)
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
           "但凡是没走它的生产者都会换分区;升级 librdkafka 前先按契约文档核对(换函数 = 换 TopicGeneration)。";
}

// ---------------------------------------------------------------------------
// 2. CONF_GLOBAL 上设的 partitioner 落到默认 topic conf
// ---------------------------------------------------------------------------

// 与 createProducer 的设置路径一致:RdKafka::Conf(CONF_GLOBAL)::set 直接调 rd_kafka_conf_set
// (src-cpp/ConfImpl.cpp),partitioner 是 topic 级属性,全局作用域不认识它,librdkafka 会
// fallthru 到默认 topic conf(没有就现建一个)。这条断言失败 = 显式钉值成了空操作。
TEST(DbTaskPartitionContract, ExplicitPartitionerOnGlobalConfReachesDefaultTopicConf)
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

// ---------------------------------------------------------------------------
// 3. 与 Go 侧共读的 golden 向量对拍
// ---------------------------------------------------------------------------

TEST(DbTaskPartitionContract, LibrdkafkaConsistentRandomMatchesSharedVectors)
{
    const fs::path vectorFile = FindDbTaskVectorFile();
    ASSERT_FALSE(vectorFile.empty())
        << "找不到共享向量文件 " << kDbTaskVectorRelativePath
        << ";它是 C++/Go 分区契约对拍的唯一凭据,缺了就等于没测。";

    const YAML::Node root = YAML::LoadFile(vectorFile.string());

    // 自检值:标准 CRC-32/IEEE("123456789") = 0xCBF43926。librdkafka 不导出 rd_crc32,
    // 用它的分区函数间接核对:对每个必测的 N,分区必须等于 check.crc32 % N(按 uint32 取模)。
    const YAML::Node check = root["check"];
    ASSERT_TRUE(check) << "向量文件缺 check";
    const std::string checkKey = check["key"].as<std::string>();
    const uint32_t checkCrc = check["crc32"].as<uint32_t>();
    EXPECT_EQ(checkKey, "123456789");
    EXPECT_EQ(checkCrc, 0xCBF43926u);
    ASSERT_FALSE(checkKey.empty());
    for (const int32_t n : kRequiredPartitionCounts)
    {
        EXPECT_EQ(LibrdkafkaPartition(checkKey, n),
                  static_cast<int32_t>(checkCrc % static_cast<uint32_t>(n)))
            << "check key=" << checkKey << " n=" << n;
    }

    const YAML::Node cases = root["cases"];
    ASSERT_TRUE(cases && cases.IsSequence()) << "向量文件缺 cases";

    std::size_t caseCount = 0;
    std::set<int32_t> seenPartitionCounts;
    for (const auto &c : cases)
    {
        const std::string key = c["key"].as<std::string>();
        const int32_t n = c["n"].as<int32_t>();
        const int32_t expectPartition = c["expect_partition"].as<int32_t>();

        // 空 key 不在契约内(librdkafka 对空 key 随机分区,还会访问 rkt);n <= 0 会除零。
        // 向量里出现这两种说明向量本身坏了,不往下调库。
        ASSERT_FALSE(key.empty()) << "case #" << caseCount << " 的 key 为空:空 key 不在分区契约内";
        ASSERT_GT(n, 0) << "case #" << caseCount << " key=" << key;

        EXPECT_EQ(LibrdkafkaPartition(key, n), expectPartition)
            << "key=" << key << " n=" << n
            << ":librdkafka consistent_random 与共享向量不一致(以机器结果为准回报,不直接改向量)";
        seenPartitionCounts.insert(n);
        ++caseCount;
    }

    EXPECT_GE(caseCount, 45u) << "向量条数少于约定的 45 条,文件可能被截断";
    for (const int32_t n : kRequiredPartitionCounts)
    {
        EXPECT_TRUE(seenPartitionCounts.count(n) > 0) << "向量缺少 n=" << n << " 的用例";
    }
}

// ---------------------------------------------------------------------------
// 4. Kafka key 的字节与 Go strconv.FormatUint(id, 10) 一致
// ---------------------------------------------------------------------------

TEST(DbTaskPartitionContract, KafkaKeyMatchesGoFormatUint)
{
    EXPECT_EQ(DbTaskKafkaKey(0), "0");
    EXPECT_EQ(DbTaskKafkaKey(1), "1");
    EXPECT_EQ(DbTaskKafkaKey(static_cast<uint64_t>((std::numeric_limits<int64_t>::max)())),
              "9223372036854775807");
    EXPECT_EQ(DbTaskKafkaKey((std::numeric_limits<uint64_t>::max)()), "18446744073709551615");

    const fs::path vectorFile = FindDbTaskVectorFile();
    ASSERT_FALSE(vectorFile.empty())
        << "找不到共享向量文件 " << kDbTaskVectorRelativePath
        << ";它是 C++/Go 分区契约对拍的唯一凭据,缺了就等于没测。";

    const YAML::Node root = YAML::LoadFile(vectorFile.string());
    const YAML::Node cases = root["cases"];
    ASSERT_TRUE(cases && cases.IsSequence()) << "向量文件缺 cases";

    // 向量里的 key 必须是规范十进制(无符号、无前导零、无空白与正负号):解析成 uint64 后用
    // DbTaskKafkaKey 格式化回来,必须逐字节还原。std::stoull 会接受前导空白、正负号、前导零和
    // 尾随垃圾,这些非规范写法都会在「还原」这一步暴露出来。
    std::size_t keyCount = 0;
    for (const auto &c : cases)
    {
        const std::string key = c["key"].as<std::string>();
        uint64_t playerId = 0;
        try
        {
            playerId = std::stoull(key, nullptr, 10);
        }
        catch (const std::exception &e)
        {
            ADD_FAILURE() << "key=\"" << key << "\" 不是 uint64 十进制: " << e.what();
            continue;
        }
        EXPECT_EQ(DbTaskKafkaKey(playerId), key)
            << "key 不是规范十进制(Go strconv.FormatUint 的输出),两侧 key 字节不同就不在同一分区";
        ++keyCount;
    }
    EXPECT_GT(keyCount, 0u) << "向量文件没有可用的 key";
}

// 与 Go 的同名协议回归共用固定字节，防止任一端悄悄更换操作编号或 wire type。
TEST(DbTaskOperationContract, ReadAndWriteUseCompactEnumEncoding)
{
    taskpb::DBTask read;
    read.set_op(taskpb::DB_OP_READ);
    EXPECT_EQ(read.SerializeAsString(), std::string("\x18\x01", 2));

    taskpb::DBTask write;
    write.set_op(taskpb::DB_OP_WRITE);
    EXPECT_EQ(write.SerializeAsString(), std::string("\x18\x02", 2));

    taskpb::DBTask decoded;
    ASSERT_TRUE(decoded.ParseFromString(std::string("\x18\x02", 2)));
    EXPECT_EQ(decoded.op(), taskpb::DB_OP_WRITE);
}

TEST(DbTaskOperationContract, LegacyStringDoesNotBecomeAnExecutableOperation)
{
    taskpb::DBTask decoded;
    ASSERT_TRUE(decoded.ParseFromString(std::string("\x1a\x05" "write", 7)));
    EXPECT_EQ(decoded.op(), taskpb::DB_OP_UNSPECIFIED);
}

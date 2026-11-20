"""C++ 节点自己的代码不新起线程:线程只能出现在登记过的清单里。不编译、不访问网络或运行服务。

负责人 2026-11-19 定:C++ 节点除主线程外不再开线程。已有的后台线程逐个评估后保留,登记在下面的
ALLOWED 里,每条写明保留原因;清单之外出现起线程的写法,这里判红。

新代码需要异步时,把阻塞调用改成非阻塞并挂到主 EventLoop,多步等待用 loop_async::AwaitCallback
协程。确实做不到(例如第三方库只给阻塞接口)再登记进清单,原因写清楚、过评审。

看不到的:第三方库内部的线程(librdkafka 等),运行期由 ThreadMonitor 观测
(docs/design/thread-count-monitoring_zh.md)。测试代码、仓内自带的 muduo 源码不在范围内。
这是文本筛查,不解析 C++:宏拼出来的、藏在别名后面的写法看不到,评审时照同一条规则人工核对。
"""

from pathlib import Path
import re
import unittest


REPO = Path(__file__).resolve().parents[3]

# 扫描范围:节点与共享库的手写代码,加上导表器生成的表加载器(它起启动期的加载线程)。
SCANNED_DIRS = ("cpp/libs", "cpp/nodes", "cpp/generated/table/code")
# 仓内自带的第三方源码,不归本规则管。
VENDORED_PREFIXES = ("cpp/libs/engine/muduo_windows/",)
# 本机构建缓存不属于仓库源码,与 test_cpp_grpc_dependency_boundary.py 同一口径。
BUILD_DIR_NAMES = {"build", "Debug", "Release", "x64", "CMakeFiles", "boost-download"}
SOURCE_SUFFIXES = {".h", ".hpp", ".cpp", ".cc"}

# 起线程或持有线程对象的写法。std::thread::id / hardware_concurrency 只是查询,不算。
THREAD_TOKEN = re.compile(
    r"std::thread\b(?!\s*::\s*(?:id|hardware_concurrency)\b)"
    r"|std::jthread\b"
    r"|std::async\b"
    r"|\bEventLoopThread(?:Pool)?\b"
    r"|\bAsyncLogging\b"
    r"|\bThreadPool\b"
    r"|\bpthread_create\b"
    r"|\b_beginthread(?:ex)?\b"
    r"|\bCreateThread\s*\("
)
# 注释、字符串与字符字面量:里面提到线程不算。字面量也要整段认出来,免得把里面的 // 当成注释开头。
COMMENT_OR_LITERAL = re.compile(
    r"//[^\n]*|/\*.*?\*/|\"(?:\\.|[^\"\\\n])*\"|'(?:\\.|[^'\\\n])*'", re.S
)
INCLUDE_LINE = re.compile(r"\s*#\s*include\b")

# 路径(相对仓库根)→ 保留原因。登记项里已经没有起线程的写法时同样判红,清单只能跟着代码走。
KAFKA_POLL = "Kafka 消费轮询线程:45k 压测后为保住 poll 间隔有意加的,消息仍投回主 EventLoop 处理"
AGONES_WORKER = "Agones 生命周期 worker:对 SDK sidecar 的 HTTP 是阻塞的,不能进主 EventLoop"
ALLOWED = {
    "cpp/generated/table/code/table_loader.cpp":
        "启动期并行加载配置表:工作线程数不超过 CPU 核数,LoadTablesAsync 返回前全部 join",
    "cpp/libs/engine/core/node/system/node/node.h":
        "muduo AsyncLogging 的落盘线程:磁盘写不能卡主 EventLoop",
    "cpp/libs/engine/core/node/system/node/node.cpp":
        "维护流程里阻塞的文件与子进程操作,只在维护期间存在",
    "cpp/libs/engine/infra/messaging/kafka/kafka_consumer.h": KAFKA_POLL,
    "cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp": KAFKA_POLL,
    "cpp/libs/engine/infra/messaging/kafka/kafka_manager.cpp":
        "维护流程里等消费者就绪 / 排空的阻塞操作,只在维护期间存在",
    "cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.h": AGONES_WORKER,
    "cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.cpp": AGONES_WORKER,
}


def thread_hits(text):
    """返回 [(行号, 命中的写法)]。#include 行不算:真用到线程类型,代码行里一定还会出现。"""
    code = COMMENT_OR_LITERAL.sub(lambda match: "\n" * match.group(0).count("\n"), text)
    hits = []
    for number, line in enumerate(code.split("\n"), 1):
        if not INCLUDE_LINE.match(line):
            hits.extend((number, match.group(0)) for match in THREAD_TOKEN.finditer(line))
    return hits


def scanned_sources():
    for directory in SCANNED_DIRS:
        for path in sorted((REPO / directory).rglob("*")):
            relative = path.relative_to(REPO)
            if (path.suffix in SOURCE_SUFFIXES and path.is_file()
                    and not relative.as_posix().startswith(VENDORED_PREFIXES)
                    and not any(part in BUILD_DIR_NAMES or part.startswith(".build") for part in relative.parts)):
                yield relative.as_posix(), path


class CppThreadSourcesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scanned = 0
        cls.hits = {}
        for relative, path in scanned_sources():
            cls.scanned += 1
            found = thread_hits(path.read_text(encoding="utf-8", errors="replace"))
            if found:
                cls.hits[relative] = found

    def test_scan_covers_the_cpp_tree(self):
        self.assertGreater(self.scanned, 400, "扫到的源文件太少,多半是目录挪了位置,守卫在空转")

    def test_threads_only_in_registered_files(self):
        unregistered = sorted(
            f"  {path}:{line}  {token}"
            for path, found in self.hits.items() if path not in ALLOWED
            for line, token in found
        )
        self.assertFalse(
            unregistered,
            "清单之外出现了起线程的写法。先改成挂主 EventLoop(多步等待用 loop_async::AwaitCallback);"
            "确实做不到,再连同原因登记进本文件的 ALLOWED:\n" + "\n".join(unregistered),
        )

    def test_registered_files_still_start_threads(self):
        stale = sorted(path for path in ALLOWED if path not in self.hits)
        self.assertFalse(stale, "这些登记项里已经没有起线程的写法,从 ALLOWED 删掉:\n  " + "\n  ".join(stale))

    def test_token_matching(self):
        for source, expected in (
            ("std::thread worker_;", ["std::thread"]),
            ("pollThread_ = std::thread([this] { Poll(); });", ["std::thread"]),
            ("std::vector<std::jthread> workers;", ["std::jthread"]),
            ("return std::async(std::launch::async, task);", ["std::async"]),
            ("muduo::net::EventLoopThread thread_;", ["EventLoopThread"]),
            ("server.setThreadNum(4); EventLoopThreadPool pool(loop, name);", ["EventLoopThreadPool"]),
            ("muduo::AsyncLogging logSystem;", ["AsyncLogging"]),
            ("pthread_create(&tid, nullptr, Run, this);", ["pthread_create"]),
        ):
            with self.subTest(source=source):
                self.assertEqual([token for _, token in thread_hits(source)], expected)
        for harmless in (
            "std::thread::id loopThread;",
            "std::max<std::size_t>(1, std::thread::hardware_concurrency())",
            "// 所有调用均在 std::async 工作线程上",
            "/* std::jthread\n   析构即 join */ int x;",
            'LOG_INFO << "std::thread started";',
            '#include "muduo/net/EventLoopThreadPool.h"',
            "#include <muduo/base/AsyncLogging.h>",
        ):
            with self.subTest(source=harmless):
                self.assertEqual(thread_hits(harmless), [])

    def test_hit_line_numbers_survive_block_comments(self):
        self.assertEqual(thread_hits("/* a\n b */\nstd::thread t;\n"), [(3, "std::thread")])


if __name__ == "__main__":
    unittest.main()

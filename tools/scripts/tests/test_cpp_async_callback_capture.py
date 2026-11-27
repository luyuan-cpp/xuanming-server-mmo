"""C++ 异步回调禁止捕获裸 this / 默认捕获 / 裸引用 / std::bind(this|shared_from_this) 的静态门禁与自测。

只读源码,不编译、不访问网络或运行服务。对应规范见 AGENTS.md §11.7
(docs/rules/engineering-principles.md §7「新代码异步回调标准」)。

分层门禁:
- 本地提交门禁:.githooks/pre-commit 调用本文件 `--staged`,毫秒级检查暂存区改动的 C++ 生产源码;
- 远端 CI 门禁:.github/workflows/cpp-build-ci.yml 的 `test_cpp*` 每次 PR / Push 全仓扫描生产代码。

检查范围(cpp/libs 与 cpp/nodes 的手写生产代码):
- 异步注册调用点:muduo set*Callback / register*Callback、EventLoop 与 TimerTaskComp 的
  runAfter/runEvery/runAt/runInLoop/queueInLoop/RunAfter/RunEvery/RunAt、RPC/gRPC/Redis/Agones/Asio 的
  CallMethod/CallUnary/SendRequest/SendAsync/BeginShutdown/AcquireAllocationPermitAsync/command/
  AsyncCommand/SendCommand/async_wait/async_read/async_write/async_connect;
- 违例写法:
  1. lambda 默认捕获 `[=]` / `[&]`;
  2. lambda 捕获裸 `this` / `*this` / 引用 `&var` / 危险初始化捕获(`self = this`、`p = &obj`、`std::addressof`);
  3. 异步回调实参中出现 `std::bind(..., this, ...)` 或 `std::bind(..., shared_from_this(), ...)`;
- 合法写法(放行):
  - ECS 业务按值捕获实体句柄与业务 ID:`[entity = entity, playerId]`;
  - 基础设施对象捕获弱引用:`[weakSelf = weak_from_this()]`、`[weak = std::weak_ptr<T>(this->member)]`;
  - 按值/移动提取成员而不捕获 `this`:`[acceptor = std::move(this->acceptor)]`、`[id = this->id]`;
  - 异步回调体内部调用的同步算法 lambda(如 `std::sort(..., [&]{})`)不误报。

排除口径(与 test_cpp_thread_sources.py / test_cpp_no_raw_pointer_member.py 一致):
- 排除 third_party、generated、构建输出目录、cpp/libs/engine/muduo_windows、cpp/plugin;
- 排除测试代码(cpp/tests/、*/tests/*、*_test.{cpp,cc,cxx,h,hpp}):单测常在栈上同步驱动 `loop.loop()` 并用
  `[&loop] { loop.quit(); }` 定时退出,不归本生产门禁管。

已知文本筛查边界:
- 本脚本只解析直接传入异步注册点的 lambda 与 std::bind(含 std::move / 三元表达式包裹),不跨变量追踪
  `auto cb = [this]{}; loop->queueInLoop(cb);` 或宏展开拼出的回调;这类写法由代码评审按同一规则把关。

历史白名单(ALLOWED)采用「(路径, 方法名, 归一化签名) -> (精确次数, 原因)」配额制:
- 不绑定行号,文件增删注释不会无故失效;
- 老文件里只要新增哪怕 1 条未登记或超配额的违例,pre-commit 与 CI 都会立即判红;
- 重构消除了旧违例后,全仓测试会自动提醒收缩/删除过期配额(单调收紧,只减不增)。

运行(仓库根目录):
- 全仓自测:python tools/scripts/tests/test_cpp_async_callback_capture.py -v
- 仅查暂存区改动(pre-commit 调用):python tools/scripts/tests/test_cpp_async_callback_capture.py --staged
"""

from collections import Counter
import io
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[3]
SCAN_DIRS = ("cpp/libs", "cpp/nodes")
SOURCE_SUFFIXES = (".h", ".hpp", ".hh", ".hxx", ".cpp", ".cc", ".cxx")
TEST_FILE_SUFFIXES = ("_test.cpp", "_test.cc", "_test.cxx", "_test.h", "_test.hpp")
SKIPPED_DIRECTORIES = {"third_party", "generated", "build", "bin", "x64", "Debug", "Release", "CMakeFiles", "tests"}
SKIPPED_PREFIXES = ("cpp/libs/engine/muduo_windows/", "cpp/plugin/")

RAW_STRING_OPEN = re.compile(r'R"([^()\s\\]{0,16})\(')

ASYNC_METHOD_PATTERN = (
    r"[sS]et[A-Z]\w*Callback|"
    r"[rR]egister\w*Callback|"
    r"runAfter|runEvery|runAt|runInLoop|queueInLoop|"
    r"RunAfter|RunEvery|RunAt|"
    r"CallMethod|CallUnary|SendRequest|SendAsync|BeginShutdown|"
    r"AcquireAllocationPermitAsync|command|AsyncCommand|SendCommand|"
    r"async_wait|async_read|async_write|async_connect"
)
# 匹配成员调用(obj.RunAfter / ptr->queueInLoop)或同类内直接调用(RunAfter),允许带单层模板实参(registerMessageCallback<T>)
ASYNC_CALL_RE = re.compile(
    rf"(?:(?:->|\.)\s*|\b)({ASYNC_METHOD_PATTERN})\s*(?:<[^<>]*>)?\s*\("
)
BIND_RE = re.compile(r"\bstd::bind\s*\(")
LAMBDA_TRAILER_KW = re.compile(r"(?:mutable|constexpr|consteval|static)\b")
RAW_THIS_RE = re.compile(r"\bthis\b(?!\s*->)")
SHARED_FROM_THIS_RE = re.compile(r"\bshared_from_this\s*\(")
ADDRESS_OF_RE = re.compile(r"(?:^|[(,])\s*&(?!&)|\bstd::addressof\s*\(")
DECL_PREFIX_TOKENS = ("void", "bool", "int", "auto", "virtual", "inline", "static", "override", "::")

# 历史已核验的合法例外:(仓库相对路径, 方法名, 归一化签名) -> (允许出现次数, 保留原因)
# 仅允许登记经确认的同寿命成员/进程级宿主/弱引用哨兵或 TLS 计数器,新代码须遵守 AGENTS.md §11.7 四选一标准。
NODE_HOST_LIFETIME = "Node 为进程主循环宿主,TimerTaskComp 同寿命且停机前经两跳 queueInLoop 排空后才析构"
REDIS_TOKEN_GUARD = "RedisManager 内部 lifetimeToken(weak_ptr) 哨兵保护,回调首行 lock() 失败即返回"
TIMER_MEMBER_LIFETIME = "TimerTaskComp 为宿主同寿命成员,析构/Shutdown 时自动取消定时器"
DISPATCHER_MEMBER_LIFETIME = "ProtobufDispatcher 与宿主同寿命,仅在宿主存活期间被同步派发调用"
MAIN_PROCESS_LIFETIME = "节点 main() 进程级宿主对象与主 EventLoop 同寿命,存活至 loop.loop() 退出"

ALLOWED = {
    ("cpp/libs/engine/core/network/rpc_server.cc", "setConnectionCallback", "std::bind(this)"): (
        1,
        "AGENTS.md §11.7 显式列举的同寿命例外:tcpServer 为 RpcServer 自身成员",
    ),
    ("cpp/libs/engine/core/node/system/discovery/service_discovery_manager.cpp", "RunAfter", "lambda[this]"): (
        1,
        TIMER_MEMBER_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/etcd/etcd_service.cpp", "RunAfter", "lambda[this]"): (
        1,
        TIMER_MEMBER_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/etcd/kill_switch_etcd_sync.cpp", "RunAfter", "lambda[this]"): (
        2,
        "kill_switch_etcd_sync.h 明确禁止拷贝与移动,两个 TimerTaskComp 为同寿命成员",
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "BeginShutdown", "lambda[this]"): (
        1,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "RunAfter", "lambda[this, capturedUuid]"): (
        1,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "RunAfter", "lambda[this]"): (
        2,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "RunEvery", "lambda[this, drainBudget]"): (
        1,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "RunEvery", "lambda[this, reRegistrationRequested = false]"): (
        1,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "RunEvery", "lambda[this]"): (
        3,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "queueInLoop", "lambda[this, loop]"): (
        2,
        NODE_HOST_LIFETIME,
    ),
    ("cpp/libs/engine/core/node/system/node/node.cpp", "queueInLoop", "lambda[this]"): (
        6,
        NODE_HOST_LIFETIME,
    ),
    (
        "cpp/libs/engine/core/node/system/node/node.h",
        "RunEvery",
        "lambda[this, &node, requiredNodeTypes, onReady = std::move(onReady), nodeName, logEveryNTicks]",
    ): (
        1,
        "DependencyGate 与进程级 Node 同寿命,probeTimer 为其成员",
    ),
    ("cpp/libs/engine/core/node/system/node/node_entry.h", "runEvery", "lambda[&loop]"): (
        1,
        MAIN_PROCESS_LIFETIME,
    ),
    ("cpp/libs/engine/core/time/comp/timer_task_boost_comp.cpp", "async_wait", "lambda[this, self = shared_from_this()]"): (
        3,
        "历史 Boost.Asio 定时器包装,闭包同时持有 self = shared_from_this() 保活",
    ),
    (
        "cpp/libs/engine/infra/agones/agones_gameserver_lifecycle.cpp",
        "runAfter",
        "lambda[alive = std::weak_ptr<int>(aliveToken), this, waiterId]",
    ): (
        1,
        "GameServerLifecycle 内部 aliveToken(weak_ptr) 哨兵保护,回调先 alive.lock() 才访问 this",
    ),
    (
        "cpp/libs/engine/thread_context/redis_manager.cpp",
        "queueInLoop",
        "lambda[token = std::weak_ptr<LifetimeToken>(lifetimeToken), this, generation = livenessGeneration]",
    ): (
        1,
        REDIS_TOKEN_GUARD,
    ),
    (
        "cpp/libs/engine/thread_context/redis_manager.cpp",
        "queueInLoop",
        "lambda[token = std::weak_ptr<LifetimeToken>(lifetimeToken), this, generation]",
    ): (
        1,
        REDIS_TOKEN_GUARD,
    ),
    (
        "cpp/libs/engine/thread_context/redis_manager.cpp",
        "runEvery",
        "lambda[token = std::weak_ptr<LifetimeToken>(lifetimeToken), this, generation]",
    ): (
        1,
        REDIS_TOKEN_GUARD,
    ),
    (
        "cpp/libs/engine/thread_context/redis_manager.cpp",
        "runEvery",
        "lambda[token = std::weak_ptr<LifetimeToken>(lifetimeToken), this]",
    ): (
        1,
        REDIS_TOKEN_GUARD,
    ),
    (
        "cpp/libs/engine/thread_context/redis_manager.cpp",
        "setConnectCallback",
        "lambda[token = std::weak_ptr<LifetimeToken>(lifetimeToken), this]",
    ): (
        1,
        REDIS_TOKEN_GUARD,
    ),
    (
        "cpp/libs/engine/thread_context/redis_manager.cpp",
        "setDisconnectCallback",
        "lambda[token = std::weak_ptr<LifetimeToken>(lifetimeToken), this]",
    ): (
        1,
        REDIS_TOKEN_GUARD,
    ),
    ("cpp/libs/services/scene/player/system/player_lifecycle.cpp", "command", "lambda[&inFlight, done = std::move(done)]"): (
        1,
        "SubmitCountedPlayerRedis 仅接受 thread_local 计数引用(tlsExitReleaseMarksInFlight / tlsRelocateHandoffMarksInFlight)",
    ),
    ("cpp/nodes/battle/client/battle_client_edge.cpp", "registerMessageCallback", "lambda[this]"): (
        5,
        DISPATCHER_MEMBER_LIFETIME,
    ),
    ("cpp/nodes/battle/logic/battle_room_manager.cpp", "RunAfter", "lambda[this, battleId]"): (
        3,
        "BattleRoomManager 持有各房间及其 TimerTaskComp,房间随管理器同寿命销毁并自动取消定时器",
    ),
    ("cpp/nodes/battle/logic/battle_room_manager.cpp", "RunEvery", "lambda[this, battleId]"): (
        1,
        "BattleRoomManager 持有各房间及其 TimerTaskComp,房间随管理器同寿命销毁并自动取消定时器",
    ),
    ("cpp/nodes/gate/handler/rpc/client_session_handler.cpp", "registerMessageCallback", "std::bind(this)"): (
        2,
        DISPATCHER_MEMBER_LIFETIME,
    ),
    ("cpp/nodes/gate/main.cpp", "RunEvery", "lambda[&n]"): (
        1,
        MAIN_PROCESS_LIFETIME,
    ),
    ("cpp/nodes/gate/main.cpp", "setConnectionCallback", "lambda[&context]"): (
        1,
        MAIN_PROCESS_LIFETIME,
    ),
    ("cpp/nodes/gate/main.cpp", "setMessageCallback", "lambda[&context]"): (
        1,
        MAIN_PROCESS_LIFETIME,
    ),
    ("cpp/nodes/scene/scene_planned_drain.cpp", "RunEvery", "lambda[this, &node]"): (
        2,
        "ScenePlannedDrain 与进程级 Node 同寿命,tickTimer / agonesLabelTimer 为其成员",
    ),
}


def strip_comments_and_literals(text):
    """注释与预处理行去掉,字符串与字符字面量换成空引号;换行原样保留,保证行号不变。"""
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        following = text[i + 1] if i + 1 < n else ""
        if c == "/" and following == "/":
            while i < n and text[i] != "\n":
                i += 1
        elif c == "/" and following == "*":
            end = text.find("*/", i + 2)
            end = n if end < 0 else end + 2
            out.append("\n" * text.count("\n", i, end))
            i = end
        elif c == "R" and following == '"' and RAW_STRING_OPEN.match(text, i):
            match = RAW_STRING_OPEN.match(text, i)
            closing = ")" + match.group(1) + '"'
            end = text.find(closing, i)
            end = n if end < 0 else end + len(closing)
            out.append('""' + "\n" * text.count("\n", i, end))
            i = end
        elif c == '"' or (c == "'" and not (i > 0 and text[i - 1].isalnum())):
            end = i + 1
            while end < n and text[end] != c and text[end] != "\n":
                end += 2 if text[end] == "\\" else 1
            out.append(c + c)
            i = end + 1
        elif c == "#" and text[text.rfind("\n", 0, i) + 1:i].strip() == "":
            while i < n:
                end = text.find("\n", i)
                if end < 0:
                    i = n
                elif text[end - 1] == "\\" or text[end - 2:end] == "\\\r":
                    out.append("\n")
                    i = end + 1
                    continue
                else:
                    i = end
                break
        else:
            out.append(c)
            i += 1
    return "".join(out)


def _find_matching(text, open_pos, open_ch, close_ch):
    depth = 0
    for i in range(open_pos, len(text)):
        c = text[i]
        if c == open_ch:
            depth += 1
        elif c == close_ch:
            depth -= 1
            if depth == 0:
                return i
    return -1


def _split_top_level_args(text):
    """按顶层逗号切分函数调用实参;只追踪成对的 ()/[]/{},避免把比较运算符 `<` 误当模板括号。"""
    parts, cur = [], []
    paren = bracket = brace = 0
    for c in text:
        if c == "(":
            paren += 1
        elif c == ")":
            paren = max(0, paren - 1)
        elif c == "[":
            bracket += 1
        elif c == "]":
            bracket = max(0, bracket - 1)
        elif c == "{":
            brace += 1
        elif c == "}":
            brace = max(0, brace - 1)
        if c == "," and paren == 0 and bracket == 0 and brace == 0:
            parts.append("".join(cur))
            cur = []
        else:
            cur.append(c)
    parts.append("".join(cur))
    return parts


def _split_capture_items(capture_text):
    """切分 lambda 捕获列表里的各项,支持模板类型(如 `weak = std::weak_ptr<Foo>(x)`)里的逗号。"""
    parts, cur = [], []
    paren = bracket = brace = angle = 0
    for c in capture_text:
        if c == "(":
            paren += 1
        elif c == ")":
            paren = max(0, paren - 1)
        elif c == "[":
            bracket += 1
        elif c == "]":
            bracket = max(0, bracket - 1)
        elif c == "{":
            brace += 1
        elif c == "}":
            brace = max(0, brace - 1)
        elif c == "<" and paren == 0 and bracket == 0 and brace == 0:
            angle += 1
        elif c == ">" and angle > 0 and paren == 0 and bracket == 0 and brace == 0:
            angle -= 1
        if c == "," and paren == 0 and bracket == 0 and brace == 0 and angle == 0:
            item = "".join(cur).strip()
            if item:
                parts.append(item)
            cur = []
        else:
            cur.append(c)
    tail = "".join(cur).strip()
    if tail:
        parts.append(tail)
    return parts


def _bad_capture_reasons(capture_text):
    """检查归一化捕获列表中的违规项,返回违规原因列表(空列表表示合规)。"""
    reasons = []
    for item in _split_capture_items(capture_text):
        norm = " ".join(item.split())
        if norm == "=":
            reasons.append("default[=]")
        elif norm == "&":
            reasons.append("default[&]")
        elif norm in ("this", "*this"):
            reasons.append(norm)
        elif norm.startswith("&"):
            reasons.append(f"ref[{norm}]")
        elif "=" in norm:
            _, rhs = norm.split("=", 1)
            rhs = rhs.strip()
            if RAW_THIS_RE.search(rhs) or ADDRESS_OF_RE.search(rhs):
                reasons.append(f"init[{norm}]")
    return reasons


def _scan_arg_surface(arg_text, base_offset, code):
    """扫描单个实参表面表达式里的 lambda 与 std::bind,不钻入 lambda 函数体 `{...}`。

    不钻入函数体的原因:
    1. 避免把异步回调体里的同步算法 lambda(如 `std::sort(..., [&]{})`)误报为异步回调;
    2. 若回调体内部还有嵌套异步调用(如内层 `queueInLoop`),外层 `ASYNC_CALL_RE.finditer`
       会独立扫描到该调用点并按其自身行号精确报告,避免重复报错。
    """
    findings = []
    i, n = 0, len(arg_text)
    while i < n:
        c = arg_text[i]
        if c == "[":
            end_br = _find_matching(arg_text, i, "[", "]")
            if end_br > i:
                prev = arg_text[:i].rstrip()
                prev_ch = prev[-1] if prev else ""
                after = arg_text[end_br + 1:].lstrip()
                is_lambda = (
                    (not prev_ch or prev_ch in "(,=?:{")
                    and (
                        after.startswith(("(", "{", "<"))
                        or bool(LAMBDA_TRAILER_KW.match(after))
                    )
                )
                if is_lambda:
                    cap = " ".join(arg_text[i + 1:end_br].split())
                    reasons = _bad_capture_reasons(cap)
                    if reasons:
                        line = code.count("\n", 0, base_offset + i) + 1
                        findings.append((line, f"lambda[{cap}]", reasons))
                    # 跳过整个 lambda 参数列表、尾置返回与函数体 `{...}`
                    k = end_br + 1
                    while k < n and arg_text[k] != "{":
                        if arg_text[k] == "(":
                            p_end = _find_matching(arg_text, k, "(", ")")
                            if p_end < 0:
                                break
                            k = p_end + 1
                        else:
                            k += 1
                    if k < n and arg_text[k] == "{":
                        b_end = _find_matching(arg_text, k, "{", "}")
                        i = (b_end + 1) if b_end >= 0 else n
                        continue
            i += 1
        elif arg_text.startswith("std::bind", i) and (
            i == 0 or not (arg_text[i - 1].isalnum() or arg_text[i - 1] == "_")
        ):
            bm = BIND_RE.match(arg_text, i)
            if bm:
                end_p = _find_matching(arg_text, bm.end() - 1, "(", ")")
                if end_p > 0:
                    bargs = arg_text[bm.end():end_p]
                    line = code.count("\n", 0, base_offset + i) + 1
                    if SHARED_FROM_THIS_RE.search(bargs):
                        findings.append((line, "std::bind(shared_from_this)", ["std::bind(shared_from_this)"]))
                    elif RAW_THIS_RE.search(bargs):
                        findings.append((line, "std::bind(this)", ["std::bind(this)"]))
                    i = end_p + 1
                    continue
            i += 1
        else:
            i += 1
    return findings


def async_callback_violations(text):
    """返回 [(行号, 异步方法名, 归一化签名, 违规原因列表)]。"""
    code = strip_comments_and_literals(text)
    violations = []
    for match in ASYNC_CALL_RE.finditer(code):
        method = match.group(1)
        matched_prefix = match.group(0)
        if not matched_prefix.startswith(("->", ".")):
            before = code[max(0, match.start() - 40):match.start()].rstrip()
            if before.endswith(DECL_PREFIX_TOKENS):
                continue
        open_paren = match.end() - 1
        close_paren = _find_matching(code, open_paren, "(", ")")
        if close_paren < 0:
            continue
        args_str = code[open_paren + 1:close_paren]
        offset = open_paren + 1
        for arg in _split_top_level_args(args_str):
            for line, sig, reasons in _scan_arg_surface(arg, offset, code):
                violations.append((line, method, sig, reasons))
            offset += len(arg) + 1
    return violations


def should_check_path(relative_path):
    """按仓库相对路径判断文件是否属于异步回调捕获检查范围(仅 C++ 生产源码)。"""
    normalized = relative_path.replace("\\", "/").lstrip("./")
    if not any(normalized.startswith(f"{root}/") for root in SCAN_DIRS):
        return False
    if not normalized.endswith(SOURCE_SUFFIXES) or normalized.endswith(TEST_FILE_SUFFIXES):
        return False
    if normalized.startswith(SKIPPED_PREFIXES):
        return False
    parts = normalized.split("/")[:-1]
    return not any(part in SKIPPED_DIRECTORIES or part.startswith(".") for part in parts)


def cpp_production_sources():
    """逐个产出 (仓库相对路径, 源码) 供全仓检查。"""
    for root in SCAN_DIRS:
        base_dir = REPO / root
        if not base_dir.exists():
            continue
        for folder, directories, names in os.walk(base_dir):
            relative_folder = Path(folder).relative_to(REPO).as_posix() + "/"
            directories[:] = sorted(
                name for name in directories
                if name not in SKIPPED_DIRECTORIES
                and not name.startswith(".")
                and not (relative_folder + name + "/").startswith(SKIPPED_PREFIXES)
            )
            for name in sorted(names):
                if name.endswith(SOURCE_SUFFIXES) and not name.endswith(TEST_FILE_SUFFIXES):
                    path = Path(folder) / name
                    yield relative_folder + name, path.read_text(encoding="utf-8-sig", errors="replace")


def evaluate_file_violations(relative_path, text, allowed=ALLOWED):
    """将单文件命中结果与白名单配额比对,返回 (未登记或超配额报错列表, Counter[(path, method, sig)])。"""
    hits = async_callback_violations(text)
    by_key = {}
    for line, method, sig, reasons in hits:
        key = (relative_path, method, sig)
        by_key.setdefault(key, []).append((line, reasons))

    problems = []
    counts = Counter()
    for key, entries in sorted(by_key.items()):
        counts[key] = len(entries)
        allowed_entry = allowed.get(key)
        allowed_count = allowed_entry[0] if allowed_entry else 0
        if len(entries) > allowed_count:
            _, method, sig = key
            lines_str = ", ".join(str(line) for line, _ in entries)
            reasons_str = ", ".join(entries[0][1])
            if allowed_count == 0:
                problems.append(
                    f"  {relative_path}:{lines_str}: {method}(...) captured {sig} ({reasons_str})"
                )
            else:
                problems.append(
                    f"  {relative_path}:{lines_str}: {method}(...) captured {sig} "
                    f"({len(entries)} occurrences exceed allowed quota {allowed_count}; {reasons_str})"
                )
    return problems, counts


def _read_staged_blobs(paths, repo):
    """单次 git cat-file --batch 读取全部暂存区内容,避免 Windows 下逐文件拉起进程。"""
    query = "".join(f":{path}\n" for path in paths).encode("utf-8")
    proc = subprocess.run(
        ["git", "-C", str(repo), "cat-file", "--batch"],
        input=query,
        capture_output=True,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(proc.stderr.decode("utf-8", errors="replace").strip() or "git cat-file failed")
    out = proc.stdout
    pos = 0
    results = []
    for path in paths:
        newline = out.find(b"\n", pos)
        if newline < 0:
            raise RuntimeError(f"unexpected git cat-file header for {path}")
        header = out[pos:newline].decode("utf-8", errors="replace")
        pos = newline + 1
        fields = header.rsplit(" ", 2)
        if len(fields) != 3 or fields[1] != "blob":
            raise RuntimeError(f"staged object is not a blob for {path}: {header}")
        size = int(fields[2])
        blob = out[pos:pos + size]
        pos += size + 1
        results.append((path, blob.decode("utf-8-sig", errors="replace")))
    return results


def check_staged(repo=REPO, allowed=ALLOWED, stdout=None, stderr=None):
    """检查 Git 暂存区中改动的 C++ 生产源码(供 .githooks/pre-commit 调用)。"""
    out = sys.stdout if stdout is None else stdout
    err = sys.stderr if stderr is None else stderr
    proc = subprocess.run(
        ["git", "-C", str(repo), "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z"],
        capture_output=True,
        check=False,
    )
    if proc.returncode != 0:
        detail = proc.stderr.decode("utf-8", errors="replace").strip()
        print(f"[async-callback-capture] ERROR: git diff --cached failed: {detail}", file=err)
        return 2
    staged_paths = [entry.decode("utf-8", errors="replace") for entry in proc.stdout.split(b"\0") if entry]
    targets = [path for path in staged_paths if should_check_path(path)]
    if not targets:
        return 0
    try:
        blobs = _read_staged_blobs(targets, repo)
    except RuntimeError as exc:
        print(f"[async-callback-capture] ERROR: failed to read staged files: {exc}", file=err)
        return 2

    problems = []
    for relative_path, text in blobs:
        file_problems, _ = evaluate_file_violations(relative_path, text, allowed=allowed)
        problems.extend(file_problems)
    if problems:
        print("[async-callback-capture] Forbidden async callback capture(s) detected in staged C++ files:", file=err)
        for problem in problems:
            print(problem, file=err)
        print(
            "[async-callback-capture] Follow AGENTS.md §11.7: capture [entity = entity, playerId] for ECS work, "
            "[weakSelf = weak_from_this()] for shared infrastructure, LoopTaskScope for multi-step async, "
            "or weak_ptr<State> for cold-path handles. Never use [=], [&], raw this, or std::bind(this/shared_from_this).",
            file=err,
        )
        return 1
    print(f"[async-callback-capture] Checked {len(targets)} staged C++ file(s): OK.", file=out)
    return 0


class RecognitionTest(unittest.TestCase):
    def hit_summary(self, source):
        return [(line, method, sig) for line, method, sig, _ in async_callback_violations(source)]

    def test_default_this_and_reference_captures_are_reported(self):
        source = (
            "void Foo::Arm() {\n"
            "  loop->queueInLoop([=] { Work(); });\n"
            "  loop->runInLoop([&] { Work(); });\n"
            "  loop->runAfter(1.0, [this] { Work(); });\n"
            "  loop->runEvery(2.0, [*this] { Work(); });\n"
            "  timer.RunAfter(3.0, [&node, id] { Use(node, id); });\n"
            "}\n"
        )
        self.assertEqual(
            [
                (2, "queueInLoop", "lambda[=]"),
                (3, "runInLoop", "lambda[&]"),
                (4, "runAfter", "lambda[this]"),
                (5, "runEvery", "lambda[*this]"),
                (6, "RunAfter", "lambda[&node, id]"),
            ],
            self.hit_summary(source),
        )

    def test_dangerous_init_captures_are_reported(self):
        source = (
            "void Foo::Arm(Bar& obj) {\n"
            "  loop->queueInLoop([self = this] { self->Work(); });\n"
            "  loop->queueInLoop([p = static_cast<Foo*>(this)] { p->Work(); });\n"
            "  loop->queueInLoop([p = &obj] { p->Work(); });\n"
            "  loop->queueInLoop([p = (&obj)] { p->Work(); });\n"
            "  loop->queueInLoop([p = &this->member] { p->Work(); });\n"
            "  loop->queueInLoop([p = std::addressof(obj)] { p->Work(); });\n"
            "  loop->queueInLoop([&r = obj] { r.Work(); });\n"
            "}\n"
        )
        self.assertEqual([2, 3, 4, 5, 6, 7, 8], [line for line, _, _ in self.hit_summary(source)])

    def test_safe_value_weak_and_member_move_captures_are_allowed(self):
        source = (
            "void Foo::Arm(entt::entity entity, uint64_t playerId, int mask, int bits) {\n"
            "  loop->queueInLoop([] { Tick(); });\n"
            "  loop->queueInLoop([entity = entity, playerId] { Apply(entity, playerId); });\n"
            "  loop->runAfter(1.0, [weakSelf = weak_from_this()] {\n"
            "    if (auto self = weakSelf.lock()) self->Work();\n"
            "  });\n"
            "  timer.RunEvery(5.0, [weak = std::weak_ptr<Session>(this->session)] { (void)weak; });\n"
            "  loop->queueInLoop([acceptor = std::move(this->acceptor), id = this->id] { (void)acceptor; (void)id; });\n"
            "  tlsPlayerRedis.command([done = std::move(done), flag = (mask & bits)](auto*, auto* r) { done(r); }, \"GET k\");\n"
            "}\n"
        )
        self.assertEqual([], self.hit_summary(source))

    def test_std_bind_with_this_or_shared_from_this_is_reported(self):
        source = (
            "void Foo::Bind() {\n"
            "  server.setConnectionCallback(std::bind(&Foo::OnConn, this, _1));\n"
            "  client.setMessageCallback(std::bind(&Foo::OnMsg, shared_from_this(), _1, _2, _3));\n"
            "  loop->queueInLoop(std::bind(&Foo::Tick, this->shared_from_this()));\n"
            "  loop->queueInLoop(std::bind(&FreeFunc, playerId));\n"
            "}\n"
        )
        self.assertEqual(
            [
                (2, "setConnectionCallback", "std::bind(this)"),
                (3, "setMessageCallback", "std::bind(shared_from_this)"),
                (4, "queueInLoop", "std::bind(shared_from_this)"),
            ],
            self.hit_summary(source),
        )

    def test_templated_and_wrapped_async_calls_are_detected(self):
        source = (
            "void Foo::Setup(bool fast, int a, int b) {\n"
            "  dispatcher.registerMessageCallback<ClientRequest>([this](const auto& c) { OnReq(c); });\n"
            "  dispatcher.registerMessageCallback<LoginReq>(std::bind(&Foo::OnLogin, this, _1));\n"
            "  loop->runAfter(a < b ? 1.0 : 2.0, std::move([this] { Work(); }));\n"
            "  loop->queueInLoop(fast ? [this] { Fast(); } : [] { Slow(); });\n"
            "  loop->queueInLoop([this]<typename T>(T v) mutable -> void { Use(v); });\n"
            "  loop->queueInLoop(Callback{[this] { Braced(); }});\n"
            "}\n"
        )
        self.assertEqual(
            [
                (2, "registerMessageCallback", "lambda[this]"),
                (3, "registerMessageCallback", "std::bind(this)"),
                (4, "runAfter", "lambda[this]"),
                (5, "queueInLoop", "lambda[this]"),
                (6, "queueInLoop", "lambda[this]"),
                (7, "queueInLoop", "lambda[this]"),
            ],
            self.hit_summary(source),
        )

    def test_inner_sync_lambdas_are_ignored_and_nested_async_reported_once(self):
        source = (
            "void Foo::Nested() {\n"
            "  loop->queueInLoop([weak = weak_from_this()] {\n"
            "    auto self = weak.lock();\n"
            "    if (!self) return;\n"
            "    std::sort(items.begin(), items.end(), [&](int a, int b) { return a < b; });\n"
            "    self->loop->queueInLoop([this] { BadNested(); });\n"
            "  });\n"
            "}\n"
        )
        self.assertEqual([(6, "queueInLoop", "lambda[this]")], self.hit_summary(source))

    def test_sync_apis_declarations_comments_and_literals_are_ignored(self):
        source = (
            "#define BAD_MACRO() loop->queueInLoop([this] {})\n"
            "class Timer {\n"
            "public:\n"
            "  bool RunAfter(double delay, Callback cb);\n"
            "};\n"
            "void Foo::Safe() {\n"
            "  // loop->queueInLoop([this] {});\n"
            "  /* loop->runAfter(1.0, [&] {});\n"
            "     server.setConnectionCallback(std::bind(&Foo::OnConn, this, _1)); */\n"
            "  std::string s = \"loop->queueInLoop([this] {});\";\n"
            "  std::string r = R\"txt(loop->runAfter(1.0, [=] {});)txt\";\n"
            "  registry.view<PosComp>().each([&](auto e, auto& pos) { (void)e; (void)pos; });\n"
            "  std::for_each(v.begin(), v.end(), [this](int x) { Sum(x); });\n"
            "  loop->queueInLoop([this] { RealHit(); });\n"
            "}\n"
        )
        self.assertEqual([(14, "queueInLoop", "lambda[this]")], self.hit_summary(source))


class StagedCheckTest(unittest.TestCase):
    def test_should_check_path_matches_production_scope(self):
        self.assertTrue(should_check_path("cpp/nodes/scene/main.cpp"))
        self.assertTrue(should_check_path("cpp/libs/services/scene/foo.h"))
        self.assertTrue(should_check_path(r"cpp\nodes\gate\bar.hpp"))
        self.assertFalse(should_check_path("cpp/tests/bag_test/bag_test.cpp"))
        self.assertFalse(should_check_path("cpp/nodes/gate/tests/gate_push_delivery_test.cpp"))
        self.assertFalse(should_check_path("cpp/nodes/scene/foo_test.cpp"))
        self.assertFalse(should_check_path("third_party/muduo/x.cpp"))
        self.assertFalse(should_check_path("cpp/generated/proto/a.pb.cc"))
        self.assertFalse(should_check_path("cpp/libs/engine/muduo_windows/src/x.cc"))
        self.assertFalse(should_check_path("cpp/plugin/main.cpp"))
        self.assertFalse(should_check_path("cpp/nodes/scene/build/gen.cpp"))
        self.assertFalse(should_check_path("go/login/main.go"))

    def test_staged_check_enforces_rules_and_quotas_on_index_blobs(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            subprocess.run(["git", "-C", str(root), "init", "-q"], check=True)
            scene_dir = root / "cpp/nodes/scene"
            test_dir = root / "cpp/tests/sample_test"
            scene_dir.mkdir(parents=True)
            test_dir.mkdir(parents=True)

            ok_file = scene_dir / "ok.cpp"
            ok_file.write_text("void f(Loop& l) { l.queueInLoop([id = 1] {}); }\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "cpp/nodes/scene/ok.cpp"], check=True)
            self.assertEqual(0, check_staged(root, allowed={}, stdout=io.StringIO(), stderr=io.StringIO()))

            # 工作区未暂存的违例不应触发误报
            ok_file.write_text("void f(Loop& l) { l.queueInLoop([=] {}); }\n", encoding="utf-8")
            self.assertEqual(0, check_staged(root, allowed={}, stdout=io.StringIO(), stderr=io.StringIO()))

            # 暂存区违例必须拦截;仅修工作区未 git add 仍应拦截
            bad_file = scene_dir / "bad.cpp"
            bad_file.write_text("void f(Loop& l) { l.queueInLoop([this] {}); }\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "cpp/nodes/scene/bad.cpp"], check=True)
            err = io.StringIO()
            self.assertEqual(1, check_staged(root, allowed={}, stdout=io.StringIO(), stderr=err))
            self.assertIn("cpp/nodes/scene/bad.cpp:1: queueInLoop(...) captured lambda[this]", err.getvalue())

            bad_file.write_text("void f(Loop& l) { l.queueInLoop([id = 1] {}); }\n", encoding="utf-8")
            self.assertEqual(1, check_staged(root, allowed={}, stdout=io.StringIO(), stderr=io.StringIO()))

            # 白名单配额制:配额内放行,超配额立刻拦截,减少(修复旧债)放行
            custom_allowed = {("cpp/nodes/scene/bad.cpp", "queueInLoop", "lambda[this]"): (1, "legacy")}
            self.assertEqual(0, check_staged(root, allowed=custom_allowed, stdout=io.StringIO(), stderr=io.StringIO()))

            bad_file.write_text(
                "void f(Loop& l) { l.queueInLoop([this] {}); l.queueInLoop([this] {}); }\n",
                encoding="utf-8",
            )
            subprocess.run(["git", "-C", str(root), "add", "cpp/nodes/scene/bad.cpp"], check=True)
            err = io.StringIO()
            self.assertEqual(1, check_staged(root, allowed=custom_allowed, stdout=io.StringIO(), stderr=err))
            self.assertIn("exceed allowed quota 1", err.getvalue())

            # 测试目录下的 [&loop] 不在拦截范围内
            bad_file.write_text("void f(Loop& l) { l.queueInLoop([id = 1] {}); }\n", encoding="utf-8")
            unit_file = test_dir / "unit_test.cpp"
            unit_file.write_text("void t(Loop& l) { l.runAfter(0.1, [&l] { l.quit(); }); }\n", encoding="utf-8")
            subprocess.run(
                ["git", "-C", str(root), "add", "cpp/nodes/scene/bad.cpp", "cpp/tests/sample_test/unit_test.cpp"],
                check=True,
            )
            self.assertEqual(0, check_staged(root, allowed=custom_allowed, stdout=io.StringIO(), stderr=io.StringIO()))

    def test_staged_check_fails_closed_outside_git_repo(self):
        with tempfile.TemporaryDirectory() as tmp:
            err = io.StringIO()
            self.assertEqual(2, check_staged(Path(tmp), allowed={}, stdout=io.StringIO(), stderr=err))
            self.assertIn("ERROR", err.getvalue())


class RepositoryAsyncCallbackCaptureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.scanned = 0
        cls.problems = []
        cls.counts = Counter()
        for relative_path, text in cpp_production_sources():
            cls.scanned += 1
            file_problems, file_counts = evaluate_file_violations(relative_path, text, allowed=ALLOWED)
            cls.problems.extend(file_problems)
            cls.counts.update(file_counts)

    def test_scan_covers_production_cpp_tree(self):
        self.assertGreater(self.scanned, 400, "must scan the full production cpp tree, not a single directory")

    def test_no_unregistered_async_callback_captures(self):
        self.assertEqual(
            [],
            self.problems,
            "\n"
            + "\n".join(self.problems)
            + "\nForbidden async callback captures violate AGENTS.md §11.7 "
            "(docs/rules/engineering-principles.md §7). Use [entity = entity, playerId] for ECS work, "
            "[weakSelf = weak_from_this()] for shared infrastructure, LoopTaskScope for multi-step async, "
            "or weak_ptr<State> for cold-path handles. If this is a verified same-lifetime member exception, "
            "register its exact quota and reason in ALLOWED in this file.",
        )

    def test_allowed_quotas_are_not_stale(self):
        stale = []
        for key, (expected_count, _) in sorted(ALLOWED.items()):
            actual_count = self.counts.get(key, 0)
            if actual_count < expected_count:
                rel, method, sig = key
                stale.append(
                    f"  {rel} {method} {sig}: allowed={expected_count}, actual={actual_count}"
                )
        self.assertEqual(
            [],
            stale,
            "These ALLOWED quotas are stale (legacy captures were cleaned up); tighten or remove them:\n"
            + "\n".join(stale),
        )


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1] == "--staged":
        sys.exit(check_staged())
    unittest.main()

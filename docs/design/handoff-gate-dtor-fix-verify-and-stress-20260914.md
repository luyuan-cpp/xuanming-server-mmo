# 交接:gate `~TcpConnection` 事故第二批修复 —— 构建 / 验证 / 压测(执行方:ChatGPT;方案方:Claude Code)

> 2026-09-14 本轮最终构建与测试已回填 §8；合并范围、验证证据和未完成的运行时验收见 §10。§0 的此前进度保留为历史快照。

> 事故报告:[docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md](../ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md)(下文用 §x 指它的章节)
> 分工(用户 2026-09-14 定):**编译 / 代码生成 / 启动服务栈由 ChatGPT 执行,Claude Code 不做;压测由 Claude 出细节方案,ChatGPT 执行并按本文 §8 模板回填,回填写回本文件。**
> 本文件里凡标 **[未验证]** 的,都是 Claude 只做了代码级分析、没有编译或运行过的结论。

## 0. 一句话现状

**最终回填**：本批按用户本轮“合并提交 push”的要求交付，连接修复范围为 26 文件。全部 17 项库/节点构建通过；新增 etcd 回调清理后再次构建 core 与三节点并重跑全套测试，22/22 工程、596/596 用例通过。下列较早记录中的“源码晚于构建”“本批未编译”等状态已由 §8 / §10 更新；整栈验收与压测仍未执行。

第一批修复(`RpcController` 取代 `tlsRpc`,已在 `main` `bb7b1cfc8`)修掉了 assert,但暴露出 `~RpcClient` 之后回调进已释放对象的 UAF(§6.3 末尾更正)。第二批修复及本轮验证已整理为独立交付范围，最终结果见 §10。

**进度(09-14 更新)**:
- **ChatGPT 05:22–06:02 已完成一轮**(记录在 `PROGRESS.md` 09-14 条目):C++ Debug/x64 串行全链构建通过(顺手补齐了 `gate_event_handler.cpp` / `gate_service_handler.cpp` / `scene_response_handler.cpp` 里 `SessionInfo::conn` 改 weak 后的 10 处访问遗漏,现已全部 `lock()`);三个节点 exe 构建通过;`rpc_controller_test` **GREEN 11/11**;Go 7 服务、Java 64/64;空库迁移 exit=0;`start_game.ps1` 六阶段拉栈,11 项原生服务与端口核验通过、网关 UP、一区 OPEN。证据:`run/logs/server-build-20260914-052225/result.json`、`running-verification.json`、`run/logs/game-launcher/20260914-060218-372/launcher.log`。**未做**:§4.2 分组 RED、§5 全套 gtest、§6 运行时验收四项、§7 压测、§8 回填(GREEN 输出也没落到 `tmp\gtest_green.txt`)。
- **之后 Claude 09:40 又改了 `node.cpp`**(关机 `quit` 由 `runAfter(0.05)` 改为两跳 `queueInLoop`,见 §1 表与事故 §7.11)以及 `rpc_server.cc` / `node_util.h` / 测试文件里 3 处注释;**ChatGPT 10:30 在工作树上精修了两跳**:把 `shutdownComplete_ = true` + notify 挪进最内跳、`quit()` 之后(理由见事故 §7.11 末段:`~Node` 的 fallback 只在标记为假时才重新驱动 loop,提前置位会让两跳还排着队、还捕获着 `this` 时就放行析构),并按 §8.1 重建 14/14 + 3/3、GREEN 11/11。**再之后 Claude 又改了一处**:`etcd_service.cpp` `Shutdown()` 改用 `SetEtcdHandler(emptyHandler)` 清空全部 11 个 handler(事故 §7.13 的唯一卫生修复)→ `core.lib` 再次落后于源码,下一轮仍是重建 `core.vcxproj` + 三个节点 exe,再走 §4.2 RED → §5 → §6 → §8。
- **提交状态(09-14 12:xx 核对)**:本批 **仍未提交**。`git log` 里 09-14 新出现的 `d349a8f88` / `8487d5e1f`(333 文件,"保存当前全部开发改动与第三方工作区补丁")/ `3ab20cf77` / `519d9515d` **不含本批任何文件**(逐一核过 `node.cpp` / `rpc_client.h` / `game_channel.h` / `session_info_comp.h` / 新测试 / 两份文档 / `AGENTS.md`);本批就是 `git status` 里那 28 项。当前 HEAD `519d9515d` 领先 `origin/main` 1 个提交、不落后。

## 1. 第二批改动清单(相对 `main` `bb7b1cfc8`;最终交付范围见 §9)

| 文件 | 改了什么 | 对应 |
|---|---|---|
| `cpp/libs/engine/core/network/rpc_client.h` | 继承 `std::enable_shared_from_this`;两个 muduo 回调改在 `connect()` 里以 `weak_ptr<RpcClient>` / `weak_ptr<GameChannel>` 绑定(非 shared 持有则 `LOG_FATAL`);**不再有析构函数**;`GetConnection()` 去掉多余的 `static TcpConnectionPtr` 哨兵,直接 `return client_.connection();` | §7.10 / §7.12 |
| `cpp/libs/engine/core/network/game_channel.h` / `.cpp` | `connection_` 改 `std::weak_ptr`;新增私有 `LockConnection(what)`,三个发送点先 lock、锁不住 `LOG_WARN` 丢弃;`HandleRpcMessage` 首行 assert 改比对 `lock()` | §7.2 |
| `cpp/libs/engine/core/network/rpc_session.h` | `connection` 改 `std::weak_ptr`;`IsConnected()` 改 lock | §7.4 |
| `cpp/nodes/battle/logic/battle_room_manager.h` / `.cpp` | `BattleRoom::directConnByPlayer` 改 `std::map<uint64_t, std::weak_ptr<TcpConnection>>`;五处使用点 `lock()`(`PushToPlayer` / `AttachDirectConnection` / `DetachDirectConnection` / `CloseDirectConnectionOf` / `CloseDirectConnections`)+ 一处赋值(`AttachDirectConnection` 的 `slot = conn`)。规则一致性改动,无单测,由编译 + §7 G1–G4 覆盖。**battle 节点必须重编** | §7.12 / §8.0 |
| `cpp/libs/services/gate/session/comp/session_info_comp.h` + `cpp/nodes/gate/{handler/event/gate_event_handler.cpp, handler/rpc/gate_service_handler.cpp, main.cpp, rpc_replies/route_message_response_handler.cpp, rpc_replies/scene_response_handler.cpp}` | `SessionInfo::conn` 改 `std::weak_ptr`(`session_info_comp.h:56`)。gate 下共 **16 处**使用:**15 处读取全部 `lock()`**(gate_event_handler ×5、gate_service_handler ×6、main ×2、route_message_response_handler ×1、scene_response_handler ×1),**1 处赋值**(`client_message_processor.cpp:628`,`weak = shared` 合法,该文件未改)。09-14 四视角评审曾按更早快照报"10 处漏改",现场已全部核为 `lock()`;复核命令 `grep -rnE "(second\|info\|session)\.conn\b" cpp/nodes/gate cpp/libs/services/gate`,每一行都应是 `.lock()` 或 `= conn`。规则一致性改动,无单测,由编译 + 运行时验收 + G1–G4 覆盖 | §7.12 / §8.0 |
| `AGENTS.md` | 新增 §11.7 连接与回调生命周期(强制规范) | §8.0 |
| `cpp/libs/engine/core/node/system/registration/registration_manager.cpp` | 两处握手重试定时器改 `weak_ptr` 捕获(`TryRegisterNodeSession`、`OnHandshakeReplied` 失败分支) | §7.1 |
| `cpp/libs/engine/core/node/system/node/node_connector.cpp` | 删除 `gNode->GetDisconnectedClientList().push_back(*existingClient)`;留下解释性注释 | §7.3 |
| `cpp/libs/engine/core/node/system/node/node.h` | 删除 `using ClientList`、`GetDisconnectedClientList()`、`disconnectedClientList` 成员 | §7.3 |
| `cpp/libs/engine/core/node/system/node/node_util.h` / `.cpp` | 新增 `NodeUtils::RemoveRpcSessionsBoundTo(conn)`(按 `lock()` 后的指针匹配,顺带清理 weak 已过期的会话);`.h` 多了 `#include <muduo/net/TcpConnection.h>` | §7.4 |
| `cpp/libs/engine/core/network/rpc_server.cc` | DOWN 分支调用上面的函数(替换 `// FIXME:`);多了 `#include "node/system/node/node_util.h"` | §7.4 |
| `cpp/libs/engine/core/node/system/node/node.cpp` | `FinalizeShutdownInLoop` 末尾 `eventLoop->quit()` → **两跳** `queueInLoop([]{ queueInLoop([]{ quit(); }); })`。09-14 评审 ML-1 推翻了先前的 `runAfter(0.05)` 写法:Windows 的 loop 是 `poll → handleEvents → timerQueue_->loop() → doPendingFunctors`,定时器若恰在 N+1 趟到期,`quit_` 先置位、本趟只跑完 `forceCloseInLoop` 就退出,`connectDestroyed` 永不执行;两跳靠队列 FIFO 保证 N+1 跑 forceClose、N+2 跑 connectDestroyed、然后才 quit,与时序无关。**10:30 精修(ChatGPT)**:完成标记 `shutdownComplete_` + notify 挪进最内跳、`quit()` 之后,闭包捕获 `[this, loop]`;否则 `~Node` 的 fallback(标记为假才重新驱动 loop)与"标记为真才允许析构成员"的等待都会被提前放行 | §7.11 |
| `cpp/libs/engine/core/node/system/etcd/etcd_service.cpp` | `Shutdown()` 里手列 6 个 handler 置空 → `etcdserverpb::SetEtcdHandler(emptyHandler)` 一次清空全部 11 个(原漏 keepalive / Compact / LeaseRevoke / TimeToLive / Leases;keepalive 带 `[this]`)。事故 §7.13 普查的唯一修复项,不可达但违反 §11.7 精神 | §7.13 |
| `cpp/tests/rpc_controller_test/rpc_client_lifecycle_test.cpp` | **新文件**,5 条测试(见 §4),文件顶部有 4 个链接桩 `InitReply/InitPlayerService/InitPlayerServiceReplied/InitServiceHandler` | §6.4 |
| `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj` | 加了上面这个 `.cpp` 的 `<ClCompile>` | — |
| `docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md` | §6.3 更正、§6.4 表、§7.1/7.3/7.4/7.10/7.11/7.12、§8 规则 8/9 | — |

**不属于本批、别一起提交**:`generated/**`、`cpp/generated/table/**`、`go/shared/generated/**`、`java/**`、`generated/tables/*.pb|json`(137 项,是 `dev.bat gen` 为修上游 09-13 晚间 `table/battle/scene` 编译断裂重新生成的,属于"生成产物同步",由用户决定单独提交);`go/db/go.mod`、`go/data_service/go.mod`、`go/proto/scene/player_attribute.pb.go`、4 个 `third_party/*` 子模块指针;另一条工作线(OrContinue 闭环)在途的 `tools/data_table_exporter/tests/test_manifest.py` 与 `docs/design/handoff-orcontinue-closure-20260914.md`。

## 2. 这台机器现在的状态,以及会咬人的地方

1. **本轮已重建最新 `core.lib` 及三个节点 exe**，包含 `node.cpp` 两跳与完成标记调整、`etcd_service.cpp` 全量 handler 清理；证据见 §10。仍要记住:`RpcClient` 的方法全部是头文件内联(`connect()` / `onConnection`),core.lib 与节点 / 测试工程若在不同头文件状态下构建,会有两个版本(ODR,链接器随便挑一个)——所以 RED 分组实验每组都要重建 core。
2. **当前没有节点进程在跑**(`tasklist` 无 gate/scene/battle),`bin\*.exe` 未被占用。若你拉过栈又要重编,先 `pwsh -File tools\scripts\dev_tools.ps1 -Command dev-stop`(节点工程的 post-build 会 `copy` 到 `bin\`,文件被占就报 `MSB3073`),或加 `/p:PostBuildEventUseInBuild=false` 只出到 `build\cpp\nodes\`。
3. **MSBuild 必须串行**(`/m:1`,且同一时刻只能有一个 MSBuild 进程),并发会报假的 `C1041 / LNK1104`。03:01 曾看到一个非 Claude 发起的 MSBuild + 9 个 cl.exe 在跑,若那是你,请等它结束再开始。
4. **`dev.bat` 必须用绝对路径调**:本机环境变量 `NoDefaultCurrentDirectoryInExePath=1`,cmd 不在当前目录找批处理,相对路径报 "`'dev.bat' is not recognized`"。正确写法:`cmd /d /c "D:\luyuan\wuxingqitan\mmorpg\dev.bat gen < nul"`。
5. **`py -3` 是 Pandora 自带的解释器**(`D:\luyuan\Pandora-Server\run\localinfra\dist\python\python3.14t.exe`,本机没有系统 Python),`dev.bat :export` 会先 `pip install -r tools\data_table_exporter\requirements.txt` 进它。这轮没改到任何包(版本恰好一致),但下次不保证;要隔离就先 `set PYTHON_EXE=<别的解释器>`。**本批不需要再跑生成器**(已经跑过,产物就是上面那 137 项)。
6. **起节点一律走 `启动服务器.cmd`(→ `tools\scripts\start_game.ps1`)**,别裸调 `dev_tools.ps1 -Command cpp-node-start`:后者不设 `PATH` 指向 `third_party\grpc\install_vs2026_dbg\bin`,三个节点会以 `0xC0000135`(缺 `zlibd.dll`)瞬死、零日志。
7. **gtest 输出要重定向到文件**,不要走管道:stdout 块缓冲,进程 abort 时整块丢失,你会误判成"启动就崩"。`build\cpp\tests\` 下已有 `zlibd.dll / rdkafka*.dll`(runner 会自动从 `third_party\grpc\install_vs2026_dbg\bin` 补)。
8. 在 Git Bash 里执行 `etcdctl`,以 `/` 开头的键会被 MSYS 改写成 Windows 路径;节点键 `SceneNodeService.rpc/zone/1/...` 不以 `/` 开头不受影响,别的键要加 `MSYS_NO_PATHCONV=1`,或改用 PowerShell。

## 3. 构建顺序与命令

```
$mb = 'E:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe'
cd D:\luyuan\wuxingqitan\mmorpg
# 库(串行,顺序不能乱;本批改了 core,但 §2.1 说了 core.lib 现在是坏的,整条重走最稳)
foreach ($p in 'cpp\generated\proto\proto.vcxproj','cpp\generated\rpc\rpc.vcxproj','cpp\generated\proto_helpers\proto_helpers.vcxproj',
  'cpp\libs\engine\config\config.vcxproj','cpp\libs\engine\infra\infra.vcxproj','cpp\libs\engine\session\session.vcxproj',
  'cpp\libs\engine\thread_context\thread_context.vcxproj','cpp\generated\grpc_client\grpc_client.vcxproj',
  'cpp\libs\engine\core\core.vcxproj','cpp\generated\table\table.vcxproj','cpp\libs\modules\modules.vcxproj',
  'cpp\libs\services\battle\battle.vcxproj','cpp\libs\services\scene\scene.vcxproj','cpp\libs\services\gate\gate.vcxproj') {
  & $mb $p /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:m; if ($LASTEXITCODE) { throw $p }
}
# 节点 exe(需要 bin\*.exe 未被占用,见 §2.2)
foreach ($p in 'cpp\nodes\gate\gate.vcxproj','cpp\nodes\scene\scene.vcxproj','cpp\nodes\battle\battle.vcxproj') {
  & $mb $p /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:m; if ($LASTEXITCODE) { throw $p }
}
```

本轮已用第二批最终代码完成构建，结果见 §8.1 / §10。后续若继续修改，重点复核这些接口:`rpc_client.h` 里 `weak_from_this()`(需 C++17,工程是 `stdcpplatest` 应无问题)与 lambda 里对私有 `onConnection` 的调用;`game_channel.cpp` 新增的 `LockConnection` 定义与 `assert(conn == connection_.lock())`;`node_util.h` 新加的 `<muduo/net/TcpConnection.h>` include;`rpc_server.cc` 新加的 `"node/system/node/node_util.h"` include;测试里 `std::make_shared<GameChannel>(conn)` 与 `RpcSession{ conn, "uuid" }` 的构造。报错原样贴到 §8。

## 4. 单元测试:先 GREEN,再分组 RED

测试工程 `cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj`,一个 exe 两个 TU,共 **11 条**:旧的 6 条(`rpc_controller_test.cpp`)+ 新的 5 条(`rpc_client_lifecycle_test.cpp`)。

```
& $mb cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj /t:Rebuild /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:m
.\build\cpp\tests\rpc_controller_test.exe --gtest_color=no > tmp\gtest_green.txt 2>&1   # 在仓库根目录跑
```

### 4.1 GREEN(当前工作树,全部修复在)

期望 **11/11**。ChatGPT 05:41 那轮已跑出 11/11(`PROGRESS.md`),但输出没留到 `tmp\gtest_green.txt`,且之后 `node.cpp` 改过 → 重建 core 后**再跑一次并落文件**。每条测试锁定的东西、以及它失败时意味着什么:

| 用例 | 锁定的不变量 | 若失败 |
|---|---|---|
| `LoopbackFixture.*`(3 条)、`RpcControllerFrom*`(3 条) | 第一批:派发后不留连接强引用;`From()` 契约 | 第一批被改坏 |
| `RpcClientLoopback.HandshakeRetryTimerDoesNotRetainTheConnection` | `TryRegisterNodeSession` 前后 `conn.use_count()` 不变,销毁实体后回基线 | 定时器又按值捕获了 conn |
| `RpcClientLoopback.DestroyingRpcClientDetachesCallbacksBeforeCloseRuns` | 销毁 `RpcClient` 转空 loop 后无新 DOWN 事件 **且** 对端看到 DOWN | 前者失败 = 回调仍进已释放对象(UAF);后者失败 = 析构函数把 `use_count` 抬高、`forceClose` 又被跳过(原缺陷回魂) |
| `RpcClientLoopback.DestroyingNodeEntityReleasesItsRpcClientCleanly` | `registry.destroy` 释放最后一根引用后同上两条 | 同上,但走的是 `node_connector.cpp` 删 push 之后的路径 |
| `RpcClientLoopback.RemoveRpcSessionsBoundToDetachesOnlyThatConnection` | 摘 2 个、跨注册表、不误伤别的连接 | `RemoveRpcSessionsBoundTo` 逻辑错 |
| `RpcClientLoopback.ClientChannelDoesNotPinDeadConnection` | 销毁 server 后 client 收到 DOWN、转空 loop,连接对象 `weak_ptr` expired | 又有人强持出站连接(`GameChannel::connection_` 没改成 weak,或别处新加了强引用) |

### 4.2 RED —— 分组 stash,每组单独证明"测试真的能抓到它要抓的东西"

**规矩**:`core.lib` 与测试 TU 必须在**同一头文件状态**下重建(`RpcClient` 全部方法是头文件内联,两边不一致时链接器随便挑一份 COMDAT,红绿都不可信)。所以每一组都是:`stash` → Build core → Rebuild 测试 → 跑 → `stash pop`。

| 组 | stash 的文件 | 跑什么 | 期望 | 说明 |
|---|---|---|---|---|
| **A** assert/UAF/钉连接 修复 | `registration_manager.cpp` `rpc_client.h` `game_channel.h` `game_channel.cpp` | `--gtest_filter=RpcClientLoopback.*` | `HandshakeRetryTimer*` **FAIL**(+1);`DestroyingRpcClient*` 与 `DestroyingNodeEntity*` **FAIL**(DOWN +1)——**这两条是 UB,直接崩也算红**;`ClientChannel*` **FAIL**(不 expired);`RemoveRpcSessions*` 前四个 EXPECT(removed==2 / aGone / bStays / cGone)PASS、**末尾 `EXPECT_EQ(downBefore, downAfter)` FAIL** —— 它的收尾与 `DestroyingRpcClient*` 是同一段 `DestroyClientAndDrain`,RED-A 下同样 UAF,整条用例记 FAIL 属预期 | 这是最重要的一组。注意 stash 掉 `game_channel.*` 后 `rpc_session.h` 仍是 weak,能编 |
| **B** 删 `disconnectedClientList` | `node_connector.cpp` `node.h` | 只编译 | 编译通过即可;**没有单测红态**(测试复刻的是 destroy 之后的行为,不调 `ConnectToTcpNode`) | 它的安全性靠 §7.3 的可达性分析 + §7 的 G4 压测 |
| **C** `RpcSession` DOWN 摘 | `node_util.h/.cpp` `rpc_server.cc` | 只编译测试 | 测试**编不过**(函数不存在)——预期如此,无单测红态 | 运行时证据见 §6.3 |
| **D** 关机排空 | `node.cpp` | 不做单测 | — | 只能运行时验证,见 §6.4 |

```
git stash push -m red-A -- cpp/libs/engine/core/node/system/registration/registration_manager.cpp cpp/libs/engine/core/network/rpc_client.h cpp/libs/engine/core/network/game_channel.h cpp/libs/engine/core/network/game_channel.cpp
& $mb cpp\libs\engine\core\core.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:m
& $mb cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj /t:Rebuild /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:m
.\build\cpp\tests\rpc_controller_test.exe --gtest_color=no --gtest_filter=RpcClientLoopback.* > tmp\gtest_red_A.txt 2>&1
git stash pop
# 然后必须再走一遍 §4.1 的 GREEN,把 core.lib 建回来
```

若某组"该红不红",先查 `lib\core.lib` 的 mtime 是否晚于 stash 时刻——没重建就是链着旧库。

## 5. 全套 gtest

```
pwsh -NoProfile -ExecutionPolicy Bypass -File tools\scripts\run_cpp_tests.ps1 -Build > tmp\suite.txt 2>&1
```

期望 **22/22 全过**(`rpc_controller_test` 那行应显示 `11/11 全过`)。09-13 用第一批代码跑过 22/22;本批只动了 core 相关源码,其它工程理论上不受影响,但 `-Build` 只重建测试工程本身、不重建库,所以**§3 的库链必须先跑完**。

## 6. 运行时验收(整栈)

先停掉 §2.2 那套旧栈,再 `启动服务器.cmd` 拉新栈。以下四项分别验证 §7.10 / §7.3 / §7.4 / §7.11。

### 6.1 节点摘除不再回调进已释放对象(§7.10)

按 §6.3 的步骤:scene 存活且已连接时
```
docker exec etcd etcdctl get --prefix "" --keys-only          # 找到 SceneNodeService.rpc/zone/1/node_type/3/node_id/N
docker exec etcd etcdctl del "SceneNodeService.rpc/zone/1/node_type/3/node_id/N"
```
然后客户端登录一次。**新的健康判据**(与 09-13 的正好相反):gate 日志里 `TcpClient::~TcpClient[RpcClient]` 之后**不再出现** `Disconnected from server` 和 `Client disconnected` 这两行(它们本来就是 UAF 在执行时打的);随后 scene 在 ~0.75s 内以同一 uuid 重注册、gate 打出 `TcpClient::TcpClient[RpcClient]` 与 `Connected to server`。gate 不崩、无 FATAL。

### 6.2 删掉的 push 分支确实不可达(§7.3)

scene 存活时对它的键**原样再 put 一次**(值用 `etcdctl get <key> --print-value-only` 取):gate 日志应出现 `Node already registered, IP: ...`(`IsNodeConnected` 拦下),**不**出现 `TCP node already registered`(那是 `ConnectToTcpNode` 同 uuid 分支的日志)。这条证明的是"今天的拓扑下那段代码进不去"。

### 6.3 `RpcSession` DOWN 摘组件(§7.4)

这条的现场在 **scene**(gate 是 scene 的入站连接)。停掉 gate(任何方式),scene 日志应出现:
```
Detached 1 RpcSession(s) from closed connection 127.0.0.1:<gate 的出站端口>
```
没有这行 = DOWN 分支没调到,或 `RpcSession` 不在 `tlsNodeContextManager` 的注册表里(那就是 Claude 判断错了,原样回填)。

### 6.4 Windows 控制台关机不再撞 assert(§7.11)

Debug 的 gate,≥1 个客户端在线,在 gate 的控制台窗口按 **Ctrl+C**(不是 `cpp-node-stop`,那是 TerminateProcess,不走这条路)。
- 修复前:Debug 下弹 assert,`TcpConnection.cc:71`(或 `Channel.cc:43`)。
- 修复后:干净退出,退出码 0,无 assert 弹窗、stderr 无 `Assertion failed`。日志顺序:`Disconnecting N client sessions before shutdown...` → `All client sessions disconnected.` → 每个**已登录(绑定了玩家)**的会话一条 `HandleConnectionDisconnection` 断开记录(未登录会话不打这条,别拿在线连接数去对)→ `Before-shutdown drain complete.`。**别找 `Node shutdown complete.`**:它是 `LOG_DEBUG` 且打在 `logSystem.stop()` 之后,正常配置下看不到,看不到不算失败。
重复 5 次,每次都在线 ≥1 客户端。

## 7. 压测细节方案(Claude 出方案,ChatGPT 执行)

目的不是吞吐,是把本事故涉及的三条生命周期路径(节点摘除、同 uuid 重注册、关机)在负载下反复走,证明:不崩、资源有界、日志形状稳定。

### 7.0 公共准备

- 机器人入口:`cmd /d /c "D:\luyuan\wuxingqitan\mmorpg\dev.bat robot"`(编 `robot\robot.exe` 后用 `robot\etc\robot.yaml` 启动;`dev.bat robot <cfg>` 可换配置;`dev.bat robot-zones "1"` 一区一进程)。**账号数、跑哪个场景、是否循环,请读 `robot\etc\robot.yaml` 决定并在 §8 写明你用的值**,不要凭空猜键名;场景实现在 `robot\*_scenario.go`(`features_smoke_scenario.go` 是登录 + 进场景 + 基础功能,压测用它)。
- 资源采样(gate、scene 各一份,每 5s 一行,CSV):
  ```
  while ($true) { $t=Get-Date -f HH:mm:ss; foreach ($n in 'gate','scene') { $p=Get-Process $n -ErrorAction SilentlyContinue; if ($p) { "$t,$n,$($p.WorkingSet64),$($p.HandleCount),$($p.Threads.Count)" | Add-Content tmp\res.csv } }; Start-Sleep 5 }
  ```
- 日志判据(每个场景结束后对 gate / scene 的日志 grep):
  - 硬红线:`FATAL`、`Assertion`、`assert`、`0xC0000005`、`0xC0000374`、进程消失。
  - §7.10 形状:每一次 `Service node stop` 之后有且仅有一条 `TcpClient::~TcpClient[RpcClient]`,**没有**紧随的 `Disconnected from server`;`TcpClient::TcpClient[RpcClient]` 与 `~TcpClient` 的条数之差 ≤ 1(当前存活的那条)。
  - §7.4 形状(scene 侧):每次 gate 断开对应一条 `Detached N RpcSession(s)`。

### 7.1 G1 —— 节点摘除抖动(§7.10 / §7.1)

- 负载:机器人 50 账号(以 `robot.yaml` 实际可配为准)登录并停留在场景。
- 动作:每 10s 删一次 scene 的 etcd 键,共 60 次(10 分钟);scene 每次会自愈重注册。
  ```
  1..60 | % { docker exec etcd etcdctl del "SceneNodeService.rpc/zone/1/node_type/3/node_id/N"; Start-Sleep 10 }
  ```
- 通过:gate / scene 全程存活;`res.csv` 里 gate 的 WorkingSet 与 HandleCount 在前 2 分钟之后**不再单调上升**(允许 ±5% 抖动);日志形状满足 7.0;机器人重连成功率 ≥ 95%(以 robot 自己的统计为准,写明口径)。
- 失败时要回填:第几次删键、gate 最后 50 行日志、`res.csv` 最后 20 行。

### 7.2 G2 —— 控制台关机 ×10(§7.11)

- 负载:机器人 20 账号在线。
- 动作:对 gate 控制台 Ctrl+C,等它退出,`启动服务器.cmd` 只拉 gate(或整栈),机器人重连;重复 10 次。
- 通过:10 次退出码全 0,无 assert;每次日志中 `HandleConnectionDisconnection` 断开记录条数 == 当时**已登录**会话数(机器人全部登录成功则等于在线数;未登录会话不打这条)。

### 7.3 G3 —— 历史必现路径:scene 硬杀立刻重启 ×10(§7.3 / §7.12)

- 负载:机器人 20 账号在线。
- 动作:`Stop-Process -Name scene -Force`,**立刻**用 `启动服务器.cmd` 的方式拉起 scene(不要等 etcd 租约过期),等机器人回到场景;重复 10 次。
- 期望与判据:新 scene 的 uuid 每次都变(gate 日志 `Node added ... uuid:`);旧 scene 的键在 ≤180s 内被 DELETE、gate 打 `Service node stop` 并销毁旧实体(一条 `~TcpClient`);**任何时刻 gate 对 scene 的活跃 `RpcClient` 不超过 2**(旧的等 DELETE、新的已连);gate 不崩。这条同时覆盖"删 push 之后没有新问题"和"§7.12 的历史路径今天怎么表现"。

### 7.4 G4 —— 稳态 soak 30 分钟(基线)

- 负载:机器人 50 账号持续跑功能冒烟,不做任何节点操作。
- 通过:gate / scene 的 WorkingSet、HandleCount 在第 5 分钟之后斜率 ≈ 0(前后 5 分钟均值差 < 5%);无红线日志。这是 G1/G3 的对照组——没有它,G1 里的增长说不清是抖动造成的还是本来就漏。

### 7.5 G5(可选,最有价值)—— ASan 下跑 G1

MSVC 支持 `/fsanitize=address`。给 `cpp\nodes\gate\gate.vcxproj`(及其链的 core)加 `<EnableASAN>true</EnableASAN>`(与 `/RTC1` 冲突,需同时关掉 `BasicRuntimeChecks`),Debug 编一份带 ASan 的 gate,跑 G1 前 10 次删键。**若 ASan 报 heap-use-after-free 且栈里有 `RpcClient::onConnection` 或 `GameChannel::HandleIncomingMessage`,就是 §7.10 没修干净,原样回填栈。** 这一项能把"分析说没有 UAF"变成"工具说没有 UAF"。链不过就放弃并回填原因,不要硬凑。

### 7.6 G6 —— 7 天 soak(生产口径;"设计上关死"≠"负载下不涨",见事故 §7.12 末尾)

G4 的 30 分钟只能证明"没有明显的每秒都在涨";按 Guid / session 键的 map 只插不删、每小时才触发一次的路径、第三方库内部缓存,30 分钟看不出来。

- 负载:机器人 50 账号持续功能冒烟(同 G4);**每天**各做一次 G1 的删键 ×10 与 G3 的硬杀重启 ×3(不做 G2 关机,它会中断采样)。
- 采样:§7.0 的 `res.csv` 改为**每 5 分钟一行**,列追加 `PrivateMemorySize64` 与 `Threads.Count`;gate / scene / battle 各一份;同时每小时记一次 `docker stats --no-stream` 里 mysql / redis / kafka / etcd 的内存(排除"涨的是它们")。
- 通过:第 1 天之后,任一节点的 WorkingSet / PrivateBytes / HandleCount / Threads 对时间的线性回归斜率 ≤ 0(允许日内周期波动;比较每天 00:00–06:00 低谷值更稳);无 §7.0 红线日志;进程无重启;机器人重连成功率不随天数下降。
- 失败时回填:哪个节点、哪个指标、从第几天开始涨、日均涨幅(MB/天)、当天日志里出现频率最高的 20 条 WARN/ERROR、以及第 1 天与最后一天各一份 `!heap -s`(cdb)或 `vmmap` 快照。
- 这一项**不在 §9 的提交门禁里**(提交不等 7 天),但在**上线门禁**里;结果回填到 §8.5。

## 8. 回填(ChatGPT 填;填完把本文件一起提交)

### 8.1 构建

| 步骤 | 结果 | 备注(错误原文 / 耗时) |
|---|---|---|
| §3 库链 14 项 | 14/14 构建成功 | 合并远程 `8487d5e1f` 后按 `/m:1 /nr:false` 串行构建，见 §10 |
| §3 节点 exe 3 项 | 3/3 构建成功 | gate、scene、battle；禁用 post-build 复制部署，只生成 build 目录产物 |
| §4 测试工程 Rebuild | 实际执行 Build 成功 | 依赖库更新后由正式 runner 构建测试工程；本轮未执行 RED 切换 |

### 8.2 单元测试

| 组 | 用例 | 期望 | 实际 | 输出文件 |
|---|---|---|---|---|
| GREEN | 全部 11 条 | 11/11 | 最终代码重建后 11/11 全过 | 仓库同级 `merge-backup-20260914-102631/final-rpc-controller-raw.log`，逐用例原始 gtest 输出；同目录 `final-cpp-tests.log` 为全套 runner 汇总 |
| RED-A | `HandshakeRetryTimer*` | FAIL(+1) | 本轮未执行 | `tmp\gtest_red_A.txt` |
| RED-A | `DestroyingRpcClient*` | FAIL / 崩 | 本轮未执行 | |
| RED-A | `DestroyingNodeEntity*` | FAIL / 崩 | 本轮未执行 | |
| RED-A | `ClientChannel*` | FAIL(不 expired) | 本轮未执行 | |
| RED-A | `RemoveRpcSessions*` | 前 4 个 EXPECT PASS,末尾 `downBefore==downAfter` FAIL | 本轮未执行 | |
| RED-B | 编译 | 通过 | 本轮未执行 | |
| RED-C | 测试编译 | 编不过(预期) | 本轮未执行 | |
| GREEN 复跑 | 全部 11 条 | 11/11 | 最新 etcd 修复后通过；未做 RED 切换，不能视作 RED 还原验证 | `final-rpc-controller-raw.log` |

### 8.3 全套 gtest

| 工程数 | 全过数 | 未过的工程与原因 |
|---|---|---|
| 22 | 22/22；596/596 用例 | 最终无失败；PVP 原断言错误已单独修正并先红后绿，见 §10 |

### 8.4 运行时验收

| 项 | 判据 | 结果 | 证据(日志行原文) |
|---|---|---|---|
| 6.1 节点摘除 | `~TcpClient` 后无 `Disconnected from server` | 本轮未执行 | 无运行时证据 |
| 6.2 重复 put | 出现 `Node already registered`,不出现 `TCP node already registered` | 本轮未执行 | 无运行时证据 |
| 6.3 `RpcSession` | scene 出现 `Detached 1 RpcSession(s)` | 本轮未执行 | 无运行时证据 |
| 6.4 Ctrl+C ×5 | 退出码 0,无 assert | 本轮未执行 | 无运行时证据 |

### 8.5 压测

| 场景 | 负载(实际用的账号数 / 场景 / 配置文件) | 时长 | 结果 | 关键指标(WS 起止、Handles 起止、红线命中) | 证据文件 |
|---|---|---|---|---|---|
| G1 抖动 ×60 | 本轮未执行 | | 待验收 | | |
| G2 关机 ×10 | 本轮未执行 | | 待验收 | | |
| G3 硬杀重启 ×10 | 本轮未执行 | | 待验收 | | |
| G4 soak 30min | 本轮未执行 | | 待验收 | | |
| G5 ASan(可选) | 本轮未执行 | | 待验收 | | |
| G6 7 天 soak(上线门禁) | 本轮未执行 | | 待验收 | 每节点 4 指标的 7 日斜率;低谷值对比;docker 侧内存 | |

### 8.6 与 Claude 判断不符的地方

任何"预期 X 实际 Y"都写这里,附原文。这些是 Claude 下一轮要重新判断的输入。

## 9. 本轮提交范围与后续验收

- 原方案建议在 §8.2 GREEN、§8.3 22/22 与 §8.4 四项完成后提交。用户本轮明确要求合并、提交并推送，按该授权执行版本交付；本轮只完成构建与自动化测试，§8.4 / §8.5 继续保留为待验收项，不能据此认定可以上线。
- 连接修复单独提交 26 文件：24 个修改与 2 个新增，包括全部弱引用调用点、两跳关闭与最终完成通知、etcd handler 清理、生命周期测试、事故报告、本交接、`AGENTS.md` 和 `PROGRESS.md`。PVP 测试的返回值断言修正单独提交。
- 本机 `go.mod` 两处路径配置与所有第三方子模块改动保留在工作区。生成产物已随远程 `8487d5e1f` 合入，本轮不重复提交；导表测试与其交接记录已在独立提交中。
- 推送前重新 fetch；普通推送，不 force。服务部署、客户端、Ctrl+C、节点摘除、RED 分组与压测不在本轮已完成的证据中。

## 10. 本轮合并交付与验证结果

- 合并：原 HEAD `bb7b1cfc8` 先完整备份工作区与未跟踪文件，再快进到 `8487d5e1f`；回放独立改动并保留双方 `PROGRESS.md` 记录。共享工作区随后新增的 etcd 关闭修复也已保存快照、复核并纳入本批。
- 连接关闭：`RpcClient` 回调与会话连接改用弱引用；关闭连接时移除绑定的 RpcSession。Node 通过两跳队列排空关闭工作，最后一跳在 `quit()` 后于锁内置完成标记并通知，解锁后不再访问 Node，避免析构提前放行。etcd 使用现有 `SetEtcdHandler` 清空全部 11 个回调。
- 构建：先完成 14 个库与 gate/scene/battle 三节点，共 17/17；新增 etcd 修复后再次构建 core 和三节点，4/4 通过。全部串行 `/m:1 /nr:false`，关闭 post-build 复制部署，未启动新服务。
- C++ 测试：最终代码执行正式 `run_cpp_tests.ps1 -Build`，22/22 工程、596/596 用例通过；连接控制器/生命周期测试另存逐用例原始输出，11/11。PVP 测试原来误把第一位玩家的 `SubmitAction` 返回值当作提交成功标记，实际返回全员就绪；保留第二位玩家为 true 及伤害断言，首位改为 false 后战斗 120/120、单独 PVP 1/1 通过。未修改生产战斗逻辑。
- 导表：本机与 Ubuntu 的 exporter 单测均 129/129。Ubuntu CI run `34857249677` 后续真实表步骤因既有 workflow 仍调用旧 `core.excel_reader` 而失败，其余门禁 skipped；整体 CI 不能记为通过，详见 [OrContinue 交接 §八](handoff-orcontinue-closure-20260914.md)。
- 证据目录：仓库同级 `merge-backup-20260914-102631`。`build-results.json` 是 17 项初轮构建；`final-build-results.json` 是新 etcd 修复后 4 项构建；`final-cpp-tests.log`、`final-rpc-controller-raw.log`、`validation-summary-final.json` 是最终结果；`pvp-failure.log`、`pvp-fixed.log` 保留红绿证据；`pre-final-build-manifest.json` 记录最终编译源文件快照。原始文件副本和 stash 均保留。
- 验证边界：现有单测没有构造 Node 来证明析构 fallback、跨线程析构及整栈关闭；Watch/CQ 排空既有问题仍未解决。§6 全部现场验收、§7 压测与 §4 RED 分组均未执行。可选 no-raw-pointer-member 检查工具缺失显示 SKIP，不能称为静态门禁通过。

# 小游戏 WebSocket 入口:待验证项交接(2026-11-20 起交 Gemini,2026-11-26 按当前仓库校正)

## 0. 现状

- 负责人 2026-11-20 把这条线的全部待做交给 Gemini。设计与全部定案在 [minigame-websocket-gate.md](../design/minigame-websocket-gate.md)(W1–W11),先通读一遍。
- 内容:微信 / 抖音小游戏经 WebSocket 连 gate 与 battle。
  - gate / battle 的客户端端口按首字节分流,同时接 TCP 与 WebSocket。
  - 边缘 nginx(ws-edge)终止 wss,按 `/ws/<PodIP>/<端口>` 转发。
  - 节点自报 `client_ws_url`,随登录分配、排队、msg 124、战斗分配、测速目标下发。
  - Unity 客户端加了 WebSocket 传输与小游戏登录。
  - robot 可以走 WebSocket(`transport.kind: websocket`)。
- **代码已全部写完并已提交,一条都没验证过**:
  - 服务端在 `9824368eb`、`55eae6be3`、`c441ce75e`、`b6a08148c` 等批量提交里;客户端 `D:\luyuan\mmorpg-client` 在 `02e0ec54`;Java 仓 `PARITY.md` 的待做登记也已提交。
  - 之后的批量提交里协议已被别的会话重新生成过(生成代码里已有 `client_ws_url`、`ws_url`、`target_ws_url` 等字段),但没有任何一次运行跑过本线的用例:`docs/PROGRESS.md` 里没有本线的验证条目。
  - 本线自己只做过静态检查:PowerShell 语法解析、`gofmt -l`、C++ 裸指针成员预检。
- **2026-11-26 按当前 HEAD 复核过,没有要补的代码**:
  - 客户端这几天新加的代码里没有绕过 `ClientTransport` 直接建连的,也没有小游戏里会卡死的写法(`Task.Run`、`Task.Delay`、线程池等);新文件按客户端仓 `AGENTS.md`「小游戏(WebGL)兼容约束」写。
  - 服务端所有带客户端地址的下发消息都带着对应的 WebSocket 地址字段。
  - robot 没有新的裸 TCP 拨号;gate / battle 的客户端口仍是 18000 / 20000,与 ws-edge 的转发目标一致。
- **剩下的全是执行**:协议生成、编译、单测、契约测试、冒烟;失败时按根因修。

## 1. 开工前必读与规矩

- 先读 `AGENTS.md` 全文与它引用的 `docs/rules/` 分章:`ai-workflow.md`(分工、写回、模型档位)、`proto-and-tables.md`(§2 协议同步)、`cpp-runtime-and-build.md`(构建与测试命令)、`engineering-principles.md`(§11.7 连接与回调生命周期)、`stress-testing.md`(冒烟前清数据)。按 `AGENTS.md` §10.1,编译、生成、测试与冒烟都由 Gemini 执行。
- **模型档位(`AGENTS.md` §10.3)**:生成、编译、跑测试与冒烟用 `Carbon Medium`。要改代码时先保留首错原文停下,切到 `Gemini 4 Neon Extra` 再改。
- 只在 main 工作区做,不开分支 / worktree,不 `git push` / `git tag`。
- **只有负责人明确要求才 commit**,而且只提交自己的修复;见第 6 节。
- 工作区里有几百个别的会话未提交的文件:不 `git stash` / `reset` / `checkout --`,不整理别人的改动。
- 不手改 `generated/` 与各语言 proto 输出树。
- 日志文案用英文;文档、注释、提交说明用中文。
- C++:
  - MSBuild 一律 `/m:1`。
  - `run_cpp_tests.ps1 -Build` 不重编库:`websocket.cpp`、`codec.cpp` 在 core 库里,先整解。
  - 类 / 结构体不放裸指针成员;节点代码不新起线程;连接与回调只存 / 只绑 `weak_ptr`(§11.7)。
- 失败时:
  - 保留第一条错误(文件:行)或失败输出原文。
  - 修根因。不注释断言、不放宽断言、不跳测试、不连续自动重试。
  - 报错落在别的会话的改动上(`git diff` 分得清),原样记录、不修,在结果里写明归属。
- 游戏未上线,数据可以全部清空。冒烟前按 §6.2 清 Redis、MySQL、etcd,重置 Kafka offset。
- 结果写进 `docs/PROGRESS.md`:**文末追加新条目,不改旧条目**。标题写「Gemini:小游戏 WebSocket 验证」,列命令、通过数、失败原文摘要;日志放 `build/verification/minigame-ws-<日期>/`。条目里不贴大段原始输出。
- Java 版不写代码:负责人定「java 留到 todo」,已在 Java 仓 `PARITY.md` 登记。

## 2. 本线的 PROGRESS 条目

在 `docs/PROGRESS.md` 里按标题开头 grep 即可定位。命令以本交接单第 3 节为准:那几条里的步骤已合并到第 3 节,有出入以这里为准。

| 日期 | 标题开头 | 内容 |
|---|---|---|
| 11-19 | 小游戏 WebSocket 入口——微信 / 抖音小游戏经 WebSocket 连 gate 与 battle | 主体:C++、proto、Go、Java、部署、客户端 |
| 11-19 | 小游戏 WebSocket 入口——交付前复读的补正 | 用例数更正;客户端处理器抛异常不再重复派发 |
| 11-19 | robot 加 WebSocket 传输 | robot 的 `transport` 配置与地址簿 |
| 11-20 | 小游戏 WebSocket 收尾 | 边缘按来源限速、battle 计数、注销重新认证、robot 改用 `wireConn` |

## 3. 一遍跑完的顺序

每步通过了再走下一步。所有命令在服务端仓根 `D:\luyuan\xuanming-server-mmo-cpp` 执行,另注明的除外。

### 3.0 协议生成

本线有客户端任务授权,用默认配置(会写客户端仓的生成代码)。按 `docs/rules/proto-and-tables.md` §2,服务端生成完之后要在客户端仓同步:

```
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run
cd D:\luyuan\mmorpg-client
.\gen.bat client
```

原来生成后跑 `go/build.ps1` 的一步作废:该脚本 11-21 随 goctl 退役删除,login 转发层是手写的,由 `go/login` 的 `TestGoctlServerWrappersImplementEveryRPC` 核对。

`gen.bat client` 依次跑 `tools/sync_tables.ps1`、`tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`。

通过标准:全部退出码 0,客户端编译检查 0 error。协议之前被别的会话生成过,这一步多半没有新改动;有改动也照常留在工作区,不用单独提交。

### 3.1 C++

```
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter routing_identity_test -LogDir build/verification/minigame-ws-<日期>/cpp-tests
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter client_endpoint_test -LogDir build/verification/minigame-ws-<日期>/cpp-tests
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test -LogDir build/verification/minigame-ws-<日期>/cpp-tests
```

通过标准:
- 整解 0 个错误。
- `routing_identity_test` 里 `WebSocketHandshake.*` 4 条、`WebSocketFrame.*` 2 条、`WebSocketEntry.*` 6 条全部 OK,gate 原有用例照旧全过。
- `client_endpoint_test` 里 `ClientEndpointWsUrl.*` 5 条 OK。
- `rpc_controller_test` 里 battle 准入用例照过(`battle_connection_admission_test.cpp` 编在这个工程里)。

### 3.2 Go 服务

```
cd go/login && gofmt -l internal && go build ./... && go test -count=1 ./internal/logic/pkg/loginqueue/... ./internal/svc/... ./internal/logic/loginpregate/...
cd go/scene_manager && gofmt -l internal && go build ./... && go test -count=1 ./internal/logic/...
cd go/match && gofmt -l internal && go build ./... && go test -count=1 ./internal/discovery/... ./internal/logic/...
```

通过标准:`gofmt -l` 无输出,其余全绿。本线的用例:
- login:`TestAdmit_WsURLFollowsTheSignedGate`、`TestBuildGateCandidates_CarriesClientWsUrl`;
- scene_manager:`TestSignRedirectToGate_WsUrlIsThePickedGatesOwn`;
- match:`TestParseReadsClientWsUrl`。

`go/match/internal/logic/network_probe.go` 同时有「探测目标分散」会话的改动,失败时先分清归属。

### 3.3 Java gateway(工作目录 `java`)

```
mvn -B -pl gateway_node -am test "-Dtest=AssignGateServiceAuthTest" -Dsurefire.failIfNoSpecifiedTests=false
```

通过标准:`gateWsUrlIsPassedThroughOnlyWhenTheGateAdvertisesOne` 与原有用例全过。

### 3.4 robot(工作目录 `robot`)

```
go mod vendor
gofmt -l pkg config main.go gate.go http_assign_gate.go http_assign_gate_ws_url_test.go battle_direct_conn.go network_probe.go heartbeat_idle_smoke_scenario.go
go build ./...
go vet ./pkg/ ./config/ .
go test ./pkg/ ./config/ -count=1
go test . -run "TestPostJSON_RecordsTheGateWebSocketAddress|NetworkProbe|AssignGate|BattleDirect" -count=1
go test ./pkg/ -race -count=1
```

通过标准:
- `go mod vendor` 之后出现 `vendor/golang.org/x/net/websocket/`。本机模块缓存里有 v0.48.0,不需要联网。
- `gofmt -l` 无输出,其余零失败。
- `pkg` 的全部用例(Call、心跳、重定向)现在跑在仓内的 `wireConn` 上,是这次搬迁的主要回归面,必须全过。
- 传输层用例:`TestTransportConfig_Validate`、`TestTransport_AdvertisedAddressWinsAndAMissingOneFallsBackOrFails`、`TestTransport_WebSocketCarriesEachClientFrameAsOneBinaryMessage`、`TestTransport_RequireAdvertisedFailsBeforeDialing`、`TestTransport_EdgeAddressIsDialedThroughDialAddrOverTLS`、`TestTransport_TCPIsTheDefaultAndIgnoresAdvertisements`。TLS 那条故意让一次握手因自签证书失败,stderr 会有一行 `http: TLS handshake error … bad certificate`,不算失败。
- `-race` 需要 cgo,本机没有就跳过并写明。

`go mod vendor` 会把 `go/proto`、`go/shared` 工作区当前内容一起带进 `robot/vendor`,别的会话没生成完的协议也会进来。之后编不过,先看是不是这个原因。

### 3.5 部署契约(只读夹具,不连集群)

```
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/gate_connection_alerts_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/battle_connection_alerts_contract.tests.ps1
python tools/scripts/check_log_text_english.py
```

通过标准:fail=0;日志英文守卫没有新增违例。`battle_connection_alerts_contract` 同时被「battle 直连准入」会话改着,失败时先看是不是那一批的断言。

### 3.6 边缘 nginx 配置的静态检查(需要本机 docker,不连集群)

`geo` 以路径里捕获的地址为键、`map` 取 /24,这两处写法都没有实测过。这一步十秒钟就能定下来:

```powershell
. tools/scripts/lib/k8s_client_entry.ps1
$out = Join-Path $env:TEMP 'ws-edge-nginx-check'; New-Item -ItemType Directory -Force $out | Out-Null
foreach ($rate in 0, 20) {
    $yaml = New-WsEdgeYaml -EdgeHost ws.example.com -TlsSecretName ws-edge-tls -PodCidrs 10.244.0.0/16 -TargetPorts 18000, 20000 -TargetApps gate, battle -Image nginx:1.28.0 -SourceRatePerSecond $rate
    $conf = [regex]::Match($yaml, '(?ms)^  nginx\.conf: \|\n(?<body>.*?)^---').Groups['body'].Value -replace '(?m)^    ', ''
    [System.IO.File]::WriteAllText((Join-Path $out "nginx-$rate.conf"), $conf)
}
docker run --rm -v "${out}:/work:ro" nginx:1.28.0 sh -c "mkdir -p /etc/nginx/tls && openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=ws.example.com -keyout /etc/nginx/tls/tls.key -out /etc/nginx/tls/tls.crt -days 1 2>/dev/null && nginx -t -c /work/nginx-0.conf && nginx -t -c /work/nginx-20.conf"
```

通过标准:两份配置都打出 `syntax is ok` 与 `test is successful`。

- 报 `unknown "ws_target_ip" variable` 或 `unknown "ws_source_24" variable`,就是这两处写法不成立。按根因改 `New-WsEdgeYaml` 的模板,同步改 `k8s_client_entry_contract.tests.ps1` 里钉住的那几行,再跑 3.5。
- 镜像里没有 `openssl` 命令时,换成在宿主机生成一对自签证书,挂进 `/etc/nginx/tls`。

### 3.7 客户端(仓根 `D:\luyuan\mmorpg-client`,先关掉占着工程的 Unity)

```
pwsh -File tools/client_compile_check.ps1 -ShowErrors 40
D:\luyuan\tools\Unity\6000.6.0f1\Editor\Unity.exe -batchmode -nographics -projectPath D:\luyuan\mmorpg-client -runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Net" -testResults D:\luyuan\mmorpg-client\.codex-artifacts\minigame-ws\editmode-net.xml -logFile D:\luyuan\mmorpg-client\.codex-artifacts\minigame-ws\editmode-net.log
```

通过标准:
- 编译 0 error。
- EditMode 零失败、零跳过,`WebSocketFramedConnectionTests` 10 条 PASS。
- `MmorpgClient.Tests.EditMode.Battle` 再跑一次同样零失败(`BattleDirectLink`、`RegionProbeClient` 的连接调用改了)。

只在小游戏里编译的分支编不到:`#if UNITY_WEBGL && !UNITY_EDITOR` 的 `JsWebSocketDriver`、`MiniGameLogin`,以及 jslib。本机只装了 Windows 构建支持。**不要自行安装 WebGL Build Support**(装工具要负责人同意,`AGENTS.md` §10.2);负责人同意的话,完整编一次 Web 包,零错误即可。

### 3.8 本机冒烟(有本机栈时)

gate 与 battle 不用设 `CLIENT_WS_BASE_URL`:它们的客户端端口本来就接 WebSocket,robot 走直连回落。

1. 把下面四份配置各复制到 `robot/logs/ws-smoke/`(已被 git 忽略),末尾加两行:`transport:` 与 `  kind: websocket`。在 `robot` 目录用 `-c` 指过去各跑一遍:
   - `etc/robot_smoke.yaml`:登录、分配 gate、进场;
   - `etc/heartbeat_idle_smoke.yaml`:裸连接验票、心跳、空闲超时;
   - `etc/battle_smoke.yaml`:战斗直连与观战;
   - `etc/gate_migrate_smoke.yaml`:msg 124 换 gate(前置条件见文件头)。
2. 通过标准:
   - 与 TCP 下相同的通过行(各配置文件头部写明);
   - robot 启动日志有一行 `robot transport: websocket`;
   - gate 的 `/metrics` 里 `gate_websocket_handshakes_total{result="accepted"}` 增长,`gate_websocket_protocol_closed_total` 不增长;
   - gate 与 battle 日志里没有 `WebSocket frames rejected`。
3. 同样四份配置去掉 `transport` 段(即 TCP)再跑一遍,确认原生路径没被 `wireConn` 搬迁带坏。
4. 可选:Unity 编辑器在 Player Settings 的 Scripting Define Symbols 里加 `MMORPG_FORCE_WS`,走完登录、进场、战斗。

### 3.9 kind 经边缘(可选,有 kind 环境时)

1. 先在 infra namespace 建 TLS Secret(自签即可)。`infra-up` 与 `zone-up` 都带:`-ClientEntryMode external -WsEdgeHost <域名> -WsEdgeTlsSecret <名> -WsEdgePodCidrs <Pod CIDR>`。
2. 看三件事:
   - ws-edge 的 Pod 起得来;
   - `/ws/<gate PodIP>/18000` 握手回 101;
   - `/ws/<别的 Pod IP>/20000` 被 NetworkPolicy 挡住,或回 403。
3. robot 配置加:
   ```yaml
   transport:
     kind: websocket
     require_advertised: true
     dial_addr: "127.0.0.1:<ws-edge 的 NodePort>"
     insecure_skip_verify: true
   ```
   `robot_smoke`、`battle_smoke` 通过。报 `the server advertised no WebSocket address for …`,说明服务端漏下发了地址,记下是哪一步。
4. 边缘限速:`infra-up` 加 `-WsEdgeSourceRatePerSecond 1`,从同一台机器连续发起 15 次以上握手,前 10 次左右回 101,之后出现 429。去掉参数重跑 `infra-up`,之后不再有 429。

## 4. 已知的易错点

- `battle_connection_admission.{h,cpp}`、gate 的 `connection_admission.*`、core 的 `accept_admission.h` 正被「battle 直连准入」会话改着(按来源限速、只限公网源地址)。本线只在 battle 的 `CollectMetrics` 末尾加了两族指标,别的报错先看归属。
- robot 的 `wireConn` 是 vendored `muduoclient` 连接管理的逐行搬迁,只把「怎么拨号」换成可注入。对上层可见的行为必须不变:后台拨号、拨不通每秒重试、发送队列满了就丢、断线后下一次发送失败才重拨、`Close` 不唤醒 `Recv`。用例红了先对照 `vendor/github.com/luyuancpp/muduoclient/muduo/connection.go` 找差异。
- 客户端的 `.meta` 已由 Unity 生成,提交时连同 `.meta` 一起。
- 服务端 C++ 的单测断言对字节逐个比对(握手应答、帧头、关闭码)。红了先看实现是否与 RFC 6455 一致,别改断言迁就实现。

## 5. 不在交接范围(要人或要账号,写代码完成不了)

- 把客户端转成微信 / 抖音小游戏包、真机联调:负责人 11-20 定上线前再做,届时打算用 Unity 7,不换团结引擎(设计稿 W10)。
- 两个域名的 ICP 备案、两家平台的开发者账号、后台登记合法域名:见 [发布清单](../ops/release-checklist.md) §7。
- 客户端在小游戏宿主里写 `persistentDataPath` 能不能跨会话保留:转包时核实。
- Java 版:待做,只登记。

## 6. 提交(只在负责人要求时)

- 本线的代码已全部提交,这一节只管验证中修出来的改动。
- 只提交到本地 main,不 push。只提交自己的修复:工作区常有别的会话未提交的文件,同一文件里混着别人的改动时,只暂存自己的那几块。
- 服务端、客户端分开提交,说明里写清修的是本线哪一条、验证结果如何。

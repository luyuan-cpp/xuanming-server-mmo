# 交接清单:2026-09-02 ~ 09-05 三天工作的留档待办(经代码核实)

> 生成日期 2026-09-05,基线 `xuanming-server-mmo` HEAD `d08843042`、`mmorpg-client` HEAD `99d891f`。

**这份清单从哪来**:过去三天(全服跨 zone 匹配 + 二期 prepare deadline/配表指纹/MMR + K8s kind A/B 档 + 问道式回合制战斗表现)已全部合并推送到 main。本清单把这期间被标为「minor / 后续 / 二期 / 待办 / 待拍板」的项,逐条对照 HEAD 代码、PROGRESS.md、设计文档与 kind 集群实况核实后整理而成;已经悄悄做完的项放附录 A,防止重做。每条都带「来源」(文件:行号或章节),没有来源的不写。

**怎么用**:读者在另一个窗口、没有本会话上下文。先读 §1 把本机约定过一遍(不看会踩 WMI/串行编译/端口保留区间的坑),再看 §2 把需要用户拍板的项列出来拿去问;然后按 §3→§5 的优先级顺序开工。每条固定字段:领域 / 工作量(S≤半天、M 1~2 天、L ≥3 天)/ 状态 / 背景与证据 / 要改的文件 / 步骤 / 验收标准 / 验证方式 / 风险与注意。`partially-done` 的条目明确写了「已完成部分 / 剩余部分」。条目编号 `P1-01`、`P2-D3` 之类在全文唯一,`-D` 后缀表示 needs-decision。

**统计**:P0 0 条(无生产阻塞事故级项);P1 17 条(其中待决策 2);P2 35 条(其中待决策 8);P3 25 条(其中待决策 11)。合计 77 条,待决策 21 条,附录 A「其实已做完」5 条。

**去重说明**:上游候选里同一件事在多个领域重复出现(策划表 JSON 大小写 ×3、tip 文案 ×2、MatchRedis ×2、死测试工程 ×2、scratchpad 脚本 ×2、PlayMode 冒烟 ×2、DevAutoPilot ×2、skill_presentation ×2、QdaoRunAssetTests ×2、Resources/Battle 压缩 ×2、怪物 7~16/外观 ×3、预留产品功能 ×2、cache mount ×3、NonBlock ×2),本清单各合并为一条,优先级取最高者。

---

## 1. 先决条件与本机约定

按顺序过一遍,每条都有出处。

### 1.1 构建 shell:先 source buildenv

- 每个构建 shell 先 `. E:\work\tools\buildenv.ps1`(设 GOROOT/GOPATH/GOPROXY/PATH)。Go 在 `E:\work\tools\go126`;GOPROXY 必须 `goproxy.cn,mirrors.aliyun.com/goproxy/,direct`。来源:memory `xuanming-build-toolchain`;runbook 第 2 条「dev.bat 必须先 source buildenv,否则 go-svc-build 报 'go' not recognized」(memory `xuanming-local-startup-runbook`)。
- protoc 用仓库 vendor `third_party/grpc/install_vs2026/bin/protoc.exe`;导表器要 `protoc` 与 `protoc-gen-go` 在 PATH(memory `xuanming-battle-spectate-phase2`)。
- 生成器 `tools/proto_generator/protogen/proto-gen.exe` 是预编译二进制,`dev_tools.ps1 -Command proto-gen-run` 默认优先用它;改生成器源码后必须 `go build -o proto-gen.exe ./cmd`(并同步 pbgen.exe),否则改动静默不生效。regen 后核对 `cpp/generated/rpc/service_metadata/rpc_event_registry.h` 的 `kMaxRpcMethodCount`,并从 git diff 恢复 `scene_node_service.cpp` 守护段外的 Agones 块(memory `xuanming-build-toolchain`)。

### 1.2 C++:MSBuild 一律串行,库链按依赖顺序

- MSBuild 路径 `D:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe`,一律 `/m:1 /p:Configuration=Debug /p:Platform=x64`。
- 节点 vcxproj 没有 ProjectReference,单独 msbuild 节点不会重编库。新增 proto/表后的串行顺序:**proto → rpc → table → core → battle 库 → scene 库 → scene/battle/gate 节点 → 引擎测试工程**(PROGRESS.md:3543-3545)。`scene.lib` 硬依赖 `battle.lib`。
- 新增 proto/表后 `.pb.cc` 不会自动登记进 `cpp/generated/proto/proto.vcxproj` / `table.vcxproj`,漏登记的症状是 LNK2019(PROGRESS.md:3508-3509、:3543;本清单 P2-06)。
- 单测统一入口 `pwsh tools/scripts/run_cpp_tests.ps1 [-Build] [-Filter x]`,21 工程 / 437 用例全绿是基线(PROGRESS.md:3979 条目「验证」段)。注意退出码非 0 但无 `[FAILED]` 行 = 用例中途 LOG_FATAL。
- 编出的 exe 在 `build/cpp/nodes/`,运行栈用的是 `bin/`;进程占用时拷不进去,必须先停栈(PROGRESS.md:3996-3997;本清单 P1-08)。

### 1.3 起进程:C++ 节点与长驻服务必须经 WMI 逃逸

- 从 AI 工具调用里用 `Start-Process` 起的节点会随工具调用结束被杀;要用 `Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = ... }`(scratchpad `full_restart.ps1` 头注释与第 1 步;memory `xuanming-battle-spectate-phase2`)。
- C++ 节点显式给 `ZONE_ID` / `RPC_PORT` / `NODE_IP`(scratchpad `relaunch_cpp_nodes.ps1:13-22`);gRPC 端口 = TCP + 30000(`node_allocator.cpp:186 kGrpcPortOffset`)。本机端口表:z1 gate 10000 / scene 20000,20001 / battle 20100(gRPC 50100);z2 gate 10010 / scene 22000,22001;Go:login 53000 / player_locator 53200 / scene_manager 60300 / match 50500;Java gateway 8081(`relaunch_cpp_nodes.ps1:14-22`、`full_restart.ps1` 第 4 步)。
- **杀节点后 ≥5s 再重拉**:旧进程 gRPC 端口未释放时新进程拿不到;etcd 旧租约未过期时新进程会 `Preset RPC port already registered` 自杀(memory runbook 第 17 条;本清单 P1-08)。
- Windows 每次开机随机保留 TCP 区间(`netsh interface ipv4 show excludedportrange protocol=tcp`),Go 侧 `go_services.ps1` 已有 `Resolve-BindablePort` 自动上移(:224-250),实际端口看 `run/etc/go_services/<svc>.yaml`;C++ 侧尚未有(PROGRESS.md:3787-3791;本清单 P1-07)。
- Java gateway 不在 `dev.bat start` 里;起之前先 `Get-NetTCPConnection -LocalPort 8081` 看是否已被别的会话拉起(runbook 第 12/15 条)。
- `/api/server-list` 里 zone 为 `MAINTENANCE` = 该 zone 的 gate 没注册到 etcd,是判断 gate 层健康的最快信号(runbook 第 14 条)。

### 1.4 重启后的整栈拉起顺序(PROGRESS.md:3648、runbook 第 17 条、scratchpad `full_restart.ps1`)

1. Docker Desktop 经 WMI 起(exe 在 `%LOCALAPPDATA%\Programs\DockerDesktop\Docker Desktop.exe`),轮询 `docker info` 直到有 ServerVersion。启动即崩看 runbook 第 6/10 条(`%LOCALAPPDATA%\Docker\run` 与 `docker-secrets-engine` 整目录改名)。
2. `docker start` 所有 Exited 容器,跳过 `*-init` 一次性任务与 kind 的 `*control-plane`;等 `docker exec kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --list` 能列出 topic。
3. Go 双 zone + C++(`recover_stack_nomatch.ps1`)。
4. **Kafka 慢起的连锁**:zone1 的 db / player_locator(53200)/ scene_manager(60300)/ login(53000)会 panic,而 login 启动同步拨 playerlocator.rpc → 补拉顺序 **player_locator → scene_manager → login**,按端口缺谁起谁(`full_restart.ps1` 第 4 步;根因在本清单 P1-05、P1-06)。`LOGIN_DEV_PASSWORD_SHARED_SECRET=123456` 必须设,否则 `dev_/robot_` 账号登录报「账号不存在」(runbook 第 7 条)。
5. 双 match + Java 网关(`post_recover.ps1`)。
6. C++ 节点显式端口重拉(`relaunch_cpp_nodes.ps1`)→ `Invoke-WebRequest http://127.0.0.1:8081/api/server-list` 看 zone 1/2 都 `OPEN`。
7. kind 集群 `mmorpg`(v1.37.0)在线,但 `full_restart.ps1` 明确跳过 kind 节点;宿主重启后 kind 内 Pod 可能卡 ErrImagePull 数十分钟,处置见 P2-03。

以上脚本都在会话临时目录,入库是 P1-09。

### 1.5 Unity:编辑器被占用时用副本工程

- 本机 Unity `C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe`。用户开着编辑器时 batchmode 会被锁,用 robocopy 副本工程(`E:\work\tmp\<copy>`,排除 Library/Temp/Logs/obj/.git/UserSettings/.vs),首次从已解析副本(如 `E:/work/tmp/presentation_verify_project/Library`)拷 Library 避免卡 UPM 下载(memory `unity-copy-project-upm-hang`、`mmorpg-client-player-build-verify`)。
- `-runTests` **不要带 `-quit`**(PROGRESS.md:3679;memory `xuanming-attribute-allocation`);PlayMode 可能需要去掉 `-nographics`。
- 离线编译体检:Roslyn csc + csproj 引用,约 2s,退出码 0 = 绿(memory `mmorpg-client-compile-verify`;脚本 scratchpad `client_compile_check.ps1`)。
- EditMode 现状:166 例 165 过,唯一失败是既有 `QdaoRunAssetTests`(PROGRESS.md:3785;本清单 P2-D5)。程序集已拆 `MmorpgClient.Tests.EditMode.Battle` / `.Tianyong`,可用 `-assemblyNames` 只跑 Battle。
- 客户端 UI 一律 UGUI,不用 FairyGUI(memory `client-uses-ugui-not-fairygui`)。

### 1.6 并行会话干扰:出包/冒烟前先编译体检

- 本机常有第二个 AI 会话同时改两个仓库:会整片停掉节点、重编 `bin\*.exe`(mtime 突然前进)、留下不可编译的中间态(memory `parallel-sessions-same-repo`;PROGRESS.md:3237)。
- 动手前基线:`tasklist | grep -E "^(gate|scene|battle)\.exe"`、`Get-Item bin\*.exe | Select LastWriteTime`;客户端出包前先跑离线编译体检,不绿就等,不改对方文件。
- 节点被停用 `relaunch_cpp_nodes.ps1` 重拉,不要走 `dev.bat` / `go_services.ps1 stop`(会连坐,见 P2-08)。
- git:`git add -A` 在本仓会 fatal(third_party/grpc 子模块 gitdir 烘着旧路径),用 `git add -A -- PROGRESS.md bin cpp data deploy docs generated go java proto robot tools`;状态用 `git status --porcelain --ignore-submodules=all`(memory `xuanming-build-toolchain`)。提交只在用户明确要求时做(AGENTS.md:149/169),提交信息首行格式见 P2-13。
- PROGRESS.md 另一会话也在追加(:3979 是它写的),改之前 `git pull --rebase`,只做追加式修改。

### 1.7 冒烟入口与账号

- robot:`cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml`(PVE_SOLO + 观战)、`etc/battle_smoke_cross_zone.yaml`(跨 zone 1v1,账号 robot_9003/9004)、`etc/attribute_smoke.yaml`(属性加点 12 步)。成功标记分别为 `BATTLE_SMOKE_OK` / `CROSS_ZONE_MATCH_OK` / `ATTRIBUTE_SMOKE_OK player_id=…`(memory `xuanming-battle-spectate-phase2`、`xuanming-attribute-allocation`)。
- 连续复跑要等 scene 侧 `battle:lock` 解冻(≈5~6 分钟)或换账号;评分重置用 scratchpad `reset_smoke_state.ps1`(默认 player_id 已过期,见 P2-07)。
- robot regen 陷阱:动了 go/proto 后 robot 必须 `go mod vendor` 再重编(默认 `-mod=vendor`)。
- 客户端实机双播放器:`pwsh -File E:/work/mmorpg-client/tools/run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe -ShotDir <dir>`,成功输出 `CROSS_ZONE_PAIR_PASS`(PROGRESS.md:3794-3796)。
- kind:`kubectl --context kind-mmorpg get pods -A`;B 档 namespace `mmorpg-infra` / `mmorpg-zone-yesterday`(deploy/k8s/README.md「Local Trial on kind」)。

---

## 2. 需要先拍板的决策(21 条)

只列选项与后果,不替用户决定。每条的完整字段(证据/文件/步骤/验收)在 §3~§5 对应编号下。

| 编号 | 决策题 | 选项与后果 | 完整条目 |
|---|---|---|---|
| D-01 | `go/db` 对宿主仓库外 `proto2mysql` 的 `replace` 归宿 | **A** 删 replace 用远端 tag(需先处理 `E:/work/proto2mysql` 未提交 diff,建议同时把 module 名迁 `luyuan-cpp`);三处同源、Dockerfile 占位 stage 可删;代价:改库要走 tag/bump。**B** `go mod vendor`;离线可构建;代价:仓库体积、与 robot vendor 口径统一。**C** 保持 replace,CI 加第二仓库 checkout;每个消费方都要记得。 | P1-D1 |
| D-02 | TiDB 迁移 Phase 1 是否继续 | **A** 近期不迁,K8s 生产基线 = MySQL;决策文档 §5/§6 标 deferred,`cmd/migrate` 阻断直接删;代价:16MB 存档/全区全服口径问题继续悬置。**B** 继续:K8s TiDB 清单 + 权限策略 + Dumpling/Lightning + §5 四项验收 + BR/PITR runbook(D8 硬前置);多天工作,本机 kind 资源紧。 | P1-D2 |
| D-03 | MatchRedis 淘汰策略与本地默认形态 | 策略:**A** 改 `noeviction`(+内存告警),满时报错不丢票据;**B** 保持 `volatile-lru`,改文档,满时静默丢带 TTL 票据。本地:**默认回落共享库**(注掉 yaml MatchRedis 段)vs **默认集群**(dev 脚本自动起 `--profile redis-cluster`)。 | P2-D1 |
| D-04 | 怪物 AI 与数值平衡三项 | ① 减伤改乘法 vs 加法+最低伤害比例下限;② 怪物是否配技能列 + AI 选招规则;③ Skill.damage 表达式按等级重定。策划项,改表触发配表指纹变更。 | P2-D2 |
| D-05 | 技能等级门槛要不要做 | **A** 不做,删两处 TODO,文档写明解锁靠 Class.skill;**B** Skill 表加 `required_level`,引擎/实时同口径校验 + 新 tip 码。 | P2-D3 |
| D-06 | 死亡/复活完整流程 | 复活时机(结算即复活 vs 回城复活点)、惩罚(经验/耐久/金币)、原地复活道具是否存在。依赖经验系统与背包;产品未定前不要动现有基线。 | P2-D4 |
| D-07 | `QdaoRunAssetTests` 走资产还是断言 | **A** 改资产:镜像帧上半身加 1~2px 起伏让 8 帧 hash 互异,重出 N/S;**B** 改断言:上半身 ≥5 + 新增下半身 8 帧互异断言;5 分钟完事但保护变弱。 | P2-D5 |
| D-08 | 战斗帧条 106 张 Uncompressed(≈212 MiB)怎么处理 | **A** 压缩(BC7/ASTC,包体 -75%,描边可能有伪影,放松美术规格);**B** Addressables 按角色懒加载(改动面最大,只在上移动端时做);**C** 只加战斗结束 `ResetCaches + UnloadUnusedAssets` 止血(任何拍板下都建议做)。 | P2-D6 |
| D-09 | 右上角色卡数量 | **A** 按规格 2 张(自己+第一队友;5v5 队友状态只能看头顶条);**B** 全队 5 张(y 20..520,信息密度高,需复核与计时环不重叠)。现值 4 是常量截断,两边都不是。 | P2-D7 |
| D-10 | 角色外观是否按职业/性别 | **1** `BattleActorState` 加 `class_id/gender`,scene 快照填充,客户端 class→class_tag 映射(跨 4 模块,依赖 P1-10);**2** 只对本人按建角选择映射,对手/观战仍哈希(零协议改动)。 | P2-D8 |
| D-11 | `GetQueueStatus.EstimatedWaitSeconds` 恒 0 | **A** 服务端按队列弹组耗时滑动窗口估算;**B** proto 注释标 deprecated,客户端不渲染。另:是否把评分回给客户端(产品)。 | P3-D1 |
| D-12 | 预留产品功能做哪几项、什么顺序 | 3v3(最小)→ 对手 zone 展示(proto+三端)→ 预组队(改队列数据结构,最大)→ ready check(新 RPC+UI)→ 观战延迟 N 回合(C++ battle 侧;MMR 上线后「无排位利益」前提已不成立)。 | P3-D2 |
| D-13 | SharedRedis 单点:哨兵还是 Cluster | **A** 哨兵/主从,不动业务 key,解单点不解扩容;**B** Cluster,按 §10.0 阻塞表逐服务重构 8 服务的跨 slot Lua/MULTI/RENAME/RPOPLPUSH,多周、触及存盘链路。 | P3-D3 |
| D-14 | 生成器对 `contracts/kafka/*_event.proto` 是否全部出 gate handler | **1** yaml 白名单/exclude,删 `match_event_handler.*` 空壳;加新 gate 消费事件多写一行配置。**2** 保持现状,每个 kafka 事件 proto 都多一个死文件并要手工登记 vcxproj。 | P3-D4 |
| D-15 | 5 个死透的 C++ 测试工程 | **A** `git rm -r`(推荐,零风险,历史可找回);**B** 按现有 API 重写(被测物大多已无替代实现,先查)。建议 redis/mrediscli/consistent_hash 删,scene/team 看产品是否保留功能。 | P3-D5 |
| D-16 | battle 行动窗口是否含演出时长 | **A** 维持 2000ms 固定窗口、客户端压缩(自动局群攻被压到看不清);**B** 服务端按上一回合事件数追加窗口并设上限(整体变慢、排位拖长)。 | P3-D6 |
| D-17 | 01/10 号立绘裁在 850 框 | **1** 接受,文档写明统一身高 175 的来源;**2** 在有生图通道的环境重出 01/10(全身含完整兵器、留 8% 边距),22 套成图尺寸全部变化、已验收帧重核。 | P3-D7 |
| D-18 | 是否投入生图(怪物头像 / 手绘动作 / 背景) | 零成本先做程序化怪物头像(从 idle 首帧裁头部);其余需在有生图通道的环境按 §2 模板出图,本 CLI 无法完成。 | P3-D8 |
| D-19 | buff 图标底色极性 | **A** Buff 表加 `polarity` 列,工具按列分配;**B** 维持 distinct buff_type %3 的伪随机并接受错色。另:`-buff-count 24` 对 20 行表补 4 个 padding 是否保留。 | P3-D9 |
| D-20 | 伤害数字字号以哪个为准 | **A** 维持规格 §1(36-40px@1220 ≈3% 屏高,现值 5.6% 已超);**B** 按参考帧 8~10% 屏高放大(`BaseGlyphScale` 0.95→1.35,避让参数同比、容器加宽)。 | P3-D10 |
| D-21 | 命令环「召唤(宠物)」 | **A** 保持置灰占位(与问道七键一致);**B** 宠物立项前隐藏为六键(`CommandCount`/`Labels` 改,布局测试自动跟随)。 | P3-D11 |

---

## 3. P1(17 条:15 待办 + 2 待决策)

### P1-01 K8s C 档:gate/scene/battle 三个 C++ 节点 Linux 镜像 + battle 节点 K8s 清单

**领域** k8s-deploy · **工作量** L · **状态** open

**背景与证据**
B 档(2026-09-04)把 Go 五服务 + Java 网关在 kind 上跑 Ready,但 gate/scene 用 alpine 占位镜像 CrashLoop(exit 127),battle 连清单都没有。后果:① 网关 `/api/server-list` 的 zone 恒为 MAINTENANCE;② 跨 zone 匹配链 gate→match→battle 在 K8s 上无法闭环(match 报「battle 池为空,暂停凑单」);③ 任何 K8s 上线口径实际不可用。证据:`deploy/k8s/Dockerfile.cpp:2` 头注「gate + scene」,`:210-211` 只 COPY gate/scene,`:233` chmod 只这两个;`tools/scripts/build_linux.sh:95-101` `EXE_PROJECTS=(cpp/nodes/gate cpp/nodes/scene)`、`BINARIES=("gate" "scene")`;`tools/archived/vcxproj2cmake.py:397-414` 生成器不出 battle 的 CMakeLists(`cpp/nodes/battle/` 与 `cpp/libs/services/battle/` 目录存在);`tools/scripts/k8s_stage_runtime.ps1:45` `$requiredBinaryNames=@("gate","scene")`;`deploy/k8s/Dockerfile.runtime:17` 同;`tools/scripts/k8s_deploy.ps1` Apply-Zone(:1838-1945)只生成 gate(:1880)与 scene 池(:1882-1910),全文无 battle Deployment;`deploy/k8s/manifests/` 只有 go-svc/infra/java-svc;`deploy/k8s/README.md:319`「C++ 节点本档不构建:用任意小镜像占位」、`:371-372` server-list 恒 MAINTENANCE;`docs/design/cross-zone-matchmaking.md` §10「battle 节点的 K8s 清单…缺清单是既有状态」;`.github/workflows/cpp-build-ci.yml:23-35` 只编 gate+scene。battle 是全局池(`proto/common/base/node.proto:33` BattleNodeService=28;`node_util.cpp:125-135` IsGlobalPoolNodeType 含 Match/Battle),部署形态应与 match 相同:放 infra namespace 一份。本机 battle 端口 TCP 20100 / gRPC 50100。

**要改的文件**
- `deploy/k8s/Dockerfile.cpp`、`deploy/k8s/Dockerfile.runtime`、`deploy/k8s/README.md`
- `tools/scripts/build_linux.sh`、`tools/archived/vcxproj2cmake.py`、`tools/scripts/k8s_stage_runtime.ps1`
- `tools/scripts/k8s_deploy.ps1`、`tools/scripts/k8s_image.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`
- `cpp/nodes/battle/battle.vcxproj`、`cpp/libs/services/battle/`、`.github/workflows/cpp-build-ci.yml`

**步骤**
1. 构建侧:`vcxproj2cmake.py:397-414` 追加 `generate("./cpp/libs/services/battle/battle.vcxproj", "./cpp/libs/services/battle/", "lib")` 与 `generate("./cpp/nodes/battle/battle.vcxproj", "./cpp/nodes/battle/", "exe")`(先确认 services/battle 有 .cpp;header-only 同 player 处理);同步 `:15-17` 头注。
2. `build_linux.sh:78-101`:LIB_PROJECTS 在 `cpp/libs/services/gate` 之后加 `cpp/libs/services/battle`(顺序按 battle.vcxproj 的 ProjectReference),EXE_PROJECTS 加 `cpp/nodes/battle`,`BINARIES=("gate" "scene" "battle")`;`:2` 头注同步。`cpp-build-ci.yml:20` 的 manifest 漂移校验会比对这两份清单。
3. `Dockerfile.cpp:2` 改「gate + scene + battle」;`:210-211` 后加 `COPY --from=builder /src/bin/battle /app/bin/battle`;`:233` chmod 加 battle(grep 显示 battle 不引用 navmesh,不用挂 `data/scene_nav_bin`)。`Dockerfile.runtime:17` 与 `k8s_stage_runtime.ps1:45` 同改。
4. 在 Docker Desktop/WSL2 执行 `docker build -f deploy/k8s/Dockerfile.cpp -t local/mmorpg-node:<tag> .`(首编 30~60 分钟,gRPC v1.83.0 源码编译;`.dockerignore` 排除 `third_party/grpc`)。然后 `docker save --platform linux/amd64 -o node.tar local/mmorpg-node:<tag>` + `kind load image-archive node.tar --name mmorpg`(README:296-300 的 Docker 29 坑)。
5. 清单侧:`k8s_deploy.ps1` 新增 `function Apply-GlobalNodeManifests`:在 `$InfraNamespace` 里 (a) `New-NodeConfigMapYaml -CurrentZoneId $ZoneId -ConfigName "node-config"` 生成 ConfigMap(infra ns 目前没有 node-config;`game_config.yaml` 可追加 `battle_table_fingerprint_mode: warn`,与 `bin/etc/game_config.yaml:19` 对齐);(b) `New-NodeDeploymentYaml -NodeName "battle" -Replicas $BattleReplicas -RpcPort 20100 -StartCommand "./battle" -ConfigMapName "node-config"`;(c) ClusterIP Service battle 暴露 20100/TCP 与 50100/TCP。`New-NodeDeploymentYaml` ports 段(:647-649)目前只开 `$RpcPort`,加 grpc containerPort `$RpcPort+30000`。
6. 参数区加 `[int]$BattleReplicas = 1`;Apply-Infra 末尾(:2103 之后)调用 Apply-GlobalNodeManifests;Wait 逻辑照 Apply-GlobalGoSvcManifests(:1500-1504)加 `Wait-ForDeploymentReady -Namespace $InfraNamespace -DeploymentName battle`;`zones.json` / `dev_tools.ps1 Invoke-K8sDeploy` 透传 `-BattleReplicas`。
7. battle 注册 etcd 用 ZoneId 只影响路径(与 match 同理,:1495-1497 注释);ConfigMap 的 `service_discovery_prefixes` 已含 BattleNodeService.rpc/MatchNodeService.rpc(:537-541),无需改。
8. 契约测试增加 Test-Case:infra-up DryRun 产物含 `kind: Deployment name: battle`、containerPort 20100 与 50100、image 与 `-NodeImage` 一致、imagePullPolicy 与 `Resolve-ImagePullPolicy` 口径一致(需先有 infra-up 基线,见 P2-02)。
9. 实跑:`infra-up -GoSvcRegistry local -NodeImage local/mmorpg-node:<tag> -ZoneId 1`,再 `zone-up -ZoneName yesterday -ZoneId 1 -GoSvcRegistry local -JavaSvcRegistry local -NodeImage local/mmorpg-node:<tag> -GateReplicas 1 -SceneReplicas 1 -WaitReady`。
10. README「Local Trial on kind」新增「C 档」小节;PROGRESS.md 记录。

**验收标准**
- `bash tools/scripts/build_linux.sh --dry-run` 打印的 EXE 列表含 `cpp/nodes/battle`,BINARIES 含 battle;镜像内 `/app/bin/{gate,scene,battle}` 三个可执行存在。
- kind 上 `kubectl get pods -n mmorpg-infra` 显示 deploy/battle 1/1 Running;`mmorpg-zone-yesterday` gate/scene 1/1 Running(不再 CrashLoop exit 127)。
- etcd 出现 `GateNodeService.rpc/zone/1/...`、`SceneNodeService.rpc/zone/1/...`、`BattleNodeService.rpc/zone/1/...`(非空 grpc_endpoint)。
- `GET /api/server-list` 的 zone 1 status 由 MAINTENANCE 变 OPEN。
- robot 对 K8s gate 跑出 BATTLE_SMOKE_OK / CROSS_ZONE_MATCH_OK,match 日志不再出现「battle 池为空」。
- 契约测试全部通过(27 条 + 新增)。

**验证方式**
```powershell
bash tools/scripts/build_linux.sh --skip-deps --relwithdebinfo --split-debug --dry-run
docker build -f deploy/k8s/Dockerfile.cpp -t local/mmorpg-node:<tag> . ; docker run --rm --entrypoint ls local/mmorpg-node:<tag> -l /app/bin
docker save --platform linux/amd64 -o node.tar local/mmorpg-node:<tag>; kind load image-archive node.tar --name mmorpg
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -DryRun -GoSvcRegistry local -NodeImage local/mmorpg-node:<tag> -ZoneId 1 | Select-String -Pattern 'name: battle','containerPort: 20100','containerPort: 50100'
kubectl -n mmorpg-infra get deploy,pod,svc -l app=battle; kubectl -n mmorpg-zone-yesterday get pods
kubectl exec -n mmorpg-infra deploy/etcd -- etcdctl get --prefix --keys-only BattleNodeService.rpc/
kubectl port-forward -n mmorpg-zone-yesterday svc/gateway 18081:8081; curl http://127.0.0.1:18081/api/server-list
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

**风险与注意**
Dockerfile.cpp 首编需 GitHub 可达 + 30~60 分钟 + 大量磁盘;Windows 无法直接跑 build_linux.sh。battle 的 CMake 依赖链未在 vcxproj2cmake.py 中验证过,可能暴露 Linux 编译错误(Windows-only 头、大小写路径)。gate 通过 etcd grpc_endpoint 直连 battle Pod IP,Pod 重启后依赖租约刷新(NodeTTLSeconds=180)。`New-NodeDeploymentYaml` 只注 POD_IP,本机 `cpp_nodes.ps1` 用 NODE_IP,需确认 `config.cpp` 读哪一个。

---

### P1-02 infra 清单生产化硬化:mysql 明文密码、backup PVC RWX 永久 Pending、kafka/etcd latest

**领域** k8s-deploy · **工作量** M · **状态** open

**背景与证据**
三件事同一根因:infra manifest 是一次性静态 yaml,没接进 `k8s_deploy.ps1` 的档位/密钥机制。① `deploy/k8s/manifests/infra/mysql.yaml:113-114` `MYSQL_ROOT_PASSWORD value "Mmorpg#2026db"`、`:127-128` `MYSQL_PASSWORD "apppass123"`、`:169` readinessProbe `-pMmorpg#2026db` 明文;`mysql-backup-cronjob.yaml:61-64` 明文并注释「In real deployments switch this to a Secret ref」;`k8s_deploy.ps1:235-280` Initialize-InjectedSecrets 把 `MMORPG_MYSQL_PASSWORD` 注进 Go/Java ConfigMap,但 MySQL 本体还是旧常量——prod 档注入新密码后 db 连不上。② `mysql.yaml:36-37` mysql-backup-pvc `ReadWriteMany`,README:304-305 记录在 kind local-path 永久 Pending。③ `kafka.yaml:32` `apache/kafka:latest` 且全文无 imagePullPolicy;`etcd.yaml:36` `bitnamilegacy/etcd:latest`。契约测试 `:179-187`「image 不得 latest」只扫 zone-up 产物。`docs/design/global-data-layer-tidb-decision.md` §D9(L141)也要求 root 明文至少改注入。

**要改的文件**
- `deploy/k8s/manifests/infra/mysql.yaml`、`mysql-backup-cronjob.yaml`、`kafka.yaml`、`etcd.yaml`
- `tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`
- `deploy/k8s/README.md`、`docs/ops/mysql-backup-pitr-runbook.md`

**步骤**
1. `k8s_deploy.ps1` 新增 `function New-InfraSecretsYaml`:输出 `kind: Secret name: mysql-credentials`(stringData: root-password=`$script:MysqlPassword`、app-user=appuser、app-password=`$script:GatewayDbPassword`)。Initialize-InjectedSecrets 的 dev 回落是 root / Mmorpg#2026db,与 mysql.yaml 现值一致,dev 档零行为变化;prod 档必须让 MYSQL_ROOT_PASSWORD 与 db ConfigMap 的 Passwd 同源。Apply-Infra 在 mysql.yaml 之前(与 :2072 New-MysqlInitConfigMapYaml 同位置)apply 该 Secret;确认 infra-up 主流程(:2120 之后)也调用 Initialize-InjectedSecrets(目前只在 zone-up 写路径调用)。
2. `mysql.yaml:113-114/127-128` 改 `valueFrom: secretKeyRef: {name: mysql-credentials, key: root-password / app-password}`;`:161-169` readinessProbe 改 `exec: [sh, -c, 'mysqladmin ping -h localhost -uroot -p"$MYSQL_ROOT_PASSWORD"']`;`mysql-backup-cronjob.yaml:61-64` 同改。
3. `mysql.yaml:36-37` 改 `ReadWriteOnce`(cronjob.yaml:130-136 注释已承认单节点前提),或加 `__MYSQL_BACKUP_ACCESS_MODE__` 占位由参数 `-MysqlBackupAccessMode`(默认 ReadWriteOnce)替换;runbook 同步。
4. `kafka.yaml:32` 钉版本(先 grep `deploy/docker-compose.yml` 的 kafka image 对齐)并加 `imagePullPolicy: IfNotPresent`;`etcd.yaml:36` 钉具体 tag(先 `docker pull` 验证可用)。
5. 契约测试:新增 infra-up DryRun 采集(P2-02),加 Test-Case:① infra 产物 image 都非 latest;② mysql Deployment env 无 `value:` 形式的 *PASSWORD;③ prod 档 Secret root-password == MMORPG_MYSQL_PASSWORD 注入值;④ 每个 infra Deployment 都有 imagePullPolicy。
6. kind 实跑:`kubectl -n mmorpg-infra delete deploy mysql`(PVC 保留)→ infra-up → mysql 1/1 Ready;`kubectl get pvc -n mmorpg-infra` 无 Pending。

**验收标准**
- `grep -n 'value: "Mmorpg\|apppass\|-pMmorpg' deploy/k8s/manifests/infra/*.yaml` 无命中;`kubectl -n mmorpg-infra get secret mysql-credentials` 存在。
- `kubectl -n mmorpg-infra get pvc` 全部 Bound。
- infra Deployment 的 image 无 latest、pullPolicy 无空。
- prod 档 DryRun:db ConfigMap Passwd、gateway 数据源密码、mysql Secret 三处同源;契约测试全绿。
- B 档验收清单(README:356-373)复跑通过。

**验证方式**
```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -DryRun -GoSvcRegistry local -ZoneId 1 | Select-String 'kind: Secret','secretKeyRef','image:','imagePullPolicy'
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
kubectl -n mmorpg-infra get pvc,secret; kubectl -n mmorpg-infra exec deploy/mysql -- sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "SELECT 1"'
kubectl -n mmorpg-infra create job --from=cronjob/mysql-backup mysql-backup-manual-test; kubectl -n mmorpg-infra logs job/mysql-backup-manual-test
```

**风险与注意**
已有 mysql-data-pvc 的集群改 root 密码不会改库内实际密码(initdb 只首启跑),需 ALTER USER 或删 PVC;Secret 名/键与 Go 服务 ConfigMap 的 Passwd 不同步会让 db 连不上;kafka 版本升级可能触发 KRaft 元数据格式变化(kind emptyDir 无所谓,真集群需评估)。

---

### P1-03 staging/prod 档 `AutoMigrateSchema=false` 但没有迁移 Job,K8s 首次拉起 db 前无人建表/加列

**领域** k8s-deploy · **工作量** M · **状态** open

**背景与证据**
dev 档靠 go/db 启动期 `AutoMigrateSchema=true` 建表(`go/db/etc/db.yaml:61-62`),kind B 档因此能过。`k8s_deploy.ps1:1135-1146` staging/prod 固定 AutoCreateDatabase/AutoMigrateSchema=false,注释引用的 `go/db/README.md` 不存在(`find go/db -name '*.md'` 为空);`grep -rn 'kind: Job' deploy/k8s/manifests tools/scripts/k8s_deploy.ps1` 只有 `redis-match-cluster.yaml:144` 建群 Job;`deploy/k8s/Dockerfile.go-svc:83-89` 只编 ENTRY 一个二进制,镜像里没有 `cmd/migrate`;`go/db/cmd/migrate/main.go:10-22` 支持 `-command status|plan|up`、`-create-database`、`-allow-modify`。`mysql.yaml:148-150` initdb 只在数据目录为空时执行;`New-MysqlInitConfigMapYaml`(:2008-2054)生成的 `01_k8s_zone_dbs.sql` 同样只首启生效——新增 zone 时不会补建 `zone_<id>_db`。`docs/design/player-attribute-allocation.md:35` 把 `go run ./cmd/migrate -command up` 写成 attribute_component 新列的上线前置。

**要改的文件**
- `tools/scripts/k8s_deploy.ps1`、`deploy/k8s/Dockerfile.go-svc`、`tools/scripts/go_svc_image.ps1`
- `go/db/cmd/migrate/main.go`、`deploy/k8s/manifests/go-svc/db.yaml`、`deploy/k8s/manifests/infra/mysql.yaml`
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`deploy/k8s/README.md`、`docs/ops/k8s-open-server-runbook.md`

**步骤**
1. 镜像:`Dockerfile.go-svc` 增加 `ARG EXTRA_CMDS`(默认空),builder 阶段对 db 追加 `go build -o /app/migrate ./cmd/migrate`,runtime 阶段 `COPY /app/migrate*`(通配避免其他服务失败);`go_svc_image.ps1` Catalogue db 条目加 `ExtraCmds="cmd/migrate"` 并透传 `--build-arg`。
2. `k8s_deploy.ps1` 新增 `function New-DbMigrateJobYaml -Namespace -CurrentZoneId -Image`:`kind: Job name: db-migrate-<GoSvcTag 规范化>`,与 db 相同镜像,command `["/app/migrate","-f","/app/etc/db.yaml","-command","up"]`(staging/prod 不加 `-create-database`),挂同一个 `go-svc-db-config` ConfigMap 到 `/app/etc`,backoffLimit 3,ttlSecondsAfterFinished 3600;Job 名带 tag(Job template 不可变,:2060-2062 已记录该坑)。
3. Apply-GoSvcManifests(:1432-1451)在 apply db Deployment 之前:apply Job → 非 DryRun 时 `kubectl -n <ns> wait --for=condition=complete --timeout=600s job/<name>`,失败 throw 并把 `kubectl logs job/<name>` 提示写进 throw 文本。dev 档可 `-SkipDbMigrate`,staging/prod 不允许跳过。
4. zone 库补建:把 `01_k8s_zone_dbs.sql` 的 CREATE DATABASE IF NOT EXISTS/GRANT 复用为 infra-up 阶段幂等 Job(mysql:8.0 镜像 `mysql -h mysql -uroot -p$MYSQL_ROOT_PASSWORD < /init/01_k8s_zone_dbs.sql`,挂 mysql-init-sql ConfigMap),或 migrate Job 用 `-create-database`(受 AllowedDatabases 白名单约束,:1129-1132)。二选一写进 README。
5. 契约测试:① staging/prod 档 DryRun 产物含 `kind: Job` + `/app/migrate` + `-command up`,且出现在 db Deployment 之前(按 `--- BEGIN MANIFEST ---` 顺序);② dev 档若 AutoMigrateSchema=true 可无 Job;③ Job 挂载 ConfigMap 名 == go-svc-db-config。
6. 文档:补 `go/db/README.md`(或改 :1140 引用到新建 `docs/ops/db-schema-migration.md`),写清 status/plan/up 三步、`-allow-modify` 风险(大表重建)、回滚策略;`k8s-open-server-runbook.md` 插入「迁移 Job 完成后再 zone-up」。
7. kind 验证:`-ReleaseProfile staging`(注入 MMORPG_* 环境变量,契约测试 :205-216 有样例)对新 namespace zone-up,观察 Job Complete、db Ready;再改一处 proto 加列 → 重打镜像 → zone-up → Job 日志出现 ALTER TABLE。

**验收标准**
- staging/prod 档 DryRun 输出包含 db-migrate Job 且位于 db Deployment 之前;dev 档行为不变。
- kind staging 档 zone-up:`kubectl get job` db-migrate Complete 1/1,db 1/1 Ready,`SHOW TABLES` 含 player_database 且有 attribute_component 列。
- Job 失败(故意错密码)时 zone-up 非 0 退出并打印 Job 日志提示,db Deployment 不被 apply。
- `k8s_deploy.ps1` 不再引用不存在的 `go/db/README.md`。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\db; go build ./cmd/migrate; go run ./cmd/migrate -f etc/db.yaml -command plan
pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Services db -DryRun
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName mig-test -ZoneId 101 -ReleaseProfile staging -SkipPreflight -DryRun -GoSvcRegistry registry.invalid/test -JavaSvcRegistry registry.invalid/test -NodeImage ghcr.io/luyuancpp/mmorpg-node:0123456789ab | Select-String 'kind: Job','/app/migrate','name: db$'
kubectl -n mmorpg-zone-<name> get job; kubectl -n mmorpg-zone-<name> logs job/db-migrate-<tag>
kubectl -n mmorpg-infra exec deploy/mysql -- sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "SHOW COLUMNS FROM zone_1_db.player_database LIKE \"attribute_component\""'
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

**风险与注意**
`cmd/migrate` 目前引用 `proto2mysql.PbMysqlDB` 在新版库已无该类型,`go build ./...` 在 go/db 被阻断(见 P2-15、P1-D2)——本条第 1 步之前必须先让 cmd/migrate 能编。`-allow-modify` 在大表上是重建表级操作,Job 里不应默认开;多 zone 并发 zone-up 对同一库 DDL 可能互相阻塞(每 zone 独立库可接受);Job 名带 tag 会积累,靠 ttl 清理。

---

### P1-04 策划表 JSON 文件名大小写:导表器已改小写,但 binary_gen 仍读大驼峰、4 个 `Attribute*.json` 大驼峰仍在 git、镜像 mv 兜底与防回归未做

**领域** ops-tooling / server-go · **工作量** S · **状态** partially-done

**已完成部分**:`tools/data_table_exporter/core/generators/json_gen.py:38-41` 已改 `out = cfg.json_dir / f"{table.name.lower()}.json"`(提交 d007e448e,注释写明「从前是 PascalCase…Linux 上 JSON 模式必然读不到」);`core/manifest.py:82`、`type_mapping.py:423` 也是 lower;Go loader `go/shared/generated/table/attributeautoplan_table.go:68` 读 `attributeautoplan.json`,C++ 模板 `templates/cpp_config.cpp.j2:21` 读 `{{ sheetname | lower }}.json`,三端读取名已一致。

**背景与证据**(剩余部分):① `git ls-files generated/tables` 仍跟踪 `AttributeAutoPlan.json / AttributeDimension.json / AttributePool.json / AttributeRule.json`(提交 143ecca96;其余 23 个已小写),而 `generated/tables/manifest.json:69` 登记的是 `attributeautoplan.json`,它们的 .pb 也是小写——Windows 大小写不敏感所以重导只覆盖内容不改文件名;② `core/generators/binary_gen.py:48` 仍 `json_path = cfg.json_dir / f"{table.name}.json"`(大驼峰读),`pb2json.py:147` `f"{name}.json"` 同——Linux/CI(`exporter-tests.yml:76` ubuntu-latest)导 .pb 会 RuntimeError;③ `deploy/k8s/Dockerfile.go-svc:127-136` 与 `Dockerfile.cpp:220-224` 的 `tr 'A-Z' 'a-z'` mv 兜底仍在,`README.md:332-336` 仍写「根因在导表/生成器命名不一致」;④ 无任何测试断言 generated/tables/*.json 全小写。

**要改的文件**
- `tools/data_table_exporter/core/generators/binary_gen.py`(:48)、`tools/data_table_exporter/pb2json.py`(:147)
- `generated/tables/Attribute{AutoPlan,Dimension,Pool,Rule}.json`
- `tools/data_table_exporter/tests/`(新增文件名一致性测试)
- `deploy/k8s/Dockerfile.go-svc`(:127-136)、`deploy/k8s/Dockerfile.cpp`(:220-224)、`deploy/k8s/README.md`(:332-336)
- `tools/scripts/go_svc_image.ps1`(:145 表目录校验处)

**步骤**
1. `binary_gen.py:48` 改 `f"{table.name.lower()}.json"`;`pb2json.py:147` 改 `f"{name.lower()}.json"`;grep 整个 data_table_exporter 其它 `f"{table.name}.json"` / `{schema.name}.json` 一并统一(`bit_index_gen.py:65` 已是 lower)。
2. Windows 上大小写改名两步走:`git mv generated/tables/AttributeAutoPlan.json generated/tables/_attributeautoplan.json; git mv generated/tables/_attributeautoplan.json generated/tables/attributeautoplan.json`(其余三张同理),`git status` 应显示 rename。或 `git config core.ignorecase false` 后 `git mv -f`。
3. tests 新增 `test_output_filenames.py`:遍历 manifest.json 每张表登记的 json/pb 文件名,断言全小写且与目录实际文件名逐字节相等(用 `os.listdir` 精确比较,不用 `Path.exists`);另一条断言 json_gen/binary_gen/manifest 三处生成名一致。`go_svc_image.ps1:145` 表目录校验加同样断言(有大写即 throw)。
4. 重跑导表(`cd tools/data_table_exporter; python run.py`,按 readme 的 venv;PATH 含 protoc-gen-go),`git status generated/tables` 应只有内容 diff、无新大驼峰文件。
5. `Dockerfile.go-svc:127-136` / `Dockerfile.cpp:220-224` 的 mv 兜底改成 `RUN ! ls /generated/tables | grep -q '[A-Z]'`(有大写即构建失败,兜底变护栏);README:332-336 改「d007e448e 起导表器统一小写;镜像保留大小写护栏」。
6. Linux 验证:`docker run --rm -v <repo>:/src -w /src/go/login golang:1.24 …` 起一次 login 确认 LoadTables 不 Fatal。

**验收标准**
- `git ls-files generated/tables | grep -E '/[A-Z]'` 为空(manifest.json 除外)。
- exporter pytest 全绿含新用例;Linux 上 `python run.py` 全流程不报 `JSON not found`。
- Dockerfile 不再含 `tr 'A-Z' 'a-z'` 的 mv 兜底;重建 Go 镜像后 kind 上 login/player-locator/scene-manager Ready,日志无 `failed to load … table`。
- robot attribute_smoke 与 battle_smoke 仍 OK(属性四表被正确加载)。

**验证方式**
```powershell
git -C E:/work/xuanming-server-mmo ls-files generated/tables | Select-String '/[A-Z]'
cd E:\work\xuanming-server-mmo\tools\data_table_exporter; python -m pytest tests -q; python run.py; git -C E:/work/xuanming-server-mmo status --short generated/tables
docker run --rm -v E:\work\xuanming-server-mmo:/src -w /src/go/login -e GOPROXY=https://goproxy.cn,direct golang:1.24 sh -c 'go build -o /tmp/login . && /tmp/login -f etc/login.yaml' 2>&1 | Select-String 'failed to load|table'
pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Services login -DryRun
kubectl -n mmorpg-zone-yesterday logs deploy/login | Select-String 'table'
```

**风险与注意**
Windows git `core.ignorecase=true` 下大小写重命名容易提交成「无变化」,要显式处理;并行会话若用旧版导表器/pb2json 再导一次会重新产出大驼峰,所以第 1 步与第 3 步的测试必须一起进;删镜像兜底后若再混入大写文件,Linux 服务直接 CrashLoop——先落断言再删兜底。Java/C#/Python 模板需 grep `| lower` 再确认。

---

### P1-05 login / player_locator 启动同步硬拨 zrpc 对端(`MustNewClient`),对端未 Ready 即 fatal 重启 → 改 NonBlock;顺带网关 K8s 静态 endpoints 兜底

**领域** server-go · **工作量** S · **状态** open

**背景与证据**
`go/login/internal/svc/servicecontext.go:118` `plConn := zrpc.MustNewClient(config.AppConfig.PlayerLocatorRpc)`、`:124` `smConn := zrpc.MustNewClient(config.AppConfig.SceneManagerRpc)`;`go/player_locator/internal/svc/servicecontext.go:55` `zrpc.MustNewClient(c.SceneManagerRpc)`(连锁上一环);`go/login/etc/login.yaml:97-115` 与 `k8s_deploy.ps1:1271-1285`(login ConfigMap 模板)两处 RPC 客户端块都没有 NonBlock;`deploy/k8s/README.md:375-377` 登记「要消除只能把客户端改成 lazy dial 或给 login 加 initContainer」;kind 上 login 两副本 RESTARTS 6 / 16。go-zero v1.9.2(`go/login/go.mod:15`)`zrpc/config.go:30` 有 `NonBlock bool json:",optional"`,`zrpc/client.go:58-59` 读到即 `WithNonBlock()`。本地整栈重启因此必须按 scene_manager → player_locator → login 逐个等端口(PROGRESS.md:3787-3789)。网关:README:381-382「`login.rpc channel up (zone=1): 127.0.0.1:53000` 三行是 application.yaml 静态兜底」——`java/gateway_node/src/main/resources/application.yaml:84` `endpoints: "1=127.0.0.1:53000,…"`,而 `k8s_deploy.ps1:1540-1560` 生成的 gateway ConfigMap 没覆盖 `login.grpc.endpoints`,K8s 上 Spring 仍拨 127.0.0.1。

**要改的文件**
- `go/login/etc/login.yaml`(:97-115)、`go/login/internal/svc/servicecontext.go`(:118/:124)
- `go/player_locator/etc/player_locator.yaml`(SceneManagerRpc 块)、`go/player_locator/internal/svc/servicecontext.go`(:55)
- `tools/scripts/k8s_deploy.ps1`(:1271-1285 login 模板;~1353-1360 player-locator 模板;:1540-1560 gateway 模板)
- `deploy/k8s/README.md`(:375-377、:381-382)
- 可选:`go/data_service/internal/svc/servicecontext.go:94`(LoginAdminRpc 同模式)
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`

**步骤**
1. `login.yaml` 的 `PlayerLocatorRpc:` 与 `SceneManagerRpc:` 块各加 `NonBlock: true`(与 Timeout 同级);player_locator 的 etc yaml SceneManagerRpc 块同加。
2. `k8s_deploy.ps1:1271-1285` login ConfigMap 模板两块同加;player-locator 模板的 SceneManagerRpc 块同加(grep `scenemanagerservice.rpc` 定位)。契约测试加一条「dev yaml 与 K8s ConfigMap 的 NonBlock 同为 true」(照第 27 条「dev 与 K8s 一致」思路)。
3. `servicecontext.go:118/:124` 保留 MustNewClient,上方加注释「NonBlock:对端未注册时不 fatal,RPC 期间返回 Unavailable」;若要代码兜底不依赖 yaml,改 `zrpc.MustNewClient(conf, zrpc.WithNonBlock())`。
4. grep `svcCtx.PlayerLocatorClient\|svcCtx.SceneManagerClient` go/login/internal/logic,确认 gRPC 错误已映射成业务错误码而非 panic;有裸 panic 的补 error 分支。
5. 网关:gateway ConfigMap 模板(:1540-1560)追加 `login: grpc: endpoints: "" / discovery-enabled: true / discovery-interval-ms: 5000`;核对 Java 侧解析 `login.grpc.endpoints` 为空字符串不抛异常(搜 gateway_node 中解析 endpoints 的类);`application.yaml:84` 本地 compose 值不动。
6. 可选:data_service `:94` 的 LoginAdminRpc 同加 NonBlock。
7. README:375-377 改「已修:配 NonBlock」,:381-382 改「K8s ConfigMap 已置空 endpoints,只走 etcd 发现」;PROGRESS.md 追加;§1.4 第 4 步的补拉顺序描述同步更新。

**验收标准**
- 本地:仅起 etcd,不起 player_locator/scene_manager,直接起 login,进程不退出且日志 STARTED SUCCESSFULLY;随后起 player_locator、scene_manager,不重启 login 即可完成一次登录(gateway:8081 走通)。
- kind:`kubectl rollout restart deploy/player-locator deploy/scene-manager deploy/login -n mmorpg-zone-yesterday` 后 login RESTARTS 增量 0;`kubectl logs … --previous` 为空。
- player_locator 在 scene_manager 未起时同样不退出。
- gateway Pod 日志不再出现 `login.rpc channel up (zone=1): 127.0.0.1:53000`,仍出现 `discovery routing changed`。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\login; go build ./... ; go test ./... -count=1
# 本地顺序验证(WMI 起进程):先停 player_locator/scene_manager,只起 login,看日志无 logx.Must/panic;再起两者;curl http://127.0.0.1:8081/api/server-list 并走一次客户端登录
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName yesterday -ZoneId 1 -GoSvcRegistry local -JavaSvcRegistry local -NodeImage local/mmorpg-node:<tag> -GateReplicas 1 -SceneReplicas 1 -DryRun | Select-String 'NonBlock|endpoints'
kubectl rollout restart deploy/player-locator deploy/scene-manager deploy/login -n mmorpg-zone-yesterday; kubectl rollout status deploy/login -n mmorpg-zone-yesterday; kubectl get pods -n mmorpg-zone-yesterday -l app=login
kubectl logs -n mmorpg-zone-yesterday deploy/gateway | Select-String '53000|discovery routing'
```

**风险与注意**
NonBlock 把失败从启动期推到首个请求(Unavailable 立刻返回而不是等 5s),login 的 EnterScene/定位调用要能把错误回给客户端让其重试;可能出现 login Ready 但下游还没接上的短窗口(现状是干脆崩,更差);需确认 login readiness 能反映依赖状态,否则「Ready 但登录全失败」。网关置空 endpoints 若 Java 解析层要求非空会启动失败,先本地用 `--spring.config.additional-location` 试。

---

### P1-06 Kafka 慢起时 go/db、go/login 在 EnsureTopics 失败直接 panic → 按错误类型区分:连接类退避重试,分区契约不符才 fatal

**领域** server-go · **工作量** M · **状态** open

**背景与证据**
`go/db/db.go:64-72` 与 `go/login/login.go:72-79` 两处 `panic(fmt.Sprintf("Kafka db-task partition contract rejected: %v", err))`;`go/shared/kafkautil/topic_init.go:26-131` EnsureTopics 把「kafka admin connect」「kafka list topics」(连接类)与「partition contract mismatch」「immutable partition marker conflicts」(契约类)都以 fmt.Errorf 字符串返回,无 sentinel;`go/match/match_service.go:79-97` 已有先例(结果消费者 EnsureTopics 失败改 Errorf + 30s 后台重试);`topic_init_test.go` 只有 TestPartitionContractMarkerIsStableAndUnambiguous;`deploy/k8s/manifests/go-svc/{db,login}.yaml` 无 initContainers。后果:本地整栈重启 Go 服务必须等 Kafka 起完再拉(§1.4 第 4 步),K8s 上 db/login 与 kafka 同时起就 CrashLoop。

**要改的文件**
- `go/shared/kafkautil/topic_init.go`、`go/shared/kafkautil/topic_init_test.go`
- `go/db/db.go`(:64-72)、`go/login/login.go`(:72-79)
- `go/match/internal/kafka/result_consumer.go`、`go/match/match_service.go`(:79-97 参考)

**步骤**
1. `topic_init.go` 新增 `var ErrPartitionContract = errors.New("kafka partition contract violated")`;把 `partition contract mismatch`、`immutable partition marker conflicts`、`invalid partition contract`、`topic name is empty` 四处改为 `fmt.Errorf("...: %w", ErrPartitionContract)`;其余(admin connect / list topics / create topic / refresh / alter retention)保持包装底层 err。
2. 新增 `func EnsureTopicsWithRetry(ctx context.Context, brokers []string, specs []TopicSpec, maxWait time.Duration) error`:循环调 EnsureTopics;`errors.Is(err, ErrPartitionContract)` 立即返回;否则 `logx.Errorf` 打「Kafka 暂不可达,Ns 后重试」并 2s→4s→…封顶 30s 退避,直到 ctx.Done 或超过 maxWait(login/db 传 10 分钟;可给运维 `KafkaStartupWaitSeconds` 配置项)。
3. `db.go:64-72` / `login.go:72-79` 改调 WithRetry;返回 err 仍 panic,文案区分:契约类 → 原「partition contract rejected」;否则 →「Kafka 在 N 内不可达,拒绝启动」。
4. `result_consumer.go` 的 EnsureTopics 改用 sentinel 判定:契约不符时不再无限 30s 重试(现状 `match_service.go:84-97` 两类错误一视同仁),直接 Errorf 并停止重试。
5. `topic_init_test.go`:把 EnsureTopics 抽成可注入的 `ensureFn`,构造返回 ErrPartitionContract 的假函数验证立即返回;构造连接类错误验证会重试并在 ctx 取消后返回;不需要真 Kafka。
6. PROGRESS.md 记一条;README B 档节补「db/login 与 kafka 同时起不再 CrashLoop」。

**验收标准**
- 本地:`docker stop kafka` 后起 login 与 db,进程存活并每次重试打 Errorf;`docker start kafka` 后 30s 内两者打出 topic ensured 与 STARTED SUCCESSFULLY,无 panic。
- 把 `login.yaml Kafka.PartitionCnt` 临时改成与现有 topic 不同的值启动 → 立即 panic 且文案含 partition contract mismatch。
- kind:`rollout restart deploy/kafka` 同时 `rollout restart deploy/db deploy/login`,RESTARTS 增量 0。
- go/shared/kafkautil、go/db、go/login、go/match `go test ./... -count=1` 全绿。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\shared; go test ./kafkautil/ -count=1 -v
cd E:\work\xuanming-server-mmo\go\login; go build ./... ; go test ./... -count=1; cd ..\db; go build ./... ; go test ./... -count=1; cd ..\match; go build ./... ; go test ./... -count=1
docker stop kafka; # WMI 起 login/db; Select-String -Path <login 日志>,<db 日志> -Pattern 'Kafka 暂不可达|panic'; docker start kafka; # 60s 后 Select-String 'STARTED SUCCESSFULLY'
kubectl -n mmorpg-infra rollout restart deploy/kafka; kubectl -n mmorpg-zone-yesterday rollout restart deploy/db deploy/login; kubectl -n mmorpg-zone-yesterday get pods -l 'app in (db,login)' -w
```

**风险与注意**
无限重试会让「Kafka 地址配错」从秒级暴露变成一直 Errorf 卡在启动期,K8s readiness 不会 Ready——默认设上限(10 分钟)并在日志首行打出 brokers;sarama 错误类型不统一,用「非 ErrPartitionContract 即可重试」的反向判定比枚举连接错误更稳。

---

### P1-07 Windows 保留端口区间规避只在 go_services.ps1;cpp_nodes.ps1 不设 RPC_PORT、无 Resolve-BindablePort、不经 WMI

**领域** ops-tooling · **工作量** M · **状态** open

**背景与证据**
`tools/scripts/go_services.ps1:224-250` 有 Get-ExcludedPortRanges / Test-PortExcluded / Resolve-BindablePort(解析 `netsh interface ipv4 show excludedportrange protocol=tcp`),`:265` 调用。`tools/scripts/cpp_nodes.ps1`(481 行)grep `excluded|reserved|RPC_PORT|Resolve-BindablePort` 全无命中,只设置 ZONE_ID/NODE_IP(:357-360),用 Start-Process 起(:362)。C++ 侧 `node.cpp:305` 读 `RPC_PORT`/`NODE_PORT`;`node_allocator.cpp:186` `kGrpcPortOffset = 30000`,`:224/:283/:305` 预设端口与 gRPC 端口拿不到均 fail-closed「caller must retry」(dd5a6d8f5)但不会自动换端口。scratchpad `relaunch_cpp_nodes.ps1:14-22` 硬编码端口且不探保留区间。PROGRESS.md:3787-3791(09-05 第二次撞上,60279-60378 吃掉 scene_manager 60300);`/api/server-list` 却仍显示 OPEN(:3789),排查成本高。

**要改的文件**
- 新建 `tools/scripts/lib/port_common.ps1`;`tools/scripts/go_services.ps1`(改为 dot-source)
- `tools/scripts/cpp_nodes.ps1`(:357-362)
- 新建 `tools/scripts/tests/port_common.tests.ps1`(用 `tests/lib/test_harness.ps1`)
- scratchpad `relaunch_cpp_nodes.ps1`(入库时改为薄封装,见 P1-09)

**步骤**
1. 新建 `lib/port_common.ps1`,把 go_services.ps1:224-250 三个函数原样搬入,增加 `Resolve-NodePortPair([int]$TcpPort, [string]$Label)`:循环 p 从 TcpPort 起,直到 `-not (Test-PortExcluded p) -and -not (Test-PortExcluded (p+30000))` 且两端口本机无 LISTEN(`Get-NetTCPConnection`),返回 p;go_services.ps1 改为 dot-source。
2. `cpp_nodes.ps1` 新增参数 `-RpcPorts @{ gate=10000; scene=20000; battle=20100 }`(每实例 +1,zone 位移与 go_services 的 `-ZonePortShift` 同源),启动每个实例前调用 Resolve-NodePortPair 并设 `$env:RPC_PORT`(参考 :357-360 对 ZONE_ID 的保存/恢复写法),日志打印实际端口;未传时保持现状(etcd 自选)。
3. `:362` Start-Process 增加 `-UseWmi` 开关,经 `Invoke-CimMethod -ClassName Win32_Process -MethodName Create` 起进程;环境变量通过 `cmd /c set RPC_PORT=… && set ZONE_ID=… && start /b …` 或 Win32_ProcessStartup 传入;stdout 用 `cmd /c … > log 2>&1` 包装。
4. `port_common.tests.ps1`:mock `netsh` 输出含 `20000 20099 *` 时 Resolve-NodePortPair 20000 返回 20100;含 `50000 50099` 时 20000 也要跳过(gRPC 落区间)。
5. relaunch 类脚本入库时改为调用 `cpp_nodes.ps1 -RpcPorts -UseWmi`。

**验收标准**
- `pwsh tools/scripts/tests/port_common.tests.ps1` 全过。
- 人工制造保留区间(`netsh int ipv4 add excludedportrange protocol=tcp startport=20000 numberofports=10`,管理员,测完 delete)后 `cpp_nodes.ps1 -Nodes scene -RpcPorts @{scene=20000} -UseWmi` 起的 scene 日志显示实际端口 ≥20010,etcd NodeInfo 端口一致,无「caller must retry」循环。
- 节点在工具调用结束后仍存活(`Get-Process scene` 仍在)。
- `go_services.ps1 -Command status` 行为不变。

**验证方式**
```powershell
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/tests/port_common.tests.ps1
netsh interface ipv4 show excludedportrange protocol=tcp
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/cpp_nodes.ps1 -Command start -Nodes scene -Zone 1 -RpcPorts @{scene=20000} -UseWmi; Get-Content E:/work/xuanming-server-mmo/run/logs/cpp_nodes/z1_scene.stdout.log -Tail 50 | Select-String 'port|retry'
docker exec etcd etcdctl get --prefix /node/ | Select-String 'scene'
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/go_services.ps1 -Command status
```

**风险与注意**
C++ gRPC 端口硬绑 TCP+30000,只能整体挪 TCP;若挪到与 go 服务 gRPC(50000-60000 段)撞车,需把本机 LISTEN 一并排除。WMI 起进程时 stdout 无法 Redirect,必须 cmd 包装。

---

### P1-08 bin/ 里在跑的 gate/scene/battle 仍是 09-04 旧版本:停栈重拷 09-05 产物 → 重拉 → 三条冒烟 + 「杀 battle 立刻重拉」fail-closed 运行时验证

**领域** ops-tooling / server-cpp · **工作量** S · **状态** open

**背景与证据**
PROGRESS.md:3996-3997「拷 bin/ 因进程占用跳过——bin/ 里在跑的仍是 09-04 拉取前的版本」。实查 `bin/*.exe` 时间戳 2026-09-04 10:24 / 10:41 / 10:44;`build/cpp/nodes/{battle,gate,scene}.exe` 2026-09-05 06:42-06:43;`lib/core.lib` 09-05 06:31;`Get-Process` 显示 battle(PID 51380)、gate(10572/54756)、scene(39872/42408/50976/54284)均从 `bin\*.exe` 起于 09-05 02:13-02:31。HEAD d08843042 含上游 d007e448e(配表 schema 迁 proto)、5ef65b3f8(背包准入)等 12 提交,未在任何运行栈上跑过冒烟。同时提交 dd5a6d8f5(09-05 02:36)修的 gRPC 端口 fail-closed(`node_allocator.cpp:305-325`)没有运行时证据:scratchpad `verify_grpc_failclosed.log` 只有 09-04 10:28 的 begin/killed/relaunched 三行(脚本被中断且跑的是旧二进制);PROGRESS.md:3741「待抓帧结束后串行重编三节点并用'杀 battle → 立刻重拉'复现验证」。

**要改的文件**
- `bin/`(拷入)、`build/cpp/nodes/`(来源)
- `robot/etc/battle_smoke.yaml`、`battle_smoke_cross_zone.yaml`、`attribute_smoke.yaml`(只跑)
- `E:/work/mmorpg-client/tools/run_crosszone_pair.ps1`(只跑)
- scratchpad `relaunch_cpp_nodes.ps1`、`verify_grpc_failclosed.ps1`、`reset_smoke_state.ps1`(只跑)
- `PROGRESS.md`

**步骤**
1. 确认 build/ 产物对应 HEAD:`git log -1 --format='%h %ad'` 与 `build/cpp/nodes/*.exe` 时间戳晚于最后一次 pull 且晚于 `lib/core.lib`(06:31);不确定则按 §1.2 顺序 `/m:1` 重编 proto→rpc→table→core→battle→scene→三节点。
2. 与并行会话确认后停 C++ 节点:`Get-Process gate,scene,battle | Stop-Process -Force`(Go 与 Docker 不用停)。
3. `Copy-Item build/cpp/nodes/{gate,scene,battle}.exe bin/ -Force`,核对 `(Get-Item bin/scene.exe).LastWriteTime`。
4. 等 ≥5s 让 etcd 旧租约过期,再按显式端口重拉(§1.3 端口表;scratchpad `relaunch_cpp_nodes.ps1`,入库后用 `cpp_nodes.ps1 -RpcPorts -UseWmi`)。
5. `Invoke-WebRequest http://127.0.0.1:8081/api/server-list` 确认 zone 1/2 都 OPEN;etcd 里 scene/battle 已注册。
6. 跑 `reset_smoke_state.ps1`(传当前 robot_9003/9004 的 player_id,见 P2-07)后依次:`robot.exe -c etc/battle_smoke.yaml`、`-c etc/battle_smoke_cross_zone.yaml`、`-c etc/attribute_smoke.yaml`;再跑 `run_crosszone_pair.ps1`。
7. fail-closed 验证:跑 scratchpad `verify_grpc_failclosed.ps1`(Stop-Process battle 后立刻起 battle.exe,ZONE_ID=1 RPC_PORT=20100,轮询 `z1_battle.stdout.log` 最多 6 分钟)。注意该脚本用 Start-Process 起 battle,在 AI 会话里活不过工具调用,改用 cpp_nodes.ps1 的 WMI 路径。
8. 检查日志出现 `saw fail-closed retry line`、`saw Assigned gRPC port`、`etcd grpc_endpoint present: True`、gate 无 `grpc_endpoint is empty`(或最后一条早于重拉时刻);再跑一次 battle_smoke。
9. 把结果写进 PROGRESS.md「2026-09-05 拉取上游后的核对」条目(替换「bin/ 里在跑的仍是 09-04 版本」),并在 09-04 条目删除「待…复现验证」。

**验收标准**
- `Get-Process gate,scene,battle | % Path | Get-Item | % LastWriteTime` 全部 ≥ 2026-09-05 06:31。
- 三条 robot 冒烟各出成功标记(`BATTLE_SMOKE_OK` / `CROSS_ZONE_MATCH_OK` / `ATTRIBUTE_SMOKE_OK player_id=…`,`robot/attribute_smoke_scenario.go:439`),退出码 0;`run_crosszone_pair.ps1` 输出 `CROSS_ZONE_PAIR_PASS`。
- scene/battle stdout 无 `LoadTables` Fatal、无「caller must retry」循环。
- 新 battle 进程日志先有 `gRPC port 50100 ... nothing published to etcd, caller must retry`,随后 `Assigned gRPC port: 50100`;`etcdctl get --prefix BattleNodeService.rpc/zone/` 的值含 grpc_endpoint 且 port=50100;gate 日志在重拉后不再出现 `Cannot connect to GRPC node: grpc_endpoint is empty`。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; git log -1 --format='%h %ad' --date=iso; Get-Item build/cpp/nodes/*.exe,bin/*.exe,lib/core.lib | Select Name,LastWriteTime
Get-Process gate,scene,battle -ErrorAction SilentlyContinue | Select Name,Id,StartTime,Path
Get-Process gate,scene,battle -ErrorAction SilentlyContinue | Stop-Process -Force; Start-Sleep 6; Copy-Item E:/work/xuanming-server-mmo/build/cpp/nodes/*.exe E:/work/xuanming-server-mmo/bin/ -Force
# 经 WMI 重拉;(Invoke-WebRequest http://127.0.0.1:8081/api/server-list -UseBasicParsing).Content
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml; .\robot.exe -c etc/battle_smoke_cross_zone.yaml; .\robot.exe -c etc/attribute_smoke.yaml
pwsh -File E:/work/mmorpg-client/tools/run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe
docker exec etcd etcdctl get --prefix BattleNodeService.rpc/zone/
Select-String E:/work/xuanming-server-mmo/run/logs/cpp_nodes/z1_gate.stdout.log -Pattern 'grpc_endpoint is empty' | Select -Last 3
```

**风险与注意**
另一并行会话可能正在用运行栈,停节点前先确认;build/ 与 HEAD 不同步会验证到错误版本;robot 冒烟需 battle:lock 解冻(≈5~6 分钟)才能复跑,连续三条注意间隔或换账号;etcd 旧租约未过期时新进程 `Preset RPC port already registered` 自杀(重拉前 ≥5s)。

---

### P1-09 47 个运维/验收 .ps1 只在会话 scratchpad(含演出验收链 4 个),tools/scripts 与 mmorpg-client/tools 均无对应文件,文档直接引用临时路径

**领域** ops-tooling / client-presentation · **工作量** L · **状态** open

**背景与证据**
实查 `C:\Users\luyua\AppData\Local\Temp\claude\E--work\3f3f6928-…\scratchpad\` 共 47 个 .ps1(2026-09-02 ~ 09-05,清单见附录 B)。`tools/scripts` 无任一同名文件;`mmorpg-client/tools` 只有 build_crosszone_player.ps1 / gen_messageids.ps1 / gen_proto.ps1 / run_crosszone_pair.ps1 / tianyong_tile_pipeline.py。`docs/design/turn-battle-presentation.md:110`「scratchpad `run_showcase.ps1` 一键」;PROGRESS.md:3557(verify_phase2_runtime.ps1)、:3629(relaunch_cpp_nodes.ps1)、:3648(full_restart.ps1)、:3782(run_showcase.ps1);memory `xuanming-phase2-battle-presentation` 接续点整段依赖该目录;`docs/design/script_directory_rules.md:3`「tools/scripts is the canonical home for maintained repo scripts」。`relaunch_cpp_nodes.ps1:3` 硬编码 `$S = "C:\Users\luyua\…\scratchpad"`、`:13` `NODE_IP='192.168.43.7'`;`run_showcase.ps1:18`、`live_capture.ps1:5` 同样硬编码 `$S`。候选项引用的 `scratchpad/tasks/*.output` 复审 journal 已不存在。这些脚本承载了本机运维全部隐性知识(WMI 逃逸、显式端口、Kafka 慢起补拉顺序、Docker 恢复顺序、清 etcd 旧租约、演出台出包→72 帧核对→实机 live_capture 验收链),换会话/清 Temp 即丢。

**要改的文件**
- scratchpad 全目录(来源);`E:/work/xuanming-server-mmo/tools/scripts/dev/`(新建)、`tools/scripts/lib/process_common.ps1`(新建)、`tools/scripts/README.md`
- `E:/work/mmorpg-client/tools/`(新增 run_showcase / live_capture / build_presentation_player / client_compile_check / unity_copy_compile)
- `docs/design/turn-battle-presentation.md`(:110、§6 minor 余项)、新建 `docs/ops/local-runbook.md`、`PROGRESS.md`

**步骤**
1. 先 `Copy-Item -Recurse <scratchpad> E:/work/tmp/scratchpad_backup_20260905` 防丢,再分类。
2. 服务端运维类 → `tools/scripts/dev/`:`full_restart.ps1`(合并 recover_stack*.ps1 / docker_recover*.ps1 / fix_z1_login.ps1 / post_recover.ps1 为子步骤)、`relaunch_cpp_nodes.ps1`(改为薄封装调 `cpp_nodes.ps1 -RpcPorts -UseWmi`,见 P1-07)、`build_cpp_serial.ps1`(合并 build_cpp_serial(2) / build_then_restart / rebuild_restart_battle_scene / rebuild_restart_scenes,顺序 proto→rpc→table→core→battle→scene→节点→测试)、`restart_both_match.ps1`、`reset_smoke_state.ps1`(P2-07)、`run_smoke.ps1`、`verify_phase2_runtime.ps1`、`verify_grpc_failclosed.ps1`。
3. 客户端验收类 → `mmorpg-client/tools/`:`run_showcase.ps1`、`live_capture.ps1`、`build_presentation_player.ps1`、`client_compile_check.ps1`(及其 rt.rsp/ed.rsp/test.rsp,放 `tools/compile_check/`)、`unity_copy_compile.ps1`;带 .SYNOPSIS/.DESCRIPTION 注释块;`$S = ...scratchpad` 改 `$PSScriptRoot`,robocopy 目标与 Library 种子目录改为参数。
4. 一次性探针类(exitcode_probe / metrics_regex_test / resp_moved_probe / player_probe / switch_cluster / start_* 单件 / after_scenes_smoke / failover_test / rerun_and_test / build_verify_only)不入仓,知识写进 `docs/ops/local-runbook.md`:Docker Desktop exe 位置、恢复顺序、端口表(§1.3)、robot 账号与 battle:lock 解冻时间。
5. 新建 `tools/scripts/lib/process_common.ps1`:`Start-DetachedProcess -FilePath -Arguments -Env @{} -LogPath`,内部 `Invoke-CimMethod Win32_Process Create` + `cmd /c set … && exe > log 2>&1`;迁入脚本统一调用,删掉 `NODE_IP='192.168.43.7'` 等硬编码,改参数(默认从 cpp_nodes.ps1 的 `Find-PhysicalNicIPv4` 取)。
6. 文档改引用:`turn-battle-presentation.md:110` 改 `mmorpg-client/tools/run_showcase.ps1`,并在 §6「已知余项(minor)」下把本清单 P2-21~P2-27、P3-10~P3-14 逐条登记(替代已丢失的 journal);PROGRESS.md 历史条目不改,`tools/scripts/README.md`「Available Scripts」新增各脚本小节;`full_restart` 依赖的 `go_services.ps1 stop` 全杀语义变更见 P2-08。

**验收标准**
- `ls tools/scripts/dev` 至少含 full_restart / relaunch_cpp_nodes / build_cpp_serial / reset_smoke_state / run_smoke / verify_phase2_runtime / verify_grpc_failclosed;`ls mmorpg-client/tools` 含 run_showcase / live_capture / build_presentation_player / client_compile_check。
- `git grep -n scratchpad -- docs tools`(服务端)与 `-- tools docs`(客户端)为空(PROGRESS.md 历史条目除外);`grep -rn 'C:\\Users\|192.168.43.7' tools/scripts/dev mmorpg-client/tools` 为空。
- 全新 PowerShell 窗口执行 `tools/scripts/dev/full_restart.ps1` 能把整栈拉起(server-list 双 OPEN,`run_smoke.ps1` PASS);`mmorpg-client/tools/run_showcase.ps1 -SkipBuild` 产出 ≥30 帧;`live_capture.ps1` 在服务栈起着时输出 CROSS_ZONE_PAIR_PASS。

**验证方式**
```powershell
Copy-Item -Recurse 'C:/Users/luyua/AppData/Local/Temp/claude/E--work/3f3f6928-3172-48ea-9ae4-d84fff460133/scratchpad' E:/work/tmp/scratchpad_backup_20260905
cd E:/work/xuanming-server-mmo; git grep -n scratchpad -- docs tools; cd E:/work/mmorpg-client; git grep -n scratchpad -- tools docs
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/dev/full_restart.ps1; (Invoke-WebRequest http://127.0.0.1:8081/api/server-list -UseBasicParsing).Content
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/dev/run_smoke.ps1 -Config etc/battle_smoke_cross_zone.yaml
pwsh -NoProfile -Command "Get-Command -Syntax E:\work\mmorpg-client\tools\run_showcase.ps1"
pwsh -File E:/work/mmorpg-client/tools/run_showcase.ps1 -ShotDir E:/work/tmp/showcase_shots_verify; (Get-ChildItem E:/work/tmp/showcase_shots_verify/*.png).Count
```

**风险与注意**
脚本含大量本机特定值(NIC IP、Docker 路径、端口),参数化不彻底会在别的机器上静默错;PROGRESS.md 历史记录不改写;Unity 侧脚本依赖编辑器被占用时打副本工程(memory `mmorpg-client-player-build-verify`),迁入时保留 robocopy 副本逻辑。

---

### P1-10 class_id 未随 PlayerAllData 下发 scene:`PlayerUint32Comp.class` 有读方无写方,loader 仍取 ClassTable 首行

**领域** server-cpp / server-go · **工作量** M · **状态** partially-done

**已完成部分**:读方已就位——`cpp/libs/services/scene/player/system/player_attribute.cpp:66-82` `PlayerClassId` 读 `PlayerUint32Comp.class_()`,`ResolveClassRow` 非 0 即按 class 取行,`:577-580` AutoAllocate 先按 class 再 0 兜底;`proto/common/component/player_comp.proto:44-47` `PlayerUint32Comp.class` 已在 `player_database.uint32_pb_component`(`mysql_database_table.proto:109`);loader emplace/CopyFrom(`player_database_loader.cpp:77,102`)。

**背景与证据**(剩余部分):写方缺失——`go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go:49-50` 建父 key 时只填 `PlayerId`,全仓 Go 侧 `Uint32PbComponent` 仅 pb.go 命中;class_id 只存在 `AccountSimplePlayer`(`user_accounts.proto:9`,账号级 Redis)。`player_database_loader.cpp:26-32` 初始属性/复活仍 `classRows.Get(0)`,`player_revive.h:18-19` 注释确认。`docs/design/player-attribute-allocation.md` §8、`turn-based-battle-server.md` §15.4 均列为缺口。后果:职业初始属性、技能集、自动加点方案全取首行/兜底行,职业系统形同虚设(客户端外观按职业 P2-D8 也依赖本条)。

**要改的文件**
- `go/login/internal/logic/pkg/dataloader/ensure_player_all_data_async.go`、`go/login/internal/logic/clientplayerlogin/entergamelogic.go`、`createplayerlogic.go`
- `cpp/libs/services/scene/player/system/player_database_loader.cpp`、`player_revive.h`、`player_attribute.cpp`
- `cpp/tests/turn_battle_engine_test/table_battle_data_provider_test.cpp`
- `robot/etc/attribute_smoke.yaml`、`docs/design/player-attribute-allocation.md`、`turn-based-battle-server.md`

**步骤**
1. Go login:EnterGame 路径已持有 `userAccount.SimplePlayers`(entergamelogic.go),把对应 player 的 ClassId 传入 `EnsurePlayerAllDataInRedisAsync`,在 `:49` 建父 key 时填 `PlayerDatabaseData.Uint32PbComponent = &PlayerUint32Comp{Class: classId}`;老号(DB 已有行且 class==0)也补写一次(幂等)。
2. scene loader:`ApplyClassInitialAttributesOrReviveFromTable` 改为先 `PlayerClassId(player)` → `FindByIdSilent`,找不到再回退首行(把 `ResolveClassRow` 从 player_attribute.cpp 匿名区提到头文件复用);同步 `player_revive.h:18-19` 注释。
3. battle 快照:`PlayerBattleSystem::PrepareBattle` 组 BattlePlayerSnapshot 时技能列表来源核对——若按 Class.skill 取,也要按 class 行取(grep `ClassTable` in `cpp/libs/services/scene/battle`)。
4. 测试:`table_battle_data_provider_test.cpp:146` `AllClassRowsShareSameInitialAttributes` 钉的是临时契约,若 Class.xlsx 后续差异化改为「每行 init_* > 0」;新增用例 `PlayerUint32Comp.class=2` 时 ResolveClassRow 返回 id=2 行。
5. robot attribute_smoke:CreatePlayer 用非 0 class_id(`login.proto:54`),登录后断言面板属性 == Class 表对应行 init_*。
6. 更新两份设计文档缺口段。

**验收标准**
- 新建 class_id=2 角色首次进 scene,Redis `PlayerAllData:{pid}.player_database_data.uint32_pb_component.class == 2`。
- scene 日志 `[PlayerInit]` 按 class 行赋初始属性(加一条含 class_id 的 INFO),面板 max_health 等于该行 init_health 经 AttributeDimension 换算。
- 自动加点选到该职业专属 AttributeAutoPlan 行(有配置时);老号(class=0)行为不变。
- `run_cpp_tests.ps1 -Filter turn_battle_engine_test` 全绿;robot attribute_smoke OK;battle_smoke OK。

**验证方式**
```powershell
. E:/work/tools/buildenv.ps1; cd E:/work/xuanming-server-mmo/go/login; go build ./... ; go test ./internal/...
msbuild E:/work/xuanming-server-mmo/game.sln /t:scene /p:Configuration=Debug /p:Platform=x64 /m:1
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Filter turn_battle_engine_test -Build
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/attribute_smoke.yaml
docker exec redis redis-cli --raw GET 'PlayerAllData:<player_id>' # 用 protoc --decode 看 uint32_pb_component
```

**风险与注意**
login 与 scene 都改,Redis 父 key 写入时序要与 scene HandlePlayerAsyncLoaded 的「player_id==0 → 空数据补丁」路径(`player_lifecycle.cpp:212`)兼容;DB 无新列(复用 uint32_pb_component),无迁移;Class.xlsx 目前各行 init_* 相同,打通后行为无变化,差异化要策划改表。

---

### P1-11 经验系统未接:exp_gain 只记日志,等级只能 GmSetPlayerLevel

**领域** server-cpp · **工作量** L · **状态** open

**背景与证据**
`cpp/libs/services/scene/battle/system/player_battle.cpp:991-997` `经验结算暂缓(经验系统未接入)` 仅 LOG_INFO;引擎 BuildSettlement(`turn_battle_engine.cpp:1276-1300`)已按 Monster.exp_reward 累加 exp_gain 下发,scene 丢弃;`proto/common/event/player_event.proto:11-15` PlayerUpgradeEvent 只是事件壳,`player_attribute.cpp:704` 只有 GmSetLevel 触发;`data/` 下无升级曲线表;`cpp/libs` 无 ExperienceComp(grep 零命中);docs §15.4/§8 列为缺口。没有经验就没有成长循环,属性加点(AttributePool.unlock_level/points_per_level)只能靠 GM 驱动。

**要改的文件**
- `cpp/libs/services/scene/battle/system/player_battle.cpp`、`scene/player/system/player_attribute.{h,cpp}`、新建 `player_experience.{h,cpp}`
- `proto/common/component/player_comp.proto`、`proto/common/database/mysql_database_table.proto`、`proto/common/event/player_event.proto`
- `data/`(新增 LevelExp.xlsx + `data/schema/levelexp_table.proto`)、`tools/data_table_exporter/`
- `go/db/mysql_database_table.sql`(三份同步)、`robot/etc/battle_smoke.yaml`

**步骤**
1. 策划表:新增 `LevelExp.xlsx`(level, exp_to_next)并写 `data/schema/levelexp_table.proto`(照 monster_table.proto),跑导表器(PATH 含 protoc 与 protoc-gen-go)。
2. proto:`ExperienceComp { uint64 exp = 1; }` 加进 player_comp.proto,挂到 mysql_database_table.proto player_database 新字段号 10(产生 DB 新列 → 三份 mysql_database_table.sql 同步 + `go run ./cmd/migrate -command up` 上线前置,照 attribute_component 先例;K8s 上依赖 P1-03)。
3. scene:新建 `PlayerExperienceSystem::AddExp(player, exp)`:累加、按 LevelExp 表循环升级到 kMaxLevel=85 上限(2026-09-10 由 200 改为 85)、每升一级 trigger PlayerUpgradeEvent(属性系统已监听会 Recalculate + PushPanel + 加点池增长)。
4. `player_battle.cpp:993-997` 日志分支替换为 `PlayerExperienceSystem::AddExp(player, settlement.exp_gain())`,保留日志。
5. bag_marshal / player_database_loader:ExperienceComp 随 PlayerAllData 存取(照 LevelComp,loader.cpp:77/102)。
6. 测试:新建 experience_test(照 currency_test 工程结构)覆盖单级/连升多级/满级截断/升级事件次数,登记进 run_cpp_tests.ps1 与 game.sln;robot battle_smoke 结算后断言 LevelComp 上升或 exp 增加(读 AttributePanel)。

**验收标准**
- PVE 胜利结算后 Redis/DB 中 ExperienceComp.exp 增加 = 击杀怪物 exp_reward 之和。
- exp 达阈值自动升级,面板 level 变化且属性重算、加点池按 points_per_level 增长;85 级封顶不越界。
- run_cpp_tests.ps1 全绿;battle_smoke / attribute_smoke OK;AutoMigrateSchema=false 环境有 migrate 项。

**验证方式**
```powershell
# 导表(PATH 先加 protoc + gopath/bin);cd E:/work/xuanming-server-mmo/go; .\build.bat
. E:/work/tools/buildenv.ps1; cd E:/work/xuanming-server-mmo/go/db; go run ./cmd/migrate -command up
msbuild E:/work/xuanming-server-mmo/game.sln /t:scene /p:Configuration=Debug /p:Platform=x64 /m:1
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml
```

**风险与注意**
proto/DB 新列牵动 proto2mysql 迁移与三份 SQL;连升多级会触发多次 Recalculate/PushPanel(可合并只推一次面板);战斗中(InBattle)入账要过 CheckWritable 口径与离线挂起结算路径(player_battle.cpp 登录补应用)。bonus_values/bonus_points 同样无写入方(§8),等装备/任务系统,可后置。

---

### P1-12 GmSetPlayerLevel(175)/GmAddCurrency(37)与普通客户端 RPC 同口径可达,上线前并入 gate GM 鉴权

**领域** server-cpp · **工作量** M · **状态** open

**背景与证据**
`cpp/nodes/gate/gate_security.h` 的 GM 签名鉴权(VerifyGmRequestFromEnv,:270-360)全仓只在 `gate_service_handler.cpp:316` GmGracefulShutdown 一处使用;gate 对 `GmSetPlayerLevel|GmAddCurrency` 零命中;消息 175/37(`player_attribute_service_metadata.h:38`、`player_currency_service_metadata.h:6`)经 `client_message_processor.cpp:640-720` 与普通消息同路径转发到 scene;scene 侧 `player_attribute_handler.cpp:151` 注释「上线前经 gate GM 鉴权白名单收口」;docs §8、PROGRESS.md:3715 列为缺口。任何已登录客户端可把自己升到满级(2026-09-10 起上限 85)、加任意货币,是上线阻塞级漏洞;但 robot attribute_smoke 与 battle_smoke 依赖这两条,收口时必须保留 dev 通道。

**要改的文件**
- `cpp/nodes/gate/gate_security.h`、`cpp/nodes/gate/handler/rpc/client_message_processor.cpp`、`cpp/nodes/gate/tests/gate_security_test.cpp`
- `cpp/nodes/scene/handler/rpc/player/player_attribute_handler.cpp`、`player_currency_handler.cpp`
- `robot/etc/attribute_smoke.yaml`、`robot/attribute_smoke_scenario.go`
- `docs/design/player-attribute-allocation.md` §8

**步骤**
1. `gate_security.h` 新增 `kGmClientMessageIds = {SceneAttributeClientPlayerGmSetPlayerLevelMessageId, SceneCurrencyClientPlayerGmAddCurrencyMessageId}` 与 `IsGmClientMessage(id)`(用生成常量而非字面量 175/37)。
2. `client_message_processor.cpp` DispatchClientRpcMessage(~:700 ValidateClientMessage 之后)加闸:命中 GM 消息且 `GATE_RUN_MODE` 非 dev/test → `SendTipToClient(kPermissionDenied 类现有码)` + `LogClientSecurityRejectionSampled`;dev/test 放行(更严格的做法:两条请求 proto 加 `string gm_envelope` 走 VerifyGmRequestFromEnv,需重生成 proto)。
3. scene 侧二道锁:`player_attribute_handler.cpp:151` / `player_currency_handler` 同位置,读节点 Mode 非 dev 一律 SetTip 拒绝,防绕过 gate 直连。
4. `gate_security_test.cpp` 加用例:prod 模式 175/37 被拒、dev 放行、非 GM 消息不受影响;若 gate_security_test 不在 `run_cpp_tests.ps1` 的 21 工程表里先登记。
5. robot attribute_smoke 保持 dev 通道,日志打一行「GM 指令依赖 dev 模式」;文档 §8 更新。

**验收标准**
- `GATE_RUN_MODE=prod` 下客户端发 175/37 收到拒绝 tip,scene 无对应日志;dev 下 attribute_smoke `ATTRIBUTE_SMOKE_OK` 12 步。
- gate_security_test 新增用例通过;run_cpp_tests.ps1 全绿。

**验证方式**
```powershell
msbuild E:/work/xuanming-server-mmo/game.sln /t:gate;scene /p:Configuration=Debug /p:Platform=x64 /m:1
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build
$env:GATE_RUN_MODE='prod'; # 经 WMI 重拉 gate; cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/attribute_smoke.yaml  (预期第 1 个 GM 步失败)
$env:GATE_RUN_MODE='dev';  # 重拉 gate; .\robot.exe -c etc/attribute_smoke.yaml  (预期 OK)
```

**风险与注意**
消息号硬编码会随 proto 重生成漂移,务必引用生成常量;`MessageLimiter.xlsx` 对 175/37 的限流条目(PROGRESS 09-04)不受影响。

---

### P1-13 gate→match JoinQueue 在快速会话 churn 下偶发 40s+ 延迟(专项排查未启动)

**领域** server-cpp · **工作量** M · **状态** open

**背景与证据**
PROGRESS.md:3437 仍是「待 gate grpc-to-match 通道 churn 行为专项排查」,之后所有条目无处置;memory `xuanming-battle-spectate-phase2`「churn 坑」。可疑点(均未验证/排除):gate 对 match 的转发走 `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:687` PickRandomNode → `cpp/generated/grpc_client/match/match_service_grpc_client.cpp:37-73` SendMatchServiceJoinQueue,ClientContext 无 `set_deadline`(grep 零命中);CQ 由 `etcd_service.cpp:38` 每 5ms 轮询,`grpc_init_client.cpp:158-162` 在 `!ok` 时 `LOG_ERROR "RPC failed"; return;` 直接放弃本 tick 该 CQ 剩余事件且不释放 tag;通道由 `grpc_channel_cache.h:110-139` 按 target 以 weak_ptr 缓存、backup poll 5000ms。robot 每几秒登录/登出连跑时 JoinQueue 偶发 40s+ 才到 match,拉开间隔即稳;真实玩家不会这样,但重连风暴/压测会。

**要改的文件**
- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`
- `cpp/generated/grpc_client/grpc_init_client.cpp`、`match/match_service_grpc_client.cpp`(生成产物,改模板:grep `HandleCompletedQueueMessage` 找 tools/ 下 .j2/.tmpl)
- `cpp/libs/engine/core/node/system/grpc_channel_cache.h`、`etcd/etcd_service.cpp`
- `robot/battle_smoke_scenario.go`、`robot/etc/battle_smoke.yaml`、`go/match/internal/logic/joinqueuelogic.go`(只看时间戳)

**步骤**
1. 复现:PowerShell 循环每 5s 跑一次 `robot.exe -c etc/battle_smoke.yaml`,连跑 20 轮,记录每轮 JoinQueue 发出时刻与 match 日志 `[match] JoinQueue` 收到时刻,得到延迟分布。
2. gate 加计时日志:`client_message_processor.cpp:687` 选到 match 节点后、`rpcHandlerMeta.sender(...)` 前打 `LOG_INFO << "[diag] fwd match msg=" << message_id << " session=" << sessionId << " node=" << ...`;`grpc_init_client.cpp` HandleMatchServiceCompletedQueueMessage 入口打回包时刻;比对 match 侧收到时刻,定位延迟在「gate 发出前」「gRPC 在途」还是「match 处理」。
3. `grpc_init_client.cpp:158-162` 的 `!ok` 分支改 `LOG_ERROR << ...; delete grpcTag; continue;`(不能 return 放弃同 CQ 其余事件;tag 泄漏也要修)——改生成模板再重生成。
4. 出站 ClientContext 加 deadline:模板里 `call->context.set_deadline(now + 10s)`,卡死调用以 DEADLINE_EXCEEDED 回 CQ 并向客户端回 kServiceUnavailable。
5. 检查 churn 下 match 节点实体是否被 etcd watch 重建(gate 日志搜 `Cannot connect to GRPC node`/节点上下线):若实体重建 → weak_ptr 失效 → 新建 channel → gRPC 重连退避(默认初始 1s、最大 120s)就能解释 40s;对策 channel 缓存改 shared_ptr 常驻或设 `GRPC_ARG_MAX_RECONNECT_BACKOFF_MS`。
6. 修后重跑 20 轮,P99 应 < 2s;结论(根因 + 修法 + 数据)补记 PROGRESS.md 并删 :3437 的「待专项排查」。

**验收标准**
- 20 轮 5s 间隔 churn 下 JoinQueue gate 发出→match 收到 P99 < 2s,无 40s+ 样本。
- gate 日志无 `RPC failed` 后紧跟的 CQ 事件堆积;`!ok` 分支不再 return。
- 人为停掉 match 后 gate 10s 内回客户端 kServiceUnavailable 而非永久挂起。
- battle_smoke.yaml 与 battle_smoke_cross_zone.yaml 各 3 次 OK。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo/robot; . E:/work/tools/buildenv.ps1; go build -o robot.exe . ; 1..20 | % { .\robot.exe -c etc/battle_smoke.yaml 2>&1 | Select-String 'BATTLE_SMOKE_OK|BATTLE_SMOKE_FAIL|JoinQueue'; Start-Sleep 5 }
Select-String -Path E:/work/xuanming-server-mmo/run/logs/cpp_nodes/z1_gate.stdout.log -Pattern 'RPC failed|\[diag\] fwd match' | Select-Object -Last 40
Select-String -Path E:/work/xuanming-server-mmo/run/logs/go/match*.log -Pattern 'JoinQueue' | Select-Object -Last 40
msbuild E:/work/xuanming-server-mmo/game.sln /t:gate /p:Configuration=Debug /p:Platform=x64 /m:1   # 改模板后先重生成 grpc client 再编
# 经 WMI 重拉 gate(kill → sleep ≥5s → start)
```

**风险与注意**
生成产物直接改会被下次 regen 覆盖,必须改模板;加 deadline 把「静默挂起」变显式错误,需确认客户端对 kServiceUnavailable 的处理;并行会话可能同时重拉 C++ 节点(etcd 旧租约未过期会 Node ID hijack 自杀)。

---

### P1-14 tip 文案全仓下发:服务端 `tip_text.json` 已导出 102 条,客户端仍显示 `server tip=N` 裸编号,且 AttributeClient 手工映射(130-144)已因 tip 轴重排(→25000+)整体失效

**领域** client-infra / server-cpp · **工作量** M · **状态** partially-done

**已完成部分**:导表器 `tools/data_table_exporter/core/generators/enum_gen.py:564-591 _generate_tip_text` 已把 Tip.xlsx B 列导成 `generated/tables/tip_text.json`(实查 102 条,含 '16000':'你正在战斗中'、'16014':'匹配中无法观战'、'25003':'剩余点数不足');PROGRESS.md:3962。

**背景与证据**(剩余部分):`E:/work/mmorpg-client/Assets/Scripts/Game/Attribute/AttributeClient.cs:84-107 DescribeTip` 硬编码 130-144;但服务端 HEAD `cpp/generated/table/proto/tip/attribute_error_tip.pb.h:66-80` 已是 `kAttributePoolNotFound=25000 … kAttributeNothingToChange=25014`(scene `player_attribute.cpp:172-173` 用符号名,实际下发 25xxx)。时间线:143ecca96(09-03)还是 130-144;71edf77f7(09-05 04:48)重排到 25000;客户端 25ca096(09-05 02:09)写死 130-144 → 现在属性拒绝显示 `加点失败:tip=25003`。`BattleClient.cs:609-610`、`SpectateClient.cs:316-317` 只输出 `(tip=N)`,`GameClient.cs:737` 输出 `server tip=N`;`Assets/Scripts/Proto/Generated/Tip.cs` 只有 TipInfoMessage;全客户端 grep `tip_text|TipText` 零命中。服务端 `robot/attribute_smoke_scenario.go:67-71` 也写死 134/135/144(同样过期)。`tip-code-axis.md` §1 明确客户端应「拿 id 查文案」。

**要改的文件**
- `E:/work/xuanming-server-mmo/generated/tables/tip_text.json`(来源)、`robot/attribute_smoke_scenario.go`(:67-71)
- `E:/work/mmorpg-client/tools/sync_tip_text.ps1`(新建)、`Assets/Resources/Tables/tip_text.json`(新建)
- `Assets/Scripts/Game/TipText.cs`(新建)、`Assets/Scripts/Game/Attribute/AttributeClient.cs`、`Game/Battle/BattleClient.cs`、`SpectateClient.cs`、`Game/GameClient.cs`
- `Assets/Tests/EditMode/Battle/TipTextTests.cs`(新建)、`AttributeClientTests.cs`(:99-102)
- `docs/design/player-attribute-allocation.md` §8、`PROGRESS.md`

**步骤**
1. 服务端重跑导表器,确认 tip_text.json 含 25000-25014 与 16000-16020。
2. 新建 `mmorpg-client/tools/sync_tip_text.ps1`:拷 `generated/tables/tip_text.json` 到 `Assets/Resources/Tables/tip_text.json`(TextAsset,路径常量 `Tables/tip_text`);在 gen_proto.ps1 末尾调用或 README 注明「改 Tip.xlsx → 导表 → 跑此脚本」。
3. 新建 `TipText.cs`(namespace MmorpgClient.Game):`Resources.Load<TextAsset>("Tables/tip_text")` 懒加载解析 `Dictionary<uint,string>`(json 是 `{"1000":"成功",...}` 平铺 string→string,JsonUtility 不支持字典,照 `BattleArtCatalog.cs` L78/L805-830 的 json 读法手写小解析);`static string Resolve(uint id)`(缺文案回 `$"tip={id}"`)与 `static string Describe(string what, TipInfoMessage tip, uint errorCode=0)`;缺表 LogWarning 一次并全部 fallback;加 `internal static void LoadFrom(string json)` 测试入口。
4. 删 `AttributeClient.DescribeTip` 的 switch(L84-107)改委托 TipText;`BattleClient.DescribeTip`(L609)、`SpectateClient.DescribeTip`(L316)同改;`GameClient.Call` L737 改 `onError(TipText.Describe("server", mc.ErrorMessage))`。
5. `TipTextTests.cs`:注入内存 json 断言 25003→'剩余点数不足'、16014→'匹配中无法观战'、未知 99999→'tip=99999'、tip.Id==0 且 errorCode=7 → '(code=7)'。
6. `AttributeClientTests.cs:99-102` 信封错误断言从 `server tip=133` 改成新 Describe 格式期望。
7. 服务端顺带:`robot/attribute_smoke_scenario.go:67-71` 改 `uint32(table.AttributeError_kAttributePointsCannotDecrease)` 等(参照 `go/match/internal/constants/errors.go`),否则 attribute-smoke 在新二进制上必红。
8. 更新 §8 与 PROGRESS.md。

**验收标准**
- 客户端收到 `TipInfoMessage.id=25003` 时文案为「…:剩余点数不足」;16014 显示「匹配中无法观战」。
- `AttributeClient.cs` 内无 130-144 / 25000-25014 字面量;全客户端只有 TipText 一处解析。
- `Resources/Tables/tip_text.json` 与服务端 `generated/tables/tip_text.json` sha256 相同。
- EditMode TipTextTests 全绿,AttributeClientTests 11 条仍全绿;robot attribute-smoke 在重编后的 scene 上 `ATTRIBUTE_SMOKE_OK`。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; . E:\work\tools\buildenv.ps1; # 按 tools/data_table_exporter/README.md 跑导表器
Get-FileHash generated/tables/tip_text.json; pwsh -File E:/work/mmorpg-client/tools/sync_tip_text.ps1; Get-FileHash E:/work/mmorpg-client/Assets/Resources/Tables/tip_text.json
# 离线编译:按 memory mmorpg-client-compile-verify 用 Roslyn csc 编主程序集 + EditMode.Battle,0 错
# EditMode(副本工程,不带 -quit):Unity.exe -batchmode -projectPath E:/work/tmp/<copy> -runTests -testPlatform EditMode -assemblyNames MmorpgClient.Tests.EditMode.Battle -testResults <xml> -logFile <log>
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/attribute_smoke.yaml
```

**风险与注意**
另一并行会话可能在改 AttributeClient,先 git pull;服务端 bin/ 若仍是 09-04 旧版本(P1-08),真机看到的仍是 130-144 旧码,先停栈重拷重启;未来加码要记得同步 tip_text.json(fallback 仍可用,不会崩)。

---

### P1-15 PlaybackBudget 用客户端本地 UTC 与服务端 action_deadline_ms 直接相减,时钟偏差直接吃掉/放大演出预算

**领域** client-presentation · **工作量** M · **状态** open

**背景与证据**
`PlaybackBudget.cs:56` `budget = ((long)deadlineUnixMs - nowUnixMs) / 1000f - reserve`;`BattleScreen.cs:758-759` 传 `BattleUiWidgets.NowUnixMs()`(`BattleUiWidgets.cs:65-66` = DateTimeOffset.UtcNow);计时环 `:340/:438/:447/:464`、`BattleChallengePopup.cs:80`、`SpectatePanel.cs:176` 同一假设;客户端无任何时钟校正(grep ClockOffset/ServerTimeMs 只命中 proto 生成物);服务端 `proto/scene/player_movement.proto:70/79` 已下发 `server_time_ms`,`proto/battle/player_battle.proto:24` BattleStateS2C 只有 action_deadline_ms;`turn-battle-presentation.md` §6.1 按「now」计算但未定义来源。自动局窗口只有 2s,客户端快 1s 就把 1.5x 压到 6x 或 Skip;慢则每回合被抢占。手机端时钟偏差常见。

**要改的文件**
- `E:/work/mmorpg-client/Assets/Scripts/Game/Battle/Presentation/PlaybackBudget.cs`、`UI/Ugui/Battle/BattleScreen.cs`、`BattleUiWidgets.cs`、`Game/GameClient.cs`
- `Assets/Tests/EditMode/Battle/PlaybackBudgetTests.cs`、新建 `ServerClockTests.cs`
- 方案 B:`E:/work/xuanming-server-mmo/proto/battle/player_battle.proto`

**步骤**
1. 方案 A(纯客户端,先做):新增静态 `ServerClock`(纯 C#):`Observe(serverMs, localMs)` 维护偏移 EMA(首样本直接采用,后续 α=0.2,丢弃与当前偏移差 >5s 的离群样本),`NowUnixMs() = localNow + offset`;在 GameClient 处理带 `server_time_ms` 的场景同步包(player_movement.proto:70/79 对应 S2C 处理处)调用 Observe。
2. `BattleUiWidgets.NowUnixMs()` 改返回 `ServerClock.NowUnixMs()`(计时环、挑战弹窗、观战列表、PlaybackBudget 一起受益),无样本时退回本地 UTC。
3. 方案 B(协议增量,可后做):`BattleStateS2C` 加 `uint64 server_now_ms`,battle 节点 BuildStateSnapshot 填 now;客户端在 BattleStart/TurnResult 到达时 Observe——需 regen + C++ 串行重编 + 节点 WMI 重拉。
4. EditMode:PlaybackBudgetTests 加「ServerClock 偏移 +1500ms/−1500ms 下 Decide 结果与无偏差一致」;ServerClockTests:首样本、EMA 收敛、离群丢弃。

**验收标准**
- 本机系统时间调快 3s 后跑实机 1v1 自动战斗,战斗记录面板不再出现「行动窗口剩 …s → 6.0x」/「跳过本回合演出」;调慢 3s 后每回合仍在窗口内播完(日志无观战抢占式 Abort)。
- EditMode PlaybackBudgetTests + ServerClockTests 全绿。

**验证方式**
```powershell
# Unity -runTests EditMode(副本工程,不带 -quit)
pwsh -File E:/work/mmorpg-client/tools/run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe -ShotDir E:/work/tmp/shots_clock; Select-String -Path E:/work/tmp/crosszone_pair_live/*.log -Pattern '行动窗口'
# 方案 B:. E:\work\tools\buildenv.ps1 → proto regen(memory xuanming-build-toolchain)→ MSBuild /m:1 → 节点 WMI 重拉
```

**风险与注意**
方案 A 依赖场景同步包频率,进入战斗前必已在场景内,样本足够;scene 与 battle 节点不同机时偏移估计仍有残差,方案 B 更准。

---

### P1-D1 `go/db` 镜像构建依赖宿主仓库外 proto2mysql(`go.mod replace ../../../proto2mysql`):脚本已 fail-fast,长期解法待拍板(决策 D-01)

**领域** k8s-deploy · **工作量** M · **状态** needs-decision

**背景与证据**
`go/db/go.mod:10` `github.com/luyuancpp/proto2mysql v0.1.0`、`:132` `replace github.com/luyuancpp/proto2mysql => ../../../proto2mysql`(d2d29dd77 2026-08-17 引入);`deploy/k8s/Dockerfile.go-svc:21-31` `FROM scratch AS proto2mysql` 占位 + `:75` `COPY --from=proto2mysql / /proto2mysql/`;`tools/scripts/go_svc_image.ps1:110` `ExternalReplaceStages=@("proto2mysql")`、`:133` 目录不存在即 throw 并提示「请先把该仓库检出到这个路径」(「fail-fast 明确报错」这一选项**已做**)。宿主 `E:/work/proto2mysql`:HEAD 2aca007 == tag v0.1.0,但 `git status` 显示 `proto2mysql.go` 有未提交修改;remote 仍 `github.com/luyuancpp/proto2mysql.git`,`docs/design/global-data-layer-tidb-decision.md` §6 第 2 步(L170)写明仓库已迁 luyuan-cpp、module 路径靠重定向。`.github/workflows/go-modules-ci.yml:95-99` 用 find go.mod 自动把 go/db 收进矩阵、`:73` 排除清单为空 → runner 上 go/db 因 replace 目录不存在必失败(需 Actions 确认)。B 档用 BuildKit `--build-context proto2mysql=<宿主目录>` 绕过。

**要改的文件**
- `go/db/go.mod`、`go/db/go.sum`、`deploy/k8s/Dockerfile.go-svc`、`tools/scripts/go_svc_image.ps1`
- `.github/workflows/go-modules-ci.yml`、`deploy/k8s/README.md`(:306-307、:315)、`docs/design/global-data-layer-tidb-decision.md`
- `E:/work/proto2mysql/proto2mysql.go`、`E:/work/proto2mysql/go.mod`

**步骤**
1. 先确认 CI 现状:`gh run list --workflow go-modules-ci.yml -L 5` 看 go/db job 是否红。
2. 拍板 A/B/C(见 §2 D-01)。A 的前置:`git -C E:/work/proto2mysql diff` 评审未提交 diff → 需要则 commit → tag v0.1.1 → push;不需要则 `git checkout -- proto2mysql.go`(先与 TiDB 迁移线确认,memory `proto2mysql-tidb-migration-state`)。
3. 选 A:`go.mod` 删 `:132`;`cd go/db && go get github.com/luyuancpp/proto2mysql@v0.1.x && go mod tidy`;`go build ./... && go test -count=1 ./...`;Dockerfile.go-svc 删 `:21-31` 占位 stage 与 `:72-75` COPY;go_svc_image.ps1 删 Get-ExternalBuildContexts 调用(:169-170)与 `:100-136` 函数或改成「发现仓库外 replace 直接 throw」防回归;README 更新。建议同时 rename module 为 `github.com/luyuan-cpp/proto2mysql` 并同步 go/db import。
4. 选 B:`cd go/db && go mod vendor`;Dockerfile.go-svc:83-89 加 `-mod=vendor`,go mod download 对 db 跳过;.gitattributes/.gitignore 确认 vendor 入库;go-modules-ci 对 go/db 加 `-mod=vendor`。
5. 选 C:go-modules-ci.yml 矩阵 job 加第二仓库 checkout(path 不能越出 workspace,需 workspace 内路径 + sed replace 或 GOFLAGS=-modfile);README 写明。
6. 无论哪个:`go_svc_image.ps1 -DryRun` 输出里确认 db 不再需要 `--build-context proto2mysql`(A/B),或 CI 与 README 都写清前置检出(C)。

**验收标准**
- 临时改名 `E:/work/proto2mysql` 模拟无检出环境,`go_svc_image.ps1 -Command build-all -Registry local -Services db` 成功产出 local/mmorpg-db:<tag>。
- `cd go/db && go build ./... && go test -count=1 ./...` 通过;go-modules-ci 的 go/db job 绿。
- kind 上 kind load 新 db 镜像后 db Pod 1/1 Ready,etcd 出现 db.rpc 键。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\db; go build ./... ; go test -count=1 ./...
Rename-Item E:\work\proto2mysql E:\work\proto2mysql.bak; pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Services db -DryRun; Rename-Item E:\work\proto2mysql.bak E:\work\proto2mysql
gh run list --workflow go-modules-ci.yml -L 5
git -C E:/work/proto2mysql status --short; git -C E:/work/proto2mysql describe --tags
```

**风险与注意**
宿主 proto2mysql 未提交 diff 可能是 TiDB 迁移线正在用的改动,丢弃前要确认;module 路径改名牵动 go/db 全部 import 与 go.sum;vendor 方案让仓库体积增大且与 TiDB 线迭代节奏冲突。注意 `cmd/migrate` 对旧 `PbMysqlDB` 类型的引用目前让 `go build ./...` 在 go/db 失败(P2-15),本条第 3 步的 `go build ./...` 会先撞上它。

---

### P1-D2 TiDB 迁移 Phase 1 真正未做的部分:k8s prod TiDB 清单 + 权限策略、Dumpling/Lightning、§5 四项验收、BR/PITR runbook(D8 硬前置)——是否/何时推进需先拍板(决策 D-02)

**领域** docs-process / k8s-deploy · **工作量** L · **状态** needs-decision

**背景与证据**
`docs/design/global-data-layer-tidb-decision.md:172`「k8s prod 清单与权限策略未做」——`grep -rni tidb deploy/k8s/manifests deploy/k8s/README.md tools/scripts/k8s_deploy.ps1` 0 命中;`:173` 迁移工具——全仓 `grep -rli dumpling|lightning` 只命中决策文档、PROGRESS.md 与两份美术文档(误中),无脚本;`:174` §5 验收(:160-163 四项)无执行记录,PROGRESS.md:3002/:3242 两次重申;`:175/:186` runbook——`grep -ni tidb docs/ops/mysql-backup-pitr-runbook.md` 0 命中;D8(:133-135)「本决策落地前不许迁生产」。K8s A/B 档(PROGRESS.md:3717)与 09-02~09-05 全部工作都在 MySQL 上,TiDB 仅有 `deploy/docker-compose.tidb.yml` dev 单节点。代码侧(方言选项、NewDB、表名锁定)已落且 08-17 验证(P2-15)。

**要改的文件**
- `docs/design/global-data-layer-tidb-decision.md`(§5 :158-163、§6 :165-177、§7 :186、D4 :87-93、D8 :133-135)
- 选 B:`deploy/k8s/manifests/infra/`(新增 tidb-pd/tikv/tidb 或 TiDB Operator CR)、`deploy/tidb-config/tidb.toml、tikv.toml`、`tools/scripts/k8s_deploy.ps1`、新建 `tools/scripts/tidb_migrate.ps1`、新建 `docs/ops/tidb-br-pitr-runbook.md`、`docs/design/zone_data_rollback.md`、`docs/design/data-consistency-stress-testing.md`

**步骤**
0. 拍板 A/B,把结论写进决策文档 §6 首段与 PROGRESS.md。以下为 B。
1. infra 新增 TiDB 清单:pd(×1 dev / ×3 prod)、tikv(×3,PVC)、tidb(Deployment ×2,Service 4000/10080);ConfigMap 挂 `tidb.toml`(`txn-entry-size-limit=33554432`)与 `tikv.toml`(`raft-entry-max-size=32MB`),镜像 pingcap/{pd,tikv,tidb}:v8.5.x;`k8s_deploy.ps1 infra-up` 加 `-Database mysql|tidb` 开关,tidb 档把 db/data-service/login 的 DSN host 指到 `tidb.mmorpg-infra:4000`。
2. 权限策略:TiDB init Job `CREATE USER app`、按 zone 逻辑库 `GRANT ALL ON zone_%_db.*`,禁止应用账号 `CREATE DATABASE`;`go/db/internal/logic/pkg/proto_sql/db.go` 的建库逻辑改为可配置关闭。
3. `tools/scripts/tidb_migrate.ps1`:`tiup dumpling -h <mysql> -B zone_N_db -o <dir> --filetype sql` → `tiup tidb-lightning -config lightning.toml`,逐库执行,末尾跑 §5 第 4 项 diff。
4. §5 验收四项逐项执行并写入决策文档「验收记录」表:① proto2mysql 测试套指向 TiDB DSN;② L1-L4 + chaos_test;③ `stress_summarize.ps1` 对比 `prev-summary.txt`;④ `GetCreateTableSQL` dump vs `SHOW CREATE TABLE` 全表 diff 为空。
5. 新写 `docs/ops/tidb-br-pitr-runbook.md`(BR 全量 + log backup + PiTR、按库过滤恢复单 zone),`zone_data_rollback.md` 七步改 BR 版;§7 :186 打勾。
6. 处理 go/db `cmd/migrate` 阻断(P2-15 第 3 步),使 `go test ./...` 全绿。

**验收标准**
- 决策文档 §6 首段写出 A 或 B、日期与决策人。
- (B)kind `kubectl get pods -n mmorpg-infra` 含 pd/tikv/tidb Ready;`SHOW CONFIG WHERE name='performance.txn-entry-size-limit'` 为 33554432;§5 四项各有可复现命令与结果;`SHOW CREATE TABLE` diff 为空;runbook 存在且被 D8 引用。
- (A)§5/§6 标注 deferred,go/db `go test ./...` 通过(migrate 阻断已清)。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; docker compose -f E:/work/xuanming-server-mmo/deploy/docker-compose.tidb.yml up -d; mysql -h 127.0.0.1 -P 4000 -u root -e "SELECT VERSION()"
cd E:/work/proto2mysql; $env:PROTO2MYSQL_DSN='root@tcp(127.0.0.1:4000)/'; go test ./...   # DSN 变量名以库 README 为准
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -Database tidb -DryRun   # 选项 B 落地后
kubectl get pods -n mmorpg-infra; kubectl exec -n mmorpg-infra deploy/tidb -- mysql -h127.0.0.1 -P4000 -uroot -e "SHOW DATABASES"
```

**风险与注意**
B 在本机 kind 上同时跑 TiDB 3 节点 + 现有 infra 可能 OOM;TiDB 与 MySQL 并存期间 DSN 指错会让 zone 数据分裂,一次只切一个 zone 并先 dev 档验证;D8 未完成前任何生产迁移都违反决策文档。A 的风险是 16MB 存档与全区全服数据层问题继续悬置(决策文档 §2)。

---

## 4. P2(35 条:27 待办 + 8 待决策)

### P2-01 `k8s_deploy.ps1 $JavaSvcCatalogue` 残留 auth → mmorpg-auth 死条目

**领域** k8s-deploy · **工作量** S · **状态** open

**背景与证据**
`tools/scripts/k8s_deploy.ps1:319-322` `$JavaSvcCatalogue = @{ auth = @{ ConfigMap="java-svc-auth-config"; Manifest="auth.yaml"; HttpPort=5555; GrpcPort=5556; ImageName="mmorpg-auth" }; gateway=... }`;`:1033-1041` Wait-ForZoneReady 已改成 manifest 不存在则 [skip-wait](B 档补丁,done);`:1520-1530` New-JavaSvcConfigMapYaml 仍有 "auth" 分支生成 nacos/sa-token 配置;`:1593-1605` 每次 zone-up 先 apply `java-svc-auth-config` 再 Write-Warning 跳过 manifest。`deploy/k8s/manifests/java-svc/` 只有 gateway.yaml;`java/` 下无 auth 工程(只有 starter 库);`tools/scripts/java_svc_image.ps1:66-71` 刻意不列 auth;`deploy/k8s/README.md:142-148` Java 表只有 gateway。两份目录表口径不一致是漂移源。

**要改的文件**
- `tools/scripts/k8s_deploy.ps1`、`tools/scripts/java_svc_image.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`deploy/k8s/README.md`

**步骤**
1. `k8s_deploy.ps1:320` 删 auth 条目;`:1520-1550` 删 New-JavaSvcConfigMapYaml 的 "auth" switch 分支(保留 default throw);`:1033-1041` skip-wait 特判保留但改通用措辞。
2. `java_svc_image.ps1:66-69` 注释改「目录表与 k8s_deploy.ps1 $JavaSvcCatalogue 一一对应」。
3. 契约测试新增「JavaSvcCatalogue / GoSvcCatalogue 每项都有 manifest 文件」:DryRun 输出不出现 `Java service manifest not found`,且正则解析目录表键逐个 Test-Path `deploy/k8s/manifests/{java-svc,go-svc}/<Manifest>`;负向断言 DryRun 不含 `java-svc-auth-config`。
4. `kubectl -n mmorpg-zone-yesterday delete cm java-svc-auth-config --ignore-not-found`。
5. README B 档第 4 条 bug 描述(:339-340)追加「已删条目」。

**验收标准**
- `grep -n 'auth' tools/scripts/k8s_deploy.ps1` 不再出现 mmorpg-auth / auth.yaml / java-svc-auth-config。
- zone-up DryRun 无 Warning `Java service manifest not found`、无 `java-svc-auth-config` 块;契约测试 ≥28/28。

**验证方式**
```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName contract-test -ZoneId 101 -DryRun -GoSvcRegistry registry.invalid/test -JavaSvcRegistry registry.invalid/test | Select-String 'auth','manifest not found'
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

**风险与注意**
若未来真要上独立 auth 服务需重新加条目 + 工程 + manifest;当前删除无运行时影响。

---

### P2-02 契约测试 `k8s_deploy_contract.tests.ps1` 未覆盖 infra-up 产物

**领域** k8s-deploy · **工作量** M · **状态** open

**背景与证据**
`tools/scripts/tests/k8s_deploy_contract.tests.ps1:30-46` 唯一基线 DryRun 是 `-Command zone-up`,全文 grep `infra-up|MysqlInit|__INFRA|kafka.yaml|etcd.yaml` 零命中;27 条 Test-Case(:53-347)全是 zone-up。被测逻辑:`k8s_deploy.ps1:2008-2054` New-MysqlInitConfigMapYaml(initdb 目录缺失 throw、CRLF→LF、`01_k8s_zone_dbs.sql` 按 Get-InfraZoneIds 生成、键按文件名排序)、`:2056-2107` Apply-Infra(`__INFRA_NAMESPACE__` 与 5 个 `__KAFKA_*__` 替换、`:2093-2097` 残留 `__[A-Z][A-Z0-9_]*__` 即 throw、ConfigMap 先于 mysql.yaml apply)。`tests/lib/deploy_capture.ps1:1-12` 采集靠 `--- BEGIN MANIFEST ---` 标记,对 infra-up 同样可用。历史:A 档 mysql.yaml binlog 目录 bug、kafka 广播短名 bug 都是未被测试拦住的问题(README:44-56、kafka.yaml:44-49)。是 P1-01/P1-02/P1-03 三条改动的回归护栏。

**要改的文件**
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`tests/lib/deploy_capture.ps1`、`tools/scripts/k8s_deploy.ps1`、`deploy/mysql-init/`、`deploy/k8s/zones.json`

**步骤**
1. 新增第二基线:`$InfraArgs = @('-Command','infra-up','-ZoneId','101','-DryRun','-GoSvcRegistry','registry.invalid/test')`,`$infraOut = (Invoke-DeployDryRun -Arguments $InfraArgs).Output`(只跑一次)。
2. Test-Case「mysql-init-sql ConfigMap 键 == deploy/mysql-init/*.sql + 01_k8s_zone_dbs.sql,键序 00_init_zone_dbs.sql < 01_k8s_zone_dbs.sql < gateway_tables.sql < guild_friend_tables.sql」:Select-ManifestByName -Name mysql-init-sql → 正则抓 `^  (\S+\.sql): \|` 顺序。
3. Test-Case「01_k8s_zone_dbs.sql 的 zone 库集合 == zones.json 的 zone_id ∪ {-ZoneId}」:ConvertFrom-Json 解析 zones.json,对每个 id 断言含 `CREATE DATABASE IF NOT EXISTS \`zone_<id>_db\`` 与 GRANT ... 'appuser'@'%'。
4. Test-Case「infra 产物无残留 __XXX__」:`[regex]::Matches($infraOut,'__[A-Z][A-Z0-9_]*__')` 为 0;断言 KAFKA_ADVERTISED_LISTENERS 含 `kafka.mmorpg-infra.svc.cluster.local:9092`。
5. Test-Case「mysql-init-sql 块出现在 mysql Deployment 块之前」:比较 IndexOf。
6. Test-Case「infra 产物 image: 无 latest」:复用 :179-187(kafka/etcd 钉版本前会红,与 P1-02 一起落)。
7. 负向 Test-Case「deploy/mysql-init 缺失时 infra-up fail-closed」:临时 Rename-Item → DryRun → ExitCode≠0 且输出含「找不到」「fail-closed」,finally 还原(并行会话风险,可改为传不存在的 `-ZonesConfigPath` 触发另一条 throw 更安全)。
8. P1-01/P1-02/P1-03 落地时把 Secret/battle/迁移 Job 断言追加到这组。

**验收标准**
- 用例数 ≥32 且全部通过;`.github/workflows/deploy-config-tests.yml` 在 ubuntu-latest 也绿(路径分隔符,deploy_capture.ps1:18-22 有提醒)。
- 故意在 kafka.yaml 加 `__FOO__` 不加 Replace,测试必红并点名 __FOO__;故意 zones.json 加一个 zone,测试报出缺库。

**验证方式**
```powershell
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -ZoneId 101 -DryRun -GoSvcRegistry registry.invalid/test | Select-String 'BEGIN MANIFEST','name: mysql-init-sql','01_k8s_zone_dbs.sql','__'
gh run list --workflow deploy-config-tests.yml -L 3
```

**风险与注意**
infra-up DryRun 是否在 Ensure-KubectlAvailable(:466-467 DryRun 放行)之后再触碰 kubectl 需确认;每加一个 DryRun 子进程约 +3~5s。

---

### P2-03 kind 节点/宿主重启后 kubelet 对已 kind load 的镜像去 Docker Hub 校验(KubeletEnsureSecretPulledImages),Pod 卡 ErrImagePull 数十分钟

**领域** k8s-deploy · **工作量** S · **状态** open

**背景与证据**
`deploy/k8s/README.md:346-351`(k8s 1.33+ 同一镜像 ID 换仓库名必须重新验证拉取权限,换 alpine:3.20 占位才正常)、`:378-380`(宿主重启后 gateway 两副本卡 ErrImagePull 40 分钟,`kubectl delete pod -l app=gateway` 重建)、`:302-303`(kind load 后老 Pod 仍卡 → delete pod)。全仓 grep `kubeadmConfigPatches|imagePullCredentialsVerificationPolicy|KubeletEnsureSecretPulledImages` 只在 README 出现,无 kind 配置文件(README:288 `kind create cluster --name mmorpg` 无 --config);`docs/ops/k8s-docker-desktop-troubleshooting.md:85-86` 不含 kind/feature gate;scratchpad `full_restart.ps1:1` 明确跳过 kind 节点。机器夜间重启是常态。

**要改的文件**
- 新建 `deploy/k8s/kind-config.yaml`;`deploy/k8s/README.md`;`docs/ops/k8s-docker-desktop-troubleshooting.md`
- 新建 `tools/scripts/k8s_kind_recover.ps1`(或 `k8s_deploy.ps1 -Command kind-recover`);scratchpad `full_restart.ps1`(入库后)

**步骤**
1. 新建 `kind-config.yaml`:`kind: Cluster / apiVersion: kind.x-k8s.io/v1alpha4 / name: mmorpg / featureGates: {KubeletEnsureSecretPulledImages: false}`(或 kubeadmConfigPatches 里 KubeletConfiguration `imagePullCredentialsVerificationPolicy: NeverVerify`,二选一,先用 featureGates 最简单)。README:288 改 `kind create cluster --config deploy/k8s/kind-config.yaml`。
2. 重建 kind:`kind delete cluster --name mmorpg && kind create cluster --config deploy/k8s/kind-config.yaml`,按 README A/B 档重新 kind load 全部镜像并 infra-up/zone-up(约 20 分钟;镜像已在宿主 docker)。
3. 兜底脚本 `k8s_kind_recover.ps1`:`kubectl get pods -A -o json` 找 waiting.reason ∈ {ErrImagePull, ImagePullBackOff} 且镜像 registry 为 `local/` 的 Pod → delete pod;打印每个被删 Pod。`full_restart.ps1` 在 Docker 起来后若 `docker ps` 有 mmorpg-control-plane 则调用。
4. 验证:`docker restart mmorpg-control-plane` 模拟节点重启,5 分钟内所有 Pod 回 Running。
5. troubleshooting 第 4 节追加「kind 特有:KubeletEnsureSecretPulledImages」小节;README:302-303、:378-380 改成引用该节。

**验收标准**
- `docker exec mmorpg-control-plane cat /var/lib/kubelet/config.yaml` 显示 feature gate false / NeverVerify。
- `docker restart mmorpg-control-plane` 后 5 分钟内 `kubectl get pods -A | Select-String 'ErrImagePull|ImagePullBackOff'` 为空,无需手工 delete。
- README 与 troubleshooting 口径一致。

**验证方式**
```powershell
kind create cluster --config deploy/k8s/kind-config.yaml; docker exec mmorpg-control-plane cat /var/lib/kubelet/config.yaml | Select-String -Pattern 'featureGates','KubeletEnsureSecretPulledImages','imagePullCredentialsVerificationPolicy' -Context 0,2
docker restart mmorpg-control-plane; Start-Sleep 120; kubectl get pods -A | Select-String 'ErrImagePull|ImagePullBackOff'
pwsh -File tools/scripts/k8s_kind_recover.ps1 -DryRun
```

**风险与注意**
重建 kind 丢 mysql-data-pvc 等所有数据(试验环境可接受,要重走 A/B 档);featureGates 名随 k8s 版本变化;NeverVerify 只用于本地 kind;兜底脚本要限定 `local/` 前缀镜像。

---

### P2-04 go/match 单测从未在 `-race` 下跑过(本机 CGO_ENABLED=0、无 gcc;CI 门禁明确不加 -race)

**领域** server-go · **工作量** S · **状态** open

**背景与证据**
`docs/design/cross-zone-matchmaking.md:261`「35 个全绿(miniredis;`-race` 因 CGO_ENABLED=0 未跑)」;`.github/workflows/go-modules-ci.yml:204-206`「不加 -race:go-zero 自身有已知可容忍的竞态…查竞态请在本地手动」;本机 `gcc --version` command not found,C:\msys64 / C:\mingw64 / TDM-GCC 均不存在,`wsl -l -q` 只有 docker-desktop,Docker 29.7.2 可用;go/match `^func Test` 计数 98(远多于文档写的 35),`go/match/go.mod:3` go 1.24.5,replace proto=>../proto、shared=>../shared(需挂整仓)。matcher(多实例 SETNX 锁 + 60s sweep)、gather(多跳 RPC + 补偿矩阵 goroutine)、spectate、评分消费者 30s 重试 goroutine 无 race 证据。

**要改的文件**
- `go/match/`(整模块)、`.github/workflows/go-modules-ci.yml`(:204-206;新增 job)、`docs/design/cross-zone-matchmaking.md:261`

**步骤**
1. Docker 跑:`docker run --rm -v E:\work\xuanming-server-mmo:/src -w /src/go/match -e CGO_ENABLED=1 -e GOFLAGS=-mod=mod -e GOPROXY=https://goproxy.cn,direct golang:1.24 go test -race -count=1 -timeout 20m ./... 2>&1 | Tee-Object race_match.log`(可 `-v $env:USERPROFILE\go\pkg\mod:/go/pkg/mod` 复用模缓存)。
2. 逐条处理 `WARNING: DATA RACE`:栈在 go/match 自有代码的必须修;全在 go-zero 内部的记录 PROGRESS.md 并用 `t.Skip`/`-run` 排除说明。
3. 顺手对 go/shared(kafkautil、snowflakealloc)与 go/login 跑同一条。
4. CI:go-modules-ci.yml 加 `race-nightly` job(`schedule: cron '0 19 * * *'` + `workflow_dispatch`,`continue-on-error: true`,matrix 只含 match),不改门禁口径。
5. 更新 cross-zone-matchmaking.md:261 与 PROGRESS.md。

**验收标准**
- race_match.log 末尾 `ok match/...` 且无 DATA RACE 落在 go/match 自有文件;go-zero 内部竞态已登记。
- 新 job 手动 workflow_dispatch 一次能跑通(不判红)。

**验证方式**
```powershell
docker run --rm -v E:\work\xuanming-server-mmo:/src -w /src/go/match -e CGO_ENABLED=1 -e GOPROXY=https://goproxy.cn,direct golang:1.24 go test -race -count=1 -timeout 20m ./...
Select-String -Path race_match.log -Pattern 'DATA RACE' | Measure-Object
gh workflow run go-modules-ci.yml; gh run list --workflow go-modules-ci.yml -L 1
```

**风险与注意**
Docker 内挂 Windows 卷 IO 慢;首次拉 golang:1.24 需走 docker.1ms.run 等镜像源;-race 下依赖时间的用例(matcher 500ms 轮询、30s 重试)可能变慢触发假失败。

---

### P2-05 match `PveTeamSizeByConfigId` 一期临时配置未删,PVE_TEAM 凑满人数仍取本地 yaml 而非 `DungeonTable.max_team_size`

**领域** server-go · **工作量** M · **状态** open

**背景与证据**
`go/match/etc/match_service.yaml:75-79`「⚠ 一期临时配置…待接导表数据后删除本段改为查表」与 `PveTeamSizeByConfigId: {"1": 3}`;`go/match/internal/config/config.go:78-82` 字段与 `:150-166 PveTeamSizeFor`;`joinqueuelogic.go:68-80` 调用并按 kMaxBattleTeamSize=5 收口;`k8s_deploy.ps1:1090` 把该值写进 match ConfigMap;go/match 无 TableDir 也不 import shared/generated/table;但 `go/shared/generated/table/dungeon_table.go` 已有 `DungeonTableManagerInstance.Load(configDir,useBinary)`/`FindById`,pb 有 `GetMaxTeamSize()`/`GetTimeLimit()`;`generated/tables/dungeon.json` id=1/2 max_team_size=5、id=3 为 10——yaml 的 3 人与表的 5 人已不一致;login/player_locator/scene_manager 都已 `table.LoadTables(TableDir,…)`(config.go:35 默认 `../../generated/tables`);Dockerfile.go-svc 已把 generated/tables 打进 Go 镜像(README:333-336);`cross-zone-matchmaking.md:303-304` 登记为二期。多实例各取本地 yaml 会让同一队列按不同人数弹人。

**要改的文件**
- `go/match/internal/config/config.go`、`go/match/etc/match_service.yaml`、`go/match/internal/svc/servicecontext.go`、`go/match/internal/logic/joinqueuelogic.go`(68-80)、`rating_match_test.go`(:222)
- `tools/scripts/k8s_deploy.ps1`(:1090 与 match ConfigMap 模板 ~1387-1410)、`docs/design/cross-zone-matchmaking.md`(§10 L303-304)

**步骤**
1. config.go 加 `TableDir string json:",default=../../generated/tables"` 与 `UseBinaryTables bool json:",optional"`;删 PveTeamSizeByConfigId 与 PveTeamSizeFor。
2. servicecontext.go NewServiceContext 开头 `table.DungeonTableManagerInstance.Load(c.TableDir, c.UseBinaryTables)`(只装 Dungeon,失败 log.Fatalf)。
3. logic 新增包级 `var pveTeamSizeFor = func(configId uint32) uint32 { row, ok := table.DungeonTableManagerInstance.FindById(configId); if !ok || row.GetMaxTeamSize()==0 {return 0}; return min(row.GetMaxTeamSize(), kMaxBattleTeamSize) }`;joinqueuelogic.go:68-80 改调它,ErrTeamSizeNotConfigured 文案改「该副本未开放组队(表无此 id)」。
4. rating_match_test.go:222 等改为 `pveTeamSizeFor = func(uint32) uint32 { return 3 }` 注入。
5. match_service.yaml 删 75-79;k8s_deploy.ps1 删 :1090 与模板输出;模板补 `TableDir: ../../generated/tables`(镜像 WORKDIR /app,与 login 一致)。
6. 可选:`RatingDrawRoundCapByConfigId`(config.go:128-131)也改从 `row.GetTimeLimit()` 派生。
7. 文档 §10 该条划掉,PROGRESS.md 记一条。

**验收标准**
- go/match/tools/deploy 无 `PveTeamSizeByConfigId` 字符串。
- 本地 JoinQueue PVE_TEAM battle_config_id=1 凑满人数为 5(dungeon.json)而非 3;id=3 收口 5;不存在 id 回 ErrTeamSizeNotConfigured。
- 缺 dungeon.json 时 match Fatal 且日志指明路径;`go test ./... -count=1` 全绿;kind match Pod Ready。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\match; go build ./... ; go test ./... -count=1
grep -rn PveTeamSizeByConfigId E:/work/xuanming-server-mmo/go E:/work/xuanming-server-mmo/tools/scripts E:/work/xuanming-server-mmo/deploy
# robot battle-smoke 以 PVE_TEAM 起 5 个机器人,match 日志 popGroup size=5
kubectl -n mmorpg-infra get pods -l app=match; kubectl -n mmorpg-infra logs deploy/match | Select-String 'dungeon'
```

**风险与注意**
match 从此硬依赖策划表目录(本地 go_services.ps1 从 go/match 起、默认路径命中;K8s 依赖 tables build-context,已有);id=1 从 3 人变 5 人会改现有冒烟脚本的机器人数量预期。

---

### P2-06 proto/表生成器不登记新 `.pb.cc` 到 proto.vcxproj / table.vcxproj,新增后必须手工改工程并按固定顺序重编

**领域** ops-tooling · **工作量** M · **状态** open

**背景与证据**
PROGRESS.md:3508-3509(手工加 `match_event.pb.cc`)、:3543-3545(手工加 `scene\player_attribute.pb.cc`、`common\component\player_attribute_comp.pb.cc`,table.vcxproj 加 `proto\tip\attribute_error_tip.pb.cc`;「串行顺序 proto → rpc → table → core → battle 库 → scene 库 → 节点 → 测试」)。`cpp/generated/proto/proto.vcxproj` 114 条显式 `<ClCompile Include="…pb.cc" />`,table.vcxproj 78 条,无通配;`grep -rn vcxproj tools/proto_generator --include=*.go` 为空;`tools/scripts/README.md` 与 `tools/proto_generator/README.md` 无「重编顺序」说明。三天内踩 3 次。

**要改的文件**
- `cpp/generated/proto/proto.vcxproj`、`cpp/generated/table/table.vcxproj`
- `tools/proto_generator/protogen/cmd/root.go`、`internal/generator/cpp/gen.go`(方案 B)
- `tools/proto_generator/README.md`、`tools/scripts/README.md`、`tools/scripts/dev_tools.ps1`

**步骤**
1. 方案 A(推荐):两份 vcxproj 的 `<ItemGroup>` 改通配 `<ClCompile Include="**\*.pb.cc" />`、`<ClInclude Include="**\*.pb.h" />`,删显式列表(.filters 与编译无关可保留)。注意改 vcxproj 含反斜杠要用 PowerShell 的 `[IO.File]::ReadAllText().Replace()`,Git Bash 会折叠 `\\`(memory `xuanming-build-toolchain`)。
2. 方案 B:protogen 增加 `--sync-vcxproj`,扫描输出目录 *.pb.cc 与 vcxproj 现有 ClCompile 求差,encoding/xml 就地插入/删除,在 cmd/root.go 末尾调用。
3. `tools/scripts/README.md` 新增「新增 proto/表后的重编顺序」,原文抄 PROGRESS.md:3545,给出 `msbuild cpp/generated/proto/proto.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64` 逐库模板;说明 rpc/table/core 必须重编的原因(kMaxRpcMethodCount / 表注册陈旧)。
4. 可选:把顺序做成 `tools/scripts/dev/build_cpp_serial.ps1`(scratchpad 已有底稿,见 P1-09)。

**验收标准**
- 新增一个空 proto(如 `proto/logic/event/_probe_event.proto`)跑生成器后不改任何 vcxproj,`msbuild proto.vcxproj /m:1` 能编出对应 .obj;删除并重生成后工程仍可编。
- README 出现「重编顺序」章节;run_cpp_tests.ps1 21/21 仍全绿。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; . E:\work\tools\buildenv.ps1; cd tools/proto_generator/protogen; go run . # 按 README 参数
msbuild E:/work/xuanming-server-mmo/cpp/generated/proto/proto.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /v:m
msbuild E:/work/xuanming-server-mmo/cpp/generated/table/table.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /v:m
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build
```

**风险与注意**
通配会把遗留的 .pb.cc(已删 proto 的旧产物)也编进去,需生成器先清目录或 .gitignore 保证干净;方案 B 的 XML 改写要保留 BOM/CRLF。

---

### P2-07 `reset_smoke_state.ps1` 默认 PlayerIds 写死旧 robot 账号 id,db/login 重建后失效;应按账号名经 Redis `player_to_account` 反查

**领域** ops-tooling · **工作量** S · **状态** open

**背景与证据**
scratchpad `reset_smoke_state.ps1:3` `param([uint64[]]$PlayerIds = @(280559246431486464, 280552072804302848))`,`:7/:11` 按 `^match:rating:(\d+)$` 只删这两个 id;memory `xuanming-phase2-battle-presentation`「db/login 重拉后 robot_9003/9004 的 player_id 变了,-PlayerIds 要传新值」;`robot/etc/battle_smoke_cross_zone.yaml:12-13` 固定账号 robot_9003/9004。反查通道:`go/login/internal/constants/login_constants.go:14-16` `account:<name>` 存 UserAccounts,`:20-22` `player_to_account:<player_id>` → account 反向映射(createplayerlogic.go 写入);MySQL user_accounts.simple_players 是 MEDIUMBLOB 不便 SQL 取 id。三天内 db/login 重建 2 次,忘了就清不掉旧评分、MMR 冒烟结论失真。

**要改的文件**
- scratchpad `reset_smoke_state.ps1` → 迁入 `tools/scripts/dev/reset_smoke_state.ps1`、`run_smoke.ps1`(随 P1-09)
- `go/login/internal/constants/login_constants.go`(只读)、`robot/etc/battle_smoke_cross_zone.yaml`(只读)

**步骤**
1. 新增 `-Accounts @('robot_9003','robot_9004','robot_9101','robot_9102')` 默认参数;`-PlayerIds` 保留为显式覆盖。
2. 新增 `Resolve-PlayerIdsByAccount`:`docker exec redis redis-cli --scan --pattern 'player_to_account:*'` 逐键 GET,值 ∈ $Accounts 则取键尾数字(键格式 `player_to_account:%d`);容器名与现脚本 :10 一致为 `redis`。
3. 反查为空时打 Warning 并要求显式 `-PlayerIds`(或回退删全部 robot 账号主的 match:rating:*,二选一写清)。
4. `run_smoke.ps1` 开头加 `& $PSScriptRoot/reset_smoke_state.ps1` 前置(可 `-SkipReset`)。

**验收标准**
- 不传参运行,输出 `ratings reset for: <id1>,<id2>` 中 id 与 `redis-cli GET account:robot_9003` 解码的 player_id 一致(或与冒烟日志 `ATTRIBUTE_SMOKE_OK player_id=` 一致)。
- 重跑 cross_zone 冒烟后 `redis-cli -p 7000..7005 KEYS 'match:rating:*'` 只含这两个新 id 且值为初始评分。

**验证方式**
```powershell
docker exec redis redis-cli --scan --pattern 'player_to_account:*' | ForEach-Object { "$_ => $(docker exec redis redis-cli GET $_)" } | Select-String robot_900
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/dev/reset_smoke_state.ps1
foreach ($p in 7000..7005) { docker exec redis-cluster-0 redis-cli -p $p KEYS 'match:rating:*' }
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke_cross_zone.yaml 2>&1 | Select-String 'CROSS_ZONE|player_id'
```

**风险与注意**
`player_to_account:*` 是 login 写的缓存,若受 Account.CacheExpire 过期影响,长时间未登录账号查不到——先登录一次(robot 冒烟本身会登录)再重置。

---

### P2-08 `go_services.ps1 stop` 不看 -Zone(无 -Services 时全杀)、`-Services match` 连坐 z2_match;分层就绪等待已改按实例等 LISTEN(这半已做)

**领域** ops-tooling · **工作量** S · **状态** partially-done

**已完成部分**:分层就绪等待 `:657-666` 已对本 tier 每个 launched 实例逐个 `Wait-TcpListenReady -Port $svc.Port`,端口来自 Resolve-InstanceConfig(含 Resolve-BindablePort,:265),PROGRESS.md:3408 所述旧逻辑已不在;但仍是软等待(超时只 warn,:384-385)。

**背景与证据**(剩余部分):`tools/scripts/go_services.ps1:686-712` Invoke-Stop——无 -Services 时 `$instanceKeys = @($pids.PSObject.Properties.Name)`(:704)取全部实例键,不按 `:203-204` 的 `z<Zone>_` 前缀过滤;有 -Services 时按 `$entry.Service`(:560-564 写入的是去前缀基础名 match)匹配,`match` 与 `z2_match` 同时命中。PROGRESS.md:3475、:3535。`tools/scripts/tests` 无 go_services 用例。双 zone 联调想只重启 zone2 或 z1 的 match,当前只能按端口找 PID 手杀(scratchpad `restart_both_match.ps1` 即如此)。

**要改的文件**
- `tools/scripts/go_services.ps1`、新建 `tools/scripts/tests/go_services_stop.tests.ps1`(用 `tests/lib/test_harness.ps1`)

**步骤**
1. Invoke-Stop 开头 `$prefix = Get-ZonePrefix`(把 :203-204 逻辑抽成函数);无 -Services 分支(:704)改 `| Where-Object { $Zone -le 0 -or $_ -like "${prefix}*" }`;$Zone -le 0 时保持全杀但 Warning「未指定 -Zone,将停止所有 zone」。
2. 有 -Services 分支(:693-702)加 `-and ($Zone -le 0 -or $prop.Name -like "${prefix}*")`;zone 0 的键要求 `$prop.Name -notmatch '^z\d+_'`。
3. Invoke-Status 同样按前缀过滤。
4. 就绪等待加 `-StrictReady` 开关,超时抛错;默认不变。
5. 抽纯函数 `Select-StopInstanceKeys -Pids -Zone -Services`,用例 4 组:{无 Zone 无 Services→全部}、{Zone 2→只 z2_*}、{Services match→只 match* 不含 z2_}、{Zone 2 + Services match→只 z2_match*}。

**验收标准**
- 4 用例全过;真实双 zone 栈 `stop -Zone 2` 后 status 显示 zone1 仍 running、z2_* stopped;`stop -Services match`(无 -Zone)后 z2_match 仍在。
- README go_services 小节补 -Zone/-Services 组合语义。

**验证方式**
```powershell
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/tests/go_services_stop.tests.ps1
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/go_services.ps1 -Command status
pwsh -File E:/work/xuanming-server-mmo/tools/scripts/go_services.ps1 -Command stop -Zone 2; pwsh -File E:/work/xuanming-server-mmo/tools/scripts/go_services.ps1 -Command status
Get-NetTCPConnection -State Listen | Where-Object LocalPort -in 50500,52500 | Select LocalPort,OwningProcess
```

**风险与注意**
PID 文件可能有旧格式整数值(Read-InstanceEntry :371 兼容分支),前缀过滤要对两种格式都测;依赖「stop 全杀」的 `full_restart.ps1` 需显式传 -Zone 0 或逐 zone 调用。

---

### P2-09 战斗掉落/道具消耗落地:items_gained/items_consumed 只记日志,`BagService::AddItem` 零生产调用点

**领域** server-cpp · **工作量** M · **状态** open

**背景与证据**
`cpp/libs/services/scene/battle/system/player_battle.cpp:999-1007` `道具结算暂缓(背包系统未挂载)` 仅 LOG_INFO;`:442-443` 快照道具副本「一期传空」;`turn_battle_engine.cpp:1280` `掉落 items_gained 依赖掉落表,留待后续接入`;grep `BagService::AddItem|.AddItem(` 在 cpp/libs/services + cpp/nodes(排除测试)零命中;Monster/Dungeon 表无掉落列(`data/schema/monster_table.proto` 仅属性+exp/gold)。背包域(bag_test 46+ 用例、准入/淘汰策略)已就绪但没有真实入包链路;`bag-rule-policy-layering.md` §6.1 明确「推进节日包/FIFO 前先接一条真实入包链路」。「未挂载玩家实体」的说法需核实——若 bag_marshal 已在 loader 里 emplace,则只差调用。

**要改的文件**
- `cpp/libs/services/scene/battle/system/player_battle.cpp`、`cpp/libs/modules/bag/bag_service.h`、`cpp/libs/services/scene/player/system/bag_marshal.h`
- `cpp/libs/services/battle/system/turn_battle_engine.cpp`、`data/Monster.xlsx`、`data/schema/monster_table.proto`、`docs/design/bag-rule-policy-layering.md`

**步骤**
1. 核实挂载现状:grep `PlayerBagsComp` in player_database_loader.cpp / bag_marshal.h;若已挂载,更新 player_battle.cpp:442/999 注释。
2. 掉落表:Monster.xlsx 加 `drop`(repeated fk:Item 或 drop_group)列 + monster_table.proto 字段 10,导表重生成;BuildSettlement(:1276-1300)胜利分支按被击杀怪物行填 items_gained。
3. scene 结算:`player_battle.cpp:1001` 改 `BagService::AddItems(bags.Get(kMain), itemCountMap, &evicted)`(走 ReserveForBatchAdd 唯一入口),满包按 kBagAddItemBagFull 走邮件/日志兜底(一期日志 + 不丢结算);items_consumed 按实际持有扣除。
4. 快照:`:442` 从主背包过滤「可战斗消耗品」生成 BattlePlayerSnapshot.items(需 Item 表有战斗可用标记,没有先跳过)。
5. 测试:新增 scene 侧用例(结算入包 / 满包不崩 / 消耗不足按 0);robot battle_smoke 结算后 GetBag 断言。

**验收标准**
- PVE 胜利后主背包出现 Monster.drop 配置道具,数量正确;败/逃不发。
- 满包时结算不失败、金币/经验照常入账,日志有 kBagAddItemBagFull。
- bag_test / cross_zone_test / turn_battle_engine_test 全绿;battle_smoke OK 且观战流不受影响。

**验证方式**
```powershell
# 导表同 P1-11;cd E:/work/xuanming-server-mmo/go; .\build.bat
msbuild E:/work/xuanming-server-mmo/game.sln /t:modules;scene;battle /p:Configuration=Debug /p:Platform=x64 /m:1   # 顺序 modules → scene → bag_test → cross_zone_test
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build -Filter 'bag_test|cross_zone_test|turn_battle_engine_test'
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml
```

**风险与注意**
依赖掉落表设计(策划);Bag 步长变动历史上引发 entt storage 断言(split 文档 §9.1),必须 modules → scene 串行重编;跨 zone 迁移带背包的还原路径要一起回归(cross_zone_test)。

---

### P2-10 `Node::StartConflictDrainWatchdog` 15s 收尾预算待压测实测复核

**领域** server-cpp · **工作量** M · **状态** open

**背景与证据**
`cpp/libs/engine/core/node/system/node/node.cpp:771-778` `constexpr auto kDrainBudget = std::chrono::seconds(15)` 上方 `// TODO: 待压测实测复核该预算。`;到期 LOG_ERROR「Conflict drain budget exceeded; shutting down with work still in flight」(:797);`docs/ops/stress-3zone-runbook.md`(45k robot)未包含「节点 ID 冲突关机」场景。低估丢存盘,高估拖慢故障切换。

**要改的文件**
- `cpp/libs/engine/core/node/system/node/node.cpp`、`docs/ops/stress-3zone-runbook.md`、`tools/scripts/stress_round19.ps1`、`stress_snap.ps1`

**步骤**
1. runbook 加场景「在线稳态 15k 时人为触发 Node ID 冲突」:起第二个同 NODE_ID 的 scene(etcd CAS 抢占)让在线节点走 onConflictShutdownFn_ → StartConflictDrainWatchdog。
2. `:797` ERROR 与 `:793` INFO 补打耗时与未落地数量(conflictDrainCompleteFn_ 返回 false 时统计尚未 ack 的玩家数)。
3. 1k/5k/15k 在线各跑 3 次,取 P99 drain 时间;<5s 可降到 10s 或改 `base + perPlayerMs × onlineCount` 加上限;>15s 上调。
4. 结论写进 node.cpp 注释(删 TODO)与 runbook。

**验收标准**
- 压测下 3 次冲突关机日志均「Conflict drain complete」或有数据支撑的新预算;被驱逐节点所有在线玩家 PlayerAllData 为关机时刻最新(抽样比对存盘时间戳);TODO 删除。

**验证方式**
```powershell
powershell -File E:/work/xuanming-server-mmo/tools/scripts/stress_round19.ps1   # 按 stress-3zone-runbook 起 15k robot
$env:NODE_ID=<在线节点 id>; # 经 WMI 起第二个 scene:tools/scripts/cpp_nodes.ps1 start scene
Select-String E:/work/xuanming-server-mmo/run/logs/cpp_nodes/z1_scene*.stdout.log -Pattern 'Conflict drain'
```

**风险与注意**
需要压测环境(45k robot 单机很重);冲突触发会真的踢下线一批 robot,别在别人联调时跑。

---

### P2-11 战斗表现层无 PlayMode 冒烟(倍率连续性 / 子 Canvas 点击 / Abort 复位),三项只有 EditMode 常量断言

**领域** client-infra / client-presentation · **工作量** M · **状态** open

**背景与证据**
`E:/work/mmorpg-client/Assets/Tests/PlayMode/` 仅 QdaoBootstrap / QdaoBoySpriteAnimator / TianyongMap 三个用例,无 Battle;`Assets/Tests/EditMode/Battle/` 14 个文件全是纯 C#。`turn-battle-presentation.md` §6 L101-102 验证只列「EditMode TurnPlan 用例」与「播放器截图」。可用钩子:`BattlePresenter.cs:48-53` public `Fx / Numbers / Ghosts / Camera / IsPlaying`,`L118 Abort()` → ClearTransient(:118-147);`BattleScreen.cs:110 Presenter`;`BattleClient.Attach(IBattleTransport)`(BattleClient.cs:125);`BattleUiRoot.EnsureSpawned`(BattleUiRoot.cs:87);合成驱动样板 `PresentationShowcase.cs:221-245`(ShowcaseTransport + Push NotifyBattleStart);`Assets/Tests/EditMode/Battle/FakeBattleTransport.cs`(internal,EditMode 程序集);`BattleFx.cs:32-33/192-193` 池 ActiveCount/PooledCount。PlayMode asmdef 已引用 MmorpgClient/UnityEngine.UI/TMP。Sequencer(GTween)、残影池、GraphicRaycaster 只能在 PlayMode 验,当前靠人工看帧。

**要改的文件**
- `Assets/Tests/PlayMode/MmorpgClient.Tests.PlayMode.asmdef`、新建 `Assets/Tests/PlayMode/Battle/BattlePresenterPlayModeTests.cs`
- `Assets/Tests/EditMode/Battle/FakeBattleTransport.cs`(抽到共用或复制)、`Assets/Scripts/App/PresentationShowcase.cs`(抽 ShowcaseFixture)
- `Assets/Scripts/UI/Ugui/Battle/BattlePresenter.cs`、`BattleScreen.cs`、`BattleUiRoot.cs`、`BattleUnitView.cs`(internal 访问器 + `InternalsVisibleTo("MmorpgClient.Tests.PlayMode")`)
- `docs/design/turn-battle-presentation.md` §6

**步骤**
1. 把 FakeBattleTransport 抽到 `Assets/Tests/Shared/MmorpgClient.Tests.Shared.asmdef`(或在 PlayMode 目录复制 `PlayModeBattleTransport.cs`);把 PresentationShowcase 的 ShowcaseTransport 与 BuildActors/BuildRound1..5 抽成 `ShowcaseFixture`(internal static)。
2. 新建 `BattlePresenterPlayModeTests.cs`([UnityTest] 协程),SetUp 照 PresentationShowcase.cs:221-245:`BattleClient.Attach(fixture.Transport)` → 销毁旧 `BattleUiRoot.Instance` → `EnsureSpawned()` → yield 两帧 → 断言 `Instance.Client == client`;构造 2v2 的 BattleStartS2C 与 TurnResultS2C。
3. 用例 A「倍率连续性」:push 一回合含同一 actor 连续两拍,`action_deadline_ms` 设小让 PlaybackBudget 取高倍率(或设 presenter.SpeedScale=1.5);每帧记录该 actor `BattleUnitView.Root.anchoredPosition`,断言相邻帧位移 < 60px(冲刺 0.22s/1.5 内最大步进)且回合结束回到 BattleStage 站位。
4. 用例 B「子 Canvas 点击」:对嵌套 Canvas(overrideSorting)结构用 `EventSystem` + `GraphicRaycaster.Raycast(PointerEventData{position=单位屏幕坐标})` 断言首个命中在该 view.Root 子树内;再 `ExecuteEvents.Execute(…, pointerClickHandler)` 断言 BattleScreen 的 `_selectedTargetId`。
5. 用例 C「Abort 复位」:播放中(`Presenter.IsPlaying==true`)调 `screen.AbortPlayback()`,yield 一帧后断言 `IsPlaying==false`、`Ghosts.ActiveCount==0`、`Numbers.ActiveCount==0`、Fx 无活跃、各 BattleUnitView 的 localScale/颜色/位置等于 ResetVisual 默认值。
6. §6 补一行「PlayMode:BattlePresenterPlayModeTests(倍率连续性 / 子 Canvas 点击 / Abort 复位)」,PROGRESS.md 记录用例数。

**验收标准**
- 至少 3 个 [UnityTest] 在副本工程 batchmode `-testPlatform PlayMode` 全绿;把 `BattlePresenter.Abort` 里 `Ghosts.Clear()` 注释掉,用例 C 必红。
- 不依赖网络/服务端;一轮 < 2 分钟(Library 已 seed);EditMode 165/166 不回归。

**验证方式**
```powershell
robocopy E:\work\mmorpg-client E:\work\tmp\shotverify_project /E /PURGE /XD Library Temp Logs obj .git UserSettings .vs   # 首次从 E:/work/tmp/presentation_verify_project/Library 拷 Library
"C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe" -batchmode -projectPath E:\work\tmp\shotverify_project -runTests -testPlatform PlayMode -testResults E:/work/tmp/battle_playmode.xml -logFile E:/work/tmp/battle_playmode.log   # 不要 -quit;PlayMode 可能需去掉 -nographics
[xml]$x=Get-Content E:/work/tmp/battle_playmode.xml; $x.'test-run'.passed; $x.'test-run'.failed; $x.SelectNodes("//test-case[@result='Failed']") | % name
```

**风险与注意**
PlayMode 在 batchmode 下需真正进 Play,BattleUiRoot 依赖 AppBootstrap/QdaoUguiRuntime 场景对象(可能要先 `SceneManager.LoadScene` 或手动 AddComponent<AppBootstrap>);点击命中对 Canvas Scaler 分辨率敏感,-nographics 下 Screen 尺寸可能为 0;首跑导 Library 约 3 分钟;GTween 用 realtime,测试用 WaitForSecondsRealtime。

---

### P2-12 DevAutoPilot 只支持 `-autoQueue 1v1`;5v5 / PVE(solo、team)/ 观战无真机自动驾驶与多实例编排

**领域** client-infra / client-presentation · **工作量** L · **状态** open

**背景与证据**
`E:/work/mmorpg-client/Assets/Scripts/App/DevAutoPilot.cs:51` `AutoQueue // "1v1" / null`;`:345-350` switch 仅 `case "1v1"`,default `Fail("queue", "不支持的 -autoQueue=…(当前只支持 1v1)")`;文件内 grep `Spectate|EnterDungeon` 零命中。`tools/run_crosszone_pair.ps1:41,73` 固定两侧 A/B、`-Mode` 透传 -autoQueue,`:60-63` `$sides` 硬编码两个元素。可用基础设施:`Proto/Generated/MatchService.cs:125-150` MatchMode 有 `_5V5=1 / _1V1=3 / PveSolo=4 / PveTeam=5`;服务端 `joinqueuelogic.go:64-92` 对 PVE_SOLO(required=1)/PVE_TEAM/1V1/5V5(required5v5Players=10)放行;`BattleQueuePanel.cs:182` `client.JoinQueue(Match.MatchMode.PveSolo, BattleUiStyle.PveSoloBattleConfigId)`;`BattleUiStyle.cs:19-34` PveSolo/PveTeam/Pvp1V1 配置 id=1、Pvp5V5=0;`SpectateClient.cs:36 Instance`、`:110 WatchRandom()`、`:117 WatchBattle(id)`;服务端 `robot/battle_smoke_scenario.go:6-13` 覆盖 PVE_SOLO + 观战。`turn-battle-presentation.md` §6 L110-113 与 PROGRESS.md:3793 实机只验 1v1;5v5/群攻/PVE 只在合成台验过。

**要改的文件**
- `Assets/Scripts/App/DevAutoPilot.cs`、`tools/run_crosszone_pair.ps1`(或新建 `tools/run_autopilot_fleet.ps1`)
- `Assets/Scripts/Game/Battle/BattleClient.cs`、`SpectateClient.cs`、`UI/Ugui/Battle/BattleUiStyle.cs`、`BattleQueuePanel.cs`(样板)
- `E:/work/xuanming-server-mmo/robot/battle_smoke_scenario.go`(5v5 补位子模式)、`data/Dungeon.xlsx`(只读)、`docs/design/turn-battle-presentation.md` §6

**步骤**
1. `DevAutoPilot.Options.AutoQueue` 取值文档 `"1v1"|"5v5"|"pve"|"pveteam"|"watch"`;`HandleEnterSuccess` switch 扩:`"5v5"→MatchMode._5V5`(config `Pvp5V5BattleConfigId`)、`"pve"→PveSolo`、`"pveteam"→PveTeam`;`"watch"` 不走 JoinQueue,`_spectate = _client.Spectate ?? SpectateClient.Instance`,订阅 OnPhaseChanged/OnSpectateState/OnSpectateEnd/OnError,`WatchRandom()`(或 `-watchBattleId N`),Stage 加 Watching;日志 `SpectateStart battle_id=N` / `SpectateEnd …`,PASS 条件:首帧到达且 turns≥1 且收到 NotifySpectateEnd(或 `-watchSeconds` 到时主动 StopWatch)。
2. PVE PASS 判定:BattleEnd 且 turns≥1(PVE solo 可能 1 回合结束,turns=1 算 PASS);5v5 沿用 BattleStart/BattleEnd + `actors==10`(DevAutoPilot.cs:387 已打 actors 数)。
3. 编排脚本 `tools/run_autopilot_fleet.ps1 -Mode <1v1|5v5|pve|pveteam|watch> -Instances N -Zones 1,2 -AccountBase robot_91 -ShotDir <dir>`:`$sides` 改循环生成 N 个(账号 robot_9101..robot_91NN,zone 轮转),-ShotDir 按 Tag 分子目录;5v5 需 10 实例(可 `-ScreenWidth 640` 或让 robot 补位:robot 加纯 JoinQueue(5v5) 子模式);watch = 先起 2 个 1v1(或 1 个 pve),再起第 3 个 `-autoQueue watch`,断言其 battle_id 等于前两者。
4. 断言:pve 单实例 BattleEnd;5v5 所有实例 battle_id 相同且 actors=10;watch 观战 battle_id 与参战一致、SpectateEnd 出现;汇总 `AUTOPILOT_FLEET_PASS mode=… battle_id=… instances=N`。
5. 用 -ShotDir 帧复核:5v5 群攻五串数字同拍(对照 showcase r2_c-aoe5)、PVE 怪物剪影/命中帧(P2-18)、观战抢占后 Abort 无残影;结果写进 §6「实机」与 PROGRESS.md。

**验收标准**
- `mmorpg.exe -zone 1 -account robot_9101 -autoQueue pve -autoBattle -quitOnBattleEnd` 退出码 0,日志 `BattleStart … BattleEnd … RESULT=PASS`。
- `-Mode 5v5 -Instances 10` 输出 `AUTOPILOT_FLEET_PASS`,10 份日志 battle_id 相同、actors=10。
- `-Mode watch` 观战实例 `SpectateStart battle_id` 与参战一致,对局结束出现 `SpectateEnd`;终局帧无残影。
- 不带新参数时 1v1 行为与 `CROSS_ZONE_PAIR_PASS` 输出完全不变。

**验证方式**
```powershell
# 服务端前置(WMI 起):双 zone C++ 节点 + Go + Java gateway;. E:\work\tools\buildenv.ps1; cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml  → BATTLE_SMOKE_OK
pwsh -File E:/work/mmorpg-client/tools/build_crosszone_player.ps1   # 副本工程出包
pwsh -File E:/work/mmorpg-client/tools/run_crosszone_pair.ps1 -ShotDir E:/work/tmp/shots_regress   # 回归 1v1
pwsh -File E:/work/mmorpg-client/tools/run_autopilot_fleet.ps1 -Mode pve -Instances 1
pwsh -File E:/work/mmorpg-client/tools/run_autopilot_fleet.ps1 -Mode 5v5 -Instances 10 -ScreenWidth 640 -ScreenHeight 360
pwsh -File E:/work/mmorpg-client/tools/run_autopilot_fleet.ps1 -Mode watch -Instances 3
```

**风险与注意**
10 个 Unity 播放器同机并跑显存/CPU 压力大,可能拖到 Preparing 15s 超时——小窗+关截图或 robot 补位;PVE solo 因数值失配常 1 回合秒杀(P2-D2),可 `-battleConfig 2/3` 换更强怪;观战首帧可能先于 WatchBattle 响应到达(SpectateClient.cs:27),断言以 OnSpectateState 为准;`Pvp5V5BattleConfigId=0` 与 robot 填的值不同会分到不同队列 key,脚本统一传 -battleConfig。失败时若 stage=queue 且相位残留,等 battle:lock 解冻 5~6 分钟。

---

### P2-13 提交信息规范化:两仓库多次把 PR 正文当标题(「## 背景」「## 变更内容」),补 commit-msg 门禁与书面规范

**领域** docs-process · **工作量** S · **状态** open

**背景与证据**
服务端 `git log --all --format='%h %ad %s'` 以 `#` 开头的标题共 9 个,8 个是「## 背景」:48b99101f(09-05)、410b5283d、6c4021ae5(09-02)、b134efea5(08-30)、d2d29dd77、b44055e76(08-17)、31e4d1d4c(07-30)、5b796117a(07-29);410b5283d 与 6c4021ae5 正文完全相同且「## 验证」四项全部未勾。客户端 99d891f「## 变更内容」(「双 zone 跨区匹配集成验证」「多分辨率战斗 UI 视觉验收」未勾)、0c4e999「## 背景」。防线现状:服务端 `.git/hooks` 只有 *.sample、`core.hooksPath` 为空、无 `.githooks/`;AGENTS.md 只有 L32「commit message 全中文」与 L149/L169「仅在用户明确要求时 commit」;`.github/workflows/*.yml` 无 commitlint;`release_preflight.ps1` 不看 git 历史;客户端仓库无 CLAUDE.md / AGENTS.md,`.github/workflows/ci.yml` 仅 scripts-lint。已推 main,重写历史会打断并行会话,不建议 rebase。

**要改的文件**
- `E:/work/xuanming-server-mmo/AGENTS.md`(CLAUDE.md 只有 `@AGENTS.md` 一行)、`docs/ops/pr-templates.md`、新建 `.githooks/commit-msg`、`tools/scripts/dev_tools.ps1`(可选 `install-hooks`)
- `E:/work/mmorpg-client/README.md` 或新建 `AGENTS.md`、新建 `.githooks/commit-msg`

**步骤**
1. 服务端 AGENTS.md L32 附近新增「提交信息格式」:首行 `type(scope): 摘要`(≤72 字符,type ∈ feat/fix/docs/build/test/refactor/chore/perf/ci/style/revert),首行禁止以 `#` 开头、禁止空首行;空行后再放 `## 背景 / ## 主要改动 / ## 验证`;工作流脚本生成提交必须 `git commit -F <file>` 或第二个 `-m`,不得作为第一个 `-m`。
2. 新建 `.githooks/commit-msg`(sh,两仓库同文):取第一非注释行,匹配 `^#` 或不匹配 `^(feat|fix|docs|build|test|refactor|chore|perf|ci|style|revert)(\([^)]+\))?!?: .+` 则中文报错 `exit 1`;放行 `Merge `/`Revert `/`fixup!`/`squash!`。
3. 两仓库 README/AGENTS 写明 `git config core.hooksPath .githooks`;服务端 `dev_tools.ps1 -Command install-hooks` 封装(sh 脚本 LF 换行、`git update-index --chmod=+x`)。
4. `docs/ops/pr-templates.md` 末尾追加「历史提交主题对照」:48b99101f=二期/属性/跨区/K8s B 档收口;410b5283d=6c4021ae5=观战+自动战斗+5v5(重复提交);b134efea5=建角协议扩展+self ActorCreate+Recast 导航;d2d29dd77、b44055e76、31e4d1d4c、5b796117a 各取 `git log -1 --format=%B` 首段;客户端 99d891f=战斗演出/属性加点/跨区验证,0c4e999=客户端回合制战斗一期。99d891f 两项未勾:前者已由 PROGRESS.md:3794-3796 `CROSS_ZONE_PAIR_PASS` 通过,后者见 P2-27。
5. 客户端 README.md 加「提交信息格式」一节并指向同一 hook(或新建 AGENTS.md)。

**验收标准**
- 两仓库 `git config core.hooksPath` 返回 `.githooks`,hook 存在且可执行。
- 临时分支 `git commit --allow-empty -m '## 背景'` 被拒并输出中文提示;`-m 'docs(process): 测试'` 通过。
- AGENTS.md / README.md 含「首行 type(scope): 摘要,禁止以 # 开头」;pr-templates.md 含 10 个 sha 对照表。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; git config core.hooksPath; git checkout -b tmp/hook-test; git commit --allow-empty -m '## 背景'; echo exit=$LASTEXITCODE   # 期望非 0
git commit --allow-empty -m 'docs(process): hook 自测'; echo exit=$LASTEXITCODE   # 期望 0;随后 git checkout main; git branch -D tmp/hook-test
git log --all --format='%s' | Select-String '^#' | Measure-Object   # 基线 9 条,不得再增
cd E:/work/mmorpg-client; # 同上三条
```

**风险与注意**
hook 只约束本地,另一并行会话若未设 hooksPath 仍可绕过——两个窗口都要安装一次;不要对已推送提交 rebase/amend;GitHub Squash Merge 时标题仍需人工看。

---

### P2-14 `deploy/k8s/README.md` 多处过期:目录清单缺 match/gateway/redis-match-cluster/java-svc、Go 镜像表缺 match、centre 已删仍写 centre=1 / -CentreReplicas(脚本里也是死参数)、L314 buildenv 路径乱码

**领域** docs-process · **工作量** M · **状态** open

**背景与证据**
README L17「manifests/infra/: (etcd, redis, kafka, mysql)」但目录实有 etcd/kafka/mysql/mysql-backup-cronjob/redis/redis-match-cluster.yaml;L18 go-svc 列 5 个但实有 db/data-service/login/player-locator/scene-manager/match/gateway/gateway-pdb/login-pdb/scene-manager-agones-rbac.yaml;Directory Layout 没提 `manifests/java-svc/gateway.yaml`、`Dockerfile.java-svc`、`Dockerfile.cpp`、`Dockerfile.robot`。L104-110 Go 镜像表无 match,而 `k8s_deploy.ps1:307` `$GoSvcCatalogue.match` 已存在。L191「centre=1, gate=2, scene=4」、L257「-WaitReady: wait for centre / gate / scene」、L262「-CentreReplicas」:k8s_deploy.ps1 只在 `:1882` 生成 gate(及 scene)Deployment,`grep centre` 只命中 :41 参数声明、:332/:338 钳位、:1657-1751 zones 解析、:1863 打印,从不生成 centre 工作负载——死参数;`zones.sample.yaml:5,16`、`zones.ops-recommended.yaml:5,14,20`、`zones.json:7,16` 仍写 `centre: 1`;memory `xuanming-spof-audit-2026-08` 记录 Centre 已删。L314 `sed -n 314p | cat -A` 输出 `. E:\work^Iools^Huildenv.ps1$`(\t、\b 被吞)。`deploy/k8s/AGENTS.md` 把 README 定为「Authoritative flow description」。

**要改的文件**
- `deploy/k8s/README.md`、`deploy/k8s/AGENTS.md`(Infra manifests 行)
- `tools/scripts/k8s_deploy.ps1`(:41、:332/:338、:1657-1751、:1842-1863、:2155/:2170)
- `deploy/k8s/zones.sample.yaml`、`zones.ops-recommended.yaml`、`zones.json`、`zones.10zones.yaml`
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`

**步骤**
1. README L17 改 `etcd, redis, redis-match-cluster(全局 match 的 Redis Cluster 6 节点), kafka, mysql, mysql-backup-cronjob`;L18 列 10 个 yaml 并注明 match 是全局服务(infra-up 阶段,见 :282-290);新增 `manifests/java-svc/`: gateway.yaml;补三个 Dockerfile 行。同步 AGENTS.md。
2. L104-110 表加 `| match | mmorpg-match |`,注明 `go_svc_image.ps1 -Services match` 单独构建。
3. Centre 清理(推荐 a):删 `$CentreReplicas` 参数、钳位、zones 解析里 `centre` 键(:1677 正则改 `scene_world|scene_instance|gate|scene`,读到旧文件打 WARN「centre 已废弃,忽略」)、Apply-Zone 的 `-CurrentCentreReplicas` 与 :1863 打印;zones.*.yaml/json 删 `centre:` 行;README L191 改「gate=2, scene=4」、L257 改「gate / scene」、L262 删。b)若不动脚本,README 三处加「(已随 Centre 下线成为无效参数)」但仍修 L191。
4. L314 改回 `. E:\work\tools\buildenv.ps1`;README 顶部加「本文件 UTF-8,编辑请用编辑器而非 PowerShell 字符串拼接」。
5. 契约测试加一条:「README Directory Layout 列出的 manifests 文件名集合 == 磁盘 `manifests/**/*.yaml` 集合」。
6. cache mount 相关(L352)见 P3-01。

**验收标准**
- `Select-String deploy/k8s/README.md -Pattern 'centre'` 为 0 条(a)或仅剩「已废弃」说明(b)。
- `sed -n 314p deploy/k8s/README.md | cat -A` 输出 `. E:\work\tools\buildenv.ps1$`。
- README 目录清单与 `Get-ChildItem deploy/k8s/manifests -Recurse -Filter *.yaml` 一一对应;Go 镜像表含 match。
- (a)zone-up DryRun 不再接受 `-CentreReplicas`(报未知参数),zones 文件不含 centre;契约测试 28/28。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; Select-String -Path deploy/k8s/README.md -Pattern 'centre|Centre'
bash -c "sed -n 314p deploy/k8s/README.md | cat -A"
Get-ChildItem deploy/k8s/manifests -Recurse -Filter *.yaml | Select-Object -Expand Name
pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

**风险与注意**
方案 a 改 k8s_deploy.ps1 参数面,tests 里若有用例传 `-CentreReplicas` 需同步删;zones.10zones.yaml 若被 CI 解析测试引用也要改。

---

### P2-15 TiDB 决策文档 §6 第 1/3 步状态落后(写「待 Codex 编译/测试」,实际 08-17 已编译并在真 TiDB 验证),需刷新并补记 go/db `cmd/migrate` 对旧 `PbMysqlDB` 的新阻断

**领域** docs-process · **工作量** S · **状态** partially-done

**已完成部分**:代码侧——PROGRESS.md:3215-3224(2026-08-17)「proto2mysql v0.1.0 经 module proxy 正常拉取;protogen 重建 + proto 全量重生成;TiDB v8.5.2 dev 集群真集群验证:txn-entry-size-limit 生效;SHOW CREATE TABLE 原样呈现 NONCLUSTERED + SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4」;memory `proto2mysql-tidb-migration-state` 记第 1-4 步已验证;`go/db/go.mod:10` require v0.1.0;`E:/work/proto2mysql` HEAD 2aca007。

**背景与证据**(剩余部分):`docs/design/global-data-layer-tidb-decision.md:169` 第 1 步「`go test` 待 Codex」、`:171` 第 3 步「**代码已落,待 Codex 编译**」陈旧。新阻断未登记:`go/db/cmd/migrate/main.go:195,200` 与 `go/db/internal/migrate/plan.go:81,210,255` 仍引用 `proto2mysql.PbMysqlDB`/`NewPbMysqlDB`,新版库已无此类型,提交 48b99101f 正文「已知限制」与 PROGRESS.md:3608 记录 `go/db go test ./...` 因此编不过(主包 `go test .` 可过)。P1-03 的迁移 Job 依赖 cmd/migrate 能编。

**要改的文件**
- `docs/design/global-data-layer-tidb-decision.md`(§6 :167-177,§7 :181-187)
- `go/db/cmd/migrate/main.go`(:195-200)、`go/db/internal/migrate/plan.go`(:81,:210,:255)(本条只登记,不改代码)

**步骤**
1. §6 第 1 步删「`go test` 待 Codex」,改「已完成(2aca007,v0.1.0 已打 tag)」。
2. §6 第 3 步改「**已完成并验证**(2026-08-17,PROGRESS.md「全栈本机构建打通 + TiDB dev 集群验证」…)」,保留原括号实现细节。
3. §6 新增第 3b 步或「明确未做」段追加:「go/db `cmd/migrate` 与 `internal/migrate/plan.go` 仍依赖旧世系 `PbMysqlDB`,v0.1.0 无此类型,`go test ./...` 在 go/db 被阻断;处置二选一:按新版 API 重写 migrate 计划器,或把 migrate 子命令拆出/删除(Phase 1 迁移改走 Dumpling+Lightning 后价值下降)」——与 P1-D2 的 A/B 联动。
4. §6 第 4 步补 dev 集群验证日期(08-17)与「k8s prod 清单与权限策略未做」;第 5/6/7 步各加「前置:见 P1-D2」。
5. §7 :186 保持未勾,补注「阻断迁生产的硬前置(D8)」。

**验收标准**
- 决策文档 §6 不再出现「待 Codex」;含 `cmd/migrate` / `PbMysqlDB` 阻断的登记与处置选项;状态与 PROGRESS.md:3215-3224、go.mod:10 一致。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; Select-String -Path docs/design/global-data-layer-tidb-decision.md -Pattern '待 Codex|PbMysqlDB'
. E:\work\tools\buildenv.ps1; cd go/db; go build ./... 2>&1 | Select-String PbMysqlDB   # 应能复现编译错误,证明登记属实
cd go/db; go test . ; go vet .   # 主包应通过
```

**风险与注意**
若另一会话已在改 migrate 计划器,文档处置选项需对齐;不要在本条里顺手删 cmd/migrate 代码。

---

### P2-16 角色 die / win 动作帧条未生成,客户端回退程序化倒地/跳跃;且当前 die 变换表按 fit 规则会把统一身高压到 0

**领域** client-art · **工作量** M · **状态** open

**背景与证据**
`tools/battle_art_gen/main.go:90` `flag.String("char-actions", "idle,attack,cast,hit", …)`;`characters.go:643-683` 已有 die/win 8 帧变换表(注释「可选动作,默认不生成」);`Characters/01_ice_sword_girl/` 仅 4 动作;CHARACTER_MANIFEST.json actions=[idle,attack,cast,hit];客户端 `BattleUnitView.cs:657-658` LoadCharacterAction(…,"die") 取不到 → rotation 75°+下沉程序化;`:828` "win" 取不到 → 原地跳两下;`BattleArtCatalog.cs:110` CharacterActions 已含 die/win、`:189-190` ActionFps die/win=8;`turn-battle-presentation.md:94` §5.2 要求 die 6 帧/win 4 帧。关键风险:`characters.go:643-651` die 帧 Dy 最大 +21、RotDeg 到 -88°,而 `charFitsAt`(:894-936)要求所有轮廓点 py ≤ 256 且 px 在 [margin, 256-margin];绕脚底锚点旋转 88° 后身体横躺,脚侧半个身宽(01 号 dw=194 → ~97px)落到基线(235.5+21)以下 → 任何身高都不满足 → `charFitHeight` 返回 0 或身高被压到极小。README「统一身高」已警告「改动作幅度会改全体成图尺寸」。

**要改的文件**
- `tools/battle_art_gen/characters.go`、`main.go`、`README.md`
- `E:/work/mmorpg-client/Assets/Resources/Battle/Characters/`(产物)、`BattleUnitView.cs`(只读)

**步骤**
1. 先干跑到临时目录:`go run . -mode characters -char-actions idle,attack,cast,hit,die,win -char-out E:\work\tmp\char_diewin_probe`,看「统一身高」输出与是否报「放不下」。
2. 改 die 几何:frameDef 增加 `GroundClamp bool`(characters.go:538 附近),渲染(:871-874)与 charFitsAt(:916-931)在变换后计算该帧轮廓最低 py,> charBaselineRow(235)则整帧上移;die 第 3~7 帧 GroundClamp=true,Dy 从 8/14/18/20/21 收到 ≤ 0。或最小改法:die 改「侧倒 45° + 缩放 0.85 + 淡出」而非 88°,逐帧试到 fitted_height 仍 175。
3. win 第 3 帧 Dy=-14、Sy=1.08 会抬高头顶,若 fitted_height 掉下去,收 Dy/Sy(目标 fitted_height 仍 175,否则 22 套 idle 全变小、演出台/实机截图口径都要重核)。
4. 确认后正式出图 `go run . -mode characters -char-actions idle,attack,cast,hit,die,win`(默认写 Resources/Battle);Unity 首次导入自动生成 .meta。
5. 客户端无需改代码;更新 README `-char-actions` 默认值说明与「统一身高」实测值;PROGRESS.md 记一条。建议先做 P3-08(只减幅度)再做本条。

**验收标准**
- 22 个 Characters/<id>/ 各多出 die_E_strip.png、win_E_strip.png(2048×256、NRGBA、8 帧),CHARACTER_MANIFEST.json actions 含 die/win 且 skipped=[]。
- fitted_height 不低于 175(若确需下降,PROGRESS 写明并重跑实机截图)。
- 工具自检无「画布边缘有 N 个实体像素」告警;die 末帧最低不透明行 ≤ 235。
- run_showcase 72 帧里群攻死亡(h-death)那拍为侧倒帧序;end_result 帧我方单位有举臂帧。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go build ./... ; go run . -mode characters -char-actions idle,attack,cast,hit,die,win
python -c "import json;d=json.load(open('E:/work/mmorpg-client/Assets/Resources/Battle/Characters/CHARACTER_MANIFEST.json',encoding='utf-8'));print(d['params']['fitted_height'],d['params']['actions'],d['skipped'],d['verify'])"
pwsh -File <tools>/run_showcase.ps1   # 副本工程出包 → 72 帧到 E:/work/tmp/showcase_shots
```

**风险与注意**
die 幅度改动牵动统一身高求解,可能让全部 22 套 idle 变小 → 与已验收帧不一致;倒地帧横向占宽接近 194px 靠近画布左右边(charFitMargin),可能触发溢出告警。

---

### P2-17 怪物无 die 帧条(`-mode monsters` 只出 idle/attack/hit)

**领域** client-art · **工作量** M · **状态** open

**背景与证据**
`tools/battle_art_gen/monsters.go:75-84` monActions 仅 idle/attack/hit;`:261-300` poseFor 只 switch 三种;`Monsters/1/` 仅三条帧条;客户端 `BattleUnitView.cs:656-657` LoadMonsterAction(…,"die") 取不到 → 程序化旋转倒地;`battle-art-prompts.md` §3 契约 `Monsters/<id>/{idle,attack,hit,die}`。怪物是程序化建模(6 模板 buildBeast4/buildHumanoid/…,monsters.go:605-948),加 die 要在 pose 上加倒地/塌陷参数而不是仿射整张图。

**要改的文件**
- `tools/battle_art_gen/monsters.go`、`monsters_verify.go`、`README.md`;产物 `Assets/Resources/Battle/Monsters/`、`MONSTER_MANIFEST.json`

**步骤**
1. `monActions` 追加 `{"die", 8, "踉跄 → 塌倒 → 变暗淡出"}`(FPS 与 BattleArtCatalog.ActionFps("die")=8 对齐)。
2. poseFor 加 `case "die"`:dx 后退 -6…-12、lean 逐帧到 -0.9、scaleY 到 0.55、新增 pose 字段 alpha(1→0.15)与 darken(0→0.35);buildBody(:505)/渲染处按 alpha·darken 叠色(hit 已有 flash 通道可对称加);spirit 模板(hover_px 22)改为下坠到基线再消散。
3. 装箱:自动 fit(MONSTER_MANIFEST notes 第 4 条)会把 die 纳入,看 meta.json fit.scale 变化,超过 5% 就收 die 幅度。
4. `monsters_verify.go:193-200` 对 die 放宽「脚底行 == 235」断言(只对 idle 首帧要求)。
5. `go run . -mode monsters` 重出 6 只;更新 README「-mode monsters」与 MONSTER_MANIFEST notes。

**验收标准**
- Monsters/1..6/ 各有 die_E_strip.png,MONSTER_MANIFEST.json 每只 actions 含 die(frames 8, fps_hint 8);monsters_verify 全过(IoU ≤ 0.6 仍成立);演出台 h-death 帧怪物为帧序倒地。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode monsters
python -c "import json;d=json.load(open('E:/work/mmorpg-client/Assets/Resources/Battle/MONSTER_MANIFEST.json',encoding='utf-8'));print([[a['action'] for a in m['actions']] for m in d['monsters']])"
pwsh -File <tools>/run_showcase.ps1   # 看 E:/work/tmp/showcase_shots 死亡拍
```

**风险与注意**
怪物 fit 参数(scale/origin)全动作共用,加 die 可能让 6 只整体缩小;与 P2-16 同时做时演出台/实机截图一起重核。

---

### P2-18 怪物攻击命中帧 index 4 vs 客户端硬编码 3(怪物受击早一帧)

**领域** client-art · **工作量** S · **状态** open

**背景与证据**
`BattleUnitView.cs:485` `int hitFrame = Mathf.Min(3, strip.Count - 1);`(PlayAttackLunge,角色与怪物共用);`:542` 施法同样 Min(3,…);`monsters.go:82` 与 `:277-283` attack 关键帧 dxK/opK/ghK 峰值在 index 4;MONSTER_MANIFEST.json notes「attack 命中帧 = 第 5 帧(0 基 index 4)」但条目无数值字段;角色侧 meta.json 有 `hit_frame_hint: 3` 且 `characters.go:577` 注释「命中帧 = 3,与 SkillPresentation.HitFrame 缺省一致」;PROGRESS.md:3746;`turn-battle-presentation.md:115`。怪物普攻目标闪白/掉血在 index 3 触发,前扑顶点在 index 4,提前 1 帧(12FPS ≈ 83ms)。与 P2-20 的 skill_presentation 同属表现数据化。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleUnitView.cs`、`BattleArtCatalog.cs`、`tools/battle_art_gen/monsters.go`、`Assets/Resources/Battle/MONSTER_MANIFEST.json`

**步骤**
1. 方案 A(数据驱动,推荐):monsters.go 在每个 action 条目(写入 MONSTER_MANIFEST.json 与 Monsters/<id>/meta.json)加 `hit_frame_hint`(attack=4),重跑 `-mode monsters`;客户端 BattleArtCatalog 增加 `LoadMonsterMeta(uint id)`(读 `Battle/Monsters/<id>/meta` TextAsset,缓存)与 `MonsterAttackHitFrame(id)`(缺则 3);`BattleUnitView.cs:485` 改 `Mathf.Min(IsMonster ? BattleArtCatalog.MonsterAttackHitFrame(MonsterTableId) : 3, strip.Count - 1)`。若 P2-20 做方案 B(monster_presentation.json),hit_frame 走那张表。
2. 方案 B(最小):monsters.go poseFor attack 六个关键帧数组整体左移一帧让峰值落 index 3,desc 与 notes 改「第 4 帧(index 3)」;客户端不改。
3. 结论写进 battle-art-prompts.md §1 表(那里 attack 命中帧=第 4 帧,与 0 基 index 3 相符)。

**验收标准**
- 方案 A:MONSTER_MANIFEST.json 每只 attack 有 hit_frame_hint=4,播放怪物攻击时 onHit 在第 5 格触发(PlayStrip 回调临时 Debug.Log 帧号核对);方案 B:manifest desc 与峰值帧一致为 index 3。
- PVE 局(live_capture 或 P2-12 的 pve 模式)里怪物前扑顶点与目标闪白同帧。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode monsters
pwsh -File <tools>/client_compile_check.ps1   # Roslyn 离线体检
"C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe" -batchmode -nographics -projectPath E:\work\tmp\<copy> -runTests -testPlatform EditMode -assemblyNames MmorpgClient.Tests.EditMode.Battle -testResults E:\work\tmp\edit_results.xml -logFile E:\work\tmp\edit.log   # 不加 -quit
```

**风险与注意**
方案 B 改怪物动画节奏,需重看 6 只 attack 帧;方案 A 多一次 Resources.Load(TextAsset) 但可缓存。

---

### P2-19 怪物帧条只覆盖 MonsterTable 1~6,副本 2/3 的怪 7/11/12/16 走程序化剪影;扩展花名册过不了 IoU 断言

**领域** client-art · **工作量** L · **状态** open

**背景与证据**
`Assets/Resources/Battle/Monsters/` 只有 1..6;`generated/tables/Monster.json` 16 行;`Dungeon.json` 副本 1 monster [1,2]、副本 2 [6,7]、副本 3 [11,12,16];`BattleArtCatalog.cs:511` GetMonsterSilhouette 程序化剪影兜底,`BattleUnitView.cs:997` 缺帧条走剪影;`tools/battle_art_gen/README.md` 末段「扩展花名册(7~16 号)过不了这条断言:石魔/山神像=石灵换色(IoU 0.99)…golem/serpent/spirit/insect 模板还没参数化」;`monsters.go:852-948` buildGolem/buildSerpent/buildSpirit/buildInsect 无逐只形体参数。工具刻意拒绝换色克隆(monsters_verify IoU ≤ 0.6)。

**要改的文件**
- `tools/battle_art_gen/monsters.go`、`monsters_verify.go`、`README.md`、`Assets/Resources/Battle/Monsters/`

**步骤**
1. 优先级按副本用量:先 7(副本 2)、11/12/16(副本 3),再 8/9/10/13/14/15。
2. 仿 beastFormOf/humanFormOf 增加 golemFormOf/serpentFormOf/spiritFormOf/insectFormOf(体块比例、头径、附件模块开关:石魔加肩甲/山神像加莲座;铁甲蟒背鳞/蛇妖颚须;怨灵拖尾长度/幽魂头巾;蚀骨虫节数/毒蜂翅型),buildGolem/… 读这些参数。
3. 与已有怪同模板的(狼王≈野狼 0.80、山魈≈山鬼 0.72)调 beastForm/humanForm 参数直到 IoU ≤ 0.6。
4. `go run . -mode monsters -monster-count 16`,verify 通过后落盘;MONSTER_MANIFEST 列 16 只。客户端不改(按 monsterTableId 目录查找)。

**验收标准**
- Monsters/1..16 各有 idle/attack/hit_E_strip.png;MONSTER_MANIFEST.json verify 两两 IoU 全 ≤ 0.6,进程 0 退出。
- 实机 PVE 副本 2/3 截帧无程序化剪影(深紫剪影 + 发光眼)。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode monsters -monster-count 16; echo $LASTEXITCODE
python -c "import json;d=json.load(open('E:/work/mmorpg-client/Assets/Resources/Battle/MONSTER_MANIFEST.json',encoding='utf-8'));print(len(d['monsters']), d.get('verify'))"
```

**风险与注意**
每加一只都要过全体两两 IoU,后面越难;工时按只线性增长(每只约半天含调参)。

---

### P2-20 `skill_presentation.json`(skill_id → fx_id/action/hit_frame)未产出,客户端走 id%3 启发式;导表器无客户端 json 通道

**领域** client-art / client-presentation · **工作量** M · **状态** open

**背景与证据**
`BattleArtCatalog.cs:78` `SkillPresentationPath = "Battle/skill_presentation"`、`:266-284` ResolveSkillFx 缺表时 cycle[skillId%3](fire_burst/ice_shard/lightning_strike,全部当 cast、hit_frame 3)、`:762-775` SkillPresentationRow{skill_id,fx_id,action,hit_frame}/SkillPresentationTable{entries}、`:802-832` EnsureSkillTable(裸数组自动包 `{"entries":…}`);`Assets/Resources/Battle/` 无 skill_presentation.json;`battle-art-prompts.md` §3 末行与 `turn-battle-presentation.md:52` D6 规定该表走客户端表管线;`generated/tables/Skill.json` 13 行且无表现字段;导表器 `exporter_config.yaml` csharp 段 deploy 到 `client/unity/Assets/Scripts/Table/Generated`,但 mmorpg-client 该目录不存在(从未部署过);json_gen 产物格式 `{"data":[…]}` 与客户端期望的 `entries`/裸数组不同;`schema.py:159` 只是把 client/design 列从服务端生成里排除,无客户端输出;`BattlePresenter.cs:336` 消费 pres.FxId/Action/HitFrame 已就位。Fx/ 下 9 套特效:slash_arc/thrust_line/fire_burst/ice_shard/lightning_strike/heal_ring/buff_rise/hit_star/death_dissolve。

**要改的文件**
- `E:/work/mmorpg-client/Assets/Resources/Battle/skill_presentation.json`(新建)、`BattleArtCatalog.cs`、`BattleUnitView.cs`(:481)、`BattlePresenter.cs`(:323)
- 方案 B:`data/Skill.xlsx`(加列)或新建 `data/SkillPresentation.xlsx` + `data/schema/skillpresentation_table.proto`、`tools/data_table_exporter/exporter_config.yaml`、`core/schema.py`、`run.py`

**步骤**
1. 方案 A(过渡,半小时):手写 `skill_presentation.json` 裸数组,每行 `{"skill_id":N,"fx_id":"…","action":"cast|attack","hit_frame":3}`,覆盖 13 个 id;fx_id 只取 Fx/ 现有 9 条;治疗 heal_ring、增益 buff_rise。
2. 方案 B(正式):Skill.xlsx 追加 owner=client 列 fx_id/action/hit_frame(row1 名、row2 类型 string/string/uint32、row3 owner=client),先跑 `python run.py` 确认服务端产物不变;导表器加客户端 json 输出:`exporter_config.yaml` 新增 `client_json: {enabled: true, dir: E:/work/mmorpg-client/Assets/Resources/Battle, tables: {Skill: skill_presentation.json}}`,run.py 末尾 emitter 取 id + owner==client 列写 `{"entries":[{"skill_id":id,…}]}`;tests 加 pytest 断言产物 schema。或新建独立 SkillPresentation.xlsx + proto(cfg_sheet="SkillPresentation",owner 标 client)并改 `EnsureSkillTable` 兼容 `{"data":[…]}`(加 `public SkillPresentationRow[] data;`,取 entries ?? data)。
3. Monster.xlsx 同法加 client 列 `attack_hit_frame`(与 P2-18 联动),导出 monster_presentation.json,BattleArtCatalog 新增 `ResolveMonsterPresentation(monsterTableId)`。
4. `BattleUnitView.PlayAttackLunge(:481)` hitFrame 改:玩家取 `ResolveSkillFx(skillId,true).HitFrame`,怪物取 monster 表(缺表默认 4);`BattlePresenter.PlayAttack(:323)` 传 pres.HitFrame 而非 DefaultHitFrame。
5. Resources 下新文件 .meta 由编辑器生成或复制 ART_MANIFEST.json.meta 改 guid。

**验收标准**
- `Resources.Load<TextAsset>("Battle/skill_presentation")` 非空且 13 条,无「解析失败」告警;`ResolveSkillFx(表内 id,false).FxId == 表值`。
- 演出台 r2/r3 帧技能特效与表一致(某技能配 heal_ring 后施放帧出现地面绿圈而非火球)。
- 方案 B:`python -m pytest` 全绿;`generated/tables/Skill.json` diff 为空。

**验证方式**
```powershell
python -c "import json;print(len(json.load(open('E:/work/mmorpg-client/Assets/Resources/Battle/skill_presentation.json',encoding='utf-8'))))"
cd E:\work\xuanming-server-mmo\tools\data_table_exporter; python run.py; python -m pytest; git -C E:/work/xuanming-server-mmo diff --stat generated/tables/Skill.json
pwsh -File <tools>/client_compile_check.ps1; pwsh -File <tools>/run_showcase.ps1   # 看 show_00xx_r2/r3 帧
```

**风险与注意**
导表器输出到另一仓库路径属跨仓写入,config 明确并写 readme;csharp/JSON 部署链路对客户端从未跑通,别指望自动落地;`schema_proto.py:63 _KNOWN_OWNERS` 含 client,protoc 校验应无问题。

---

### P2-21 顶部行动预告条在回合结算态与胜负面板期间常驻,播完只 ResetHighlight 不 Clear

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`BattleScreen.cs:773` `_orderBar.ResetHighlight();`(CoPlayTurn 播完);`:284/:300` `Clear()` 只在 Open/Close;`:328` AbortPlayback 也只 ResetHighlight;`BattleHud.cs:266-285` ResetHighlight/Clear 定义。帧 `E:/work/tmp/showcase_shots/show_0030_tick.png`(「第 3 回合 · 结算」顶部 9 头像仍在)、`show_0061_end_result.png`(结算板已出,顶部 4 头像仍在)、`E:/work/tmp/shots_live5/A/A_0163_settle.png`(终局仍有 2 瓦片)。规格 §1 出手序只在回合播放中出现。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`、`BattleHud.cs`、`BattleUiRoot.cs`

**步骤**
1. `BattleScreen.CoPlayTurn:773` `ResetHighlight()` 改 `Clear()`(或 `SetVisible(false)`,下一回合 SetOrder 前 SetVisible(true))。
2. `AbortPlayback(:328)` 同改 Clear。
3. `BattleUiRoot.ShowResult(:722)` 调用前加 `_battleScreen.HideActionOrder()` 公开方法一并清掉。
4. UI 对象无法 EditMode,用演出台帧验收。

**验收标准**
- run_showcase 重跑后标 `第 N 回合 · 结算` 之后、下一回合 marker 之前的 tick 帧顶部无头像瓦片;end_result / settle 帧顶部无瓦片;回合播放中瓦片仍按出手序出现且高亮推进。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1 -SkipBuild   # 需先出包一次;看 show_0030_tick.png、show_0061_end_result.png
"C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe" -batchmode -nographics -projectPath E:\work\tmp\shotverify_project -runTests -testPlatform EditMode -testResults E:/work/tmp/orderbar_editmode.xml -logFile E:/work/tmp/orderbar_editmode.log   # 不加 -quit
```

**风险与注意**
观战流(CoPlaySpectateTurn)每回合都会被抢占,Clear 后瓦片在新回合 SetOrder 前有一帧空白,可接受。

---

### P2-22 命令环底图贴屏幕右下边(底边越出设计面 5px),缺安全边距;布局测试只校验按钮矩形不校验环底图

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`BattleCommandRing.cs:38-41` `CenterX=2290, CenterY=850, RingSize=470` → 环底图 y 615..1085,超出 DesignHeight 1080(`QdaoUguiTheme.cs:14`)5px,右缘 2525(距 2560 仅 35px);`BattleHudLogic.cs:152-156` InteractiveRects 只登记 ButtonRect/AutoKeyRect,BattleUiLayoutTests 因此通过;帧 show_0001_start_5v5.png 右下环外圈贴底,Development Build 水印(ShowcaseBuild.cs:67,属预期)压在「道具」格右下;`turn-battle-presentation.md:115` 登记「命令环右下安全边距」。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleCommandRing.cs`、`BattleHudLogic.cs`、`Assets/Tests/EditMode/Battle/BattleUiLayoutTests.cs`

**步骤**
1. `:38-39` 改 `CenterX = 2270f`、`CenterY = 815f`(环底图 x 2035..2505,y 580..1050;右/下各留 55/30px);按 24~32px 目标微调。
2. 核对 AutoKeyRect(:60-64,AutoKeysTop=920)与新环位置在自动/手动切换时视觉一致,必要时 AutoKeysTop 上移到 ~900。
3. `BattleUiLayout.InteractiveRects(:152)` 新增 `("命令环底图", new Rect(CenterX-RingSize/2, CenterY-RingSize/2, RingSize, RingSize))`;可选断言「命令环底图与设计面右/下边距 ≥ 24」。

**验收标准**
- EditMode BattleUiLayoutTests 全绿且包含命令环底图矩形;show_0001 帧右下环外圈与底/右各 ≥ 24 设计像素;1920×1080、2340×1080 帧(P2-27)同样不贴边。

**验证方式**
```powershell
# Unity -runTests EditMode(副本工程,不带 -quit),看 xml 里 BattleUiLayoutTests
pwsh -File <tools>/run_showcase.ps1   # 放大 show_0001_start_5v5.png 右下
```

**风险与注意**
环上移后与右上角色卡列(第 4 张卡底 y≈420)仍相距 >150px;若角色卡改 5 张(P2-D7)需一起看。

---

### P2-23 右上角色卡等级文字不可读:Lv 文本落在九宫金边区内、金字压金边(原判「被裁」实为低对比+贴边)

**领域** client-presentation · **工作量** S · **状态** partially-done

**已完成部分**:`BattleHud.cs:350` `CreateText("Level", card.Root, 246f, 8f, 76f, 26f, …)` 文字右缘 322,卡宽 330(:295),内边距 8px 已有,「被裁」在最新帧已不成立。

**背景与证据**(剩余部分):`:339-340` 卡底 `ApplyNineSlice(bg, "panel_9slice")`,`battle-art-prompts.md` §4 写 panel_9slice 为 64 边距,x 266..330 全是金色边框区;WarnText=#E5C557(`BattleUiStyle.cs:78`)金字压金边。帧 show_0001_start_5v5.png 右上四张卡 Lv 几乎不可辨;A_0000_start.png 同。`turn-battle-presentation.md:115` 措辞「被裁」应改为「与九宫边重叠/低对比」。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleHud.cs`、`Assets/Resources/Battle/UI/panel_9slice.png`(只读)、`docs/design/turn-battle-presentation.md:115`

**步骤**
1. `BattleHud.cs:348-350`:名字宽 150→130,等级 Text 改 `x=190f, width=70f`(右缘 260,落在九宫内容区 64..266 内),或 CardWidth 加到 360 并同步 CardRect/RightMargin。
2. 等级颜色改亮色(QdaoUguiTheme.Cream 或 #FFE9A8),`BattleUiWidgets.ApplyOutline(levelText, 0.2f, 黑)`(BattleUnitView.cs:259 已有同名工具)。
3. HpText(:354,12pt)提到 14pt。
4. BattleUiLayoutTests 中角色卡矩形若改宽需同步 CardRect 断言;文档措辞修正。

**验收标准**
- show_0001 右上四张卡 Lv62/Lv60/Lv63/Lv59 原尺寸下可辨,不与金边重叠;A_0000(1600×900)右上卡 Lv 可读。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1   # 放大 show_0001_start_5v5.png 右上
pwsh -File E:\work\mmorpg-client\tools\run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe -ShotDir E:/work/tmp/shots_live6 -ShotInterval 0.3 -ScreenWidth 1600 -ScreenHeight 900   # A_0000_start.png
```

**风险与注意**
无。

---

### P2-24 演出台(PresentationShowcase)命名截图挂在事件入队时刻而非演出拍点,且开局回合框显示「第 0 回合」

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`PresentationShowcase.cs:340-341` `Mark(marker); _net.Push(NotifyTurnResult, turn);` → ShotLoop(:741-757)在下一 WaitForEndOfFrame 立即截,演出尚未开始;`:88` `_round` 初值 0,`:249` 开局 Snapshot(:421 `RoundIndex = _round`)→ BattleScreen.Open(:286)显示「第 0 回合」;帧 show_0001 左上「第 0 回合」、show_0015_r2_c-aoe5_* 左上仍「第 1 回合」且无任何特效/数字。`run_showcase.ps1:82` 已传 `-shotInterval 0.25`。命名帧是验收对账锚点,现在全是「上一状态」。

**要改的文件**
- `E:/work/mmorpg-client/Assets/Scripts/App/PresentationShowcase.cs`

**步骤**
1. `PlayTurn(:322)`:`Mark(marker)` 移到 Push 之后并延迟到命中拍:`_net.Push(...); yield return WaitSeconds(0.55f); Mark(marker);`(普攻 0.22s 冲刺+0.1s 命中,群攻 SKILL 拍约 0.6s 命中);对 r2/r5 群攻再补 `Mark(marker+"_hit2")` 于 +1.0s。
2. 开局:Snapshot 的 RoundIndex 用 `Math.Max(1, _round)`,或 BattleStart 前 `_round = 1` 并让 PlayTurn 首次不再 ++(与服务端一致:BattleStartS2C.state.round_index 从 1 起,见 A_0000)。
3. start_5v5 marker 改为 Push 后 0.6s 再截(配合 P3-11 出生光环复核)。
4. CoverageLegend(:303 附近)注明「命名帧 = 命中拍」。

**验收标准**
- show_00xx_r2_c-aoe5_* 帧内可见范围特效 + 5 串红色数字同拍;r5 帧可见暴击黄字与团灭渐隐;开局帧左上「第 1 回合」。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1   # 需重新出包,不能 -SkipBuild;看 run_showcase.log 帧名与 E:/work/tmp/showcase_shots
```

**风险与注意**
WaitSeconds 用 realtime;合成台 ActionDeadlineMs 若为 0 则 Unbounded 原速,延迟值稳定。

---

### P2-25 实机 1v1 阵型:服务端 formation_slot 两边都为 0,客户端按首槽摆放导致两单位都落在左列、我方在正中而非右下

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
服务端 `turn_battle_engine.cpp:127/180` `set_formation_slot(NextFormationSlot(team))`(每队从 0 递增),`turn_battle_engine_test.cpp:1338-1340` 断言 A=0/B=1/怪物 0;客户端 `BattleStage.cs:129-163` AssignSlots 直接用 formation_slot,SlotPosition(:94-104)按 `col - 2` 偏移,slot 0 = 排最左下;帧 `E:/work/tmp/shots_live5/A/A_0000_start.png`:敌 (390,480)、我 (750,520) @1600×900,右半场空;PROGRESS.md:3739-3740 已把「人数不足时排内居中」交给验收台但未落地。`battle_data.proto:79` 注释 formation_slot「演出站位用,不参与判定」,客户端可自行居中。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleStage.cs`、`Assets/Tests/EditMode/Battle/BattleStageTests.cs`、`BattleScreen.cs`

**步骤**
1. BattleStage 新增 `public static Dictionary<ulong,int> CenterWithinRows(Dictionary<ulong,int> assigned)`:分别统计前排(0-4)/后排(5-9)占用列集合,占用列为 {0..n-1} 且 n<5 则整体右移 `(5-n)/2`(1 人 → 列 2;2 人 → 1、2;3 人 → 1..3;4 人 → 0..3)。
2. AssignSlots 末尾(:163 return 前)或 AssignAll(:170)调用;两队各自居中。
3. BattleStageTests 新增:1v1 两队各 1 人 → 槽位均为 2,SlotPosition(mine,2).x > Center.x、SlotPosition(enemy,2).x < Center.x;2 人 → {1,2};5 人不变;后排独立居中。
4. 可选(服务端,不推荐本轮):NextFormationSlot 改中心优先 2,1,3,0,4。

**验收标准**
- 实机 1v1 帧:敌方在左上象限、我方在右下象限(1600×900 下我方 x≈1000~1150,y≈560~620);5v5 演出台阵型与 show_0001 完全一致;BattleStageTests 全绿。

**验证方式**
```powershell
# Unity -runTests EditMode(副本工程)
pwsh -File E:\work\mmorpg-client\tools\build_crosszone_player.ps1; pwsh -File E:\work\mmorpg-client\tools\run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe -ShotDir E:/work/tmp/shots_live6 -ShotInterval 0.3 -ScreenWidth 1600 -ScreenHeight 900   # A_0000_start.png;服务栈需经 WMI 起齐
```

**风险与注意**
PVE 怪物队也走同一居中逻辑;DungeonTable 怪物组 1~3 只时会居中,符合录像观感。

---

### P2-26 实机终局(玩家阵亡)截不到胜负结算面板:自动驾驶 FinishWithShots 窗口(≈1.3s)短于「末回合演出播完 → ShowResult → 面板入场」

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`DevAutoPilot.cs:551-563` FinishWithShots:6 张 × (1 + 12 帧) ≈ 1.3s@60fps 后 QuitNow;`BattleUiRoot.cs:454-462` HandleBattleEnd 在 `_playing` 时只记 `_pendingEnd`,等 CoPlayTurn 播完(:432-447)才 ShowResult(:722);帧 A_0156_final~A_0163_settle:HP 0/830、「-2」数字、命令环仍在、「回合播放中…」,无「失败」大字与奖励;对比合成台 show_0068_settle.png 有结算板。败方面板是否正确渲染至今未被实机验证。

**要改的文件**
- `Assets/Scripts/App/DevAutoPilot.cs`、`Assets/Scripts/UI/Ugui/Battle/BattleUiRoot.cs`、`BattleResultPanel.cs`

**步骤**
1. BattleUiRoot 暴露 `public bool IsResultShowing => _resultShowing;`。
2. FinishWithShots:先 `while (!(BattleUiRoot.Instance?.IsResultShowing ?? false) && elapsed < 8s) yield return null;`,再从面板出现起按 0.3s 截 8 张,然后 QuitNow;超时仍无面板则 RESULT 行追加 `result_panel=missing`。
3. 检查 `BattleResultPanel.Show` 对 Outcome=SideBWin(己方败)的文案与按钮。

**验收标准**
- A 侧(败)与 B 侧(胜)最后 8 帧均见胜/负大字、奖励列表与确认按钮;命令环被 ModalDim 压暗;RESULT 行不含 result_panel=missing。

**验证方式**
```powershell
pwsh -File E:\work\mmorpg-client\tools\run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe -ShotDir E:/work/tmp/shots_live6 -ShotInterval 0.3   # 看 <ShotDir>/A|B 末尾 settle 帧与 crosszone_pair_live/*.log 的 RESULT 行
```

**风险与注意**
末回合演出被 PlaybackBudget 判 Unbounded 原速播,5v5 可长达 6~9s,等待上限 8s 需按模式放宽。

---

### P2-27 客户端提交 99d891f 验证清单未勾「多分辨率战斗 UI 视觉验收」:EditMode 几何已覆盖 8 种分辨率,但帧只在 2560×1080 与 1600×900 看过

**领域** client-presentation · **工作量** S · **状态** partially-done

**已完成部分**:`Assets/Tests/EditMode/Battle/BattleUiLayoutTests.cs:16-24` Resolutions 含 2560×1080/1920×1080/1920×1200/2340×1080/1280×720/1366×768/2560×1440/3440×1440,断言所有可交互矩形在可见区内(Expand,`BattleUiRoot.cs:272-276`);「双 zone 跨区匹配集成验证」已由 PROGRESS.md:3793 CROSS_ZONE_PAIR_PASS 覆盖。

**背景与证据**(剩余部分):`git show 99d891f` 正文「- [ ] 多分辨率战斗 UI 视觉验收」;视觉帧仅 showcase 2560×1080(`run_showcase.ps1 -Width/-Height` 参数存在)与实机 1600×900;`run_crosszone_pair.ps1` 默认 1280×720。命令环底图不在 InteractiveRects(P2-22)。

**要改的文件**
- scratchpad `run_showcase.ps1`(或入库后的 `tools/run_showcase.ps1`)、`PROGRESS.md`、`docs/design/turn-battle-presentation.md` §6

**步骤**
1. `run_showcase.ps1 -SkipBuild -Width 1920 -Height 1080 -ShotDir E:/work/tmp/showcase_1080p`,再 `-Width 2340 -Height 1080 -ShotDir E:/work/tmp/showcase_2340`,各取 start/r2/end_result 三帧核对:命令环、角色卡、预告条、回合框、计时环不出屏,两阵斜带不被裁,数字可读。
2. 实机 `run_crosszone_pair.ps1 -ScreenWidth 1920 -ScreenHeight 1080` 再跑一轮。
3. 结果(分辨率 × 通过/问题)写进 §6 视觉验收段与 PROGRESS;提交信息无法改,勾选以文档为准(P2-13 对照表里注明)。

**验收标准**
- 三种分辨率 start 帧四角 HUD 完整、无裁切;发现问题登记为新 minor。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1 -SkipBuild -Width 1920 -Height 1080 -ShotDir E:/work/tmp/showcase_1080p
pwsh -File <tools>/run_showcase.ps1 -SkipBuild -Width 2340 -Height 1080 -ShotDir E:/work/tmp/showcase_2340
```

**风险与注意**
`-screen-width/height` 在窗口模式下受桌面分辨率限制(2340 宽超过 1920 桌面会被缩),必要时 `-screen-fullscreen 0` 且桌面 ≥2560 宽。

---

### P2-D1 MatchRedis:生产形态与文档不一致(K8s volatile-lru vs §11.9 noeviction)、本地默认 yaml 指向 7000-7005 集群但 compose profile 默认不起、RatingEnabled 时 MatchRedis 缺省应拒启/WARN(决策 D-03)

**领域** server-go / k8s-deploy · **工作量** S · **状态** needs-decision(部分已做)

**已完成部分**:K8s 档 `k8s_deploy.ps1:1099-1101`、`:1384-1396` match ConfigMap 写 MatchRedis=redis-match-cluster-{0..5} headless FQDN + Type cluster(A 档实跑生效);`go/match/etc/match_service.yaml:23-25` 本地默认指向 127.0.0.1:7000-7005 cluster;设计文档 `cross-zone-matchmaking.md` §4.2(L159-161)与 §11.9 第 1 条(L428)已写明约束。

**背景与证据**(剩余部分):① `go/match/internal/svc/servicecontext.go:154-158` NewRedisHandles 在 MatchRedis.Host 为空时静默回落共享句柄,无 RatingEnabled 联动校验;`config.go:21` MatchRedis `json:",optional"`、`:93` RatingEnabled 默认 true——少配一行,评分静默写进 allkeys-lfu 的共享库(`deploy/docker-compose.yml:121`),分丢了无日志;② `redis-match-cluster.yaml:25-28` `maxmemory 512mb / maxmemory-policy volatile-lru`,注释「match 的 key 都是短 TTL」——评分 `match:rating:*` 无 TTL,volatile-lru 不淘汰它们,但满时会静默淘汰带 TTL 的票据/挑战/观战索引(票据被淘汰等价于玩家被踢出队列且无日志),与 §11.9 字面 noeviction 不一致,512mb 未按玩家数估算;③ `deploy/docker-compose.yml:127-152` redis-cluster 为 `profiles: ["redis-cluster"]` 默认不起,开发者不起 profile 时 match 起不来,`tools/scripts` 无 redis-cluster 引用;compose 节点(:159、:173)未配 maxmemory → 默认 noeviction。

**要改的文件**
- `go/match/internal/svc/servicecontext.go`、`servicecontext_test.go`、`go/match/internal/config/config.go`、`go/match/etc/match_service.yaml`
- `deploy/k8s/manifests/infra/redis-match-cluster.yaml`(25-28)、`deploy/k8s/scene-manager-alerts.yaml`(参考格式)
- `deploy/docker-compose.yml`(127-152)、`tools/scripts/go_services.ps1` / `dev.bat`
- `docs/design/cross-zone-matchmaking.md`(§4.1 L147-149、§11.9 L427-428)、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`

**步骤**
1. 代码门禁(不依赖拍板):config.go 加 `RequireMatchRedis bool json:",default=false"`;servicecontext.go NewRedisHandles 之后新增 `validateRatingStorage(c)`:`if c.RatingEnabled && c.MatchRedis.Host == "" { if c.RequireMatchRedis || c.Mode == "pro" { return error/log.Fatal } else { logx.Errorf("[match] RatingEnabled 但 MatchRedis 缺省:评分落共享库,allkeys-* 淘汰会把评分回落 1500;生产必须配独立 MatchRedis(设计文档 §11.9)") } }`。可选:启动时对 MatchRedis 每个主节点 `CONFIG GET maxmemory-policy`,非 noeviction/volatile-* 时 WARN(托管 Redis 常禁 CONFIG,失败只 WARN)。
2. servicecontext_test.go 加 3 例:RatingEnabled+缺省+Require=true → 错误;=false → 仅告警;配置齐全 → 通过。
3. k8s_deploy.ps1 match ConfigMap(:1384-1420)写 `RequireMatchRedis: true`;契约测试加 go-svc-match-config 的 MatchRedis.Host 含 redis-match-cluster 且 Type==cluster、RequireMatchRedis==true。
4. 拍板淘汰策略。A:`redis-match-cluster.yaml:28` 改 `noeviction`,注释改写(「排队/票据短 TTL + 评分无 TTL」),按 DAU 估算 maxmemory(评分 hash 约 200B/玩家,100 万 ≈ 200MB,建议 1gb 加注释公式),加 Prometheus `redis_memory_used_bytes / redis_memory_max_bytes > 0.8` 告警;B:文档 §4.1/§11.9 改「volatile-lru,评分 key 无 TTL 不受影响;票据可能在满时被淘汰,以 match_queue_depth 与 ErrAlreadyQueued 比例观察」。改 redis.conf ConfigMap 后要 rollout restart StatefulSet。
5. 拍板本地默认形态。「默认回落」:match_service.yaml:23-25 注释掉 MatchRedis 块并给出开 cluster 两步;「默认集群」:dev.bat/go_services.ps1 起 match 前 `docker compose -f deploy/docker-compose.yml --profile redis-cluster up -d` 并等 7000 端口。
6. `match_service.yaml:86-90` 注释同步 RequireMatchRedis;文档与 README 同步。

**验收标准**
- 本地注掉 MatchRedis 段、RequireMatchRedis: true 起 match → 拒启并打印含「MatchRedis」「§11.9」的错误;缺省时只有 ERROR 日志、照常启动;`go test -count=1 ./internal/svc/...` 通过。
- K8s DryRun:go-svc-match-config 含 RequireMatchRedis: true;`redis-cli CONFIG GET maxmemory-policy` 与文档字面一致;契约测试全绿。
- 本地 `dev.bat`/go_services.ps1 一键起后 match STARTED SUCCESSFULLY,不需人工额外起 profile;cross-zone 冒烟不回归。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\match; go build ./... ; go test -count=1 ./internal/svc/...
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -ZoneId 101 -DryRun -GoSvcRegistry registry.invalid/test | Select-String 'MatchRedis','RequireMatchRedis','maxmemory'
kubectl -n mmorpg-infra exec redis-match-cluster-0 -- redis-cli CONFIG GET maxmemory-policy; kubectl -n mmorpg-infra logs deploy/match | Select-String 'MatchRedis'
docker compose -f E:\work\xuanming-server-mmo\deploy\docker-compose.yml --profile redis-cluster ps; Test-NetConnection 127.0.0.1 -Port 7000
```

**风险与注意**
强校验若默认开会让「本地单库、不起 redis-cluster profile」的开发形态拒启,所以默认只 WARN、K8s ConfigMap 显式开;noeviction 下真满内存时 JoinQueue/评分写入全部报错直到扩容,需配套告警;改 redis.conf 需重启 StatefulSet,评分在 appendonly 下不丢。

---

### P2-D2 怪物 AI 只普攻 + 数值平衡(速度量级 / 防御加法减伤归零 / 技能 damage 表达式)——策划项,先拍板再改表(决策 D-04)

**领域** server-cpp · **工作量** M · **状态** needs-decision

**背景与证据**
`turn_battle_engine.cpp:511-524` FillDefaultActions 对怪物一律 BATTLE_ACTION_ATTACK、target 0 随机;`data/schema/monster_table.proto:17-27` 无技能列;`generated/tables/Skill.json` 首行 damage='100*level'(`skill_table.proto:78`);Class 表 init_speed=20(`class_table.proto:26`),`player-attribute-allocation.md` §8 记速度 1 级 23 > 全部怪物 8~20、体质 5 点让 1 号怪普攻归零(ApplyDamage `turn_battle_engine.cpp:1047-1056` 饱和到 0);PROGRESS.md:3707「记入缺口不擅改表(策划平衡项)」。PVE 已数据化到 15~24 回合,但怪物永远后手、逃跑 95% 饱和、低级投体质即无敌。**2026-09-10 更新**:角色属性系数已由策划上调(体质 50 血 + 5 防 / 灵力 40 法伤 + 10 蓝 / 力量 50 物伤 / 敏捷 3 速度),上述"5 点""15~24 回合"都是旧系数口径——新系数下 1 级投 2 点体质(或 3 级不投点)即让 1 号怪普攻归零,低级副本 1~2 回合结束,PVP 7 级起首击秒杀;最新定量见 `player-attribute-allocation.md` §8。本条剩余待决的是怪物侧强度。

**要改的文件**
- `data/Monster.xlsx`、`data/schema/monster_table.proto`、`data/Skill.xlsx`、`data/Class.xlsx`、`data/AttributeDimension.xlsx`
- `cpp/libs/services/battle/system/turn_battle_engine.cpp`、`cpp/tests/turn_battle_engine_test/`
- `docs/design/turn-based-battle-server.md`、`player-attribute-allocation.md`

**步骤**
1. 策划拍板三项(§2 D-04),写进 turn-based-battle-server.md 新小节。
2. 最低伤害下限(若选):`turn_battle_engine.cpp` 计算 finalDamage 处(:610/:715 调用 ApplyDamage 前)加 `finalDamage = max(finalDamage, rawDamage * kMinDamageRatio)`,常量入 turn_battle_constants.h;用例「高防御下伤害不为 0」。
3. 怪物技能(若选):monster_table.proto 加 `repeated uint32 skill = 10 [(cfg_slots)=N]`,Monster.xlsx 加列;AppendMonsterActor(:169-195)装技能;FillDefaultActions 对 MONSTER 走 `PickMonsterAction`(按规则选 SKILL/ATTACK,用引擎 RNG 保证确定性);用例(怪物出技能事件流确定性 / 无技能列回退普攻)。
4. 改表后跑导表器 + `cd go && build.bat`,重编 battle 库与节点;table_battle_data_provider_test 真表契约同步;battle_smoke 观察回合数与「怪物永远后手」「0 伤害事件流」消失。

**验收标准**
- 1 级玩家投 2 点体质(2026-09-10 新系数下的归零阈值;旧系数口径为 5 点)对 1 号怪的事件流中 DAMAGE value > 0;同级怪物速度与玩家有交错,逃跑成功率不再饱和 95%;(若配技能)怪物回合内出现 SKILL 事件且单测确定性;turn_battle_engine_test 全绿;battle_smoke 回合区间按新系数重定(旧系数口径 15~30 回合)。

**验证方式**
```powershell
# 导表器 + cd E:/work/xuanming-server-mmo/go; .\build.bat
msbuild E:/work/xuanming-server-mmo/game.sln /t:battle /p:Configuration=Debug /p:Platform=x64 /m:1
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle_engine_test
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml 2>&1 | Select-String 'turns|SIDE_|DAMAGE'
```

**风险与注意**
改表影响配表指纹(BattleTableFingerprint),enforce 模式下双端表不一致会拒建局,改表后所有 battle 节点一起重拉;Skill.damage 表达式全仓共用(实时战斗也读)。

---

### P2-D3 引擎/实时侧 CheckPlayerLevel 恒通过(技能等级需求校验形同虚设,Skill 表无等级列)(决策 D-05)

**领域** server-cpp · **工作量** S · **状态** needs-decision

**背景与证据**
`turn_battle_engine.cpp:401-407` `实时侧同名校验尚未实现(TODO 挂点)…恒通过`;`cpp/libs/services/scene/combat/skill/system/skill.cpp:162-165` `// TODO: Implement level requirement validation` return kSuccess;`skill_table.proto` 无 required_level/unlock_level。同文件 `:1226`「实时 OnBuffRefresh 为 TODO」——实时侧 `buff.cpp:328` / `modifier_buff_impl.cpp:19` 已有 OnBuffRefresh 实现,注释可能过时。

**要改的文件**
- `cpp/libs/services/battle/system/turn_battle_engine.cpp`、`cpp/libs/services/scene/combat/skill/system/skill.cpp`
- `data/Skill.xlsx`、`data/schema/skill_table.proto`、`data/tip/Tip.xlsx`、`cpp/libs/services/scene/combat/buff/system/modifier_buff_impl.cpp`、`cpp/tests/turn_battle_engine_test/`

**步骤**
1. 拍板 A/B(§2 D-05)。
2. B:skill_table.proto 加 `uint32 required_level = 21`,Skill.xlsx 加列,Tip.xlsx skill 段加 kSkillLevelNotEnough(按 tip-code-axis.md §6 流程导表),`cd go && build.bat`。
3. 引擎 CheckPlayerLevel:`actor.level() < skillRow.required_level() → 新码`;实时 skill.cpp:162 同口径读 LevelComp;两处注释同步。
4. 引擎单测:等级不足 SKILL 降级普攻并产出对应事件/错误;实时 skill_test 同款。
5. 顺带核对 `:1226` 注释:若 modifier_buff_impl.cpp OnBuffRefresh 已刷新持续时间,改注释为「与实时 OnBuffRefresh 同口径」。

**验收标准**
- (B)等级不足时 SubmitBattleAction 走 SKILL 被拒/降级,事件流有记录;实时 UseSkill 返回新 tip。(A/B)turn_battle_engine_test、skill_test 全绿;TODO 注释消失。

**验证方式**
```powershell
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build -Filter 'turn_battle_engine_test|skill_test'
msbuild E:/work/xuanming-server-mmo/game.sln /t:battle;scene /p:Configuration=Debug /p:Platform=x64 /m:1
```

**风险与注意**
改 Skill 表触发配表指纹变更;新 tip 码要走 Tip.xlsx 段式发号(手写字面量会被 TestNoHandWrittenTipCodes 判红,PROGRESS.md:3979 合并缝 1)。

---

### P2-D4 完整死亡/复活流程(复活点/惩罚/原地复活道具)待产品定稿(决策 D-06)

**领域** server-cpp · **工作量** M · **状态** needs-decision

**背景与证据**
`docs/design/turn-based-battle-server.md` §15.4(:496-500)「基线(2026-09-02)…产品定稿死亡流程时替换此基线」;`player_revive.h` + `player_database_loader.cpp:24-32` ApplyClassInitialAttributesOrRevive;143ecca96 后复活按 Derived 上限回满(PROGRESS.md:3979 条目)。基线保证阵亡玩家不会带 0 血再入队被秒(冒烟实测过的缺陷),但 PVP 失败零代价。

**要改的文件**
- `cpp/libs/services/scene/player/system/player_revive.h`、`player_database_loader.cpp`、`cpp/libs/services/scene/battle/system/player_battle.cpp`、`docs/design/turn-based-battle-server.md`

**步骤**
1. 产品出死亡流程规格(写进 §15.4 替换基线段)。
2. 实现:结算 is_dead 分支(player_battle.cpp 应用结算处)改为进入「死亡态」组件 + 复活点传送/倒计时/惩罚扣减;PrepareBattle 对死亡态拒绝不变;player_revive.h 纯规则加惩罚参数并保留现有 4 条 PlayerReviveRuleTest。
3. robot battle_smoke 阵亡分支断言改为新流程。

**验收标准**
- 按规格:阵亡后状态/位置/惩罚可观察;登录补应用结算路径同规则;PlayerReviveRuleTest 与 battle_smoke 通过。

**验证方式**
```powershell
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle_engine_test
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml
```

**风险与注意**
依赖经验系统(P1-11,经验惩罚)与背包(P2-09,复活道具)两条前置线;产品未定前不要动基线。

---

### P2-D5 既有失败用例 `QdaoRunAssetTests.DirectionalRunStrips_HaveDistinctUpperBodyPosesAndGroundedFeet`(walk_N 与 walk_S 上半身各仅 5/8 帧不重复)(决策 D-07)

**领域** client-infra / client-art · **工作量** S · **状态** needs-decision

**背景与证据**
实测(PIL 对 `Assets/Resources/World/Characters/QdaoHeadbandBoy/walk_*.png` 按 512 格切、上 384 行 md5):N distinct upper=5 / lower=8;S upper=5 / lower=8;E、NE 均 8/8。用例 `Assets/Tests/EditMode/Tianyong/QdaoRunAssetTests.cs:49-50` 断言 `upperBodyHashes.Count == FrameCount(8)`,Directions 顺序 `{N,NE,E,SE,S,SW,W,NW}`(L17-18)→ N 上第一次 Assert 就抛,**S 的同样问题被遮住**。根因是修复工具设计使然:`E:/work/output/imagegen/qdao_headband_boy_walk_v3_fixed/fix_walk_gait.ps1` 头注「upper body … is kept untouched」,Order 里 `m0` 与 `0` 共用上半身 → 必然 3 对重复。PROGRESS.md:3530/3623/3692/3707/3785 每轮 EditMode 都是 N-1 绿,新红容易被当老红忽略。注意 `E:/work/mmorpg-client/sync_qdao_walk_to_resources.ps1` 默认 `-Source` 仍是 `qdao_headband_boy_run_v1`(不是 v3_fixed;memory 说已指向新源与文件不符),文件名模式按 `…run_<DIR>_strip_4096x512_v1.png` 找,而 v3_fixed 目录里是 `…walk_<DIR>_strip…`,盲跑会把 v1 原稿覆盖回去。

**要改的文件**
- `Assets/Tests/EditMode/Tianyong/QdaoRunAssetTests.cs`、`Assets/Resources/World/Characters/QdaoHeadbandBoy/walk_N.png`、`walk_S.png`
- `E:/work/output/imagegen/qdao_headband_boy_walk_v3_fixed/fix_walk_gait.ps1`、`E:/work/mmorpg-client/sync_qdao_walk_to_resources.ps1`、`PROGRESS.md`

**步骤**
1. 拍板 A/B(§2 D-07)。
2. A:fix_walk_gait.ps1 新增 `-UpperBob` 参数,在 MirrorLegs 后对 m 帧上半身整体上移 1~2px;`-Source <N 母版> -Dest <N 输出> -SplitY 390 -Band 36 -Order "<原 Order>" -UpperBob 2`;S 用 -SplitY 388;核对脚底行仍在 38-42(用例 L42);然后 sync(先改脚本文件名模式或改名,显式传 `-Source E:/work/output/imagegen/qdao_headband_boy_walk_v3_fixed`)。PowerShell System.Drawing 要引用 System.Drawing.Primitives/System.Private.Windows.GdiPlus,避免 Graphics.FromImage。
3. B:`QdaoRunAssetTests.cs:49-50` 改 `Is.GreaterThanOrEqualTo(5)`,新增 `lowerBodyHashes`(384..512 行)断言 `Count == FrameCount`(步态确实交替),注释写明镜像手术复用上半身。
4. 无论 A/B:跑 EditMode 副本工程,8 向全过(特别是 S),PROGRESS.md 记「EditMode 100% 绿」并注明路线,更新 memory `qdao-walk-gait-mirror-fix` 的 sync 说明;门禁脚本写明 `-assemblyNames MmorpgClient.Tests.EditMode.Battle` 单跑 Battle。

**验收标准**
- EditMode 全量 passed==total、failed==0;A 路线 PIL 复测 N、S distinct upper == 8 且首个不透明行仍 38-42;B 路线下半身 8 帧互异断言存在并通过;PROGRESS.md 记录路线与原因。

**验证方式**
```powershell
python -c "from PIL import Image;import hashlib;im=Image.open('E:/work/mmorpg-client/Assets/Resources/World/Characters/QdaoHeadbandBoy/walk_N.png').convert('RGBA');print(len({hashlib.md5(im.crop((f*512,0,f*512+512,384)).tobytes()).hexdigest() for f in range(8)}))"
"C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe" -batchmode -nographics -projectPath E:\work\tmp\attribute_verify_project -runTests -testPlatform EditMode -testResults E:\work\tmp\editmode.xml -logFile E:\work\tmp\editmode.log; Select-String -Path E:\work\tmp\editmode.xml -Pattern 'result="Failed"'   # 期望 0 行;不加 -quit
git -C E:/work/mmorpg-client status
```

**风险与注意**
A 改美术资产,另一会话若在用同一批 walk 图会看到轻微变化;B 小心不要放松到「8 帧全同」也能过。

---

### P2-D6 Resources/Battle 115 张帧条强制 Uncompressed 进包(角色+怪物 ≈212 MiB 纹理),BattleArtCatalog 缓存只增不减(决策 D-08)

**领域** client-infra / client-art · **工作量** M · **状态** needs-decision

**背景与证据**
`Assets/Resources/Battle/`:Characters 88 + Monsters 18 + Fx 9 = 115 张 2048×256 帧条;磁盘 Characters 32M + Monsters 3.0M。`Assets/Editor/QdaoCharacterSpriteImporter.cs:18,31,35-37` 对 `Assets/Resources/Battle/` 全部 `Uncompressed`、`crunchedCompression=false`、`mipmapEnabled=false`、maxSize 4096 → RGBA32 每张 2 MiB,106 × 2 = 212 MiB(Fx 18 MiB);.meta DefaultTexturePlatform textureCompression:0。基线:最近播放器 `E:/work/tmp/livecap_player` 842 MB,`resources.assets.resS` 668 MB(含天墉城图块等);`tools/battle_art_gen/README.md:96-99` 与 PROGRESS.md:3777-3778 把「不落盘 W」当作省 212 MiB 的理由,剩余 E 向同量级。加载侧:`BattleUnitView.cs:466-657` 演出时才 Resources.Load(已按参战者懒加载),`BattleArtCatalog.cs:113-118` 静态字典缓存,`ResetCaches()`(:128)无任何调用方,战斗结束不释放;Packages/manifest.json 无 addressables。README「客户端接线要点」明确建议 RGBA32 关 Crunch(alpha 渐变会色带)。

**要改的文件**
- `Assets/Editor/QdaoCharacterSpriteImporter.cs`、`Assets/Scripts/UI/Ugui/Battle/BattleArtCatalog.cs`、`BattleUnitView.cs`、`BattleUiRoot.cs`(战斗结束回调)
- `Assets/Resources/Battle/Characters/**`、`Monsters/**`、`Assets/Editor/CrossZoneVerifyBuild.cs`(量基线)、`Packages/manifest.json`(选 B)
- `E:/work/xuanming-server-mmo/tools/battle_art_gen/README.md` L96-99 / L171-178、`PROGRESS.md`

**步骤**
0. 先量基线(两个数字进 PROGRESS):① 包体——CrossZoneVerifyBuild 出包后 `Get-ChildItem <out>/mmorpg_Data -Recurse | Measure-Object Length -Sum`,Editor.log BuildReport「Textures xx MB」;② 首局加载——`BattleArtCatalog.LoadTexture` 加 Stopwatch 日志,跑一局 1v1 记录首个 LoadCharacterAction 耗时与 `Profiler.GetTotalAllocatedMemoryLong()` 差值;运行一局后 `Texture.currentTextureMemory`。
1. C(止血,任何拍板下都做):战斗退出路径(BattleUiRoot 离开战斗 / BattleScreen 关闭)调 `BattleArtCatalog.ReleaseBattleStrips()`(遍历 s_strips/s_textures 中 `Battle/Characters|Monsters` 前缀项,`Resources.UnloadAsset(tex)`,Sprite.Create 出来的 Sprite 要 Destroy)后 `Resources.UnloadUnusedAssets()`。
2. A(压缩):Importer 加 `bool isBattleStrip = assetPath.StartsWith(BattleFolder+"Characters/") || StartsWith(BattleFolder+"Monsters/")`,对其 `textureCompression = CompressedHQ; crunchedCompression = true; compressionQuality = 80;`(PC BC7/DXT5,移动端 ASTC 6x6);Fx/UI/Buff/digits 保持 Uncompressed;Reimport 目录(`AssetDatabase.ImportAsset(path, ImportAssetOptions.ForceUpdate)`)。
3. B(Addressables 按 characterId 懒加载):Characters/Monsters 移出 Resources,`BattleArtCatalog.LoadTexture` 改 `Addressables.LoadAssetAsync`,BattleUnitView.Bind 等待,开局按本局 actor 列表预热;只在明确上移动端时做。
4. 重跑 run_showcase.ps1 与 run_crosszone_pair.ps1 -ShotDir,对照压缩前帧(`E:/work/tmp/showcase_shots`、`shots_live5`)看描边/alpha(重点 hit 闪白帧与 cast 光晕);前后包体、首局加载耗时写进 PROGRESS.md 与 battle_art_gen/README.md「客户端接线要点」。

**验收标准**
- PROGRESS.md 记录基线与修后:resources.assets.resS 体积、BuildReport 纹理体积、首局首次取图耗时。
- A:Characters/Monsters .meta textureCompression 非 0,纹理体积下降 ≥ 60%;截图对照无肉眼可见色带/描边崩坏(至少 3 角色 + 1 怪的 attack/hit 帧)。
- B:Resources/Battle 下无 Characters/Monsters,Addressables 分组按 characterId,首局只加载所需组(日志可见)。
- C:多局连打(≥3 局)后 `Texture.currentTextureMemory` / `GetTotalAllocatedMemoryLong()` 不随局数线性增长。

**验证方式**
```powershell
"C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe" -batchmode -nographics -quit -projectPath E:/work/tmp/<copy> -executeMethod MmorpgClient.Editor.CrossZoneVerifyBuild.Build -logFile <log>; Get-ChildItem <out>/mmorpg_Data -Recurse | Measure-Object Length -Sum; Select-String -Path <log> -Pattern 'Textures\s+\d'
pwsh -File <tools>/run_showcase.ps1 -ShotDir E:/work/tmp/showcase_shots_compressed   # 与 E:/work/tmp/showcase_shots 同 marker 帧对比
pwsh -File E:/work/mmorpg-client/tools/run_crosszone_pair.ps1 -ShotDir E:/work/tmp/shots_compressed
cd E:\work\mmorpg-client; git status --short Assets/Resources/Battle/**/*.meta
```

**风险与注意**
Uncompressed 是美术规格明确要求(README「preserve crisp edges」、battle-art-prompts.md §3),改压缩属于放松规格,要美术/产品确认;Addressables 改变 BattleUnitView 同步取图假设(演出首帧等待);两条路线都让 .meta 大面积变更,与并行会话冲突概率高,动手前 git pull 并单独提交。

---

### P2-D7 右上角色卡数量策略未定:MaxCards=4 使 5v5 只显 4 张、1v1 只 1 张,规格写「两张(自己+队长/宠物)」(决策 D-09)

**领域** client-presentation · **工作量** S · **状态** needs-decision

**背景与证据**
`BattleHud.cs:299` `public const int MaxCards = 4;`;`BattleHudLogic.cs:81-102` PartyCardOrder(自己第一,队友按 id 升序,截断 maxCards);`BattleHudLogicTests.cs:94-108` 断言 maxCards:4 截断;`turn-battle-presentation.md:35`「右上两张角色卡(头像 + 等级 + 红蓝条,自己与队长/宠物)」;帧 show_0001(5 人只 4 卡,缺玄石道人)、A_0000(1 卡)。宠物系统不存在,规格的「自己+宠物」无法落地。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleHud.cs`、`BattleHudLogic.cs`、`Assets/Tests/EditMode/Battle/BattleHudLogicTests.cs`、`BattleUiLayoutTests.cs`、`docs/design/turn-battle-presentation.md`

**步骤**
1. 拍板 A/B(§2 D-09)。
2. 改 `BattleHud.cs:299` 常量 + `BattleHudLogicTests.cs:94-108` 期望值。
3. BattleUiLayoutTests 的 InteractiveRects 自动按 MaxCards 产出卡矩形,重跑确认所有分辨率可见;B 时与右上计时环(BattleScreen.TimerRect)复核不重叠。
4. 结果记入 turn-battle-presentation.md §1/§6。

**验收标准**
- 5v5 帧 show_0001 右上卡数 = 拍板值;1v1 帧 A_0000 = min(拍板值, 队伍人数);EditMode BattleHudLogicTests / BattleUiLayoutTests 全绿。

**验证方式**
```powershell
# Unity -runTests EditMode(副本工程)
pwsh -File <tools>/run_showcase.ps1   # show_0001_start_5v5.png
```

**风险与注意**
选 B 时角色卡列延伸到 y≈520,要确认不压右上计时环。

---

### P2-D8 角色外观按 actor_id 哈希从 22 套里挑,与建角职业/性别无关;接外观表要先把 class_id/gender 带进 BattleActorState(决策 D-10)

**领域** client-art / client-presentation · **工作量** M · **状态** needs-decision

**背景与证据**
`BattleArtCatalog.cs:173-178` CharacterIdFor → `BattleHudLogic.PortraitIndexFor(actor.ActorId, 22)`(注释「外观表接入后改为按 class/appearance 分派」);`proto/battle/battle_data.proto:62-83` BattleActorState 无 class_id/gender;`user_accounts.proto:9` class_id 存账号级;`turn-based-battle-server.md:496`「class_id 未随 PlayerAllData 下发 scene」(P1-10);CHARACTER_MANIFEST/meta.json 已带 gender/class_tag/role_cn(`characters.go:1020-1035` charTags 按文件名关键词推断,含 unknown);客户端 `RoleFlowUi.cs:27` Classes 表与 `GameClient.cs:449` CreatePlayerRequest{ClassId,Gender} 说明客户端只知道自己的选择;`generated/tables/Class.json` 9 个职业无名称/性别列。

**要改的文件**
- `proto/battle/battle_data.proto`、scene PrepareBattle 填快照处(按 turn-based-battle-server.md §15.4 定位)
- `Assets/Scripts/UI/Ugui/Battle/BattleArtCatalog.cs`、`BattleHudLogic.cs`、`Assets/Resources/Battle/Characters/CHARACTER_MANIFEST.json`、`Assets/Tests/EditMode/Battle/BattleHudLogicTests.cs`

**步骤**
1. 拍板选项 1 后:BattleActorState 加 `uint32 class_id = 21; uint32 gender = 22;`,`dev.bat gen`/proto 重生(C++/Go/C#,客户端 `tools/gen_proto.ps1 -ProtoRoot E:\work\xuanming-server-mmo`);scene 在构建战斗快照处从 PlayerAllData/账号数据填充(先解 P1-10);battle 节点原样转发。
2. 客户端 `BattleArtCatalog.CharacterIdFor(actor)`:若 actor.ClassId>0,用 Class id → class_tag 映射表(手写 Dictionary,与 Class.json 9 行对齐,策划确认)在 CHARACTER_MANIFEST.characters 筛 class_tag==tag && gender==actor.Gender 的候选,再 PortraitIndexFor(actor_id, 候选数) 稳定挑一套;否则回退哈希。头像 LoadPlayerPortrait 走同一函数。
3. EditMode BattleHudLogicTests 加:同 class/gender 的两个 actor_id 只在候选集内变化;class_id=0 回退哈希与旧实现一致。
4. 选项 2:只改客户端,GameClient 保存本人建角 ClassId/Gender,`CharacterIdFor` 对 ActorId==本人 player_id 用映射,其他哈希;文档写清局限。

**验收标准**
- 选项 1:同一账号在任意局、任意观战端形象与职业标签一致,对手同理;EditMode 用例过。选项 2:本人形象与建角职业一致;PROGRESS 记明对手/观战仍哈希。

**验证方式**
```powershell
# C++:MSBuild 串行 /m:1 重编 battle/scene 后经 WMI 重拉;cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml  → BattleStartS2C 的 actors[].class_id 非 0
pwsh -File <tools>/live_capture.ps1   # 双播放器截帧,核对两侧同一玩家形象一致且与 Classes 表选择一致
# Unity EditMode(副本工程,-assemblyNames MmorpgClient.Tests.EditMode.Battle,不加 -quit)
```

**风险与注意**
依赖 P1-10 先落地;proto 增量要三语言同步重生并保持消息号;22 套立绘 class_tag 由文件名关键词推断,class→class_tag 映射需策划确认;与并行 server-cpp 会话协调避免同时 regen。

---

## 5. P3(25 条:14 待办 + 11 待决策)

### P3-01 Go/Java 镜像构建慢:Dockerfile.go-svc 无 go mod cache mount、build-all 串行;Java 基础镜像国内镜像源只在口头 runbook

**领域** k8s-deploy · **工作量** S · **状态** open

**背景与证据**
`deploy/k8s/Dockerfile.go-svc:83-89` `RUN GOPROXY=${GOPROXY} go mod download && ... go build`,全文无 `--mount=type=cache`(已用 BuildKit `--build-context`,cache mount 可用);README.md:352-355「go mod download 每个服务重下一遍;temurin 基础镜像从 Docker Hub 直拉约 100KB/s…docker.1ms.run/library/eclipse-temurin:23-jdk 可用,拉完 docker tag 成官方名」;`Dockerfile.java-svc:14、:26` `FROM eclipse-temurin:23-jdk` / `:23-jre-alpine` 无 ARG;`java_svc_image.ps1:57-59` 注释承认没有 ARG;`go_svc_image.ps1:147-186` Invoke-BuildAll foreach 串行。六个 Go 服务各自 go mod download 一遍相同依赖。

**要改的文件**
- `deploy/k8s/Dockerfile.go-svc`、`Dockerfile.java-svc`、`tools/scripts/go_svc_image.ps1`、`java_svc_image.ps1`、`deploy/k8s/README.md`(:352-355)

**步骤**
1. `Dockerfile.go-svc:83` 改 `RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build GOPROXY=${GOPROXY} go mod download && CGO_ENABLED=0 ... go build ...`(首行 `# syntax=docker/dockerfile:1.4`;go_svc_image.ps1 顶部 `$env:DOCKER_BUILDKIT='1'`)。
2. `Dockerfile.java-svc` 加 `ARG JDK_IMAGE=eclipse-temurin:23-jdk` / `ARG JRE_IMAGE=eclipse-temurin:23-jre-alpine`,FROM 改 `${JDK_IMAGE}`/`${JRE_IMAGE}`;maven 阶段 `--mount=type=cache,target=/root/.m2`。`java_svc_image.ps1` 加 `-BaseImageMirror`(非空时 `--build-arg JDK_IMAGE=<mirror>/library/eclipse-temurin:23-jdk …`);README:353-355 改为引用该参数。
3. `go_svc_image.ps1 Invoke-BuildAll` 可选 `-Parallel`(`ForEach-Object -Parallel -ThrottleLimit 3`),默认串行。
4. `docker builder prune -f` 后 build-all 计时两次,记录到 README;README:352 改「已加 cache mount」。

**验收标准**
- 第二次 build-all 各服务 go mod download 显示 CACHED 或秒级;总耗时明显下降(记录前后数值)。
- `java_svc_image.ps1 -Command build -BaseImageMirror docker.1ms.run -DryRun` 打印含 `--build-arg JDK_IMAGE=docker.1ms.run/library/eclipse-temurin:23-jdk`;不传时与现状一致。
- kind 重新 load 后 B 档验收清单通过。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; Measure-Command { pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Tag t1 }; Measure-Command { pwsh -File tools/scripts/go_svc_image.ps1 -Command build-all -Registry local -Tag t2 }
pwsh -File tools/scripts/java_svc_image.ps1 -Command build -Registry local -BaseImageMirror docker.1ms.run -DryRun
docker buildx du | Select-String 'go/pkg/mod'
```

**风险与注意**
cache mount 在 GitHub Actions 默认不持久;镜像源可用性随时变(README 记录 docker.m.daocloud.io 当日 unavailable);并行构建可能吃满 Docker Desktop 内存。

---

### P3-02 match 旧队列迁移 `migrateLegacyQueues`(每 60s SCAN SharedRedis match:queue:*)只服务升级窗口,文档要求下一版本删除

**领域** server-go · **工作量** S · **状态** open

**背景与证据**
`go/match/internal/logic/matcher.go:91-94` `legacyQueueSweepInterval = time.Minute`、`:106/:109-111` 启动与每分钟调用、`:130-165` migrateLegacyQueues/migrateLegacyQueue;`keys.go:86-97` legacyMatchQueueScanPattern/legacyMatchQueueKey;`queue.go:170、:448` 注释;测试 `crosszone_test.go:283-289`、`rating_match_test.go:329`、`review_fix_test.go:340-377`;`cross-zone-matchmaking.md:297`「下一版本删除」、§11.9 L428-429。跨 zone 匹配 2026-09-02 已上 main 并全按新格式跑,共享库全键空间 SCAN 每分钟一次是纯开销且是 SharedRedis 上唯一的 SCAN。

**要改的文件**
- `go/match/internal/logic/matcher.go`、`keys.go`、`queue.go`、`crosszone_test.go`、`rating_match_test.go`、`review_fix_test.go`、`docs/design/cross-zone-matchmaking.md`(§10 L297、§11.9 L428-429)

**步骤**
1. 先确认每个环境 SharedRedis 无残留:本地 `docker exec redis redis-cli --scan --pattern 'match:queue:*'`;kind `kubectl -n mmorpg-infra exec deploy/redis -- redis-cli --scan --pattern 'match:queue:*'`,为空才继续。
2. matcher.go 删 legacyQueueSweepInterval、StartMatcherLoop 里的调用与 lastSweep,删两函数及无人再用的 import。
3. keys.go 删两常量,86-92 注释改「历史:旧格式已在 <版本> 前迁移完毕」;queue.go:170/:448 注释同步。
4. 删/改三份测试的迁移用例。
5. 文档 §10 L297 划掉并注明删除版本;§11.9 第 2 条删除;PROGRESS.md 记「回滚到含 migrateLegacyQueues 之前版本不再受支持」。

**验收标准**
- `grep -rn 'migrateLegacyQueues\|legacyMatchQueue\|match:queue:' go/match` 仅剩历史注释或为空;`go test ./... -count=1` 全绿;match 运行 5 分钟 MONITOR 不再出现 SCAN;cross_zone 冒烟仍 OK。

**验证方式**
```powershell
docker exec redis redis-cli --scan --pattern 'match:queue:*'
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\match; go build ./... ; go test ./... -count=1
# docker exec redis redis-cli MONITOR 采样 90s | Select-String 'SCAN'
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke_cross_zone.yaml
```

**风险与注意**
若任一环境仍在跑 2026-09-02 之前的 match 实例,删掉后其入队玩家永久卡队列;删除后不能再向旧版本回滚而不清队列。

---

### P3-03 切磋(PVP_CHALLENGE)应战后 gather 开局失败只用 accepted=false 兜底,无专用提示码

**领域** server-go · **工作量** S · **状态** open

**背景与证据**
`go/match/internal/logic/challengelogic.go:274-282` RunChallengeGather 失败分支注释「一期用 accepted=false 的结果消息兜底提示双方,产品化提示二期」;`proto/match/match_service.proto:135-139` ChallengeResultS2C{challenge_id, accepted, responder_id} 无失败原因;`go/match/internal/constants/errors.go:1-30` 新错误码流程(Tip.xlsx `//match_error base=16000` 加行 → 导表 → errors.go 加引用 → errors_test.go tipCodes());`errors.go:50-63` 七个 ErrChallenge* 均无「开局失败」。客户端收到 accepted=false 按「对方拒绝/超时」展示,语义错误。

**要改的文件**
- `data/tip/Tip.xlsx`(//match_error 组)、`go/match/internal/constants/errors.go`、`errors_test.go`、`go/match/internal/logic/challengelogic.go`(274-282、309)
- `proto/match/match_service.proto`(135-139)、`E:/work/mmorpg-client/Assets/Scripts/Proto/Generated/`(C# regen)与切磋结果 UI 脚本

**步骤**
1. Tip.xlsx //match_error 加行 `kMatchChallengeGatherFailed | 切磋开局失败,请稍后再试`,重跑导表器,确认 tip_text.json 与 `go/shared/generated/pb/table/match_error_tip.pb.go` 多出该枚举。
2. errors.go 加 `ErrChallengeGatherFailed = uint32(table.MatchError_kMatchChallengeGatherFailed)`;errors_test.go tipCodes() 加一行(6557eb94a 先例)。
3. proto ChallengeResultS2C 加 `uint32 tip_id = 4; // 非 0 时客户端按 TipInfo 文案提示`;regen Go/C++/C#。
4. `challengelogic.go:309` pushChallengeResult 加 tipId 参数;`:279-281` 失败分支传 ErrChallengeGatherFailed,其它调用点 0。
5. 客户端切磋结果 handler:tip_id != 0 时走 Tip 文案(与 P1-14 的 TipText 结合)。
6. PROGRESS.md 记一条;文档切磋节补充。

**验收标准**
- errors_test.go 全绿;本地两机器人切磋、停掉 battle 节点让 CreateBattle 失败,双方收到 `ChallengeResultS2C{accepted=false, tip_id=ErrChallengeGatherFailed}`;正常拒绝路径 tip_id=0;客户端显示「切磋开局失败」。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\match; go test ./internal/constants/ ./internal/logic/ -count=1
grep -n ChallengeGatherFailed E:/work/xuanming-server-mmo/generated/tables/tip_text.json E:/work/xuanming-server-mmo/go/shared/generated/pb/table/match_error_tip.pb.go
# 停 battle 节点后跑切磋冒烟,match 日志 Select-String '应战后开局失败'
```

**风险与注意**
proto 增字段需三端 regen 与客户端出包;C# regen 注意 Vector3/Transform 命名冲突(memory `mmorpg-client-compile-verify`);替代是走 TipInfoMessage 单独推送(pushChallengeResult 已有 gate 路由可复用)。

---

### P3-04 `player_battle.cpp` 头部「以下生成产物当前尚未生成」注释已过时,「见 open_issues」引用不存在的文件

**领域** server-cpp · **工作量** S · **状态** open

**背景与证据**
`cpp/libs/services/scene/battle/system/player_battle.cpp:49-58` 仍写「以下生成产物当前尚未生成,proto 重生成后出现」,其后 8 个头文件均已存在并参与编译(scene.lib 09-05 重编 OK);`:430`「现状见 open_issues」、`:442`、`:992/1000` 引用的 open_issues 仓库内无此文件。

**要改的文件**
- `cpp/libs/services/scene/battle/system/player_battle.cpp`

**步骤**
1. 删 `:49-50` 两行注释,保留 include;`:430/:442/:992/:1000` 的「见 open_issues」改「见 turn-based-battle-server.md §15.4」。
2. 串行重编 scene 库确认零错(纯注释改动,可不重拉)。

**验收标准**
- 文件内不再出现「尚未生成」「open_issues」;scene.lib 编译零错。

**验证方式**
```powershell
Select-String E:/work/xuanming-server-mmo/cpp/libs/services/scene/battle/system/player_battle.cpp -Pattern '尚未生成|open_issues'
msbuild E:/work/xuanming-server-mmo/game.sln /t:scene /p:Configuration=Debug /p:Platform=x64 /m:1
```

**风险与注意**
无。

---

### P3-05 PROGRESS.md 两个 09-03 条目标题仍写「—— 进行中」,实际 09-05 已闭环;缺集中登记「二期余项」的小节

**领域** docs-process · **工作量** S · **状态** open

**背景与证据**
`PROGRESS.md:3497`「## 2026-09-03 二期全面推进(…)—— 进行中」、`:3533`「- **进行中(resume)**:…」;`:3538`「## 2026-09-03(续)二期收口:… —— 进行中」、`:3554`「- **进行中**:跨区冒烟 + 二期运行时核对…」。闭环证据 `:3794-3796`「**最终实机验收(2026-09-05 02:56)**…至此本轮全部目标闭环」。`grep -n 余项|待办` 在 3490 行后只有 :3639/:3723/:3735/:3796 散落,无集中小节。AGENTS.md 会话启动门禁要读 PROGRESS.md,标题「进行中」会误导后续会话。

**要改的文件**
- `E:/work/xuanming-server-mmo/PROGRESS.md`

**步骤**
1. `:3497` 「—— 进行中」改「—— 已闭环(见 2026-09-04/09-05 条目)」;`:3533` 改「后续(已完成):…→ 见 :3610 / :3631 / :3794」。
2. `:3538` 同改;`:3554` 改「后续(已完成):跨区冒烟见 :3794 CROSS_ZONE_PAIR_PASS;客户端表现层见 :3610」。
3. 文件末尾(:3998 之后)新增「## 2026-09-05 二期余项清单(集中登记)」,逐条列本文档编号并指向 `docs/design/handoff-backlog-2026-09-05.md`(本文件)。
4. 不改 `:3644-3645` 历史描述(176 张 {E,W}),只在新小节注明「现状为 E-only 88+18 张,见 :3742」。
5. 先 `git pull --rebase`,只做追加式修改(§1.6)。

**验收标准**
- `Select-String -Path PROGRESS.md -Pattern '—— 进行中'` 返回 0 条;末尾存在「二期余项清单」小节,每条带出处。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; Select-String -Path PROGRESS.md -Pattern '进行中' | ForEach-Object { "$($_.LineNumber): $($_.Line)" }
Select-String -Path PROGRESS.md -Pattern '二期余项清单' | Measure-Object
```

**风险与注意**
另一并行会话也在追加 PROGRESS.md,避免合并冲突。

---

### P3-06 美术契约文档与产物不一致:battle-art-prompts §3 / turn-battle-presentation §3、§5.2 仍写 {E,W} 双向、die/win、旧命名;MONSTER_MANIFEST 与 characters.go 引用不存在的「规格 8px」与 README「与规格的偏差」章节

**领域** docs-process / client-art · **工作量** S · **状态** partially-done

**已完成部分**:`tools/battle_art_gen/README.md:49、:93-100`「为什么不落盘 W」、「统一身高」、「-mode monsters」末段 IoU 说明已按 E-only 写;MONSTER_MANIFEST.json notes 第 2/5/6 条。

**背景与证据**(剩余部分):`docs/design/battle-art-prompts.md:64`「Characters/<characterId>/{idle,attack,cast,hit,die,win}_{E,W}_strip.png」、`:65` Monsters 同、`:10-11`「只做 E 与 W 两向…」、`:13` 含 die/win 帧率;`turn-battle-presentation.md:94-96` §5.2「只做 E 与 W 两向…die 6 帧、win 4 帧…」;`:73-75` §3 仍写 `Resources/Battle/Monsters/<id>.png` 单图与 `{attack,cast,hit,die}_strip.png` 旧命名。磁盘:`*_W_strip.png` 0 个,`die_*/win_*` 0 个;PROGRESS.md:3742-3743「删掉 88+18 张 W 帧条…最终产物 Characters 88 张 E 向 + Monsters 18 张」。8px:`MONSTER_MANIFEST.json` notes「battle-art-prompts.md 里写的 8px 与该 pivot 对不上」,`characters.go:13`「≈ 20px(不是 8px,见 README「与规格的偏差」)」——`grep -n 8px docs/design/battle-art-prompts.md` 为 0(含 143ecca96 版本),`grep 偏差 README.md` 为 0:两个悬空引用;规格实际只写 `pivot (0.5, 0.08)`(:12、:74),与 `BattleArtCatalog.cs:107 FeetPivot`、`characters.go:43 charFeetFromBottom = 20`、manifest `feet_margin_px: 20` 一致(`turn-battle-presentation.md:34` 的「约 8px」是受击后仰位移)。

**要改的文件**
- `docs/design/battle-art-prompts.md`(:10-13、:60-75)、`docs/design/turn-battle-presentation.md`(:73-75、:93-97、:115)
- `tools/battle_art_gen/characters.go`(:13)、`monsters.go`(生成 notes 的字符串,搜「8px」)、`README.md`;`Assets/Resources/Battle/MONSTER_MANIFEST.json`(工具重生成,不手改)

**步骤**
1. battle-art-prompts.md :64-65 改 `Characters/<characterId>/{idle,attack,cast,hit}_E_strip.png   # 2048×256;W 不落盘,客户端 LoadDirectionalStrip 缺 W 取 E 并 Mirrored 翻转;die/win 未生成,由客户端程序化动作代替(命名保留 *_W_strip / die_ / win_ 供将来)`;Monsters 行 `{idle,attack,hit}_E_strip.png`,注明 MONSTER_MANIFEST.json 为权威清单;:10-11 改「只需产出 E 向;W 运行时镜像」;:13 die/win 标「(预留,当前未产出)」;§1 表补「怪物 attack 命中帧见 MONSTER_MANIFEST(当前 index 4)」;:74 后加「脚底基线 = 底边上方 round(0.08×256)=20px(第 235 行),与 FeetPivot 同源;工具常量 charFeetFromBottom=20」;§4 加一句指向 README「统一身高」(改幅度会改全体尺寸)。
2. turn-battle-presentation.md §3 :73-75 改 `Battle/Monsters/<id>/<action>_E_strip.png`、`Characters/<id>/<action>_E_strip.png`;§5.2 :94-96 改与上面一致并引用 README「为什么不落盘 W」;§6 :115 按本清单状态更新。
3. `characters.go:13` 改「≈ 20px,与 battle-art-prompts.md §3 一致」;monsters.go 删「battle-art-prompts.md 里写的 8px 与该 pivot 对不上」半句;`go run . -mode monsters` 重生成 MONSTER_MANIFEST.json——用 `git status` 确认只有 manifest 变化,帧条二进制若变了说明生成不确定,停下来记录。
4. PROGRESS.md 不改 :3644-3645 历史,在 P3-05 新小节注明现状。

**验收标准**
- `Select-String docs/design/battle-art-prompts.md,docs/design/turn-battle-presentation.md -Pattern '\{E,W\}|E/W 两向|E 与 W 两向|Monsters/<id>.png'` 为 0 条;两文档写出「W 运行时镜像」「die/win 未产出」。
- `Select-String -Path tools/battle_art_gen/*.go, MONSTER_MANIFEST.json -Pattern '8px'` 为 0 条。
- 路径契约与 `Get-ChildItem -Recurse Assets/Resources/Battle/Characters/01_ice_sword_girl` 实际文件名一一对应;重生成后 `git status --short Assets/Resources/Battle` 只列 MONSTER_MANIFEST.json。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; Select-String -Path docs/design/battle-art-prompts.md,docs/design/turn-battle-presentation.md -Pattern '\{E,W\}|两向|die|win|8px|Monsters/<id>.png'
Get-ChildItem -Recurse E:/work/mmorpg-client/Assets/Resources/Battle -Filter '*_W_strip.png' | Measure-Object; Get-ChildItem -Recurse E:/work/mmorpg-client/Assets/Resources/Battle -Include 'die_*','win_*' | Measure-Object
. E:\work\tools\buildenv.ps1; cd E:/work/xuanming-server-mmo/tools/battle_art_gen; go test ./...; go vet ./...; go run . -mode monsters; git -C E:/work/mmorpg-client status --short Assets/Resources/Battle
```

**风险与注意**
重生成怪物帧条若非逐字节确定会产生 18 张 PNG 无意义 diff——只提交 manifest;MONSTER_MANIFEST.json 是工具产物,必须改 monsters.go。

---

### P3-07 cast 脚底光晕贴 256 画布底边被硬切,且 verifyCharStrip 不扫底边 y=255

**领域** client-art · **工作量** S · **状态** open

**背景与证据**
`tools/battle_art_gen/characters.go:822-829` drawCastGlowBack:cy = charBaselineRow+1 = 236,AddEllipseGlow 纵向半径 15+11g(g=1 → 26px)→ 262 > 255;`:1571-1585` verifyCharStrip 溢出扫描只查 x=0 / x=2047 与 y=0,不查 y=255;PROGRESS.md:3746;`turn-battle-presentation.md:115`。cast 第 3-5 帧脚底出现平直切边。

**要改的文件**
- `tools/battle_art_gen/characters.go`

**步骤**
1. drawCastGlowBack / drawCastGlowFront(:822/:840):纵向半径改 `min(15+11g, 255-cy-1)`≈18,或 cy 上移到 charBaselineRow-6 并半径 ≤ 19;横向半径不变。
2. verifyCharStrip(:1571-1585)增加最后一行 y=charCell-1 与每格左右格间边(x = f*256 与 f*256+255)的 alpha ≥ 16 计数进 warns。
3. 重跑 `go run . -mode characters`(seed 不变其他帧逐字节不变)。

**验收标准**
- cast_E_strip.png 第 3-5 帧 y=255 行 alpha 全 0;自检对 cast 无边缘告警;其余动作产物逐字节一致(mmorpg-client `git diff --stat` 只有 22 个 cast_E_strip.png)。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode characters
python -c "from PIL import Image;im=Image.open('E:/work/mmorpg-client/Assets/Resources/Battle/Characters/01_ice_sword_girl/cast_E_strip.png');print(max(im.getpixel((x,255))[3] for x in range(2048)))"
cd E:\work\mmorpg-client; git status --short Assets/Resources/Battle/Characters | Measure-Object
```

**风险与注意**
无;纯工具侧。

---

### P3-08 attack/cast 下蹲帧用正向 Dy 平移实现,脚底掉到 FeetPivot 基线之下 3~6px

**领域** client-art · **工作量** S · **状态** open

**背景与证据**
`characters.go:585-587` attack 帧 1/2 `fd(-4, 1, …)` `fd(-6, 3, …)`;`:612-614` cast 帧 1/2 `fd(0, 3, …)` `fd(0, 6, …)`(fd 第二参数 Dy,`:538` 注释「Dy 向下为正」);脚底锚点在 235 行,Dy=6 → 241 行;客户端 FeetPivot (0.5,0.08) 固定(`BattleArtCatalog.cs:107`)。下蹲帧脚底沉到地面之下,地台光带上表现为脚插进地里。

**要改的文件**
- `tools/battle_art_gen/characters.go`

**步骤**
1. attack 帧 1/2、cast 帧 1/2/3 的 Dy 改 0(或 ≤ 0),下蹲感只靠 Sy(0.968/0.918/0.952)与 shear;要保留重心下沉可把 Sy 再压 1~2%。
2. 工具级保护:charFitsAt(:928)下边界从 `py > charCell` 收紧为 `py > charBaselineRow+0.5`(对非 GroundClamp 帧),任何正向 Dy 会被求解器拒绝而非静默通过。
3. 重跑 `-mode characters`,统一身高仍 175。先做本条再做 P2-16(只减不加幅度,不会压低身高)。

**验收标准**
- attack/cast 每帧最低不透明行 ≤ 235;idle 首帧仍恰好 235;fitted_height 仍 175。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode characters
python -c "from PIL import Image;im=Image.open('E:/work/mmorpg-client/Assets/Resources/Battle/Characters/01_ice_sword_girl/cast_E_strip.png').convert('RGBA');print([max(y for y in range(256) for x in range(256) if im.getpixel((f*256+x,y))[3]>=8) for f in range(8)])"
```

**风险与注意**
与 P2-16 同时改动作表会一起影响 fit 求解。

---

### P3-09 CHARACTER_MANIFEST.json file_count 少计自身 1 个(110 vs 实际 111),与 ART_MANIFEST 口径相反

**领域** client-art · **工作量** S · **状态** open

**背景与证据**
`characters.go:1360-1365`:`writeJSON(mpath, &mf)` 之后才 `mf.FileCount++`;实测 file_count=110,`find Characters -type f ! -name *.meta | wc -l` = 111(88 strip + 22 meta.json + 1 manifest);`main.go:208` ART_MANIFEST 的 FileCount 显式 `+2` 含自身与 digits_meta.json。

**要改的文件**
- `tools/battle_art_gen/characters.go`(及 monsters.go :1325 计数口径核对)

**步骤**
1. `:1360` 把 `mf.FileCount++` 挪到 writeJSON 之前(统一「含清单自身」),README「产出」节写明。
2. 核对 monsters.go 写 MONSTER_MANIFEST 时的计数口径,不含自身也统一。
3. 重跑 `-mode characters`(其它文件逐字节不变)。

**验收标准**
- CHARACTER_MANIFEST.json file_count == Characters/ 下非 .meta 文件总数(111)。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode characters
python -c "import json,glob,os;d=json.load(open('E:/work/mmorpg-client/Assets/Resources/Battle/Characters/CHARACTER_MANIFEST.json',encoding='utf-8'));n=len([f for f in glob.glob('E:/work/mmorpg-client/Assets/Resources/Battle/Characters/**/*',recursive=True) if os.path.isfile(f) and not f.endswith('.meta')]);print(d['file_count'],n)"
```

**风险与注意**
无。

---

### P3-10 「防」徽标显示在血条右端黑底块、buff 图标排在血条上方,规格为血条左侧小格

**领域** client-presentation · **工作量** M · **状态** open

**背景与证据**
`BattleUnitView.cs:261-266` `_badgePlate = CreatePanel("Badge", _plate, barX + BarWidth + 6f, 58f, 44f, 24f, PanelBg)`(条右侧);`:253` `_buffRow = CreateRect("Buffs", _plate, barX, 4f, ...)` + LayoutOverhead(:276-285)`buffY = hpY - 4 - (BuffIconSize+2)`(条上方);`:353/:689` ShowBadge("防");`turn-battle-presentation.md:24`「头顶两条细条…左侧有小 buff 图标格」;帧 show_0030_tick.png 雪花/紫箭头/红圈图标悬在血条正上方。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleUnitView.cs`、`Assets/Resources/Battle/Buff/`

**步骤**
1. LayoutOverhead:_buffRow 改与 HP 条同高、放条左侧:`x = barX - (n*(BuffIconSize+3)) - 6`,y = hpY;BuffIconSize 缩到 HpBarHeight+2+MpBarHeight。
2. ShowBadge("防")改为向 _buffRow 追加「防」图标(Buff/ 现有金色盾图或新增 `Battle/Buff/defend.png`),HideBadge 时移除;_badgePlate 保留给「亡/逃」或一并迁移。
3. 图标行向左生长注意后排单位靠屏幕左侧越界:把行宽计算抽成静态函数,BattleStageTests 加「buff 行左缘 ≥ 0」纯计算断言。
4. 演出台 r3(BUFF_ADD)/r1(防御)帧核对。

**验收标准**
- show_0005(i-defend)雷破天血条左侧出现「防」小格,右端无黑底文字块;show_0030 三个 buff 图标排在血条左侧、与条同高。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1   # show_0005_r1_*.png、show_0030_tick.png
# Unity -runTests EditMode(BattleStageTests)
```

**风险与注意**
左侧生长会靠近相邻单位名牌;群攻多 buff 时限 MaxBuffIcons 控宽。

---

### P3-11 出生光环尺寸过大(300×Scale)、渲染在覆盖层盖住立绘下半身;敌方只见一环需复核是否为淡出错峰

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`BattlePresenter.cs:184-215` PlayEntrance:`size = 300f * Max(0.6f, view.Scale)`,`CreateImage("SpawnRing", _overlayLayer, ...)`(覆盖层在单位之上),delay = 0.25+0.05*index,放大 0.45s → 停 0.35s → 淡出 0.5s;`BattleUnitView.cs:190-194` 另有单位自带 220×110 `_ring`(只用于目标高亮);帧 show_0005_r1_*.png:我方 5 个太极盘互相重叠并盖住白芷仙子/玄石道人/雷破天下半身与脚下名字,敌方只有血罗刹脚下淡红环。槽位间距只有 170(BattleStage.RowStep),300 必然重叠;敌方只见一环很可能是截帧时刻(开局 1.6s 后)恰好落在我方尚在、敌方已淡出/未起的窗口。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattlePresenter.cs`、`BattleUnitView.cs`、`BattleScreen.cs`

**步骤**
1. PlayEntrance:size 改 `≈ 1.1 × 单位宽`(立绘宽 ≈ 256×Scale → size ≈ 190*Scale),高宽比 0.5。
2. 环创建在舞台层单位之下:改用 `_stageRoot` 并 `rect.SetAsFirstSibling()`,或复用 view 自带 _ring(新增 `view.PlaySpawnRing(delay)` 在 BattleUnitView 内做同一套 tween,天然在立绘下层、跟随 scale)。
3. 复核不对称:PresentationShowcase 把 start 截图改为 Push 后延迟 0.6/1.0/1.4s 三张(P2-24),确认两队各 5 环都出现;若仍缺,检查 BattleScreen.RebuildUnits 构建 _views 的顺序与 index delay。

**验收标准**
- 开局 0.6~1.0s 帧:两队 10 个环各自可见、互不重叠、不盖住立绘与脚下名字;环随后排 0.85 / 敌方 0.95 缩放。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1   # show_0002~0004 tick 帧(开局后 0.25s 间隔)
```

**风险与注意**
复用 view._ring 时要与目标高亮(SetHighlight)互斥,入场期间不会有目标选择,风险低。

---

### P3-12 回合框下的状态提示(请选择行动/回合播放中…)与顶中「战斗开始!」几乎不可读

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`BattleScreen.cs:214-215` `_hintText = CreateText("Hint", _hudRoot, 680f, 1000f, 1200f, 40f, "", 22f, QdaoUguiTheme.StatusCream, Center)`,`:900-907` RefreshActionBar 写入提示;帧 show_0001/show_0015 左上「请选择行动」与顶中「战斗开始!」为淡色细字,A_0000(1600×900)同。录像 f_003 无此类提示。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`、`QdaoUguiTheme.cs`、`BattleScreenFx.cs`

**步骤**
1. 核对 QdaoUguiTheme.StatusCream 的 alpha;_hintText 改 Cream + `BattleUiWidgets.ApplyOutline(_hintText, 0.25f, 黑)`,字号 22→26。
2. 决定「请选择行动」是否保留:去掉则 RefreshActionBar(:906)WaitingAction 分支置空,只保留阵亡/逃离/自动战斗三条。
3. grep "战斗开始" 找顶中横幅创建处(BattleScreenFx 或 BattleScreen.Open :290 附近),字号 ≥40、加描边、停留 1.2s 后淡出。

**验收标准**
- show_0001「战斗开始!」原尺寸清晰可读;show_0015 左上提示要么不存在要么清晰。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1   # show_0001、show_0015
```

**风险与注意**
无。

---

### P3-13 伤害数字锚在受击者槽位 FootPosition,不跟随单位;拍长 0.9s < 数字寿命 1.2s,受击者下一拍冲锋时数字与人分离

**领域** client-presentation · **工作量** M · **状态** open

**背景与证据**
`BattleUnitView.cs:106` `HeadPosition => (FootPosition.x, FootPosition.y - ...)`,`:862-868` ShowNumber → `Numbers.Show(new Vector2(headPos.x + xOffset, headPos.y), ...)`(静态坐标);PlayAttackLunge(:457-475)tween 的是 `_root.anchoredPosition`,FootPosition 不变;`TurnPlan.cs:211` AttackSeconds=0.9,`DamageNumber.cs:21` LifeSeconds=1.2;帧 `E:/work/tmp/shots_live5/A/A_0010_tick.png`「-22」在原槽位上方而受击者已冲到敌人旁。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/DamageNumber.cs`、`BattleUnitView.cs`、`BattlePlateFollower.cs`(参考)、`Assets/Scripts/Game/Battle/Presentation/BattleSequencer.cs`

**步骤**
1. 方案 A(跟随):DamageNumberPool.Show 增加可选 `RectTransform follow`;Entry 记录 follow 与初始偏移,每帧用 `follow.anchoredPosition + offset + 上飘量` 更新;BattleUnitView.ShowNumber 传 `_root`(名牌 BattlePlateFollower 已有同类跟随可参考)。
2. 方案 B(节奏):BattleSequencer.Advance 在下一拍出手者 == 上一拍受击目标且上一拍有数字时,给下一拍加 0.3s 前置等待(TurnPlan 层加 `LeadInSeconds` 字段,纯 C# 可测)。
3. 二选一或同时做;A 更贴录像。实机 1v1(-shotInterval 0.2)连续帧验收。

**验收标准**
- A_00xx 连续帧中「-N」始终位于受击者当前立绘头顶 ±40px 内直到淡出。

**验证方式**
```powershell
pwsh -File E:\work\mmorpg-client\tools\run_crosszone_pair.ps1 -ExePath E:/work/tmp/livecap_player/mmorpg.exe -ShotDir E:/work/tmp/shots_live7 -ShotInterval 0.2   # 逐帧看 A 侧 tick 帧
# Unity -runTests EditMode(方案 B:TurnPlanTests 新增 LeadIn 用例)
```

**风险与注意**
方案 A 群攻多目标时每串数字各自跟随,错位避让(AvoidOffset)按初始坐标算即可。

---

### P3-14 MANA 事件方向靠 source==target 推断为耗蓝取负;服务端将来给他人回蓝会被显示成耗蓝(预防性)

**领域** client-presentation · **工作量** S · **状态** open

**背景与证据**
`TurnPlan.cs:575-580` MergeMana:`if (delta > 0 && who == beat.ActorId && ev.SourceId == who) delta = -delta;`;`:104` 注释;服务端目前只产出施法者自耗(`turn-battle-presentation.md:107` PresentationSkillManaCostEmitsManaEventInSkillGroup:value=消耗、target_mana_after=剩余);`TurnPlanTests.cs:418-462` 三个 Mana 用例均为自耗/目标附着。value 为 uint64 无符号,方向只能靠 target_mana_after 与当前蓝量差判断(与 TickIsHeal 同法)。

**要改的文件**
- `Assets/Scripts/Game/Battle/Presentation/TurnPlan.cs`、`Assets/Scripts/UI/Ugui/Battle/BattlePresenter.cs`、`Assets/Tests/EditMode/Battle/TurnPlanTests.cs`

**步骤**
1. TurnPlan 新增 `public static bool ManaIsGain(BeatTarget t, ulong currentMana) => t.HasManaAfter && t.ManaAfter > currentMana;`。
2. BattlePresenter 处理 TargetEffect.Mana / HasActorManaAfter 时(:296-299 附近)用 view.CurrentMana(若无该属性,BattleUnitView 加 `public ulong CurrentMana`)判断方向,回蓝 `+N` 蓝字,耗蓝 `-N`。
3. MergeMana 保持现有推断作为无 target_mana_after 时的回退。
4. TurnPlanTests 加「MANA source=A target=B、ManaAfter > 当前 → ManaIsGain=true」与「自耗 ManaAfter < 当前 → false」。

**验收标准**
- EditMode TurnPlanTests 新增用例通过;现有 Mana_* 三用例不变。

**验证方式**
```powershell
# Unity -runTests EditMode(副本工程)→ xml 中 TurnPlanTests
```

**风险与注意**
无;服务端行为不变。

---

### P3-D1 `GetQueueStatus.EstimatedWaitSeconds` 恒为 0(客户端可见半成品);评分不进客户端协议(决策 D-11)

**领域** server-go · **工作量** M · **状态** needs-decision

**背景与证据**
`go/match/internal/logic/getqueuestatuslogic.go:74-79` 注释「一期不做等待时长预估(需要历史凑单速率统计,二期随负载上报一起做)」并硬编码 `EstimatedWaitSeconds: 0`;`proto/match/match_service.proto:86` `uint32 estimated_wait_seconds = 2; // Server-estimated wait time` 已在协议中;`cross-zone-matchmaking.md:437`「评分不进客户端协议(GetQueueStatus 不回评分,EstimatedWaitSeconds 仍为 0)」。客户端若显示「预计等待」会误导。

**要改的文件**
- `go/match/internal/logic/getqueuestatuslogic.go`(74-79)、`matcher.go`(popGroup 弹组点)、`keys.go`(新增 wait 统计 key,带 {mq} tag)、`proto/match/match_service.proto`(:86)、`docs/design/cross-zone-matchmaking.md`(§11.10 L437)

**步骤**
1. 拍板 A/B(§2 D-11)。B:proto :86 注释加「未实现,恒 0;客户端勿显示」,客户端不渲染,完成。
2. A:matcher 弹组成功处对每个成员计算 now - ticket.EnqueuedAtMs,以 Lua 或 pipeline 写入 `match:{mq}:wait:<mode>:<config>`(LPUSH + LTRIM 32 或 EWMA hash),TTL 1h。
3. getqueuestatuslogic.go 读样本取中位数/EWMA,返回 `max(0, estimate - queuedSeconds)`;无样本仍 0。
4. 单测(miniredis):弹组两次后 GetQueueStatus 返回非 0 且随 queuedSeconds 增大递减到 0;keys_test.go CROSSSLOT 自检覆盖新 key。
5. 若同时公开评分:GetQueueStatusResponse 加 `double rating = 4`,regen Go/C++/C#,文档 §11.10 更新。

**验收标准**
- B:proto 注释与客户端不显示预估,文档一致。A:排队两组打完后第三名入队者 EstimatedWaitSeconds > 0;无样本队列回 0;CROSSSLOT 自检过。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\match; go test ./internal/logic/ -run 'QueueStatus|Wait' -count=1 -v
# 本地双机器人冒烟后 grpcurl -plaintext 127.0.0.1:50500 match.MatchService/GetQueueStatus(带 x-session-detail-bin)看 estimated_wait_seconds
```

**风险与注意**
等待估算受队列稀疏影响大(低峰期样本过期回 0),需明确「0 = 未知」语义;新增评分字段要动客户端协议与 UI。

---

### P3-D2 战斗/匹配预留产品功能(未开工):预组队 party_member_ids、ready check、3v3、对手 zone 展示、观战事件流延迟推送(决策 D-12)

**领域** server-go / server-cpp · **工作量** L · **状态** needs-decision

**背景与证据**
预组队:`joinqueuelogic.go:57-61`「一期只做单人入队…预组队(party_member_ids)二期接入」仅打日志;`match_service.proto:60` 字段已在;`cross-zone-matchmaking.md:438`「预组队 party_member_ids 仍一期忽略」。3v3:proto `:47` `MATCH_MODE_3V3 = 2 // 未开放`,`joinqueuelogic.go:88-95` default 回 ErrModeNotOpen,`rating.go:94-101` isRatedMode 只含 1V1/5V5。对手 zone 展示:`battle_data.proto:62-83` BattleActorState 无 zone 字段;`cross-zone-matchmaking.md:98`(D9)、`:294`。ready check / 观战延迟推送:`turn-based-battle-server.md:270-274`「仍预留」、`:334-337` §10.6「延迟缓冲留到有竞技需求时做」——MMR 落地(2026-09-03)后「无排位利益」前提已破:PVP 观战可同步看到对手指令,小号偷看有收益;go/match grep `ready.check|delay|延迟推送` 为空;PROGRESS.md:3406。另:ListWatchableBattles 按 spectateStaleBeforeMs 过滤(`listwatchablebattleslogic.go:52-66`)但不过滤「即将结束」(小改,可独立做)。

**要改的文件**
- `go/match/internal/logic/joinqueuelogic.go`(57-61、63-95)、`rating.go`(94-101;assignBalancedTeams)、`gather.go`、`watchbattlelogic.go`、`spectate.go`
- `proto/match/match_service.proto`(:47、:60)、`proto/battle/battle_data.proto`(62-83)
- `cpp/nodes/battle/logic/battle_room_manager.cpp`(观众推送)、`E:/work/mmorpg-client/Assets/Scripts/Proto/Generated/BattleData.cs`
- `docs/design/turn-based-battle-server.md`(§9 L270-274、§10.6 L334-337)、`cross-zone-matchmaking.md`(D9 L98、§10 L294、§11.10 L437-440)

**步骤**
1. 产品拍板选做项与顺序(§2 D-12);任一不做则在文档对应节写「已决策不做/延后」。
2. 3v3:joinqueuelogic switch 加 `case MATCH_MODE_3V3: required = required3v3Players(6)`;rating.go isRatedMode 加 3V3;gather 分队走 assignBalancedTeams(3/3);matched TTL 按组大小公式自动得出;单测;proto :47 去「未开放」。
3. 对手 zone 展示:BattleActorState 加 `uint32 zone_id = 21`;gather 把票据 zone_id(D4)透传到 CreateBattleRequest 的 actor 快照;C++ battle 落到 BattleActorState;三端 regen;客户端名牌显示「X 区」。
4. 预组队:JoinQueue 校验 party_member_ids 全员在线且未在战斗;票据加 party_id;队列成员改为 party 单元(Lua 入队一次写多成员并记 party 大小),popGroup 按人数拼组;评分聚合规则先定(平均/最高/加权,写进 §11);assignBalancedTeams 以 party 为不可拆单元。
5. ready check:gather 前加 ReadyCheck 阶段(match 推 S2C,N 秒内全员确认才 PrepareBattle,超时未确认者删票、其余回队首,沿用 D5 CAS 语义),需新 RPC/S2C 与客户端 UI。
6. 观战延迟推送(C++ battle):BattleRoom 加 `std::deque<TurnResultS2C> recentTurns`(容量 N),AddObserver 时首帧快照回退 N 回合,观众推送在 ResolveRound 后推 N 回合前那条;仅 PVP 生效;引擎/节点用例覆盖 N=0/1/2。
7. ListWatchableBattles 过滤「即将结束」:记录里加 created_at + deadline,summary 过滤 deadline - now < 阈值。

**验收标准**
- 每项以各自设计文档新增节 + 单测 + battle-smoke 场景为准;至少:3v3 六机器人凑单成局 zone_mix 指标正常;对手 zone 在客户端可见;预组队 3 人入队后同队出现在同一 team_index;(延迟)PVP 观众收到的 TurnResultS2C.round 恒 ≤ 参战者当前 round - N,PVE 不受影响。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\go\match; go test ./... -count=1
# robot battle-smoke(mode=3v3 / party)冒烟,match 日志 Select-String 'popGroup|zone_mix'
msbuild E:/work/xuanming-server-mmo/game.sln /t:battle /p:Configuration=Debug /p:Platform=x64 /m:1
# proto 改动后三端 regen 按 CLAUDE.md;客户端离线编译校验见 memory mmorpg-client-compile-verify
```

**风险与注意**
预组队会改队列数据结构(list 成员从 player_id 变为 party 单元),牵动 D3/D5 全部 Lua 与 CAS 语义,是跨 zone 匹配以来最大的一次改动;对手 zone 展示与 ready check 都要动客户端;观战延迟改 battle 节点观众推送不变量(key=player_id、target_instance_id 防僵尸),要回归 36 条观战用例;ready check 会拉长匹配时间,影响 D5 matched TTL 公式。

---

### P3-D3 SharedRedis 切 Redis Cluster 的阻塞清单(scene hiredis / player_locator / login / scene_manager / guild·friend / db / 运维 FLUSHALL)—— 长期项(决策 D-13)

**领域** server-go / server-cpp · **工作量** L · **状态** needs-decision

**背景与证据**
`docs/design/cross-zone-matchmaking.md:273-286`(§10.0 阻塞表:C++ scene hiredis 无 MOVED/ASK + dirty_keys_set Lua;player_locator 8 个跨 slot Lua;login 4 处 MULTI/EXEC + 跨 slot MGET;scene_manager 7 key/3N key Lua;guild/friend 非 0 号 DB + TxPipelined + RENAME;db RPOPLPUSH 跨 slot;FLUSHALL 口径)、`:288-291`;双存储设计只让 MatchRedis 集群化(D2 L89);`go/match/internal/logic/keys_test.go:55-61` 自实现 CRC16-XMODEM slot 计算可复用;memory `xuanming-spof-audit-2026-08` 记录 Redis 仍单点。

**要改的文件**
- `docs/design/cross-zone-matchmaking.md`(§10.0 L273-291)、`go/player_locator/`、`go/login/`、`go/scene_manager/`、`go/guild`、`go/friend`、`go/db/`、`go/match/internal/logic/keys_test.go`(抽到 go/shared)、`deploy/docker-compose.yml:107-125`、`deploy/k8s/manifests/infra/redis.yaml`

**步骤**
1. 拍板 A/B(§2 D-13);A 只是部署工作(compose/K8s 加 replica + sentinel,go-zero redis 哨兵支持需核实,C++ hiredis 需重连逻辑),不动业务 key。
2. B:把 keys_test.go 的 CRC16 slot 抽到 `go/shared/redisslot`,提供 `AssertSameSlot(keys...)`。
3. 按服务拆任务:列出全部多 key 命令/Lua(grep Eval/EvalSha/TxPipelined/MGET/RENAME/RPOPLPUSH),同一事务内 key 加统一 hash tag(如 `{player:<id>}`)或拆单 key + 补偿,加 slot 单测;guild/friend 先把非 0 号 DB 改为 key 前缀。
4. C++ scene 接 hiredis-cluster(或 redis-plus-plus cluster),dirty_keys_set 改按玩家 tag 分片。
5. 运维:CLAUDE.md §6.2 压测 FLUSHALL 改为逐节点或 `redis-cli --cluster call … FLUSHALL`。
6. 每个服务完成后单独灰度到 kind,汇总更新 §10.0。

**验收标准**
- A:共享 Redis 主节点 kill 后 30s 内服务自动切副本,login/scene 无长时间报错;spof-audit 单点清单划掉 Redis。B:§10.0 每行有对应 PR 与 slot 单测;共享库以 Cluster 起时全栈冒烟(登录/进场/存盘/匹配)通过,无 CROSSSLOT 日志。

**验证方式**
```powershell
grep -rn 'Eval\|EvalSha\|TxPipelined\|Mget\|Rename\|RpopLpush\|Select(' E:/work/xuanming-server-mmo/go/{player_locator,login,scene_manager,guild,friend,db} --include=*.go | grep -v _test | wc -l   # 形成清单基线
# docker compose --profile redis-cluster up -d 后把各服务 Redis.Host 指向 7000 起服,Select-String 'CROSSSLOT|MOVED' 各服务日志
# kind:kubectl -n mmorpg-infra delete pod <redis 主> 观察恢复时间
```

**风险与注意**
B 是多周工作且触及玩家存盘链路(db RPOPLPUSH 重试队列)与 C++ 存盘 Lua,风险最高;不建议与匹配/战斗迭代并行。A 不解决扩容但解决单点,是更低风险的先手。

---

### P3-D4 生成器把 `proto/contracts/kafka/*_event.proto` 也当事件给 gate 生成 handler 骨架(`match_event_handler` 空壳已入 gate.vcxproj)(决策 D-14)

**领域** ops-tooling · **工作量** S · **状态** needs-decision

**背景与证据**
`tools/proto_generator/protogen/internal/generator/cpp/event.go:324-337` 与 `:351-352`:读 `ContractsKafka` 目录、`filterProtoFilesBySuffix(files, "_event.proto")`,对每个文件 generateEventHandlerFiles → GateNodeEventHandlerDirectory;配置 `etc/proto_gen.yaml:79`(gate_node_event_handler_directory)、`:215`(contracts_kafka)。产物 `cpp/nodes/gate/handler/event/match_event_handler.cpp`(143ecca96)两个 handler 体为空,但 Register() 已把 BattleResultTeam/BattleResultEvent 接进 tlsEcs.dispatcher;event_handler.cpp 引用了它。PROGRESS.md:3508。`proto/contracts/kafka` 下 5 个文件:gate_command / gate_event / match_event / player_event / scene_command;match_event(topic match-results)gate 完全不参与。

**要改的文件**
- `tools/proto_generator/protogen/internal/generator/cpp/event.go`、`etc/proto_gen.yaml`
- `cpp/nodes/gate/handler/event/match_event_handler.{h,cpp}`、`event_handler.cpp`、`cpp/nodes/gate/gate.vcxproj`

**步骤**
1. 拍板选项 1 后:proto_gen.yaml 在 `contracts_kafka` 旁加 `gate_event_whitelist: [gate_event.proto, player_event.proto]`(或 `gate_event_exclude: [match_event.proto]`)。
2. event.go:333 的 filterProtoFilesBySuffix 后再按白名单过滤;无白名单保持旧行为并打 Warn。重编 proto-gen.exe/pbgen.exe(§1.1 陷阱)。
3. `git rm cpp/nodes/gate/handler/event/match_event_handler.{h,cpp}`,gate.vcxproj 删对应 ClCompile/ClInclude,event_handler.cpp 删 `MatchEventHandler::Register/UnRegister` 调用。
4. 重编 gate(/m:1),跑 battle_smoke_cross_zone 确认 go/match 消费方不受影响。

**验收标准**
- 重跑生成器后 gate/handler/event 下不再出现 match_event_handler.*;gate.vcxproj 无其引用;gate.exe 链接通过;robot cross_zone 冒烟仍 PASS。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; . E:\work\tools\buildenv.ps1; # 按 tools/proto_generator/README.md 运行生成器; git status --short cpp/nodes/gate/handler/event
msbuild E:/work/xuanming-server-mmo/cpp/nodes/gate/gate.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /v:m
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke_cross_zone.yaml 2>&1 | Select-String 'CROSS_ZONE|FAIL'
```

**风险与注意**
若未来要让 gate 消费 match 结果(如推「匹配成功」给客户端),再走白名单加回;删文件前 grep 全仓确认无其他引用。

---

### P3-D5 5 个死透的 C++ 测试工程(scene_test / team_test / consistent_hash_node_test / redis_test / mrediscli_test)删除或重写待决策(决策 D-15)

**领域** ops-tooling / server-cpp · **工作量** S~M · **状态** needs-decision

**背景与证据**
`tools/scripts/run_cpp_tests.ps1:38-44` 注释:「不在表里的是『被测代码已被删除』的死工程…scene_test = SceneSystem/SceneNodeStateSystem/SceneNodeSelectorSystem 全没了;team_test = team_system.h 没了;consistent_hash_node_test = ConsistentHashNode 没了;redis_test / mrediscli_test = 引用 common/src/pb/pbc 那棵已删的 proto 树…要么连测试源码一起重写或直接删,别贸然加进来」;`ls cpp/tests` 确认 5 个目录仍在(各自带独立 .sln/.vcxproj/x64 残留);game.sln 对这 5 个名字零引用;PROGRESS.md:3776-3779。当前 21/21 全绿不受影响,但留着误导新人。

**要改的文件**
- `cpp/tests/{scene_test,team_test,consistent_hash_node_test,redis_test,mrediscli_test}/`、`tools/scripts/run_cpp_tests.ps1`(:38-44)、`game.sln`、`PROGRESS.md`

**步骤**
1. 逐个确认被测物真没了:`git grep -n 'class SceneNodeSelectorSystem\|team_system.h\|ConsistentHashNode\|pb/pbc' cpp/` 为空即可删。
2. A:`git rm -r cpp/tests/<name>`;run_cpp_tests.ps1:38-44 注释改一句「历史死工程已于 <日期> 删除,见 PROGRESS」;PROGRESS 记一条。
3. B(仅当有对应新实现):复制 turn_battle_engine_test 的 vcxproj 作模板,用例改到现行 API,登记 game.sln 与 run_cpp_tests.ps1 $projects 表。
4. 确认 CMakeLists(build_linux.sh)未引用这五个目录(`find cpp/tests -name CMakeLists.txt` 当前为空)。

**验收标准**
- cpp/tests 下不再有未登记且不能编译的工程(表与目录一一对应或注释说明例外);`msbuild game.sln /t:ValidateSolutionConfiguration` 通过;run_cpp_tests.ps1 全绿。

**验证方式**
```powershell
cd E:/work/xuanming-server-mmo; git grep -n 'SceneNodeSelectorSystem\|team_system.h\|ConsistentHashNode\|pb/pbc' -- cpp
Compare-Object (Get-ChildItem E:/work/xuanming-server-mmo/cpp/tests -Directory | % Name) ((Select-String -Path E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Pattern "^\s+'(\w+)'\s+=" -AllMatches).Matches | % { $_.Groups[1].Value })
powershell -File E:/work/xuanming-server-mmo/tools/scripts/run_cpp_tests.ps1 -Build
msbuild E:/work/xuanming-server-mmo/game.sln /t:ValidateSolutionConfiguration /p:Configuration=Debug /p:Platform=x64
```

**风险与注意**
删除前确认并行会话没有在重写这些工程;mrediscli_test/redis_test 目录里有 x64 编译残留(大文件);.sln 手改易破坏 GUID 配对,优先用工具移除。

---

### P3-D6 battle 行动窗口(kAutoRoundIntervalMs=2000)不含演出时长,是否 windowMs += f(events.size()) 待产品拍板(决策 D-16)

**领域** server-cpp · **工作量** S · **状态** needs-decision

**背景与证据**
`cpp/libs/services/battle/constants/turn_battle_constants.h:35` `kAutoRoundIntervalMs = 2000`;`cpp/nodes/battle/logic/battle_room_manager.cpp:792-808` ArmRoundTimer 只按 AllPlayersReady 二选一固定窗口,不看事件数;`turn-battle-presentation.md` §6.1(:116-127)「客户端负责压缩,服务端不改…属产品节奏决策,不在本轮范围」。现契约:客户端 PlaybackBudget 按 action_deadline_ms 压缩(自动局 1.5x 起、上限 6x、塞不下 Skip),5v5 群攻在自动局 2s 窗口被压到看不清(4~6s 演出塞进 2s)。

**要改的文件**
- `cpp/nodes/battle/logic/battle_room_manager.cpp`、`turn_battle_constants.h`、`docs/design/turn-battle-presentation.md`、`E:/work/mmorpg-client/Assets/Scripts/Game/Battle/Presentation/PlaybackBudget.cs`(不需改)

**步骤**
1. 拍板 A/B(§2 D-16)。A 只在 §6.1 加「已决策维持」。
2. B:ArmRoundTimer 改为在 ResolveRound 拿到 `result.events_size()`(或按 group_id 数=拍数)后计算 `extraMs = clamp(perEventMs * n, 0, kAutoRoundExtraCapMs)`,窗口 = 基础 + extra;新增常量 kAutoRoundPerEventMs / kAutoRoundExtraCapMs;action_deadline_ms 已由 ResolveRound:826-827 回填,无需改 proto。注意 ArmRoundTimer 在 ResolveRound 内先于 set_action_deadline_ms 调用,需把事件数作为参数传入。
3. 客户端无需改(预算变大自然降倍率);同步 §6.1。
4. 验证:5v5 全自动局观察每回合 TurnResultS2C.action_deadline_ms - 上一条 ≥ 基础 + n×perEvent 且不超上限。

**验收标准**
- (B)自动局回合间隔随事件数增长且不超上限;手动局窗口不变;客户端演出倍率在群攻回合 ≤2x;观战端不掉队。(A/B)turn_battle_engine_test 与 battle 节点编译绿;battle_smoke 回合数不变。

**验证方式**
```powershell
msbuild E:/work/xuanming-server-mmo/game.sln /t:battle /p:Configuration=Debug /p:Platform=x64 /m:1
cd E:/work/xuanming-server-mmo/robot; .\robot.exe -c etc/battle_smoke.yaml 2>&1 | Select-String 'action_deadline|turn'
pwsh -File <tools>/run_showcase.ps1   # 看群攻回合帧
```

**风险与注意**
拉长窗口会让观战/连续战斗整体变慢,MMR 排位下拖长对局;必须设上限。仅在产品决策后动手。

---

### P3-D7 源立绘裁在 850×850 框内(01/10 横向持械触边),约束统一身高 175px;悬浮法宝/飘带可能缺角(决策 D-17)

**领域** client-art · **工作量** L · **状态** needs-decision

**背景与证据**
`Characters/01_ice_sword_girl/meta.json` bbox_xywh=[87,129,850,765](源 1024 图内包围盒宽正好 850);README「统一身高」节:「受 01_ice_sword_girl / 10_crimson_spear_girl(横向持剑/持矛,包围盒 850 宽 > 765/793 高)约束」;PROGRESS.md:3746;`turn-battle-presentation.md:115`。其余角色单张可到 189~219(meta.json fit_height)。重出需要生图通道,本 CLI 没有(battle-art-prompts.md 头注)。

**要改的文件**
- `E:/work/mmorpg-client/Assets/Resources/UI/qdao_v3/characters/01_ice_sword_girl_v3.png`、`10_crimson_spear_girl_v3.png`、`docs/design/battle-art-prompts.md`、`tools/battle_art_gen/README.md`

**步骤**
1. 拍板(§2 D-17)。选项 1:README/turn-battle-presentation.md §6 把它从余项改为已接受的约束并说明 175 的来源。选项 2:用 battle-art-prompts.md §2.1 模板在有生图通道的环境重出 01/10(full body 含完整兵器、留 8% 边距),替换 qdao_v3 源图后重跑 `-mode characters`,看 fitted_height 能否 ≥ 185。
2. 选项 2 时:PortraitFiles 名称不变,头像裁切 PortraitHeadCrop(`BattleArtCatalog.cs:104`)复核头部位置。

**验收标准**
- 选项 1:文档不再列为待办;选项 2:01/10 meta.json fit_height ≥ 185,fitted_height 上升且 22 套 idle 头顶行离差仍 ≤ 4px。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode characters   # 看「统一身高」输出行
python -c "import json,glob;print(sorted((json.load(open(p,encoding='utf-8'))['fit_height'],p.split('/')[-2]) for p in glob.glob('E:/work/mmorpg-client/Assets/Resources/Battle/Characters/*/meta.json')))"
```

**风险与注意**
选项 2 改变 22 套全部成图尺寸,已验收帧全部重核;选项 1 零风险。

---

### P3-D8 生图资产未产出:怪物头像 portrait.png、真·手绘动作帧、Backgrounds/ 契约目录(本 CLI 无生图通道)(决策 D-18)

**领域** client-art · **工作量** L · **状态** needs-decision

**背景与证据**
`docs/design/battle-art-prompts.md` 头注「本 CLI 会话没有生图通道」、§2.1-2.3 提示词、§3 契约 `Monsters/<id>/portrait.png` 与 `Backgrounds/<name>.png`;`Assets/Resources/Battle/` 无 Backgrounds 目录、Monsters/1..6 无 portrait*;`BattleArtCatalog.cs:207` LoadMonsterPortrait 缺图回退 GetMonsterSilhouette,`BattleScreen.cs:791` 行动预告条用之;`BattleArtCatalog.cs:84-85` ArenaBackgroundPath/EntryLoadingPath 指向 `Resources/UI/Ugui/Battle/Backgrounds/qdao_battle_arena_cloud_terrace_2560x1080_v1.png` 等(已存在,只是不在 §3 的 Battle/Backgrounds 路径;`:75` BackgroundsRoot 常量无调用方)。缺口按影响:怪物头像(预告条里怪物是深紫剪影)> 首批 6 怪手绘定稿 > 角色手绘 die/win > 更多背景。

**要改的文件**
- `docs/design/battle-art-prompts.md`、`tools/battle_art_gen/monsters.go`、`Assets/Resources/Battle/Monsters/<id>/portrait.png`(目标)、`BattleArtCatalog.cs`

**步骤**
1. 零成本先做:monsters.go 在出 idle 后另存 `portrait.png`(取 idle 首帧,按头部区域裁 128×128 居中,spirit 模板考虑 hover_px 22),写进 MONSTER_MANIFEST;客户端已按 `Monsters/<id>/portrait` 读取,无需改。
2. 若拍板投入生图:按 §2.2 出 6 怪定稿页 → §2.1 模板出动作 → 沿用 walk v1 抠图/去洋红边/脚底基线/打包(工具在 `E:/work/output/imagegen/…`,memory qdao 相关)→ 放 §3 路径覆盖程序化产物即生效。
3. 文档对齐:§3 的 Backgrounds/<name>.png 与实际使用的 `Resources/UI/Ugui/Battle/Backgrounds/` 路径二选一写清。

**验收标准**
- 行动预告条怪物头像不再是剪影(Monsters/1..6/portrait.png 存在);若生图:替换后帧条 2048×256、脚底 235 行,monsters_verify/verifyCharStrip 通过。

**验证方式**
```powershell
. E:\work\tools\buildenv.ps1; cd E:\work\xuanming-server-mmo\tools\battle_art_gen; go run . -mode monsters; ls E:\work\mmorpg-client\Assets\Resources\Battle\Monsters\*\portrait.png
pwsh -File <tools>/run_showcase.ps1   # 看顶部行动预告条帧
```

**风险与注意**
生图侧不可在本 CLI 完成;程序化头像观感一般但可立即消掉剪影。

---

### P3-D9 buff 图标底色极性靠 distinct buff_type 序号 %3 分配,非语义;`-buff-count 24` 对 20 行表补 4 个 padding(决策 D-19)

**领域** client-art · **工作量** S · **状态** needs-decision

**背景与证据**
`tools/battle_art_gen/buffs.go:88-110` categorizeBuffTypes(注释「没有增益/减益/控制的极性语义」);README「buff 底色是怎么定的」;`generated/tables/Buff.json` 20 行无极性列(`data/Buff.xlsx` 列名:id,designer,no_caster,buff_type,tag,…,bonus_damage);`buff_table.proto:61` buff_type 仅 uint32;proto/ 无 BuffType/极性 enum;`ART_MANIFEST.json` buff_files 21..24 source="padding",当前映射把 buff_type 1(id 1-4,13)判成 debuff、100 判 control,是否符合策划意图无人核过。

**要改的文件**
- `data/Buff.xlsx`、`data/schema/buff_table.proto`、`tools/battle_art_gen/buffs.go`、`main.go`

**步骤**
1. 拍板加列:按 data/AGENTS.md「加一列」,Buff.xlsx 加 `polarity`(1 增益/2 减益/3 控制),buff_table.proto 加 `uint32 polarity = <最大号+1>`,`dev.bat gen`。
2. buffs.go:buffRow 加 `Polarity int \`json:"polarity"\``;categorizeBuffTypes 按 polarity 直接映射(0/缺失回退现规则并在 note 标注);ART_MANIFEST buff_table_mapping 改按 id。
3. 去 padding:main.go `-buff-count` 默认改 0 = 按表行数,只在显式指定时补位;或保留但 README 写清。重跑 `go run .`(默认 -mode battle)。客户端不改(LoadBuffIcon 按 id 读)。

**验收标准**
- ART_MANIFEST.json buff_files 每项 category 与 Buff 表 polarity 一致,无 source=padding 项(或数量 = 表行数)。

**验证方式**
```powershell
cd E:\work\xuanming-server-mmo; dev.bat gen; . E:\work\tools\buildenv.ps1; cd tools\battle_art_gen; go run .
python -c "import json;d=json.load(open('E:/work/mmorpg-client/Assets/Resources/Battle/ART_MANIFEST.json',encoding='utf-8'));print([(b['id'],b['category'],b['source']) for b in d['buff_files']])"
```

**风险与注意**
改表要过 schema 校验(dev.bat gen 对不上直接报错、零落盘);服务端不读该列,无运行时风险。

---

### P3-D10 伤害数字字号:规格 §1 与后续帧复审口径冲突(36-40px@1220 ≈3% 屏高 vs 录像 8-10% 屏高),现值介于两者之间(决策 D-20)

**领域** client-presentation · **工作量** S · **状态** needs-decision

**背景与证据**
`DamageNumber.cs:29-32` `BaseGlyphScale = 0.95f`(48×64 字格 → 设计坐标高 ≈61px,占 1080 的 5.6%;注释「spec §1:字高 36-40、厚描边」);Expand 下 1600×900 缩放 0.625 → 屏幕约 38px(4.2%);`turn-battle-presentation.md:26`「超大(高约 36-40px)…」(录像 1220 高 → 3%);参考帧 f_003.jpg 量出 ≈9% 屏高;帧 A_0010「-22」约 30px@900。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/DamageNumber.cs`、`Assets/Tests/EditMode/Battle/DamageNumberLayoutTests.cs`、`docs/design/turn-battle-presentation.md:26`

**步骤**
1. 拍板(§2 D-20)。B:BaseGlyphScale 0.95 → 1.35(字格 86px 设计坐标),CritScale 保持 1.3,AvoidRadiusX/AvoidStepX 同比 ×1.4。
2. 选 B:同步 turn-battle-presentation.md:26 字高描述与 DamageNumberLayoutTests 中涉及 GlyphScale 的断言;`BattleUnitView.cs:890` Number 容器(400px)同步加宽。
3. 2560×1080 演出台与 1600×900 实机各截一帧量字高。

**验收标准**
- 拍板后帧中单串数字高度 = 目标比例 ±10%;群攻 5 串不重叠。

**验证方式**
```powershell
pwsh -File <tools>/run_showcase.ps1   # show_00xx_r2 命中帧
pwsh -File E:\work\mmorpg-client\tools\run_crosszone_pair.ps1 -ShotDir E:/work/tmp/shots_font   # A 侧 tick 帧
# Unity -runTests EditMode(DamageNumberLayoutTests)
```

**风险与注意**
放大后 1v1 的 MISS「闪」与暴击「暴击-N」长串可能超出 400px 容器宽。

---

### P3-D11 命令环「召唤(宠物)」只弹提示「功能尚未开放」,服务端亦无宠物系统(决策 D-21)

**领域** client-presentation · **工作量** S · **状态** needs-decision

**背景与证据**
`BattleScreen.cs:542` `SetHint("召唤(宠物)功能尚未开放")`;`:564` 自动三键「宠物」→「宠物系统暂未接入」;`BattleCommandRing.cs:34/138` 注释「召唤预留置灰」;服务端 `battle_data.proto` eBattleActionType 无召唤动作。

**要改的文件**
- `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`、`BattleCommandRing.cs`、`BattleHudLogic.cs`(RingPosition/CommandCount)

**步骤**
1. 拍板(§2 D-21)。B:隐藏「召唤」键与自动三键「宠物」,命令环改六键(BattleHudLogic.RingPosition 按 CommandCount 均分,改 CommandCount 与 Labels;BattleUiLayoutTests 自动跟随)。
2. 登记到 turn-battle-presentation.md §6 或产品待办。

**验收标准**
- 拍板结果写入文档;若选 B,环上无「召唤」且 EditMode 布局测试通过。

**验证方式**
```powershell
# Unity -runTests EditMode(若改键数)
```

**风险与注意**
无。

---

## 附录 A:本次核实发现「其实已经做完了」的项(防止再做一遍)

| 项 | 证据(一行) |
|---|---|
| cross-zone §11.9 升级/回滚说明(回滚需手工 DEL `match:{mq}:rank:*`、match-results 分区数不可改、ResultTopic 与 C++ kMatchResultsTopic 同名) | `docs/design/cross-zone-matchmaking.md:425-433` §11.9 已完整登记四条口径;`go/match/etc/match_service.yaml:91-94` ResultTopic: match-results / ResultTopicPartitions: 3 并注明「分区数是不可变契约」,`kafkautil.EnsureTopics`(topic_init.go:86-89)分区不一致时拒启——属运维文档,非代码待办。 |
| HandlePlayerMigration 目的端幂等检查(runbook 标「待修/测试必失败」) | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:973-1030` 已有两级去重:Tier 1 `tlsEcs.GetPlayer(msg.player_id())` 已存在 → 只重发 ACK;Tier 2 比对 `msg.payload_sha256()` 与本地 re-marshal 哈希,不同则 DestroyPlayer 后重建。**但** `docs/ops/cross-zone-failure-test-runbook.md:198-201` 仍写「已知缺陷(待修)…这个测试目前会失败」——文档陈旧,顺手改文档即可(可并入 P3-05 的余项小节)。 |
| 导入设置依赖未提交的 Editor 脚本 | `E:/work/mmorpg-client/Assets/Editor/QdaoCharacterSpriteImporter.cs` 已在 25ca096 提交(`git log -- Assets/Editor/QdaoCharacterSpriteImporter.cs` 可见),工作区干净;`:18` BattleFolder="Assets/Resources/Battle/" 已覆盖战斗目录;PROGRESS.md:3747 该留档已过期。 |
| 22 角色身高 170~189 不一 → 已按身高统一为 175px | CHARACTER_MANIFEST.json params.fitted_height=175、verify.idle_top_row_spread=0(全部头顶行 60);`Characters/01_ice_sword_girl/meta.json` height=175、fit_height=175;README「统一身高是算出来的」节;PROGRESS.md:3744 复审 major ② 已修。 |
| 弓手头顶灰色空矩形与治疗数字残影 | `BattleUnitView.cs:757-772` buff 图标只在 `LoadBuffIcon(buffId)==null` 时创建蓝底字母块(旧帧「灰矩形」即此回退,`Assets/Resources/Battle/Buff/` 现有 24 图标);`:261-266` 徽标底板初始 SetActive(false),只在 ShowBadge 显示、HideBadge(:1240-1242)隐藏;治疗数字走 NumberKind.Heal → `digits_heal.png`(存在)且由 DamageNumberPool 管理(`DamageNumber.cs:105-110` Clear 立刻回收);最新帧 show_0030_tick.png 幽泉女头顶只有条与图标,无灰矩形与「+500」残影。 |

另有几项已并入对应条目的「已完成部分」而非独立待办:B 档 `Wait-ForZoneReady` 对缺 manifest 的 Java 服务 `[skip-wait]`(`k8s_deploy.ps1:1033-1041`,见 P2-01);`go_svc_image.ps1:133` 对仓库外 replace 目录缺失 fail-fast throw(见 P1-D1);`go_services.ps1` 分层就绪等待已按实例等 LISTEN(见 P2-08);K8s match ConfigMap 已指向 redis-match-cluster(见 P2-D1);json_gen 导表器已改小写(见 P1-04);tip_text.json 已导出(见 P1-14);class_id 读方链已写好(见 P1-10)。

---

## 附录 B:相关脚本与产物索引

### B.1 仓库内已有(可直接用)

| 路径 | 用途 |
|---|---|
| `E:/work/tools/buildenv.ps1` | 每个构建 shell 先 source(GOROOT/GOPATH/GOPROXY/PATH) |
| `xuanming-server-mmo/dev.bat` | 本地整栈 start/stop/gen(先 source buildenv;Java gateway 不在其中) |
| `tools/scripts/go_services.ps1` | Go 服务多实例/多 zone 起停,含 Resolve-BindablePort(stop 语义见 P2-08) |
| `tools/scripts/cpp_nodes.ps1` | C++ 节点起停(无 RPC_PORT/WMI,见 P1-07) |
| `tools/scripts/run_cpp_tests.ps1 [-Build] [-Filter]` | C++ 单测统一入口,21 工程 / 437 用例 |
| `tools/scripts/k8s_deploy.ps1` | K8s infra-up / zone-up / DryRun;`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 27 条契约 |
| `tools/scripts/go_svc_image.ps1`、`java_svc_image.ps1`、`k8s_image.ps1`、`k8s_stage_runtime.ps1`、`build_linux.sh` | 镜像构建链(C++ 部分见 P1-01) |
| `tools/scripts/dev_tools.ps1 -Command proto-gen-run` | 生成器入口(默认用预编译 proto-gen.exe,改源码后先 go build) |
| `tools/data_table_exporter/run.py` | 导表器(PATH 含 protoc + protoc-gen-go) |
| `tools/battle_art_gen`(`go run . -mode characters` / `-mode monsters` / `-mode battle`) | 程序化美术产物生成,写入 mmorpg-client/Assets/Resources/Battle |
| `robot/robot.exe -c etc/{battle_smoke,battle_smoke_cross_zone,attribute_smoke}.yaml` | 三条冒烟 |
| `tools/scripts/stress_round19.ps1`、`stress_snap.ps1`、`stress_summarize.ps1` | 压测(P2-10 用) |
| `tools/scripts/release_preflight.ps1` | 发布前配置检查 |
| `mmorpg-client/tools/run_crosszone_pair.ps1`、`build_crosszone_player.ps1` | 实机双播放器跨 zone 验收 / 出包 |
| `mmorpg-client/tools/gen_proto.ps1 -ProtoRoot E:\work\xuanming-server-mmo`、`gen_messageids.ps1` | 客户端 proto/消息号 regen |
| `mmorpg-client/sync_qdao_walk_to_resources.ps1` | walk 帧条部署(默认 -Source 仍是 run_v1,见 P2-D5) |
| `E:/work/output/imagegen/qdao_headband_boy_walk_v3_fixed/fix_walk_gait.ps1` | walk 步态镜像修复工具(仓库外) |

### B.2 会话临时目录(`C:\Users\luyua\AppData\Local\Temp\claude\E--work\3f3f6928-3172-48ea-9ae4-d84fff460133\scratchpad\`,47 个 .ps1,换会话即丢)

全部建议按 **P1-09** 迁入仓库;下表标注建议归宿。

| 脚本 | 用途 | 建议归宿 |
|---|---|---|
| `full_restart.ps1` | 重启后一键链:Docker Desktop(WMI)→ 起 Exited 容器 → 等 Kafka → recover_stack_nomatch → db/login 补拉 → post_recover → relaunch_cpp_nodes → server-list 核对 | `tools/scripts/dev/full_restart.ps1`(合并下面 recover/docker/fix 子步骤) |
| `recover_stack.ps1` / `recover_stack_nomatch.ps1` | Go 双 zone + C++ 整栈拉起(nomatch 版不起 match) | 并入 full_restart |
| `docker_recover2.ps1` / `docker_recover3.ps1` | Docker 引擎退出后的恢复顺序(Stop 进程 → 改名 run/secrets 目录 → 重起) | 并入 full_restart;知识写 `docs/ops/local-runbook.md` |
| `post_recover.ps1` | 双 match + Java 网关 | 并入 full_restart |
| `fix_z1_login.ps1` / `fix_z1_then_capture.ps1` | zone1 login 补拉(Kafka 慢起连锁)/ 补拉后截帧 | 并入 full_restart 第 4 步;P1-05/P1-06 落地后可删 |
| `relaunch_cpp_nodes.ps1` | 只重拉 C++ 节点,显式 ZONE_ID/RPC_PORT/NODE_IP(硬编码 192.168.43.7),经 WMI | `tools/scripts/dev/relaunch_cpp_nodes.ps1`(改为薄封装 `cpp_nodes.ps1 -RpcPorts -UseWmi`,P1-07) |
| `start_cpp_z1.ps1` / `start_z2_cpp_ports.ps1` / `start_battle.ps1` / `start_z1_scene2.ps1` | 单件 C++ 节点起 | 不入仓,被 relaunch_cpp_nodes 覆盖 |
| `start_zone1_go.ps1` / `start_zone2_go.ps1` / `start_zone2_stack.ps1` / `start_zones.ps1` / `start_z2_match.ps1` / `start_gateway.ps1` / `restart_all_go.ps1` / `restart_db_login.ps1` / `restart_match.ps1` | Go 服务/网关分批起停 | 不入仓,被 full_restart + go_services.ps1 覆盖;`restart_both_match.ps1` 保留(见下) |
| `restart_both_match.ps1` | 双 zone match 按端口找 PID 重启(绕 stop 连坐) | `tools/scripts/dev/`;P2-08 落地后改调 go_services.ps1 -Zone |
| `build_cpp_serial.ps1` / `build_cpp_serial2.ps1` / `build_then_restart.ps1` / `rebuild_restart_battle_scene.ps1` / `rebuild_restart_scenes.ps1` / `build_verify_only.ps1` | 串行 /m:1 重编库链与节点、重编后重拉 | 合并为 `tools/scripts/dev/build_cpp_serial.ps1`(顺序 proto→rpc→table→core→battle→scene→节点→测试,P2-06) |
| `reset_smoke_state.ps1` | 冒烟前重置 robot 评分/状态(PlayerIds 写死已过期) | `tools/scripts/dev/`(P2-07) |
| `run_smoke.ps1` / `after_scenes_smoke.ps1` / `rerun_and_test.ps1` | 跑 robot 冒烟并汇总 | `tools/scripts/dev/run_smoke.ps1` |
| `verify_phase2_runtime.ps1` | 二期运行时核对(prepare deadline / 指纹 / MMR) | `tools/scripts/dev/` |
| `verify_grpc_failclosed.ps1` | 杀 battle → 立刻重拉,轮询日志验证 fail-closed(P1-08;用 Start-Process,AI 会话内改 WMI) | `tools/scripts/dev/` |
| `failover_test.ps1` | 节点故障切换探针 | 不入仓,知识写 runbook |
| `exitcode_probe.ps1` / `metrics_regex_test.ps1` / `resp_moved_probe.ps1` / `player_probe.ps1` / `switch_cluster.ps1` | 一次性探针(退出码/metrics 正则/RESP MOVED/玩家数据/切 Redis 集群) | 不入仓 |
| `run_showcase.ps1` | 演出验收台:副本工程出包 → 播放器 -showcase → 72 帧截图到 `E:/work/tmp/showcase_shots`(硬编码 `$S` 第 18 行) | `mmorpg-client/tools/run_showcase.ps1`(P1-09;`turn-battle-presentation.md:110` 引用它) |
| `live_capture.ps1` | 实机双播放器截帧链(硬编码 `$S` 第 5 行) | `mmorpg-client/tools/live_capture.ps1` |
| `build_presentation_player.ps1` / `presentation_verify.ps1` | 演出播放器出包 / 演出核对 | `mmorpg-client/tools/` |
| `client_compile_check.ps1`(+ rt.rsp / ed.rsp / test.rsp) | Roslyn csc 离线编译体检(~2s,退出码 0 = 绿) | `mmorpg-client/tools/compile_check/` |
| `unity_copy_compile.ps1` | robocopy 副本工程 + batchmode 编译 | `mmorpg-client/tools/` |

另一会话目录 `C:/Users/luyua/AppData/Local/Temp/claude/E--work/3e98f3f0-d453-4849-a175-5d4b33bd895b/scratchpad/run_editmode.ps1`:EditMode 副本工程跑法(robocopy → 从 `E:/work/tmp/presentation_verify_project/Library` 拷 Library → `-runTests -testPlatform EditMode`,不带 -quit),同样建议迁入 `mmorpg-client/tools/`。

### B.3 产物与基线目录(本机)

| 路径 | 内容 |
|---|---|
| `E:/work/xuanming-server-mmo/bin/*.exe` | 运行栈用的节点(现为 09-04 旧版,P1-08) |
| `E:/work/xuanming-server-mmo/build/cpp/nodes/*.exe`、`lib/core.lib` | 09-05 06:31-06:43 最新编译产物 |
| `E:/work/xuanming-server-mmo/run/logs/cpp_nodes/z*_*.stdout.log`、`run/logs/go/*.log`、`run/logs/java/` | 节点/服务日志 |
| `E:/work/xuanming-server-mmo/run/etc/go_services/<svc>.yaml` | Resolve-BindablePort 派生的实际端口 |
| `E:/work/tmp/livecap_player/mmorpg.exe` | 最近一次实机播放器出包(842 MB) |
| `E:/work/tmp/showcase_shots/`、`E:/work/tmp/shots_live5/{A,B}/` | 演出台 72 帧 / 实机双侧帧基线(多条 P2/P3 的对照帧) |
| `E:/work/tmp/presentation_verify_project/`、`attribute_verify_project/`、`shotverify_project/` | Unity 副本工程(Library 种子) |
| `E:/work/tmp/crosszone_pair_live/*.log` | DevAutoPilot 播放器日志(RESULT= 行) |
| `E:/work/mmorpg-client/Assets/Resources/Battle/{CHARACTER,MONSTER,ART}_MANIFEST.json` | 程序化美术清单(工具产物,不手改) |
| `E:/work/xuanming-server-mmo/generated/tables/tip_text.json` | 102 条 tip 文案(P1-14 来源) |
| kind 集群 `mmorpg`(`kubectl --context kind-mmorpg`),namespace `mmorpg-infra` / `mmorpg-zone-yesterday` | B 档实跑环境(v1.37.0) |

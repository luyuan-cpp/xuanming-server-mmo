# 客户端/服务端导航数据、出生点与位置纠正契约

初版：2026-09-05；现状核对：2026-10-23。

当前场景 1/2/3 已于 2026-10-18 换为主城、八仙岛、渔村的新导航与出生点。烘焙器、服务端和旧地图的联机验证已有执行记录，本文原来的“未编译、未重烘、未验收”不再代表当前状态；新地图的离线验证与完整联机验收仍须分别判断，见 §6、§9。

本文维护服务端落位与移动裁决契约；资源同步管线见 [场景导航网格管线 §9](scene-navmesh-pipeline.md#9-2026-10-18-当前三张地图替换正式场景)。本次只核对本仓源码、合同副本与既有执行记录，未读取独立客户端、重启服务或重跑联机验收。客户端函数名、表现和日志仅在明确注明的历史记录中保留。

## 1. 历史问题与故障链（2026-09-05）

当时三份 `main/dungeon/mirror_scene.bin` 是同一份 20648 字节的 UE 占位导航，和客户端天墉城地图不匹配。新角色 Transform 为 `(0,0,0)`，进场没有服务端落位校验；客户端自行挪到 Unity `(200,0,180)` 后没有回报，形成两端位置分歧。

最吻合当时现象的链路是：客户端本地进城 → 首次移动上报城内位置 → 服务端以旧网格裁决失败，保留原点 → MoveAck 拉回 → 客户端钳制到不可走区域，移动与镜头随之异常。当时旧 ack 日志未记录坐标，不能把“该次回包目标恰为原点”当作已证实事实。

修复由三个接缝共同承担：同源 mask 烘焙导航、服务端在下发自身 Actor 前校验落位、移动裁决避免把已知非法的旧位置继续回给客户端。历史地图、账号和机器路径不再作为当前验收的固定输入。

## 2. 当前数据与坐标契约

### 2.1 地图来源与出生点

当前三图的来源清单是 [current_maps_20261009.json](../../data/scene_nav_bin/current_maps_20261009.json)，合同副本位于 [current_maps/](../../data/scene_nav_bin/current_maps/)。资源来源为客户端 `World/CurrentMaps20261009/{main,island,village}`；服务端副本由同步工具导出，不在服务端独立改图或改出生点。

三图合同仍标记为 `layout_preview`；300×300 米是此次接入尺度，不代表容量设计已验收。

| 场景配置 ID | 地图 key / 地点 | 正式导航文件 | Unity 出生 `(x,y,z)` | 服务端出生 `(x,y,z)` |
|---|---|---|---|---|
| 1 | main / 主城 | main_scene.bin | (199,0,133) | (133,199,0) |
| 2 | island / 八仙岛 | penglai_scene.bin | (195,0,173) | (173,195,0) |
| 3 | village / 渔村 | donghai_scene.bin | (201,0,147) | (147,201,0) |

`penglai_scene.bin` 现在承载八仙岛新图，不能仅凭旧文件名推断地图内容。场景 5–16 复用主城网格，出生点也同步为 `(133,199,0)`；场景 4、17–21 保留原导航与出生点。不要将一份天墉 bin 覆盖到所有正式文件。

运行时出生点的唯一读取入口是 [`SceneSpawnSystem::DefaultSpawnFor`](../../cpp/libs/services/scene/spatial/system/scene_spawn.cpp)，读取 `BaseScene.spawn_x/y/z`；权威 schema 为 [`basescene_table.proto`](../../data/schema/basescene_table.proto)，三个字段均为 `double`，编号 3/4/5。表行不存在、坐标非有限或三轴全零时才回落到旧天墉常量 `(180,200,0)`。这组常量是兼容兜底，新增地图应通过正式配置管线填表。

### 2.2 位图、网格与查询

| 项目 | 当前契约 | 实现入口 |
|---|---|---|
| 位图 | 150×150 格，2m/格；Unity 世界矩形 x=50–350、z=0–300；从上向下逐行、MSB-first，2813 字节，尾部填充位为零 | 各图 `source.json` / `walkmask.txt`、`sync_current_maps.py` |
| 二进制导航 | `MSET` v1；烘焙器和运行时使用同一套 `ue5navmesh`，`dtReal=double` | `navmesh_baker.cpp` / `recast.cpp` |
| 换轴与单位 | 米；Unity `(x,y,z)` → 服务端 `(z,x,y)`；查询内部 `nav=(server.y,server.z,server.x)` | `nav_query.h/.cpp` |
| 平面 mask 烘焙 | radius=0、cs=0.25m、地面 y=-0.2m；quad 内缩 0.01m，默认跳过悬崖过滤以对齐格线；OBJ 模式仍默认启用悬崖过滤 | `navmesh_baker.cpp` |
| 运行时吸附 | `findNearestPoly` 搜索水平 ±2m、垂直 ±4m；找到邻近多边形即成功，不等于输入点精确位于可走面上 | `NavQuerySystem::SnapToMesh` |
| 出包校验 | `--mask-base64` 必须显式提供 Unity 坐标 `--probe x,y,z`；探针除邻近搜索成功外，还要求三轴吸附误差均 ≤0.02m；独立 mask 模式另查询全部 22500 个格心，任一不一致即拒绝落盘 | `navmesh_baker.cpp` / `sync_current_maps.py` |

旧文“保留悬崖过滤造成 0.25m 内缩，再靠纠偏阈值吞差值”的方案已被 2026-09-08 实测否定，不能用于当前平面地图出包。格心全量一致也不替代墙边、拐角与连续移动的联机验收。

## 3. 当前实现与失败语义

### 3.1 进场落位

[`PlayerSceneSystem::HandleEnterScene`](../../cpp/libs/services/scene/player/system/player_scene.cpp) 在绑定目标场景之后、发送 `EnterSceneS2C` 与自身 `ActorCreateS2C` 之前调用 `EnsureValidEnterLocation`。Transform 属于加载边界应创建的持久组件；此处缺失时拒绝入场，落位函数不补建组件，遵守 [组件访问规则](ecs-component-access-rules.md)。

| 输入状态 | 当前处理 |
|---|---|
| 已绑定同一场景实体 | 幂等返回，不重复落位与通知 |
| 旧场景信息可读，且新旧 `scene_config_id` 不同 | `useSceneSpawn=true`，采用目标场景出生点，即使旧坐标在目标网格也可吸附 |
| 不强制使用出生点，且有导航 | 保存位置可吸附则写回吸附结果；吸附失败则采用目标出生点 |
| 不强制使用出生点，且无导航 | 仅修全零的未初始化位置，其余位置保留 |
| 强制使用出生点，且无导航 | 使用目标场景配置出生点 |

“同图重入保位”具体指保留可吸附的位置，允许高度或贴边吸附带来的微调。`EnsureValidEnterLocation` 返回值表示是否走重定位分支，`false` 也可能是合法保位，不能解释成入场失败。

当前 `mapChanged` 判断依赖本地旧场景绑定；没有旧场景信息的跨节点或跨 zone 入场不由这个判断无条件强制出生点，而是按目标网格校验所加载的坐标。跨服务归属与交接语义见 [场景切换与释放设计](scene-switch-release-design.md)。

### 3.2 网格加载与寿命

[`NavigationSystem::LoadNavBins`](../../cpp/libs/services/scene/spatial/system/navigation.cpp) 对同一路径只加载和初始化一次，以 `shared_ptr<NavComp>` 共享稳定地址；每行 BaseScene 的出生点仍单独探针，成功后才注册对应配置 ID。`NavComp` 不可拷贝、不可移动，避免 `navQuery` 内部指针悬垂。

文件加载、查询初始化或出生探针失败时，不注册该行导航并打 ERROR。这里拒绝注册坏网格，但后续查询拿不到导航时移动仍按 fail-open 放行；不能把“不注册”理解为节点拒启或玩家拒入场。验收仍要求所有配置的导航正常注册。

`SceneNavManager` 为 `thread_local`，查询在所属线程内串行执行。配置成功回调调用 `LoadNavBins`；当前不是跨线程共享或导航热更新协议。`nav loaded` 在逐行出生探针成功时输出，不能再断言“仅三个文件，所以恰好三条 nav loaded”。

### 3.3 移动与纠偏

[`player_movement_handler.cpp`](../../cpp/nodes/scene/handler/rpc/player/player_movement_handler.cpp) 对位置上报执行：

1. 有导航且起点可吸附：沿当前位置到上报位置射线裁决；撞墙夹持至阻挡点并回退 0.05m。
2. 起点不可吸附：尝试吸附上报点（`guided`）；两端均失败则回落场景出生点（`respawned`）。
3. 无导航：原样接受位置，导航阻挡校验失效。
4. 写回 Transform、rotation 并置脏；裁决位置与上报位置的**服务端水平 x/y 距离严格大于 0.5m** 才发送 MoveAck，并记录 `current/reported/accepted` 与 `blocked/guided/respawned` 等裁决信息。高度差本身不触发 ack。

速度矢量独立截断到 10m/s；[`MovementSystem`](../../cpp/libs/services/scene/spatial/system/movement.cpp) 逐 tick 积分时也做导航夹持。速度截断不等于已实现按时间预算约束上报位置的完整防超速校验。

`InBattleComp` 或 `PlayerFrozenComp` 存在时，MoveStart/MoveSync 丢弃；MoveStop 跳过位置更新，但仍清零速度并广播停止。瞬移后的过期位置上报另由 `ShouldDropReportAfterTeleport` 过滤。

在导航与出生点一致的正常配置下，裁决输出应位于合法网格上；`ResolveSpawnOnMesh` 若连出生点都吸附失败，会报 ERROR 后返回未吸附出生点。因此不能写成“所有 MoveAck 无条件保证在网格上”。无导航放行时接受点等于上报点，通常也不会产生位置差触发的 ack。

## 4. 当前验收标准

| 验收项 | 判据与证据 |
|---|---|
| 数据一致 | 三图 source 关键字段与来源清单一致，mask/bin 的 SHA256 与清单一致，BaseScene 源表及正式 JSON/PB 清单一致；重烘时三份 bin 逐字节一致、67500 格心查询零差异，出生探针无需位移 |
| 启动注册 | 各已配置 `nav_bin_file` 对应场景均有 `nav registered for scene N`；无 `nav bin load failed` / `skip nav registration`；日志中出生点匹配该表行 |
| 新号与非法旧位置 | 自身 Actor 初次下发前已采用当前目标出生点；非法存档自然修复，不直接改 Redis 或重置角色 |
| 保位与换图 | 同图重入保留合法位置；本地可判定换图时采用目标出生点；跨节点/跨 zone 单独核对交接目标与最终落位 |
| 正常移动 | 当前地图可走道路、墙边与拐角的 WASD / 点击寻路持续可走，无反复回拉；移动后重登位置与退出终点在约定误差内 |
| 导航拒绝 | 从真实权威起点发送足以穿越墙体的非法上报，裁决与上报差 >0.5m 时收到对应 input_seq 的 MoveAck；服务端日志与网络坐标一致；客户端纠正后仍可移动 |

“绕过客户端 mask”不一定绕过 CharacterController 物理碰撞。若实际只在墙前移动了几厘米，未收到 ack 不能据此断定服务端导航失效；必须先证明测试确实送达了非法位置。

客户端当前脚本及断言应在获得客户端任务授权后核对。旧 `run_move_test.ps1` 的 `(200,0,180)` 新生点和固定墙坐标仅适用于旧天墉图，不能直接用于新三图。旧验收允许 `server_wall` 阶段有预期纠偏，不能把总 `snaps=0` 当作所有阶段统一的通过标准。

## 5. 验证与出包入口

以下是后续相关代码或数据变更的执行入口，本次文档更新未执行这些构建、重烘或联机步骤。工作目录统一为本仓根目录，使用已有工具链；构建日志与临时包放本项目忽略目录或系统 `%TEMP%`。不再依赖旧 `E:\work\tools\buildenv.ps1`、固定局域网 IP、临时重拉脚本或旧副本工程路径。

### 5.1 仅服务端数据检查

无需读取客户端即可核对 [来源清单](../../data/scene_nav_bin/current_maps_20261009.json)、合同副本、正式 bin 和 BaseScene 表。若要复现烘焙，先用已有 CMake 构建工具：

```powershell
cmake -S tools/navmesh_baker -B tools/navmesh_baker/build -A x64
if ($LASTEXITCODE -ne 0) { throw '烘焙器配置失败，停止' }
cmake --build tools/navmesh_baker/build --config Release -- /m:1
if ($LASTEXITCODE -ne 0) { throw '烘焙器构建失败，停止' }
```

以主城为例，输出到临时目录，不覆盖正式 bin：

```powershell
$navProbeFile = Join-Path ([IO.Path]::GetTempPath()) ('main-nav-probe-' + [guid]::NewGuid().ToString('N') + '.bin')
& tools/navmesh_baker/build/Release/navmesh_baker.exe --mask-base64 data/scene_nav_bin/current_maps/main/walkmask.txt --probe 199,0,133 --out $navProbeFile
if ($LASTEXITCODE -ne 0) { throw '主城导航重烘失败，保留输出并停止' }
Get-FileHash $navProbeFile, data/scene_nav_bin/main_scene.bin -Algorithm SHA256
```

通过标准：精确出生探针成功，stdout 含 `mask cells checked=22500 mismatches=0`，退出 0，临时 bin 与正式 bin SHA256 相同；八仙岛与渔村分别使用对应 mask 和 §2.1 的 Unity 出生点。这只证明本仓合同副本到导航的对应，不能证明副本仍与独立客户端当前版本一致。失败时保留烘焙 stdout/stderr 与临时文件，不绕过探针或自动覆盖正式数据。

### 5.2 正式资源同步（需要客户端任务授权）

[`sync_current_maps.py`](../../tools/navmesh_baker/sync_current_maps.py) 的默认同步和 `--verify` **都会读取同级客户端**，包括图片与 `navigation.json`；即使只验证也不是服务端范围内的操作。获得两端资源同步授权后按以下顺序执行：

```powershell
python tools/navmesh_baker/sync_current_maps.py
if ($LASTEXITCODE -ne 0) { throw '地图同步失败，停止' }
python tools/data_table_exporter/run.py
if ($LASTEXITCODE -ne 0) { throw '正式导表失败，停止' }
python tools/navmesh_baker/sync_current_maps.py --verify
if ($LASTEXITCODE -ne 0) { throw '地图与导表核对失败，停止' }
python -m unittest discover -s tools/navmesh_baker -p 'test_*.py'
if ($LASTEXITCODE -ne 0) { throw '烘焙器合同测试失败，停止' }
```

沿用已配置的正式导表器，不手改生成 JSON/PB 或合同副本。源 schema 未变时不因改出生数据额外重生成协议；若 schema 变化则执行根规范的完整生成与受影响服务编译流程。部署前核对本轮改动范围和全部导表清单，完整规则见 [导航管线 §9](scene-navmesh-pipeline.md#9-2026-10-18-当前三张地图替换正式场景)。

### 5.3 服务端编译与回归

修改导航、出生点或进场逻辑后，按依赖顺序构建表库、scene 库、scene 节点；MSBuild 串行执行 `/m:1`，不在服务运行时覆盖其产物：

```powershell
foreach ($navBuildTarget in @(
    'cpp/generated/table/table.vcxproj',
    'cpp/libs/services/scene/scene.vcxproj',
    'cpp/nodes/scene/scene.vcxproj',
    'cpp/tests/bag_test/bag_test.vcxproj'
)) {
    & msbuild $navBuildTarget /m:1 /p:Configuration=Debug /p:Platform=x64
    if ($LASTEXITCODE -ne 0) { throw "构建失败：$navBuildTarget，停止后续验证" }
}
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Filter bag_test -LogDir build/verification/nav-spawn
if ($LASTEXITCODE -ne 0) { throw 'bag_test 验证失败，保留日志并停止' }
```

每一步退出 0 才进入下一步。这里显式检查 MSBuild 退出码，不使用测试脚本的 `-Build`：该分支当前仅匹配错误文本，可能漏判失败并运行旧 exe。现有 [`SceneSpawnTest`](../../cpp/tests/bag_test/scene_spawn_test.cpp) 覆盖逐行出生导航、独立地图网格、非法保存位置、保位/目标出生、无导航兼容及缺失 Transform 不补建；脚本实际会运行整个 bag_test，不将其中无关失败隐去。通过标准以本轮真实测试日志为准，不固定沿用历史 6 项或 220 项计数。失败时保留首个错误、完整构建日志与测试摘要，按根规范停止并报告。

运行期验收另按 §4 进行，使用明确的本地开发环境和本轮配置，保留 scene 启动、落位与裁决日志。不能用离线烘焙、编译或健康检查代替玩家登录、换图和连续移动验收。

## 6. 验证证据与适用范围

| 日期 | 已有证据 | 适用范围与限制 |
|---|---|---|
| 2026-09-08 | 烘焙器构建、旧天墉导航重烘、服务端库与节点重编、R1/R2/NEW 移动脚本最终 PASS | 证明当时旧图故障闭环，详见 §9；不代表当前三图通过 |
| 2026-09-13 | 旧三地地图接入；C++ 四工程构建，bag_test 220/220（当时 SceneSpawnTest 6/6），烘焙契约 6/6；真实 2→3→4→1 传送及移动通过 | 旧蓬莱/东海/揽仙地图；详见 [导航管线 §7–8](scene-navmesh-pipeline.md#7-2026-09-12-三地节庆地图接入) |
| 2026-10-16 | 冻结试玩包四向移动均可走且无回拉；完整 moveTest 在 server_wall 失败，点击寻路未执行；另一个网络探针已证明服务端拒绝穿墙并回正确 ack | 完整脚本失败保留；独立探针只覆盖导航拒绝，详见 [进度记录](../PROGRESS.md) 当日“本地 Docker 试玩交付” |
| 2026-10-18 | 新三图 Release 烘焙器构建、13 项合同测试、67500 格心零差异、精确出生探针、重烘字节与导表清单一致 | 当前三图离线证据；当次没有运行完整 scene/gate/Unity 联机，详见 [导航管线 §9](scene-navmesh-pipeline.md#9-2026-10-18-当前三张地图替换正式场景) |
| 2026-10-23（本文） | 三份 bin 大小/SHA 与清单一致，MSET v1、tile 数 55/40/47；三图 mask 编码、哈希、可走格数与出生格一致；BaseScene.xlsx 的 21 行与正式 JSON 逐字段相等，源表及 JSON/PB 大小/SHA 与导表清单一致；文档链接、命令语法及差异检查通过 | 仅静态核对；PB 未重新解码，未读取客户端图片/navigation 实体，未重跑 Detour 查询、构建、测试或联机 |

历史测试数量、日志路径和机器环境只描述当时执行，不构成当前工作区或部署产物的重新验证。当前三图的完整登录、换图、WASD、点击寻路、重登和墙边纠偏仍需一次与本轮部署身份绑定的联机证据。

## 7. 当前边界与后续

- 无导航仍 fail-open；出生点吸附异常仍可能回未吸附点。加载、初始化、出生探针失败及出生点吸附异常会记 ERROR，无导航放行本身不保证错误日志；这些兼容路径不能作为正式地图数据验收通过项。
- 进场和导航读路径不补建缺失持久组件；合法旧坐标是否保留按目标网格吸附判断，旧存档无需手工改 Redis。
- `SceneNavManager` 为 thread_local，当前只在配置加载回调装载；新增场景线程或热更新前须重新设计装载、查询归属与生命周期，不能把 `shared_ptr` 解释为可跨线程并发查询。
- `NavQuerySystem::FindPath` 仍无业务调用方，dtCrowd 未接；`TeleportRequest` RPC 仍为空桩，已有域内 `TeleportWithinScene` 与上报收敛逻辑不能当作该 RPC 已实现。
- 已有速度截断与阻挡夹持；当前不据此宣称完整防超速、client_time 单调性检查或违规计数已完成。
- 本次没有读取独立客户端，未重核预测、ack 应用死区、自动移动脚本与当前资源的适配；§8–9 中的客户端行为属于历史执行口径。
- Java 对应实现与两版对账由独立仓库的 `PARITY.md` 记录。此次只更新本仓文档，无功能或共享协议变化，不修改 Java 仓或登记新版本，也不替另一版宣称验证通过。

## 8. 历史多智能体审查结论（2026-09-05）

保留本节编号，供既有审查引用定位；以下描述当时问题及修复，不替代 §2–5 的现行契约。

- mask 提取器曾锚定第一次使用 `WalkMaskBase64` 的位置而不是常量声明，导致提取为空；修正声明锚点并拒绝空串。
- Windows 烘焙构建补 `UNICODE _UNICODE _TCHAR_DEFINED`、`/utf-8 /bigobj`；MSET 头部清零以保证可复现，空几何提前拒绝，地面下移一个体素。
- NavComp 先在堆上定址再初始化查询器，同路径共享，避免浅拷贝悬垂与重复加载；之后进一步改为逐行出生探针。
- 当时客户端修复了静止 ack 的 settle、非法服务端落点的最近可走点回报及熔断、进场回报挂起补发、自动移动中控制器销毁检查等问题。这些是历史客户端记录，本次未重核其现状。
- 当时决定保留 `rcFilterLedgeSpans` 并把纠偏阈值改到 0.5m；**悬崖过滤方案已在 09-08 联机中撤回**。现存 0.5m 阈值是纠偏容差，不能继续用旧内缩方案解释当前烘焙规则。

## 9. 历史实际执行记录（2026-09-08）

当时已按顺序完成烘焙器构建、重烘、依赖库与节点重编、本地栈拉起、Unity 6000.6.0f1 播放器出包及五轮移动验收迭代，结束于 `[move] PASS`。

| 现象 | 当时根因 | 当时处理 |
|---|---|---|
| 烘焙器 Release 首编 C3861 `assert` | UE fork 裸用 assert，Release 头文件未包含声明 | 构建强制包含 cassert |
| 节点首链出现 RPC / Kafka 未解析符号 | 部分依赖库早于接口变化 | 按依赖顺序重编再链接 |
| 本地角色控制器不就绪 | 当时客户端将 EnTT 首实体 0 当作“无角色” | 当时增加显式本地角色标志；当前服务端下发的是独立 actor 网络号，不再以 ECS 句柄作为网络身份 |
| 寻路沿墙反复回拉 | 悬崖过滤使网格内缩，与 mask 可走边界不一致 | 平面 mask 烘焙跳过悬崖过滤 |
| 单格柱旁卡住 | 客户端路径平滑与物理侧滑进入阻挡格 | 当时增加横向余量、受阻重规划和 mask 外回退 |
| 关闭过滤后导航外扩 | quad 边落在体素边界，退化裁剪覆盖邻格 | quad 内缩 0.01m |
| scene 重拉后一度无法进场 | 当时 gate 持有旧 RPC 连接 | 当次整组节点重拉后完成验收；该绕行不作为现行恢复方案 |

最终 06:50 轮旧账号 R1 终点 Unity `(188.34,0.01,116.25)`；R2 重登出生 `(188.34,0,116.25)`，水平误差 0.00m；新账号出生 `(200,0,180)`。R1 的 `acks=6 snaps=6` 均发生在预期 `server_wall` 阶段，合法移动未发生额外回拉。旧图日志记录 `tiles=74` 和服务端出生 `(180,200,0)`，这不是当前主城的 tile 数或出生点。

原始日志当时位于 `E:\work\tmp\move_test\move_{R1,R2,NEW}.log`；路径仅作历史索引，本次未确认其仍存在。旧 `buildenv.ps1`、固定端口/IP、PATH/DLL 排查、Unity 副本出包及并行会话中间态已退出当前操作清单；后续使用仓库现行工具和本轮环境。

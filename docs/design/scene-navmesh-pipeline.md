# 场景导航网格管线(Recast 烘焙 + 服务器寻路阻挡校验)

日期:2026-08-26。状态:代码就位,**未编译,待 Codex 验证**(见 §6)。

## 1. 背景与问题

- 服务器启动时按 `BaseScene` 表 `nav_bin_file` 加载 `data/scene_nav_bin/*.bin`
  (`spatial/system/navigation.cpp`),但 `navQuery` 全仓只有 init,从未被查询——
  导航是死数据。
- 旧的三个 bin(main/dungeon/mirror)**md5 完全相同**,是 UE 客户端时代的占位
  数据(UE 布局、厘米单位、agent 144/35/35),与现在 Unity 客户端的天墉城
  400×300 地图毫无关系,且仓库内没有任何能重新生成它们的工具。
- 移动 handler(`player_movement_handler.cpp`)此前全部空桩,服务器收到移动包
  即丢;`MovementSystem` 无条件积分,没有任何阻挡校验。
- 客户端权威阻挡数据 = `TianyongPaintedCity.cs` 内嵌的 `WalkMaskBase64`
  (150×150 位图,单格 2m,painted-city 主题默认启用);两端此前毫无关联。

## 2. 管线全貌

```
mmorpg-client/TianyongPaintedCity.cs (WalkMaskBase64, 权威阻挡真相)
        │ tools/navmesh_baker --painted-city  (直接解析 C# 源里的 base64)
        ▼
可走格 → 地面三角面(不可走=留洞) → Recast tile 烘焙(ue5navmesh 同源码)
        │ --out
        ▼
data/scene_nav_bin/tianyong_scene.bin ('MSET' v1,与 recast.cpp 加载器逐字节兼容)
        │ 复制为 main/dungeon/mirror 三个文件名(当前所有场景同一张图)
        ▼
scene 节点 LoadNavBins → SceneNavManager → NavQuerySystem(吸附/阻挡射线/寻路)
        ├─ player_movement_handler:MoveStart/Sync/Stop 位置裁决 + MoveAck 纠偏
        └─ MovementSystem:每 tick 积分后的阻挡夹持(撞墙停在阻挡点并清速度)
```

未来真 3D 场景:Unity 导出 OBJ,走 `navmesh_baker --obj` 同一条烘焙路径。

## 3. 关键决策

| 决策 | 选择 | 原因 |
|---|---|---|
| Recast 源码 | 沿用 `third_party/ue5navmesh`(dtReal=double,WITH_NAVMESH_* 全不定义) | 运行时已用它编译;烘焙器与运行时**必须同一套布局**,否则 bin 二进制不兼容。原版 recastnavigation 的 float 版 `dtNavMeshParams` 与之不通 |
| 烘焙输入 | 直接解析客户端 C# 源里的 WalkMaskBase64,不开 Unity | 地图是运行时程序化生成,编辑期场景无几何;mask 就是客户端判定真相,解析它 = 两端语义一比一,且无 Unity/Python 依赖 |
| agent radius | 0(不收缩) | 客户端 `TianyongPlayerController` 只拿脚底点查 mask,无半径概念;服务器校验同一个点。收缩会造成"服务器说撞墙、客户端看还有缝" |
| 单位/坐标 | 米,Unity 坐标系(Y-up)烘焙;查询侧换轴 | 换轴契约与客户端 `WorldCoordinateConverter` 一致:nav=(sy,sz,sx),server=(nav.z,nav.x,nav.y)。服务器是 Z-up |
| cs | 0.25m | mask 格 2m 的整数分之一,导航边界与 mask 格线对齐;**注意** `rcFilterLedgeSpans` 会把紧邻洞的一圈体素当悬崖剔掉,实际边界向内缩 1 个体素(0.25m),`kMoveCorrectionEpsilon` 取 0.5m 吞掉这个差值(2026-09-05 审查确认,见 nav-spawn-fix 文档 §8) |
| 校验失败策略 | fail-open(场景无导航→放行)+ 阻挡点截断 + MoveAck 纠偏 | 不能因缺数据卡死玩家;撞墙时服务器权威位置停在阻挡点,水平差 >0.25m 回 MoveAck 让客户端拉回 |

Agent 参数:height 1.8 / climb 0.35(对齐客户端 `TianyongMapConfig`
playerHeight/playerStepOffset)。速度信任上限 `kMaxTrustedClientSpeed=10`
(客户端 moveSpeed 9 + 余量,见 `spatial/constants/nav.h`)。

## 4. 本轮代码改动

新增:
- `tools/navmesh_baker/navmesh_baker.cpp` + `CMakeLists.txt`:离线烘焙器
  (mask/OBJ → tile 烘焙 → MSET v1)。
- `cpp/libs/services/scene/spatial/system/nav_query.{h,cpp}`:`NavQuerySystem`
  ——SnapToMesh / ValidateMove(返回阻挡点)/ FindPath(拉直路径),
  对外收发服务器坐标,内部换轴。

修改:
- `spatial/manager/scene_nav.h`:补 `Get(id)`。
- `spatial/constants/nav.h`:速度上限 / 纠偏阈值常量。
- `spatial/system/movement.cpp`:积分后导航夹持 + Transform/Velocity 脏位
  (补上了此前注释里欠的 `kTransformFieldNumber` 脏位)。
- `cpp/nodes/scene/handler/rpc/player/player_movement_handler.cpp`:
  MoveStart/MoveSync/MoveStop 落地(位置裁决 + 速度截断 + MoveAck)。
  TeleportRequest 仍为空桩(需传送表,独立特性)。
- `scene` 库 CMakeLists / vcxproj / filters:注册 nav_query。

## 5. 已知边界(不在本轮)

- ~~首进场 spawn 契约仍是 (0,0,0)~~ **2026-09-05 已修**(见
  `nav-spawn-fix-2026-09-05.md`):`SceneSpawnSystem::EnsureValidEnterLocation`
  在 `HandleEnterScene` 里按导航校验进场位置,不合法落到出生点
  `kTianyongSpawn* = (180,200,0)`(== Unity (200,0,180));`LoadNavBins` 注册前
  探针出生点,旧占位 bin 会被拒绝注册;烘焙器 `--probe` 出包前同样探针。
  出生点仍是常量而非 BaseScene 表列(所有场景共用一张图)。
- 客户端未做预测回滚,MoveAck 纠偏 >1.5m 硬 snap(GameClient.cs 现状);
  snap 落点不在 mask 上时客户端会恢复到最近可走格并回报 MoveStop(兜底,
  正常数据下不触发)。
- AI 寻路(`FindPath`)已有 API,尚无调用方;dtCrowd 仍未接。
- 反作弊只有速度截断 + 阻挡夹持,无 client_time 单调性/违规计数。
- `SceneNavManager` 是 thread_local,`LoadNavBins` 只在启动线程跑过——多场景
  线程模型下需要每线程加载(现状如此,本轮未改)。

## 6. Codex 执行清单(编译 + 出数据 + 验证)

顺序执行,C++ MSBuild 串行 `/m:1`:

1. **烘焙器构建**(工作目录 `xuanming-server-mmo`):
   ```
   cmake -S tools/navmesh_baker -B tools/navmesh_baker/build
   cmake --build tools/navmesh_baker/build --config Release
   ```
   期望产物 `tools/navmesh_baker/build/Release/navmesh_baker.exe`,零 error。
2. **烘焙**(工作目录 `xuanming-server-mmo`,客户端仓在同级 `../mmorpg-client`):
   ```
   tools\navmesh_baker\build\Release\navmesh_baker.exe --painted-city ..\mmorpg-client\Assets\Scripts\World\Tianyong\TianyongPaintedCity.cs --out data\scene_nav_bin\tianyong_scene.bin
   copy /Y data\scene_nav_bin\tianyong_scene.bin data\scene_nav_bin\main_scene.bin
   copy /Y data\scene_nav_bin\tianyong_scene.bin data\scene_nav_bin\dungeon_scene.bin
   copy /Y data\scene_nav_bin\tianyong_scene.bin data\scene_nav_bin\mirror_scene.bin
   ```
   通过标准:stdout 有 `mask geometry: ... quads`、`built N tiles`(N>0)、
   `saved ...: N tiles`;bin 尺寸不再是旧的 20648 字节。
3. **scene 编译**:重编 `scene` 静态库与 `scene` 节点(msbuild game.sln 对应
   工程,`/m:1`)。新文件 nav_query.cpp 已注册进 CMake/vcxproj/filters。
4. **冒烟**:起本地栈(dev.bat 路径),客户端或 robot 进场后:
   - scene 日志无 `nav bin load failed` / `skip nav registration`;
   - 客户端朝城墙/建筑走,观察收到 MoveAck 且位置被拉回(或 robot 侧断言
     MoveSync 上报非法点后收到 MoveAckS2C.server_location ≠ 上报点);
   - 沿街道正常行走不应收到纠偏 MoveAck。
   失败时保留 scene 节点 stdout/log 尾部 200 行。


## 7. 2026-09-12 三地节庆地图接入

三地的日景和节庆画面各两张,同一地区共享一份保守可走位图。地图资源与
原图尺寸不决定服务器的单位:仍沿用 Unity 米制 300×300 地面、x=50–350、
z=0–300,150×150 位图每格 2m、从画面上方向下排列、MSB-first 共 2813 字节。

| 场景配置 ID | 地点 | nav_bin_file 文件名 | Unity 出生点 | 服务器出生点 |
|---|---|---|---|---|
| 1 及原有其它场景 | 天墉城 | main/dungeon/mirror_scene.bin | (200,0,180) | (180,200,0) |
| 2 | 蓬莱岛 | penglai_scene.bin | (220,0,170) | (170,220,0) |
| 3 | 东海渔村 | donghai_scene.bin | (200,0,180) | (180,200,0) |
| 4 | 揽仙镇 | lanxian_scene.bin | (200,0,180) | (180,200,0) |

这些 ID 已在 World 表中,继续走现有 EnterSceneC2S(scene_config_id, scene_id=0)
路由,无需新增协议或临时预览场景。日景/节庆切换是客户端画面状态,不新建网络场景。
同节点传送到不同配置地图时采用目标出生点;同配置换线、重登仍保留合法保存位置。

出生坐标从 `data/schema/basescene_table.proto` 与 `data/BaseScene.xlsx` 正式增加
`spawn_x/y/z` 三个 double 字段,字段号 3/4/5。唯一消费点仍为
`SceneSpawnSystem::DefaultSpawnFor`。无场景/旧表无出生列时保留天墉兼容值;
所有正式行都显式填值。网格文件按路径只加载一次,但每行出生点都要通过探针才注册。

新地图位图在独立客户端的
`Assets/Resources/World/FestivalRegions/{penglai,donghai,lanxian}/walkmask.txt`。
烘焙器 `--mask-base64 <文件>` 直接接受这些资源,与旧 C# 入口走同一几何管线。
此模式必须显式 `--probe x,y,z`,不偷偷套天墉出生点,且拒绝错误长度/编码、空几何、
不在网格的探针以及非有限坐标。`--painted-city` 的原用法与默认天墉探针保持兼容。

示例(工作目录为服务端仓库):

```powershell
tools/navmesh_baker/build/Release/navmesh_baker.exe --mask-base64 ../mmorpg-client/Assets/Resources/World/FestivalRegions/penglai/walkmask.txt --probe 220,0,170 --out data/scene_nav_bin/penglai_scene.bin
```

出包后须先重编并部署所有读取 BaseScene 的服务,再重启本地 scene 加载配置与网格。
Go 的 JSON 表解析拒绝未知字段,因此本机 scene_manager/player_locator/login 也须更新;
friend/guild 如有部署同样需要重编。仅复制新 JSON 给旧 Go 程序会在启动时拒绝 spawn_x。
不要清理数据库或重置角色;
旧地图非法存档坐标由进场逻辑自然修复。验证入口为烘焙器的 `test_baker.py`、
现有 bag_test 工程中的独立 `SceneSpawnTest` 和客户端真实登录/传送/移动验收。


## 8. 2026-09-13 接入验证

- 三份最终位图与客户端资源逐字节一致,蓬莱/东海/揽仙分别含 2265/2218/2295 个可走格,
  烘焙为 34/40/45 个 tile,文件为 86064/80520/102088 字节。保留不收缩边缘的旧烘焙规则;
  各自出生探针返回精确配置坐标,天墉及原有其它场景继续使用 61 tile 网格。
- `table`、`scene` 静态库、`scene` 节点及 `bag_test` 四个工程按 Debug/x64、`/m:1`
  串行构建通过。完整 `bag_test` 为 220/220,其中 `SceneSpawnTest` 6/6 覆盖 21 行导航
  注册、三图网格独立、非法存档修复、同图重入保位、跨图出生及无导航兼容。
  烘焙器契约测试 6/6,覆盖独立位图与旧入口一致、缺失/非法出生、坏编码及空几何拒绝。
- 导表 manifest v13 的 28 张表所有源文件、JSON/PB 产物大小与 SHA256 匹配;
  正式新增 BaseScene 的 Go 绑定及五个读取者 scene_manager/player_locator/login/friend/guild
  均编译通过;前三个已部署本机运行目录,后两个未启用。Go 表包既有测试仍引用已经移除的
  TestMultiKey/Buff `FindBy*` API 而无法编译,本轮未更改这些测试或对应表,不计入通过项。
- 证据位于工作区 `../tmp/festival-final-*`,含构建日志、C++ XML、烘焙日志及
  `festival-final-server-validation.json`(位图与 bin 哈希对应)。独立代码复核未发现本轮
  同节点传送阻塞问题;跨节点切换仍遵守既有的上游安全门禁。

- 正式客户端首轮登录成功后,传送到配置2停在加载态:Go 已发送新 RoutePlayer,但 gate
  只在待登录类型非零时通知 scene。修复后同节点不同场景实例使用 LOGIN_NONE 入场,
  不重复触发登录事件;相同路由重投不重复入场。发送前置检查失败不提交新场景或消费待
  登录类型,缺少场景实例的 BindSession 继续等待。gate 串行重编通过,路由身份测试26/26,
  其中7条覆盖两种登录顺序、重复投递、跨图、同图换线和暂缺RPC连接后的重投恢复。

- 正式客户端最终联机验收 PASS:2→3→4→1 四次服务端入场、三地日景/节庆与返城7张截图;各图出生误差、导航不一致和移动重定位均为0。服务端入场原始行、进程PID/启动时间、4图网格及BaseScene运行文件SHA256收在 `../tmp/festival-final-server-deployment-evidence.json/.log`。

## 9. 2026-10-18 当前三张地图替换正式场景

正式场景 1/2/3 改用客户端 `World/CurrentMaps20261009` 的主城、八仙岛、渔村。
这些是当前已制作布局的先行接入,美术合同标为 `layout_preview`;
300×300 米是本次接入尺度,不是容量设计验收结论。新导航只由各图的
`walkmask.txt` 烘焙,不复用前述天墉/蓬莱/东海旧画面位图。

| 场景 ID | 客户端 key | 正式导航文件 | Unity 出生 | 服务端出生 |
|---|---|---|---|---|
| 1 | main | main_scene.bin | (199,0,133) | (133,199,0) |
| 2 | island | penglai_scene.bin | (195,0,173) | (173,195,0) |
| 3 | village | donghai_scene.bin | (201,0,147) | (147,201,0) |

导航文件名保留既有 BaseScene 路径契约;`penglai_scene.bin` 此后装的是八仙岛新图。
源表 5–16 也引用 `main_scene.bin`,因此出生点一并采用新主城坐标,避免加载同一网格时
其它表行的出生探针失败。场景 4、17–21 及其旧导航保持原样;当前客户端不开放无新资源地图。
登录请求不携带存档场景 ID:login 的 `resolveEnterSceneRoute` 返回 sceneID=0,
scene_manager 的 `resolveSceneForEnter(0,0,zone)` 从 World 第一行选择正式场景 1,
因此旧账号正常登录由服务器进入新主城。跨区传送中已有的 pending 目标仍遵守既有票据规则。

`tools/navmesh_baker/sync_current_maps.py` 维护可重复管线:

1. 从同级客户端各图 `source.json` 核对场景 ID、世界矩形、位图与图片 SHA256、
   22500 位长度/尾部零填充、可走格数及出生点;任何不一致先拒绝,不写正式文件。
2. 从三份新位图烘焙,逐一验证出生点不需要吸附,并对最终 Detour 网格查询全部
   22500 个格心。三图全部成功后才替换正式 bin 并修改 `data/BaseScene.xlsx`。
3. 同步 `data/scene_nav_bin/current_maps/{main,island,village}/{source.json,walkmask.txt}`
   作为跨语言合同产物,与客户端逐字节一致;Java ContractSync 复制到对应
   `config-data/navmesh/current_maps/`。这些是派生产物,不可在服务端单独改图或改出生点。
4. 按既有正式导表器生成 BaseScene JSON/PB 与 manifest;没有修改 schema 或手改生成表。
   `--verify` 重新烘焙并比较 bin 全字节、合同副本、源表、生成 JSON 和清单中 JSON/PB 的 SHA。

```powershell
cmake -S tools/navmesh_baker -B tools/navmesh_baker/build
cmake --build tools/navmesh_baker/build --config Release -- /m:1
python tools/navmesh_baker/sync_current_maps.py
# 按本机已有工具链运行正式导表器,不是手工编辑 generated/tables。
python tools/data_table_exporter/run.py
python tools/navmesh_baker/sync_current_maps.py --verify
python -m unittest discover -s tools/navmesh_baker -p "test_*.py"
```

本次实际使用正规导表器的沙盒配置,只生成数据与用于二进制导表的 Python proto;
源 schema 未变,其它语言代码无需重生成。核对只有 `basescene.json`、`basescene.pb`
和 `manifest.json` 改变后采用这三个产物。最终先快进到服务端主干 `e4aee007f`,
再从最新 45 张源表重新导出;44 张非 BaseScene 表和它们的清单条目与主干完全一致,
manifest 从 28 升为 29,保留排行榜、邮件、装备增强等主干配置。
三图分别 55/40/47 tile、6651/4018/4524 个可走格;67500 格心差异为 0。
MSVC Release 烘焙器编译、13 项合同测试、重烘焙字节一致和 schema 索引检查通过。
证据清单在 `data/scene_nav_bin/current_maps_20261009.json`,本机导表日志在
`../tmp/current-map-server-20261018/export-latest.log`,采用对账在
`export-latest-adoption.json`。schema 索引首次检查发现主干的 MessageLimiter 条数仍写 81,
实际已有 87 行;按正式索引生成器更新该文档行后复查通过,未修改 MessageLimiter 源表。

本次没有运行完整 scene/gate 服务或 Unity 联机验收:本机固件虚拟化关闭、Docker
daemon 未运行,且 `bin/gate.exe`、`bin/scene.exe`、`bin/battle.exe` 缺失。
上述 9/13 的联机成功记录属于旧地图,不能作为这次新地图的联机验收证据。

## 11. 2026-11-09 九张节庆地图开发接入

本机交付状态：隔离栈 `festival-maps-20261109` 已运行新镜像，场景日志确认全部九图导航
实际加载并注册；网关 `http://127.0.0.1:18185/api/server-list` 返回 zone 1 OPEN。
对应记录为 `build/festival-maps-20261109/stack/festival-integration-status.json`、
`festival-nav-registration.log` 与 `node-runtime-identities.txt`。
用户决定自行手动验收，因此本次自动 Unity 验证在进入 Play 前停止；脚本编译通过，
尚无双端实机验收结果。测试服保持运行，客户端用 Editor 启动，无需打包。

本批外部 ID 保留 `01001`、`01002`、`05008`、`10000`、`19004`、`21003`、`23006`、
`28007`、`35004`；服务器模板号取交接的 `numericSceneId`，不作为运行实例号使用。
15 套 `spring` / `mid-autumn` 外观仍共享各图的同一导航，客户端 `clientOnly=true` 和正式
World 入口保持关闭。`prepare_festival_test_tables.py` 只在指定输出目录生成隔离测试表。

### 导航合同与生成

`sync_festival_maps.py` 固定核对交接 JSON 的 SHA 和每套文本资源 SHA，调用同源
ue5navmesh 烘焙器生成 `data/scene_nav_bin/festival_maps/<五位 ID>/`：

- `source.json`、`walkmask.txt`、`navigation.json` 为已交付 canonicalAppearance 的原字节。
- `scene.bin` 保持 MSET v1、`dtReal=double`，不直接把客户端位图当 MSET。
- `scene.bin.mask` 是 `FMASK 1` 合同：列/行、cellSize、worldRect、effectiveWorldRect、MSB 位图十六进制。
  这一附加合同约束 Recast 体素边缘；其北南行序按 `rows-1-floor((z-minZ)/cellSize)`，
  格心用实际格与有效内容区域的交集，最北行不足整格时不发生偏移。
- `scene.bin.sha256` 两行分别绑定 MSET 和 FMASK；运行端在 Detour 解析之前检查，
  文件缺失、截断、位翻转或伴随合同不匹配均不注册。
- 批次清单 `festival_maps_20261109.json` 记录场景映射、外观、出生点、SHA 与未解除限制。

烘焙器新增显式 `--mask-grid columns,rows,cellSize,minX,minZ,width,height` 与
`--effective-rect x,z,width,height`；旧 `--painted-city` 与默认 150×150 入口保持兼容。
生成前核对所有格心及全部可走格的域内可达性，域间路径必须不可达，再落盘。
`19004` 仍只有 48 格；`10000` 两域及 `35004` 三域均没有自动连桥。

### 运行端门禁

`BaseScene.nav_bin_file` 使用 `data/scene_nav_bin/festival_maps/` 前缀即声明严格导航合同。
`NavigationSystem::LoadNavBins` 在 SHA、FMASK 解析、真实 Detour 逐格比对和出生探针均成功后注册。
`NavQuerySystem` 在原有唯一换轴点使用严格半开边界/位图/禁止对角穿角规则，并拒绝部分路径
冒充跨域成功。缺导航时 gRPC/RPC 两个 CreateScene 入口、入场预检、客户端位置报告、速度积分和
组队跟随均拒绝。统一同场景瞬移入口同样验证可用导航、合法终点与同域可达性；归队瞬移失败会撤销
驱动状态，玩家可自行控制与重试，不能由跟随 fallback 跨岛。逐步跟随也经过严格移动裁决。
旧地图的无导航兼容分支没有扩大修改。

水平误差上限仍为 0.02m；垂直单独允许地面 ±0.2m 内脚底偏移（Unity floor 顶面和
CharacterController skinWidth 的实际合同），接受后归零，不允许扩大 X/Z mask 或吸附进障碍。
混合方向过格角的相同参数时刻先去重，避免零长度区间被当作额外正交中转格；四方向单侧阻挡均有回归。

```powershell
cmake --build tools/navmesh_baker/build --config Release -- /m:1
python -B tools/navmesh_baker/sync_festival_maps.py
python -B tools/navmesh_baker/test_baker.py
python -B tools/navmesh_baker/test_current_maps.py
tools/navmesh_baker/build/Release/nav_mask_contract_test.exe data/scene_nav_bin/festival_maps
# 冻结发布输入期间，重烘焙必须写隔离输出，不覆盖正在构建的 data：
python -B tools/navmesh_baker/sync_festival_maps.py --output build/verification/festival-server-20261109/rebake
```

本轮已实际取得：baker MSVC Release 构建通过，九图 **140055 格心、18286 可走格** 的真实
Detour 几何/连通验证通过；重烘焙 54 个文件逐字节一致；baker 10 项、旧图合同 5 项通过；
独立 `NavMask` 接口检查 36616 项通过（混合方向穿角回归先检出两次失败，再修复为零失败）。日志和逐文件 SHA 证据位于
`build/verification/festival-server-20261109/`。

Windows Debug `scene.lib`、`bag_test.exe` 已实际构建通过。运行端测试直接调用正式
LoadNavBins、NavQuery、入场预检和统一瞬移入口，实际 **15/15 通过**：新增 8 项覆盖
48 格、越界、缺/坏 bin/mask/checksum、地面高度容差和跨平台归队瞬移拒绝；另有 7 项
既有 SceneSpawn 回归（含原 21 个场景导航与出生点）通过。
日志与 XML 为 `native-navigation.log` / `native-navigation.xml`，汇总为
`server-navigation-validation.json`。Linux 节点构建见下述 r3 回执；隔离客户端联调仍须由
本次执行记录继续补充，不能把上述离线证据当作实机通过。
`10000` 画面接缝仍为待修；所有地图均未据此解除 `clientOnly` 或开放生产传送。

### 隔离配置与本机测试栈

以下命令在 `D:\luyuan\xuanming-server-mmo-cpp` 运行。客户端是同级目录
`D:\luyuan\mmorpg-client`；同级 `xuanming-server-mmo` 是 Java 仓库，不是本节 C++ 栈目录。
本轮使用 `build/festival-maps-20261109/` 保存私有配置、编译证据和运行快照：

| 产物 | 本轮路径 |
| --- | --- |
| 隔离源表、正式导表器输出与核对回执 | `tables-r2/festival-test-tables.json` |
| 同批 gate / scene / battle Linux 构建回执 | `cpp-build-r3/result.json` |
| Compose 私有配置 | `stack/.env`、`stack/config/` |
| 绑定已核验表和导航的 Compose overlay | `stack/compose.festival.json` |
| 运行快照及 SHA 回执 | `stack/tables/`、`stack/navigation/`、`stack/festival-runtime-manifest.json` |

`prepare_festival_test_tables.py` 复制正式源表到新的忽略目录，校验九图与 15 外观，
调用仓库正式导表器；仅 `basescene.json/pb`、`world.json/pb` 和 `manifest.json` 应变化。
原 21 个 BaseScene 保留，新增九个场景模板与隔离 World 入口，正式 `data/*.xlsx` 不变。
输出目录必须尚不存在；下面是本轮对应的取值，复跑应选择新的输出目录并同步后续引用：

```powershell
python -B tools/navmesh_baker/prepare_festival_test_tables.py `
  --client-root ../mmorpg-client `
  --output build/festival-maps-20261109/tables-r2

# 独立项目、网络和端口；仅准备配置，不启动或重启服务。
python -B tools/scripts/prepare_docker_debug.py `
  --output build/festival-maps-20261109/stack `
  --project festival-maps-20261109 --subnet 172.29.222.0/24 `
  --gateway-port 18185 --gateway-client-port 18186 `
  --gate-port 28100 --battle-port 28110

# 仅在本批 Linux 构建 result.json 的 success=true 后执行。
python -B tools/navmesh_baker/configure_festival_local_stack.py `
  --run-dir build/festival-maps-20261109/stack `
  --tables-report build/festival-maps-20261109/tables-r2/festival-test-tables.json `
  --node-build build/festival-maps-20261109/cpp-build-r3/result.json `
  --image-env build/docker-debug-current/.env
```

`configure_festival_local_stack.py` 要求项目名以 `festival-maps-` 开头，核验导表 SHA、
导航两行 SHA 与三个节点的同批镜像 ID，再生成只读挂载快照。`--image-env` 只提供既有
辅助服务镜像引用，不复制原栈账号、口令、端口、网络或数据卷。已存在快照会拒绝覆盖；
`.env` 含私有开发口令，不打印、上传或提交。上述两个节庆工具均不启动容器。

本轮 HTTP 网关为 `http://127.0.0.1:18185`（另一个同网关宿主映射为 18186），
客户端 gate 为 `127.0.0.1:28100`，battle 端口 28110；Compose 项目
`festival-maps-20261109` 使用 `172.29.222.0/24`。原 `xuanming-debug-*` 栈继续使用自己的
配置与数据，不作为本轮已更新的证明。配置快照、镜像构建成功和容器启动分别记录，均不能
代替真实入场和移动验收。本轮 Linux r3 与配置快照已成功，三个节点使用同一镜像
`local/xuanming-node:debug-20261109t100006.210999z`，不可变 ID 为
`sha256:201b6eb9ef333201e1cd21971a3ef4008ce4874714078498697cfb77a017401b`；
`festival-runtime-manifest.json` 此时仍记 `liveVerified=false`，本轮 E2E 仍待结果。

首次启动还需完成落点库迁移：当前 `compose.game.yml` 的 `db-migrate` 默认只迁
`zone_1_db`。`DB_ALLOWED_DATABASES` 中包含 `player_store_1000000_db` 只表示准入，
不会自动遍历该库。隔离 MySQL、`sql-boot` 准备及常规迁移成功后，写者注册和业务启动前，
需额外执行正式迁移入口（使用同一新栈环境与 Compose 文件）：

```powershell
docker compose -p festival-maps-20261109 `
  --env-file build/festival-maps-20261109/stack/.env `
  -f deploy/debug/compose.game.yml `
  -f build/festival-maps-20261109/stack/compose.migrations.json `
  -f build/festival-maps-20261109/stack/compose.festival.json `
  run --rm --no-deps db-migrate -f /app/etc/service.yaml -command up -storage-id 1000000
```

缺少此步会使 `data-service-sql-boot` 因落点库没有 `route_meta` 拒绝注册。本轮已实际补跑
成功，exit 0、applied 1，日志在 `stack/player-store-migration.log`。若 `db-sql-boot` 已在
补迁移前成功，它当时只会登记已有闸门表的库，须在启动 db 业务前与失败的
`data-service-sql-boot` 一起重建一次（沿用上述 Compose 参数，命令改为
`up -d --no-deps --force-recreate db-sql-boot data-service-sql-boot`），再确认注册完成并
继续新栈业务启动。其它已成功且未消费的写者授权不因这次落点库补迁移而重建。

### Unity Editor 联调入口

隔离服务器就绪后，在客户端仓库通过 Editor 打开正式 Bootstrap 并进入 Play，无需打包：

```powershell
Set-Location D:/luyuan/mmorpg-client
# 手动登录本机测试账号。
pwsh -File tools/run_festival_maps_local.ps1 -Editor -Gateway http://127.0.0.1:18185

# 或仅向新启动的 Editor 子进程注入隔离开发账号口令。
pwsh -File tools/run_festival_maps_local.ps1 -Editor `
  -LocalServerEnvFile ../xuanming-server-mmo-cpp/build/festival-maps-20261109/stack/.env

# 自动游历九图、切换 15 外观、移动、越界报告裁决并返回主城。
pwsh -File tools/run_festival_maps_local.ps1 -Editor `
  -LocalServerEnvFile ../xuanming-server-mmo-cpp/build/festival-maps-20261109/stack/.env `
  -Account dev_festival_verify -AutoVerify
```

这些是可选启动方式，不要同时打开同一主工程；脚本发现主工程已打开会拒绝，不关闭其它
编辑器副本。开发入口只接受回环 HTTP 网关与 gate，正式地图窗口的传送门禁仍保持关闭。
运行日志位于客户端 `Logs/festival-local-player-*.log`；自动模式另写
`.codex-artifacts/festival-local-verify-*/festival-local-verification.json` 和截图。
详细检查项见客户端 `Docs/FestivalMapsLocalTest20261109.md`。应逐图记录服务端入场通知、
场景模板/实例、外观与可见分片、权威移动终点、非法输入裁决和返回主城；请求日志或画面出现
都不单独作为通过证据。未遍历的视域外分片、故障注入、重连、跨区与人工画面验收另列未完成项。

### 逐地图结果登记

下表的已完成列来自 `festival_maps_20261109.json` 与本节离线证据，所有九图均已通过
真实 Detour 全格心比对、域内可达及域间不连桥检查。原生 15/15 是整批测试套件结果，
其中缺坏文件/48 格用例使用 `19004`、跨岛瞬移用例使用 `10000`，不冒充逐图网络验收。
E2E 列待本轮真实运行回执补充；所有行仍为 `clientOnly=true`。

| 地图 ID → 场景模板 | 已交付外观 | 可走格 / 平台域大小 | 导航产物 | 本轮 E2E / 特别未解决项 |
| --- | --- | --- | --- | --- |
| `01001` → 1001 | `spring`、`mid-autumn` | 250 / 250 | 已生成并核验 | 待双端实测 |
| `01002` → 1002 | `spring`、`mid-autumn` | 309 / 309 | 已生成并核验 | 待双端实测 |
| `05008` → 5008 | `spring`、`mid-autumn` | 2313 / 2313 | 已生成并核验 | 待双端实测 |
| `10000` → 10000 | `spring`、`mid-autumn` | 7268 / 2317、4951 | 已生成并核验 | 待双端实测；两域不通、画面接缝待修 |
| `19004` → 19004 | `mid-autumn` | 48 / 48 | 已生成并核验 | 待双端实测；仅此 48 个安全格 |
| `21003` → 21003 | `spring`、`mid-autumn` | 3218 / 3218 | 已生成并核验 | 待双端实测 |
| `23006` → 23006 | `mid-autumn` | 833 / 833 | 已生成并核验 | 待双端实测 |
| `28007` → 28007 | `spring` | 1481 / 1481 | 已生成并核验 | 待双端实测 |
| `35004` → 35004 | `spring`、`mid-autumn` | 2566 / 20、82、2464 | 已生成并核验 | 待双端实测；三个域不自动连桥 |

本轮改动尚未提交，不能把基础 HEAD 当作配套交付提交号。最终记录需同时填写客户端和
服务器实际提交号（未提交则明确注明）、构建输入摘要、镜像 ID、运行配置/导航 SHA、
逐图 E2E 回执和未解决项；不得仅用 Pull 成功或镜像标签代替这些身份信息。

## 2026-11-12 手绘图片地图的平面 OBJ 入口

图片分块只承担显示。客户端手绘可走区并扣除障碍后生成同一份 Unity Y-up、X/Z 地面
三角网：客户端由官方 NavMeshBuilder 烘焙，服务端继续使用本工具链的 UE5 double ABI
Recast/Detour。Unity NavMeshData 与 MSET v1 不直接互换。

为这种水平平面显式添加 `--profile authored-flat`，不能把通用三维 OBJ 或现有位图模式
默认为该模式。它要求 `--obj` 的所有顶点有限且具有相同 Y，并要求显式原始脚点探针。
Recast span 取整曾使 Y=0 的 OBJ 输出可走面位于 Y=0.2；专用模式在构建粗网格和细节
网格后将两者高度恢复为输入的原始水平面。没有移动 OBJ、出生脚点或改大 2cm 探针容差。
通用 OBJ 及旧位图的高度处理、有效默认参数和输出格式保持原样。

共享参数由客户端显式导出：`--cs`、`--agent-radius`、`--agent-height`、`--agent-climb`、
`--agent-slope`、`--tile-size`、`--min-region-area`。坡度单位为度，最小区域单位为世界
平方米，服务端使用 `ceil(area / (cs * cs))` 转为体素面积。手绘模式缺省 tile=256、
minRegionArea=0、slope=45；旧模式仍为 tile=128、64 个最小区域体素及 slope=50。
`--merge-region-area` 同样接受平方米，但仍属服务端专有，缺省为 400 个体素。
`--ch` 当前仍固定 0.2，`--filter-ledges 1` 保持显式；Unity 未公开独立 cellHeight，
因此不能把 ch、合并区域和过滤过程声称为精确等价。工具打印原始参数及半径/高度/台阶的
体素取整值；台阶按 `floor(climb/ch)` 取整，不能仅凭相同输入值声称最终边界与拓扑一致。

新增离线 `navmesh_query` 使用同 ABI 读取真实落盘 MSET：重复 `--point x,y,z` 查询
原点是否在网格、nearest，重复 `--route sx,sy,sz,ex,ey,ez` 查询完整路径、拐点及直线
阻挡。输入保持 Unity 坐标，输出 JSON；不启动服务器、不修改导航数据。它只支持当前
烘焙器的地面多边形，拒绝 off-mesh 连接和损坏/超限文件，不能代替运行端场景加载验收。

本轮离线构建及回归证据位于 `build/verification/navmesh-authored-flat-20261112/`，
包含原脚点失败及修复结果、原工具副本与 SHA、旧位图/矩形位图/通用 OBJ 的 MSET 字节
对比、新模式及真实落盘查询测试。未改正式 MSET、源表、出生高度、mask 或运行服务。
现有 `/festival_maps/` 的 mask/SHA/有效范围合同仍需在正式接入时另外对齐，不能仅将
新 OBJ 的 MSET 直接替换进去。同级独立的完整 Java 游戏服务器仍沿用现有合同，尚未
接入本次手绘 OBJ 模式；这不指当前本仓 C++ 栈使用的 `java/gateway_node` HTTP 网关。

### 手绘地图运行端合同（2026-11-12）

手绘地图使用独立的 `data/scene_nav_bin/authored_maps/<mapId>/scene.bin`，不替换旧节庆
位图目录。该路径与 `festival_maps/` 一样要求导航存在：缺失或拒绝装载时，场景入场和
移动均拒绝，不能回退为无阻挡。首批 26000 对应中秋、26008 对应春节；空白的 26008
中秋草稿不被当作已交付导航，具体外观、作者资产、同源 OBJ、参数和出生点记录在交付清单。

每份 MSET 必须配两个文件：`scene.bin.authored` 为 ASCII 文本，完整格式如下；
`scene.bin.sha256` 为 MSET 与 `.authored` 的两个小写 SHA-256，以空白分隔，不能有其它字段。

```text
ANAV 1
minX minZ width height groundY
```

范围沿用作者原世界矩形，所有字段有限、宽高为正且上界可表示；有效矩形为左下包含、
右上不包含。当前 `groundY` 必须为零。读取拒绝缺字段、未知版本和尾随垃圾，SHA 在
Detour 解析前核对，并拒绝同目录残留 `scene.bin.mask`。加载后对所有粗/细节顶点检查
原始平面高度，表中的出生点再经真实查询检查，不能用附近网格吸附来掩盖错误出生点。

手绘模式的原点与 nearest 在 X/Y/Z 每轴最多相差 2cm，原脚点还须处于有效矩形及原地面。
移动直线阻挡由真实 Detour raycast 裁决，完整路径必须到达终点多边形；断岛的 partial
结果拒绝。该模式不再加载或校验旧位图，也不保留第二份旧 mask 作为平行可走真相。
旧位图模式的格线/对角线和脚底高度兼容规则保持原样。换轴仍只在 NavQuerySystem 的
服务器 Z-up 与导航 Unity Y-up 边界完成，OBJ、合同范围和 baker 探针不重复换轴。

这里规定的是运行装载与查询合同；源表正式导出、构建镜像、导航文件部署及真实进场/移动
证据须分别登记，不能由离线查询通过推断已经更新当前服务器。

### 本地开发数据与原生验证结果（2026-11-12）

本次开发 overlay 在原 21 行 BaseScene 上追加两行，未替换正式源表及原有地图。26000
使用中秋作者源，Unity 脚点为 `(110.921211,0,33.47244)`，服务器表坐标为
`(33.47244,110.921211,0)`；26008 使用春节作者源，开发测试脚点为
Unity `(122.5,0,127.5)`、服务器 `(127.5,122.5,0)`。26008 草稿尚未设置出生点，后者
来自其已烘焙最大连通域的安全测试位置，只记录在交付清单及开发配置，未写回用户草稿。
两图范围分别为 `(0,0,300,240)`、`(0,0,300,225)`；作者的 `boundsConfirmed=false`
保持，不能将本次开发试图当作正式业务世界比例已验收。26008 中秋仍未交付导航。

Windows 当前 modules、scene、bag_test 依赖按 `/m:1` 实际构建完成，完整裸指针门禁通过。
为恢复既有门禁，仅将 `asset_op_system.cpp` 故障日志结构中的 `const char* table` 改成
`std::string table`，没有改资产业务；导航读取 Detour tile 使用公开的 const 重载。
24 项相关原生测试全部通过，包含 9 项手绘装载/坏合同/缺文件/残留 mask/原脚点/半开边界
回归及既有节庆、断岛和出生测试。9 项手绘故障注入使用原有已交付网格的隔离副本，不冒充
两张用户地图的全面路线验收；另一项真实 `LoadNavBins` 测试则使用新开发 overlay 全部
23 行，逐项核对真实导航登记和原出生脚点，确实加载了两图当前安装的 authored MSET。

负对照使用 HEAD 旧 navigation/nav_query 源在忽略目录编译，并优先链接到专用测试副本；
同一 9 项手绘回归均在目标行为断言上失败。当前绿色测试可执行文件原件 SHA 前后相同，
没有覆盖当前程序或修改主源码来取红。早期旧依赖链接、既有裸指针门禁和最小测试配置的
失败日志均保留，不把这些环境/构建失败当作行为回归证据。

证据位于 `build/verification/authored-nav-runtime-20261112/`：`native-summary.json`、
`green-native.xml/.log`、`counterfactual-red.xml/.log`、`counterfactual-inputs.json`、
`final-source.json` 和构建日志。这里证明本地 C++ 开发 overlay 的原生装载与查询行为；
当前运行栈是否更新及真实游戏交互由部署回执另记。同级独立的完整 Java 游戏服务器仍未
接入本次手绘导航；本仓当前栈的 `java/gateway_node` 网关继续承担 HTTP 入口，不解析
MSET。客户端打包及发布画面也不属于这些原生测试的验收范围。

### 本机开发栈部署与后端联机验证（2026-11-12）

本机开发栈本轮更新已通过最终部署检查，回执状态为 `DEPLOYED_AND_VERIFIED`：
先更新 gate、scene、battle，再完成 login-sql-boot 及 scene-manager、player-locator、
login 更新。三 C++ 节点共用构建标签 `debug-20261112t140808.551745z`，不可变镜像 ID 为
`sha256:bfb8190e6ec06cfb9a4f14e1899868b55bec9d0f843cae119858a8262d342804`。
六个业务服务的源码与运行数据检查全部通过，scene 启动日志实际登记了 26000 和 26008；
16 份运行文件完成 SHA 核对，其它容器身份保持，历史 one-off 容器保留。详细回执为
`build/verification/authored-festival-20261112/deployment-20261112-r2/result.json`，
配套 `source-and-runtime-data-check.json`、`scene-authored-navigation.log` 与日志来源记录。

开发登录入口为 `http://127.0.0.1:18085`，区号为 1，测试场景模板为 26000 中秋与
26008 春节。此 HTTP 网关是当前本仓 C++ 栈中的 `java/gateway_node`，继续复用当前入口；
它不解析 MSET。尚未接入本次 ANAV 的是同级独立完整 Java 游戏服务器，不是此网关。
客户端接入及打包按同级客户端 `Docs/Handoffs/Doubao-FestivalAuthoredNavMesh-20261112.md`
读取共同 delivery、脚点、外观和地址，26008 中秋仍未交付导航。

后端网络 E2E 已通过：`TestAuthoredFestivalNavMeshE2E` 实际运行 44.22 秒，完成当前
HTTP 登录、两图真实进场及移动验证，报告中 `26000-mid-autumn`、`26008-spring` 均为
PASS。证据为 `build/verification/authored-nav-e2e-20261112/actual-e2e-r2.json` 和
`network-r2.log`。首份探针采用旧认证 fixture，在登录阶段收到 2000 拒绝，尚未进入地图；
修正独立测试为当前 HTTP 与开发认证约定后得到本次 r2 结果，首份失败证据仍保留。

这些结果覆盖本机 C++ 开发 overlay 的部署和后端网络行为。两图仍保留作者未确认的
世界尺度状态，正式源表及旧地图未被开发配置替换。真实 Unity Player 的导航接入、
客户端打包、游戏内试走及画面验收由豆包继续完成；本轮后端 E2E 没有运行 Unity Player。
安全部署、E2E 和原生收据副本已交到客户端
`Docs/VerificationEvidence/authored-festival-delivery-20261112/`，与前述作者源和共同交付清单配套。

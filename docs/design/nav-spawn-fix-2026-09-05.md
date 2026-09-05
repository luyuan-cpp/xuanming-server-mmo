# 客户端/服务端导航数据、出生点与位置纠正契约统一(2026-09-05)

状态:**代码就位、客户端离线 Roslyn 编译通过;服务端 C++ 与烘焙器未编译、导航未重烘、未验收 —— 见 §5 Codex 执行清单。**
基线:`xuanming-server-mmo` HEAD `d08843042` + 本次未提交改动;`mmorpg-client` HEAD `99d891f` + 本次未提交改动。

## 1. 问题(已确认的证据)

- 服务端 `data/scene_nav_bin/main_scene.bin`(及 dungeon/mirror 两份同 md5)仍是 20648 字节的 UE 时代占位导航,
  与天墉城 400×300 地图无关;仓库有烘焙器 `tools/navmesh_baker` 但**从未编译运行过**(`scene-navmesh-pipeline.md` §6)。
- 角色 `281594980286332928`(账号 `robot_move_self_20260905_0734`)的 Redis 存档 Transform 为服务器坐标 `(0,0,0)`
  —— 新号的 proto 默认值,`PlayerDatabaseMessageFieldsUnmarshal` 原样 emplace,进场无任何校验。
- 客户端 `TianyongMapRuntime.AttachLocalController` 发现出生点不可走会**本地**把角色挪到 Unity `(200,0,180)`,
  但没有告诉服务器;服务器仍在 `(0,0,0)`。
- 移动诊断日志 `E:/work/image/movement_diagnostics/move_20260905_082427.log:293` 记录首次移动后
  `[move] reconcile snap input_seq=1`:移动包发出、收到了 MoveAck、客户端被硬拉。

故障链(最吻合现象):客户端本地放到城内 → 首次 MoveStart 上报城内坐标 → 服务端从 `(0,0,0)` 用旧导航裁决
(`ApplyReportedLocation`:起点不在网格 → 吸附上报点 → 旧网格里吸不到 → `accepted = current = (0,0,0)`)→
MoveAck 回原点 → 客户端 `WarpTo` 的 `ClampXZ(…, 2)` 钳到 `(2,0,2)` → 该处不在 WalkMask 上 → WASD/寻路全被客户端
mask 挡住、镜头跟丢。
**"该次回包目标恰为原点"此前没有日志能证实**(旧 ack 日志不带坐标),本次已补坐标日志(§3 客户端第 5 条),
Codex 验收时可回放旧包看。

## 2. 契约(两端唯一真相)

| 项 | 值 | 出处 |
|---|---|---|
| 阻挡真相 | 客户端 `TianyongPaintedCity.WalkMaskBase64`(150×150,2m/格,x∈[50,350] z∈[0,300]) | `TianyongPaintedCity.cs` |
| 服务端导航 | 用同一份 mask 经 `tools/navmesh_baker --painted-city` 烘成 `MSET` v1(ue5navmesh 同源码,dtReal=double) | `scene-navmesh-pipeline.md` |
| 换轴 | Unity `(x,y,z)` → 服务器 `(x'=z, y'=x, z'=y)`;导航在 Unity 系,`nav_query.cpp` 内部换轴 | `WorldCoordinateConverter.cs` / `nav_query.h` |
| 出生点 | Unity `(200,0,180)` == 服务器 `(180,200,0)`;服务器权威 | `TianyongMapDefinition.DefaultSpawn` / `spatial/constants/nav.h` `kTianyongSpawn*` |
| 出生点探针 | 烘焙器出包前、scene 节点加载时都用 `findNearestPoly`(±2m 水平 / ±4m 垂直)校验出生点在网格上 | `navmesh_baker.cpp ProbeNavMesh` / `navigation.cpp` |
| 纠偏 | `MoveAck.server_location` 恒为网格上的合法点(或无导航时的上报点本身) | `player_movement_handler.cpp` |

mask 已离线解码核对:出生格 `cx=75, cy=59` 可走;沿 z=180 的可走走廊是 x∈[190,206],四周最近的墙:西 x=188、东 x=208、
南 z=164、北 z=200(客户端撞墙验收就用这些)。

## 3. 改动清单

### 服务端 `xuanming-server-mmo`

1. `cpp/libs/services/scene/spatial/constants/nav.h`:新增 `kTianyongSpawnX/Y/Z = (180, 200, 0)` 及换轴注释。
2. **新增** `cpp/libs/services/scene/spatial/system/scene_spawn.{h,cpp}`:`SceneSpawnSystem`
   - `DefaultSpawnFor(sceneConfigId)`:当前所有场景同一张图,统一返回天墉城出生点(分场景后改读 BaseScene 表)。
   - `ResolveSpawnOnMesh(nav, id)`:出生点吸附到网格;无导航/吸不上时 ERROR 并返回未吸附点(不卡人)。
   - `EnsureValidEnterLocation(player, scene)`:进场落位 —— 在网格上则只写回吸附点;不在网格上(含 `(0,0,0)`
     与旧图坐标)则落到出生点;场景无导航时只修全零的"未初始化"位置。写 Transform + 置脏位 + INFO 日志
     `EnterScene spawn: player … location (…) off navmesh -> spawn (…)`。
   - `FallbackLocationForPlayer(player, nav)`:移动裁决"两头都不在网格"时的兜底点(出生点)。
   - 已登记 `scene.vcxproj` / `.filters` / `CMakeLists.txt`。
3. `cpp/libs/services/scene/player/system/player_scene.cpp` `HandleEnterScene` 第 3.5 步:绑定场景之后、自身
   ActorCreate(4.5)与 AOI 之前调用 `EnsureValidEnterLocation`。跨场景/跨 zone 的合法坐标原样保留(吸附成功即不改)。
4. `cpp/nodes/scene/handler/rpc/player/player_movement_handler.cpp` `ApplyReportedLocation`:
   "服务器当前位置与上报位置都不在网格" 分支从 `accepted = current`(把非法原位回给客户端 → 拉进墙)改为
   `SceneSpawnSystem::FallbackLocationForPlayer`;每次发 MoveAck 时打 INFO
   `move corrected: entity=… seq=… verdict=blocked|guided|respawned current=(…) reported=(…) accepted=(…)`。
5. `cpp/libs/services/scene/spatial/system/navigation.cpp` `LoadNavBins`:注册前探针出生点;**不在网格上的 bin
   (旧占位数据)拒绝注册并 ERROR**(fail-open:该场景无导航、移动不校验,但玩家不会被拉进墙);注册成功打
   `nav registered for scene N: … tiles=T orig=(…) tile=WxH spawn=(180,200,0) snapped=(…)`。
6. `tools/navmesh_baker/navmesh_baker.cpp`:新增 `--probe x,y,z`(Unity 坐标,可多个)与 `--no-default-probe`;
   `--painted-city` 模式默认探针 `(200,0,180)`;任一探针不在网格 → 不写文件、退出码 1。探针范围与
   `nav_query.cpp kSnapExtents` 一致,保证"烘焙器说在网格上 ⇔ scene 说在网格上"。

### 客户端 `mmorpg-client`

1. `TianyongNavigationGrid.TryFindNearestWalkable(world, out result)`:公开"最近可走点"。
2. `TianyongPlayerController`:
   - `WarpFromServer(feet)`:服务器权威 snap;落点不在 mask 上时恢复到最近可走格并 `ReportPositionToServer()`
     (发 MoveStop 让服务器采纳,服务端 `guided` 分支会吸附它),返回是否发生了恢复。
   - `ReportPositionToServer()`:在当前脚底点发一次 MoveStop。
   - `Navigation` 属性;`SetDebugDirection(dir)`(等价按住 WASD,不受 UI 键盘门控)与 `SetDebugIgnoreMask(bool)`
     (验收专用:绕过客户端 mask 硬冲墙,证明服务端还会挡)。`Update` 里脚本方向优先于键盘。
3. `ActorWorld.Teleport` 本地分支改调 `WarpFromServer`(MoveAck reconcile 与 TeleportS2C 都走这里)。
4. `TianyongMapRuntime.AttachLocalController`:服务器出生点不可走时(现在只剩兜底意义)改为最近可走点 +
   `ReportPositionToServer()`,并打 Warning —— 不再"本地挪了不告诉服务器"。
5. `GameClient`:每个 MoveAck 打 `[move] ack input_seq=N server=(x,y,z) unity=(…) local=(…) dist=D`;snap 时打
   `[move] reconcile snap input_seq=N to=(…) after=(…)`;自身 ActorCreate 打 `[actor] self entity=… server=(…) unity=(…)`;
   新增 `MoveAckCount` / `MoveReconcileCount` / `OnMoveReconcile`。
6. `DevAutoPilot -moveTest [-quitOnMoveTestEnd] [-moveTestTimeout 90]`:进场后按脚本走
   出生点检查 → 四向 WASD 各 1.2s → 客户端撞最近的墙 → 绕过 mask 硬冲同一堵墙 1.5s(服务器必须回 MoveAck)→
   点击寻路到 ≥8m 的可达点;每步 `move: …` 日志,末行 `RESULT=PASS stage=move_test spawn=(…) final=(…) acks=N snaps=0`。
   判 FAIL 的条件写在 `MoveTestRoutine` 注释里(出生点不可走 / 合法移动被 snap / 穿墙 / 冲墙无 ack / 寻路未到达)。
7. `tools/run_move_test.ps1`:同一账号跑两轮(R1、R2)+ 可选新账号(NEW),断言 RESULT=PASS、出生点可走、
   R2 出生点 == R1 终点(≤2m,重登落位)、NEW 出生点 == (200,0,180)(≤2m),退出码 0/1。

## 4. 验收标准(对应用户四条)

| # | 要求 | 判据 | 证据位置 |
|---|---|---|---|
| 1 | 服务端导航与客户端 WalkMask 一致,实际场景加载正确数据 | 烘焙器 stdout `probe unity=(200.00,0.00,180.00) … ON MESH`、`saved …: T tiles`(T>0);三份 bin 尺寸不再是 20648;scene 启动日志 `nav registered for scene 1 … tiles=T`,**没有** `skip nav registration` | 烘焙器 stdout / scene stdout |
| 2 | 服务器决定合法出生点,旧存档无效位置被处理 | 旧账号首登 scene 日志 `EnterScene spawn: player 281594980286332928 … (0,0,0) unset (0,0,0) -> spawn (180,200,0)`;客户端 `[actor] self … server=(180.00,200.00,0.00) unity=(200.00,0.00,180.00)`;新账号同样 | scene stdout / 播放器日志 |
| 3 | 纠正后不卡在不可走区域 | `run_move_test.ps1` 全部 `move: … walkable=True`;`server_wall` 阶段 `acks≥1` 且 `walkable=True`;全程无 `[TianyongPlayerController] server snap … is not walkable` Warning(出现说明服务器给了 mask 外的点,要查) | 播放器日志 |
| 4 | 新/旧角色:登录、首次移动、连续 WASD、点击寻路、重登正常;合法道路不回拉,墙体阻挡有效 | `run_move_test.ps1` 输出 `[move] PASS`:R1/R2/NEW 三轮 `RESULT=PASS … snaps=0`;`relogin … distXZ ≤ 2`;`wall … along ≤ wall_at+0.5`;`server_wall … acks≥1` | 脚本汇总 + 三份日志 |

## 5. Codex 执行清单(串行)

所有 shell 先 `. E:\work\tools\buildenv.ps1`。开工前做并行会话体检(memory `parallel-sessions-same-repo`):
`tasklist | findstr /i "gate.exe scene.exe battle.exe"`、`bin\*.exe` mtime;客户端先跑离线编译体检(§5.4 第 1 步)。

### 5.1 烘焙器构建(工作目录 `E:\work\xuanming-server-mmo`)

cmake 不在 PATH,用 VS 自带的:

```powershell
$cmake = "D:\Program Files\Microsoft Visual Studio\18\Enterprise\Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe"
& "D:\Program Files\Microsoft Visual Studio\18\Enterprise\Common7\Tools\Launch-VsDevShell.ps1" -Arch amd64 -SkipAutomaticLocation
& $cmake -S tools/navmesh_baker -B tools/navmesh_baker/build -A x64
& $cmake --build tools/navmesh_baker/build --config Release
```

通过:`tools\navmesh_baker\build\Release\navmesh_baker.exe` 存在、零 error。若默认生成器选不到 VS 18,
加 `-G "Visual Studio 18 2026"`(`cmake --help` 看生成器名)。烘焙器从未编过,编译错误直接贴回(本文档 §6 列了
已静态核对过的 API 面,报错多半是 UE fork 的签名差异,按头文件改烘焙器即可,**不要改 third_party**)。

### 5.2 烘焙 + 探针 + 落盘

```powershell
tools\navmesh_baker\build\Release\navmesh_baker.exe --painted-city ..\mmorpg-client\Assets\Scripts\World\Tianyong\TianyongPaintedCity.cs --out data\scene_nav_bin\tianyong_scene.bin
Copy-Item data\scene_nav_bin\tianyong_scene.bin data\scene_nav_bin\main_scene.bin -Force
Copy-Item data\scene_nav_bin\tianyong_scene.bin data\scene_nav_bin\dungeon_scene.bin -Force
Copy-Item data\scene_nav_bin\tianyong_scene.bin data\scene_nav_bin\mirror_scene.bin -Force
Get-Item data\scene_nav_bin\*.bin | Select-Object Name, Length, LastWriteTime
```

通过:stdout 依次有 `mask geometry: N quads`、`bounds: (50.00 … )-(350.00 … ), tiles 10x10`(32m tile)、`built T tiles`(T>0)、
`probe unity=(200.00,0.00,180.00) server=(180.00,200.00,0.00): ON MESH nearest=(…)`、`saved …: T tiles`;退出码 0;
三份 bin 同尺寸且 ≠ 20648。探针 `OFF MESH` 则不落盘,把 stdout 全文贴回(mask 映射或换轴出了问题,不要绕过探针)。

### 5.3 scene 库 + scene 节点编译、替换、重启

```powershell
$msb = "D:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe"
& $msb cpp\libs\services\scene\scene.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64
& $msb cpp\nodes\scene\scene.vcxproj        /m:1 /p:Configuration=Debug /p:Platform=x64
```

节点 post-build 拷 `bin\scene.exe` 时若 scene 在跑会报 MSB3073:先只停 scene(`Get-Process scene | Stop-Process -Force`,
**不要**用 `dev.bat`/`go_services.ps1 stop`,会连坐 gate/battle),≥5s 后再编或手动把 `build\cpp\nodes\scene.exe` 拷到 `bin\`。
重拉 scene(经 WMI 逃逸,显式端口;z1 20000/20001,z2 22000/22001,`NODE_IP=192.168.43.7`,`ZONE_ID`/`RPC_PORT` 环境变量),
模板见 `C:\Users\luyua\AppData\Local\Temp\claude\E--work\3f3f6928-3172-48ea-9ae4-d84fff460133\scratchpad\relaunch_cpp_nodes.ps1`
(只取 scene 那几行)或 `tools/scripts/cpp_nodes.ps1 -Command start -Nodes scene -SceneCount 2`。
起来后 `http://127.0.0.1:8081/api/server-list` 两个 zone `OPEN`,scene stdout 里:

- 有 `nav registered for scene 1: ../data/scene_nav_bin/main_scene.bin tiles=T … spawn=(180,200,0) snapped=(…)`;
- **没有** `skip nav registration` / `nav bin load failed`。出现 `spawn point off navmesh — stale/mismatched nav bin`
  = bin 没换成功或 `DataRootDirectory`(`bin/etc/base_deploy_config.yaml`,`../`)指错。

### 5.4 客户端出包(编辑器开着时用副本工程)

1. 离线编译体检(~2s,退出码 0 = 绿):
   `pwsh -File E:\work\mmorpg-client\tools\client_compile_check.ps1`
   (本次已跑过 `exit=0 files=132 errors=0`;不绿说明并行会话留了半成品,轮询等它,别改对方文件。
   引用/宏在 `tools/compile_check/runtime_refs.rsp`,Unity 升版本后要从 `MmorpgClient.csproj` 重抽)。
2. 同步到副本工程并出包(与 `showcase_player` 同一条产线,产物覆盖 `E:/work/tmp/showcase_player/mmorpg.exe`):

```powershell
robocopy E:\work\mmorpg-client\Assets          E:\work\tmp\shotverify_project\Assets          /MIR /NFL /NDL /NJH /NJS
robocopy E:\work\mmorpg-client\ProjectSettings E:\work\tmp\shotverify_project\ProjectSettings /MIR /NFL /NDL /NJH /NJS
robocopy E:\work\mmorpg-client\Packages        E:\work\tmp\shotverify_project\Packages        /MIR /NFL /NDL /NJH /NJS
& "C:\Program Files\Unity\Hub\Editor\6000.5.8f1\Editor\Unity.exe" -batchmode -nographics -quit -projectPath E:/work/tmp/shotverify_project -executeMethod MmorpgClient.Editor.ShowcaseBuild.Build -showcaseOut E:/work/tmp/showcase_player -logFile E:/work/tmp/showcase_build.log
Select-String -Path E:\work\tmp\showcase_build.log -Pattern "\[ShowcaseBuild\] result=|error CS" | Select-Object -Last 5
```

通过:`result=Succeeded`、`mmorpg.exe` mtime 更新。robocopy 退出码 ≤7 都算成功。副本 batchmode 首轮导入可能十几分钟,
Unity.exe 自身 CPU 不涨是正常的(memory `unity-copy-project-upm-hang`)。

### 5.5 移动验收(需 5.3 的 scene 与 5.4 的播放器都是新版)

```powershell
pwsh -File E:\work\mmorpg-client\tools\run_move_test.ps1 -Account robot_move_self_20260905_0734 -FreshAccount ("robot_move_new_" + (Get-Date -Format HHmmss))
```

密码 `123456`(login 的 `LOGIN_DEV_PASSWORD_SHARED_SECRET`)。通过:末行 `[move] PASS`。三份日志在 `E:/work/tmp/move_test/`。
失败时把 `[move]` 汇总 + 对应日志里全部 `[AutoPilot]` / `[GameClient] [move]` / `[actor] self` 行 + scene stdout 里
`EnterScene spawn` / `move corrected` 行贴回。
补充人工核对(可选):手动起播放器进游戏,城内街道 WASD/点击各走一段,scene stdout 不应出现 `move corrected`;
朝建筑走应被客户端挡住,也没有 `move corrected`。

## 6. 静态核对过的 API 面(供编译报错时定位)

烘焙器与 scene 侧改动只用到 ue5navmesh 这些符号,均已按 `third_party/ue5navmesh/Public` 头文件核对签名:
`rcCreateHeightfield / rcMarkWalkableTriangles / rcRasterizeTriangles / rcFilterLowHangingWalkableObstacles /
rcFilterLedgeSpans / rcFilterWalkableLowHeightSpans / rcBuildCompactHeightfield / rcErodeWalkableArea /
rcBuildDistanceField / rcBuildRegions / rcBuildContours / rcBuildPolyMesh / rcBuildPolyMeshDetail / rcContext::doLog(const rcLogCategory, const char*, const int)`、
`dtNavMesh::init/addTile(data,size,flags,lastRef,result*)/getTile(i)/getTileRef(tile)/getMaxTiles()/getParams()`、
`dtNavMeshQuery::init(nav,maxNodes)/findNearestPoly(center,extents,filter,ref*,pt*)`、`dtNavMeshParams{walkableHeight,walkableRadius,walkableClimb,bvQuantFactor,orig,tileWidth,tileHeight,maxTiles,maxPolys}`、
`dtCreateNavMeshData`、`dtReal=double`、`NAVMESH_API` 为空宏。

## 7. 边界与后续

- 出生点仍是代码常量(`nav.h`),因为所有场景配置共用一张图;分场景时把 `spawn_x/y/z` 加进 `data/BaseScene.xlsx` +
  `data/schema/basescene_table.proto`,`SceneSpawnSystem::DefaultSpawnFor` 是唯一读取点。
- `SceneNavManager` 仍是 thread_local、`LoadNavBins` 只在配置加载线程跑(`scene-navmesh-pipeline.md` §5 原有边界)。
- 客户端仍无预测回滚;>1.5m 的 ack 才 snap。`WarpFromServer` 的"最近可走格 + 回报"是对**服务器给了 mask 外的点**的兜底,
  正常数据下不应触发(触发即打 Warning,验收时视为需要查的信号)。
- 旧存档不需要手工改 Redis:下次登录 `EnsureValidEnterLocation` 会落到出生点,退出时按新位置存盘。
- 反作弊面不变(速度截断 + 阻挡夹持);`SetDebugIgnoreMask` 只有 DevAutoPilot 会调,真实输入路径不受影响。

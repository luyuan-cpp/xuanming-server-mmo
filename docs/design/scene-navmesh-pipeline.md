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
| cs | 0.25m | mask 格 2m 的整数分之一,导航边界与 mask 格线精确重合 |
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

- 首进场 spawn 契约仍是 (0,0,0)(TianyongMap.md 记录的缺口)。handler 对
  "服务器当前位置不在网格上"做了引导落位兜底(吸附客户端上报点),但正解
  是进场时按场景表出生点落位。
- 客户端未做预测回滚,MoveAck 纠偏 >1.5m 硬 snap(GameClient.cs 现状)。
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

# 水车庭院（`waterwheel-courtyard` / 场景 20000）双版服务端导航与联机配表接入设计方案

> 日期：2026-11-28  
> 适用仓库：`xuanming-server-mmo`（C++/Go 权威仓）、`xuanming-server-mmo`（Java 平行仓）、`mmorpg-client`（Unity 客户端）

---

## 1. 需求分析

1. 承接图仓提交 `b36c0513f9b162919e7d2847bad81a08a03eec9c` 的水车庭院（`pc:20000` / `designs/pc-20000-waterwheel-hd8-festivals-v001/`）春节/中秋双节正式交付，将水车庭院从单机预览（`clientOnly: true`）升级为三端联机可进图的正式主世界场景（`sceneId = 20000`）。
2. 离线烘焙 C++ 权威 `scene.bin`（MSET）+ `scene.bin.mask`（FMASK）+ `scene.bin.sha256` 双行校验和，与客户端双节 `320×320` 走图掩码逐格零失配。
3. 在 `BaseScene.xlsx` 与 `World.xlsx` 同步登记行 `id = 20000`，经官方导表器生成双端 `basescene` / `world` 产物并通过 `ContractSync --scope current-maps` 同步至 Java 独立服务端，同时将客户端 `QdaoMaps20261102/catalog.json` 中的 `waterwheel-courtyard` 切换为 `clientOnly: false`，完成三端联机进图闭环。

## 2. 设计分析

### 2.1 场景身份与坐标契约
- 场景编号沿用独立核定的 `sceneId = 20000`（`key = "waterwheel-courtyard"`，`revision = "waterwheel-hd8-20261128"`），与现有 `BaseScene` 1–21 及 `festival_maps_20261109.json` 零冲突。
- `BaseScene.xlsx` 新增行 `id = 20000`（绑定 `data/scene_nav_bin/current_maps/waterwheel-courtyard/waterwheel-hd8-20261128/scene.bin`），`World.xlsx` 同步新增行 `id = 20000, scene_id = 20000`，使 C++ `RequestZoneTravel`、Go `scene_manager` 与 Java `game-scene` / `game-scene-manager` 均自动放行并建立场景 20000 世界分线。
- 坐标换轴遵循 `nav.h` / `WorldCoordinateConverter` 唯一契约（服务端 Z-up `(x, y, z) = Unity (z, x, y)`）：
  - Unity 出生点：`(160.0, 0.0, 140.0)`
  - 服务端 `BaseScene.xlsx` 行 `20000` 出生点：`spawn_x = 140.0, spawn_y = 160.0, spawn_z = 0.0`
- 世界与有效矩形：
  - `worldRect = (0, 0, 320, 320)`（每块 1024px 切片 = `40×40` 世界单位，`25.6 px/u`）
  - `effectiveWorldRect = (0, 32.578125, 297.1484375, 287.421875)`（对应 `7607×7358` 有效画幅，裁除右侧 `585 px` 与底部 `834 px` 填充边）
  - 走图栅格：`320×320`（`cellSize = 1.0`，`15,357` 可走格，单连通分量，20 地标探针、19 连通路径、17 阻挡探针）

### 2.2 导航烘焙体素精度（`--cs 0.125`）
- 沿用青岚镇已验证的 `--cs 0.125` 精细体素，准确覆盖 `effectiveWorldRect` 右/底亚格边条（`0.1484375u` / `0.421875u`）。
- 实测 Recast 生成 `106` 块 tile（`148,832` 字节），全部 `102,400` 格 `mismatches=0`、单连通分量 `15,357` 格 `cross_component_bridges=0`、全部 `20` 个地标探针准确落网。

### 2.3 双版 Java 服务端同步
- 扩展 `tools/ContractSync.java --scope current-maps` 一并同步 `World` 表（`config-data/tables/world.pb` 与 `maps.world.pb.sha256`），确保 `BaseScene`、`World`、`manifest.json` 与 `config-data/navmesh/current_maps/` 原子一致且零干扰其他在途契约。
- `ConfigSceneTables.java` 自动按 `loadVersionedCurrentMap` 装载 `waterwheel-courtyard/waterwheel-hd8-20261128`，在 `CurrentMapNavigation.java` 补充 `WATERWHEEL_FOLDER` 常量并在 `ConfigSceneTablesTest` / `CurrentMapNavigationTest` 补齐场景 20000 回归断言。

## 3. 思想分析

- **零核心逻辑侵入**：C++ 侧复用 `NavDataContract::VersionedMask`（MSET + FMASK + 双行 SHA-256），Go/Java `scene_manager` 与 `game-scene` 复用 `World` / `BaseScene` 通用装载链路，无需新增特例分支。
- **双端同源、失败关闭**：三仓共用同一份 SHA-256 绑定的 `walkmask.txt` 与 `source.json`，任一校验失败拒绝注册并阻断入场。
- **最小改动边界**：保留灵潮池 1+3 默认配置与并发会话既有改动，不顺手扩展未要求的玩法。

## 4. 漏洞分析

1. **只改 `BaseScene.xlsx` 不改 `World.xlsx` 会导致联机传送被拒**：C++ `player_lifecycle.cpp`（`RequestZoneTravel`）、Go `scene_manager`（`worldConfIds`/`IsWorldConf`）与 Java `SceneWorld` 均校验目标场景必须登记在 `World` 表中；已同步追加 `World.xlsx` 行 `20000 -> 20000` 并扩展 Java `ContractSync --scope current-maps` 同步 `world.pb`。
2. **本机 WinGet `protoc 35.0` 版本污染风险**：仅改动数据行、零 schema 变更；导表后已还原 protoc 版本头噪声目录，仅保留 `basescene.{json,pb}`、`world.{json,pb}`、`client_tables/{basescene,world}.pb` 与 `manifest.json`。
3. **有效画幅边缘外填充区越界**：右/底边缘填充区（`x >= 297.1484375`、`z < 32.578125`）在 C++ / Java / 客户端均严格阻挡，并在 `SceneSpawnTest` 与 `ConfigSceneTablesTest` 中覆盖断言。
4. **并发会话隔离**：三仓均存在青岚镇与其他专题的并行未提交改动，操作前后均经 `git status` / `git diff` 核验，严禁回退或覆盖无关文件。

## 5. 步骤分析

1. 切换客户端水车庭院 `catalog.json` / `source.json` / `navigation.json` 为 `clientOnly: false, authoritativeServerRegistered: true` 并重算摘要。
2. 新增 `tools/navmesh_baker/prepare_waterwheel_courtyard.py`（默认 `--cs 0.125`），烘焙并校验场景 20000 `scene.bin` / `.mask` / `.sha256` / `import-receipt.json`。
3. 更新 `data/BaseScene.xlsx` 与 `data/World.xlsx` 新增行 `20000`，运行官方导表器同步双端产物并清理 protoc 版本噪声。
4. 扩展 Java `ContractSync.java --scope current-maps` 支持 `world.pb`，执行同步与 `--check`，补充 Java 常量、单测与 `PARITY.md`。
5. 补充 C++ `festival_navigation_test.cpp` 与 `scene_spawn_test.cpp` 场景 20000 用例、落盘本设计文档并更新 `docs/PROGRESS.md` 与 `docs/README.md`。
6. 更新客户端 `CityTravelWindow.WorldMapV2.cs`、`CurrentMapTests.cs`、`CityTravelWindowTests.cs` 与 `FestivalRegionMapTests.cs`，在三仓分别执行编译与测试验证。

## 6. 执行步骤

1. 更新客户端水车庭院元信息并运行 `tools/navmesh_baker/prepare_waterwheel_courtyard.py`，核验 `102,400` 格零失配与 20 地标探针。
2. 在 `data/BaseScene.xlsx` 与 `data/World.xlsx` 追加行 `20000` 并执行 `python tools/data_table_exporter/run.py`，核验仅数据文件发生预期变化。
3. 在 Java 仓更新 `tools/ContractSync.java` 并执行 `--scope current-maps` 与 `--check`，补充 `CurrentMapNavigation.WATERWHEEL_FOLDER`、单测与 `PARITY.md`。
4. 补充 C++ 单测、设计文档、进度记录与索引。
5. 更新客户端联机大地图文案与 EditMode 用例。
6. 串行重编并实跑 C++ `bag_test.exe`（`SceneSpawnTest.*:FestivalNavigationTest.*`）与 `scene.exe --validate-tables`，运行 Go `scene_manager` 测试、Java `game-scene`/`game-scene-manager` 测试及客户端编译、契约测试与 Unity EditMode 回归。

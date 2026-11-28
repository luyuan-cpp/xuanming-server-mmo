# 青岚镇（`qinglan-town` / 场景 4）双版服务端导航与联机配表接入设计方案

> 日期：2026-11-28  
> 适用仓库：`xuanming-server-mmo`（C++/Go 权威仓）、`xuanming-server-mmo`（Java 平行仓）、`mmorpg-client`（Unity 客户端）

---

## 1. 需求分析

1. 承接图仓提交 `d36d5575a8905f9a6d9381c0be2e875b2463edb0` 的青岚镇（`original:legacy-lanxian-town`）春节/中秋双节正式交付，将 `BaseScene` 场景 `id = 4` 从旧版 `data/scene_nav_bin/lanxian_scene.bin`（`spawn = (180, 200, 0)`）升级为青岚镇双节共享严格导航。
2. 离线烘焙 C++ 权威 `scene.bin`（MSET）+ `scene.bin.mask`（FMASK）+ `scene.bin.sha256` 双行校验和，与客户端双节 `308×308` 走图掩码逐格零失配。
3. 经官方导表器同步 `BaseScene.xlsx` 产物至 C++/Go 服务端与 Unity 客户端，并通过 `ContractSync --scope current-maps` 同步至 Java 独立服务端，同时将客户端 `QdaoMaps20261102/catalog.json` 中的 `qinglan-town` 切换为 `clientOnly: false`，完成三端联机进图闭环。

## 2. 设计分析

### 2.1 场景身份与坐标契约
- `World.xlsx` 已预分配 `id = 4, scene_id = 4`，`go/scene_manager` 与 Java `game-scene-manager` 自动按 `World` 表创建场景 4 世界分线并承接 `EnterSceneC2S(scene_config_id=4)`，无需新增场景槽位或修改 `World.xlsx`。
- 坐标换轴遵循 `nav.h` / `WorldCoordinateConverter` 唯一契约（服务端 Z-up `(x, y, z) = Unity (z, x, y)`）：
  - Unity 出生点：`(-83.6923828125, 0.0, -65.1943359375)`
  - 服务端 `BaseScene.xlsx` 行 `4` 出生点：`spawn_x = -65.1943359375, spawn_y = -83.6923828125, spawn_z = 0`
- 世界与有效矩形：
  - `worldRect = (-154, -154, 308, 308)`（每块 1024px 切片 = `77×77` 世界单位）
  - `effectiveWorldRect = (-154, -95.197265625, 308, 190.39453125)`（裁除上下透明留白 `y < 782` 与 `y >= 3314`）
  - 走图栅格：`308×308`（`cellSize = 1.0`，`6,389` 可走格，单连通分量，21 地标、22 连通路径、26 阻挡探针）

### 2.2 导航烘焙体素精度（`--cs 0.125`）
- `effectiveWorldRect` 南边缘 `yMin = -95.197265625` 与整数格线 `z = -95.0` 之间形成 `0.197265625` 世界单位窄条。
- 实测默认 `--cs 0.25` 体素大于该边缘条宽，导致 `col=22, row=249` 处出现 1 格轮廓外溢被烘焙器安全拦截；采用 `--cs 0.125` 后，Recast 生成 103 块 tile（`206,024` 字节），全部 `94,864` 格 `mismatches=0`、单连通分量 `6,389` 格 `cross_component_bridges=0`、全部 21 个地标探针落网。

### 2.3 `WorldMapPlay` 灵潮池边界
- 按 [world-map-play-v2.md](../planning/world-map-play-v2.md) §1.1 D2「开服严控 1+3 常驻灵潮池」定调，`OpenWorldMapIds` 默认 `[1, 2, 3, 5]` 与 `TideCandidateMapIds` 默认 `[2, 3, 5]` 保持不变；玩家经大地图「游历」抽屉选择青岚镇（春节/中秋双景与分线）即可正常联机传送进入场景 4，后续需要纳入灵潮轮换时仅改配置项即可。

### 2.4 双版 Java 服务端同步
- 通过 `java tools/ContractSync.java --mmorpg <cpp仓> --scope current-maps` 单域同步 `basescene.pb`、`manifest.json`、`config-data/navmesh/current_maps/qinglan-town/qinglan-20261127-v002/{source.json,walkmask.txt}` 与 `contract/SOURCE.properties` 中的 `maps.*` 键，零干扰其他在途契约。
- `ConfigSceneTables.java` 已按通用 `loadVersionedCurrentMap` 装载所有 `current_maps/<key>/<revision>/scene.bin`，无需改写业务加载逻辑；在 `CurrentMapNavigation.java` 补充 `QINGLAN_FOLDER` 常量并在 `ConfigSceneTablesTest` / `CurrentMapNavigationTest` 补齐场景 4 回归断言。

## 3. 思想分析

- **零核心逻辑侵入**：C++ 侧复用 `NavDataContract::VersionedMask`（MSET + FMASK + 双行 SHA-256），Java 侧复用 `loadVersionedCurrentMap`，Go/Java `scene_manager` 复用 `World.xlsx` 既有槽位 4。
- **双端同源、失败关闭**：三仓共用同一份经过 SHA-256 绑定的 `walkmask.txt` 与 `source.json`，任一校验失败直接拒绝注册并阻断入场。
- **最小改动边界**：保留灵潮 1+3 默认配置与并发会话未提交改动，不顺手扩展未要求的玩法。

## 4. 漏洞分析

1. **边缘窄条体素精度失配**：`0.197m` 有效边界条宽小于 `0.25m` 导致单格漏判；已将 `prepare_qinglan_town.py` 默认 `--cs` 固定为 `0.125` 并通过全 94,864 格校验。
2. **本机 WinGet `protoc 35.0` 版本污染风险**：`tools/data_table_exporter/run.py` 使用 PATH 上的 `protoc` 重编译全部 `.proto`，若本地 `protoc` 为 `7.35.0` 而 C++ 头文件为 `7.35.1` 会导致生成代码版本宏不匹配；本次仅改动 `BaseScene.xlsx` 数据行（零 schema 变更），导出后已核对并还原 protoc 版本噪声目录，仅保留数据产物 `basescene.{json,pb}`、`client_tables/basescene.pb` 与 `manifest.json`。
3. **旧存档坐标 `(180, 200, 0)` 越界**：旧 `lanxian_scene.bin` 出生点落在青岚镇 `[-154, 154]` 之外；已在 `SceneSpawnTest` 与 Java `ConfigSceneTablesTest` 验证进图自动回落至新出生点 `(-65.1943359375, -83.6923828125, 0)`。
4. **并发会话隔离**：客户端工作区与 Java 仓均存在其他专题未提交改动，操作前后均经 `git status` / `git diff` 核验，严禁回退或覆盖无关文件。

## 5. 步骤分析

1. 客户端青岚镇批次切 `clientOnly: false, authoritativeServerRegistered: true`，更新 `CityTravelUiRoot` / `WorldMapV2` 文案与 `CurrentMapTests` / `CityTravelWindowTests` / `FestivalRegionMapTests` 并复跑 `import_festival_maps.py import` 核验哈希。
2. 新增 `tools/navmesh_baker/prepare_qinglan_town.py`（默认 `--cs 0.125`）烘焙场景 4 `scene.bin` / `.mask` / `.sha256` / `import-receipt.json`，下线旧 `data/scene_nav_bin/lanxian_scene.bin`。
3. 更新 `data/BaseScene.xlsx` 第 4 行，运行官方导表器同步双端 `basescene.pb` 与 `manifest.json`，清理 protoc 版本噪声。
4. 补充 C++ `festival_navigation_test.cpp` 与 `scene_spawn_test.cpp` 场景 4 用例、落盘本设计文档并更新 `docs/PROGRESS.md` 与 `docs/README.md`。
5. 在 Java 独立仓运行 `ContractSync --scope current-maps`，更新 `CurrentMapNavigation.java`、单测与 `PARITY.md`。
6. 在三仓分别执行编译、静态门禁、C++ `bag_test`、Go `scene_manager` 测试、Java Maven 测试与客户端编译/C# 导航校验。

## 6. 执行步骤

1. 更新客户端 `Docs/VerificationEvidence/qinglan-20261127-v002/batch.json` 并重跑 `import_festival_maps.py import`。
2. 编写并运行 `tools/navmesh_baker/prepare_qinglan_town.py`，核验 `94,864` 格零失配与 21 地标探针，删除旧 `lanxian_scene.bin`。
3. 更新 `data/BaseScene.xlsx` 行 `id = 4` 并执行 `python tools/data_table_exporter/run.py`，核验仅数据文件发生预期变化。
4. 补充 C++ 单测、设计文档、进度记录与索引。
5. 执行 Java 仓 `ContractSync --scope current-maps`，补充 Java 常量、单测与 `PARITY.md` 记录。
6. 串行重编 C++ `bag_test.exe` 并实跑 `SceneSpawnTest.*:FestivalNavigationTest.*`、跑 `scene.exe --validate-tables`、跑 Go `scene_manager` 测试、跑 Java `game-scene`/`game-scene-manager` 测试及客户端全量编译与运行时校验。

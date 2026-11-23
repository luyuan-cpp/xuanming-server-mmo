# 端游原图静态背景服务端核实与双端 ID 对账设计（2026-11-23）

## 1. 需求分析

- **交接来源**：`D:/work/image/.codex-artifacts/pc-original-static-backgrounds-20261123/GEMINI_CLIENT_SERVER_INTEGRATION_HANDOFF.md`。
- **任务目标**：配合客户端接入 36 张端游原尺寸静态 `back` 背景的本地查看与切换，在服务端仓库（`xuanming-server-mmo-cpp`，`main` 分支）核实全部 40 个端游目标 ID 与现有服务端地图编号、入口、传送、碰撞与导航数据的对应关系，建立可机检的双端对账清单与自动化校验脚本，并如实单独记录 4 个缺口 ID 与 2 个 HOLD 特殊备选。
- **硬性边界**：
  1. **严禁从背景 PNG 推断服务端数据**：不凭静态背景图臆造 `BaseScene.xlsx` 行、`World.xlsx` 入口、出生坐标、碰撞体、WalkMask 或 Recast/Detour `scene.bin`。
  2. **保持现有正式场景与开发导航不受污染**：现有 `BaseScene`（1–21）、`World`（1–16）、`festival_maps_20261109.json`（9 图）与 `authored_maps_20261112.json`（2 图）均保持原样，不提前开放这 40 个 ID 的联机传送（`productionTravelEnabled = false`）。
  3. **Java 版并行演进说明（遵循 `AGENTS.md §12`）**：本轮仅核实并登记静态原图预览对账事实，未改动协议、配表 schema 或正式场景运行行为；独立 Java 版服务端（`luyuan-java/xuanming-server-mmo`）同样未对这 40 个端游 ID 注册 `BaseScene`/`World`/导航，待后续完成道家 Q 版改绘与双端导航验收后再按统一契约同步接入。

## 2. 现有服务端地图数据核实结果（设计分析）

只读核查当前仓库 `HEAD` 的权威数据与源码：

| 服务端权威来源 | 现状核实 | 与本次 40 个端游 ID 的重叠 |
| --- | --- | --- |
| `data/BaseScene.xlsx` / `generated/tables/basescene.json` | 共 21 个场景模板：`1` 主城（`main_scene.bin`，出生 `(133, 199, 0)`）、`2` 八仙岛（`penglai_scene.bin`，出生 `(173, 195, 0)`）、`3` 东海渔村/月汐湾（`current_maps/village/yuexi-bay-20261115/scene.bin`，出生 `(36.73125, 20.19375, 0)`）、`4` 揽仙镇（`lanxian_scene.bin`，出生 `(180, 200, 0)`）、`5–16` 复用主城导航占位、`17–19` 副本、`20–21` 镜像。 | **0 个重叠** |
| `data/World.xlsx` / `generated/tables/world.json` | 共 16 条主世界入口行（`id 1..16 -> scene_id 1..16`），由 `go/scene_manager/internal/logic/world_init.go` 启动加载。 | **0 个重叠** |
| `data/scene_nav_bin/festival_maps_20261109.json` | 9 个开发期节庆导航 ID：`1001, 1002, 5008, 10000, 19004, 21003, 23006, 28007, 35004`（均 `clientOnly=true`、`productionTravelEnabled=false`）。 | **0 个重叠** |
| `data/scene_nav_bin/authored_maps_20261112.json` | 2 个开发期手绘平面导航 ID：`26000`（中秋）、`26008`（春节）。 | **0 个重叠** |
| 入口与传送机制 | 跨图切换统一走 `EnterSceneC2S(scene_config_id, scene_id=0)` 经 `scene_manager` 校验 `World`/`BaseScene`；`cpp/nodes/scene/handler/rpc/player/player_movement_handler.cpp` 中的 `TeleportRequest` 当前仍为未接入传送表的空桩。 | 40 个 ID 均无可用服务端入口与传送配置 |
| 碰撞与导航契约 | `cpp/libs/services/scene/spatial/constants/nav.h` 按目录前缀区分 `Legacy` / `ExactMask` / `VersionedMask` / `AuthoredFlat`，均要求明确 WalkMask 或权威 OBJ 烘焙的 MSET v1 与出生点探针通过。 | 40 个 ID 均无导航、无碰撞、无出生坐标 |

**特别辨析（防 ID 混淆）**：
- `05000`（天墉城，原图 `10000×6774`）：服务端当前正式主城模板为 `BaseScene.id = 1`（`main_scene.bin`，300×300m 旧布局）；`5000` 并未在 `BaseScene`/`World` 中登记，不能把 `05000` 静态原图与 `BaseScene 1` 的 300×300m 导航混同。
- `02000`（揽仙镇外，原图 `3634×4000`）：不等同于 `BaseScene.id = 4`（揽仙镇 `lanxian_scene.bin`）。
- 4 个缺口 ID（`29005`、`11005`、`41049`、`50024`）与 2 个特殊 HOLD 备选（`15000` 8000×8000 `gbits_jdhj` 视觉断崖 HOLD、`50024` 4000×3000 缺 `info.map` viewport 证据）单独登记在 `unavailableMaps` 与 `specialHoldCandidates` 字段中。

## 3. 思想分析

- **单一真相与防漂移**：沿用本仓库 `current_maps_20261009.json`、`festival_maps_20261109.json`、`authored_maps_20261112.json` 的成功范式，把端游原图预览批次也固化为一份带 SHA-256 绑定与显式状态机的 JSON 登记文件 `data/scene_nav_bin/pc_original_static_backgrounds_20261123.json`，配合 `tools/navmesh_baker/verify_pc_original_map_parity.py` 自动化核对：
  1. 服务端侧自检：确认 `BaseScene` / `World` / 既有导航清单未被误注入，确认 40 = 36 ready + 4 unavailable 划分严密，确认所有条目保持 `productionTravelEnabled = false` 与 `serverNavigationStatus = "UNPROVEN_STATIC_BACK_ONLY"`。
  2. 双端联动核对（传入 `--client-catalog`）：逐一比对客户端 `PcOriginalPreview20261123/catalog.json` 的 36 张地图 ID、原始像素尺寸、源 PNG SHA-256、客户端资源目录、分片数/格式以及 4 个缺口与 2 个 HOLD 备选，防止两端登记脱节。

## 4. 漏洞分析

1. **误把静态预览图当作可传送场景**：若后续开发者误以为 36 图已可联机入场，会导致 `EnterScene` 拒绝或玩家落入无导航空间。**缓解**：清单显式标记 `serverBaseSceneRegistered: false`、`serverWorldRegistered: false`、`productionTravelEnabled: false`、`serverNavigationStatus: "UNPROVEN_STATIC_BACK_ONLY"`，并在校验脚本中断言不可伪造为 true。
2. **与 `05000 -> 1` 等历史命名混淆**：文档中“天墉城”既指现有 `BaseScene 1` 也指端游 `05000`。**缓解**：清单中对 `05000` 和 `02000` 单独记录 `relatedExistingServerSceneNote`，明确不可混用坐标与模板号。
3. **双端清单漂移**：客户端后续把某张图从端游原图替换为道家 Q 版或新增切片版本时，若服务端对账文件未同步会失真。**缓解**：`verify_pc_original_map_parity.py --client-catalog` 直接比对客户端真实 `catalog.json` 哈希与全部条目字段，失配即返回非零退出码。
4. **文档索引断链**：按 `AGENTS.md §5`，新增 `docs/design/` 文档后必须运行 `python tools/scripts/gen_docs_index.py` 更新 `docs/README.md`，并在 `docs/PROGRESS.md` 文末追加记录。

## 5. 步骤分析与执行步骤

1. 在客户端先生成并冻结 `Assets/Resources/World/PcOriginalPreview20261123/catalog.json` 及全部 36 张分片与校验摘要。
2. 编写 `tools/navmesh_baker/verify_pc_original_map_parity.py`（支持 `--write-manifest` 从客户端与服务端真实数据生成 `data/scene_nav_bin/pc_original_static_backgrounds_20261123.json`，默认模式只读核验）以及完整正/反例单测 `tools/navmesh_baker/test_pc_original_map_parity.py`。
3. 运行生成与双端对账核验，确认 36 张 ready、4 个缺口、2 个 HOLD 及服务端 21 个 `BaseScene` / 16 个 `World` / 9 个节庆 / 2 个手绘 ID 全部零误差通过。
4. 追加 `docs/PROGRESS.md` 条目，运行 `tools/scripts/gen_docs_index.py` 更新 `docs/README.md`。
5. 核实 `git status`、`git diff` 与单测全部通过后，在 `main` 分支提交并推送到 `origin/main`。

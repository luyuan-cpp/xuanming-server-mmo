# 碧渊水宫本体（`biyuan-water-palace` / 场景 5）服务端导航与配表接入设计方案

> 日期：2026-11-23  
> 适用仓库：`xuanming-server-mmo`（C++/Go 权威仓，关联客户端 `mmorpg-client`）

---

## 1. 需求分析

1. 为碧渊水宫本体（`biyuan-water-palace`）分配独立服务端场景配置 ID，不与既有 `1..4`、`17..21`、9 张开发节庆图或端游 `26000` / `26008` 冲突。
2. 使用客户端正式交付的双节共用导航掩码（`320×256`、`cellSize = 1.0`、`worldRect = (0,0,320,256)`、`effectiveWorldRect = (0,8,320,240)`）离线烘焙权威 Recast/Detour `scene.bin` 与严格掩码 `scene.bin.mask` 及 `scene.bin.sha256`。
3. 更新 `data/BaseScene.xlsx` 对应行并通过正式导表器 `tools/data_table_exporter/run.py` 重新生成 `generated/tables/basescene.{json,pb}`、`generated/client_tables/basescene.pb` 与 `generated/tables/manifest.json`；同步补充 `SceneSpawnTest` 与 `FestivalNavigationTest` 回归测试。

## 2. 设计分析

### 2.1 场景 ID 选择：`sceneId = 5`
- `data/World.xlsx` 已预分配 `id = 1..16`（`scene_id = 1..16`），其中 `1..3` 分别为主城、八仙岛/八景岛、东海渔村/月汐湾，`4` 为揽仙镇保留槽位，`5..16` 当前全部指向占位 `data/scene_nav_bin/main_scene.bin`。
- `Dungeon.xlsx`（`17..19`）、`Mirror.xlsx`（`20..21`）、`MysteryMerchantSpot.xlsx`（仅引用 `1`）均不占用 `5`。
- 启用 `id = 5` 作为碧渊水宫正式主世界场景：`World.xlsx` 无需新增行，`scene_manager` 自动为 `scene_id = 5` 创建世界分线；`sync_current_maps.py` 仅校验 `1..3` 与 `main_scene.bin` 绑定行，不影响既有工具与测试。

### 2.2 导航目录与运行时严格合同
- 产物路径：`data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123/`
  - `scene.bin`（MSET v1，由 `tools/navmesh_baker` 以 `--mask-base64`、`--mask-grid 320,256,1,0,0,320,256`、`--effective-rect 0,8,320,240`、`--cs 0.25`、`--min-region-area 0`、`--probe 222.5,0,58.5` 及全 16 地标探针烘焙）
  - `scene.bin.mask`（FMASK 严格掩码，供运行期逐段与有效区裁剪）
  - `scene.bin.sha256`（`scene.bin` 与 `scene.bin.mask` 双行 SHA-256 校验和）
  - `source.json`、`walkmask.txt`、`navigation.json`、`import-receipt.json`
- 路径以 `data/scene_nav_bin/current_maps/` 开头，自动命中 `cpp/libs/services/scene/spatial/constants/nav.h` 中的 `NavDataContract::VersionedMask` 合同：
  - `NavigationSystem::LoadNavBins()` 强制校验 `.sha256`、`.mask`、81,920 格逐格一致性及出生点落网；任一校验失败拒绝注册并阻断入场（fail-closed）。
  - `NavQuerySystem::FindPath()` 在贴窄廊/桥拐点处具备 `FindVersionedMaskPath` 精确掩码寻路回退，彻底避免切角回拉。
- 坐标换轴（遵循 `nav.h` / `WorldCoordinateConverter` 唯一契约）：
  - Unity `(222.5, 0.0, 58.5)` → 服务端 Z-up `(spawn_x=58.5, spawn_y=222.5, spawn_z=0.0)`。

### 2.3 复用工具与自动化校验
- 新增 `tools/navmesh_baker/prepare_biyuan_body.py`（参照 `prepare_yuexi_bay.py`），自动校验客户端 `catalog.json`、`spring`/`mid-autumn` 双节一致的 `walkmask.txt` 与哈希、调用 `navmesh_baker.exe` 烘焙并校验全部 81,920 格 `mismatches=0`、单连通分量 `9,093` 格与 16 个关键地标 `--probe`。
- 在 `cpp/tests/bag_test/scene_spawn_test.cpp` 与 `cpp/tests/bag_test/festival_navigation_test.cpp` 增加场景 5 的加载、寻路、阻挡与缺失校验和拒绝用例。

## 3. 思想分析

- **复用既有 `VersionedMask` 合同**：零侵入 C++ 场景运行时核心逻辑，完全沿用月汐湾已验证的 `current_maps/<key>/<revision>/scene.bin` 严格校验机制。
- **双端同源出生与位图**：服务端 `walkmask.txt` 直接从客户端已校验交付目录复制并核对 SHA-256，烘焙器自带全格对照，从源头消除两端碰撞失配。
- **规范同步**：严格遵守 `AGENTS.md` §3/§4，修改 `data/BaseScene.xlsx` 后只经正式导表器生成产物，不手改 `generated/`。

## 4. 漏洞分析

1. **ABI 兼容风险**：`third_party/ue5navmesh` 必须以 `dtReal=double` 及与 `scene` 一致的编译宏构建 `navmesh_baker.exe`；已通过 `nav_mask_contract_test`（17/17）和 `test_baker.py`（10/10）验证。注意本地 submodule 检出后不得误提交 `third_party/ue5navmesh` 指针变更。
2. **导表器影响范围风险**：正式导表只应变更 `BaseScene` 相关数据文件（`basescene.json`、`basescene.pb`、`client_tables/basescene.pb`、`manifest.json`），不得改动其他表或 proto；运行后需对比 `git diff --stat` 核实。
3. **本机 `navmesh_query.exe` 与 C++ 全量工程测试环境限制**：Windows 应用程序控制策略阻止运行新链接的 `navmesh_query.exe`，而 `navmesh_baker.exe` 与 `nav_mask_contract_test.exe` 可正常运行。`navmesh_baker.exe` 在写盘前已执行全 81,920 格 `findNearestPoly` 对照、连通分量检查与全 16 地标 `--probe` 验证；C++ `bag_test` 需在具备完整 C++ 依赖库构建环境上补充执行。
4. **Java 服务端对账**：本机未克隆独立 Java 仓库（`luyuan-java/xuanming-server-mmo`），本轮交付需明确注明 Java 侧待同步 `BaseScene` 与 `biyuan-water-palace` 位图。

## 5. 步骤分析

1. 等待客户端 `biyuan-water-palace` 正式资源及双节 `walkmask.txt` 落盘并通过客户端校验。
2. 编写 `tools/navmesh_baker/prepare_biyuan_body.py`，校验客户端双节同源位图并调用 `navmesh_baker.exe` 烘焙 `data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123/scene.bin`（含 `.mask`、`.sha256`、`import-receipt.json`）。
3. 更新 `data/BaseScene.xlsx` 第 `id = 5` 行，运行正式导表器 `tools/data_table_exporter/run.py` 更新 `generated/tables/` 与 `generated/client_tables/`，再同步 `basescene.pb` 到客户端 `Assets/StreamingAssets/Tables/`。
4. 在 `cpp/tests/bag_test/scene_spawn_test.cpp` 与 `festival_navigation_test.cpp` 及 `tools/navmesh_baker/test_current_maps.py` 增加场景 5 回归测试；更新 `docs/PROGRESS.md` 与 `docs/README.md` 索引。
5. 运行 `test_baker.py`、`test_current_maps.py`、`nav_mask_contract_test.exe` 及 C++ 裸指针门禁预检 `test_cpp_no_raw_pointer_member.py`，确认 `git status` 干净（不提交 `third_party/ue5navmesh`），提交并推送。

## 6. 执行步骤

1. 在客户端完成资源导入与 `verify` 后，运行 `prepare_biyuan_body.py` 完成烘焙与落盘。
2. 更新 `data/BaseScene.xlsx`（`id=5`）并运行正式导表器。
3. 同步 `generated/client_tables/basescene.pb` 至客户端并复跑客户端编译体检。
4. 补充服务端测试与文档记录，运行可执行测试与门禁检查，清理无关构建产物或保持 `.gitignore`，最后显式暂存、提交并推送。

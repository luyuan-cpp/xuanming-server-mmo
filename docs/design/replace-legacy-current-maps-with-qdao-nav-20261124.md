# 退役旧版 12.5 GB 三图（`CurrentMaps20261009`）、接通八景岛/月汐湾双端导航与主城临时替代方案

- **日期**：2026-11-24
- **状态**：已完成实现与验证（临时复用碧渊水宫顶替 `sceneId=1` 主城、彻底删除 `CurrentMaps20261009` / `CityTiles4KReview` / 旧 4K 流式与 `schemaVersion=1` 兼容分支 / 服务端旧 `.bin` 与旧同步脚本、补全八景岛与月汐湾三端导航）
- **涉及仓库**：
  - C++/Go 权威服务端：`xuanming-server-mmo-cpp`
  - Unity 客户端：`mmorpg-client`
  - Java 平行服务端：`xuanming-server-mmo`（双版对账 `PARITY.md`）

---

## 1. 需求分析

1. **彻底退役废弃旧图**：客户端 `Assets/Resources/World/CurrentMaps20261009/` 存放的 2026-10-25 旧三图（`main` / `island` / `village` 共 519 张 4K PNG，约 12.5 GB / 2,121 个文件）已确认废弃不再使用，必须彻底删除；同步删除服务端旧版 `main_scene.bin`、`penglai_scene.bin`、`donghai_scene.bin`、`current_maps_20261009.json` 及旧 150×150 掩码目录 `current_maps/{main,island,village}/{source.json,walkmask.txt}`。
2. **完成正式联机四图导航闭环**：
   - **`sceneId = 5`（碧渊水宫 `biyuan-water-palace` / `biyuan-body-20261123`）**：保持现有三端正式联机状态不变。
   - **`sceneId = 3`（月汐湾 `village` / `yuexi-bay-20261115`）**：C++ 与 Java 服务端已完成导航烘焙与 `BaseScene.xlsx` 绑定；客户端 `QdaoMaps20261102/catalog.json` 需将 `clientOnly` 翻为 `false`，正式切离旧渔村。
   - **`sceneId = 2`（八景岛 `island` / `bajingbody20261114`）**：客户端已有双节 1024×1024 切片与 `587×427` 完整双节共享位图（24,016 可走格、单连通分量、18 地标）；需用本机 `navmesh_baker.exe` 烘焙 `data/scene_nav_bin/current_maps/island/bajingbody20261114/scene.bin`（含 `.mask`、`.sha256`、`import-receipt.json`），更新 `BaseScene.xlsx` 行 2、同步 Java 服务端，并将客户端 `clientOnly` 翻为 `false`。
   - **`sceneId = 1`（主城 `main`，及 `BaseScene` 占位槽 `6..16`）**：新主城美术尚未完成，临时复用**碧渊水宫**（`biyuan-body-20261123`）双节正式图与导航顶替 `sceneId = 1`，新主城交付后直接切换配置即可替换。
3. **关联玩法点位同步**：`MysteryMerchantSpot.xlsx` 中绑在场景 1（行 1–6）和场景 2（行 7–9）的神秘商人刷新点需全部迁移到临时主城（碧渊水宫）与八景岛的已校验可走地标，防止刷在不可走区域。
4. **并行工作区隔离**：三仓当前均有其他会话的未提交改动（`PlayMode` / `EquipEnhance` / `Dungeon` / `Ranked` / `SkillScrollCraft`），所有文件改动与导表必须严守隔离，不覆盖、不回退任何在途改动。

---

## 2. 设计分析

### 2.1 零冗余复用：主城临时别名方案

为避免在客户端复制 135 MB 碧渊水宫 PNG、或在服务端复制第二份 `scene.bin`，采用“数据层显式共享引用”：

1. **服务端（C++ + Java）**：
   - `data/BaseScene.xlsx` 行 `1` 及占位槽 `6..16` 的 `nav_bin_file` 统一指向 `data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123/scene.bin`，出生点为 `(58.5, 222.5, 0)`（对应 Unity `(222.5, 0, 58.5)`）。
   - C++ `NavigationSystem::LoadNavBins()` 天然按 `nav_bin_file` 路径去重（`byPath` 复用同一份 `std::shared_ptr<NavComp>`），并对每个 `scene_id` 单独执行 `ProbeSceneSpawn`，**零运行时 C++ 代码侵入**。
   - Java `ConfigSceneTables.java` 对 `data/scene_nav_bin/current_maps/<key>/<revision>/scene.bin` 按 `relativeFolder` 缓存 `CurrentMapNavigation`，并允许临时主城（`id = 1`）与占位世界槽位（`id = 6..16`）共享 `biyuan-water-palace/biyuan-body-20261123`（原 `sceneId = 5`），其余主场景仍严格校验 `source.sceneId == row.id` 与出生点一致。
2. **客户端（`mmorpg-client`）**：
   - 在 `MapDeliveryRegistration` 新增两个向后兼容的可选字段 `assetKey`（缺省回退 `key`）与 `assetSceneId`（缺省回退 `sceneId`）。
   - 在 `Assets/Resources/World/QdaoMaps20261102/catalog.json` 新增 `sceneId = 1, key = "main", name = "主城（临时·碧渊水宫）", revision = "biyuan-body-20261123", clientOnly = false, assetKey = "biyuan-water-palace", assetSceneId = 5`，直接引用碧渊水宫双节资源目录，**零磁盘图片冗余、零破坏原有 SHA-256**。
   - 移除 `CurrentMapCatalog.Legacy` 硬编码旧目录兜底，`FindForOnline` 统一只从已交付 `catalog.json` 中 `CanTravelOnline == true` 的条目解析。

### 2.2 八景岛（`sceneId = 2`）服务端严格导航烘焙

参照 `tools/navmesh_baker/prepare_biyuan_body.py` 新增 `tools/navmesh_baker/prepare_bajing_body.py`：
- 校验客户端 `QdaoMaps20261102/catalog.json` 中 `sceneId = 2 / key = island / revision = bajingbody20261114` 的 `spring` 与 `mid-autumn` 双节 `layout.png`、`walkmask.txt`、`navigation.json`、`navigation-plan.json` 哈希及双节导航完全一致；
- 调用 `navmesh_baker.exe --mask-base64 ... --mask-grid 587,427,1,-83.33333333333333,-3.3333333333333335,586.6666666666666,426.6666666666667 --effective-rect 0,-4.440892098500626e-16,419.99999999999994,419.99999999999994 --cs 0.25 --min-region-area 0 --probe ... --out data/scene_nav_bin/current_maps/island/bajingbody20261114/scene.bin`，探测出生点及全部 18 个评审地标，核验 `mask cells checked=250649 mismatches=0` 与单连通分量 `24016` 格；
- 生成 `scene.bin`、`scene.bin.mask`、`scene.bin.sha256`、`import-receipt.json` 并复制 `source.json`、`walkmask.txt`、`navigation.json`。

### 2.3 `MysteryMerchantSpot.xlsx` 点位迁移

- 场景 1（行 1–6，临时碧渊水宫）：迁移至碧渊水宫已评审可走地标（出生点双灯门楼、太极中庭、主殿门前、钟鼓亭前、茶亭前院、东拱桥）。
- 场景 2（行 7–9，八景岛）：迁移至八景岛已评审可走地标（南码头出生点 `70,205`、中央太极广场 `200.41666666666666,166.66666666666669`、中央主殿前 `240,209.58333333333331`）。
- 场景 5（行 10–12，碧渊水宫）：保持不变。

### 2.4 双版 Java 服务端同步

- 更新 `xuanming-server-mmo/tools/ContractSync.java`：`--scope current-maps` 不再硬编码要求旧版无版本目录 `main` / `island` / `village` 存在，改为自动扫描 `data/scene_nav_bin/current_maps` 下所有含 `source.json` + `walkmask.txt` 的版本化目录，并按 `BaseScene` 与 `MysteryMerchantSpot` 同步表数据。
- 更新 `ConfigSceneTables.java`、`CurrentMapNavigation.java` 及 `game-scene` 单元测试（覆盖场景 1 临时碧渊、场景 2 八景岛、场景 3 月汐湾、场景 5 碧渊水宫，移除对已删旧目录的硬依赖）。

---

## 3. 思想分析

- **单一事实源（SSOT）与零重复资产**：主城临时替代通过目录别名指向既有碧渊水宫资产与 `scene.bin`，杜绝 135 MB 冗余贴图复制，未来新主城出图只需替换一条 `catalog.json` 记录与 `BaseScene.xlsx` 对应行。
- **失败关闭（Fail-Closed）与逐格一致**：八景岛完全走 `NavDataContract::VersionedMask`（MSET + FMASK + 双行 SHA-256），服务端启动与烘焙期逐格校验全部 250,649 格，保证两端碰撞零偏移。
- **彻底清债、不留死路径**：清理 `CurrentMaps20261009`（12.5 GB）同时同步更新引用旧路径的 `CurrentMapCatalog`、测试与工具脚本，避免出现“删了图但代码或测试留悬垂引用”。

---

## 4. 漏洞分析

1. **客户端 `source.json` 哈希不可随意改写**：`CurrentMapNavigation.java` 校验月汐湾 `YUEXI_SOURCE_SHA`，`prepare_biyuan_body.py` 与 `import_festival_maps.py` 校验 `source.json` 与 `catalog.json`。因此开放月汐湾与八景岛时**只改 `catalog.json` 中的 `clientOnly: false`**，绝不改写各图目录下的 `source.json`、`walkmask.txt` 或 PNG 字节。
2. **Java 服务端共享导航校验冲突**：`BaseScene` 中场景 1 与占位槽 6..16 复用 `biyuan-water-palace/biyuan-body-20261123/scene.bin`（`source.sceneId = 5`），若不放宽 `ConfigSceneTables` 的共享映射检查会直接启动报错；已限定只允许场景 `1` 与占位槽 `6..16` 共享碧渊主城导航，其余场景仍强制 `source.sceneId == row.id`。
3. **导表器干扰在途未提交表**：`xuanming-server-mmo-cpp` 工作区已有其他会话修改的 `PlayMode`/`EquipEnhance`/`Dungeon`/`Monster` 及新增 `EquipAffixQuality`/`SkillScrollCraft`。运行正式导表器前须快照各生成文件哈希，导出后逐文件核对仅 `basescene.*`、`mysterymerchantspot.*` 与 `manifest.json` 发生预期变更，不触碰无关文件。
4. **`MysteryMerchantSpot` 与既有单测耦合**：`cpp/tests/bag_test/player_mystery_merchant_test.cpp` 自建内存态商户坐标（`kMerchantX=176, kMerchantY=206`）不依赖表行坐标，但需确认导表后该单测仍保持语义完整；同时在 `scene_spawn_test.cpp` 中补齐场景 1（临时碧渊）与场景 2（八景岛）的寻路、阻挡与老坐标回落回归。
5. **Unity `.meta` 与删除完整性**：删除 `Assets/Resources/World/CurrentMaps20261009` 时同步删除 `CurrentMaps20261009.meta`，避免 Unity 报孤儿 `.meta` 警告；检查 `CityTileStreamingTests.cs` / `CurrentMapTileStreamingTests.cs` / `CurrentMapTests.cs` / `CityTravelWindowTests.cs` / `TianyongPaintedCityTests.cs` 不再硬依赖已删除路径。

---

## 5. 步骤分析

1. **C++/Go 服务端导航烘焙与配表更新**：编写并运行 `prepare_bajing_body.py` 烘焙八景岛 `scene.bin` + `.mask` + `.sha256`；更新 `BaseScene.xlsx`（1/2/6..16）与 `MysteryMerchantSpot.xlsx`（1..9）；运行正式导表器更新 `basescene.*` / `mysterymerchantspot.*` / `manifest.json`；删除旧 `main_scene.bin` / `penglai_scene.bin` / `donghai_scene.bin` / `current_maps_20261009.json` 及旧 150×150 `current_maps/{main,island,village}` 文件；更新 `scene_spawn_test.cpp`、`festival_navigation_test.cpp`、`sync_current_maps.py`、`test_current_maps.py`。
2. **Unity 客户端换图与删除 12.5 GB 旧资源**：扩展 `MapDeliveryRegistration` / `CurrentMapCatalog` / `CurrentMapTileStreaming` 支持 `assetKey` / `assetSceneId` 共享别名并移除 `Legacy` 旧目录回落；更新 `QdaoMaps20261102/catalog.json`（新增 `main` 临时别名、将 `island` 与 `village` 翻为 `clientOnly: false`）；同步 `Assets/StreamingAssets/Tables/{basescene,mysterymerchantspot}.pb`；删除 `Assets/Resources/World/CurrentMaps20261009` 及其 `.meta`；更新 `CityTravelUiRoot.cs` 与受影响的 EditMode 测试和离线脚本。
3. **Java 服务端双版同步**：更新 `ContractSync.java`、`ConfigSceneTables.java`、`CurrentMapNavigation.java`，同步 `basescene.pb`、`mysterymerchantspot.pb`、`manifest.json` 与版本化 `config-data/navmesh/current_maps/`（删除旧 150×150 目录，新增八景岛）；更新 `game-scene` 单元测试与 `PARITY.md`。
4. **全链路验证**：在 C++ 仓跑 `prepare_bajing_body.py` + `test_baker.py` + `test_current_maps.py` + `test_cpp_no_raw_pointer_member.py`；在客户端跑 `tools/client_compile_check.ps1`；在 Java 仓跑 `./mvnw -B -pl game-scene -am test` 与 `ContractSync --check`；同步修正 `minigame-remote-map-streaming-and-scene-gating.md` 中的现状盘点段落。

---

## 6. 执行步骤

1. 落盘本设计文档并更新 `minigame-remote-map-streaming-and-scene-gating.md` §1.2。
2. 编写 `tools/navmesh_baker/prepare_bajing_body.py` 并运行烘焙八景岛（`sceneId = 2`），验证 250,649 格零失配与全 18 地标探针通过。
3. 更新 `data/BaseScene.xlsx`（场景 1/6..16 → 碧渊水宫，场景 2 → 八景岛）与 `data/MysteryMerchantSpot.xlsx`（行 1–9 迁至可走地标），运行正式导表器并核对导出差异。
4. 删除服务端旧版 `main_scene.bin`、`penglai_scene.bin`、`donghai_scene.bin`、`current_maps_20261009.json` 与旧 150×150 `current_maps` 掩码，更新 C++ 测试与 Python 校验工具。
5. 修改客户端 `MapDeliveryContract.cs`、`CurrentMapCatalog.cs`、`CurrentMapTileStreaming.cs`、`CityTravelUiRoot.cs`、`QdaoMaps20261102/catalog.json` 与受影响测试；同步最新 `basescene.pb` / `mysterymerchantspot.pb`；彻底删除 `Assets/Resources/World/CurrentMaps20261009` 及 `.meta`。
6. 修改 Java 服务端 `ContractSync.java`、`ConfigSceneTables.java`、`CurrentMapNavigation.java` 及 `game-scene` 单测，同步最新 `basescene.pb` / `mysterymerchantspot.pb` / `navmesh/current_maps/`，并在 `PARITY.md` 登记。
7. 逐仓执行编译检查与自动化测试，核查三仓 `git status` 确认零误伤在途改动后汇总交付报告。

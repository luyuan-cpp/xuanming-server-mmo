# 小游戏与 App 双形态远程地图流式加载、CDN 分发与服务端切图门禁设计

- **日期**：2026-11-24
- **状态**：设计草案（已落盘，尚未改动代码、协议或资源，待负责人确认关键决策后分阶段实施）
- **适用仓库**：
  - 权威服务端（C++ 节点 + Go 微服务 + Java Gateway）：`xuanming-server-mmo-cpp`
  - Unity 客户端（微信/抖音小游戏 WebGL2 + iOS/Android App IL2CPP）：`mmorpg-client`
  - 平行 Java 服务端（双版契约对账，见 `AGENTS.md §12` 与 `PARITY.md`）：`xuanming-server-mmo`
- **关联事实源**：
  - 小游戏 WebSocket 长连接与边缘域名：[minigame-websocket-gate.md](minigame-websocket-gate.md)（W1–W11）
  - 跨节点切场景换手门与冻结上限：[scene-switch-release-design.md](scene-switch-release-design.md)、[cross-zone-scene-travel.md](cross-zone-scene-travel.md)
  - 区服列表与网关现状：[serverlist-static-publish-2026-05.md](serverlist-static-publish-2026-05.md)
  - 上线长周期前置手续：[../ops/release-checklist.md](../ops/release-checklist.md)（§7.1）
  - 客户端双形态强制规范：`mmorpg-client/Docs/rules/minigame-and-app-platforms.md`
  - 客户端当前 4K 分片加载与验证记录：`mmorpg-client/Docs/CityTiles4K.md`、`mmorpg-client/Docs/VerificationEvidence/generated-map-tiles-20261025/README.md`

---

## 1. 需求分析

### 1.1 背景与核心澄清

微信小游戏与抖音小游戏对**本地包体**（主包通常约 4 MB，本地代码分包总上限通常约 20–30 MB，具体数值以两家上线当期文档为准）与**运行期内存 / 显存**（WebAssembly 堆 + WebGL2 显存，尤其 iOS 微信小游戏）均有严格上限；原生 App 也面临商店蜂窝下载提示线与中低端机 OOM 红线。

需要首先厘清一条关键架构边界：

1. **包体小 ≠ 玩家只能去几座主城**：哪怕只开放 1 座高清主城，也无法把高清地图贴图打进小游戏首包。地图开放数量取决于策划/美术产能与服务端导航完备度，而不取决于小游戏包体大小；真正解决包体限制的手段是**首包与重地图资源彻底解耦（全量走 CDN 按需流式下载 + 缓存淘汰）**。
2. **小游戏与 App 同服互通时，地图开放集合必须一致**：同一区服内不得出现“App 能进某图、小游戏进不去”，否则组队跟随、帮战、世界 Boss、跨图传送等社交与玩法闭环会直接破裂。平台差异只能落在**资源加载方式**（小游戏全按需 CDN，App 可选预装扩展包）与**贴图画质/压缩档位**。
3. **服务端完全不存美术贴图**：服务端只持轻量的 `BaseScene` 表、NavMesh / `walkmask`（单图约 110–125 KB）与六边形空间索引 `GridSystem`，不受客户端包体限制。

### 1.2 现状盘点（截至 2026-11-24）

- **客户端重资产全部位于 `Assets/Resources/World/`**：
  - 早期 2026-10-25 旧三图（`CurrentMaps20261009/`，519 张 4K PNG / 12.5 GB）已确认废弃并纳入清理（见 [replace-legacy-current-maps-with-qdao-nav-20261124.md](replace-legacy-current-maps-with-qdao-nav-20261124.md)）；
  - 当前正式道家 Q 版双节地图位于 `QdaoMaps20261102/`（`1024×1024` 切片体系，合计约 **833 MB**，包含碧渊水宫 `sceneId=5`、八景岛 `sceneId=2`、月汐湾 `sceneId=3`、临时复用碧渊水宫的主城 `sceneId=1` 及若干待接双节图），另含 `PcOriginalPreview20261123/`（36 张端游静态预览，约 **643 MB**）以及 `Characters` / `Pets` 序列帧（约 **4.0 GB**）。Unity 正式构建会把 `Resources` 内资源打包进安装包/首包，上线小游戏/App 前仍必须全部迁出至 CDN。
- **客户端已有可复用的分片流式加载骨架**：
  - `CurrentMapTileStreaming.cs` 已实现「总览 `layout.png` 兜底 + C++ 同源六边形七格与相机视野查询 + 最多 2 并发加载 + 全局最多 6 片预算 + 离开 2 秒释放」，但当前直接调用 `Resources.Load` / `Resources.LoadAsync`，尚无远程 CDN 源抽象与下载前预检。
  - 当前开发期导入格式仍为无压缩纹理（`1024×1024` 单块无压缩约 3–4 MiB，总览图 `4096–6144` 更大），小游戏 WebGL2 上线前仍需按平台档位切换 GPU 压缩纹理（ASTC / ETC2）与受控预算。
- **服务端现状**：
  - `java/gateway_node` 已有按 `platform` 分发的 `/api/hotfix-check`（`HotfixCheckService.java`、`HotfixProperties.java`），具备启动期 fail-closed 配置校验；原无调用的 `/api/cdn-sign` 已作为安全风险删除，**不再重启自定义签名预言机**。
  - 服务端 `scene_manager` 的 `EnterScene`（`go/scene_manager/internal/logic/enterscenelogic.go`）与 C++ `SceneSceneClientPlayerHandler::EnterScene`（`cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp`）已具备跨节点换手门（`owner_epoch` + `handoff` 标记）、冻结上限（70s）和客户端超时预算（`CityTravelRequest.AcceptedHandoffBudgetSeconds = 75s`），但**尚无独立的场景开放白名单/灰度门禁**，也**尚无应对客户端远程下载延迟的 `Entering → SceneReady` 保护态**。
  - `docs/ops/release-checklist.md` §7.1 仅记录了 `ws-edge`（`socket`）与 `gateway`（`request`）的备案与合法域名登记，尚未补录 CDN 静态资源域名（`downloadFile` / `request`）。

### 1.3 目标与非目标

- **目标**：
  1. 首包极瘦化：小游戏首包只保留引擎/WASM、登录与基础 HUD、必需中文字体子集、裁剪后的 `StreamingAssets/Tables/*.pb` 配表与极简引导配置；所有地图（底图 + 分片）脱离 `Assets/Resources/`，按平台/画质档位从 CDN 按需加载。
  2. 双级地图加载：进图前只拉极小「一级必需包」（元数据 + 低清总览底图，目标 1–3 MB），秒进可玩；进图后流式拉「二级视野高清分片」，不卡主线程、不断心跳。
  3. 服务端权威闭环：支持按配置控制已开放场景集合（防客户端改包越权进未开放图）；支持客户端主动切图「先下后切」与服务端强制拉图「`Entering → SceneReady` 保护态」；由网关统一签发合规 HTTPS CDN 基址与资源清单版本。
  4. 双形态兼容：同一套业务代码同时满足微信/抖音小游戏（WebGL2 单线程、无 `UnityEngine.Caching`、仅合规 HTTPS 域名）与 iOS/Android App（IL2CPP/AOT，可选随包或边玩边下）。
- **非目标**：
  1. 本阶段不立刻把 Unity 工程转成微信/抖音小游戏包（遵循负责人 2026-11-20 约定：转包放到上线前用 Unity 7 执行，当前阶段做好代码抽象、服务端契约与打包边界防返工）。
  2. 不重新引入 `/api/cdn-sign` 公网签名接口；公开只读地图静态资产通过内容哈希不可变 URL + CDN 公开只读分发（防盗链靠 CDN Referer/UA/限频，不把签名私钥放进公网匿名接口）。
  3. 不在开发阶段删除编辑器本地预览所需的原图资产；本地编辑模式继续支持本地源加载，正式打包通过构建管线排除并产出 CDN 上传包。

---

## 2. 设计分析

### 2.1 客户端资源分发方案对比（Approaches）

| 维度 | 方案 A：轻量化 CDN 清单 + `UnityWebRequest` 散图/分包抽象（**推荐**） | 方案 B：全量接入 Unity Addressables 远程分组 |
|---|---|---|
| 与现有代码契合度 | **极高**：`CurrentMapCatalog` / `CurrentMapTilesManifest` / `CurrentMapTileStreaming` 已是 JSON 清单 + 逐块异步加载，只需抽象 `IMapAssetSource`（本地源 / CDN 源） | **较低**：当前工程未接入 Addressables；需重写全部目录、清单、哈希校验与校验测试工具链 |
| 小游戏兼容风险 | **低**：全链路只用 `UnityWebRequest` + 微信/抖音转换插件自带文件系统缓存，天然避开小游戏不支持 `UnityEngine.Caching` 的坑 | **中**：Addressables 默认依赖 `Caching`，转微信/抖音小游戏仍需接平台插件补丁、处理 Catalog 并发和版本清理 |
| 增量热更与回滚 | **简单直观**：每个地图清单与切片文件名带内容 SHA-256，切外观或回滚只需切换服务端下发的 `manifest_version` | 官方支持 Catalog 热更，但依赖 Unity 版本匹配（未来升级 Unity 7 时 Addressables 版本迁移有额外风险） |
| 适用边界 | 最适合当前以 2D 地图底图 + 分片纹理 + JSON 元数据为主的地图系统；未来若 3D 模型/特效/UI 极度复杂，可在非地图域再评估 Addressables | 适合大量跨 Prefab 依赖树的重度 3D 资产工程 |

**推荐结论**：采用 **方案 A（`IMapAssetSource` 双源抽象 + 版本化不可变 CDN 清单）**，以最小改动量复用现有 4K 分片与六边形空间索引体系，并降低未来迁移 Unity 7 转小游戏包的耦合风险。

### 2.2 资源分层与包体边界

#### 1）首包（随代码包下发，严控体积）
- WASM / IL2CPP 代码、登录/选服/通用 HUD UI、必需中文字体子集；
- 裁剪后的客户端配置表 `Assets/StreamingAssets/Tables/*.pb`；
- 本地最小回退资源（Loading 背景、错误占位、轻量元配置）。
- **硬红线**：任何正式地图底图、4K 分片、未上线端游预览图（`PcOriginalPreview20261123`）、角色/宠物大序列帧图集，**严禁留在 `Assets/Resources/` 进入正式发布构建**（由独立构建前校验脚本拦截，发现 `Resources/World` 残留直接阻断发包）。

#### 2）CDN 远程资源两级结构
按平台与画质档位分目录发布到对象存储（OSS/COS）+ CDN（备案 HTTPS 子域名，登记为微信/抖音 `request` 与 `downloadFile` 合法域名）：

```text
https://<cdn-host>/maps/<manifest_version>/
  catalog.json                                    # 全图索引（sceneId, key, appearance, tier, hashes）
  <key>/<revision>/<appearance>/<tier>/
    bundle-l1.json                                # 一级清单：source + walkmask + layout + tiles 元数据
    layout.<ext>                                  # 一级低清总览底图（压到 0.5~2 MB，秒进可玩）
    tiles/r01_c01.<hash8>.<ext>                   # 二级视野高清分片（按七格/相机按需流式拉取）
```

- **画质与纹理压缩分档（`tier`）**：
  - `webgl-low` / `webgl-mid`（微信/抖音小游戏）：**严禁使用无压缩 `RGB24`**。单张分片降档至 `512×512` 或 `1024×1024`，采用 WebGL2 硬件压缩纹理（ASTC / ETC2，或小体积 WebP/JPG 解码后受控池化），全局常驻纹理预算按真机实测显存收紧（例如 iOS 微信小游戏显存硬预算远低于原生），彻底消除 288 MiB RGB24 OOM 风险。
  - `app-high`（iOS / Android App）：使用平台原生 GPU 压缩纹理（iOS ASTC、Android ASTC/ETC2），支持高分片规格；并允许 App 构建选项将核心主城一级包随 OBB/ODR 预置。
  - `editor-local`（开发编辑器）：继续直读本地工程目录，不依赖公网 CDN，不打断当前美术验收与本地测试。

#### 3）客户端缓存策略（严守 WebGL2 限制）
- 小游戏端**禁用 `UnityEngine.Caching` 与异步 `FileStream`**；
- 统一通过 `IMapAssetCache` 适配层：
  - 编辑器 / 原生 App：落 `Application.persistentDataPath/map_cache/`，按内容哈希索引 + LRU 容量上限淘汰；
  - 微信 / 抖音小游戏：接入两家 Unity 转换插件提供的本地缓存目录/LRU 管理（或 `WXFS` / 宿主缓存接口），缓存被系统回收时无感回源 CDN 重拉（`persistentDataPath` 丢失零业务副作用）。

### 2.3 服务端设计（三层职责与契约）

#### 1）网关版本与 CDN 入口分发（复用现有 `/api/hotfix-check`）
不另造新的无鉴权接口，直接扩展 `java/gateway_node` 已有的 `HotfixProperties` / `HotfixCheckResponse`（见 `java/gateway_node/src/main/java/com/game/gateway/service/HotfixCheckService.java`）：
- 按 `platform`（如 `wechat_minigame`、`douyin_minigame`、`ios`、`android`）在配置中增加可选字段：
  - `assetBaseUrl`：合法 HTTPS CDN 根地址（启动期校验必须为 `https://` 且非裸 IP、无查询串/片段，配错拒绝启动，沿用 `HotfixCheckService` 现有 fail-closed 风格）；
  - `assetManifestVersion`：当前生效的地图与远程资源清单版本（含内容哈希或不可变发布号）；
  - `assetTier`：该平台默认画质档位（如 `webgl-mid` / `app-high`）。
- 客户端在登录前调用 `/api/hotfix-check` 时一并获取 CDN 基址与清单版本，拉取 `catalog.json` 并校验 SHA-256。换节庆皮肤（春节/中秋）或修复地图分片时，只需上传新版本目录并切换网关配置的 `assetManifestVersion`，全端一致且可秒级回滚。

#### 2）场景开放权威门禁（服务端唯一事实源）
如果运营或首期版本仅开放部分核心主城（例如先开主城 / 八仙岛 / 东海渔村 / 碧渊水宫），绝不只靠客户端隐藏 UI：
- **事实源**：在 `BaseScene.xlsx`（`data/schema/basescene_table.proto`）新增开放状态字段（如 `bool travel_enabled` 或枚举 `open_state`，新字段号顺延，上线后永不复用），或由 `scene_manager` 结合区服运行期开放白名单配置控制；
- **校验点**：
  1. `go/scene_manager/internal/logic/enterscenelogic.go` 在进入场景 / 跨区传送选图前，校验目标 `scene_config_id` 处于开放状态；未开放直接拒绝并返回统一 `Tip.xlsx` 错误码（不写 location、不触发换手）；
  2. `WorldMapOverview`（`proto/scene/player_scene.proto` 的 `WorldMapOverviewEntry`）与切线目录仅发布已开放地图，客户端地图窗直接以服务端下发目录为准渲染可去列表；
  3. 同步守卫规则：**同一区服内不对 `wechat_minigame` / `douyin_minigame` 与 `ios` / `android` 施加不同的可去场景子集**（同服互通红线）；若未来开设独立小游戏专服，则按 `zone_id` 粒度配置开放集合，逻辑天然统一。

#### 3）切图协议与客户端远程下载协同（核心难点）

切图存在两类入口，必须区别处理：

##### 路径 A：玩家主动切图（传送门 / 世界地图窗 / 切线）——「先下后切」
```text
玩家点击前往目标城
  ↓
[客户端] 查本地缓存是否具备目标图 一级必需包（bundle-l1 + layout 底图）
  ├── 已命中缓存 ──────────────────────────────┐
  └── 未命中 → 弹轻量 Loading 进度条            │
               从 CDN 拉取一级必需包（1~3 MB）   │（期间 Gate 长连接正常发心跳，不断线）
               ├── 玩家取消 / 下载失败 → 原地提示，未发 EnterScene，服务端零脏状态
               └── 校验 SHA-256 成功 ──────────┘
  ↓
[客户端] 发送 EnterSceneC2SRequest（走现有 CityTravelRequest 预算与 RpcGuard）
  ↓
[服务端] scene_manager 校验开放门禁 → 换手/切场景 → 推送 NotifyEnterScene (EnterSceneS2C)
  ↓
[客户端] 立即用已缓存的 一级低清底图 渲染落地可玩
  ↓
[客户端] 后台通过 CurrentMapTileStreaming（并发 ≤ 2）按七格/视野从 CDN 流式拉取 二级高清分片
```
- **优势**：下载发生在旧场景，不占用服务端冻结时间（完全不挤压 `travel_freeze_cap::kFreezeCap = 70s` 与 `CityTravelRequest.AcceptedHandoffBudgetSeconds = 75s`），取消或断网时零服务端回滚成本。

##### 路径 B：服务端强制拉图（队长带队、活动/副本传送、首次登录/重连落点）——「`Entering → SceneReady` 保护态」
服务端主动把玩家放入新场景时，客户端无法提前拦在发请求之前。此时采用两段确认：
1. 服务端把玩家落地新场景并下发 `NotifyEnterScene` 时，在场景节点为玩家挂上轻量 ECS 组件 `PlayerSceneLoadingComp`（记录 `scene_id`、单调时钟 `entered_steady_ms`、保护上限如 `kSceneReadyTimeoutMs = 30000`）；
2. 处于 `PlayerSceneLoadingComp` 期间：
   - 玩家免疫被动遇怪 / 非法位移误判；
   - 移动、发起战斗、交互请求暂缓或返回“场景加载中”提示，避免未渲染完即误操作；
   - Gate 正常维持连接与心跳，客户端下载一级必需包期间在主线程 `Poll()` / `Update()` 持续发心跳（**严禁阻塞主线程**）；
3. 客户端一级必需包就绪、渲染出低清底图后，发送新增轻协议 `SceneReadyC2S(scene_id)`；
4. 服务端核对 `scene_id` 匹配当前场景后摘除 `PlayerSceneLoadingComp`，放开正常交互并补齐完整 AOI/动态状态；
5. **超时兜底（fail-safe）**：若超过 30 秒仍未收到 `SceneReadyC2S`（例如弱网极慢或老客户端未发），服务端按单调时钟自动解除保护态并记指标告警，避免玩家永久卡无敌或永久锁死；**断线/再次切图时同步清理 `PlayerSceneLoadingComp`**。

---

## 3. 思想分析

1. **资源体积与玩法规模解耦**：用“极小一级包（必选）+ 视野二级分片（渐进增强）”把玩家的等待时间从“整张图数百 MB”压到“单张低清底图 1–3 MB”，让小游戏在 4 MB 主包限制下仍能支撑完整大世界。
2. **复用现有基础设施，拒绝造第二套轮子**：
   - 客户端复用 `CurrentMapTileStreaming` 的六边形七格索引、双并发队列、共享租约与离开滞后释放，仅把数据源抽象为 `IMapAssetSource`；
   - 服务端复用 `HotfixCheckService` 已有平台发布线与启动校验，复用 `scene_manager.EnterScene` 权威门禁与换手屏障，不新增独立静态资源签名服务。
3. **跨模块边界清晰（Actor/消息隔离与 SRP）**：
   - 客户端“下载与缓存调度（`MapAssetLoader`）”与“场景切换状态机（`CityTravelRequest` / `GameClient`）”解耦，通过只读进度/完成事件回调，回调前按规范校验对象存活与 `(PlayerId, SessionGeneration)`，杜绝跨账号或切图后旧下载回调误切场景；
   - 服务端 `PlayerSceneLoadingComp` 严格遵循 ECS 组件边界，用单调时钟驱动超时，不新开后台线程（遵守 `AGENTS.md §9` 与 C++ 无裸指针成员规则）。
4. **双服务端契约同步**：新增的 `BaseScene` 开放字段、`/api/hotfix-check` 扩展字段与 `SceneReadyC2S` 协议均属于跨端契约，必须在 `xuanming-server-mmo-cpp` 与 Java 仓 `xuanming-server-mmo`（`PARITY.md`）同步落地。

---

## 4. 漏洞分析

| 编号 | 风险 / 漏洞点 | 触发场景与后果 | 防护措施 |
|---|---|---|---|
| **V1** | **客户端藏入口代替服务端门禁** | 玩家抓包直接发 `EnterSceneC2SRequest(scene_config_id=未开放图)`，绕过客户端 UI 进未完工/未开放地图，导致无导航崩溃或卡死 | 服务端 `scene_manager.EnterScene` 强制校验场景开放标志（fail-closed）；`WorldMapOverview` 只广播开放图 |
| **V2** | **同服 App 与小游戏可去地图不一致** | 队长（App）带小游戏队员跨图，或帮战拉人到小游戏屏蔽图，导致拆队、无法参战或掉线 | 架构硬约束：同 `zone_id` 内全平台地图集合完全一致；如开独立小游戏服则按 `zone_id` 维度配置开放表 |
| **V3** | **下载期间触发 Gate 30 秒空闲踢线** | 弱网下拉取一级包耗时超过 30 秒，若客户端在 Loading 态停掉 `GameClient.Poll()`，会被 Gate 按空闲超时断开 | 下载全部走非阻塞 `UnityWebRequest` 协程/`Awaitable`，Loading 遮罩期间保持 `GameClient.Poll()` 与心跳照常运行；下载单次请求设超时与重试上限（如 15s × 2） |
| **V4** | **主线程阻塞导致小游戏页面假死** | 在 WebGL2 下使用 `Task.Run`、`FileStream` 异步 API、`.Result`/`.Wait()` 解码或写缓存导致页面永久卡死 | 全链路禁止线程池/异步文件 IO；纹理下载走 `UnityWebRequestTexture`（或主线程分帧限预算解码），严格通过 `minigame-and-app-platforms.md` §1.1 检查 |
| **V5** | **iOS 微信小游戏 WebGL2 显存 OOM** | 直接把开发期 `Uncompressed RGB24`（单张 4K = 48 MiB）放到 CDN，同时加载 3–6 张即在 iOS 微信闪退 | 生产 CDN 强制走 `tier` 压缩纹理产物（ASTC/ETC2）+ 降档分片尺寸；按平台动态配置 `MaxResidentAndPendingTiles`（小游戏可降至 2–4）；发布前必须真机实测峰值显存 |
| **V6** | **`SceneReady` 恶意拖延卡无敌** | 恶意客户端永远不发 `SceneReadyC2S`，企图在 `PlayerSceneLoadingComp` 保护态下永久免遇怪或免受攻击 | 服务端严格限制保护态上限（如 30s 单调时钟自动解除），且保护态内**禁止**主动移动、攻击、拾取、交易等收益行为；切图/下线立即摘除组件 |
| **V7** | **切图下载竞态与旧回调污染** | 玩家先点去 A 城触发 CDN 下载，中途取消或被队长拉去 B 城，随后 A 城下载完成回调误触发 `EnterScene(A)` 或覆盖 B 城底图 | 每次预下载绑定单调 `downloadToken` 与 `(PlayerId, ConnectionGeneration, TargetSceneId)`；取消、登出、断线或收到 `NotifyEnterScene` 时立即作废旧令牌，回调首行核对不一致即丢弃 |
| **V8** | **CDN 资源被篡改或旧缓存串版本** | 换节庆外观或更新导航后，CDN 边缘节点或小游戏本地缓存返回旧版 `layout.png` / `walkmask`，造成人物穿墙或画面错位 | 所有不可变切片/底图 URL 内嵌内容哈希（长缓存 `Cache-Control: immutable`）；仅顶层 `catalog.json` 按 `assetManifestVersion` 版本路径寻址；客户端加载一级包后核对 `layoutSha256` 与 `walkmaskSha256`，校验失败立即丢弃并回退重拉 |
| **V9** | **小游戏合法域名与 ICP 备案遗漏** | 开发者工具勾选“不校验域名”正常，提审或线上真机因 CDN 域名未备案/未登记 `request` 与 `downloadFile` 域名导致全员进不去图 | 将 CDN 静态域名纳入 `docs/ops/release-checklist.md` §7.1 上线长周期前置清单；`HotfixCheckService` 启动期校验 `assetBaseUrl` 必须是合规 HTTPS 子域名 |
| **V10** | **`Assets/Resources/World/` 误打进正式包** | 开发者忘记迁移或新增预览图放回 `Resources`，导致小游戏首包爆表、构建失败或 App 包膨胀至数 GB | 新增 CI / 构建前门禁脚本：正式构建（非本地开发包）扫描 `Assets/Resources/World/`，发现未豁免的大体积贴图即报错阻断 |

---

## 5. 步骤分析

按“先协议与抽象、再服务端闭环、最后资产迁移与真机压缩验收”拆成 4 个可独立验收的阶段，避免与当前在途的玩法迭代冲突，也贴合“转小游戏包放到上线前（Unity 7）再执行”的既定节奏：

1. **阶段 1：契约与只读门禁（低风险，可随时落地）**
   - 补齐 `HotfixProperties` / `HotfixCheckResponse` 的可选 CDN 字段（`assetBaseUrl`、`assetManifestVersion`、`assetTier`）；
   - 补齐 `BaseScene.xlsx` / `basescene_table.proto` 开放标志并在 `scene_manager.EnterScene` 加权威门禁；
   - 同步更新 `docs/ops/release-checklist.md` §7.1（补录 CDN 域名备案与微信/抖音合法域名登记项）。
2. **阶段 2：服务端强制拉图保护态 `SceneReady`（C++/Go + Java 双版同步）**
   - 在 `proto/scene/player_scene.proto` 新增 `SceneReadyC2S`（不破坏既有 `EnterSceneC2SRequest` / `NotifyEnterScene` 语义）；
   - 在 C++ `scene` 节点接入 `PlayerSceneLoadingComp` 与 30s 单调时钟超时摘除 + 单测；
   - 在 Java 仓 `xuanming-server-mmo` 实现同等语义并更新 `PARITY.md`。
3. **阶段 3：客户端 `IMapAssetSource` 双源抽象与主动切图预下载**
   - 将 `CurrentMapCatalog` / `CurrentMapTileStreaming` 从硬编码 `Resources.Load*` 抽象为 `IMapAssetSource`（`ResourcesMapAssetSource` 本地开发源 + `RemoteCdnMapAssetSource` 走 `UnityWebRequest` + 本地缓存）；
   - 在主动传送入口（`CityTravelUiRoot` / `GameClient`）接入「先下目标图一级必需包 → 再发 `EnterScene`」及收到 `NotifyEnterScene` 后一级包就绪上报 `SceneReadyC2S`；
   - 补全取消下载、断线重连、旧令牌作废与 WebGL2 禁用 API 静态审查的单测。
4. **阶段 4：资产迁出 `Resources`、CDN 压缩纹理管线与小游戏真机验收（上线/转包前）**
   - 离线打包工具：从权威原图生成各平台压缩纹理档位（`webgl-low`/`webgl-mid`/`app-high`）、计算 SHA-256 清单并生成可上传 OSS/COS 的版本化目录；
   - 将正式构建的 `Assets/Resources/World/` 重贴图剥离出首包（加构建前门禁检查）；
   - 在微信/抖音真机（含低端 Android 与 iOS 微信）实测首包体积、首进耗时、跨图加载耗时、缓存淘汰与 WebGL2 峰值显存，完成验收签字。

---

## 6. 执行步骤

> 以下为获批后的可执行操作清单（当前处于设计评审阶段，未执行代码修改）。

1. **评审确认三项决策（开工门禁）**：
   - 确认小游戏与 App 是否同服互通（若是，锁死“同区地图集合一致”原则）；
   - 确认客户端采用方案 A（轻量化 `IMapAssetSource` + 版本化 CDN 清单）而非 Addressables；
   - 确认当前先落地阶段 1–3，还是仅留档设计待当前在途玩法（`PlayMode` / `EquipEnhance`）合并后再开工。
2. **阶段 1 实施（服务端 + 网关）**：
   - 修改 `java/gateway_node` 的 `HotfixProperties.java`、`HotfixCheckResponse.java`、`HotfixCheckService.java` 及 `HotfixCheckServiceTest.java`，扩展可选 `assetBaseUrl` / `assetManifestVersion` / `assetTier` 与启动校验；
   - 修改 `data/schema/basescene_table.proto` 与 `data/BaseScene.xlsx` 新增场景开放状态列，跑 `dev.bat gen`（交由低档执行/Gemini 会话），在 `go/scene_manager/internal/logic/enterscenelogic.go` 增加开放门禁及对应单测与指标；
   - 更新 `docs/ops/release-checklist.md` §7.1 增加 CDN 域名 ICP 备案与两家平台合法域名登记条目；
   - 更新 `docs/README.md`（跑 `python tools/scripts/gen_docs_index.py`）与 `mmorpg-client/Docs/rules/minigame-and-app-platforms.md` 的跨端索引链接。
3. **阶段 2 实施（`SceneReady` 双服务端同步）**：
   - 在 `proto/scene/player_scene.proto` 增加 `SceneReadyC2SRequest` / `SceneReadyC2SResponse` 与 `rpc SceneReady`，按 `AGENTS.md §4` 跑服务端生成并同步客户端 `.\gen.bat proto`；
   - 在 `cpp/libs/services/scene/player/` 新增 `PlayerSceneLoadingComp`、落地挂接、超时扫描摘除与行为闸，在 `cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp` 接 `SceneReady` 回调并补齐单测（含超时、断线、二次切图、乱序 `scene_id`）；
   - 在 `xuanming-server-mmo`（Java 版）实现同等 `BaseScene` 开放门禁与 `SceneReady` 保护态，并在 `PARITY.md` 记账。
4. **阶段 3 实施（`mmorpg-client` 双源加载与预下载闭环）**：
   - 在 `Assets/Scripts/World/CurrentMaps/` 抽离 `IMapAssetSource`、`IMapAssetCache`、`ResourcesMapAssetSource`、`RemoteCdnMapAssetSource`（仅用 `UnityWebRequest` / `UnityWebRequestTexture`，严禁线程池与 `UnityEngine.Caching`），同步维护所有新增文件的 `.meta`；
   - 改造 `CurrentMapCatalog` 与 `CurrentMapTileStreaming` 通过 `IMapAssetSource` 读取 `source`/`walkmask`/`layout`/`tiles` 与分片，保留本地编辑器模式零配置可用；
   - 在 `GameClient` / `CityTravelUiRoot` 接入主动切图前一级包预下载（带 `downloadToken` 与会话代次校验）以及 `NotifyEnterScene` 落地后一级包就绪发 `SceneReadyC2S`；
   - 跑 `tools/client_compile_check.ps1` 与新增 EditMode 单测（涵盖取消下载、SHA 校验失败回退、旧令牌作废、缓存命中秒进、弱网心跳不中断）。
5. **阶段 4 实施（转小游戏包与发版前）**：
   - 编写离线地图纹理压制与 CDN 清单生成工具（输出 `webgl-low`/`webgl-mid`/`app-high` 及 SHA-256 校验回执），在 CI 增加正式包禁止包含 `Assets/Resources/World/` 大图的阻断校验；
   - 接入微信/抖音 Unity 转换插件的本地文件缓存适配层；
   - 在真实微信/抖音小游戏测试号与 iOS/Android 真机执行首包体积、跨图流畅度、弱网重连与显存峰值验收，归档实测证据。

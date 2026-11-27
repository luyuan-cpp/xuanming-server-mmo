# 离线名匠口碑雅号异步回账、营业战报与西市极品热榜设计方案

> 适用仓库：`xuanming-server-mmo-cpp`（C++/Go 权威）、`xuanming-server-mmo`（Java 对账）、`xuanming-server-mmo-python`（Python 平替）、`mmorpg-client`（Unity 小游戏 + App 双形态客户端）
> 关联前置设计：[craft-proxy-offline-and-support-enhance.md](craft-proxy-offline-and-support-enhance.md)

---

## 1. 需求分析

1. **补齐离线/跨节点代打的社交正反馈闭环**：
   - 上一阶段已打通全区 24 小时跨线/离线名匠代打与 95% 净银两经 `PlayerMailOutbox -> MailAdmin -> mail_asset_op` 异步入账；但宗师离线或跨节点期间成交的订单，尚未把**口碑单数（`proxy_orders_completed`）**、**极品产出数（`supreme_forged_count`）**、**稀世制书数（`rare_books_crafted_count`）**与由此触发的**零属性名匠雅号解锁（百炼宗师 / 天工神匠 / 万卷书圣）**回账给宗师。
   - 生活玩家睡一觉上线，应同时收获「邮件里的 95% 净银两」与「口碑成长 + 新雅号解锁 + 一目了然的离线营业战报」。
2. **提升西市名匠摆摊的逛街与社交竞争乐趣**：
   - 在西市名匠代打页增加「**全区极品热榜 Top 3**」展示条与「热榜优先」排序：实时叠加在线与离线尚未回账的成交单数与极品数，宗师即使离线也能在全区热榜上爬升，买家逛西市一眼识别高口碑与高产极品名匠并一键找他代打/代制。
3. **合规与反「上班感」红线**：
   - 零每日打卡、零日清活跃、零离线惩罚、零绑定材料洗白（继续严守 T/B 隔离与 5% 税销毁）、零自由文本 UGC 风险（最近买家名仅取服务端已过审角色名，雅号与招牌均为固定枚举）。

---

## 2. 设计分析

### 2.1 持久存档扩展（向后兼容，旧存档默认 0 合法）
- `PlayerCraftComp`（C++ / Python）与 `PlayerCraftState`（Java）新增未读营业战报字段：
  - `unread_proxy_digest_orders`（`uint32`）：尚未查阅的代打累计成交单数
  - `unread_proxy_digest_supreme`（`uint32`）：尚未查阅的极品产出次数
  - `unread_proxy_digest_rare`（`uint32`）：尚未查阅的稀世制书次数
  - `unread_proxy_digest_net_gold`（`uint64`）：尚未查阅的税后 95% 净银两总额（对应已寄往邮箱的分红）
  - `unread_proxy_digest_last_buyer`（`string`）：最近一位下单侠士角色名
  - `unread_proxy_digest_new_titles_mask`（`uint32`）：本期间新解锁的名匠雅号位图（bit `title_id - 1`，遵守位图存储规范）
- Java 侧同步补齐 `PlayerCraftState` 中原缺省的 `proxy_orders_completed` / `supreme_forged_count` / `rare_books_crafted_count` / `master_showcase_tag` / `equipped_craft_title_id` 持久字段，使三服口碑与雅号口径 100% 一致。

### 2.2 跨节点/离线共享待回账台账与 24h 热榜（Redis 同槽原子计数）
- 复用既有控制面/同区共享 Redis 同槽 hash tag，新增伴随 Hash 键：
  - C++：`craft_proxy_reputation:{ctl:<zone>}`（`PlayerCraftSystem::ProxyReputationLedgerKey(zoneId)`）
  - Java：`<ns>:world:{z:<zone>}:craft-proxy-reputation`（`RedisKeys.craftProxyReputation(zoneId)`，与 `craftProxyStalls(zoneId)` 同槽）
- 每位宗师 `M` 对应以下独立字段，**全部使用 Redis `HINCRBY` 原子增减**，杜绝多场景节点并发下单覆盖丢数：
  - `<M>:o` — 待回账口碑单数（买家 `+1`，宗师持久化成功后 `-drained`）
  - `<M>:s` — 待回账极品产出数
  - `<M>:r` — 待回账稀世制书数
  - `<M>:g` — 待回账税后 95% 净银两总额
  - `<M>:24o` — 近期热度单数（随代打成交 `+1`，按 24 小时自然窗或热榜统计展示）
  - `<M>:24s` — 近期极品热度数（随代打产出 5 档极品 `+1`）
  - `<M>:b` — 最近买家角色名（`HSET` 覆盖写最新一位）
- **实时热榜评分与 Top 3 授予规则（三服共享纯函数）**：
  - 实时热度分：`hotScore = recent_24h_supreme * 3 + recent_24h_orders`
  - 仅在营业且满足 `recent_24h_orders > 0 || proxy_orders_completed > 0 || supreme_forged_count > 0` 的宗师参与评选；
  - 排序键：`hotScore` 降序 → `recent_24h_supreme` 降序 → `supreme_forged_count` 降序 → `proxy_orders_completed` 降序 → `master_player_id` 升序（确定性破平局）；
  - 前 3 名依次授予 `hot_rank = 1 / 2 / 3`（金/银/铜榜），其余为 `0`。
- **实时叠加未回账口碑**：
  - `ListCraftProxyStalls` 返回时，对每条共享摊位快照实时叠加其尚未回账的 `pending(o, s, r)`，确保宗师离线时买家下单后刷新西市列表即刻看到口碑与热榜同步增长。

### 2.3 宗师上线/周期回账与防重防丢流程
- 触发时机：宗师登录载入存档（`SyncPlayerProxyStall` / `_load_player`）、5 秒共享目录周期同步、以及 `GetSkillCraftingState` 读路径。
- 回账顺序（严格先持久后扣减 pending）：
  1. 仅在玩家在线且 `IsPlayerMutable`（非战斗冻结/非退出态）且该玩家无在途 drain 时执行。
  2. 读取本节点镜像中的 `pending(o, s, r, g, lastBuyer)`；若 `o == 0 && s == 0 && r == 0 && g == 0` 直接跳过。
  3. 计算回账前已解锁雅号位图 `beforeMask`，累加 `proxy_orders_completed += o`、`supreme_forged_count += s`、`rare_books_crafted_count += r`（饱和加法防溢出），计算回账后位图 `afterMask`，将 `afterMask & ~beforeMask` 按位或入 `unread_proxy_digest_new_titles_mask`。
  4. 累加 `unread_proxy_digest_orders / supreme / rare / net_gold` 并更新 `unread_proxy_digest_last_buyer`。
  5. 触发存档持久化；**仅在存档提交（或入盘排程确认）成功后**，从本地 pending 扣除 `(o, s, r, g)` 并向控制面 Redis 发 `HINCRBY -o / -s / -r / -g`，同时把最新口碑与雅号重发至 `craft_proxy_stall:{ctl:<zone>}`。若存盘失败，保留 pending，下次 tick 自动重试，零丢单。

### 2.4 客户端协议扩展（零破坏、末尾追加）
- `SkillCraftingStateSnapshot` 追加字段 `24`：`CraftProxyDigestSummary unread_proxy_digest`（含 `orders_completed`、`supreme_forged_count`、`rare_books_crafted_count`、`net_gold_earned`、`last_buyer_name`、`newly_unlocked_title_mask`）。
- `CraftProxyStallInfo` 追加只读展示字段 `16..18`：`recent_24h_orders`、`recent_24h_supreme`、`hot_rank`。
- `SceneSkillClientPlayer` 末尾追加天然幂等 RPC：`AckCraftProxyDigest(AckCraftProxyDigestRequest) returns (AckCraftProxyDigestResponse)`（清空未读战报、落盘并返回最新 `state`；已清空时重放直接成功）。

---

## 3. 思想分析

1. **钱账与口碑账分治（资金强一致 + 口碑原子增量）**：
   - 真金白银（95% 净银两）继续完全依托买家同事务 `PlayerMailOutbox -> MailAdmin -> mail_asset_op` 的强一致链路，不引入新的资金风险面；
   - 口碑统计与离线战报采用 Redis `HINCRBY` + 玩家存档先落盘后扣减 pending 的轻量模式，避免新增整套跨服微服务与 DB 表，实现三服零分叉、低复杂度与高并发无锁累加。
2. **UI 渐进式美术替换**：
   - 客户端弹窗与热榜条预埋专属图集加载键并在缺图时无缝回退既有已过审控件，今天即可完整联调体验，待异机 Codex 产出精美切图后直接放入对应目录即可零改代码生效。

---

## 4. 漏洞分析与防线

| 潜在风险 | 缓解防线 |
|---|---|
| 多场景节点同时有买家给同一离线宗师下单导致计数覆盖丢单 | 全链路改用 Redis `HINCRBY` 原子增量，禁止整包 `HSET` 覆盖待回账字段 |
| 宗师回账存盘失败或崩溃导致口碑丢失 | 严格先落盘再扣减 pending（C++ 检查 `Persist` 返回值；Java/Python 在 `commit`/存盘成功回调后扣减），存盘失败则保留 pending 下次重试 |
| 重复进入界面或刷新导致重复弹出离线战报或重复扣除 pending | `AckCraftProxyDigest` 持久清零 `unread_proxy_digest_*` 且天然幂等；同一宗师 drain 加单飞标记防 5s tick 重入 |
| Redis 重启/清空导致永久口碑丢失或负值下溢 | 永久累计值存于角色 DB `PlayerCraftComp`；pending 仅作增量中转，读取时钳制 `>= 0` 永不下溢 |
| 极品数/单数整数溢出 | 全路径饱和加法（`uint32_max` / `uint64_max` 封顶） |
| 客户端未导入 Codex 新切图时出现白块/报错 | 所有新切图路径均做 `null` 判空并优雅回退现有已过审通用卷轴/印章/按钮控件 |

---

## 5. 步骤分析

1. **协议与代码生成**：更新 `player_craft_comp.proto`、`player_skill.proto`，在 `xuanming-server-mmo-cpp` 跑 `proto-gen-run`，再在 `mmorpg-client` 同步 `gen_proto.ps1` + `gen_messageids.ps1`。
2. **C++/Go 实现与单测**：更新 `equip_craft_quality_rules.h`、`player_craft.{h,cpp}`、`player_equipment_v1.cpp`、`player_skill_handler.cpp` 与 `player_craft_quality_test.cpp`，通过裸指针检查、英文日志检查、串行 MSBuild (`/m:1`) 与 GTest/Go 测试。
3. **Java 对账实现与单测**：同步 `game-proto`、更新 `RedisKeys` / `player_state.proto` / Schema Ledger / `EquipCraftQualityRules` / `CraftProxyStallDirectory` / `SkillCraftService` / `EquipmentCraftService` / `SceneNode` / `PARITY.md` 与 JUnit 测试。
4. **Python 平替实现与单测**：同步 `.proto` + `build_protocol.py`，更新 `scene_skill_craft.py` / `scene_equip_craft.py` / `scene.py` 与 `pytest`。
5. **Unity 客户端 UI 接入与验收**：更新 `SeasonEconomyUiArt.cs`、`SkillCraftClient.cs`、`SkillCraftModels.cs`、`SkillCraftWindow.cs` 与 `SkillCraftClientTests.cs`，跑 `client_compile_check.ps1` 与无头 NUnit 单测。

---

## 6. 执行步骤

- [x] 写入设计文档并运行 `python tools/scripts/gen_docs_index.py`
- [x] 更新 `player_craft_comp.proto` 与 `player_skill.proto` 并完成 C++/Go/客户端协议生成
- [x] C++ 实现离线口碑/雅号异步回账、战报确认 RPC 与西市极品热榜并通过单测与静态门禁
- [x] Java 同步实现并通过 Schema 与多模块 Maven 测试、更新 `PARITY.md`
- [x] Python 同步实现并通过 `pytest`
- [x] Unity 客户端接入离线营业战报弹窗与西市极品热榜 UI 并通过编译与 NUnit 测试
- [x] 核查四仓 `git status` 并在回复中交付完整可复制给 Codex 的纯作图交接正文

---

## 7. 可直接复制给 Codex 的异机作图交接正文

> 以下整段内容可直接复制发送给另一台电脑的 Codex 作图窗口。

```text
请制作「西市名匠离线营业战报与极品热榜」界面及独立切图，输出到图仓新专题目录：
D:\work\image\designs\craft-proxy-digest-and-ranking-v1\
（如换机请按图仓相对路径 designs/craft-proxy-digest-and-ranking-v1/ 定位）。

【开工必做：逐张查看并展示实图】
动笔前，请先核实并实际查看图仓以下标准参考图，并在当前回复中逐张显示图片、名称与准确图仓相对路径（缺图请先如实报告，不得用文字描述或其他图冒充）：
1. 帮会界面（首要标准）：designs/ranked-play-v2-ui-20261125/assets/approved-guild-reference.png
2. 邮件界面：designs/mail-ui-v1/01-mail-event.png
3. 组队界面：designs/team-ui-v2/team-ui-v2.png
以及同功能已确认参考图：
4. 主城名匠代打摊位界面：designs/craft-quality-skill-ui-20261124/screens/08-city-master-proxy-stall.png
5. 名匠托管横幅：designs/craft-quality-skill-ui-20261124/ui/banner_safe_escrow.png
6. 名匠荣誉图标：designs/craft-quality-skill-v2-v5-icons-20261126/icons/icon_master_honor.png
7. 同队免税角标：designs/craft-quality-skill-v2-v5-icons-20261126/ui/badge_team_free_proxy.png

【本次功能与界面用途】
1. 宗师「离线营业战报」弹窗：名匠开启 24 小时跨线/离线代打摊位后，上线打开代打界面时弹出一次结算喜报，展示离线期间累计成交单数、产出极品数、撰出稀世数、已寄往邮箱的税后净银两总额、最近下单侠士名，以及新解锁的宗师雅号提示，底部放两个按钮（左侧收下战报，右侧直达邮箱领银两）。
2. 西市「极品热榜 Top 3」展示条：嵌在主城名匠代打摊位页中段，横向并排展示当前全区热度排名前三的名匠卡片（第 1 名、第 2 名、第 3 名），每张卡左上角挂名次角标，卡片内动态叠放名匠名称、近 24 小时单数与极品数，以及快捷代打/代制入口。

【交付清单与文件规格】
全部独立切图必须为透明底 PNG-32，界面内所有动态文字区一律彻底留空、切图内不带任何中英文文字或数字，并保证九宫格/横向拉伸安全留白：

一、整屏布局预览图（仅作布局对照，目录：previews/）
1. previews/01-proxy-digest-modal-preview.png — 1920x1080，展示「离线营业战报」弹窗叠加在名匠代打界面上的整体布局、数据行区、新雅号喜报栏与底部双按钮位置。
2. previews/02-west-market-hot-ranking-preview.png — 1920x1080，展示名匠代打界面中段新增「极品热榜 Top 3」三卡横向条与下方名匠列表的整体排布。

二、独立无字切图（全部透明底 PNG-32，目录：ui/ 与 icons/）
1. ui/panel_proxy_digest.png — 1200x820，无字弹窗主底板，预留顶部标题牌位、中部四项战报数据留空区、新解锁雅号提示带位与底部双按钮区；四周边框内缩留出 64px 九宫格安全拉伸区，内部留白干净均匀，无任何文字与数字。
2. ui/banner_hot_masters.png — 1400x96，无字横幅底条，用于西市极品热榜 Top 3 区段标题栏；左右两端各预留不超过 120px 的固定边界区，中段横向平整可拉伸，完全无字。
3. ui/card_hot_master.png — 440x168，无字单张热榜名匠底卡（用于第 1～3 名三卡并排）；四角保留 32px 安全九宫格边距，左上角预留 72x72 名次角标贴位，右侧预留文字与按钮留空区，完全无字。
4. ui/badge_hot_rank_1.png — 128x128，独立图标角标，表示热榜第 1 名（榜首），居中构图四周留白 ≥ 8px，严禁带数字“1”或文字。
5. ui/badge_hot_rank_2.png — 128x128，独立图标角标，表示热榜第 2 名（榜眼），居中构图四周留白 ≥ 8px，严禁带数字“2”或文字。
6. ui/badge_hot_rank_3.png — 128x128，独立图标角标，表示热榜第 3 名（探花），居中构图四周留白 ≥ 8px，严禁带数字“3”或文字。
7. ui/badge_title_unlocked.png — 480x76，无字横幅喜报标签底条，用于战报弹窗内展示「新解锁雅号」文字（运行时居中叠字），左右两端各预留 ≤ 72px 的固定边界区，中段横向平整可拉伸，完全无字。
8. icons/icon_digest_scroll.png — 256x256，独立图标，表示宗师离线营业账册/战报，用于弹窗顶标与摊位区「营业战报」按钮配图，居中构图四周留白 ≥ 16px，完全无字。

三、配套交付记录（目录根目录）
- manifest.json：记录每张成品的文件路径、像素尺寸、是否透明底、九宫格/拉伸安全边距与对应参考图清单。
```

---

## 8. 本机研发导入备忘（不发给 Codex）

- 待 Codex 在 `D:\work\image\designs\craft-proxy-digest-and-ranking-v1\` 交付并经人工视觉验收后，将 `ui/` 与 `icons/` 下 8 张无字 PNG-32 导入 `mmorpg-client/Assets/Resources/UI/Ugui/CraftProxyDigest/`（同步生成 `.meta`，Texture Type 设 `Sprite (2D and UI)`，需要九宫格拉伸的 `panel_proxy_digest`、`banner_hot_masters`、`card_hot_master`、`badge_title_unlocked` 按 `manifest.json` 设好 border）。
- 客户端 `SeasonEconomyUiArt.CraftProxyDigestRoot = "UI/Ugui/CraftProxyDigest/"` 与 `SkillCraftWindow.cs` 已预接全部 8 个资源键并带无图安全回退，导入后无需改动 C# 业务逻辑即可直接显示正式切图。

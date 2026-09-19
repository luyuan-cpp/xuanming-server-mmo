# 91 最终批次计划与 Codex 验证(第 1 部分:批次顺序、文件数、授权点)

> 依据:契约 §1、S1–S6 的批次清单、`90_consistency*.md` 的修正。文件数 = **手改文件**,不含生成物、go.sum/vendor、文档与 PROGRESS(统一口径,Y-18)。每批开工前单独取得用户授权(契约 §1);Claude 不构建,全部验证由 Codex 串行执行(第 2 部分)。

## 1. 批次顺序

落地一律**串行**(proto-gen、导表、MSBuild、Tip.xlsx 都不能并行);代码可以提前写好,但按下表顺序合入。

| 序 | 批次 | 内容 | 手改文件 | 相对各节原数 | 依赖 |
|---|---|---|---|---|---|
| 0 | P0 | 不改代码:拍板 D1–D10;交易会话提交 schemamigrate 的 proto2mysql replace(或打 v0.1.2 tag) | 0 | — | — |
| 1 | B1 | 独立库 `mmorpg_guild`、schemamigrate、超时预算、friend 去门表、测试助手(含 X-09 改为按 `Tables()` 派生) | 23 | = | P0 |
| 2 | B1b | merge_zone / data_consistency_check `-guild-schema` | 9 | = | B1 |
| 3 | B2s | 管理与申请服务端、推送、GuildRule/GuildLevel、C++ 表登记(顺带补 ActivitySchedule)、robot 编译级;含 X-10 状态锁行、D7 RC、D10 uint64、Y-03/Y-04 | 30 | = | B1 |
| 4 | B2c | 客户端管理与申请、MessageLimiter 8 行、robot 管理段 M1–M10 | 9 | = | B2s |
| 5 | B3a-1 | 名字注册表(data_service)、RoleNameRule、login/user_accounts proto、`profile_component = 15`、login tip | 29 | = | — (与 B2 无代码依赖,但共用 Tip.xlsx / 表工程文件,排在 B2c 之后) |
| 6 | B3a-2 | login 建角与补齐、scene/Java 解析、merge_zone 按列名拷贝、stress 段 | 23 | = | B3a-1 |
| 7 | B3b | guild 批量取名(`WithPlayerNames`)、建角界面、成员显示名 | 20 | +1(Y-07 `guild_manage_logic.go`) | B2c、B3a-2 |
| 8 | B4a-1 | 资产通道 C++ 本体、`asset_op_ledger = 16`、asset tip 9 行、签名校验 | 30 | = | — |
| 9 | B4a-2 | 客户端 GM 统一闸门 | 6 | = | B4a-1 |
| 10 | B4a-client | Pet/Attribute 客户端 27000–27008 镜像 | 2 | = | B4a-1 |
| 11 | B4b | `go/shared/scenenode`、`go/shared/assetop`、聚宝斋文档改名 | 22 | −3(文档不计) | B4a-1 |
| 12 | B5a | guild_db 资产三表完整定义(part2 §1)、guild.proto 经济 RPC、GuildDonate/GuildShop、TX_GUILD_DONATE_REFUND、白名单、gameday、号段登记 | 20 | −1(不改 GuildLevel.xlsx) | B2c、B4b |
| 13 | B5b | guild 经济服务:仓库、Store(ListDue+Claim、纪元、签名)、离帮提前截止、assetopfix、`go_services.ps1` 密钥 | 23 | 净 0(−`guild_manage_logic.go` −`push.go` +`tables.go` +`go_services.ps1`) | B5a |
| 14 | B5c | robot 经济冒烟、客户端捐献/商店/总览、货币改名 | 12 | = | B5b |
| 15 | B5d | 回档 fail-closed:guild `GuildInternal.ListAppliedAssetOpsSince`、data_service `GetPlayerAssetOpLedger` 与 Rollback 前置检查、guild `LedgerReader` 接线 | 估 ≤18 | 新增(D3) | B5b;**需先补详细设计**(S4 4.38/4.39 只给了接口) |
| 16 | B6a-srv | GuildActivity 表、灯会/团圆、活动 tip 10 行、MessageLimiter 5 行;#18 改为 `guild_manage_repo.go` | 30 | = | B5b |
| 17 | B6a-cli | 活动页、灯会/团圆客户端、robot activities 段 | 9 | −2(文档/PROGRESS 不计) | B6a-srv |
| 18 | B6b-srv1 | battle 回显与结果持久化、match `MatchInternal` | 17(+≤3 生成工程登记) | = | B6a-srv;与组队、属性会话协调 |
| 19 | B6b-srv2 | guild 历练:邀请房间、结果消费、结算、owed 循环、巡检器 | 21 | −1(文档不计) | B6b-srv1 |
| 20 | B6b-cli | 历练选人与邀请客户端、robot S9–S15 | 6 | −1(PROGRESS 不计) | B6b-srv2 |
| 门禁 | B4c | 玩家存盘属主围栏 | ≤12 | 单独评审 | B4a-1;**任何共享/预发环境开启帮会资产操作之前** |
| 上线 | BK8s | 见 `90_consistency_part4.md` G-05 | 另计 | — | B4c |

**可调整的顺序**:B3(序 5–7)与 B4(序 8–11)互不依赖,可整体对调以尽早开工 B5;对调后 `asset_op_ledger` 取 15、`profile_component` 取 16(G-03 规则不变)。
> **"对调"指的是把本表里两个批次块的序号整体互换,不是"谁先动手谁先占号"。**
> 2026-09-18 踩过一次:B4a 因为别的会话先开工而先落码,被误判成"已对调",差点让 `asset_op_ledger` 取 15 而与
> `03-names.md:543` 已写死的 `profile_component = 15` 撞号。表没改 = 没对调,字段号一律按 90 清单 G-03 的
> `profile_component = 15` / `asset_op_ledger = 16`。B3b 只依赖 B2c 与 B3a-2,可晚于 B5c。B5d 可与 B6 并行编写,但合入仍串行。

## 2. 用户授权点(除"每批开工前授权"外的额外拍板)

| 时点 | 需要用户确认 |
|---|---|
| P0 / B1 前 | D1–D10;交易会话提交或打 tag(proto2mysql 191 前缀与 TiDB 选项) |
| B2s 前 | 新 tip `GuildBusyRetry`;事务改 RC(需 `binlog_format=ROW`);`guild.proto` 时间戳改 uint64;JoinGuild 删除后 B2s→B2c 窗口内旧客户端"加入"按钮失效 |
| B2 验收后 | root 执行 `DROP TABLE IF EXISTS mmorpg.guild_member, mmorpg.guild, mmorpg.guild_schema_migration`(S1 §19 第 5 步,永久删表) |
| B3a-2 端到端前 | 删除冒烟账号数据 `DEL account:robot_*`(或本地 FLUSHALL),`player_name` 可 TRUNCATE(永久删数据) |
| B4a-1 前 | 资产 tip 新增 `AssetPartialApplied`、`AssetAuthFailed`;冒烟专用账号 robot_9501 |
| B5a 前 | D2 离帮退款规则;D3 是否做 B5d;D4 只做 CLI |
| B5b 验证 | 开发库重建 `DROP DATABASE mmorpg_guild`(只在需要时;新表由 schemamigrate 自动补建,首次不必删) |
| B5d 前 | 补齐的详细设计(数据服务调 guild 的鉴权与拒绝语义) |
| B6a-srv 前 | U1(当期=档期)、U2(阵亡不得奖);活动 tip 5 个契约外新码;邀请确认制 |
| B6a-cli 冒烟 | 对 4 个测试账号执行 `DELETE FROM mmorpg_guild.guild_daily_counter …`(S6 §6.44,永久删行) |
| B6b-srv1 前 | 与组队会话确认 `gather.go` 改动方式,与属性会话确认 `battle_data.proto` 已提交;Kafka 结果事件新增字段 |
| 共享环境前 | B4c 已落地并验证 |
| 上线前 | BK8s 清单、密钥注入、NetworkPolicy |

## 3. 每批合入前的公共检查(Codex 与实现方共同遵守)

1. `git status --short` 快照(目录见第 2 部分 §0),他人未提交产物生成前后不得变化。
2. 与聚宝斋、组队、聊天会话确认本时段无人跑 proto-gen / 导表 / MSBuild;Tip.xlsx、MessageLimiter.xlsx 无他人未提交修改(G-08)。
3. 本批落码前重读 `90_consistency*.md` 中点名本批的条目;行号按函数名重新定位。
4. 交付说明必须写:实际手改文件清单与数量、`message_id.txt` 新增项、tip 新增项、是否需要删库重建、EditMode 用例增量(G-09)。

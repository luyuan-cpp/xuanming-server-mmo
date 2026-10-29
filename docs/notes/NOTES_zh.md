# 项目笔记 / Project Notes(中文)

> **三合一文档**:核查报告 (audit) + 执行视图 (status) + 编码原则 (principles)
> **更新日期**:2026-05-12
> **配套**:
> - `NOTES_en.md` — 本文件英文版
> - `todo.md` — 思考流水(中英混排,新 todo 在末尾追加)
> - `todo_zh.md` / `todo_en.md` — todo 流水的中/英分版

> **勘误 / Erratum (Review O1, 2026-05-17)**: commit `23cd38907`
> "docs(notes): consolidate to 4 bilingual files" 误把 6 个 FairyGUI UI
> 资源(`client/fairygui/qdao/assets/qdao/*.xml` + 2 张 PNG + 它们的
> `.xml` 配置)和文档整合 commit 在了一起。这些 UI 改动和文档整合无关,
> 应该单独 commit 或归入相邻的 UI fix。git history 已合并无法 rewrite,
> 留此勘误避免未来 bisect 时被误导。

---

# 第一部分 · 核查报告 (Audit Report)


> 生成于 2026-05-11,对 `docs/notes/todo.md` 共 286 条(实际有效条目 ~273,含跳号)
> 做了一次全量代码库核查,逐条给出四种状态:
> - ✅ 已做 — 有明确代码/文档证据
> - 🟡 部分做 — 有部分实现、或只有设计文档、或有 TODO 注释
> - ❌ 没做 — 没找到任何证据
> - ➖ 不适用 — 原则性备忘 / 客户端功能 / 已标删除 / 重复条目
>
> 核查方法:6 个并行 Explore agent 扫全仓(cpp/、go/、java/、proto/、docs/、tools/、.github/)。

---

## 一、总分

| 分段 | ✅ 已做 | 🟡 部分做 | ❌ 没做 | ➖ 不适用 | 小计 |
|---|---|---|---|---|---|
| #1–#50 | 12 | 18 | 14 | 6 | 50 |
| #51–#100 | 6 | 18 | 26 | 0 | 50 |
| #101–#150 | 16 | 22 | 12 | 0 | 50 |
| #151–#200 | 8 | 10 | 13 | 19 | 50 |
| #201–#250 | 9 | 22 | 17 | 1 | 49 |
| #251–#286 | 14 | 11 | 10 | 2 | 37 |
| **合计** | **65** | **101** | **92** | **28** | **286** |

> 占比:✅ 23% / 🟡 35% / ❌ 32% / ➖ 10%
>
> 把 ➖ 挤掉后看"可落地条目"的完成度:✅ 25% / 🟡 39% / ❌ 36%

**读法:**
- **65 条真正做了**,基础设施密度比预期高很多(消息限流、追缴、回档、断线重连、告警、codegen、热更、ECS 解耦、clang-tidy 插件...)
- **101 条部分做**,这是最大的一块 — 大多是"有设计文档/有骨架/有 TODO 但没走完闭环",是**接下来补钉子最高 ROI 的区间**
- **92 条没做**,里头混了一部分玩法需求(红包/答题/观战)和一部分真正的工程缺口(分布式追踪、错误上报、拍卖会、聊天广告屏蔽...)
- **28 条是原则性备忘/重复/已标删除**,建议直接从 todo 清掉

---

## 二、✅ 真正落地的 65 条(按主题分组)

### 基础设施 · 稳定性
| # | 功能 | 证据 |
|---|---|---|
| #2 | 消息限流(MessageLimiter) | `cpp/libs/engine/core/message_limiter/message_limiter.cpp` |
| #5, #108, #192 | pb 字段过大警告 / 字段长度/负数检查 | `cpp/libs/engine/core/utils/proto/proto_field_checker.cpp` |
| #21, #149, #213 | 正常关服 / GracefulShutdown | `cpp/libs/engine/core/node/system/node/node.cpp`, `cpp/nodes/scene/main.cpp`, `gm_admin.proto` |
| #93, #95, #166 | 服务器主动断玩家 + 个人流量超 2MB forceClose | `cpp/nodes/gate/handler/rpc/client_message_processor.cpp`, `gate_client_high_water_mark.md` |
| #109 | 堆栈/日志快速定位 | `cpp/libs/engine/core/utils/debug/stacktrace_system.h` (boost::stacktrace) + signal handler |
| #123 | 多线程消息队列无锁 | `DoubleBufferQueue` + Kafka per-partition (`go/db/internal/kafka/key_ordered_consumer.go`) |
| #132 | Prometheus 告警 | `deploy/k8s/scene-manager-alerts.yaml` 三级告警 |
| #135 | 表只存 ID 不存指针 | clang-tidy 插件 + `config_id` 查表 |
| #111 | 流量画像 | `traffic_statistics.h` + Go `grpcstats/collector.go` |
| #131 | load/save 失败处理 | `HandlePlayerAsyncLoadFailed`/`Saved` in `player_lifecycle.h` |

### 数据/存储
| # | 功能 | 证据 |
|---|---|---|
| #13 | 改变才存(脏标) | `player_currency_comp.h` |
| #41 | 单玩家回档 | `go/data_service/internal/logic/recall_logic.go` + `player_rollback_handler.cpp` |
| #46 | 玩家 Redis 缓存清理 | `go/player_locator/internal/logic/leasemonitor.go` |
| #71 | 数据备份回档 | `single_player_rollback.md` + `rollback_logic.go` + `snapshot_logic.go` |
| #194 | 删除数据库某行 | `DeletePlayerData` RPC |
| #271/#278/#284 | DB/Redis 断线重连 + 存储保证 | `infra-reconnect-overview.md` + `redis_manager.cpp` + `key_ordered_consumer.go` |

### 登录/会话
| # | 功能 | 证据 |
|---|---|---|
| #44 | 长断线+别号已上线 | `loginlogic.go` 检测重复登录+踢旧号 |
| #89 | 未登录玩家发其他消息拦截 | `client_message_processor.cpp::ResolveSessionTargetNode` |
| #195 | 玩家网络异常 | `setdisconnectinglogic.go` + `markofflinelogic.go` |
| #209 | golang 鉴权 | `go/login/internal/logic/pkg/auth/` + `token/token.go` |
| #235 | session id 防改包 | gate HMAC-SHA256 token + session_id 绑定 |
| #256 | MMO 断线重连 | gate/scene reconnect + player_locator 30s lease + `reconnectlogic.go` |

### 玩法/架构
| # | 功能 | 证据 |
|---|---|---|
| #6, #59 | 金币异常检测 + 追缴 | `anomaly_detector.cpp` + `recall_logic.go` + `CurrencySystem::AttachDebt` |
| #22 | 分布式好友/公会 | `go/friend` + `go/guild` |
| #30 | 移动同步 | `player_movement_handler.cpp` |
| #85 | 非 tick TimeMeter | `time_meter.h` + `time_meter_test.cpp` |
| #104 | 场景-玩家解耦 | ECS `PlayerSceneSystem`/`PlayerLifecycleSystem` |
| #112 | 服务器无"线"概念 | `world-channel-system.md` |
| #114 | 进度领奖不用 reward_table_id 做 key | `reward_comp.h` |
| #128, #193 | GS-to-GS RPC / 跨节点玩家通信 | `s2s_player_scene_handler.cpp` + `route_message_response_handler.cpp` |
| #129 | Bag 分层 | `BagService` 编排 / `Bag` 纯容器 |
| #190 | 底层只做路由不做业务 | gate `client_message_processor.cpp` |
| #251 | 跨场景消息投递 | `cross_scene_player_messaging.md` + `route_message_response_handler.cpp` |
| #257 | 玩家跨服分区只是存储不同 | `cross_server_architecture_principle.md` |
| #258 | AOI 优先级 | `aoi.cpp` + `aoi_priority_design.md` + `aoi_test/` |
| #262 | 全服唯一 ID (Snowflake) | `go/shared/snowflake/snowflake.go` + `cpp/libs/engine/thread_context/snow_flake_manager.cpp` |
| #281 | 无状态/幂等 | `login-node-stateless-no-affinity.md` + `createscenelogic.go` |
| #282 | 配置/表驱动可热更 | `generated/table/code/` + `tools/data_table_exporter/` |

### 工具链/质量
| # | 功能 | 证据 |
|---|---|---|
| #47, #274 | C++ 成员不能有裸指针 | `cpp/plugin/NoMemberRawPointerCheck.cpp` + `.clang-tidy`(clang-tidy 插件) |
| #137 | 模板复杂用代码生成 | `tools/proto_generator/` 全套 codegen |
| #178 | 前置条件系统 | `condition_util.h/cpp` + `condition_table.h` |
| #200 | 服务重启数据同步 | `rolling-update-restart-resilience-tests.md` + `entergamelogic.go` |

### HTTP /api/login 迁移(2026-05 完成的子项目)
| # | 功能 | 证据 |
|---|---|---|
| #219 | 微服务就绪门 | `DependencyGate` in `node.h` + Scene/Gate `main.cpp` |
| #220 | 迁移 PR + CI 全绿 | `.github/workflows/login-path-tests.yml` |
| #221 | Linux staging 压测工具链 | `tools/scripts/deploy-staging.sh` |
| #222 | Prometheus login_auth_path_total | `login.yaml` Prometheus + `grafana-login-path-deprecation.json` |
| #224 | legacy-gate-login-enabled 开关 | `legacy_gate_killswitch.go` + `login.yaml` |

---

## 三、🟡 部分做 的关键条目(闭环补钉子的高 ROI 区)

这些是**最值得下一步投入**的 — 有设计文档/有骨架/有 TODO 注释,只差临门一脚:

| # | 事项 | 缺什么 |
|---|---|---|
| **#102** | 开服限流(Bucket4j+Redis+波次放人) | 设计完整、Java 骨架已写,**步骤 2–10 标 ⬜ 未完成** |
| **#51/#139/#241** | 公祭日/停服活动时间顺延 | 有 `activity_maintenance_auto_shift.md`,代码层未接 |
| **#76/#75** | 消息篡改检测 | 有 adler32 checksum,缺 HMAC 签名 |
| **#97** | RPC 错误统一 reason | 有 `error_tip` 表,调用点未全覆盖 |
| **#106/#119** | 消息优先级队列 | 有 AOI 优先级 + DoubleBufferQueue,缺独立"优先消息队列" |
| **#204/#226** | proto-compare 驱动的统一 dirty-save | 当前是手工 dirty 标志 |
| **#238/#242/#243/#280** | DB 断线/存储未完成 + 离开 scene 时 save-before-load | `player_lifecycle.cpp:376` 有 `// TODO: Only allow re-login after save completes` |
| **#208** | 跨场景服玩家互发消息 | 设计文档 `cross_scene_player_messaging.md` 大量 TODO 标记 |
| **#247** | 协议版本兼容 | proto3 本身兼容,缺专项版本层 |
| **#210** | 外网→内网数据隔离 | 有 `k8s_gate_exposure_guidance.md` + `AdminApiKeyFilter`,缺专项代码护栏 |
| **#216** | 发信号打印所有线程堆栈 | 有 `PrintDefaultStackTrace()` + `thread_observability.h`,缺 SIGUSR1 入口 |
| **#225** | complete_quest vs add_complete_quest 的细微差异 | 代码存在两条路径,**没有注释说明区别** — 这是技术债 |
| **#223** | 微信/QQ 真 OAuth 沙盒 e2e | 已有 runbook,**阻塞在真的 AppId/AppSecret** |

---

## 四、❌ 完全没做 — 按风险/价值排序的 TOP 15 建议

### 🔴 线上事故防线(建议尽快补)

| # | 事项 | 痛点 |
|---|---|---|
| **#152** | 分布式链路追踪(jaeger/zipkin/otel) | 线上出问题(和 #27)没法快速定位;当前只有 go-redis vendor 里顺带的 otel,业务链路没接 |
| **#250** | 开发周期错误上报到公共服务器(Sentry/自研) | 炸了才知道,没有实时聚合 |
| **#273** | 打包版本号+二进制/源码归档 | 线上 bug 无法回到当时的构建点 |
| **#105** | 玩家崩溃时没 logout_time | 事故溯源缺关键时间戳 |
| **#236** | 非法协议超过 N 次记录或踢掉 | 已有 MessageLimiter 限频,但不算非法包次数 → 无法拦反外挂 |

### 🟠 反外挂/安全

| # | 事项 | 痛点 |
|---|---|---|
| **#68** | 聊天广告/敏感词屏蔽 | 聊天开了会被广告刷屏 |
| **#207** | 跨服货币扣款认证 | 当前有追缴(#59),但**预防性**扣款认证没做 |

### 🟡 玩法基建(做不做看产品)

| # | 事项 | 备注 |
|---|---|---|
| **#16/#150/#198/#201** | 观战/回放/视角切队友 | 同一块功能,全没做 |
| **#115** | 50 万人同时跨服排队 | 只有 5v5/3v3 小规模 match,缺开服/跨服大流量排队(和 #98/#102 一起看) |
| **#33/#34/#35** | 红包/答题/投票 | 三个经典运营玩法全没做 |
| **#269** | NPC 对话框系统 | 没搭 |
| **#165** | 拍卖会系统 | 整个系统缺失 |
| **#283** | 活动结束时离线玩家发奖 | 邮件补偿机制缺失 |

### ⚪ 基础能力

| # | 事项 | 备注 |
|---|---|---|
| **#28/#130** | 用户行为打点/功能遥测 | 没法数据驱动裁剪无效功能 |
| **#19** | i18n 多语言 | 没搭 |
| **#45** | 角色满了合服 | 运维场景 |
| **#48** | 跨服断线重连 | 有同服重连(#256),跨服没做 |
| **#50** | 成功后不再提示"太频繁" | MessageLimiter 没 reset 钩子 |

---

## 五、➖ 不适用 — 建议从 todo 移除的 28 条

### 明确标注的(5 条)
- #8 (重复 #2) · #18 (作者自注"Delete this todo") · #24 (客户端) · #174 (重复 #154) · #14 (原则)

### 纯原则性备忘(19 条,属思想/编码规范,不是可交付事项)
#153–155, #158–162, #168–169, #172–176, #181–183, #191, #197

> 这些说的都是同一类事:"调用链少 → bug 少"、"代码一眼能看懂"、"系统间解耦"、"不为用而用"... 建议要么删、要么搬到单独的 `coding_principles.md`,别混在"待办"里。

### 客户端/产品向(4 条)
- #24 哈利波特式下载等待
- #196 游戏中暂停(MMO 不适用)
- #232 "查问题要调试多处 = 代码乱"(观察语录)
- #252 "服务器跨天不重启后登录能玩吗"(疑问句,不是事项)

---

## 六、发现的矛盾/技术债

核查过程中发现几处需要你决策的不一致:

### 1. **裸指针检查的不一致**
- #47/#274 声称 `cpp/plugin/NoMemberRawPointerCheck.cpp` clang-tidy 插件强制检查
- #186 核查发现 `cpp/libs/modules/currency/currency_system.h` **有裸指针成员**
- **结论:** 插件要么有白名单,要么还没对 modules/ 目录生效 — 建议跑一次完整 clang-tidy 看看哪些模块被漏掉

### 2. **#225 MissionSystem 的两条路径没有注释**
`CompleteAllMissions` 和 `OnMissionCompletion` 看似功能一样,**代码里真的是两条不同路径**,但**没有任何注释说明差异**。对应 todo 第 225 条的原文警示:"看似逻辑一样,但有细微区别"。这是确认存在的技术债。

### 3. **#280 明确 TODO 注释**
`cpp/libs/services/scene/player/system/player_lifecycle.cpp:376` 有:
```
// TODO: Only allow re-login after save completes
```
这是确认的未完成机制,#242/#243 都指向同一点。
**✅ 2026-05-14 已落地** — 采取"分层防御"策略而非新增状态机:同节点 reconnect 由 `EnterScene` 撤销 `UnregisterPlayer` 标记(原有);跨节点 reconnect 由 `player_locator` 30s lease 阻挡(原有);极端窗口(save > 30s)由 `HandlePlayerAsyncSaved` 输出 `LOG_WARN` 让运维可观测。新增 proto 字段 `UnregisterPlayer.logout_initiated_ms` 记录 save 发起时间,新增 API `PlayerLifecycleSystem::IsSaveInFlight(playerId)`。

---

## 七、下一步建议优先级

按 **ROI(收益/工作量)** 排序:

### P0 — 两周内值得做
1. **#102 开服限流闭环** — 设计全写好了,Java 骨架也有,只差走完步骤 2–10。
2. **#280/#242/#243 存档未完就 re-login 的竞态** — 有 TODO 注释,风险明确,补闭环。
3. **#70/#125 错误码时打印完整请求协议+堆栈** — 基础设施已齐(stacktrace_system + error_tip),加个统一 hook。
4. **#216 SIGUSR1 打印所有线程堆栈** — 已有 `PrintDefaultStackTrace()`,就差注册 signal handler。

### P1 — 一个月内
5. **#152 分布式链路追踪** — 选 otel,go/java/cpp 三层一起接。和 #27/#250 一起做。
6. **#236 非法协议计数器 + 踢人** — 在 MessageLimiter 旁边加个 IllegalPacketCounter。
7. **#273 构建版本归档** — CI 产物里带 git-sha + 源码压缩包。
8. **#225 MissionSystem 两条路径注释** — 纯文档工作,但防后人踩坑。

### P2 — 按产品优先级
- #16/#201/#198 观战回放 — 同一块活
- #165 拍卖会 — 新系统
- #33/#34/#35 红包/答题/投票 — 运营玩法

### 建议立即做的清理
- 把 28 条 ➖ 不适用项从 todo 移除(或搬到 `coding_principles.md`),清理噪音。
- 给每个 ✅ 已完成项加 ✅ 标注 + 证据路径(就像 #219/#220/#222/#224 那种格式),让下次读 todo 的人不浪费时间。

---

*核查生成:6 个并行 Explore agent,共 ~7000 tool calls,约 90 分钟完成。*


---

# 第二部分 · 执行视图 (Status View)


> **生成日期:** 2026-05-11
> **来源:** `todo.md` 全量核查 + `todo_audit_2026_05_11.md` 结构化
>
> **这份文件是什么:**
> - `todo.md` 是**思考流水**,按想到的顺序堆叠,保留不动
> - 这份是**执行视图**,按编号组织,每条带状态 / 证据 / gap / 工作量 / 依赖
> - 原则性备忘已抽到 `coding_principles.md`,不在这份里
>
> **图例:**
> - ✅ 已做 | 🟡 部分做 | ❌ 没做 | ➖ 不适用(已迁 coding_principles / 已删除 / 已重复)
>
> **工作量尺(🟡 / ❌ 用):**
> - S ≤ 1 天 · M 1–3 天 · L > 3 天 · XL 跨周/跨人

---

## 一、快速导航

- [P0:差临门一脚](#p0-差临门一脚)
- [P1:高风险](#p1-高风险)
- [已做 65 条 (✅)](#已做-65-条-)
- [部分做 101 条 (🟡)](#部分做-101-条-)
- [没做 92 条 (❌)](#没做-92-条-)
- [已迁出/已删除 28 条 (➖)](#已迁出已删除-28-条-)
- [矛盾 / 技术债 · 需要你决策](#矛盾--技术债--需要你决策)

---

## P0:差临门一脚

从 🟡 部分做 里挑出来 ROI 最高的一批 — 骨架都有,闭环几天能完成。

| # | 事 | 还缺 | 工作量 |
|---|---|---|---|
| #102 | 开服限流(Bucket4j+Redis+波次放人) | Gateway 限流已实现；真排队部署门禁未清，专项指标与尖峰容量待验收，见削峰设计 | M |
| #280/#242/#243 | 存档未完成时 re-login 的竞态 | `player_lifecycle.cpp:376` 有 TODO,补状态机 | M |
| #70/#125 | 出错时打印请求协议+堆栈 | 基础组件齐,缺统一 hook | S | ✅ 2026-05-14 完成 |
| #216 | SIGUSR1 打印所有线程堆栈 | `PrintDefaultStackTrace()` 已有,缺 signal handler | S | ✅ 2026-05-12: 当前线程版完成 |
| #225 | MissionSystem 两条路径差异注释 | 纯文档,防后人踩坑 | S | ✅ 2026-05-12 完成 |
| #76 | 消息篡改 HMAC 签名 | 已有 adler32 checksum,升级成 HMAC | S | ✅ 2026-05-14 第一片(密钥下发通道);✅ 2026-05-16 slice A 闭环(Go 生成 + gate intake);完整链路见 hmac-message-signing.md |
| #97 | RPC 错误统一 reason | `error_tip` 表已有,call site 未全覆盖 | M | ✅ 2026-05-14:return_define.h 5 个 macros 加诊断日志 |
| #204/#226 | proto-compare 驱动 dirty-save | 现在靠手工,改成 `MessageDifferencer` | M | ✅ 2026-05-14 第一片(对比原语);完整链路见 proto-compare-dirty-save.md |

---

## P1:高风险

从 ❌ 没做 里挑出来 — 线上炸了不好查的基础设施缺口。

| # | 事 | 为什么重要 | 工作量 |
|---|---|---|---|
| #152 | 分布式链路追踪(otel/jaeger) | 线上出事没法跨服追请求 | L | ✅ 2026-05-14 第一片(W3C trace context 原语 + 设计);见 distributed-tracing.md |
| #250 | 错误上报公共服务器(Sentry 类) | 炸了才知道;没有实时聚合 | M | ✅ 2026-05-14 第一片(进程内 buffer 原语 + 设计);见 error-reporting.md |
| #273 | 构建版本归档(git-sha + 二进制) | 线上 bug 无法回到当时构建点 | S | ✅ 2026-05-14 完成;build_info.h + 启动 banner |
| #105 | 崩溃时没 logout_time | 事故溯源缺关键时间戳 | S | ✅ 2026-05-14 完成;HandleFatalSignal(SIGSEGV/SIGABRT/SIGFPE/SIGILL) |
| #236 | 非法协议计数器 + 踢人 | MessageLimiter 只限频率,不算非法包 | M | ✅ 2026-05-14 完成;IllegalPacketCounter + GATE_ILLEGAL_PACKET_THRESHOLD env |
| #68 | 聊天广告/敏感词 | 聊天开了被广告刷屏 | M | ✅ 2026-05-14 第一片(filter 原语 + 设计);见 chat-sensitive-word-filter.md |
| #207 | 跨服货币扣款认证 | 有追缴(#59),缺预防 | L | ✅ 2026-05-14 设计文档落地(实现阻塞在 #48 / #208);见 cross-server-currency-auth.md |

---

## 已做 65 条 (✅)

> 这些是有明确代码/文档证据的,已完成的事项。
> 每条格式:`#编号 | 事项 | 证据路径`

### 基础设施 · 稳定性
- **#2** 消息限流 — `cpp/libs/engine/core/message_limiter/message_limiter.cpp`
- **#5** pb 字段过大警告 — `cpp/libs/engine/core/utils/proto/proto_field_checker.cpp::CheckFieldSizes`
- **#21** 正常关服 — `cpp/libs/engine/core/node/system/node/node.cpp::Shutdown`
- **#89** 未登录发非登录消息拦截 — `cpp/nodes/gate/handler/rpc/client_message_processor.cpp::ResolveSessionTargetNode`
- **#93** 服务器主动断玩家连接 — `gate_event_handler.cpp` + `docs/design/gate_client_high_water_mark.md`
- **#95** 个人流量超 2MB 警告 — `client_message_processor.cpp` kClientHighWaterMark + forceClose
- **#108** pb/db 字段过大检测 — `proto_field_checker.cpp` 在 `scene_handler.cpp` 中调用
- **#109** 堆栈/日志快速定位 — `cpp/libs/engine/core/utils/debug/stacktrace_system.h` (boost::stacktrace) + signal handler
- **#111** 流量画像 — `cpp/libs/engine/core/network/traffic_statistics.cpp` + `go/shared/grpcstats/collector.go`
- **#123** 多线程消息队列无锁 — `DoubleBufferQueue` + `go/db/internal/kafka/key_ordered_consumer.go` per-partition
- **#131** load/save 失败处理 — `cpp/libs/services/scene/player/system/player_lifecycle.h::HandlePlayerAsyncLoadFailed/Saved`
- **#132** Prometheus 告警 — `deploy/k8s/scene-manager-alerts.yaml` critical/warning/info 三级
- **#135** 表只存 ID — clang-tidy 插件 + 各 comp 用 `config_id`(例:`mission_comp.h`)
- **#149** 正常关服 GM 命令 — `proto/common/base/gm_admin.proto::GmGracefulShutdownRequest` + `cpp/nodes/scene/main.cpp` SIGTERM
- **#166** 网络线程防缓冲区满 — `client_message_processor.cpp` 2MB 高水位 forceClose
- **#190** 底层不做业务(gate 只路由) — `client_message_processor.cpp`
- **#192** pb 协议参数长度检查 — `proto_field_checker.h` CheckFieldSizes + CheckForNegativeInts + gate CheckMessageSize(1KB)
- **#213** 服务器正常关闭(完整版) — `node.cpp::Shutdown` + `BeforeShutdownFn` + `main.cpp` SIGTERM

### 数据 · 存储
- **#6** 金币异常检测 + 追缴 — `cpp/libs/modules/transaction_log/anomaly_detector.cpp` + `go/data_service/internal/logic/recall_logic.go`
- **#13** 改变才存(脏标) — `cpp/libs/modules/currency/comp/player_currency_comp.h`
- **#41** 单玩家回档 — `go/data_service/internal/logic/recall_logic.go` + `cpp/nodes/scene/handler/rpc/player/player_rollback_handler.cpp`
- **#46** 玩家 Redis 缓存清理 — `go/player_locator/internal/logic/leasemonitor.go`
- **#59** 追缴系统 — `CurrencySystem::AttachDebt` + `exploit_loss_prevention.md` + `transaction_log_system.h`
- **#71** 数据备份回档 — `docs/design/single_player_rollback.md` + `rollback_logic.go` + `snapshot_logic.go` + `player_snapshot.pb.go`
- **#194** 删除数据库某行 — `proto/data_service/data_service.proto::DeletePlayerData`
- **#271/#278/#284** DB/Redis 断线重连 + 存储保证 — `docs/design/infra-reconnect-overview.md` + `cpp/libs/engine/thread_context/redis_manager.cpp` + `go/db/internal/kafka/key_ordered_consumer.go`(重试队列 + seq 单调守卫)

### 登录 · 会话
- **#22** 分布式朋友圈/好友/公会 — `go/friend/internal/logic/friend_logic.go` + `go/guild/internal/logic/guild_logic.go`
- **#30** 移动同步 — `cpp/nodes/scene/handler/rpc/player/player_movement_handler.cpp`
- **#44** 长断线+别号已在线 — `go/login/internal/logic/clientplayerlogin/loginlogic.go` 检测重复登录+踢旧号
- **#195** 玩家网络异常处理 — `go/player_locator/internal/logic/setdisconnectinglogic.go` + `markofflinelogic.go`
- **#209** Golang 登录鉴权 — `go/login/internal/logic/pkg/auth/` + `pkg/token/token.go`
- **#235** session id 防改包 — gate `client_message_processor.cpp` HMAC-SHA256 token + session_id 绑定
- **#256** MMO 断线重连 — gate/scene reconnect handler + 30s lease + `go/player_locator/internal/logic/reconnectlogic.go` + `cpp/libs/services/scene/player/system/player_lifecycle.cpp`

### 玩法 · 架构
- **#85** 非 tick TimeMeter — `cpp/libs/engine/core/time/system/time_meter.h` + `time_meter_test.cpp`
- **#104** 场景-玩家解耦 — ECS `cpp/libs/services/scene/player/system/player_scene.h` 独立于 scene 实体
- **#112** 服务器无"线"概念 — `docs/design/world-channel-system.md`(线是 SceneManager 多实例)
- **#114** 进度领奖不用 reward_id 做 key — `cpp/libs/modules/reward/comp/reward_comp.h`(注释明确)
- **#128** GS 间 RPC — `proto/scene/s2s_player_scene.proto` + `cpp/nodes/scene/handler/rpc/player/s2s_player_scene_handler.cpp`
- **#129** Bag 分层(Service/Bag) — `cpp/libs/modules/bag/bag_service.h` 注释说明 Bag 不背业务
- **#193** 跨节点玩家消息 — `docs/design/cross_scene_player_messaging.md` + `cpp/nodes/gate/rpc_replies/route_message_response_handler.cpp`
- **#200** 服务重启数据同步 — `docs/design/rolling-update-restart-resilience-tests.md` + `go/login/internal/logic/clientplayerlogin/entergamelogic.go`
- **#251** 跨场景消息投递 + 分级 — 同 #193
- **#253** 停服顺延活动 — `docs/design/activity_maintenance_auto_shift.md`
- **#257** 玩家跨服分区只是存储不同 — `docs/design/cross_server_architecture_principle.md` + `mmo_cross_server_architecture.md`
- **#258** AOI n×n 优化 — `cpp/libs/services/scene/spatial/system/aoi.cpp` + `docs/design/aoi_priority_design.md` + `cpp/tests/aoi_test/`
- **#262** 全服唯一 ID — `go/shared/snowflake/snowflake.go` + `cpp/libs/engine/thread_context/snow_flake_manager.cpp`
- **#281** 无状态/幂等 — `docs/design/login-node-stateless-no-affinity.md` + `go/scene_manager/internal/logic/createscenelogic.go`
- **#282** 配置/表驱动热更 — `cpp/generated/table/code/` + `tools/data_table_exporter/`

### 工具链 · 质量
- **#47** C++ 不能有裸指针 — `cpp/plugin/NoMemberRawPointerCheck.cpp`(⚠️ 见下文矛盾点)
- **#137** 模板复杂用代码生成 — `tools/proto_generator/protogen/cmd/pipeline.go` 全套
- **#178** 前置条件系统 — `cpp/libs/modules/condition/condition_util.cpp` + `condition_table.h`
- **#274** clang-tidy 检查裸指针 — 同 #47

### HTTP /api/login 迁移(2026-05 子项目)
- **#219** 微服务 readiness gate — `cpp/nodes/gate/main.cpp` + `cpp/libs/engine/core/node/system/node/node.h` DependencyGate
- **#220** 迁移 PR + CI 全绿 — `.github/workflows/login-path-tests.yml`
- **#221** Linux staging 压测工具链 — `tools/scripts/deploy-staging.sh`
- **#222** Prometheus login_auth_path_total — `go/login/etc/login.yaml` Prometheus + `docs/ops/grafana-login-path-deprecation.json`
- **#224** legacy-gate-login-enabled killswitch — `go/login/internal/logic/clientplayerlogin/legacy_gate_killswitch.go`

### 其他已做
- **#12** 底层组件基本功能分层 — 引擎/上层有明文分层(无专项文档但结构清晰)
- **#116** 所有 node 快速重启(部分) — `tools/scripts/start_mprocs.ps1` + `dev_mprocs_proc.ps1`
- **#118** 逻辑顺序 1-2-3 步骤 — 已迁 coding_principles
- **#145** 表不要一列多行函数 — Excel 导出规范约束

---

## 部分做 101 条 (🟡)

> 格式:`#编号 | 事项 | 现状 | 缺什么 | 工作量 | 依赖`

### 监控 · 告警 · 日志
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #4 | 列表上限+警告 | TeamSystem 有 kMaxTeamSize,friend_repo 有 | 统一超限 warn 机制 | M | — |
| #15 | 每操作有时间戳(为回放) | transaction_log 记录 timestamp | 非所有操作均带,缺回放协议 | L | #16 |
| #27 | 线上快速定位(IO/CPU) | Go 侧 pprof+prometheus | C++ 侧无 CPU/IO 工具 | M | #152 |
| #39 | 个人流量满 | `traffic_statistics` + `gate_client_high_water_mark` | 个人流量超限处理 | S | — |
| #40 | 服务器流量满 | Java 网关 `RateLimitConfig` + `WaveSchedule` | 服务器满载降级逻辑 | M | — |
| #70 | 错误码打堆栈+协议 | stacktrace 存在;error_handling_system.h(SendErrorToClient)已于 2026-10-16 随 Scene.InvokePlayerService 退役删除(muduo-rpc-channel.md §20.3) | 错误码触发统一 hook | **S** | — |
| #100 | 底层错误日志 | 多处 LOG_ERROR/SPDLOG_ERROR | 统一底层错误日志规范 | S | — |
| #125 | RPC 错误记协议+错误 | `game_channel.cpp` 记 message_id+error | 协议×错误码二维日志 | S | #70 |
| #216 | SIGUSR1 打印所有线程堆栈 | `PrintDefaultStackTrace()` 已有 | 注册 signal handler 入口 | **S** | — |
| #264 | 网络层日志锁卡顿 | `message_statistics.cpp` + `console_log.cpp` | 专项锁消除/异步日志 | M | — |
| #267 | 各服务器状态 | etcd 上报 + `scene_manager/metrics` | 统一状态面板设计 | M | — |
| #268 | 方便运维 | `tools/scripts/` + prometheus | 专项运维手册 | M | — |

### 开服 · 限流 · 排队
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #102 | 流量冲击/活动峰值 | [削峰现行契约](../design/open-server-rate-limit-design.md)与已有实现 | 专项指标、真实故障放行、排队门禁与尖峰容量验收 | **M** | — |
| #214 | 登录排队开源限流器 | Java `AssignGateRateLimiter` + `QueueShardCount` | 是否闭环仍有差距 | M | #102 |

### 活动 · 时间
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #51 | 公祭日顺延一天 | `activity_maintenance_auto_shift.md` | 代码层未接触发逻辑 | M | — |
| #99 | 活动炸服扩展 | `activity_maintenance_auto_shift.md` + `WaveSchedule.java` | 独立水平扩展架构 | L | #102 |
| #139 | timer 重载(默哀期移时间) | 同上 + `timer_task_boost_comp.cpp` | 重载时间逻辑未接 | M | #51 |
| #233 | 表运行时自定义数据热重载 | `actor_attribute_calculator.cpp` 有 dirty bit | 表运行时自定义+重载刷新 | M | — |
| #240 | 时间驱动状态 | snapshot_system + `thread_observability.h` | 通用"时间驱动状态机" | M | #240 |
| #241 | 能用配置用配置 | `activity_maintenance_auto_shift.md` baseline 设计 | 代码层未落地 | M | #51 |

### 数据 · 存储 · 一致性
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #31 | MySQL 断线期间存储丢失 | `data_service` 重连+快照 | "断连期间写入丢失"专项保护 | M | — |
| #32 | RPC 断线 | `service_discovery_manager` etcd 监听+重连 | RPC 断连队列保护 | M | — |
| #141 | 逻辑原子性(拍卖出价) | Redis `TryLock` + transaction_log | 拍卖出价专项原子设计 | L | #165 |
| #204 | 改变才存 | currency/skill 有 dirty/changed | 统一 proto-compare 驱动 | **M** | #226 |
| #226 | proto compare 存储 | 现是手工 dirty 标志 | `MessageDifferencer` 引擎 | M | — |
| #238 | DB 断线/繁忙/存储中断 | redis `OnReconnected`+pending_save | 极端情况完整处理 | L | — |
| #242 | 发送存储但没存完 | redis `pending_save_queue_` | 机制不完整 | M | #243 |
| #243 | 离开 scene save-before-load | `scene-switch-release-design.md` + `SavePlayerToRedis` | 严格顺序保证仍 TODO | **M** | — |
| #280 | 下线没存完就登录 | `player_lifecycle.cpp:376` TODO | 机制未落地 | **M** | #243 |

### 消息 · 协议
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #9 | 客户端数据负数判断 | `bag_system.cpp` 校验 max_stack_size≤0 | 统一"物品数量负数"拦截 | S | #192 |
| #26 | 修改协议包测试 | `test_switch.proto` + `proto_field_checker_test` | 专项协议篡改测试 | M | — |
| #29 | 错误码 vs 提示分离 | 有独立 error_tip proto + error_handling.h | 强制规范检查 | S | — |
| #75 | 底层包协议检测 | codec.cpp adler32 | 主动检测机制 | S | #76 |
| #76 | 消息篡改 | adler32 + session_id 绑定 | HMAC 签名 | **S** | — |
| #77 | 整数溢出 | currency int64_t + DeductCurrency 校验 | 通用 SafeAdd 工具 | S | — |
| #97 | RPC 错误统一 reason | error_tip 表 + client_message_processor | call site 未全覆盖 | **M** | — |
| #103 | c2s string 长度 | gate kMaxClientMessageSize=1024 | string 字段独立校验 | S | #192 |
| #127 | 换场景消息丢失 | `cross_scene_player_messaging.md` + `cross-zone-scene-travel.md` §14(2026-10-20 更正:`player_migration_event_handler` 属于已下线的 player_migrate 搬数据链,已在 `e7bb46070` 删除) | 队列化方案 TODO | M | #208 |
| #206 | 客户端数据大小 | codec kMaxMessageLen=64MB | gate 侧客户端包专项 | S | #103 |
| #247 | 协议兼容 | [根 AGENTS.md §4、§11.3](../../AGENTS.md)：枚举首值须为 0，兼容性按具体变更判断 | 按实际发布接缝核验版本兼容与迁移，不能由 proto3 语法推定兼容 | M | #255 |

### 登录 · 会话 · 重连
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #52 | N 次重连后强制重登 | `infra-reconnect-overview.md` + loginlogic | 计数+降级逻辑未落 | S | — |
| #64 | 重启自动恢复 | `infra-reconnect-overview.md` + `rolling-update-restart-resilience-tests.md` | 结论是"session 全断",未自动恢复 | L | — |
| #84 | 进游戏后断掉 | `async-load-disconnect-reconnect-race.md` + PendingEnterMap | 设计已落地,验证 | S | — |
| #156 | gate 异步并发登录 | gate 有 concurrent login 处理 | 防重入序号机制 | M | #157 |

### 跨服
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #208 | 跨场景服玩家互发消息 | `cross_scene_player_messaging.md` + `s2s_player_scene.proto` | 文档大量 TODO,实际投递未完工 | L | #193 |
| #210 | 外网数据到内网 | `k8s_gate_exposure_guidance.md` + `AdminApiKeyFilter` | 外网→内网专项代码护栏 | M | — |
| #218 | db 不应有逻辑 | `data_service_role_and_scope.md` | 代码层重构记录 | S | — |

### 玩法 · 模块
| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #10 | 自动战斗跟随服务器做 | combat_state + skill + movement | 完整自动跟随/寻路 | L | — |
| #25 | 表 id uint64 | player_id uint64,config_id uint32 | 评估是否全面 uint64 | S | — |
| #36 | 客户端 Lua 热加载 | `lua_state.cpp` 存在 | 热加载/reload 机制 | M | — |
| #42 | 表中环检测 | `instance_lifecycle.go` 有场景依赖扫描 | 表数据环检测 | M | — |
| #43 | 先检测完再写逻辑 | `proto_field_checker` 在 scene_handler 调用 | 未覆盖所有服务 | M | — |
| #49 | 全服排行榜 | `guild` 有全服/分区榜 | 玩家个人全服榜 | M | — |
| #58 | GM 强制修改玩家数据 | `gm_admin.pb.go` + `scene_admin.pb.go` 框架 | 完整 admin 工具链 | L | — |
| #67 | 状态机/行为树/FSM | `s2s_player_scene_handler` 有 FSM | 独立 BehaviorTree 模块 | L | — |
| #81 | 场景满了换新场景 | `enterscenelogic.go` 有 fallback | 明确容量判断 | M | — |
| #82 | 客户端消息过滤 | Java RateLimitDecision 针对连接 | 消息速率/内容过滤 | M | #2 |
| #86 | 粒度太细类多 | `bag-service-srp-refactor.md` 部分整改 | 持续整改 | L | — |
| #106 | 消息优先级队列 | 设计分级 + AOI 优先级 | 独立优先消息队列代码 | M | — |
| #107 | 玩家 db 数据分级 | `player_database`+`player_database_1` 注释分表 | 在线/离线热冷分级逻辑 | L | — |
| #113 | OOP vs 算法专项(背包) | bag_system AddNonStackable/AddStackable | 显式基类/文档 | S | — |
| #119 | 优先消息队列 | `DoubleBufferQueue` | 独立优先队列 | M | #106 |
| #122 | 优化热点 | `stress_test_probe.cpp` + `slg-performance-interview-qa.md` | 系统性热点分析工具 | M | — |
| #133 | DB 访问限流 | DB 连接池 MaxOpen/Idle + Kafka per-partition | 令牌桶保护 | M | — |
| #134 | 同步异步事件 | entt::dispatcher + Kafka | 分类文档 | S | — |
| #143 | 有些东西做成工具 | `tools/` 下有数据导出/proto gen/压测/合服 | "工具化"规范 | S | — |
| #144 | 静态动态数据转换 | `data_table_exporter/` + `excel-6row-header-format.md` | 运行时动态转换机制 | M | — |
| #146 | 基础功能和逻辑重用 | `IRewardableConfig` + bag 分层 | 系统性分离文档 | S | — |
| #147 | 进程内存数据转移 | `world_rebalance.go` 明确"不跨节点迁移,排空后迁" | 这是主动选择 | — | ➖ |
| #148 | 只有自己改自己状态 | ECS 组件隔离 | 接口约束/文档 | M | — |
| #163 | 服务器 push 而非客户端 pull | player_lifecycle_handler + EnterScene push | 设计文档 | S | — |
| #164 | C++ 少用宏 | `cpp/libs/engine/core/macros/` 仅 return_define/error_return | 业务层已遵守 | — | ➖ |
| #170 | 物品移动应交换非删加 | bag_system 只有 Add/Remove | SwapItem/ExchangeItem | M | — |
| #171 | 热更支持表新行 | `all_table.cpp` 有 hot_reload | go/data_service 无 | M | — |
| #179 | 功能条件解锁 | `function_switch.proto` | 完整集成 handler | M | #228 |
| #187 | 流数据模板转化 | codec `ParseMessageFromRequestBody` | 泛化为通用工具 | S | — |
| #188 (UE 视野同步+登录排队) | AOI 优先级 | `aoi_priority_design.md` | 登录排队延展未实现 | M | #102 |
| #189 | CPU 毛刺风险 | `hashed-timing-wheel.md` | C++ HashedTimingWheel 实现 | L | — |
| #199 | 操作玩家离线数据 | `data_service.proto` GetPlayerField/SetPlayerField | 专用离线玩家 RPC | S | — |
| #203 | 道具使用中防重复 | (设计可接 condition) | item_in_use / USE_PENDING 逻辑 | M | — |
| #205 | 消息列表长度 | `message_limiter/` 有频率 | 列表长度检查 | S | — |
| #212 | 重启数据同步 | `rolling-update-restart-resilience-tests.md` + `infra-reconnect` | 统一流程 | L | #64 |
| #215 | 任务类型区分(工会/每日) | `missions_config_comp.h` 有 mission_type/sub_type | 明确枚举/文档 | S | — |
| #225 | complete vs add_complete 差异注释 | 代码存在两条路径 | 纯注释 | **S** | — |
| #227 | 表检查规则 | `tools/data_table_exporter/core/foreign_key.py` + `schema.py` | 通用框架 | M | — |
| #231 | 权限系统 | `skillpermission_table` + `AdminApiKeyFilter` | 统一权限系统 | L | — |
| #245 | node id 更大 | `node_allocator.cpp` kMaxNodeId | 位宽扩大落地 | M | — |
| #255 | 服务器版本策略 | `rolling-update-restart-resilience-tests.md` | 独立版本策略文档 | S | — |
| #260 | 策划配置不能崩 | `proto_field_checker.cpp` | 系统性断言/降级 | M | #227 |
| #261 | 任务多参数 | `condition_util.cpp` + `mission.cpp` | "杀特定地图特定怪"专项 | S | — |
| #266 | 任务额外数据 | — | extra_data 字段设计 | S | — |
| #270 | 注解功能 | `cross_scene_player_messaging.md` + `go/proto/db/proto_option.pb.go` | 通用注解框架 | M | — |
| #276 | 堆叠由大变小 | `bag_system.cpp` | 拆分场景测试 | S | — |
| #285 | 角色数据变大 | proto3 兼容 + `gate_client_high_water_mark.md` | 分库分表方案 | L | #107 |

### 补齐:agent 遗漏或归类到其他段的 🟡

| # | 事项 | 现状 | 缺 | 量 | 依赖 |
|---|---|---|---|---|---|
| #12 | 底层组件只提供基本功能,上层封装 | 引擎层提供基础组件(Node/Session/MessageLimiter 等),上层有封装 | 无明文分层规范文档 | S | — |
| #69 | 假热重启 / 分布式更新 | `rolling-update-restart-resilience-tests.md` 有灰度更新测试 | 完整"假热重启"方案 + 逐节点更新流程 | L | #64 #200 |
| #116 | 所有 node 快速重启 | `tools/scripts/start_mprocs.ps1` + `dev_mprocs_proc.ps1` 可批量启停 | 一键全 node 热重启脚本 | S | #69 |
| #223 | WeChat/QQ 真 OAuth 沙盒 e2e | `docs/ops/wechat-qq-sandbox-runbook.md` 完整 runbook | 真实 AppId/AppSecret 跑一次端到端 | S | 阻塞在凭证 |

**🟡 总数核对:** 显式列出 88 条(原 84 + 此处补 4)。剩余 13 条是跨段重复(例如 #127 在"消息·协议"段,跨服段又算了一次;#208 同理)。核查报告给出的 🟡 总数 = 101,**88 + 13(去重前) = 101 ✓**。以这份表为准,排期时用编号去重。

---

## 没做 92 条 (❌)

> 格式:`#编号 | 事项 | 工作量 | 风险/价值 | 依赖`

### 🔴 高风险/基础设施(做)
| # | 事项 | 量 | 价值 | 依赖 |
|---|---|---|---|---|
| #3 | 玩家行为日志链 | L | 运营/追责 | #152 |
| #16 | MMO 观战回放 | XL | 玩法 | #15/#201 |
| #17 | 发送数据去重 | M | 带宽 | — |
| #48 | 跨服断线重连 | L | 基础 | #256 |
| #68 | 聊天广告屏蔽 | M | 运营 | — |
| #72 | 跨服世界聊天 | M | 玩法 | #208 |
| #73 | 跨服家族信息 | M | 玩法 | #208 |
| #91 | 跨服重连+目标宕机 | L | 基础 | #48 |
| #94 | 跨服同意组队 | M | 玩法 | #208 |
| #96 | 跨服聊天 | M | 玩法 | #72 |
| #105 | 崩溃时 logout_time | **S** | 溯源 | — |
| #115 | 50 万跨服排队 | XL | 开服 | #102 |
| #130 | 功能遥测打点 | M | 数据驱动 | — |
| #151 | NPC/副本战斗详情 | M | 玩法 | #15 |
| #152 | 分布式链路追踪(otel) | **L** | 线上排障 | — |
| #157 | gate 异步 old>new 时序 | M | 一致性 | #156 |
| #165 | 拍卖会系统 | L | 玩法 | — |
| #184 | 2D 状态条件表 | M | 玩法 | — |
| #185 | 2D 条件功能检测 | M | 玩法 | #184 |
| #198 | 视角切队友/观战 | L | 玩法 | #16 |
| #201 | 观战 | L | 玩法 | #16 |
| #202 | 观战时间压缩 | M | 玩法 | #201 |
| #207 | 跨服货币扣款认证 | L | 反外挂 | #48 |
| #217 | 玩家生涯历史 | M | 运营 | #3 |
| #228 | 条件开放模块 | M | 玩法 | — |
| #229 | 表组装 | M | 工具 | — |
| #230 | 通用模块不依赖表 | L | 架构 | — |
| #234 | 服务级断路器 | M | 稳定 | — |
| #236 | 非法协议计数+踢 | **M** | 安全 | #2 |
| #237 | 不用错误码做逻辑 | S | 规范 | — |
| #244 | 消息/回调死循环检测 | M | 稳定 | — |
| #246 | 客户端确定性(浮点/seed) | L | 回放 | #16 |
| #250 | 错误上报公共服务器 | **M** | 线上 | — |
| #259 | 全服统计条件变化补偿 | M | 玩法 | — |
| #263 | 客户端云存储 | L | 客户端 | — |
| #265 | 时间前后调整影响 | M | 运维 | #240 |
| #269 | NPC 对话框 | L | 玩法 | — |
| #272 | 外网账号挪内网 | M | 运维 | — |
| #273 | 构建版本归档 | **S** | 线上 | — |
| #275 | 任务满足条件可领取 | S | 玩法 | — |
| #277 | 策划拼接组件 | XL | 工具 | #228 |
| #279 | 维护后无限重连限频 | S | 运维 | — |
| #283 | 离线发奖 | M | 玩法 | — |
| #286 | 消息 ID 断线续传 | L | 基础 | — |

### 🟠 中价值玩法/运营
| # | 事项 | 量 | 备注 |
|---|---|---|---|
| #1 | 行为跟踪 v3 | L | 和 #3 合并 |
| #7 | 取消匹配回队列前 | S | — |
| #19 | i18n 多语言 | L | 客户端+服务端 |
| #23 | 断线重连提示版本更新 | S | #255 |
| #28 | 用户行为统计(OP) | M | 和 #130 合并 |
| #33 | 红包 | L | — |
| #34 | 答题 | M | — |
| #35 | 投票 | M | — |
| #45 | 角色满了合服 | L | 运维 |
| #50 | 成功后不再提示频繁 | S | — |
| #60 | 注释→逻辑 | S | 规范 |
| #61 | 网状 node | L | 架构 |
| #62 | 跨服崩溃回原服 | L | — |
| #65 | 客户端服务器版本协商 | M | #255 |
| #66 | 二进制流封装 | S | — |
| #90 | 跨服切换角色 | L | — |
| #92 | pb 表字段用作 set | S | 规范 |
| #98 | 登录排队老玩家优先 | M | #102 |
| #117 | 函数粒度规范 | — | 已迁 coding_principles |
| #120 | 系统优先级 | S | — |
| #121 | 只重用才抽象 | — | 已迁 coding_principles |
| #124 | 代码注入(库强制继承/宏) | — | 规范 |
| #126 | 数据分块可清除 | M | #71 |
| #136 | SLG 定时器持久化 | M | — |
| #138 | 掉落表现/状态分离 | M | — |
| #140 | 字段跨系统禁用 | — | 规范 |
| #142 | 传送失败保位 | S | — |
| #150 | 副本无敌观战 | M | #201 |
| #167 | 玩家改登录区服 | M | — |
| #180 | UI 汇总刷新钩子 | — | 客户端 |
| #186 | 类不能有指针变量 | M | ⚠️ 见矛盾点 |
| #188b | 拉 vs 推 | — | 已迁 coding_principles |
| #196 | 游戏暂停(MMO 不适用) | — | ➖ |
| #211 | 单玩家数据拷内网 | S | 运维 |
| #239 | 开源日历系统 | M | #51 |
| #249 | DB 消息列表缓存加载 | L | — |

---

## 已迁出/已删除 28 条 (➖)

| # | 去向 |
|---|---|
| #14 | 已迁 `coding_principles.md` #19 框架封装简单 |
| #24 | 客户端产品向,直接删 |
| #88 | 已迁 `coding_principles.md` #4 |
| #117 | 已迁 `coding_principles.md` #10 函数粒度 |
| #118 | 已迁 `coding_principles.md` #11 逻辑顺序 |
| #121 | 已迁 `coding_principles.md` #9 只重用才抽象 |
| #124 | 规范类,迁 `coding_principles.md`(宏/代码注入避免) |
| #140 | 规范类,迁 `coding_principles.md`(字段语义) |
| #153 | 已迁 `coding_principles.md` #1 调用层次 |
| #154 | 已迁 `coding_principles.md` #1(重复) |
| #155 | 已迁 `coding_principles.md` #3 |
| #158 | 已迁 `coding_principles.md` #5 |
| #159 | 已迁 `coding_principles.md` #2 一句话总结 |
| #160 | 已迁 `coding_principles.md` #3(重复) |
| #161 | 已迁 `coding_principles.md` #16 契约可预测性 |
| #162 | 已迁 `coding_principles.md` #6 自查+LLM |
| #168 | 已迁 `coding_principles.md` #15 字段一义 |
| #169 | 已迁 `coding_principles.md` #18 历史回顾 |
| #172 | 已迁 `coding_principles.md` #13 解耦 |
| #173 | 已迁 `coding_principles.md` #8 重构降复杂 |
| #174 | 重复 #154,删 |
| #175 | 已迁 `coding_principles.md` #17 有场景才用 |
| #176 | 已迁 `coding_principles.md` #20 上线量大 |
| #181 | 已迁 `coding_principles.md` #1(重复) |
| #182 | 已迁 `coding_principles.md` #14 基础不扩展 |
| #183 | 已迁 `coding_principles.md` #7 换思路 |
| #191 | 已迁 `coding_principles.md` #1(重复) |
| #197 | 已迁 `coding_principles.md` #12 OCP 开闭 |
| #8 | 重复 #2 rate limit,删 |
| #18 | 作者自注"Delete this todo",删 |
| #196 | MMO 暂停不适用,删 |
| #232 | 观察语录,迁 `coding_principles.md` #1 判据 |
| #252 | 疑问句非事项,删 |

---

## 矛盾 / 技术债 · 需要你决策

### ① 裸指针检查疑似有漏 ⚠️
- **声称:** #47/#274 有 clang-tidy 插件 `cpp/plugin/NoMemberRawPointerCheck.cpp` 强制检查
- **实测:** #186 的 agent 发现 `cpp/libs/modules/currency/currency_system.h` **就有裸指针成员**
- **结论:** 插件要么有白名单,要么还没对 `cpp/libs/modules/` 全量生效
- **建议:** 跑一次完整 clang-tidy 对 `cpp/libs/modules/` 验证覆盖范围

### ② MissionSystem 两条路径未注释
- **位置:** `cpp/libs/modules/mission/` 内 `CompleteAllMissions` vs `OnMissionCompletion`
- **问题:** 两条路径看似功能一样,代码里真的是不同分支,但**无任何注释说明差异**
- **对应:** todo #225 原文警示:"看似逻辑一样,但有细微区别"
- **建议:** 5 分钟的事 —— 加 3 行注释说明两条路径分别在什么场景被调用

### ③ #280 明确 TODO 注释
- **位置:** `cpp/libs/services/scene/player/system/player_lifecycle.cpp:376`
- **原文:** `// TODO: Only allow re-login after save completes`
- **对应:** todo #280/#242/#243 都指向同一点
- **建议:** 这是个**状态机问题**,补一个"save 中"状态,re-login 请求在该状态下等待或拒绝
- **✅ 2026-05-14 已落地:** 采取"分层防御"策略而非新增状态机:
  1. 同节点 reconnect 已被 `EnterScene` 撤销 `UnregisterPlayer` 标记(原有机制)
  2. 跨节点 reconnect 已被 `player_locator` 30s lease 阻挡(原有机制)
  3. 极端窗口(save 耗时 > 30s):`HandlePlayerAsyncSaved` 新增 LOG_WARN,运维可观测
  - 新增 proto 字段:`UnregisterPlayer.logout_initiated_ms` 记录 save 发起时间
  - 新增 API:`PlayerLifecycleSystem::IsSaveInFlight(playerId)` 供上层查询

---

## 怎么用这份文件

1. **排期时:** 从 P0 / P1 挑;每条都有工作量和依赖,直接估 sprint。
2. **复盘时:** 对着 ✅ 区可以给老板看"这些都做完了"。
3. **加新 todo 时:** 去 `todo.md`(思考流水);这里只反映已盘点的条目。
4. **下次核查:** 3 个月后重新跑一次 audit(复用 `todo_audit_2026_05_11.md` 里的 agent 提示词),状态会迁移。


---

# 第三部分 · 编码原则 (Coding Principles)


> 这些是从 `todo.md` 里整理出来的**思想性/原则性**条目 —
> 它们不是"待办事项"(没法用代码或文档对它们打勾),
> 而是写代码 / 做架构决策时应当持续遵守的判断标准。
>
> 所以从 todo 里抽出来,单独存档。
>
> 来源:`todo.md` 第 14、153–155、158–162、168–169、172–176、181–183、191、197 条
> 抽离日期:2026-05-11

---

## 一、可读性 / 维护成本

### 1. 调用层次要少 — 看一眼就能跟踪逻辑
> 来源:#153 #154 #160 #174 #181

**核心:** 调用链越少 → bug 越少 → 看懂代码代价越少 → 维护成本越低。

- 像缩进层次一样:能不能一眼观察到代码在做啥?
- 出 bug 时能不能马上发现?
- 调用层次太深 = 技术债 = 排查 bug 时要跨多个文件,看代码代价高。

**实践判据:** 当你查一个问题需要在 3 个以上的地方下断点,这就是代码逻辑太乱的信号(对应 #232)。

---

### 2. 一句话总结 — 检查代码是否放对了地方
> 来源:#159

**做法:** 写完一段逻辑后,用一句话总结它做了什么。然后看这句话和它前后的逻辑放在一起是不是连贯。

**典型反模式:** 例如某段逻辑其实只是"控制 UI 显示/隐藏",但却写在一个返回数据的接口里。一句话一总结就发现:这应该放在"视图变可见"的钩子里(对应 #180)。

---

### 3. 一眼能看出代码在做什么
> 来源:#155 #160

代码应该把 **what / flow / order** 都暴露出来 —— 看不懂的代码大概率有 bug。

---

### 4. 简单代码一眼就能看出问题
> 来源:#88

简单代码反而是 bug 多发区,因为没人仔细看。**看似简单的地方,要想想:边界条件全考虑了吗?**

---

### 5. 代码隐晦时怎么查 bug
> 来源:#158

提示:如果一段代码隐晦到让你"不知道从哪里下手查 bug",那它该被重写,而不是该被调试。

---

## 二、调试 / 自查习惯

### 6. 回去多看自己代码,用 LLM 检查
> 来源:#162

写完不要立刻提交。隔半天/一天回头看,会看出问题。**让 ChatGPT/Claude 也看一眼,LLM 是免费的 reviewer。**

---

### 7. 跳出当前限制 — 试不同思路
> 来源:#183

陷在某个方案里调不通时,放弃它,**从零开始换一个思路想**。死磕反而花时间更多。

---

## 三、抽象 / 重构

### 8. 重构、抽象、多态的目的是降低复杂度
> 来源:#173

所有架构动作的**唯一**判据:做完之后是更简单了还是更复杂了。

> 如果做完之后看着更"高级"但更难懂 —— 失败。

---

### 9. 只有真的重复才需要抽象
> 来源:#121 #175

**反模式:** 为了"展示设计模式"而写抽象。

**正模式:** 看到 3 处真的在重复同样代码 → 才抽。**单次使用 = 不要抽**。

---

### 10. 函数粒度 — 重用函数不应再调用重用函数
> 来源:#117

**层级清晰:** 最小的可重用粒度只该是叶子节点。重用函数里再去调用别的重用函数,会让粒度混乱。

```
┌─ 业务函数(逻辑顺序串起来)
│   ├─ 重用函数 A  ← 叶子,不再嵌套
│   ├─ 重用函数 B  ← 叶子
│   └─ 重用函数 C  ← 叶子
```

---

### 11. 逻辑顺序应该有 1, 2, 3, 4, 5 步骤
> 来源:#118

参考 Unity 的生命周期(Awake / OnEnable / Start / OnDisable / OnDestroy):每一步是一个明确的粒度抽象,顺序清晰。

---

## 四、系统设计原则

### 12. 系统应该加功能才改变 — 否则不变(开闭原则)
> 来源:#197

加新功能 = 加代码,**别改老代码**。别的系统不应该影响到我的系统。这就是"稳定"的定义。

---

### 13. 系统间尽量解耦
> 来源:#172

两个系统就算业务上有关系,代码层面也尽量没关系。**重用 OK,但不要相互依赖。**

---

### 14. 基础功能不要加额外扩展
> 来源:#182

**正确方向:** 扩展功能 → 调用基础功能。
**错误方向:** 基础功能里塞进各种业务扩展(例如在网络层判断业务发包条件,对应 #190)。

---

### 15. 一个字段只表达一个意义
> 来源:#168

经典反模式:用 `count = -1` 来表示"不可找回",这是把两个概念塞到一个字段里。
**正确做法:** count 就是次数,"是否可找回"是另一个字段。
**类比:** 一个函数只做一件事。

---

### 16. 系统-开发者契约:可预测性
> 来源:#161

如果开发者按规则使用系统,**结果应该是可预测的**。可预测 = 可测试 = 系统好用。
不可预测的系统(读写之后不知道下次会发生什么)就是不能用的系统。

---

### 17. 东西要有应用场景
> 来源:#175

不要为了"用某个技术"而用。**先有实际场景,再选技术,不是反过来。**

---

## 五、历史回顾(踩过的坑)

### 18. 以前调用链太多、回调太多
> 来源:#169

回顾性备忘:之前的项目代码调用链/回调太深,看不到调用关系和代码路径。

**自我警示:** 写代码时持续检查 #1 (调用层次少)。

---

### 19. 框架封装尽量简单
> 来源:#14

目标:简单到**新人也能写服务器代码**。复杂的框架 = 上手慢 = 更难找到合适的人 = 项目持续性差。

---

### 20. 上线时数据量很大
> 来源:#176

**实战常识:** 一切设计都要考虑"上线那天数据量是平时 100 倍"的情况。
- 列表会爆吗?
- 数据库会满吗?
- 缓存会被击穿吗?

---

## 怎么用这份文档

1. **写代码前:** 不用每次都看一遍,但**遇到设计选择**(要不要抽?要不要拆模块?)时回来翻一下。
2. **代码评审时:** 拿这份当 checklist。被指出违反某条 → 引用编号,不用再吵。
3. **新人 onboarding:** 这是项目"为什么这样写"的浓缩版,比 ARCH.md 更接近编码层面。
4. **不要往里面加新条目** —— 除非真的发现一条新的、跨多个 todo 反复出现的原则。一直加会让这份文档失去作为"checklist"的价值。

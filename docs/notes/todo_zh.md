# 工程待办 / 设计备忘

1. 行为追踪 v3
2. 频繁发消息限流
3. 玩家行为审计日志 —— 每个玩家可追溯的操作链
4. 所有列表必须有上限；超过阈值时告警
5. 数据库字段过大时告警
6. 金币漏洞统计 —— 两种回滚措施：(1) 告警系统，(2) 追回系统
7. 房间匹配取消时，将原玩家放回队列头部，避免等待时间变长
8. （与 #2 重复）频繁发消息限流
9. 客户端数据不可信 —— 校验物品数量是否为负数
10. 自动战斗和跟随逻辑由服务端运行
11. 队伍系统应携带额外状态标志，避免消息需要在服务器之间转发
12. 基础组件只提供基本功能；上层封装更高级的特性 —— 逐层构建，不要把所有东西堆在一层
13. 只在角色数据实际变化时才持久化
14. 框架应足够简单，初级开发也能编写服务端代码
15. 每个操作都应有时间戳（为将来的回放/观战做准备）
16. MMO 观战和回放
17. 避免发送重复数据
18. （删除此待办）
19. 多语言分支，通过 Unity 工具实现自动切换
20. 队伍通信应通过消息传递
21. 服务器优雅关闭
22. 分布式社交功能：动态、好友、私信、聊天、社区
23. 长时间后台 → 客户端/服务端版本不一致 → 重连时提示版本更新？
24. 账号注册流程：等待完整下载后再进入游戏
25. 表 ID 能否使用 uint64？
26. 测试协议包篡改
27. 生产问题排查：如何快速定位问题（如活动循环导致 IO 打满，或 CPU 跑满无响应）
28. 用户行为分析：在测试期间追踪玩家使用了哪些功能（操作统计）
29. 错误码和面向用户的提示不能混为一谈
30. 移动同步
31. MySQL 断连：断连期间排队等待写入的数据会丢失吗？
32. RPC 连接中断处理
33. 红包功能
34. 答题 / 知识竞赛功能
35. 投票功能
36. 客户端 Lua 热更新
39. 处理单个玩家带宽打满
40. 处理全服带宽打满
41. 单人数据回滚（针对一个玩家的数据恢复）
42. 配置表循环引用检测
43. 服务端服务在执行逻辑前应校验所有前置条件
44. 长时间断线且另一个账号已在线；离线 2 天后恢复
45. 合服时角色槽位耗尽
46. 从 Redis 缓存中淘汰玩家数据
47. C++ 对象中不使用裸指针
48. 跨服重连
49. 全服（所有服务器）排行榜
50. 成功响应后不要显示"操作过于频繁"提示
51. 国家公祭日：活动日程顺延一天
52. 重连 N 次后，要求完整重新登录
53. 逻辑分层 / 关注点分离
58. 强制修改玩家数据（管理工具）
59. 漏洞资源追回系统
60. 逻辑不清晰时，先加注释，再将注释细化为代码
61. 网状拓扑节点架构
62. 跨服崩溃后，玩家未返回原服
64. 所有服务重启后应自动恢复（参考王者荣耀）
65. 客户端-服务端版本协商
66. 将第三方 API 的二进制流响应封装为带类型的反序列化，使每个消费方无需自行解析原始字节
67. 状态机 / 行为树 / FSM 模式
68. 聊天广告 / 垃圾信息过滤
69. 伪热重启：分布式滚动更新方案
70. 返回错误码时，记录完整调用栈和请求协议
71. 数据库备份和时间点恢复
72. 跨服世界聊天
73. 跨服公会/帮派信息展示
75. 底层数据包协议校验
76. 消息篡改检测
77. 负数加正数时的整数溢出
79. 误操作审计日志
80. 浮点数与定点数运算
81. 玩家上线但之前的场景已满
82. 客户端消息过滤
83. protobuf 字段大小超过阈值时告警
84. 进入游戏后立即断线
85. 非 tick 驱动的计时器（TimeMeter）
86. 粒度太细 → 调用层级太深 → 类太多
87. 同一功能维护两份状态是危险的
88. 简单的代码应该一眼看上去就明显正确
89. 玩家未认证就发送非登录消息 —— 处理随机/恶意数据包
90. 跨服状态下切换角色
91. 跨服重连时目标服务器已宕机
92. Protobuf 表字段作为集合类型
93. 服务端主动断开玩家连接
94. 跨服状态下接受队伍邀请
95. 单玩家数据包大小异常时告警
96. 跨服状态下聊天
97. 所有拒绝逻辑路径必须返回面向用户的错误信息（RPC 返回时）
98. 登录队列优先级：已有账号的玩家优先；新玩家引导至新服
99. 新活动上线常导致服务器崩溃 —— 活动设计应支持水平扩展
100. 底层错误日志基础设施
101. 检测单玩家在时间窗口内的异常数据包量
102. 流量高峰处理：活动峰值负载
103. 校验客户端到服务端消息中的字符串长度
104. 最大化场景系统与玩家系统之间的解耦
105. 服务器崩溃时玩家没有下线时间戳
106. 消息优先级队列：高负载时丢弃或延迟低优先级消息（不要求有序的），负载降低后再刷给客户端
107. 分级玩家数据（在线 vs 离线），参考海战项目
108. 检测 protobuf 和数据库字段中的超大数据
109. 快速问题定位：调用栈打印和日志
110. 不使用全局变量 —— 析构顺序不确定
111. 流量画像（例如英雄联盟一场比赛使用的带宽很少）
112. 服务端场景没有"线/频道"概念 —— 分线是客户端的显示约定
113. OOP 统一抽象 vs 算法专项优化：例如背包系统对可叠加和不可叠加物品一视同仁，导致不必要的遍历。为不可叠加路径做特殊处理以提高效率
114. 进度奖励领取状态不应以奖励 ID 为键 —— 策划更改奖励 ID 后领取数据会损坏
115. 50 万玩家同时跨服需要基于队列的准入
116. 所有节点快速重启
117. 函数应小且线性组合。可复用函数不应调用其他可复用函数 —— 那会混淆粒度。最小粒度应是复用单元
124. 代码注入：当开源库强制你继承或定义宏时，那就是注入 —— 优先使用直接调用 API
125. 每次 RPC 失败时记录完整请求协议和错误响应，便于调试
126. 数据分区：哪些数据可以清除、哪些不能（例如能否只清除活动任务数据而不影响主线任务？）
127. 场景切换期间如何处理玩家的在途消息？消息丢失风险
128. 游戏服务器之间的 RPC 调用（GS 到 GS）
129. （回顾性备注）背包和队伍系统没有将基础层和逻辑层分离
130. 功能遥测：追踪玩家实际使用了哪些功能（例如队伍跟随 —— 如果没人用就别再投入时间）
131. 优雅处理加载/保存失败
132. 告警 / 监控系统
133. 数据库访问限流 / 节流
134. 同步 vs 异步事件处理
135. 配置表只存 ID，不存指针
136. SLG 计时器重启后恢复
137. 如果模板变得太复杂，改用代码生成
138. 同一功能内若两个模块可以独立，就拆开。例如：掉落表现 vs 掉落状态——看似相关，实际可以完全解耦
139. 计时器重载（例如哀悼期间，所有计时器统一往后顺延）
140. 不要让同一个表字段在不同系统之间被共用
141. 某些逻辑操作必须是原子的。例如拍卖：当前出价 / 我的出价 / 一口价必须在同一个原子块内一起改。任何一步失败，所有数据都不能改
142. 进副本传送失败时，绝对不能把玩家留在虚空状态
143. 有些东西应该做成工具，而不是写进代码
144. 静态数据 ↔ 动态数据 的转换模式
145. 配置表应避免"一列多值"
146. 基础功能 vs 逻辑复用：把可复用逻辑抽成调用基础组件的工具函数，不要嵌进基础层里
147. 进程之间的内存数据迁移（数据未持久化到 DB）
148. 只有持有者本身才能修改自己的状态。不要让一个玩家的代码直接修改另一个玩家的状态
149. 服务器优雅关闭（扩展版）
150. 副本无敌模式：观察其他玩家的状态
151. NPC 战斗详情 / 副本战斗回放详情
152. 用于操作追踪的分布式链路追踪（UI + 后端）
153. 调用链越少 = bug 越少 = 理解越快。像缩进深度一样：能否一眼看出代码在做什么？能否立刻发现 bug？这是维护成本因子
154. 调用层次浅 → 逻辑追踪和 bug 定位更快
155. 代码应该一眼能读懂
156. Gate 异步并发登录场景
157. Gate 异步并发登录：如果旧 gate 的登出在新 gate 的登录之后才到达 centre，会不会导致登录失败？怎么处理？
158. 如何调试隐晦的代码路径
159. 用一句话总结一段代码，再看它和上下文逻辑是否吻合。如果不吻合，说明可能放错位置。例如：如果一段逻辑只控制显示/隐藏，应该挂在"视图变可见"的钩子上，而不是任意 return 路径里
160. 代码应该展示：在做什么 / 完整流程 / 执行顺序——这样问题才容易找。读不懂的代码大概率有 bug
161. 系统-开发者契约：开发者按规则用，读写结果就应该可预测。可预测保证确定性。不可预测的系统很难用
162. 反复回看自己的代码——能找出问题。借助 ChatGPT 辅助
163. 服务器应该 push 数据给客户端，而不是等客户端来拉——客户端不知道服务器啥时候完成场景切换
164. C++ 开发者应该尽量少用宏——预处理器只是文本替换，没有类型检查，宏 bug 极难追踪
165. 拍卖审核：如果所有客户端在 10:01 同时请求，瞬时负载尖峰。固定时刻的突发流量要专门设计
166. 专用网络线程接收消息：让场景 tick 在网络 I/O 进行时仍能继续，避免内核缓冲区满
167. 允许玩家更换登录区服
168. 一个字段 = 一个含义。不要在不同上下文中给同一字段赋不同语义（例如用 reward-recovery-count = -1 表示"不可找回"）。不同概念用不同字段，类似"一个函数只做一件事"
169. （回顾性备注）以前的代码调用链和回调太多——根本看不出整体流程
170. GUID 复用问题：移动一个物品 = 删除 + 新增 → 每次产生新 GUID。更好的做法是 swap 交换
171. 配置表新增行的热更支持
172. 系统之间尽量解耦——即使业务上相关，代码层面也要独立。复用是 OK 的
173. 每一次重构 / 抽象 / 多态都应该降低复杂度
174. （重复 #154）调用层次浅 → 调试更快
175. 只有真有用例时再用某种技巧——不要为了用模式而用模式
176. 上线时大数据量场景的处理
178. 前置条件 / Precondition 系统
179. 功能解锁 / 条件系统
180. 集中式刷新钩子：例如视图可见时统一刷新 UI 数据（不只是"返回"导航）。基于定时器的刷新是糟糕的（IO 打满 + 数据陈旧）
181. 技术债：调用链深 → bug 难追 → 复杂度增加 → bug 更多
182. 基础功能不能含扩展逻辑；扩展应该调用基础功能
183. 卡在当前逻辑时，退一步彻底换一个思路
184. 二维状态-条件表
185. 二维条件-功能校验表
186. 类不能有指针成员变量
187. 任何抽出来的字节流都应能通过模板直接反序列化为目标类型
187b. UE 风格的相关性同步：越近的物体同步越频繁。延展到登录排队：排在越前面，状态刷新频率越高。UE 还根据物体类型和重要性调整同步频率
188. 选方案前比较两种做法。例如迭代删除：复制后迭代 vs 收集后再删——后者更好（工作集更小）
188b. 客户端-服务器交互：拉优于推。让客户端发请求并控制时机，适应自己的运行环境。这给客户端按需取数据的灵活性
189. CPU 尖峰 / 抖动是潜在性能风险
190. 底层系统不应包含业务逻辑（例如：不要把发包条件塞进网络层）
191. 代码分层规范
192. Protobuf 字段长度校验
193. 不同 game node 上的玩家互发消息
194. 删除数据库某一行
195. 处理单玩家网络异常
196. 游戏中的暂停功能
197. 系统只在加新功能时改动；否则保持不变。其他系统不应影响我的系统
198. MMO 视角切换到队友 / 观战功能
199. 操作玩家的离线数据
200. 服务重启后的数据同步
201. 观战系统
202. 观战时间压缩 / 暂停技术
203. 道具使用中防重复点击：上一次响应未到时再次使用，提示"已在使用"
204. 脏标存储：只在数据真的变了时才持久化
205. 校验消息列表长度
206. 校验客户端发送的数据大小
207. 跨服时尝试性扣款（最终确认需跨服认证）
208. 一个游戏场景服上的玩家直接给另一个场景服上的玩家发消息
209. Golang 登录鉴权
210. 把外网数据导入内网（安全边界）
211. 把单玩家数据拷贝到内网
212. 各种服务器重启场景下的数据同步
213. 服务器优雅关闭（最终版）
214. 用开源限流器做登录排队
215. 任务类型字段必须区分：公会任务有自己的类型枚举，不和每日任务共享。每日任务由每日任务系统处理，不放进基础任务系统
216. 给服务器发信号转储所有线程堆栈 — ✅ 2026-05-12 第一版完成（仅信号处理线程）；后续工作见 #287
217. 玩家生涯：历史操作记录和事件
218. DB 层不应该有业务逻辑——只关心存储。（之前的错误：data_service 在创建角色）
~~219. 无状态微服务的就绪门~~ ✅ 已完成：DependencyGate 抽象在 node.h，Scene/Gate main.cpp 已接入
~~220. 迁移 PR + CI 全绿~~ ✅ 2026-05-10 完成：PR #11 已合并
~~221. 在真实 Linux staging 上跑 1k/2k/5k 压测档~~ ✅ 2026-05-10 工具链就绪：tools/scripts/deploy-staging.sh + 压测模板；2026-05-11 测得 gateway-only 底线
~~222. 把 login_auth_path_total 暴露给 Prometheus + Grafana~~ ✅ 2026-05-10 完成
223. WeChat / QQ provider 真实 OAuth 沙盒 e2e — 🟡 Runbook + 离线 mock 已就绪，阻塞在真 AppId/AppSecret
~~224. legacy-gate-login-enabled 特性开关~~ ✅ 2026-05-10 完成
225. CompleteAllMissions（GM 批量）vs OnMissionCompletion（正常完成）——看似相同，行为不同 — ✅ 2026-05-12：mission.cpp / mission.h 已加差异注释
280. 下线没存储完成时玩家又登录怎么办 — ✅ 2026-05-14：三层防御已落地——同节点 reconnect 已被 EnterScene 撤销 UnregisterPlayer 标记 + 跨节点 reconnect 已被 player_locator 30s lease 阻挡 + save 耗时 > 30s 的极端窗口由 HandlePlayerAsyncSaved 输出 LOG_WARN 让运维可观测。新增 proto 字段 `UnregisterPlayer.logout_initiated_ms` 记录 save 发起时间,新增 API `PlayerLifecycleSystem::IsSaveInFlight(playerId)` 供上层查询
281. P0 收尾(2026-05-14):
    - **#70/#125 错误码统一 hook** — `error_handling.h` 加 `LogErrorContextAndReturn(tag, code, request)`;`SendErrorToClient` / `game_channel.cpp::HandleResponseMessage`(RPC_ERROR 收) / `SendErrorResponse` 都接通了"错误码 + proto 类型 + 请求 ShortDebugString + 堆栈"统一日志
    - **#76 消息篡改 HMAC(第一片)** — `GateTokenPayload.hmac_session_key = 4` 字段 + Go login 端 `crypto/rand` 生成 32 字节 session key;后续工作(gate intake / wire tag / verify / metric)详见 `docs/design/hmac-message-signing.md`
    - **#97 RPC 错误统一 reason** — `return_define.h` 5 个 macros 都加了 `LOG_WARN` 带 `__FUNCTION__` / `__FILE__` / `__LINE__` / code,杜绝静默吞错
    - **#204/#226 proto-compare 驱动 dirty-save** — ✅ 2026-06-03 全套接通: 第一片 (2026-05-14) `cpp/libs/engine/core/utils/proto/proto_dirty_compare.h` 落 `dirty_save::IsEqual()` / `ShouldPersist()`;slice A/B (~2026-05-17) 落 `PlayerLastPersistedSnapshotComp` + `SavePlayerToRedis` 入口 IsEqual fast-path + `HandlePlayerAsyncSaved` 里 `snap.Replace(message)`,probe 顺序保护(R2 fix)防 STRESS_TEST_PROBE 模式静默失效;slice C (2026-06-03) 落 `dirty_save_stats.h` 两个 atomic 计数器 + RedisSystem 30s timer 输出 `[DirtySave] total=X skipped=Y skip_pct=Z%` LOG_INFO,`stress_summarize.ps1` 加 section 2bb 解析。剩 slice D 等 Round 17 跑出实测跳过率(预估 30%-70%,>30% 才算正常工作)。详见 `docs/design/proto-compare-dirty-save.md`。
286. 基于消息 ID 的断线重连续传机制。客户端与服务端为每条业务消息分配单调递增的序列号（message ID）。连接断开重连后，客户端上报本地最后已确认收到的消息 ID，服务端从该 ID 之后的消息开始补发，从而实现"断点续传"式恢复，避免重复推送也避免消息丢失。设计要点：(1) 服务端每会话维护有界环形缓冲（按消息 ID 索引），保留最近 N 条已发出但未必被客户端处理完的消息；超出窗口的消息按业务策略丢弃或降级为"全量拉取"。(2) 客户端定期或按阈值上报已处理的最大消息 ID，服务端据此推进缓冲区低水位、释放内存。(3) 重连请求中携带 `last_seen_msg_id`；服务端比对缓冲区：命中则补发增量，未命中（缓冲区已滚出）则触发全量状态同步并清空客户端缓存。(4) 即便补发与新推送在网络层短暂重叠，客户端也能凭消息 ID 去重。(5) 与现有重连窗口共存：enter_gs_type=3 (RECONNECT, 30s 窗口) 沿用不变；本机制是在重连成功后用于"消息流"层面恢复，而不是会话层面。待补充：消息 ID 命名空间（全局 vs 按会话）、缓冲区大小与内存上限的压测数据、与 Kafka `gate-{gateId}` 顺序保证的衔接。
287. #216 跟进：真正"所有线程"的堆栈 dump。2026-05-12 完成的 #216 第一版只 dump 信号处理线程的栈，加上进程线程总数。要做到名副其实的"打印所有线程堆栈"，还需要：Linux 通过 `pthread_kill(tid, SIGRTMIN+x)` 给每个线程广播一个专用实时信号，各线程在自己的 handler 里抓 `boost::stacktrace` 写到共享缓冲区，主线程收齐后串成一份报告；Windows 用 `OpenThread + SuspendThread + GetThreadContext + StackWalk64`，需要 dbghelp.lib 和符号路径，Windows 主要用于开发机，可以接受相对简陋的实现。风险：在生产挂起所有线程时间过长会让 watchdog/etcd lease 觉得节点死了——控制总耗时 < 200ms，或者在已知卡死时再触发。落地点：`cpp/libs/engine/core/utils/debug/stacktrace_system.h` 已经有第一版的 `DumpProcessStackTraceOnSignal`，扩展那个函数即可。触发 signal 仍走 SIGUSR1（Linux）/ SIGBREAK（Windows）。
288. P1 收尾(2026-05-14)——7 条 P1 一次性走完:
    - **#273 构建版本归档** ✅ 完成 — 新增 `cpp/libs/engine/core/build_info/build_info.h`(`__DATE__`/`__TIME__` + 可注入的 `BUILD_GIT_SHA`/`BUILD_BRANCH` 宏);`node.cpp::Initialize` 启动时打印 banner。
    - **#105 崩溃 logout_time** ✅ 完成 — 在 `node.cpp` 匿名命名空间加 `HandleFatalSignal` + `InstallFatalSignalHandlerOnce`,捕获 SIGSEGV/SIGABRT/SIGFPE/SIGILL,写一份带 wall-clock 毫秒 + 线程总数 + 堆栈的 dump,然后 `SIG_DFL` + `raise` 让 OS 完成 core dump。重入保护用 atomic flag。
    - **#236 非法协议计数+踢** ✅ 完成 — 新增 `cpp/libs/engine/core/message_limiter/illegal_packet_counter.h`(`RegisterAndShouldKill` / `Reset` / `ThresholdFromEnv`);`SessionInfo` 新增 `illegalPacketCount` 字段;`CheckMessageLimit` rate-limit 失败时累计,达 50(env `GATE_ILLEGAL_PACKET_THRESHOLD` 可调)后 `forceClose`。
    - **#68 聊天敏感词(第一片)** ✅ — 新增 `cpp/libs/engine/core/text/sensitive_word_filter.h`(`Load` / `Contains` / `Redact`);chat 业务链尚未存在,接入待 slice C。完整 slice 计划见 `docs/design/chat-sensitive-word-filter.md`。
    - **#250 错误聚合上报(第一片)** ✅ — 新增 `cpp/libs/engine/core/error_reporter/error_reporter.h`(`Buffer::Record/Drain/Snapshot` + 1024-event ring,drop-oldest);本地原语就绪,中央上报通道选型 / HTTP push 待 slice C。完整 slice 计划见 `docs/design/error-reporting.md`。
    - **#152 分布式链路追踪(第一片)** ✅ — 新增 `cpp/libs/engine/core/tracing/trace_context.h`(W3C 兼容的 128-bit TraceId + 64-bit SpanId + traceparent 解析/序列化 + `NewRoot`/`ChildSpan`);log 关联 / gRPC 拦截器 / Kafka header 传播 / OTel 后端选型待 slice A-F。完整 slice 计划见 `docs/design/distributed-tracing.md`。
    - **#207 跨服货币扣款认证** ✅ 设计文档 — 实现阻塞在 #48 跨服重连 + #208 跨场景消息;设计文档把两阶段提交流程、Kafka topic 布局、failure-mode 矩阵都写清楚,见 `docs/design/cross-server-currency-auth.md`。
    - **#204/#226 slice A(snapshot retention 组件)** ✅ — 新增 `cpp/libs/services/scene/player/comp/last_persisted_snapshot_comp.h`(`PlayerLastPersistedSnapshotComp { snapshot, saved_at_ms, Replace, HasSnapshot }`);SavePlayerToRedis fast-path 接入和 Kafka 路径协同待 slice B。
289. Slice A 收尾(2026-05-15)——5 个 slice 一次性推进:
    - **#250 slice A(接现有错误点位)** ✅ — `SendErrorToClient` / `game_channel.cpp::HandleResponseMessage` RPC_ERROR 分支 / `SendErrorResponse` / `client_message_processor.cpp::CheckMessageLimit`(#236 非法包计数)四处都接通了 `error_reporter::Record(code, tag, message)`。`HandleFatalSignal`(#105)**故意没接** — 致命信号路径是 async-signal-unsafe,在那里调 mutex+deque push 风险大,设计文档里写明这是 trade-off。
    - **#152 slice A(TLS trace 上下文 + 日志关联)** ✅ — 新增 `cpp/libs/engine/thread_context/trace_context_tls.{h,cpp}` 提供 `tlsTrace` TLS slot + `ScopedSpan` RAII 子 span + `TLOG_INFO/WARN/ERROR/DEBUG` 宏(在 LOG_ 输出前自动加 `[trace=xxxxxxxx]` 8 字符前缀,grep-friendly)。muduo Logging 没动。RPC entry 自动塞 traceparent / 出 traceparent 待 slice C。
    - **#204/#226 slice B(SavePlayerToRedis fast-path)** ✅ — `SavePlayerToRedis` 入口加了 `dirty_save::IsEqual(*message, *snap->snapshot)` 跳过逻辑;`HandlePlayerAsyncSaved` 成功路径里 `snap.Replace(message)` 更新快照。第一存(无 snapshot)和实体被 Destroy 的情况都正确处理。Kafka DBTask 路径同步跳过尚未做(slice C),目前 fast-path 跳过整个 SavePlayerToRedis 函数 → Kafka 也跳过 → 安全但保守。
    - **#68 slice A+B(词表 + 单例 + reload swap)** ✅ — 新增 `cpp/libs/engine/core/text/sensitive_word_filter_singleton.h`,`sensitive_word_filter::Singleton::{Init, Reload, Snapshot, Contains, Redact, Size}`。源码改成**文件路径驱动**(env `GATE_SENSITIVE_WORDS_PATH` 或 `etc/sensitive_words.txt`)而非 Excel 表 — 目的是和 #102 工作流的 generated/table dirty 文件解耦,迁移到 Excel-driven 是 loader 替换,swap 机制保持不变。Reload 用 atomic shared_ptr swap,在飞 scan 不会被打断。
    - **#76 slice A(gate intake placeholder)** ⚠️ 部分 — `SessionInfo` 加了 `std::string hmacSessionKey` 字段位置;实际从 `GateTokenPayload.hmac_session_key` 取值的代码没接 — 需要 proto codegen 重生成 `cpp/generated/proto/common/base/*.pb.h` 才能取该字段,而 codegen 会和工作树里 #102 流程的 generated/proto dirty 文件冲突。等 #102 commit 后再做,设计文档 hmac-message-signing.md 已记录这个阻塞。
290. Slice C-mini + slice D 收尾(2026-05-15):
    - **#250 slice B(Prometheus exporter)** ⚠️ 降级合并 — cpp 侧没有 prometheus 库依赖,加它是 L 级 cmake/third_party 改动(会拽到 dirty 工作树),不在本轮范围。改成"事件 + 计数同时写入 dump 文件"的 JSONL header line — 三个计数(total/dropped/current)在每次 dump 的文件头里,等以后 cpp prometheus 集成那天再读。
    - **#250 slice D(SIGUSR2 dump-to-file)** ✅ — `error_reporter.h` 加 `DumpSnapshotToFile(path)` / `DumpSnapshotToDefaultPath()`(JSONL 格式,转义 `"\bfnrt + \uXXXX` 控制字符);`node.cpp` 加 Linux 专用 `HandleErrorReporterDumpSignal` + `InstallErrorReporterDumpHandlerOnce`,SIGUSR2 触发写 `logs/error_reporter_dump_<yyyymmdd_HHMMSS>.jsonl`。Windows 无 SIGUSR2,Ctrl+Break(SIGBREAK)同时触发 stack dump + error_reporter dump(在 `HandleDiagnosticSignal` 里附带)。
    - **#152 slice C-mini(RPC entry 自动 install trace)** ✅ — `game_channel.cpp::ProcessMessage`(所有 RPC 处理器的统一入口)在最开头 `tracing::tlsTrace = tracing::NewRoot()`,这样每个 RPC 处理过程里所有 `TLOG_*` 日志都带同一个 trace_id。跨进程 trace 传播(incoming traceparent 解析 / outgoing 写入)等 slice C/D。这一步把"本进程内 trace 一致性"打通了,grep `[trace=xxxxxxxx]` 能拉出一个 RPC 链上所有日志。
291. #76 slice A 收尾(2026-05-16)——#102 已 commit,codegen 安全跑通:
    - **codegen** 已跑(`proto-gen-run`),衍生物干净:`cpp/generated/proto/common/base/message.pb.{h,cc}` 加 `GateTokenPayload::hmac_session_key()` 访问器;`go/proto/common/base/message.pb.go` 加 `HmacSessionKey []byte` 字段;同时补齐之前遗漏的 `go/proto/common/component/player_comp.pb.go` 的 `UnregisterPlayer.logout_initiated_ms`(#280 衍生)+ `generated/proto/{_unified,db,login}/` 三套镜像同步。
    - **Go 端真正赋值** ✅ — `go/login/internal/logic/pkg/loginqueue/gatetoken.go`:`crypto/rand.Read` 生成 32 字节 session key,直接赋给 `payload.HmacSessionKey`(撤掉之前的 placeholder TODO 注释)。`go vet` exit=0 验证。
    - **gate 侧真正 intake** ✅ — `cpp/nodes/gate/handler/rpc/client_message_processor.cpp::HandleClientTokenVerify` 在 expiry 检查通过、`session.verified = true` 之前:`session.hmacSessionKey.assign(payload.hmac_session_key())` + `IllegalPacketCounter::Reset(session.illegalPacketCount)`。空 key 落到空 `std::string` → gate 走 adler32-only 回退,**完全向后兼容**。
    - **message_id 138 (LoginPreGateQueryQueueStatus)** ✅ — 已在你的 `708d54507` commit 里随 #102 提交,本轮 codegen 跑完后**没有**再变动 `proto/message_id.txt`,确认无需我再做。
    - **剩余的 #76 slice B+C+D+E**(wire-format 加 `hmac_tag` 字段、客户端 SDK 配套、gate 端验签、`gate_hmac_mismatch_total` 监控)按 `docs/design/hmac-message-signing.md` 推进 — 这些每条都是独立工作,不阻塞当前服务器跑。
292. 跨服 / 回档 / 合服 现状盘点 + 差距修复方案(2026-05-17):
    - **目标**: 你说"继续做跨服回档合服",我盘点后**没有从零设计**,因为现有基础设施做得比预想完整(`tools/merge_zone/` 已 538 行,`rollback_logic.go` 已 394 行,`zone_data_rollback.md` / `single_player_rollback.md` 已写完)。我转而出"差距分析 + 修复方案"。
    - **现状盘点** ✅ `docs/design/cross-server-rollback-merge-audit.md` — 已盘清:跨服路由(Data Service `RemapHomeZoneForMerge`)、单玩家回档(`RollbackPlayer`)、整 zone 回档(`RollbackZone` 两阶段含孤儿清理)、合服工具(`tools/merge_zone/` 5 步流程 + `dev_tools.ps1 merge-zone` 入口)。**真实差距**列了 12 条按 P0/P1/P2/P3 排序。
    - **跨服回档差距修复** ✅ `docs/design/cross-server-rollback-gap-fixes.md` — 4 个真正的差距:P0-E(MySQL CronJob 备份模板,K8s YAML 草稿)、P1-B(跨服节点崩溃状态回滚 — 推荐扩展 `transaction_log` 加更多 op type + replay 机制 + 物品复制风险三条)、P2-A(跨 zone `RollbackPlayer` GM 工具壳)、P2-C/D(Kafka offset 重置 + `k8s-zone-rollback` 一键脚本)。每条带 slice 拆分。
    - **合服差距修复** ✅ `docs/design/server-merge-gap-fixes.md` — 5 个真实差距:P0-G(玩家昵称冲突 — 推荐 G2 强制改名方案,但**不能新增第 5 种 enter_gs_type**,改用 `force_rename_required` 字段 + 客户端检查)、P0-J(资源全量审计 — 邮件 / 好友 / 拍卖 / 聊天 / 公会申请等是否完整迁移)、P1-I(merge-zone-runbook + checklist)、P2-K(不一致检测脚本)、P3-H(玩家通知)。
    - **本轮不做的事**(诚实标注阻塞): K8s CronJob YAML 实文件(待 ops namespace/secret 命名约定)、`transaction_log` 扩展新 op type(待和后端策划过 op 列表)、`dev_tools.ps1` 加新命令(没法本地跑 K8s 测试)、`PlayerStatusComp` 加 `force_rename_required` 字段(等下次干净 commit 窗口跑 codegen)、客户端 UI 改动(不在我可动范围)、`audit_resources.go` 实现(需先调研 proto/ 资源清单)。
293. Java 版玩家持久收件箱 v1（GM 处置域）收尾（2026-10-19 登记；Java 仓 `D:\luyuan\xuanming-server-mmo`，全部在工作区未提交、PARITY 收件箱行（第 171 行）与本仓 PROGRESS 都还没改；依据设计 `docs/design/player-inbox-service.md` §12、交接 §6.4、PARITY 收件箱行 12 条「Java 落码须对齐」）:
    - **已核对** ✅ 契约不用再同步：game-proto 的收件箱 proto（Ack `scene_node_id = 6`、审计 10–15、FORCE_VOID、VoidMode / force_verdict）与 tip 段已与本仓工作区逐字一致（cea4d88 进库）。
    - **已写、game-inbox 模块编译通过（未写测试、未跑测试）** ✅ game-player-store：`player_state.inbox_ledger = 10` 与共享只读规则 `InboxLedgerRules`（环容量 32 唯一常量）；game-api：`PlayerInboxService` / `SceneInboxService`、group inbox / scene-inbox、`SceneNodeInfo.inbox_consumer_enabled = 11`；game-discovery：`RedisKeys.inboxMaintenanceLeader()`；新模块 game-inbox：mix64 分片、围栏（R = 玩家表 owner_epoch）、入箱（锁内判重先于满箱、ATTACH_DEBT 启动即拒）、Fetch / Ack、VoidItem 普通与 FORCE_MISMATCH 四条件证明、对账（步长 4、16 轮、截断计数、分片 tally、空洞谓词）、提醒退避（30s–30min、node_missing 1 小时落定）、Redis 领导锁、写前门禁（读线上 scene 节点的消费开关）、运维 HTTP `/admin/inbox/players/...`（独立令牌 `GAME_INBOX_ADMIN_TOKEN`）。
    - **已写、未编译** ⚠️ game-scene：`inbox/` 下 InboxRules / InboxLedger / PlayerInboxRuntime / InboxConsumers（只有封锁 / 解封执行器）/ DubboInboxTransport / InboxService（判定顺序、回包闸不过丢副本重拉、Ack InvalidParameter 停用、离场补发、停用域 STALE 不重试）/ SceneInboxProvider；`world/InboxHooks`；ScenePlayer、SceneWorld（新构造重载 + 四个挂点）、PlayerBattleService.clearFreeze、SceneNodeProperties.InboxSettings、SceneRpcServer.export 重载。
    - **剩下的**: (1) SceneNode 装配（建 InboxService、每秒 tick、导出 SceneInboxProvider、目录发布消费开关、加载时校验账本、停服关 transport）与 game-scene application.yaml 的 `inbox:` 段（consumer-enabled: false）；(2) game-data RestoreBuilder 回档保留现档 inbox_ledger（同 blocked_types）+ 用例；(3) 守卫测试登记：NamespaceLiteralGuardTest 的 CODE_IDENTITY（"game-inbox-scene"、"game-scene-inbox"）与 GOLDEN_JDBC_URLS（game-inbox），加进 start-slice 时改 LocalSliceOrderTest / SliceScriptsTest 的进程数；(4) 测试：分片金样（与 Go `TestRowShard` 同值）、围栏 / Ack / 作废 / 证明 / 注册表 / 退避单测、H2 存储用例、scene 状态机用例；(5) 文档：Java `docs/porting/data-ops-spec.md` 欠款段改走收件箱（D13）、architecture.md 模块表、PARITY 第 171 行；(6) 按 Java AGENTS `./mvnw -B clean install` 跑全量测试（本机当时没有 MySQL / Redis / Docker，真库集成没跑）。
    - 详细交接见同组会话笔记 `java-inbox-progress-20261019.md`。
294. **TODO（C++26 正式发布且 Windows/Linux 工具链齐备后）**（2026-11-21 登记）：
    - [ ] **可空借用迁移到 `std::optional<T&>`（P2988R12）**：C++23 及以前不支持引用类型的 `std::optional`，当前统一写 `std::optional<std::reference_wrapper<T>>`。待 Windows（MSVC）与 Linux（GCC / libstdc++）正式工具链均支持 C++26 `std::optional<T&>` 后，将可空借用成员/返回值逐步替换为 `std::optional<T&>`，并同步更新 `docs/rules/engineering-principles.md`、`docs/rules/cpp-runtime-and-build.md` 与 `tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 说明。
    - [ ] **`std::string + std::string_view` 直接拼接**：待双端工具链支持 C++26（P2591R5）后，简化当前为拼接临时包一层的 `std::string(...)`（背景见 `docs/rules/cpp-runtime-and-build.md`）。
    - [ ] **重写现有 C++ 协程写法并迁移到 C++26 标准 `std::execution`（P2300 / P3552 `std::execution::task` / P3149 `counting_scope`）**：待 Windows（MSVC）与 Linux（GCC / libstdc++）正式工具链齐备后，将当前基于 Boost.Cobalt + Asio 的协程底座与业务协程调用点统一按 C++26 标准写法重写：
      - 底座（`cpp/libs/engine/core/async/loop_task_scope.{h,cpp}`、`coroutine_callback.h`、`cpp/libs/services/scene/core/system/scene_task_scope.{h,cpp}`）：用 muduo `EventLoop` 自定义 Scheduler（保留 `queueInLoop` 投递与 `tracing::ScopedSpan` 恢复）取代 `SceneExecutor`/Asio `execution_context`；用 `std::execution::counting_scope`（`spawn` / `request_stop` / `close`）取代手工维护 `asio::cancellation_signal` 集合；将 `loop_async::AwaitCallback` 改为返回标准 Sender，取消走 `set_stopped` 通道，不再为表达本地停止在外层套 `std::optional<T>`。
      - 业务协程与测试（`etcd_service`、`player_team`、`player_battle`、`player_lifecycle`、`player_scene`、`player_store_client`、`role_admission_system`、`battle_node`、`scene_node_service`、`id_segment_transport` 及配套单测）：移除 `BOOST_COBALT_NO_LIB` / `boost/cobalt/*` 及每步 `co_await` 后重复的 `!result` + `this_coro::cancelled` 样板，无依赖异步步骤按需改用 `std::execution::when_all` 组合；跨 `co_await` 仍保留值语义身份复核（`valid` / `IsSamePlayer` / `try_get`）与“本地停止 vs 远端写入终态”区分。
      - 清理与文档：同步更新 `docs/rules/engineering-principles.md` §11.7，确认无残留后移除 Boost.Cobalt 编译项及 `deploy/*/Dockerfile.cpp` 镜像接线（完整工具链探针与回归清单见 `docs/design/team-system.md`「C++ 协程工具链核实与标准 task 迁移」）。
295. **TODO（新地图交付、负责人说开始后再做）客户端整图加载**（2026-11-26 登记）：
    - [ ] 客户端换场景改为目标图整张加载完再切，删掉按视野分片（九宫格）的加载。负责人 2026-11-10 定等新地图做完再改，在此之前客户端不动。
    - [ ] 方案全文在客户端仓 `Docs/QdaoMapDeliveries20261102.md` 末节「整图加载方案」（出图规格、加载流程、删除清单、预算守护、跨仓影响）。文中「9 图 15 套」等数字是 11-11 的盘点，开工时按当时的 catalog 重新核对；先确认每套外观单边 ≤4096，新主城、八仙岛、东海渔村按 sceneId 1/2/3 交付。
    - [ ] 协议不变，两版服务器只跟导航数据：本仓 `tools/navmesh_baker/sync_festival_maps.py` 换成新交接文件的 SHA 与计数，按新 walkmask 重烘 `data/scene_nav_bin/current_maps`；Java 仓按同一批 walkmask 更新 `config-data/navmesh/current_maps`。
296. **TODO（上线前，由负责人 / 法务 / 运维办）官方通行证上线前待办**（2026-11-27 登记；代码都已写完，设计见 `docs/design/official-passport-registration.md` §3.6~§3.10，部署见 `deploy/k8s/passport/README.md`）：
    - [ ] **手机回调改名（等安卓包名 / iOS Bundle ID 定）**：现在是 `xuanming://passport/callback`。RFC 8252 §7.1 要求用反向域名形式（如 `com.<公司>.<游戏>:/passport/callback`），只为不和别的 App 撞名，与安全无关。包名定了之后同批改客户端 `ClientSettings.DefaultMobilePassportRedirectUri`、`deploy/passport/init_data.json` 的 `redirectUris` 与 `tools/scripts/tests/passport_k8s_contract.tests.ps1`（设计稿 §3.8 第 3 条）。
    - [ ] **《用户协议》《隐私政策》正文与地址（法务出）**：出正式包时传给客户端仓 `tools/build_client.ps1 -TermsUrl … -PrivacyUrl …`，部署通行证时传给 `deploy/k8s/passport/render-init-data.ps1`；以后协议改版，把客户端 `PrivacyConsent.CurrentVersion` 加一（设计稿 §3.9）。
    - [ ] **域名与证书**：填 `deploy/k8s/passport/public-entry.yaml` 两个 Ingress 的 `ingressClassName`、域名与证书 Secret，`networkpolicy.yaml` 的 Ingress 控制器 namespace；客户端正式包传 `-GatewayUrl … -PassportOrigin …`。
    - [ ] **发码通道与行为验证码**：Casdoor 后台给 `app-xuanming-game` 挂邮件（或短信）发码通道，图片验证码换成行为验证码（规则保持 `Always`）；供应商密钥不进仓库。
    - [ ] **README「上线前还要人办的」其余各项**：静态资源自托管（`staticBaseUrl`）、要多副本先配 `redisEndpoint`、库备份加密（`cert` 表里有签名私钥）。
    - [ ] **预发环境走一遍**：按 README 第 7 步走注册、登录、找回密码、桌面 / 安卓 / iOS 各一次「官方账号登录」（安卓多试几台国产机与自带浏览器）、改密后被踢下线、授权地址加 `&prompt=none` 回 403；页面缺接口就补进白名单与契约测试。
    - [ ] **验证（交 Gemini）**：按 `docs/handoff/passport-hardening-handoff-20261126.md` 跑生成、编译、单测、契约测试与冒烟；红了保留首错交回 Claude。
    - [ ] **Java 版服务端**：通行证渠道、注销删通行证用户、登录 / 刷新 / 重新认证前的核对与踢线、通行证回调入口，全部只在 Java 仓 `PARITY.md`「2026-11-26 官方通行证」条目登记待做，未写代码；客户端与部署清单两版共用。

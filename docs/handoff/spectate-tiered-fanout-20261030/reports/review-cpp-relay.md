# 语义评审：观战中继 / 广播 battle → Kafka → gate 链路（2026-10-30）

范围：§15.3–§15.5 契约，`battle_spectate_relay.*`、`battle_room_manager.*`（Attach / Promote / 每回合帧 / END）、`cpp/nodes/gate/spectate/*`、`gate/main.cpp` 装配、`client_session_handler.cpp` 本地拦截与关闭钩子、`node_kafka_command_handler.h` 新选项。

口径：全部是静态阅读，未编译、未运行、未压测。严重-2 的命中率、一般-1 的触发负载都是由代码推得，没有实测。

结论：发现 7 条（严重 2，一般 5，无阻断）。帧格式、topic / 分区 / 过滤、凭证、weak 绑定都对得上；问题集中在「最后一回合与 END 的先后」：两条严重都会让观众收到 166 却看不到最后一回合的 158。

---

## 严重

### 严重-1：END 抢占没发完的最后一回合扇出，单 gate 订阅者超过 512 时必丢最后一回合

- **位置**：`cpp/nodes/gate/spectate/gate_spectate_hub.cpp:288-296`（`OnEndFrame`）、`:372-388`（`StartFanout`，`:379-382` 丢弃未发完的目标并把游标归零）。上游紧邻产出：`cpp/nodes/battle/logic/battle_room_manager.cpp:1432-1436`（普通局）、`:1966` 与 `:1997`（确认模式）。
- **触发序列**（某 gate 上该场订阅者 N > 512）：
  1. battle 在同一调用栈里先发 TURN(r)、再发 END，两条落在同一分区，几乎总在同一个 Kafka batch。消费线程逐条 `queueInLoop`（`kafka_consumer.cpp:402-413`），两帧相隔微秒级。
  2. 主 loop 跑 `OnFrame(TURN r)`：`StartFanout` → `PumpFanout` 发前 512 个，余下排续发。
  3. 同一轮或下一轮 `OnFrame(END)`：`OnEndFrame` 调 `StartFanout(finalFrame, AllSubscribers)`，`coalesced += N - 512`，`fanoutFrame` 换成 166，`fanoutNext = 0`。
- **后果**：后面约 N − 512 个会话永远收不到 TURN(r)。这不是「下一帧自愈」：`SpectateEndS2C` 只有 `battle_id / outcome / reason`（`proto/battle/player_battle.proto:85-89`），不带状态也不带事件。所以 1000 人的 gate 约一半观众看不到最后一回合，单台 2 万人时几乎全部看不到；每场结束都发生，只是计入 `coalesced`，没有别的信号。人少的本地冒烟测不出来。
- **测试缺口**：`NewFrameTakesOverTheUnfinishedFanout`（`gate_spectate_hub_test.cpp:691`）只测 TURN → TURN；`EndFanoutIsBatchedAndReleasesSubscribersWhenFinished`（`:797`）之前没有在途 TURN。
- **最小修法**：END 不合并在途帧。`OnEndFrame` 置 `kEnded`、存 `finalFrame` 后，若 `fanoutNext < fanoutTargets.size()` 就先返回；`FinishFanout` 发现 `phase != kLive` 且 `fanoutFrame != finalFrame` 时再 `StartFanout(finalFrame, AllSubscribers)`。配套两处：`DeliverToTarget` 的 MustDeliver 判据改成 `fanoutFrame == finalFrame`（`:433` 现在按 phase 判）；`latestState` / `firstFrame` 的清理推迟到 END 开始扇出（否则尾部没收过状态的会话合成不出 161）。
- **回归用例**：批大小 2、订阅者 5 个，`OnFrame(TurnFrame(9)); OnFrame(EndFrame(9)); RunAllDeferred();`，断言每个会话都恰好先 158 后 166。

### 严重-2：升档局的中继 END 会越过广播 topic 上的最后一回合 TURN

- **位置**：`battle_room_manager.cpp:3531-3535`（升档后 TURN 只发广播）与 `:3550-3557`（END 还要发给全部 `relayGates`）；`gate_spectate_hub.cpp:205-208`（`kEnded` 之后的帧一律丢弃）。头文件 `battle_room_manager.h:567-568` 明确选择了多发这几条，§15.4「所有中继与广播通道发 END」同样如此。
- **触发序列**（任一升过档的场次，在升档前挂过的 gate 上；按默认阈值自动升档时至少 32 台，运营标记后还没有中继观众就升档的场次可能是 0 台）：
  1. battle 同一调用栈发 TURN(r) 到 `spectate-bcast_g<N>`，随后发 END 到 `gate-spectate_g<N>` 与广播。
  2. gate 上两条 topic 是两个独立消费者、两个线程（`main.cpp:266-288`、`kafka_consumer.cpp:381-400`），各自 fetch（`fetch.wait.max.ms=50`，`kafka_consumer.cpp:178`），之间没有顺序。
  3. 中继 END 先到：`OnEndFrame` 置 `kEnded`，扇出 166、清空订阅。
  4. 广播 TURN(r) 后到：`:205` 直接返回；广播 END 同样丢弃。
- **后果**：这些 gate 上的观众收到 166，看不到最后一回合。每场升档局结束、每台升档前挂过的 gate 都要赛跑一次（未实测，机制上没有偏向）。而这些 gate 正是观众最多的。
- **测试缺口**：`PromoteIsIdempotentAndLaterRoundsOnlyBroadcast`（`battle_observer_contract_test.cpp:1251-1258`）把「升档后中继也发 END」钉成了契约；gate 用例 `gate_spectate_hub_test.cpp:524` 只覆盖 END 之后的帧被丢弃，没有跨 topic 乱序的场景。
- **最小修法**：`PublishSpectateEnd` 已升档时只发广播（`if (room.spectateBroadcast) { PublishToBroadcast(frame); return; }`），与 §3「升到 L2 的回合 R 起只发广播」对齐，同分区保序就消除了赛跑。同步改 §15.4、`battle_room_manager.h:567-568` 与上面的用例。代价：广播 END 丢了这些 gate 要等 `T_idle` 收尾；没注册 L2 消费者的 gate（见一般-2）收不到 END，它们升档后本来也收不到 TURN，同样靠 `T_idle`。

---

## 一般

### 一般-1：合并后从头重发，过载时表尾会话被持续饿死

- **位置**：`gate_spectate_hub.cpp:285`、`:66-75`（`AllSubscribers` 按哈希表迭代序）、`:379-382`。
- **触发序列**：一轮扇出耗时超过回合间隔（2 万订阅者要约 40 轮 loop，每轮 512 次 `write`，gate 过载时 loop 变慢）。TURN(r+1) 到达时游标归零，新名单与上一轮顺序相同（没有 rehash 时哈希表迭代序不变），又从表头发起。
- **后果**：表头会话帧帧都收到，表尾会话一帧也收不到，也不收 END 之前的任何状态，连续多回合黑屏，指标只有总的 `coalesced`。§15.5 写的是「上一帧还没发完的会话直接改发新帧」，实现让它们排在最后。软水位本身不会饿死会话，这条才会。
- **最小修法**：合并时把新名单旋转到上一轮停下的位置，例如 `StartFanout` 前对 `targets` 做 `std::rotate(begin, begin + fanoutNext % size, end)`，过载时变成所有人轮流丢帧。

### 一般-2：走维护控制启动的 gate 没有 L2 消费者，升档后这台 gate 的观众全部 SOURCE_LOST

- **位置**：`cpp/nodes/gate/main.cpp:277-282`、`cpp/libs/engine/core/node/system/node/node.cpp:1262-1263`、`battle_room_manager.cpp:3531-3535`。
- **触发序列**：gate 带 `MMORPG_MAINTENANCE_CONTROL_DIR` 启动（`tools/scripts/cpp_nodes.ps1`、`maintenance_drain.ps1`；`deploy/` 与 `k8s_deploy.ps1` 里没有，K8s 不受影响）→ 只注册 L1 → 该场升档后 battle 不再给它发中继 → 已在看的观众 30 s 后 SOURCE_LOST；之后的 WatchBattle 仍回 LOBBY，订阅成功，30 s 后再 SOURCE_LOST，match 不知情。
- **后果**：比 `main.cpp:276` 注释说的「单个具名节点的受控过程」范围大：prepare-only 开放之后这个 boot 长期服务（P1-GATE 风险第 3 条已提）。只影响观战，不碰资产。
- **最小修法**：需要拍板。要么给 `RegisterKafkaMessageHandler` 加「不进维护证据」的选项让 L2 也能注册，要么把「走维护流程的部署 `BroadcastMaxConcurrent` 置 0」写进部署约束。

### 一般-3：回选角与换角色不摘观战订阅

- **位置**：`cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp:379-403`（`OnClientLeaveGame`）、`scene_route_helper.h:290-302`（`ApplyLoginBinding` 换 `session.playerId`）。全仓 hub 的摘除调用只有 `client_session_handler.cpp:629` 与 `gate_event_handler.cpp:227` 两处断线路径。
- **触发序列**：玩家 A 在会话 S 上订阅 → LeaveGame 回选角（TCP 不断）→ 以角色 B EnterGame → BindSession 把 `session.playerId` 改成 B → 订阅还在，帧继续发到 S，直到战斗结束。
- **后果**：凭证绑的是 A，帧却继续发给已经是 B 的会话。观战帧是公开局脱敏帧，不泄密，也不碰状态，只是多收帧，依赖客户端自己退订。与 §15.1 列出的清订阅条件（新订阅、结束、断线、空闲）没有冲突，但属于 AGENTS §11.4 要求核对的销毁路径。
- **最小修法**：`OnClientLeaveGame` 里调 `gate_spectate::OnSessionClosed(sessionId)`。

### 一般-4：gate 重启后，发给前任实例的中继帧在继任者上逐帧打 WARN

- **位置**：`cpp/libs/engine/core/node/system/node/node_kafka_command_filter.h:135-142`；中继帧走同一过滤（`node_kafka_command_handler.h:119`）。
- **触发序列**：gate 重启并拿回同一 node_id（分配器立刻回收）→ battle 的 `relayGates` 还是旧实例，没有摘除通道，只有该 gate 有新观众重新 Attach 才覆盖 → 这些场每回合一帧发到旧实例 → 继任者读到「node_id 是自己、instance 不对」，每帧一行 WARN，直到场次结束。
- **后果**：未采样。这条分支原本面向偶发的控制命令，观战帧是持续流：1000 场、3 秒一回合约 330 行 / 秒，在主 loop 上同步写日志（AGENTS 记过同类问题）。
- **最小修法**：`KafkaCommandTargetFilter` 加一个采样选项，只在两条观战 topic 的 options 上打开；或者只在观战帧路径把这条降成采样。

### 一般-5：设计文档与注释还留着单水位规则，客户端照写会复发同一个缺陷

- **位置**：`docs/design/battle-spectate-tiered-fanout.md:220`（§9「gate 和客户端都丢弃 round_index 不大于已收值的帧」）、`:139`（§6.1 客户端按 `(battle_id, round_index)` 去重）；`proto/contracts/kafka/gate_event.proto:152`；`gate_spectate_hub.h:16`（还写 `last_round` 单水位）。
- **触发序列**：客户端按 §9 写 LOBBY 去重：收 161（`state.round_index = r`）记水位 r → 收 158（`round_index = r`），`r <= r` 丢弃。每次挂接、升档少播一回合；最后一回合挂接时看不到终局回合。这正是 gate 刚修的那个缺陷，§15.5 只改了 gate。
- **最小修法**：§6.1 / §9 写明「158 只与 158 比较，161 不推进水位；TURN(r) 的 state 回合是 r + 1」，同步改 `gate_event.proto` 注释与 `gate_spectate_hub.h:16`。Java 版同语义时注意同一点。

---

## 六个重点的核对结论

1. **帧格式两端**：对得上，未发现问题。`ClientEnvelope`（`battle_room_manager.cpp:85-91`）→ `client_frame`（`:3509`）→ gate 解析与「种类 / 消息号」核对（`gate_spectate_hub.cpp:23-52`）→ `EncodedFrame` 用同一 codec（`gate_push_delivery.h:62-65`）。合成 161 取 158 的 `state`，这份 state 已过 `RedactStateForViewer`（`:2043`）。回合语义与 §15.4 一致：STATE 取快照回合，TURN 取结算前的 `roundIndex`（`turn_battle_engine.cpp:798`），gate 不看 END 的回合。
2. **两条水位与时序**：单 topic 内的时序都对。挂接 STATE(r) → TURN(r)、TURN 先于 STATE、等回执期的冻结 STATE(r) → TURN(r) → END、升档时中继旧帧晚到，都能得到预期结果，缓存不回退。丢终局的是严重-1、严重-2。
3. **topic / 分区 / 代号 / 过滤**：两端一致，未发现问题。代号与分区数同读 `node_command_route.h`；广播 16 分区是同一常量；中继只 assign `node_id % P`；目标字段名与 `BuildDefaultKafkaOptions` 派生结果相同（`target_gate_id` 是 uint32，反射读得到）。`registerLegacyPerNodeTopic = false` 且广播 options 由中继 options 复制，不会注册旧 topic；gate-cmd 注册不变；`SetKafkaHandlers` 覆盖默认钩子的时机正确（`node_entry.h:182-184`）。
4. **Hub 生命周期**：断线与无连接残留两条 erase 路径都接了钩子，session_id 复用安全；顶号走 `shutdown`、重定向保留会话，都等 TCP 关闭时摘除，期间 `send` 是空操作；续发闭包只持 `weak_ptr` 与值，定时器与 Kafka 回调不捕获对象，令牌机制挡住条目重建后的旧闭包；发送不会同步重入；无人订阅条目（30 s 无帧）与墓碑（10 分钟）都有界。只有一般-3 这条摘除路径缺口。
5. **凭证与首帧顺序**：未发现问题。校验顺序是密钥 → 外层 → 签名 → 载荷 → 玩家 → 场次 → 期限，签名字节与 Go 对齐（小写 hex，`spectate_ticket.go:57-59`）；实例为空 fail-closed；回执先于 161 / 166（`gate_spectate_client.cpp:117-119`）。附带说明：凭证不绑 gate（`gate_spectate_ticket.proto:10-16`），客户端重连到没挂接的 gate 时复用旧凭证只会空等 30 s 后收 SOURCE_LOST；按 §5.3 重连应重发 WatchBattle，建议写进客户端契约。
6. **分批与软水位**：Linux 上真的让出 loop：首批在 `doPendingFunctors` 内，`queueInLoop` 触发 wakeup，下一批之前先处理网络事件（`third_party/muduo-linux/muduo/net/EventLoop.cc:160-170`）。Windows 移植的 `wakeup()` 是空操作、poll 超时 100 ms（`third_party/muduo/muduo/net/EventLoop.cc:35-37,249-259`），没有其它事件时每批最多等 100 ms，约 5120 会话 / 秒：不影响正确性，但不要拿 Windows 的结果定 §11 P0 要实测的扇出上限。软水位不会饿死会话：缓冲降下来下一帧就发，首帧被跳过会补 161，166 不受限；饿死来自一般-1。

## 未覆盖

`gate_spectate_client.cpp`（真实软水位判定、两条 RPC 回执）与 `main.cpp` 接线没有单测，要等 robot 支持 SubscribeSpectate 后冒烟；两端用例也没有用 battle 真实产出的帧驱动 hub。Go match、部署脚本、Java 不在本次范围。

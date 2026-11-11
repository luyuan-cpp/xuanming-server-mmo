# 世界频道:每副本近窗快照与推送扇出

服务器缺口清单第 19 条([server-gap-audit-20261101.md](../handoff/server-gap-audit-20261101.md))。2026-11-06 起草。

## 1 问题

- 世界频道全服只有一条流,`stream_key = 'world-v2'`(`go/chat/internal/authority/store.go` 的 `streamKey`)。发送写进 `chat_send_authority` 一行,SQL 提交是唯一真相。
- 没有推送。客户端要看到新消息只能调 `PullChatHistory`,每次调用都要在 SQL 上执行一次
  `WHERE stream_key='world-v2' AND state=1 ORDER BY accepted_ms DESC LIMIT n`,读的是 `idx_chat_stream` 的同一段尾部。
- 读量 = 在线人数 × 拉取频率,而且全压在一个索引段上。加 chat 副本不涨吞吐:瓶颈在库,不在副本。

## 2 目标与不变量

1. SQL 提交仍是唯一真相。发送路径(幂等、限流、敏感词、身份校验)不变,不加 Redis,不改表。
2. 世界频道的读:每个副本对 SQL 的读量只取决于副本数和刷新周期,与在线人数无关。
3. 拉取结果的语义与直读 SQL 相同:最新一条超过 `HistoryTTLSeconds` 就整窗不可见,相邻两条间隔达到 TTL 就截断,条数上限不变,遇到损坏的原件照样报错。唯一的差别是有界的滞后,最多 `WorldHistoryCacheMs`。
4. 快照里只有已提交的原件,读失败不缓存,读不到就按存储故障回(fail-closed,与直读一致)。

## 3 第一批:每副本近窗快照(2026-11-06 落码,未编译)

实现在 `go/chat/internal/authority/world_window.go`,`Store.Pull` 的世界频道分支调用它,私聊仍直读。

- **快照**:一次 SQL 读取世界频道最新的 `min(HistoryMaxLimit, HistoryMaxEntries, MaxHistoryPullLimit)` 行(任何一次拉取最多就要这么多),连同读取时的数据库时间。只存原始行,不存解码结果。
- **拉取**:快照的单调年龄小于 `WorldHistoryCacheMs` 就直接用。过期就重读,同一时刻全副本只有一次重读在途,其余请求等它的结果(单飞)。重读在独立 goroutine 里跑,不受第一个请求取消的影响,自带 3 秒超时;等待方各自按自己的请求期限放弃。
- **语义一致**:直读和快照共用 `scanHistoryRows` / `visibleHistory` 两个函数,窗口截断、逐行校验、条数上限是同一份代码。拉取时只解码遍历到的行,损坏的原件也只在遍历到时才报错,与直读遇到才报的时机相同。
- **时间**:TTL 窗口用数据库时间。快照记下读取时的数据库时间,应答时加上快照的单调年龄,不用本机墙钟。
- **读己之写**:本副本世界频道提交成功后,快照立即作废,并让在途的重读结果不进缓存(作废计数)。所以发送者在同一副本上的下一次拉取一定看得到自己的消息。拉取落到别的副本时,最多滞后 `WorldHistoryCacheMs`。
- **开关**:`WorldHistoryCacheMs` 缺省 500,0 表示回到直读,上限 5000。它不进 `policyBytes`:只影响本副本读库的频率,不改变已登记原件的判定,各副本可以不同,滚动改它不该被当成规则漂移(同 `ActorVerifyTTLSeconds`)。
- **读量**:每个副本每 500ms 至多一次约 50 行的读,约每秒 2 次,与在线人数无关。拉取的 CPU 开销是解码至多 limit 条小消息。
- **指标**:`chat_world_window_load_total{outcome="ok|error"}`。命中数可由 `chat_pull_total{channel="world",outcome="ok"}` 减去它得到。
- **K8s**:`WorldHistoryCacheMs` 是 chat ConfigMap 的可选镜像键,服务 yaml 有就逐字写,没有就不写,交给 Go 默认值(`k8s_deploy.ps1` 的 `$chatOptional`)。

## 4 第二批:推送扇出(2026-11-06 落码,未编译,需先重新生成协议)

第一批去掉了库的读放大,但客户端仍要轮询才能看到新消息,请求量仍是在线人数 × 频率,只是落在可扩的 chat 副本上。第二批改成推送,客户端只在打开聊天面板和重连时补拉一次历史。

- **协议**:`ClientPlayerChat` 客户端服务里加下行方法 `rpc NotifyWorldChat (ChatMessage) returns (Empty)`,标 `option (OptionClientRpcKind) = CLIENT_RPC_PUSH`,写法同 `friend.proto` 的 `NotifyFriendEvent`。消息号由生成器分配、写回 `message_id.txt`;生成的客户端可调方法集合(`shared/generated/clientrpc`)不收它,客户端主动调它在入口准入就被挡住;服务端内嵌 Unimplemented,不实现它。
- **发布**(`go/chat/internal/svc/world_push.go`):发送事务首次提交成功后,`authority.Store.Send` 在 `SendResult.Message` 里带回这次写入的显示原件。重放与限流时它为 nil,所以不会推第二次。`ChatLogic.SendChat` 把它交给 `ServiceContext.PublishWorld`,后者把原件包成 `BroadcastToAllEvent{message_content{message_id = NotifyWorldChat, serialized_message}}`,同步写进 `chat-world-bcast_g<N>`:
  - key 是发送者 player_id,走 Hash,同一个人的消息有序(AGENTS.md §7.3);
  - RequireOne 加同步写,超时 500ms,挂在请求预算下;
  - 推送是至多一次的通知:写失败只计 `chat_world_push_total{outcome="error"}` 并记日志,发送应答照常成功;提交后、写 Kafka 前进程退出,也只是少一次推送,历史里照样有;
  - 真相仍是 `PullChatHistory`,客户端按 `send_time_ms` 排序、按 (sender_player_id, send_time_ms) 去重;
  - `Kafka.Brokers` 为空时不推;推送在同一次同步 RPC 里发出,受 RootAdmission 管,没有后台发布者,维护排空的约定不变。
- **gate 消费**(`cpp/nodes/gate/main.cpp` 的 `RegisterChatWorldBroadcastConsumer`):照观战 L2 广播(`spectate-bcast`)的写法,注册一个 `assignAllPartitions` 的消费者。
  - 类型是 `BroadcastToAllEvent` 本身,不用 `GateCommand` 信封:这个 topic 只许承载「推给本机全部已绑定会话」一种动作,踢人、重定向之类的命令进不来。
  - 处理函数先核对 `message_id` 等于 `NotifyWorldChat`,不等就丢弃并记 ERROR(纵深防御:topic 写权限之外再挡一道),再交给 `GateEventHandler::BroadcastToAllEventHandler`。
  - 维护受控启动不注册,同观战 L2。
- **每台 gate 的转发预算**:推送开销是「世界消息数 × 本机在线人数」,刷屏或脚本把全服聊天量推高时,不设上限每台 gate 的下行按平方增长。gate 用令牌桶按每秒 10 条、突发 20 条转发,超出的那几条对本机全部会话一起不推,采样打 WARN。每台 gate 的推送开销因此只与本机在线人数成正比,与全服聊天量无关。不推的消息仍在历史里。这组数是人读得完的量级,不是压测结论,压测后按下行带宽再定。
- **分区**:4 分区,不可变契约,改数走换代号。C++ `node_command_topic.h` 的 `kChatWorldBroadcastTopicBase` / `kChatWorldBroadcastTopicPartitions` 与 Go `shared/kafkacmd` 的 `ChatWorldBroadcastTopicBase` / `ChatWorldBroadcastTopicPartitions` 逐字相同(`chat_world_topic_test.go` 读 C++ 头文件对拍)。代号与命令 topic 同源:C++ 读 `base_deploy_config.yaml`,Go 读环境变量 `KAFKA_COMMAND_TOPIC_GENERATION`,本机、docker 调试与 K8s 都注入了。本机启动器复用已在跑的 chat 进程之前,同样核对它启动时的代号(`start_game.ps1` 的 `Test-UsesKafkaCommandContract`,2026-11-11 补上):Go 侧在进程内只读一次这个变量,换代后不核对就复用旧进程,推送会写进旧代号的主题,gate 收不到。
- **部署**:四处预建,4 分区、保留 10 分钟(gate 从分区末尾读,不回放;漏掉的由客户端拉历史补):
  - K8s `kafka-topic-init.yaml`,占位由 `Apply-KafkaTopicInitJob` 填;
  - docker compose 的 `kafka-topic-init`;
  - docker 调试栈的 `prepare_docker_debug.py`;
  - 本机 `start_game.ps1`;
  - chat 的 ConfigMap 加 `Kafka.Brokers`。这个 topic 量小、保留短,落在 Kafka 盘预算的「其余 topic 预留」里,没有单列。
  - 合同用例在 `k8s_deploy_contract.tests.ps1`。
- **客户端**:处理 `NotifyWorldChat`;打开面板和重连时拉一次历史,不再定时轮询。由 Codex 实现。
- **分两批的原因**:第二批要改客户端契约(新的下行消息)、gate C++ 和部署 topic,按 AGENTS.md §12 客户端契约两版要同批改。第一批只去掉库的读放大,不改任何客户端可见行为,可以先单独上线。

## 5 不做与残余

- **写入热点**:世界频道的写入仍追加在同一段索引尾部。写入量受每人每秒 5 条的限流和产品规则约束,远小于读量。要等压测证明单段扛不住,再按 `world-v3:<分片>` 拆写入、在快照里合并多段。
- **表无限增长**:发送原件永久保存(运行账号没有 DELETE),和本条无关,单独处理。
- **发送侧的全服配额**:gate 的转发预算只保护下行。全服世界频道的发送量现在只受每人每秒 5 条约束。要在发送时就拒绝刷屏(比如全服每秒上限、世界频道冷却),这是产品规则,定了再在 chat 做。
- **gate 转发预算没有单元用例**:它在 `main.cpp` 的匿名命名空间里,要靠冒烟与压测观察 WARN 采样行。
- **Java 版**:两批都待做(客户端契约要同批),见 Java 仓 `PARITY.md`。

# 世界频道:每副本近窗快照与推送扇出

服务器缺口清单第 19 条([server-gap-audit-20261101.md](../handoff/server-gap-audit-20261101.md))。2026-11-06 起草;2026-11-12 补第三批(§5:发言间隔、原件保留期、写入分段)与客户端。三批都未编译,待 Codex 验证。

## 1 问题(2026-11-06 核对时的状态)

- 世界频道全服只有一条流,`stream_key = 'world-v2'`。发送写进 `chat_send_authority` 一行,SQL 提交是唯一真相。
- 没有推送。客户端要看到新消息只能调 `PullChatHistory`,每次调用都要在 SQL 上执行一次
  `WHERE stream_key='world-v2' AND state=1 ORDER BY accepted_ms DESC LIMIT n`,读的是 `idx_chat_stream` 的同一段尾部。
- 读量 = 在线人数 × 拉取频率,而且全压在一个索引段上。加 chat 副本不涨吞吐:瓶颈在库,不在副本。

三批做完之后:读走每副本快照(§3),新消息靠推送(§4),写入按发送者分成 8 段、全服发送量受发言间隔约束、原件保留 30 天(§5)。

## 2 目标与不变量

1. SQL 提交仍是唯一真相。发送路径的幂等、敏感词、身份校验不变,不加 Redis。第一、二批不改表;第三批给原件表加了保留期列,并多一条世界频道发言间隔(§5)。
2. 世界频道的读:每个副本对 SQL 的读量只取决于副本数和刷新周期,与在线人数无关。
3. 拉取结果的语义与直读 SQL 相同:最新一条超过 `HistoryTTLSeconds` 就整窗不可见,相邻两条间隔达到 TTL 就截断,条数上限不变,遇到损坏的原件照样报错。唯一的差别是有界的滞后,最多 `WorldHistoryCacheMs`。
4. 快照里只有已提交的原件,读失败不缓存,读不到就按存储故障回(fail-closed,与直读一致)。

## 3 第一批:每副本近窗快照(2026-11-06 落码,未编译)

实现在 `go/chat/internal/authority/world_window.go`,`Store.Pull` 的世界频道分支调用它,私聊仍直读。

- **快照**:一次读取世界频道最新的 `min(HistoryMaxLimit, HistoryMaxEntries, MaxHistoryPullLimit)` 行(任何一次拉取最多就要这么多),连同读取时的数据库时间。只存原始行,不存解码结果。第三批起世界频道分 8 段,一次读取是同一个事务里的 8 条查询,合并后取最新的这么多行(§5)。
- **拉取**:快照的单调年龄小于 `WorldHistoryCacheMs` 就直接用。过期就重读,同一时刻全副本只有一次重读在途,其余请求等它的结果(单飞)。重读在独立 goroutine 里跑,不受第一个请求取消的影响,自带 3 秒超时;等待方各自按自己的请求期限放弃。
- **语义一致**:直读和快照共用 `scanHistoryRows` / `visibleHistory` 两个函数,窗口截断、逐行校验、条数上限是同一份代码。拉取时只解码遍历到的行,损坏的原件也只在遍历到时才报错,与直读遇到才报的时机相同。
- **时间**:TTL 窗口用数据库时间。快照记下读取时的数据库时间,应答时加上快照的单调年龄,不用本机墙钟。
- **读己之写**:本副本世界频道提交成功后,快照立即作废,并让在途的重读结果不进缓存(作废计数)。所以发送者在同一副本上的下一次拉取一定看得到自己的消息。拉取落到别的副本时,最多滞后 `WorldHistoryCacheMs`。
- **开关**:`WorldHistoryCacheMs` 缺省 500,0 表示回到直读,上限 5000。它不进 `policyBytes`:只影响本副本读库的频率,不改变已登记原件的判定,各副本可以不同,滚动改它不该被当成规则漂移(同 `ActorVerifyTTLSeconds`)。
- **读量**:每个副本每 500ms 至多一次读取(第三批起是 8 条各约 50 行的查询),约每秒 16 条查询,与在线人数无关。拉取的 CPU 开销是解码至多 limit 条小消息。
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
- **每台 gate 的转发预算**(`cpp/nodes/gate/handler/event/world_chat_push_budget.h`):推送开销是「世界消息数 × 本机在线人数」,刷屏或脚本把全服聊天量推高时,不设上限每台 gate 的下行按平方增长。gate 按每秒 10 条、最多连发 20 条转发,超出的那几条对本机全部会话一起不推,采样打 WARN。每台 gate 的推送开销因此只与本机在线人数成正比,与全服聊天量无关。不推的消息仍在历史里。
  - 2026-11-11 从 `main.cpp` 的匿名命名空间挪成独立的纯逻辑头,算法换成 GCRA(与令牌桶等价,只记一个「理论到达时刻」,全用整数时长,拒绝不改状态);用例 `cpp/nodes/gate/tests/world_chat_push_budget_test.cpp`,登记在 `routing_identity_test` 工程。
  - 每秒 10 条、连发 20 条定为正式值:这是一个人读得过来的量级;全服的发送量另由发言间隔约束(§5)。压测只用来核对下行带宽,不再是定值的前提。
- **分区**:4 分区,不可变契约,改数走换代号。C++ `node_command_topic.h` 的 `kChatWorldBroadcastTopicBase` / `kChatWorldBroadcastTopicPartitions` 与 Go `shared/kafkacmd` 的 `ChatWorldBroadcastTopicBase` / `ChatWorldBroadcastTopicPartitions` 逐字相同(`chat_world_topic_test.go` 读 C++ 头文件对拍)。代号与命令 topic 同源:C++ 读 `base_deploy_config.yaml`,Go 读环境变量 `KAFKA_COMMAND_TOPIC_GENERATION`,本机、docker 调试与 K8s 都注入了。本机启动器复用已在跑的 chat 进程之前,同样核对它启动时的代号(`start_game.ps1` 的 `Test-UsesKafkaCommandContract`,2026-11-11 补上):Go 侧在进程内只读一次这个变量,换代后不核对就复用旧进程,推送会写进旧代号的主题,gate 收不到。
- **部署**:四处预建,4 分区、保留 10 分钟(gate 从分区末尾读,不回放;漏掉的由客户端拉历史补):
  - K8s `kafka-topic-init.yaml`,占位由 `Apply-KafkaTopicInitJob` 填;
  - docker compose 的 `kafka-topic-init`;
  - docker 调试栈的 `prepare_docker_debug.py`;
  - 本机 `start_game.ps1`;
  - chat 的 ConfigMap 加 `Kafka.Brokers`。这个 topic 量小、保留短,落在 Kafka 盘预算的「其余 topic 预留」里,没有单列。
  - 合同用例在 `k8s_deploy_contract.tests.ps1`。
- **客户端**(2026-11-11 落码,客户端仓 `../mmorpg-client`,未在 Unity 里运行):
  - `SocialClient` 构造时订阅 `NotifyWorldChat`,只收当前已就绪的角色;解析失败整条丢弃,下次拉历史补上。同一消息号只保留最后注册的处理器,换角色或重连后新建的实例会替换旧的,旧实例释放后它的处理器直接返回。
  - `SocialState` 把「拉历史整表替换」改成「合并去重」:推送与历史按同一个 Id(发送者、发送时刻、正文)合并,按发送时刻排序,只留最近 100 条(`HistoryLimit`)。以前整表替换时,拉取途中到达的推送会被冲掉。重复的推送不触发界面刷新;不在看这个频道时计未读。
  - 拉历史的时机不变:打开面板、切换频道、点刷新、发送成功之后各一次,本来就没有定时轮询。重连后界面状态整体复位,再打开面板时拉一次。
  - 被限流(每秒条数或发言间隔)时提示「发言太频繁,请稍等几秒再发」,草稿保留。
  - 用例在 `Assets/Tests/EditMode/Social/SocialTests.cs`:合并不互相覆盖、重复与错频道的推送被忽略、未读只在不看该频道时计、本地窗口只留最新 100 条、推送不触发请求、释放或未就绪时不收、坏包丢弃、限流提示。
- **分两批的原因**:第二批要改客户端契约(新的下行消息)、gate C++ 和部署 topic,按 AGENTS.md §12 客户端契约两版要同批改。第一批只去掉库的读放大,不改任何客户端可见行为,可以先单独上线。

## 5 第三批:发言间隔、原件保留期、写入分段(2026-11-12 落码,未编译)

前两批留下三条「以后再说」。负责人 2026-11-11 定「不等压测,先按标准做法定案写完」,这一批把它们做完。三项都只动 `go/chat`,不改协议。**要清库**:表多了一列、流键换了名、策略行换了版本,旧的 chat 库起不来(启动时核结构,会明确报错),项目未上线,删掉 `mmorpg_chat` 的两张表后重新 `-migrate`。

### 5.1 世界频道发言间隔

- **规则**:同一名玩家相邻两条世界频道消息至少隔 `WorldChannelCooldownMs`(缺省 5000,0 = 不限,上限 10 分钟)。只管世界频道,私聊仍只受每秒条数约束。
- **为什么是按人的间隔,不是全服每秒上限**:全服上限要一个全服共享的计数,它本身就是不能水平扩展的热点,而且谁先抢到算谁的,正常玩家会被刷屏的人挤掉。按人的间隔把全服世界消息量压在「在线人数 ÷ 间隔」之内,每个人只看自己的行,这也是网游世界频道的通行做法。下行另有每台 gate 的转发预算兜底(§4)。
- **实现**(`store.go` 的 `worldCooldownActive`):发送事务里查这名玩家在自己那一段(§5.3)最近 `WorldChannelCooldownMs` 毫秒内有没有被接受的世界消息,沿 `(player_id, state, accepted_ms)` 的索引走,行数不超过「间隔秒数 × 每秒条数上限」。时间用数据库时间。
- **被挡的请求**回 `kRateLimitExceeded`(与每秒限流同一个提示码),不落库、不占 request_id,间隔到了用同一个 id 能发出去。间隔内重放已提交的那条仍按重复回。指标 `chat_send_total{outcome="world_cooldown"}` 与 `rate_limited` 分开记。
- **近似**:查询与插入之间没有锁,与每秒限流相同。同一会话的请求由 gate 的会话 lane 串行,正常客户端不会并发;绕过 lane 的并发最多多放过在途的那几条。
- **不进策略行**:它是运营会调的值,滚动改它时各副本短暂按不同间隔判,只影响新请求放不放行。K8s ConfigMap 逐字镜像服务 yaml 的这一行。

### 5.2 原件保留期

- **规则**:`chat_send_authority` 的行保留 30 天(`constants.OriginalRetentionDays`),到期由 TiDB 的 TTL 任务在库里删除,每天扫一次。以前这张表只增不减。
- **为什么用库的 TTL,不在 chat 里跑清理**:运行账号只有 SELECT / INSERT,被攻破的 chat 进程改不了、删不了已登记的原件;在 chat 里清理就得给它 DELETE。
- **实现**(`schema.go`):
  - TTL 只认日期时间列,所以另存一列 `accepted_at DATETIME(3)`,与 `accepted_ms` 是同一时刻,固定写 UTC。不用 TIMESTAMP,因为它到 2038 年为止。
  - 属性写在 TiDB 专用注释里(`/*T![ttl] … */`,TiDB 自己的 `SHOW CREATE TABLE` 就这样输出):MySQL 当普通注释忽略,开发用 MySQL 时表照建、只是不删。
  - 启动时在 TiDB 档读 `SHOW CREATE TABLE` 核对属性还在、还开着,不在就打一行 ERROR,不拒启:后果是表只增不减,数据没有错。
- **连带的三处语义**:
  - 历史可见窗口 `HistoryTTLSeconds` 不得超过 29 天(保留期减一天),配置校验与 `authority.New` 都拒绝。留出的一天盖住 `accepted_at` 按 UTC 写、集群时区不是 UTC 时最多十几个小时的偏差;TTL 任务只会晚删,不会早删。
  - 历史最多回看 30 天:以前只要相邻两条间隔不到 TTL,一条连续的历史可以一直往回看。
  - 超过保留期的旧 request_id 重发会被当成新请求。客户端每次发送用新的 id,重试只发生在几秒到几分钟之内。
- **搬库**(`tools/dbmove`):搬 `mmorpg_chat` 期间目标集群上这张表的 TTL 要关着,audit 通过之后再打开,步骤写在 [搬库手册](../ops/db-move-runbook.md) §4 第 8 步。BR 恢复出来的表本来就是 `TTL_ENABLE='OFF'`,要做的是事后记得打开;忘了的话 chat 启动时会报 ERROR。两边各自跑 TTL 任务时删的行不同步,固定位置上的校验和会对不上;对不上只会让搬库中止,不会写错数据。源集群的 TTL 删除是普通事务,会随复制到目标。
- **钉在策略行里**:保留天数与分段数(§5.3)是存储布局,策略字节升到 `chat-authority-v2` 并带上这两个数。以后的构建改了它们,对着旧库起不来,必须显式迁移。

### 5.3 世界频道写入分段

- **问题**:每条世界消息都要进按 `(stream_key, state, accepted_ms, …)` 排的索引。只有一个流键时,全服的写入都追加在这个索引的同一处,TiDB 上是一个拆不开的热点 Region。主键与按玩家的索引以 `player_id` 开头,发号器已把它打散,不是热点。
- **做法**:流键从 `world-v2` 改成 `world-v3:<k>`,k = 发送者 `player_id % 8`(`constants.WorldStreamShards`)。8 个追加点能被拆到不同的 Region。一名玩家的世界消息只落在一段。
- **读**(`readWorldRows`):每段各取最新 N 行,按与单段查询相同的顺序(接受时刻、发送者、请求号都从大到小)合并,取前 N 行。全服最新的 N 行里,任何一行都在它自己那一段的最新 N 行之内,所以不会漏。可见窗口的截断规则在合并之后照旧逐行走。
- **读的代价**:一次读取从 1 条查询变成 8 条。世界频道走每副本快照(§3),每个副本每 500ms 至多读一次,这点代价与在线人数无关;把 `WorldHistoryCacheMs` 设成 0 关掉快照时,每次拉取都是 8 条查询,不建议在生产这样配。
- **8 段够不够**:写入量已被发言间隔压住(10 万人同时在线、人人卡着 5 秒发,也只有每秒 2 万条,摊到每段 2500 条);单个 Region 的写入能力在每秒数千条的量级。段数是存储布局契约,改它要清库或迁移。

## 6 不做与残余

- **发言间隔之外的产品规则**(等级门槛、消耗道具的喇叭、禁言名单):没有需求,不做。
- **分段读的并发**:8 条查询在一个事务里先后发,没有并行。走快照时每个副本每秒十几条查询,不值得为它开多条连接。
- **保留期的运行证据**:TTL 属性的写法按 TiDB 文档与它自己的 `SHOW CREATE TABLE` 输出写,没有在真库上跑过。真库用例的夹具在建表之后立即核对属性(`RetentionEnabled`),Codex 跑任何一条 `TestChatTiDB*` 都会验到;`dbmove` 那一步只写了手册,没有自动化。
- **Java 版**:三批都待做(第二批改客户端契约,要同批),见 Java 仓 `PARITY.md`。

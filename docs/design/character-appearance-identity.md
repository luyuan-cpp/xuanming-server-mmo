# 人物外观身份链

本批复用账号角色列表和 `PlayerProfileComp` 存档，不用职业、性别或列表下标代替外观身份。

- `appearance_id` 是人物资源目录的完整、稳定 ID，建角请求显式选择；不含 V13/V14 版本。客户端按同 ID 完整 V14 优先、完整 V13 回退，缺资源不得改写已保存身份。
- login 校验允许 ID 后，把它与原有 class/gender 一起写入账号 blob。丢响应后的建角重试必须同时匹配外观，避免相同职业性别的不同人物串用。
- 首次 EnterGame 复用 `backfillPlayerIdentity` 的玩家登录锁、完整字节 CAS、在线会话/位置围栏，把账号外观与性别补进 Redis 中的 `PlayerAllData.PlayerProfileComp`。只补空缺；scene 在线持有权威数据时不写。组件随现有 scene 加载/保存链进入 MySQL；本次只加嵌套字段，不新增 SQL 列，但既有 `player_database.profile_component` 第 15 列必须已迁移，当前真库状态尚未核实。
- scene 自己及 AOI 的 ActorCreate、PrepareBattle 快照、战斗初始/重连/观战状态、match 中的队伍展示读取同一外观；职业与性别仍独立传递，供空外观的旧角色兼容。
- 老客户端省略字段、老存档字段为空时保留已有职业/性别映射。不得把全部旧角色迁移成某个新人物；本批不实现在线换装 RPC。
- 账号 self-heal 曾只恢复 player_id/name。选角响应对空外观从现有 PlayerAllData 缓存补只读副本；预加载完成后的首次入场以玩家锁和账号整字节 CAS 恢复缺失外观/职业/性别，保留其他角色。账号目录不设 TTL；PlayerAllData 仍保留原缓存 TTL。两端非空外观不一致则拒绝入场，不能静默覆盖。
- 新旧二进制混跑期间，旧服务不能保证外观回传。发布顺序为生成协议、构建相关服务、停止旧 login/scene/battle/match 后更新，再启用新客户端建角入口；数据库无损兼容但旧客户端不具备新身份展示验收能力。

验收分层：协议生成与离线测试只证明字段契约；正式验收仍须跑登录、选角、主城、战斗、退出重登，并对每名已验收人物检查同一 ID 与实际行走截图。未完成资源不得因能存 ID 而宣称接入通过。

本机协议工具使用 `third_party/grpc/install_vs2026_dbg/bin/protoc.exe`（35.1）及现有 `protoc-gen-go`，仅重生成六个变更源对应的 C++/Go binding；Go 包映射与现有统一生成器相同（`proto/<目录>`）。Release 安装目录内 protoc 是 31.1，不能用于当前 C++ 7.35.1 runtime。C# 由独立客户端原有 gen_proto.ps1 生成，不复制隔离工程 GUID。

构建日志位于 `run/logs/appearance-identity/`。首次 C++ 构建暴露检查器缺少 WIN32/utf8_range、沙盒 FileTracker 拒绝，以及 Redis ScriptSlot 静态 Lua 源文本裸指针；本批补齐检查器参数并以 string_view 保存静态文本，未关闭检查器。运行服务使用现有本机数据，未清库、重建数据卷或重置 Kafka offset。

## 存储层核实与当前故障边界（2026-09-21）

SQL 按 `PlayerDatabase` / `UserAccounts` 的顶层字段展开列；`profile_component` 和 `simple_players` 则各自是 MEDIUMBLOB 子消息。当前 db 固定依赖 `proto2mysql v0.1.1-0.20260914130151-f3b308f37020` 的 `pbconv/convert.go` 对嵌套消息调用 `proto.Marshal` / `proto.Unmarshal`，存原始 wire 字节，不是 JSON，也不是仓内部分旧注释写的 base64。新增 `appearance_id` / `gender` 不要求独立 SQL 列、独立表或表注册项；生成协议并重编消费者仍是必要条件。

`player_database_loader.cpp:104` 加载整个 `PlayerProfileComp`，`:143` 整组件 `CopyFrom` 存回，未挑拣 name 字段，因此 appearance 不会被既有 ECS marshal 丢弃。随后 `PlayerLifecycleSystem::SavePlayerToRedisImpl` 序列化 PlayerDatabase、发送 DBTask，db 的 `key_ordered_consumer.go` 解码后调用 `SqlModel.Save`。此链已做源码核对，尚无当前节点运行和真实 MySQL 往返证明。

**账号角色目录采用现有 Redis 权威记录，不能声称已完成 MySQL 账号持久化。** `CreatePlayer` 与外观 self-heal 写 `account:<account>` 的 UserAccounts 整体 protobuf（旧注释称 `account_data`）。仓内未找到将此键写入 MySQL `user_accounts.simple_players` 的 write-behind、脏标记消费者或冷读恢复；当前 SQL 密码认证只读取 `account/password`，不恢复角色列表。

本轮修复既有 `Account.CacheExpire=12h` 导致角色列表自动消失的问题：建角围栏 Lua、EnterGame self-heal、外观修复 CAS 和管理端移除角色四条更新路径统一使用无过期 SET；登录初始化同样不设 TTL。两种 Login 入口通过单条 Lua 原子执行 GET/PERSIST/缺失初始化，保留既有 protobuf 原字节，避免初始化空账号覆盖并发建角。旧配置字段仅兼容解析，不再影响账号寿命。会话、token、玩家缓存及分布式锁的 TTL 不变；没有增加 Redis/MySQL 双写。

Compose 配置为 Redis `--appendonly yes`、`redis-data:/data` 持久卷，同时使用 `allkeys-lfu` 淘汰。Docker 当前无法启动，实际容器挂载、AOF 状态、fsync 及恢复效果未验证；配置意图不能作为运行证据。无 TTL 修复消除了新写入/已登录迁移账号的自动到期，但 AOF/持久卷不会阻止 LFU 淘汰。尚未登录或更新的旧 key 仍保留原 TTL，已经过期的历史目录无法凭空恢复。已有 player_to_account 和 profile 只能在持有 player_id 的 EnterGame self-heal 路径恢复某名角色，不能让空选角列表自动枚举账号全部角色；首次进场/scene 存盘前的创建记录尤其只有账号 Redis 一份。

上线前需核实并补齐的前置：

1. 每个目标 zone 用既有 `go/db/cmd/migrate -command plan` 核查 `profile_component` MEDIUMBLOB、主键与所有现行基础列。db 默认 `AutoMigrateSchema=false`，旧手工 SQL 文件还缺 profile 等列，不能据源码存在列就认为真库已迁移。需要补列时按现有部署迁移入口实施；本轮未连接或修改数据库。
2. Redis 权威目录部署必须保证账号及其反向索引不被内存淘汰，并验证 AOF、持久卷和恢复策略。当前共享实例的 `allkeys-lfu` 仍是上线缺口；本轮没有全局更改其他服务的缓存策略。上线迁移还需保护尚未访问的旧 `account:*` key，不能仅等待它们在登录时 PERSIST；本轮未连接或批量改动实例。
3. 环境恢复后验证真实 MySQL profile 字节、正常重登、跨旧 TTL 时长重登和 Redis 服务重启恢复。新增 miniredis 用例覆盖两种 Login 入口、48 小时后选角身份、旧字节/未知字段保留、建角及两类 self-heal/管理端更新；这些是命令和逻辑契约测试，不是 AOF、实库或正式客户端流程证明。已丢失目录只能基于独立备份或可核验历史记录恢复，本轮未实现自动冷读重建。

data_service 的全局 schema 管理 player_name / id_segment / snapshot / audit 等表；其字段读取接口读取 Redis 字节、快照保留字节载荷，不会自动为账号新增落盘消费者，也不负责迁移 zone 的 profile_component 列。

## 旧账号目录上线迁移步骤（待执行，当前未连接 Redis）

1. 确认目标 Redis 实例、逻辑 DB、`redis-data` 实际卷挂载和可恢复备份位置；先完成已有流程的 AOF/RDB 一致性备份并在隔离实例验证可恢复。另导出待迁移账号的原始 protobuf 字节、PTTL、字节 SHA256 和角色身份清单，备份不进入代码仓库。不能把单纯文件复制或配置里的 `appendonly yes` 当成成功恢复证明。
2. 发布新 login 到所有实例，防止旧写入端重新设置 TTL；在迁移窗口暂缓建角、管理删角和账号自愈写入。若不能暂停，对下列 CAS 返回“已变化”的 key 必须重新读取、核验并重试，不能强制覆盖。迁移开始前仍可能到期的账号需优先备份，已经丢失的账号单列异常，不创建空记录来掩盖。
3. 用 Redis 游标迭代 `SCAN <cursor> MATCH account:* COUNT 100` 直到游标回到 0，只处理这个精确前缀并去重；不得扫描后更改其他前缀、使用 KEYS 阻塞全库或取消会话/token/锁 TTL。读取 GET/PTTL/TYPE，非 string、读取时已消失或 protobuf 解析失败的 key 单列异常并停止该 key 的迁移。
4. 通过 Go Redis 客户端读取二进制，使用本次生成的 `UserAccounts` 解析，核对迁移前后 player_id、appearance_id、class_id、gender 及角色数量。旧角色允许 appearance_id 为空，不重排、不新分配 ID，不丢未知 protobuf 字段。保存原始字节供 CAS 比较；不要经终端文本或 JSON 重序列化 blob。
5. 每个已核验 key 执行以下单 key Lua，ARGV[1] 必须是第 4 步读取的原始字节。脚本不 SET、不改 blob；返回 0 代表期间数据变化，返回 -1 代表已经不存在，只有 1 代表本次检查及保留完成。

```lua
local value = redis.call("GET", KEYS[1])
if not value then return -1 end
if value ~= ARGV[1] then return 0 end
redis.call("PERSIST", KEYS[1])
return 1
```

6. 使用同一清单重新读取 GET/TTL，逐 key 确认 `TTL = -1`、字节 SHA256 与原样备份一致、protobuf 角色身份一致；出现改变时重新核对当前合法写入，不能写回旧备份覆盖新角色。复扫 `account:*`，检查没有遗漏的正 TTL，记录成功/变化/缺失/解析异常数量。保持业务写入窗口关闭直到清单核验完成，再恢复入口；不得以成功计数代替缺失账号处理。
7. 选独立测试账号完成正式登录/选角、主城、战斗、退出重登，以及跨旧 12h 和 Redis 重启后恢复。恢复验证必须包括 AOF/卷和进程崩溃场景；本轮未执行。`allkeys-lfu` 仍可能淘汰无 TTL 账号，需要独立确定权威目录不被淘汰的部署措施，不能把上述 PERSIST 迁移称为完整耐久性闭环。

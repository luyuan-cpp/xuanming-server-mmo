# 人物外观身份链

本批复用账号角色列表和 `PlayerProfileComp` 存档，不用职业、性别或列表下标代替外观身份。

- `appearance_id` 是人物资源目录的完整、稳定 ID，建角请求显式选择；不含 V13/V14 版本。客户端按同 ID 完整 V14 优先、完整 V13 回退，缺资源不得改写已保存身份。
- login 校验允许 ID 后，把它与原有 class/gender 一起写入账号 blob。丢响应后的建角重试必须同时匹配外观，避免相同职业性别的不同人物串用。
- 首次 EnterGame 复用 `backfillPlayerIdentity` 的玩家登录锁、完整字节 CAS、在线会话/位置围栏，把账号外观与性别补进 `PlayerProfileComp`。只补空缺；scene 在线持有权威数据时不写。存档组件已由现有加载/保存链往返，无新增 SQL 列。
- scene 自己及 AOI 的 ActorCreate、PrepareBattle 快照、战斗初始/重连/观战状态、match 中的队伍展示读取同一外观；职业与性别仍独立传递，供空外观的旧角色兼容。
- 老客户端省略字段、老存档字段为空时保留已有职业/性别映射。不得把全部旧角色迁移成某个新人物；本批不实现在线换装 RPC。
- 账号 self-heal 曾只恢复 player_id/name。选角响应对空外观从现有 PlayerAllData 缓存补只读副本；预加载完成后的首次入场以玩家锁和账号整字节 CAS 恢复缺失外观/职业/性别，保留其他角色和缓存 TTL。两端非空外观不一致则拒绝入场，不能静默覆盖。
- 新旧二进制混跑期间，旧服务不能保证外观回传。发布顺序为生成协议、构建相关服务、停止旧 login/scene/battle/match 后更新，再启用新客户端建角入口；数据库无损兼容但旧客户端不具备新身份展示验收能力。

验收分层：协议生成与离线测试只证明字段契约；正式验收仍须跑登录、选角、主城、战斗、退出重登，并对每名已验收人物检查同一 ID 与实际行走截图。未完成资源不得因能存 ID 而宣称接入通过。

本机协议工具使用 `third_party/grpc/install_vs2026_dbg/bin/protoc.exe`（35.1）及现有 `protoc-gen-go`，仅重生成六个变更源对应的 C++/Go binding；Go 包映射与现有统一生成器相同（`proto/<目录>`）。Release 安装目录内 protoc 是 31.1，不能用于当前 C++ 7.35.1 runtime。C# 由独立客户端原有 gen_proto.ps1 生成，不复制隔离工程 GUID。

构建日志位于 `run/logs/appearance-identity/`。首次 C++ 构建暴露检查器缺少 WIN32/utf8_range、沙盒 FileTracker 拒绝，以及 Redis ScriptSlot 静态 Lua 源文本裸指针；本批补齐检查器参数并以 string_view 保存静态文本，未关闭检查器。运行服务使用现有本机数据，未清库、重建数据卷或重置 Kafka offset。

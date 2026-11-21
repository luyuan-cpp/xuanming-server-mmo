# 命名规范与权限边界

> 对应根索引 [AGENTS.md §8 / §9](../../AGENTS.md#8-命名规范)。

## 1. 命名规范（§8）

### 第一原则（强制）
所有命名（类名、函数、变量、字段、proto、配置表、表列、错误码、指标等）**必须清晰易懂且语义完全准确**，做到见名知意、不产生歧义：
- 准确表达业务对象、行为动作、状态语义与单位（如 `_ms` / `_sec` / `_count` / `_bytes`），拒绝含糊缩写、误导性命名、与实际行为不符的“历史遗留名”及泛化命名（如无上下文的 `data` / `info` / `flag` / `temp` / `Handle` / `Process`）。
- 布尔值命名必须明确真值含义，不制造双重否定或歧义。
- 命名不得把两层概念混在一起（例如“是什么”与“摆在哪”，或“奖励表 ID”与“业务领取位序”）。

### 分层与模块命名约定
- RPC 请求 handler 是 `cpp/nodes/scene/handler/rpc/...` 下的 `*Handler` 类（例如 `SceneScenePlayerHandler`）。
- 节点间 muduo RPC（RPC0）的回包在调用点处理：`CallMethod` 的每次调用 lambda（`RpcStatus` + 响应），不再按 `message_id` 注册全局回包函数；服务端要等下游再回复用 `RpcDoneCallback::Adopt(done)`。见 [muduo-rpc-channel.md](../design/muduo-rpc-channel.md)。
- `rpc_replies/` 只放 gRPC 异步应答适配（沿用 `On<Domain><Method>Reply` 命名）与玩家回包链。
- ECS / 域逻辑放 `*System` 类与 `*Comp` 结构（`cpp/libs/services/scene/...`）。
- 用清晰的动词式 RPC handler 名 + 薄包装（thin wrapper）委托模式（保留生成的 RPC 方法名，将具体逻辑委托给命名清晰的内部函数，如 `ProcessClientPlayerMessage`）。

## 2. 禁止事项与权限边界（§9）

### 关联 Git 仓库与工作区约束
- 核心主仓库（按 GitHub 地址定位，本地克隆目录名因机器而异，不写死盘符路径）：
  - C++/Go 服务端（本仓库）：`https://github.com/luyuan-cpp/xuanming-server-mmo`
  - Java 服务端：`https://github.com/luyuan-java/xuanming-server-mmo`
  - Python 服务端：`https://github.com/luhailong-python/xuanming-server-mmo-python`
  - Unity 客户端：`https://github.com/luhailong-cpp/mmorpg-client`
  - 协议转 MySQL 工具库：`https://github.com/luyuan-cpp/proto2mysql`
- 临时产物、验证包、构建日志放在所属仓库已忽略的目录或系统 `%TEMP%`；Git worktree 使用托管目录或工作区根目录之外的位置，不在工作区根目录新建 `*-wt-*`、`builds`、`tmp`、`.codex-artifacts` 等杂项目录。

### 硬禁令
- ❌ 不要手改生成产物（见 [proto-and-tables.md](proto-and-tables.md)）。
- ❌ 不要在 `docs/design/` 之外随便建 README，不要为记录改动新建 markdown（除非用户要求）。
- ❌ 不要把 `player_id` 当 Prometheus label（高基数会爆）。
- ❌ **不要开启新的线程**：C++ 节点默认不新加 `std::thread` / `std::jthread` / `std::async` / `EventLoopThread`，异步逻辑挂主 EventLoop，多步等待用 `loop_async::AwaitCallback`。**例外**：除非压测数据证实瓶颈确实需要开启新线程（例如压测评估中考虑开启新的 Kafka consumer 线程），且**必须先和程序/负责人确认并说明原因与压测依据**，获批后再同步登记到 `tools/scripts/tests/test_cpp_thread_sources.py`（清单之外 CI 判红；线程构成见 [thread-count-monitoring_zh.md](../design/thread-count-monitoring_zh.md)）。
- ❌ 不要用 `--no-verify`、注释断言、跳 test 等方式绕过安全检查。
- ❌ 不要执行 `git push` / `git tag`；`git commit` 仅在用户明确要求时执行。
- ❌ 不要登录远端账号（GitHub / k8s / 云厂商 / 注册表），不要改 CI 凭证 / secrets。
- ❌ 不要把 secret / token / 密码写入 git 跟踪文件或日志。
- ❌ 不要 `kubectl apply` 到生产或 `docker push` 到 registry；只可操作本地或用户明确指定的 dev 环境。

# 开发者上手指南 — MMO Server

更新：2026-10-15。本文说明本地启动入口与常用排查路径；服务清单、端口和编译要求统一以 [根 README](../../README_zh.md) 为准。

## 1. 准备环境与产物

- Windows C++ 构建使用 Visual Studio 2026 / v145，MSBuild 串行 `/m:1`。
- Go 使用 1.27.2 或更新版本；Java 网关要求 JDK 23；维护脚本使用 PowerShell 7。
- 本地基础设施使用 [deploy/docker-compose.yml](../../deploy/docker-compose.yml)，不使用仓库根不存在的 compose 文件。
- 子模块、服务配置和编译产物准备方法见 [根 README](../../README_zh.md#快速开始)。启动脚本不会代编译缺失的产物。

以下命令从仓库根执行：

```powershell
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build

Push-Location java/gateway_node
try { mvn package } finally { Pop-Location }
```

玩家存档在地域存档库里（本机是 dev 地域 0、K=1，只有 `player_store_1000000_db`；按区分的 `zone_<n>_db` 已退役，见[总体方案 §7.2](./player-data-tidb-scaling-plan.md)）。平时不用手工建：`start_game.ps1` 每次启动都会先建库、建表、给每个实例发写资格（`sql-boot-catalog-local`）。常驻的 db 进程不建库也不改表，要单独跑迁移时用迁移入口，`-storage-id` 必填：

```powershell
Push-Location go/db
try {
    $env:DB_ALLOWED_DATABASES = 'player_store_1000000_db'
    go run ./cmd/migrate -f etc/db.yaml -storage-id 1000000 -command up -create-database
} finally { Pop-Location }
```

## 2. 启动本地整套服务

```powershell
# 只检查运行环境与产物
pwsh -File tools/scripts/start_game.ps1 -CheckOnly

# 启动基础设施、Go 服务、网关与一区 C++ 节点
./start-server.cmd
```

[start_game.ps1](../../tools/scripts/start_game.ps1) 根据现有产物与前置检查启动服务，包含 `client_rpc_router`；friend / trade 等可选服务会按其库和产物是否就绪决定是否启动。网关入口为 `http://127.0.0.1:8081`。Sa-Token 桥接是可选鉴权渠道，不是所有登录路径的必需前置。

开发密码认证只面向显式允许的本地账号前缀，服务端与 robot 的共享密钥必须一致。可用环境变量 `LOGIN_DEV_PASSWORD_SHARED_SECRET` 注入，真实值不写入版本库或日志；本地启动脚本当前也兼容从 robot 配置读取已有开发密码。生产密码认证与第三方渠道有独立配置，见 [认证设计](./auth-provider-framework.md)。

需要机器人冒烟时，按 [robot README](../../robot/README.md) 选择场景、账号与认证配置。登录压测必须先满足根 [AGENTS.md §6](../../AGENTS.md#6-压测纪律最重要) 的清理、基线、采样和复盘要求，不能把默认 `dev.bat robot` 当作无前置的登录验证。

## 3. 日常操作与日志

```powershell
dev.bat help
dev.bat status
dev.bat logs
dev.bat stop
```

日志目录为 `run/logs/`：一键启动记录在 `game-launcher/<时间>/`，服务日志在 `go_services/`、`cpp_nodes/` 等子目录，见 [运行目录说明](../ops/run-directory.md)。入口失败时按启动窗口给出的原因与日志定位，不依赖固定“十秒就绪”或固定容器数量。

## 4. 构建与测试入口

```powershell
# C++
pwsh -File tools/scripts/run_cpp_tests.ps1 -Build

# Go：逐 module 执行；下面只覆盖 login
Push-Location go/login
try { go test ./... } finally { Pop-Location }

# Java 网关
Push-Location java/gateway_node
try { mvn test } finally { Pop-Location }
```

`go/test.bat` 是特定 L1 子集入口，范围以 `go/test.ps1` 的 `L1Suites` 为准；没有统一的 `dev.bat test` 全语言全量入口。测试数量与耗时以当次输出为准。

## 5. 修改契约或配置表

- 协议只改根 `proto/`，完整重生成按 [AGENTS.md §4](../../AGENTS.md#4-proto-同步流程)。`go/build.bat` 只处理 db/login go-zero 包装，不能代替完整生成。
- 默认协议生成配置会写同级客户端仓。没有客户端任务授权时，使用 `tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
- 配置表的类型、owner、键、索引与外键写在 `data/schema/*.proto`；xlsx 只写列名和数据，操作见 [data/AGENTS.md](../../data/AGENTS.md)。
- 新客户端 RPC 的路由与消息号规则见 [客户端 RPC 路由服](./client-rpc-router.md) 和 [Gate/Login 边界](./gate-login-rpc-boundary.md)。

## 6. 理解运行时链路

```text
客户端 HTTP → Java Gateway → gRPC login
客户端 TCP  → C++ gate → gRPC client_rpc_router → Go 业务服务
                         → muduo TCP RPC → 同区 C++ scene
客户端 TCP  → C++ battle（参战或观战时的第二条连接）
Go 控制命令 → Kafka 节点命令 topic → gate / scene
```

本地启动与 Kubernetes 部署默认启用路由服模式；C++ 进程在未设置开关时仍保留直连回退。Gate 断线通知走 Login，再由 Login 更新 Locator；重新握手和登录支持迁到其他 Gate。详见 [架构总览](./ARCH.md) 与 [登录流程](./player_login_flow.md)。

## 7. 接手顺序与故障入口

按根 [AGENTS.md](../../AGENTS.md) 完成启动门禁，再读 [ARCH.md](./ARCH.md)、相关服务设计和最新 [PROGRESS.md](../PROGRESS.md)。`CLAUDE.md` 只导入规范，不单独维护项目记忆。

- 配置和服务发现：先核对目标服务的 `etc/`、etcd 注册与 zone。
- Gate 连不上下游：核对路由模式及 `client_rpc_router` 是否就绪。
- 端口耗尽或登录长尾：看 [Gate 内核调优](../ops/gate-kernel-tuning-runbook.md)。
- Kubernetes 对外开服：看 [生产开服手册](../ops/k8s-open-server-runbook_zh.md)，不要复用本地启动参数。

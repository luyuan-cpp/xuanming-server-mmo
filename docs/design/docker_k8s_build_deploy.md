# Docker & K8s Build/Deploy Guide (Windows)

**Updated:** 2026-03-27

## Overview

三类 Docker 镜像，均可在 Windows 上通过 Docker Desktop（Linux containers 模式）构建。

| 类型 | Dockerfile | Build Context | 镜像名 |
|------|-----------|---------------|--------|
| C++ 节点 (gate+scene) | `deploy/k8s/Dockerfile.cpp` | 仓库根目录 | `mmorpg-node` |
| Go 服务 (5个) | `deploy/k8s/Dockerfile.go-svc` | `go/` | `mmorpg-{service}` |
| Java Gateway | `deploy/k8s/Dockerfile.java-svc` | `java/gateway_node/` | `mmorpg-gateway` |

## C++ 节点镜像 — 多阶段构建

```
Stage 1 (deps)    : gcc:13 + setup_dependencies.sh
                    → 编译 gRPC, muduo, hiredis, librdkafka, yaml-cpp, zlib
Stage 2 (builder) : vcxproj2cmake.py → build_linux.sh --skip-deps --release
                    → 编译 14 个静态库 + gate/scene 可执行文件
Stage 3 (runtime) : ubuntu:24.04
                    → 仅复制 gate/scene 二进制 + 数据表 + zoneinfo
```

首次构建约 30–60 分钟（gRPC 从源码编译）。后续构建利用 Docker 层缓存，只要 `third_party/` 没变就不会重编依赖。

## 完整命令

### 前提条件

- 安装 Docker Desktop，开启 Linux containers 模式
- 安装 `kubectl`，配置好集群连接
- （可选）本地基础设施：`cd deploy; docker compose up -d`

### 构建镜像

```powershell
# C++ 节点 (多阶段编译，无需 Linux 环境)
docker build -f deploy/k8s/Dockerfile.cpp -t ghcr.io/luyuancpp/mmorpg-node:v1 .

# Go 服务 (5个)
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build-images `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1

# Java Auth
pwsh -File tools/scripts/dev_tools.ps1 -Command java-svc-build-image `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1
```

### 推送镜像

```powershell
docker push ghcr.io/luyuancpp/mmorpg-node:v1

pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-push-images `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1

pwsh -File tools/scripts/dev_tools.ps1 -Command java-svc-push-image `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1
```

### 部署到 K8s

```powershell
# 开单区
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName yesterday -ZoneId 101 `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v1 `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1 `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1 `
  -WaitReady

# 批量开区
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-all-up `
  -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml `
  -OpsProfile managed-cloud `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:v1 `
  -GoSvcRegistry ghcr.io/luyuancpp -GoSvcTag v1 `
  -JavaSvcRegistry ghcr.io/luyuancpp -JavaSvcTag v1 `
  -WaitReady
```

### 关区（危险操作，删除整个 namespace）

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday
```

## 替代方案：预编译二进制

如果 C++ 已在 Linux 主机/WSL 上编译好，可以跳过多阶段构建，直接使用轻量 `Dockerfile.runtime`：

```powershell
# 1. 暂存二进制到 staging 目录
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-stage-runtime `
  -BinarySourceRoot D:/linux-build/bin `
  -ZoneInfoSource bin/zoneinfo `
  -TableSource generated/tables

# 2. 构建 runtime-only 镜像（秒级，不编译）
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-build-image `
  -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
```

## 关键文件

| 文件 | 用途 |
|------|------|
| `.dockerignore` | 排除 bin/、.git、docs 等减小构建上下文 |
| `deploy/k8s/Dockerfile.cpp` | 多阶段 C++ 编译镜像 |
| `deploy/k8s/Dockerfile.runtime` | 轻量运行镜像（需预编译二进制） |
| `deploy/k8s/Dockerfile.go-svc` | Go 服务多阶段镜像 |
| `deploy/k8s/Dockerfile.java-svc` | Java Auth 多阶段镜像 |
| `tools/scripts/k8s_image.ps1` | 镜像 preflight / build / push |
| `tools/scripts/k8s_stage_runtime.ps1` | 暂存 Linux 二进制 |
| `tools/scripts/k8s_deploy.ps1` | zone-up / zone-down / zone-status |
| `deploy/k8s/zones.ops-recommended.yaml` | 推荐开区配置 |

## 注意事项

- `centre` 节点已废弃，已从 preflight/staging 检查中移除
- K8s 基础设施 (mysql/redis/kafka/etcd) 使用 `emptyDir`，仅限开发，生产需配 PVC
- Gate 暴露方式：托管云 → `LoadBalancer`；自建裸金属 → `NodePort` + 外部 L4
- 每个 zone = 1 个 K8s namespace (`mmorpg-zone-{name}`)

## 本机 Docker Desktop 与 VS Code 断点（2026-10-20）

本机完整开发栈入口为 `deploy/debug/compose.game.yml`，调试覆盖为同目录 `compose.debug.yml`。它包含 15 个常驻服务与 3 个初始化任务，使用独立网络及持久卷；不复用已有容器或历史演示数据。只启用登录、建角、主世界进场和战斗所需的服务。

（2026-10-31 补）SQL 写者接了写者授权：每个写者前有一次性注册步骤 `<服务>-sql-boot`，初始化任务相应变多，首次要多构建一个 `local/mmorpg-sql-boot` 镜像；重启写者要连注册步骤一起重建。操作见 [`tools/scripts/README.md`](../../tools/scripts/README.md) 的 prepare_docker_debug.py 一节。

在仓库根准备配置：

```powershell
python tools/scripts/prepare_docker_debug.py
docker --context desktop-linux compose --env-file build/docker-debug-current/.env -f deploy/debug/compose.game.yml -f deploy/debug/compose.debug.yml config --quiet
```

准备程序从当前十份源配置派生容器配置，随机密钥仅写入被 Git 忽略的 `build/docker-debug-current/`，再次执行保留原密钥。不要打印、上传或提交展开后的 Compose、`.env` 和带口令的派生配置。

首轮 C++ 调试镜像通过 `deploy/debug/Dockerfile` 继承同一次构建的运行镜像与 builder 镜像，增加容器内 GDB，并把对应的分离符号放到 `/app/bin/`；日常重编改用下节的 `Dockerfile.cpp` 与统一入口。三个节点通过 `SYS_PTRACE` 支持附加，不开放 SSH 或调试 TCP 端口。本节启停命令使用 `.env` 记录的本地调试镜像，重编入口会更新相应引用。各服务镜像成功构建后，在仓库根执行：

```powershell
$dc = @('--context', 'desktop-linux', 'compose',
  '--env-file', 'build/docker-debug-current/.env',
  '-f', 'deploy/debug/compose.game.yml',
  '-f', 'deploy/debug/compose.debug.yml')
docker @dc up -d --no-build --pull never --wait --wait-timeout 300
docker @dc ps --all --format json |
  ConvertFrom-Json | Select-Object Service,State,Health,ExitCode

# 日常停止，保留本轮数据卷；下次使用同一条 up 命令恢复。
docker @dc stop
```

本轮配置已经准备好，日常启停不必重跑准备程序。修改 C++ 源码后须重新构建镜像并重建对应容器，再附加调试；仅重启原容器不会更新二进制。

本机客户端默认 HTTP 入口为 `http://127.0.0.1:8081`，同一网关保留 `http://127.0.0.1:18081` 供本轮已有工具使用（该附加端口可用 `GATEWAY_PORT` 调整）。`GET /api/server-list` 正常返回 `zones` 数组；本轮可选区服为 `zone-1`。如果客户端未设置过网关地址，它使用 `8081`，因此 Docker 必须映射这个端口。配置准备程序将 `8081` 保留给 HTTP，Gate、Battle 不可使用该端口。

Gate 入口为 `127.0.0.1:28000`、Battle 为 `127.0.0.1:28010`，通过 `CLIENT_ENDPOINT_*` 发布给客户端；内部 `NODE_IP` 仍为容器地址。Scene 与 gRPC 不发布到宿主机。

本栈启用受限的开发口令认证，账号须以 `dev_` 或 `robot_` 开头，密码来自本轮 `.env` 的 `LOGIN_DEV_PASSWORD_SHARED_SECRET`。本机已验证的账号为 `dev_demo`；完整登录信息保存在忽略目录 `build/docker-debug-current/client-login-docker.txt`，密码仅在本机文件中提供。HTTP 登录须同时检查响应业务码为 `0` 且令牌存在；HTTP 200 本身也可能携带认证拒绝。新账号尚无角色属于正常结果，进入游戏还需后续选角／建角与进场。

调试多个新账号时，分别使用 `dev_test_001`、`dev_test_002` 或 `robot_000001` 等不同账号名，密码仍取同一份本机登录信息文件，无需为每个账号修改容器配置。登录后按各自账号的角色列表选角或建角；多客户端测试时使用不同账号名。

本机 `.vscode/launch.json` 提供「Docker：附加 Gate / Scene / Battle」：VS Code 的 C/C++ 扩展通过 `docker --context desktop-linux exec` 调用容器内 GDB，`sourceFileMap` 把编译路径 `/src` 映射为当前工作区。用 VS Code 打开本仓库根目录，在自己的 C++ 源码设置断点，从「运行和调试」下拉选择「Docker：附加 Scene」，按 `F5` 后选择 `scene`（PID 为 `1`）。Gate、Battle 选择各自的入口与同名进程。源码必须与镜像构建输入一致；首轮 `RelWithDebInfo` 有优化，日常断点调试使用下节的 `Debug/-O0` 重编入口。长时间断点仍会触发正常的分布式超时与租约处理。

**首轮验证状态（2026-10-20 已完成）**：Gate、Scene、Battle 的 Linux `RelWithDebInfo` 构建、运行镜像与调试镜像均成功；镜像内 1,456 个 C++ 源文件与本地逐一 SHA256 匹配。七个 Go 服务及 DB 迁移工具共八个 Linux 镜像、Java 网关镜像均已构建。最终调试配置下 15 个常驻服务运行，11 个带健康检查的服务均为 healthy，3 个初始化任务正常退出；网关健康接口为 UP，HTTP 登录返回业务码 0。

三个节点通过与 VS Code 相同的 Docker/GDB MI 管道完成附加和解除附加；Scene 实际命中本仓 `cpp/libs/engine/core/node/system/node/node.cpp:159`，随后恢复运行，三个节点的 `TracerPid` 均为 0。切换到最终调试镜像后，双玩家功能冒烟再次退出 0，登录、进场、战斗与观战通过，战斗直连和观战直连各收到 9 个回合结果。本轮验证了 GDB 管道与真实源码断点，没有操作 VS Code 界面按 F5。

负责人已授权补齐 `data_service` 的分页围栏接口；对账包 73 项测试、`go vet`、真实 Redis 的 6 项围栏测试通过且无跳过。Java 网关 Maven 测试 205 项通过、5 项数据库条件测试跳过、无失败。首轮构建失败及本机 gRPC 精确快照适配原件均保留；负责人收紧范围后未再运行或排查第三方测试。证据位于 `build/verification/docker-debug-20261020-052903/`，最终结果见 `cpp-runtime/package-result.json`、`cpp-debug-up-result.json` 和 `smoke/` 中的源码匹配、GDB、功能冒烟记录。


## 修改后重新构建与多语言断点（本机开发）

编辑、保存代码后，先在 VS Code 断开该服务的调试连接，再从仓库根运行对应命令。构建成功后入口会把本轮独占镜像引用写入忽略目录 `.env` 的非敏感镜像键，只重建指定服务，最后检查运行产物与当前源码是否一致；失败保留原始构建证据，不自动换旧镜像宣称成功。不会清空数据卷、账号或更换开发密码。

修改服务 `etc` 配置或网关 `application.yaml` 时，先按本轮原端口参数重新运行 `prepare_docker_debug.py`，再重编对应服务；准备程序保留现有随机密钥与镜像引用。检查会拒绝尚未按当前源配置重新派生的状态；prepare 本身不会重启节点，尤其 C++ YAML 已在启动时读入内存，不能跳过后续重编与容器重建。Go 镜像同时冻结当前服务的 `data*` 资源与 `generated/tables`，表文件在容器内按小写名称落盘并逐一核对摘要。

```powershell
# C++ 共享库与三个节点一起编译、更新；首次无优化构建较慢。
python tools/scripts/rebuild_docker_debug.py cpp

# 只更新 Go 登录服；共享 Go 模块修改后应列出全部受影响服务。
python tools/scripts/rebuild_docker_debug.py go --services login
python tools/scripts/rebuild_docker_debug.py go --services login,db,data-service,scene-manager,player-locator,client-rpc-router,match

# Java 网关：离线 Maven 测试、重新打包并更新容器。
python tools/scripts/rebuild_docker_debug.py java

# 附加前也可手动核对源码及二进制、符号。
python tools/scripts/rebuild_docker_debug.py check --services scene
```

VS Code 的“终端 → 运行任务”中已有同名“Docker：重编并更新 C++ 节点 / Go Login / 全部 Go 服务 / Java 网关”。构建完成后回到“运行和调试”，选下表配置，按 F5。C++ 仍选择对应节点进程 PID 1；Go、Java 直接连接各自的调试端口。所有端口只发布到宿主 `127.0.0.1`。

| 服务 | VS Code 配置 | 本机调试连接 |
|---|---|---|
| Gate / Scene / Battle | Docker：附加 Gate / Scene / Battle | docker exec → 容器内 GDB |
| login | Docker：附加 Go login | 2345 |
| db | Docker：附加 Go db | 2346 |
| data-service | Docker：附加 Go data-service | 2347 |
| scene-manager | Docker：附加 Go scene-manager | 2348 |
| player-locator | Docker：附加 Go player-locator | 2349 |
| client-rpc-router | Docker：附加 Go client-rpc-router | 2350 |
| match | Docker：附加 Go match | 2351 |
| gateway | Docker：附加 Java Gateway | 5005 |

C++ 新入口用实际 `Debug` 构建，并核 core 与三个节点的最终编译参数；原 `debug-20261020` 镜像的应用源码虽然与本地一致，参数仍包含后置 `-O2`，会出现跳行和局部变量不可见。新的节点、分离符号、SDK 共享库与源码清单来自同批构建。对象缓存按精确 SDK 与 Debug 隔离；SDK 源码或锁定输入变化时须先更新 SDK，不能混用旧依赖。

Go 使用 `-gcflags=all=-N -l`，保留 DWARF 与宿主源码路径，通过 `substitutePath` 对应当前工作区；Delve 在容器中运行，VS Code 不需要另外安装宿主 Delve。Java 保留行号与调试信息，使用现有 JDK 23；工作区推荐 C/C++、Go、Java 语言支持及 Debugger for Java 扩展。各调试配置都有源码检查前置任务，代码保存后尚未重新构建时先执行重编任务，再附加。

可复现的触发点：Scene 的 `SceneMovementClientPlayerHandler::MoveStart`（角色重新开始移动）、Go 登录服的 `DevelopmentPasswordProvider.ValidatePassword`（点击登录）、Java `ServerListController.getServerList`（刷新区服列表）。断点暂停节点或请求线程期间，正常的客户端超时与服务租约仍会计时。

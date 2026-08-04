# 开发者上手指南 — MMO Server

**Date:** 2026-05-09
**目的:** 让新人 / AI 在不到 5 分钟内把整套服务跑起来,看见机器人成功登录。

> 如果你只想了解架构而不跑起来,先看 [ARCH.md](./ARCH.md)。
> 这篇是"按下哪个按钮"。

---

## 0. 快速对照:目录与运行时角色

```
┌─ java/                         JVM 服务
│  ├─ gateway_node/              ★ HTTP 门户(限流 / /api/login / /api/assign-gate / 公告)  :8081
│  └─ springboot_satoken_auth_starter/   Sa-Token 认证桥接                                   :18080
│
├─ go/                           Go 微服务(go-zero gRPC)
│  ├─ login/                     ★ 登录 / EnterGame / 双 token / AssignGate                  :50000
│  ├─ scene_manager/             场景路由 + 负载均衡
│  ├─ player_locator/            会话权威源 (Redis ZSET)
│  ├─ data_service/              玩家数据持久化
│  ├─ chat / friend / guild      社交服务
│  └─ db/                        DB 写入 / 数据恢复
│
├─ cpp/                          C++ 长连接 / ECS
│  ├─ nodes/gate/                ★ 玩家 TCP 长连接 + 协议路由                                :9000+
│  └─ nodes/scene/               战斗 ECS 节点
│
├─ robot/                        压测 / 机器人客户端
└─ infra (docker-compose)        redis(6379) etcd(2379) mysql(3306) kafka(9092)
```

★ 是登录链路上的核心服务;先跑这三个加 infra 就能进游戏。

---

## 1. 一次性环境准备

### 1.1 必装

| 工具 | 检查命令 | 说明 |
|---|---|---|
| Visual Studio 2022/2026 + C++ 工作负载 | `vswhere -latest` | cpp gate / scene 编译 |
| Go ≥ 1.24 | `go version` | go 服务 + robot |
| JDK ≥ 23 | `java --version` | Spring Boot 3.4 / Java Gateway |
| Docker Desktop | `docker ps` | redis / etcd / mysql / kafka |
| Maven(可选) | `mvn -v` | gateway 用,没装会用项目缓存的 wrapper |

### 1.2 一次性启 infra

```bash
# 在仓库根
docker compose up -d        # 启 redis / etcd / mysql / kafka(按 docker-compose.yml)
docker ps                   # 验证 6 个容器全 Up
```

期望端口:`6379 / 2379 / 3306 / 9092` 都 LISTENING。

---

## 2. 跑起来(最小可登录路径)

### 2.1 编译

```bash
dev.bat build               # 编 cpp(MSBuild)+ go(全部 go-svc)
```

第一次编译 ~5–10 分钟,之后增量。

### 2.2 启服(新路径,推荐)

```bash
dev.bat infra               # 确认 redis/etcd/kafka 在跑(已在 docker 里跑就跳过)
dev.bat start-satoken       # Sa-Token 认证服(:18080)
dev.bat start               # 一键起 cpp gate×2 + scene×4 + 全套 go 服务
                            #   含 go-zero login.rpc(:50000)
                            #   注册到 etcd
                            # 等约 10 秒看到 "ready" 日志
cd java/gateway_node
mvn spring-boot:run         # 启 Java Gateway(:8081)
                            # 默认配置: gate.rate-limit.enabled=false(行为同改前)
```

### 2.3 跑机器人

```bash
# robot/etc/robot.yaml 关键开关
# use_http_login: true              # 走新 /api/login 路径(推荐)
# auth_type: "password"             # 仅配合 login.DevPasswordAuth 的 robot_ 前缀账号
# 生产 password 已实现但默认关闭；启用前须完成 MySQL schema/Argon2id 迁移并从环境变量注入只读 DSN

# 本地启用 DevPasswordAuth 时，共享密钥只放环境变量，不写入 YAML：
# $env:LOGIN_DEV_PASSWORD_SHARED_SECRET='<local-only-secret>'

dev.bat robot               # 默认 1000 机器人
# 或单跑:
cd robot && go run . -c etc/robot.yaml
```

成功登录的 STDOUT 长这样:
```
{"level":"info","msg":"entered game","account":"robot_0001","player_id":...}
{"level":"info","msg":"received auth tokens",...}
```

---

## 3. 关键端口速查

| 端口 | 服务 | 协议 | 由谁连 |
|---|---|---|---|
| 8081 | Java Gateway | HTTP | 客户端 / robot |
| 18080 | Sa-Token | HTTP | Gateway / robot |
| 50000 | go-zero login.rpc | gRPC | Gateway / cpp gate |
| 9000+ | cpp gate | TCP(自定义) | 客户端 / robot |
| 6379 | Redis | RESP | 全员 |
| 2379 | etcd | gRPC | 服务发现 |
| 3306 | MySQL | mysql | data_service / login |
| 9092 | Kafka | kafka | gate / login / scene_manager |

---

## 4. 看日志 / 状态

```bash
dev.bat status              # 看哪些进程在跑
dev.bat logs                # 滚动 tail go / cpp / sa-token 日志
                            # 路径: run/logs/{go_services,cpp_nodes,sa_token.log}
```

关键日志关键词:
- `entered game` — 玩家进游戏成功
- `[DEPRECATION]` — 老 gate Login RPC 路径被命中(throttled,1 分钟一条)
- `BindSession` / `RoutePlayer` — Kafka 命令收到
- `ListenOverflows` / `Bound` — 撞 ephemeral port / accept queue 上限

---

## 5. 跑测试

### Java Gateway
```bash
cd java/gateway_node
mvn test                    # 34 个测试,~30 秒
# 或单类:
mvn -Dtest=LoginEndpointIntegrationTest test
mvn -Dtest=AssignGateRateLimiterTest test
```

### Go(go-zero login)
```bash
cd go/login && go test ./...
```

### Robot 单元测试
```bash
cd robot && go test ./...
```

### 全量
```bash
dev.bat test                # 跑 go 全 + cpp 测试 + Java 测试
```

---

## 6. 加新东西的标准动作

### 6.1 加一个客户端 RPC(玩家在游戏内调)
1. `proto/<域>/<file>.proto` 加 message + rpc,service 标 `option (OptionIsClientProtocolService) = true`
2. `dev.bat proto` 重新生成 pb 代码(C++/Go/Java 三端)
3. 选实现端:
   - Go 服务里实现 → cpp gate `HandleGrpcNodeMessage` 自动转发
   - cpp scene 里实现 → cpp gate `HandleTcpNodeMessage` 自动转发
4. 客户端按 message_id 发包
5. 详细规则见 [`gate-login-rpc-boundary.md`](./gate-login-rpc-boundary.md) §一

### 6.2 加一个第三方登录渠道(如 Apple Sign-in)
4 步,模板: [`auth-provider-framework.md`](./auth-provider-framework.md) §How to Add
1. `go/login/internal/config/config.go` 加 `AppleAuthConf` 结构
2. `go/login/internal/logic/pkg/auth/providers.go` 加 `AppleProvider` + `Validate`
3. `go/login/internal/svc/auth_init.go` 注册 `auth.Register("apple", ...)`
4. `go/login/etc/login.yaml` 加配置段
**不用动 cpp gate / Java Gateway / 客户端协议。**

### 6.3 调登录限流参数
`java/gateway_node/src/main/resources/application.yaml` 的 `gate.rate-limit.*`
设计文档: [`open-server-rate-limit-design.md`](./open-server-rate-limit-design.md)

### 6.4 改 sysctl(gate 机器)
`/etc/sysctl.d/99-gate.conf` + `sysctl --system`
模板 + 排查 SOP: [`docs/ops/gate-kernel-tuning-runbook.md`](../ops/gate-kernel-tuning-runbook.md)

---

## 7. 常见错误(踩过就知道)

| 症状 | 原因 | 修复 |
|---|---|---|
| robot 启动 `proto/proto/scene is not in std` | vendor 过期 | `cd robot && go mod vendor` |
| Gateway 起不来 `Cannot resolve dependencies bucket4j_jdk17-core` | pom 依赖名写错过(已修),确认 artifactId 是 `bucket4j-core` / `bucket4j-redis` | 拉最新 pom.xml |
| robot 报 `connect: cannot assign requested address` | gate 机器 ephemeral port 耗尽 | 先看 [`gate-kernel-tuning-runbook.md`](../ops/gate-kernel-tuning-runbook.md) §五 SOP |
| login 长尾 25s | 同上 | 同上(不是 gate 内部瓶颈) |
| `[DEPRECATION] ClientPlayerLogin.Login called via legacy cpp gate path` | robot 没开 `use_http_login` | `robot/etc/robot.yaml` 加 `use_http_login: true` |
| Gateway `mvn test` 卡 13s | LoginEndpointIntegrationTest 起 SpringBootContext,正常 | — |
| cpp gate 编译过但 copy `bin/gate.exe` 失败 | 有 gate 进程在跑占着文件 | `dev.bat stop` 再 build |
| etcd 起着但服务找不到对方 | host 名称不一致(机器名 / IP)| 检查 `bin/etc/base_deploy_config.yaml` 的 `etcd_hosts` |
| 顶号没踢成功 | `is-concurrent` / `MaxLoginDevices` 配置 | `go/login/etc/login.yaml` `Account.MaxDevicesPerAccount` |

---

## 8. mental model(记不住时回这里)

```
玩家 ──HTTP──→ Java Gateway ──gRPC──→ go-zero login          (登录前)
玩家 ──TCP─────→ cpp gate    ──gRPC──→ go-zero login          (游戏内的 login 类 RPC: EnterGame/LeaveGame/CreatePlayer)
                              ──TCP──→ cpp scene             (战斗)
                              ──gRPC──→ go chat/friend/guild (社交)
```

- **Java Gateway 管"还没进游戏"**(限流 / OAuth / 削峰)
- **cpp gate 管"已经在游戏 TCP 上"**(协议路由器,不只为登录)
- **go-zero login 是会话权威**(无状态,etcd 注册)
- **player_locator 是会话事实源**(Redis ZSET,30s lease)
- **muduo / proto 框架代码不动**

---

## 9. 我接手第一天该读什么

1. [CLAUDE.md](../../CLAUDE.md) — 项目记忆 5 分钟版(最先读)
2. [ARCH.md](./ARCH.md) — 总架构 + 决策表
3. [player_login_flow.md](./player_login_flow.md) — 登录/重连/顶号事件流(权威)
4. [gate-login-rpc-boundary.md](./gate-login-rpc-boundary.md) — 为什么 gate 还要连 login
5. [architecture-current-state-vs-gaps-2026-05.md](./architecture-current-state-vs-gaps-2026-05.md) — 当前已知缺口
6. 本文档 — 跑起来

---

## 10. 帮助 / 问题反馈

- 日志在 `run/logs/`,出问题先 grep `ERROR` 和 `FATAL`
- Kafka 看板: <http://localhost:8080>(kafka-ui 容器)
- Nacos(可选): <http://localhost:8848>
- 提 issue / PR 时附上:`dev.bat status` 输出 + 复现步骤 + 关键日志片段

---

更新日期 2026-05-09。改动需在 [ARCH.md §11 决策表](./ARCH.md#11-关键决策摘要) 同步。

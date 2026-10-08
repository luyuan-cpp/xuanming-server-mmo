# java/ —— Spring Boot 服务

| 目录 | 端口 | 职责 | 状态 |
|------|------|------|------|
| `gateway_node/` | HTTP 8081 | 客户端的第一个入口:区服列表、分配 gate、公告、管理接口 | 本地一键启动与 Kubernetes 部署都会用到 |
| `config_node/` | HTTP 8090 | 配置表查询服务,导表器 Java 产物的落点 | 只在导表器的 CI 里编译,日常不启动 |
| `springboot_satoken_auth_starter/` | — | Sa-Token 认证的示例应用 | 仅 `dev.bat start-satoken` 与 robot 的 satoken 登录方式使用 |

## gateway_node

客户端登录前只和它说话:

1. `GET /api/server-list` —— 拿区服列表与各区状态
2. `POST /api/assign-gate` —— 选区后要一台 gate;网关经 gRPC 调 `login`,返回 gate 地址与 token
3. 客户端用拿到的地址与 token 直连 gate,之后不再经过网关

区服状态有两个来源:管理接口写进 MySQL 的人工状态,以及对 etcd、Redis 的自动探测;人工状态优先。
管理接口(`/admin/**`)需要 `X-Admin-Key` 请求头。

```powershell
cd java/gateway_node
mvn package        # 打包
mvn test           # 测试
```

需要 JDK 23。编码约定与各接口的代码位置见 [AGENTS.md](AGENTS.md)。

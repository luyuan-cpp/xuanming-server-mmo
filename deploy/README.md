# deploy/ —— 部署

本地开发用 Docker Compose 起基础设施,线上用 Kubernetes。

## 本地:Docker Compose

| 文件 | 起什么 |
|------|--------|
| `docker-compose.yml` | 基础设施:etcd、Redis、MySQL、Kafka(带 kafka-ui);`redis-cluster` profile 另起一套 Redis Cluster |
| `docker-compose.observability.yml` | 日志查看:Loki、Alloy、Grafana(`dev.bat obs`) |
| `docker-compose.tidb.yml` | 单节点 TiDB,与 MySQL 并存,用于验证 TiDB 兼容性 |
| `docker-compose.login-stack.yml` | 登录链路的 Linux 容器版:第三方登录 mock、login、网关 |

```powershell
dev.bat infra         # 起基础设施
dev.bat infra-down    # 停
```

配套目录:

| 目录 | 内容 |
|------|------|
| `mysql-init/` | MySQL 首次初始化时执行的建库脚本 |
| `tidb-config/` | TiDB 配置 |
| `env/` | Kafka 的开发档与类生产档参数 |
| `observability/` | Loki、Alloy、Grafana 的配置 |
| `login-stack.linux/` | `docker-compose.login-stack.yml` 用的 login 配置 |

## 线上:Kubernetes

```
k8s/
├── manifests/
│   ├── infra/        etcd、Redis、MySQL、Kafka 等基础设施
│   ├── go-svc/       各 Go 服务
│   └── java-svc/     网关
├── Dockerfile.*      各类镜像:cpp(编译)、runtime(C++ 节点运行时)、go-svc、java-svc、robot
├── runtime/          C++ 节点运行时镜像的暂存目录
├── zones.*           多区定义的样例
└── *-alerts.yaml、*-dashboard.json   告警规则与监控面板
```

部署不直接 `kubectl apply`,而是通过脚本入口:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-up
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ZoneName <name> -ZoneId <id> -WaitReady
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-status -ZoneName <name>
```

完整用法见 [k8s/README.md](k8s/README.md),开服操作步骤见
[docs/ops/k8s-open-server-runbook.md](../docs/ops/k8s-open-server-runbook.md),
发布打包规范见 [docs/design/release-packaging-standard-20260914.md](../docs/design/release-packaging-standard-20260914.md)。

# 任务 P1-DEPLOY：观战中继 / 广播的部署配置、密钥注入与建 topic

先读同目录 `00-common-rules.md`。权威契约：`docs/design/battle-spectate-tiered-fanout.md` §15.3、§15.6、§15.7。
新增配置：C++ `BaseDeployConfig.spectate_token_secret`（字段 24，gate 读）；Go match 的 `Spectate` 段（字段名以 P1-MATCH 任务为准：
`RelayEnabled`、`BroadcastMaxConcurrent`、`BroadcastAutoGateThreshold`、`TicketTTLSeconds`、`TicketSecret`、`OpsCallerSecret`）。
新 topic：`gate-spectate_g<N>`（分区数 = 命令 topic 分区数，保留 1 小时，同 gate-cmd）、`spectate-bcast_g<N>`（16 分区，保留 10 分钟），
代号与命令 topic 同源。

## 允许改的文件
- `bin/etc/base_deploy_config.yaml`（填 dev 值：至少 32 字节、与 gate / battle 令牌 dev 值不同）
- `go/match/etc/match_service.yaml`（新增 `Spectate` 段，dev 值；`TicketSecret` 与上面同值；`OpsCallerSecret` dev 值）
- `deploy/docker-compose.yml` 的 kafka-topic-init（`create_topic` / `assert_partitions` / `ensure_retention`，照 gate-cmd 写法）
- `deploy/k8s/manifests/infra/kafka-topic-init.yaml` 与 `tools/scripts/k8s_deploy.ps1`：
  - 两条新 topic 的创建与分区断言（照 `GATE_COMMAND_TOPIC` 的注入写法加占位符）；
  - 新密钥 `MMORPG_SPECTATE_TOKEN_SECRET`：用 `Resolve-InjectedSecret` 注入 gate 的 BaseDeployConfig 与 match 配置的 `Spectate.TicketSecret`，
    校验「去除首尾空白后至少 32 字节、与 `MMORPG_GATE_TOKEN_SECRET` 和 `MMORPG_BATTLE_TOKEN_SECRET` 都不同」，口径照抄 battle 令牌那段；
  - `MMORPG_MATCH_OPS_CALLER_SECRET`：注入 match 的 `Spectate.OpsCallerSecret`（可空；空 = 运营入口关闭），非空时至少 32 字节；
  - `Spectate.RelayEnabled` 的 K8s 默认值：单 Kafka 集群部署为 true（与现有 Kafka 拓扑一致），并在脚本注释里写明 Q6
- `tools/scripts/start_game.ps1` 等本地启动脚本：若它们生成或注入上述配置 / topic，同步补齐
- 契约测试：`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 补新 topic 与新密钥的断言（照现有 battle 令牌与 gate-cmd 的用例）

## 要做的事
按上面逐项补齐，保持与现有写法完全同形（占位符命名、错误文案语言、校验顺序）。不要改与本任务无关的段落；
`k8s_deploy.ps1` 很大且有别人的改动，按函数 / 段落外科编辑，改完用 Grep 自查同名函数没有出现两份定义。
交付说明里给 Codex：`pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（或仓库现有的 Pester 调用方式）与本地
`docker compose -f deploy/docker-compose.yml up kafka-topic-init` 的核对命令及期望（两条 topic 存在、分区数正确）。

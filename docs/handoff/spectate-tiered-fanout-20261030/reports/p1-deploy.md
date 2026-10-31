# P1-DEPLOY 交付说明(未跑任何测试,待 Codex 验证)

只做过静态解析:ps1 过 `Parser::ParseFile`(0 错误、无重名函数),py 与 yaml 能解析。

## 必须先处理(不在本任务可改范围)

1. **阻断**:`cpp/libs/engine/config/config.cpp` 的 `readBaseDeployConfig` 按键名逐个读,没有 `SpectateTokenSecret`,P1-GATE 的允许列表也没有这个文件。不补则 gate 读到空密钥,K8s 上(gate 按 prod 跑)「空值拒启」落地后全部 CrashLoop。紧跟 `BattleTokenSecret` 那段加:
   `if (root["SpectateTokenSecret"]) { baseConfig.set_spectate_token_secret(root["SpectateTokenSecret"].as<std::string>()); }`
2. `tools/scripts/tests/k8s_client_entry_contract.tests.ps1:1618` 的 `$ProdSecretEnv` 加 `MMORPG_SPECTATE_TOKEN_SECRET = 'contract-ce-spectate-token-0123456789abcdef'`,否则 1653 行用例必红。

## 改动文件

- `bin/etc/base_deploy_config.yaml`:172 `SpectateTokenSecret` dev 值。
- `go/match/etc/match_service.yaml`:211 `Spectate` 段,`TicketSecret` 与上同值。
- `deploy/docker-compose.yml`:204-209 两条 topic。
- `deploy/k8s/manifests/infra/kafka-topic-init.yaml`:96-101 占位,202-203 `ensure_topic`。
- `tools/scripts/k8s_deploy.ps1`:884-996 新函数 `Assert-SpectateSecretShape`、`Resolve-SpectateTokenSecret`、`Resolve-MatchOpsCallerSecret`、`New-MatchSpectateYaml`(`RelayEnabled` 写 true,注释写明 Q6);1170 `Initialize-InjectedSecrets` 在写操作前判;6443 node-config 写密钥,battle-node-config 不写;9721、10816 match 模板;13283 起 `Apply-KafkaTopicInitJob`。
- `tools/scripts/start_game.ps1`:173-174 核对清单。
- `tools/scripts/prepare_docker_debug.py`:docker 调试栈建 topic、注入两把密钥(旧 `.env` 缺这两把时补生成)。
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`:1191 `$ProdEnv`;4970-5255 新增用例。

## 新增用例(14 条,第 4997 行起)

topic:C++ 常量核对、K8s Job、本地三处清单。配置:dev 值跨文件一致、node-config、match ConfigMap、`New-MatchSpectateYaml`。校验:上述三个校验函数。负向:prod 缺密钥、与 gate / battle 相同、只部署 infra 不要求、运营密钥过短。

## 给 Codex(仓库根,按顺序)

1. 先处理上面两项。
2. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:14 条全绿。静态看 `$guildInfraArgs` 缺 `-PhysicalRegion`,复用 `$guildInfraRun` 的旧用例可能本来就红,与本任务无关,如实记录。
3. 同目录 `lobby_authority_render.tests.ps1`、`k8s_client_entry_contract.tests.ps1`、`start_game_command_contract.tests.ps1`:绿。
4. `go/match` 下 `go test ./internal/config/... -count=1`:仓内 yaml 能加载。
5. Compose(缺省代号是 g1,要显式给 g2):
   ```powershell
   $env:KAFKA_INIT_COMMAND_TOPIC_GENERATION='2'; $env:KAFKA_INIT_COMMAND_PARTITIONS='256'
   docker compose -f deploy/docker-compose.yml up kafka-topic-init
   ```
   通过:退出 0;`kafka-topics.sh --describe` 看 `gate-spectate_g2` 分区 256、`spectate-bcast_g2` 分区 16;`kafka-configs.sh --describe` 看 `retention.ms` 分别 3600000、600000。

## 风险与待拍板

- **比规格更严(有意)**:密钥含空白、双引号、反斜杠一律拒绝(Go 签 C++ 验,值要嵌进 YAML 双引号);非 dev 档注入 dev 回落值按占位拒绝;运营密钥不得与凭证密钥相同。
- **Q6 未定**:`RelayEnabled` 按现有单 Kafka 写死 true。若 `-PhysicalRegion` 将来对应多 Kafka 集群,必须改 false,否则观众拿到凭证却收不到帧。
- **待拍板:中继保留期**:按规格 1 小时,帧没人回放却全留着。假设 1000 场中继、每场 4 台 gate、3 秒一回合、每帧 1.5 KB,约 6.5 GiB / 小时(未实测),超过 `KafkaOtherTopicsReserveGi`(1 / 2 / 4 GiB)。建议降到 10 分钟。
- **轮换不自动滚动**:同 gate / battle 令牌,Pod 模板没有指纹(`match.yaml` 不在允许范围)。换密钥要手动滚 gate 与 match,窗口内订阅被拒。
- **运营工具**:开 `-NetworkPolicies on` 时要在 match 准入名单内的 Pod 里跑。
- **文档未改(不在允许范围)**:`deploy/k8s/README.md`、`deploy/k8s/AGENTS.md`、`docs/ops/k8s-open-server-runbook*.md:12`、`docs/design/kafka-topic-retention-strategy.md:21`、设计 §15.7(补运营密钥)。

## Java 版

待做,随 Java P1 / P2 批:内部通道走 Redis pub/sub,不需要这两条 topic,需要等价的凭证密钥配置。PARITY 未登记。

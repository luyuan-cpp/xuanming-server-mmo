# 配置表热更与表包分发运维手册

> **设计依据**：[docs/design/config-table-hot-reload.md](../design/config-table-hot-reload.md)
> **告警规则**：[deploy/k8s/table-bundle-alerts.yaml](../../deploy/k8s/table-bundle-alerts.yaml)；sidecar 指标的抓取：[deploy/k8s/table-sync-podmonitor.yaml](../../deploy/k8s/table-sync-podmonitor.yaml)（两份一起装，见 §3）

## 1. 架构与日常发布流程

表包按内容摘要不可变存放在对象存储（`tables/<schema_digest>/<content_digest>.tar.gz` + `.sha256`），etcd 只存轨道指针（`/mmorpg/tables/pointer/<track>/<schema_digest>`）和带 30 秒租约的 Pod 回报（`/mmorpg/tables/status/<track>/<namespace>/<pod>`；带 namespace 是因为各 zone 的 StatefulSet 同名，设计 §6.1）。回报只在状态变化时写，平时只续租。

标准发布顺序：

1. **导表并提交**：在仓库运行导表（`dev.bat gen`），确认 `generated/tables/manifest.json` 的 `schema_version == 2` 且 `schema_digest` 与线上二进制一致。
2. **打包与上传**（带对象存储写凭据的发布机）：
   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command table-bundle -TableBundleArgs @("pack", "-dir", "generated/tables", "-out", "run/table_bundles/out")
   pwsh -File tools/scripts/dev_tools.ps1 -Command table-bundle -TableBundleArgs @("upload", "-dir", "generated/tables", "-s3-endpoint", "<endpoint>", "-s3-bucket", "mmorpg-tables")
   ```
   对象存储同名键已存在且 `.sha256` 相同时跳过，不同则拒绝覆盖。
3. **二进制预检**（设计 §5.4 第 2 道，用与线上同一版本的各服务二进制跑 `--validate-tables`）：
   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command table-bundle -TableBundleArgs @("verify", "-dir", "generated/tables", "-bins", "scene,gate,battle,match,guild,login,scene_manager,mail,rank")
   ```
4. **先发 canary、再发 stable**：
   ```powershell
   # 发到 canary 轨道(缺省 65 秒后全服统一切换;到点前任一 Pod rejected 自动撤回 next)
   pwsh -File tools/scripts/dev_tools.ps1 -Command table-bundle -TableBundleArgs @(
     "publish", "-admin-url", "https://<gateway>", "-admin-key", "<key>",
     "-track", "canary", "-schema-digest", "<16hex>", "-content-digest", "<64hex>",
     "-bundle-sha256", "<64hex>", "-version", "<ver>", "-reason", "balance update"
   )
   # 观察指标与 GET /admin/tables 正常后,发到 stable 轨道
   pwsh -File tools/scripts/dev_tools.ps1 -Command table-bundle -TableBundleArgs @(
     "publish", "-admin-url", "https://<gateway>", "-admin-key", "<key>",
     "-track", "stable", "-schema-digest", "<16hex>", "-content-digest", "<64hex>",
     "-bundle-sha256", "<64hex>", "-version", "<ver>", "-reason", "balance update"
   )
   ```
5. **定时生效**（例如活动表凌晨五点生效）：给 `publish` 加 `-activate-at "2026-12-01T05:00:00+08:00"`。

## 2. 本机无 etcd / 无 S3 即时热更（策划与开发）

给本机启动的 C++ / Go 节点设置 `MMORPG_TABLE_BUNDLE_ROOT=run/table_bundles`（可选 `MMORPG_TABLE_STATUS_ROOT=run/table_status`），改表并导表后执行：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local
```

节点在 2 秒内自动校验并切换。

## 3. 集群部署接线

`k8s_deploy.ps1` / `dev_tools.ps1` 传 `-TableSyncS3Endpoint`（生产/预发，使用运维预置的只读 Secret `table-sync-s3`）或 `-TableSyncDevFixture`（仅 `dev`，复用 SeaweedFS 夹具建 `mmorpg-tables` 桶）：

- 挂载 `table-sync` 原生 sidecar 的工作负载：
  - C++：`gate`、`scene`（Deployment 与 Agones Fleet）、`battle`（Deployment、HostPort、Agones Fleet 与金丝雀池）；
  - Go：`guild`、`login`、`mail`、`match`、`rank`、`scene-manager`。
- 不挂 sidecar 的服务：`player-locator`、`trade`、`payment`、`chat`、`friend`、`data-service`、`db`、`client-rpc-router`、`player-inbox`、`content-guard`。
- **外接对象存储时，先在每个跑 sidecar 的 namespace 里建只读凭据 Secret**：infra namespace（battle 池与全局的 guild、mail、rank、match 在这里），以及每个 zone 的 namespace（gate、scene、login、scene-manager）。Secret 名缺省 `table-sync-s3`（`-TableSyncS3SecretName` 可改），键 `access-key-id`、`secret-access-key`，都不能为空。部署脚本在该 namespace 的任何工作负载之前核对它，缺了就报 `Secret table-sync-s3 does not exist in namespace …` 并停下，什么都不写。只核对键名与长度，不读值。没有这道核对时，缺 Secret 的后果是该 namespace 里所有带 sidecar 的 Pod 卡在 `CreateContainerConfigError`。
  ```powershell
  kubectl -n <namespace> create secret generic table-sync-s3 --from-literal=access-key-id=<只读 key> --from-literal=secret-access-key=<只读 secret>
  ```
- **监控**：除了告警规则，还要装 `deploy/k8s/table-sync-podmonitor.yaml`。sidecar 的指标在它自己的 9099 端口上，Pod 注解只能指业务容器的端口，不装这份，`TableSyncStale` 和 `TableSyncFetchFailing` 永远没有数据。
  ```powershell
  kubectl apply -n observability -f deploy/k8s/table-bundle-alerts.yaml
  kubectl apply -n observability -f deploy/k8s/table-sync-podmonitor.yaml
  ```
- **单包上限 64 MiB**（`pack` 时就拒绝）。它和 sidecar 卷的 `sizeLimit: 1Gi` 是一起算的（设计 §7「磁盘预算」），要放大必须两个一起改，否则一个大包会让 kubelet 驱逐整条轨道的 Pod。

## 4. 撤销、回滚与清理旧 schema 线

- **到点前撤销**：`table-bundle cancel -track <track> -schema-digest <16hex>`（调用 `DELETE /admin/tables/next`）。
- **到点后回滚**：`table-bundle rollback -track <track> -schema-digest <16hex> -target-digest <old64> -target-sha256 <oldsha> -target-version <oldver> -current-dir <cur> -target-dir <old>`。
  - 若新包曾新增 `(cfg_ids_permanent)` 表（如 `Item`、`Pet`）的行且已产出资产，回滚会让这些 ID 消失导致资产停在 RETRY。工具默认拦截并提示向前修；确需强制回滚才加 `-force`。
  - `-current-dir` 与 `-target-dir` **必填**（不加 `-force` 时缺一个就拒绝，什么都不写）：`<cur>` 是这条线此刻生效那一包的表目录，`<old>` 是 `-target-digest` 那一包的表目录（从对应提交导表，或把对象存储里的包解开）。工具先用 `GET /admin/tables` 取生效包，核对 `<cur>` 的 `content_digest` 等于它、`<old>` 的等于 `-target-digest`、两边 `schema_digest` 等于 `-schema-digest`，对不上直接报错，`-force` 也不放过。
  - `-force` 只放过两件事：不带两个目录（跳过检查，stderr 打 WARN）、比出了丢行（照样回滚，WARN 里列出丢的 id）。线上这条线没有生效指针（用的是镜像自带的表）时目录核不了，只能 `-force` 不带目录回滚。依据见设计 §9「回滚」。
- **删掉废弃的 schema 线**：旧版本全部下线后，执行 `table-bundle delete-line -track <track> -schema-digest <old16>`。只要仍有在线 Pod 回报该线，管理接口返回 `409 Conflict` 拒绝删除。

## 5. 告警处置

| 告警 | 严重度 | 处置 |
|---|---|---|
| `TableBundleRejected` | critical | 调 `GET /admin/tables` 看 `rejected_pods` 的原因。未到 `activate_at` 立即 `cancel`；已过生效点立即查明是数据问题还是个别 Pod 异常，按需 `rollback` 或向前发新包 |
| `TableBundleNotConverged` | warning | 调 `GET /admin/tables` 找出滞后 Pod；查该 Pod `table-sync` 容器日志（是否 S3 拉取/校验失败）与业务日志（是否 `needs_restart`） |
| `TableSyncStale` | warning | 检查 `table-sync` 到 `etcd.<infra>:2379` 的连通性与 etcd 集群健康；期间业务容器继续用本地旧表，不影响在线玩家 |
| `TableSyncFetchFailing` | warning | sidecar 连续 5 分钟以上取不到包。看 `kubectl logs <pod> -c table-sync`（403 先查签名与凭据，404 查包是否已上传、桶名是否对），再看卷里 `POINTER.json` 的 `sync_errors`。在跑的 Pod 继续用手上的表、收不到新包；这期间新起的 Pod 会拒绝启动（§6 第一行），所以要先修好存储再做扩容或滚动重启。到点前碰上，先 `cancel` 掉 `next` |
| `TableBundleNeedsRestart` | info | 有实例停在 `needs_restart` 超过 24 小时：变更触及未标 `(cfg_hot_reload) = true` 的结构表，按常规发布手册滚动重启对应工作负载即可生效。重启后，或者指针改回不需要重启的包，告警自己消失 |

## 6. 不带告警的两种现象

| 现象 | 原因与处置 |
|---|---|
| 新 Pod 的业务容器反复重启，日志里是 `Startup refused: table bundle for schema line …`（Go 是小写开头的同一句） | 这个 Pod 的 `table-sync` 没把本线的表包同步下来（对象存储不通、包不存在、摘要对不上），业务进程因此拒绝启动，不退回镜像里更旧的表。这是有意的取舍：起不来会马上被发现，带着旧表起来不会（设计 §8.1）。看 `kubectl logs <pod> -c table-sync`，以及卷里 `POINTER.json` 的 `sync_errors`。同步好之后业务容器自己会起来，不用人工干预；已经在跑的 Pod 不受影响 |
| 被拒绝的包撤掉或回滚之后，`GET /admin/tables` 里的拒绝记录过几秒自己消失 | 正常。进程发现指针不再指向那一包，就清掉自己的拒绝 / 需要重启结论并重新回报。同一包再发一次会重新校验 |

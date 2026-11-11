# K8s Docker Desktop 故障排查指南

在 Docker Desktop K8s 上部署 10 个区服（170 个 Pod）时总结的经验教训（2026-03-28）。

## 将镜像加载到 containerd

Docker Desktop K8s 使用 containerd。`docker build` 只将镜像放入 Docker daemon——K8s 无法看到这些镜像。

```powershell
# 加载单个镜像
docker save ghcr.io/luyuancpp/mmorpg-node:v1 | docker exec -i desktop-control-plane ctr -n k8s.io images import --all-platforms -

# 加载所有 Go/Java 镜像
@("mmorpg-auth","mmorpg-data-service","mmorpg-db","mmorpg-login","mmorpg-player-locator","mmorpg-scene-manager") | ForEach-Object {
    docker save "ghcr.io/luyuancpp/${_}:latest" | docker exec -i desktop-control-plane ctr -n k8s.io images import --all-platforms -
}
```

如果清单使用 `:latest` 但镜像构建时标记为 `:v1`，需要先打标签：
```powershell
docker tag ghcr.io/luyuancpp/mmorpg-login:v1 ghcr.io/luyuancpp/mmorpg-login:latest
```

验证 containerd 中的镜像：
```powershell
docker exec desktop-control-plane crictl images | Select-String "mmorpg"
```

## maxPods 限制

默认 `maxPods` 为 110。10 个区服 × 17 个 Pod = 170，Pod 会卡在 `Pending` 状态（"Too many pods"）。

```powershell
# 提升到 250
docker exec desktop-control-plane sh -c 'echo "maxPods: 250" >> /var/lib/kubelet/config.yaml'
docker exec desktop-control-plane sh -c 'kill -HUP $(pidof kubelet)'

# 验证
kubectl get node desktop-control-plane -o jsonpath='{.status.capacity.pods}'  # 应显示 250
```

## 查看已部署的区服

```powershell
# 所有区服命名空间
kubectl get ns | Select-String mmorpg-zone

# 快速健康摘要
1..10 | ForEach-Object {
    $ns = "mmorpg-zone-zone-$_"
    $pods = kubectl get pods -n $ns --no-headers 2>$null
    $total = ($pods | Measure-Object).Count
    $running = ($pods | Select-String "Running" | Measure-Object).Count
    Write-Host "zone-$_`: $running/$total Running"
}

# 单个区服
kubectl get pods -n mmorpg-zone-zone-1
kubectl get svc  -n mmorpg-zone-zone-1
kubectl logs <pod> -n mmorpg-zone-zone-1
kubectl describe pod <pod> -n mmorpg-zone-zone-1

# 查找非运行状态的 Pod
kubectl get pods -n mmorpg-zone-zone-1 --no-headers | Where-Object { $_ -notmatch "Running" }
```

## 故障排查记录

### 1. C++ 节点段错误——数组越界
- **现象**：gate/scene Pod 处于 CrashLoopBackOff，`InitMessageInfo()` 中发生段错误
- **根因**：`kMaxRpcMethodCount = 102` 但消息 ID 最大到 117。数组越界写入破坏了相邻的 BSS 内存（gRPC `Status::OK` 守护变量）。
- **修复**：在 `rpc_event_registry.h` 中设置 `kMaxRpcMethodCount = 118`；在 `.cpp` 数组定义中使用常量（而非字面量）。
- **诊断方法**：使用 `nm` 获取符号地址，计算数组跨度，确认越界写入覆盖了 `Status::OK`。

### 2. C++ Redis 主机名解析
- **现象**：在 `sockets::fromIpPort` 处崩溃——`inet_pton("redis")` 返回 0。
- **根因**：muduo 的 `InetAddress(ip, port)` 期望数字 IP。K8s 服务名 `"redis"` 是 DNS 名称。
- **修复**：在 `node.cpp` 中构造地址前使用 `InetAddress::resolve()`。注意：etcd（gRPC）和 Kafka（librdkafka）原生支持 DNS 解析——仅 Redis/muduo 受影响。

### 3. 表文件名大小写敏感（Windows → Linux）
- **现象**：`File2String("actoractioncombatstate.json")` 失败——磁盘上的文件名为 `ActorActionCombatState.json`。
- **根因**：C++ 代码将文件名转为小写；在大小写不敏感的 Windows 上可用，在 Linux 上失败。
- **修复**：在 Dockerfile 的 RUN 步骤中，COPY 之后将所有 `.json` 表文件名转为小写。

### 4. Go/Java ImagePullBackOff
- **现象**：Go/Java Pod 处于 ImagePullBackOff，C++ 节点正常运行。
- **根因**：containerd 中的镜像标记为 `:v1`，清单引用的是 `:latest`。
- **修复**：先 `docker tag :v1 :latest`，然后 `docker save | ctr images import`。

### 5. db CrashLoopBackOff（启动顺序问题）
- **现象**：db Pod 处于 Error/CrashLoopBackOff，MySQL 正常运行。
- **根因**：db Pod 在 MySQL 就绪前启动（由于 maxPods 限制，所有 Pod 最初都处于 Pending 状态）。等 MySQL 启动后，db 已经缓存了连接失败状态。
- **修复**：`kubectl rollout restart statefulset/db -n <zone-ns>`。

### 6. Go protobuf 注册冲突
- **现象**：Go 服务启动时因 protobuf 注册重复而 panic。
- **修复**：在所有 5 个 Go 服务上设置环境变量 `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn`。

### 7. Kafka Brokers 配置类型
- **现象**：Go 服务无法解析 Kafka 配置。
- **根因**：`k8s_deploy.ps1` 将 Brokers 渲染为逗号分隔的字符串；Go 期望 `[]string`（YAML 列表）。
- **修复**：模板渲染时使用 YAML 列表格式输出 Brokers。

## 关键经验总结

- 在 Docker Desktop K8s 上 `docker build` 之后，务必将镜像加载到 containerd。
- 在单节点上部署多个区服前，先提升 `maxPods` 上限。
- 对于启动顺序问题，`kubectl rollout restart` 是最快的修复方式。
- muduo 的 `InetAddress` 需要 `resolve()` 来处理 DNS 主机名——与原生支持 DNS 的 gRPC 和 librdkafka 不同。
- Windows→Linux 文件路径：注意生成文件名的大小写敏感性。

# 时钟运维手册

机器时钟错了,按墙钟算的业务会悄悄出错:会话和令牌过期、每日重置、活动开关、冷却、按时间列排序翻页。出过的事故:线上运维把机器时间调回一年前又调回来,没有任何告警。

2026-11-19 起有两道防线:节点时钟告警(§2)和服务启动核对(§5)。**代码未编译、未测试,告警未在集群上 apply 过。**

## 1. 要知道的事实

- 时钟由节点上的 NTP(chrony)维护,云上用云厂商的时间源。**不要手工改机器时间**(`date -s`、`timedatectl set-time`)。
- chrony 一般只在开机时允许跳变(`makestep 1 3`),运行中只慢慢追。差一年时它要追很多年,手工调错了只能手工改回来(§4)。
- 容器里改不了节点时钟(没有 `CAP_SYS_TIME`)。一个节点上的全部 Pod 共用节点的时钟,所以告警按节点报。
- 各部分对时钟的依赖:
  - 永久 ID 发号器:唯一性不靠时钟。时钟回拨只会报 `ahead_of_clock`;时钟被调快会把计数器抬高,见[发号器运维手册](id-issuer-runbook.md) §3、§4.3。
  - C++ 的 snowflake(scene_id):回拨时接着用上一秒剩下的序号,打一次 ERROR,不停发。
  - 令牌过期、每日重置、活动、冷却、按时间排序:直接按墙钟算,时间错了就错。

## 2. 告警

`deploy/k8s/clock-alerts.yaml`(PrometheusRule,手工 apply,见 `deploy/k8s/README.md`)。比的是节点时间(node_exporter 的 `node_time_seconds`)和 Prometheus 的抓取时刻。

| 告警 | 含义 | 处置 |
|---|---|---|
| `NodeClockOffsetDrifting`(warning) | 节点和 Prometheus 差 1 秒以上,持续 15 分钟 | NTP 没在正常工作,按 §3 查 chrony |
| `NodeClockOffsetTooLarge`(critical) | 差 30 秒以上,持续 2 分钟。和启动拒启门槛相同:这个节点上的 Pod 重启后起不来 | 按 §4 处置 |

- 所有节点一起响:先看 Prometheus 所在节点的时钟。
- kube-prometheus 自带的 `NodeClockNotSynchronising`(NTP 没同步)照旧有效。它自带的 `NodeClockSkewDetected` 读内核 timex 的 offset,chrony 不经内核 PLL 调时的时候看不见手工调错的时钟,所以另写了这两条。

## 3. 查 NTP

在节点上:

```bash
chronyc tracking
```

```bash
chronyc sources -v
```

- `System time` 是本机和时间源的差,`Leap status: Not synchronised` 是没同步。
- 时间源不可达:查节点到时间源的网络。云上确认用的是云厂商的时间源。

## 4. 时钟被改错了

1. 隔离节点:`kubectl cordon <节点>`,再 `kubectl drain <节点> --ignore-daemonsets --delete-emptydir-data`。
2. 在节点上把时间改回来:`chronyc makestep` 让 chrony 立刻跳到正确时间,再看 `chronyc tracking` 的 `System time` 是否接近 0。
3. 查是谁改的:节点 `journalctl -u chronyd` 里的 `System clock wrong by`、`System clock was stepped`;打开了改时间审计(§6)的话看审计日志。
4. 核对出事期间按墙钟算的数据:
   - 时间被调回去过:那段时间里过期的令牌又能用,结束的活动又开;
   - 时间被调快过:每日重置、活动可能提前触发;发号器看 `ahead_of_clock`;
   - 那段时间写下的时间列(`created_at` 等)是错的,按时间排序、翻页会乱。
5. `kubectl uncordon <节点>`,放回调度。

## 5. 启动核对

每个服务启动时把本机时钟和 Kubernetes API server 的时钟(回应里的 `Date` 头)比一次。规则在 `go/shared/clockcheck`、C++ 的 `clock_check.h`、Java 的 `java/server_startup`(网关和商城共用),三处相同。

- 差 30 秒以内:照常启动,打一行 `clock check passed: the local clock is ... off the Kubernetes API server clock`。
- 差 30 秒以上:拒启,退出码非 0。Go 服务和 Java 服务打 `<服务> refuses to start: the local clock is ...`;C++ 节点打 `clock config: the local clock is ...`。
- 读不到 API server(集群 CA 没挂载、网络不通、Date 头读不懂):打 `clock check skipped: cannot read the Kubernetes API server clock: ...`,照常启动。拿不到参照不等于时钟错了,为此拒启会把 API server 的一次抖动放大成重启的服务都起不来。
- 不在 Kubernetes 里(本机开发):不核。

覆盖全部 15 个 Go 服务、C++ 的 gate / scene / battle、Java 的网关和商城(mall)。

只在启动时核一次:进程跑着的时候时钟被改,只有 §2 的告警看得见,进程不会自己停。

### 应急开关

给工作负载加环境变量 `MMORPG_CLOCK_CHECK_MODE=warn`,超限只告警、照常启动(三种语言同一个变量,只认小写的 `warn`)。

- 只给一种情形用:API server 自己的时钟错了,所有节点上的服务一起拒启。
- 先用 §3 确认节点时钟是对的,再加;用完立刻去掉。
- 不要拿它把时钟确实错了的节点上的服务硬拉起来,先按 §4 改时钟。

## 6. 预防

- 收回节点上改时间的权限(root、sudo),改时间走变更流程。
- 打开改时间的审计,例如:

  ```bash
  auditctl -a always,exit -F arch=b64 -S settimeofday -S clock_settime -S adjtimex -k time-change
  ```

- 告警(§2)接进值班;`NodeClockOffsetTooLarge` 是 critical。

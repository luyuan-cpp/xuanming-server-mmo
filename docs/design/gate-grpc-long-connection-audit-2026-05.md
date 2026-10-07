# Gate→Login gRPC 长连接核查 (2026-05)

**Date:** 2026-05-08
**Task:** #13
**结论:** ✅ **gate→login gRPC 是真长连接复用,不是每次新建。** 压测残留的 `Bound=2704` 不是 gate→login 引起的。

---

## 一、核查证据(代码路径)

### 1. 单例 Channel Cache

`cpp/libs/engine/core/node/system/grpc_channel_cache.h`:

```cpp
class GrpcChannelCache {
public:
    std::shared_ptr<grpc::Channel> GetOrCreateChannel(const std::string& target) {
        auto it = channelCache_.find(target);
        if (it != channelCache_.end()) {
            if (auto existing = it->second.lock()) {
                return existing;        // ← 命中,复用
            }
        }
        // 未命中才 CreateCustomChannel
        ...
    }
private:
    std::unordered_map<std::string, std::weak_ptr<grpc::Channel>> channelCache_;
};
```

按 `IP:Port` 维度缓存,**每个 (login_ip, login_port) 全进程共用一条 Channel**。

### 2. Channel 持有点

`cpp/libs/engine/core/node/system/node/node_connector.cpp` `ConnectToGrpcNode`:

```cpp
auto cachedChannel = gNode->GetGrpcChannelCache().GetOrCreateChannel(target);
const auto &grpcChannel =
    targetRegistry.emplace<std::shared_ptr<grpc::Channel>>(createdId, std::move(cachedChannel));

InitGrpcNode(grpcChannel, targetRegistry, createdId);
```

Channel 通过 `shared_ptr` 持有在 ECS registry 上,生命周期 = node 生命周期(进程级)。

### 3. 调用路径不重建 Channel

`HandleGrpcNodeMessage` (`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:397`)
→ `rpcHandlerMeta.sender(registry, *node, ...)` 通过 ECS 拿现成 Channel/Stub
→ 复用同一条 HTTP/2 长连接,gRPC 多路复用 stream

**没有"每次请求 new Channel"的反模式。**

---

## 二、HTTP/2 长连接 + 多路复用的端口特征

| 特征 | 表现 |
|---|---|
| 一条 HTTP/2 Channel | 1 个 ephemeral port |
| N 个并发 stream | 共享同一连接,不吃额外端口 |
| 单进程对单 login 实例 | 总共 1 个 ephemeral port(理论值) |

→ 即使 gate 同时跑 5 万并发登录请求,**到同一个 login 实例只占 1 个 ephemeral port**。

---

## 三、那 `Bound=2704` 残留是哪来的?

排除了 gate→login,候选是:

| 来源 | 判别 |
|---|---|
| **压测客户端短连**(机器人) | 压测机上 `ss -s` 看 TIME_WAIT 是否几万 |
| gate→Redis | gate 上 `ss -tan state time-wait dst <redis_ip>` 计数 |
| gate→Kafka | dst 是 broker 的端口 |
| gate→etcd | dst 是 etcd 端口 |
| gate→其他 Go 服务(player_locator / scene_manager / data_service) | 同样走 `GrpcChannelCache`,理论不该是源头,但要确认 zone 跨服务时是否有 Channel 频繁失效重建 |

### 推荐验证命令

```bash
# 1. 在 gate 机器上看 TIME_WAIT 分布到哪个对端
ss -tan state time-wait | awk '{print $4}' | cut -d: -f1 | sort | uniq -c | sort -rn

# 2. 对比启动时 vs 压测中 cached channel 数量
#    grep 日志: "ResourceQuota max threads"  / "Connecting to GRPC node"

# 3. 在压测客户端机器上看(如果跑机器人)
ss -s | head
ss -tan state time-wait | wc -l
```

### 重点怀疑

`robot/` 机器人客户端如果是**每个机器人一个进程 / 每次循环新建 TCP**,那 `Bound=2704` 来自压测机自己的端口耗尽,与 gate 无关。

→ 在 [stress-test-2026-05-ephemeral-port.md](../stress/stress-test-2026-05-ephemeral-port.md) §三 已建议复测确认。

---

## 四、附加发现 / 改进建议

### 4.1 Channel 用 `weak_ptr`,可能在低活时被释放重建

```cpp
std::unordered_map<std::string, std::weak_ptr<grpc::Channel>> channelCache_;
```

ECS 持有 `shared_ptr`,所以只要 node 还在,Channel 就在。**正常情况下不会重建。**
但如果某段时间没有节点持有(比如 etcd 上 login 全部下线再上线),Channel 会被释放,下次请求重建。

→ 不是 bug,是符合预期的行为。

### 4.2 gRPC keepalive 没显式配置

当前 `grpc::ChannelArguments` 只设了:
- `ResourceQuota` (并发线程数)
- `grpc.client_channel_backup_poll_interval_ms`

**没有显式设 keepalive**,默认值在某些 Linux 内核下可能被中间防火墙/NAT 踢掉空闲连接,导致下次请求触发握手 → 短暂 ephemeral port 消耗。

**建议补充**(在 `GetOrCreateChannel` 里):
```cpp
args.SetInt(GRPC_ARG_KEEPALIVE_TIME_MS, 30000);          // 30s 一次
args.SetInt(GRPC_ARG_KEEPALIVE_TIMEOUT_MS, 10000);
args.SetInt(GRPC_ARG_KEEPALIVE_PERMIT_WITHOUT_CALLS, 1); // 空闲也保活
args.SetInt(GRPC_ARG_HTTP2_MAX_PINGS_WITHOUT_DATA, 0);
```

低优先级优化,实测后再决定上不上(开了之后 server 端要相应放宽 `KeepalivePolicy`,否则会被 GOAWAY)。

---

## 五、结论与下一步

1. ✅ **gate→login gRPC 长连接复用是正确的**,不需要改。
2. 🔎 `Bound=2704` 残留**最可能来自压测机器人侧的短连接**,需在下一轮阶梯压测时同步在压测机上抓 `ss -s` 确认。
3. 📋 把"压测机也调 sysctl + 客户端开 keep-alive"写入 [gate-kernel-tuning-runbook.md](../ops/gate-kernel-tuning-runbook.md)。
4. 🔧 (低优先级) 给 `GrpcChannelCache` 加 keepalive 参数,提升弱网环境鲁棒性。

---

## 关联
- [docs/design/ARCH.md](./ARCH.md) §6 游戏内通信
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](./architecture-current-state-vs-gaps-2026-05.md) 缺口 #13
- [docs/stress/stress-test-2026-05-ephemeral-port.md](../stress/stress-test-2026-05-ephemeral-port.md) §三 哪台机器
- 代码: `cpp/libs/engine/core/node/system/grpc_channel_cache.h`
- 代码: `cpp/libs/engine/core/node/system/node/node_connector.cpp` (ConnectToGrpcNode)
- 代码: `cpp/nodes/gate/handler/rpc/client_message_processor.cpp` (HandleGrpcNodeMessage)

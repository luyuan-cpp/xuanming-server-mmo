# proto/ —— 协议契约

所有跨进程、跨语言的接口都在这里定义,这是**唯一事实源**。C++、Go、Java、机器人和客户端用到的
消息代码全部由这些 `.proto` 生成;改接口永远从这里开始。

## 目录

```
proto/
├── common/              多个服务共用的消息
│   ├── base/            基础类型与节点信息
│   ├── component/       ECS 组件的数据结构(同时用于存盘)
│   ├── event/           事件消息
│   ├── database/        落库相关的消息
│   ├── constants/       常量与功能开关
│   ├── options/         自定义 option
│   ├── asset/           资产通道
│   └── rollback/        玩家快照与流水(回档用)
├── contracts/kafka/     经 Kafka 投递的命令与事件(gate、scene、match、player)
├── <service>/           一个服务一个目录:login、scene、scene_manager、data_service、
│                        db、player_locator、gate、battle、match、team、chat、guild、
│                        friend、mail、rank、trade、client_rpc_router、instance、etcd
├── message_id.txt       消息号注册表(生成器读写;删掉的方法留 `N=reserved:<原名>` 墓碑,号永不复用)
└── event_id.txt         事件号注册表(生成器读写;墓碑格式同上)
```

## 哪些是客户端能看到的

带 `option (OptionIsClientProtocolService) = true` 的服务是客户端契约,改动要和客户端同批发布:
`login`、`match`、`team`、`chat`、`friend`、`mail/mail.proto`、`guild`、`trade`、`battle/player_battle.proto`,
以及 `scene/` 下显式标注该 option 的服务。文件名前缀不能决定可调用范围，例如 `player_lifecycle.proto` 是内部接口；方法调用形态以 `OptionClientRpcKind` 为准，详见[客户端调用桩设计](../docs/design/client-rpc-callback.md)。其余服务不能仅凭名称当作客户端接口。

## 改协议的流程

```powershell
# 1. 改 proto/ 下的 .proto
# 2. 生成器源码有变更或本机尚无工具时先构建
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
# 3. 仅服务端任务使用 no-unity 配置；默认配置会写独立客户端仓
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
# 4. 重新编译受影响的 C++ / Go / Java 工程
```

生成产物的去向:

| 语言 | 位置 |
|------|------|
| C++ | `cpp/generated/proto/`、`cpp/generated/grpc_client/`、`cpp/generated/rpc/` |
| Go | `go/proto/`,以及各服务的 `generated/` |
| 机器人 | `robot/` 下的生成代码 |
| 客户端 | 同级仓库 `../mmorpg-client/` |

## 三条硬规则

1. 不手改任何生成产物,改源头再重新生成。
2. 字段编号上线后不复用;删字段写 `reserved N;` 并注明原因。
3. 字段类型有强制约束:坐标用 `double`,雪花 ID、时间戳、货币用 64 位。完整列表见
   [AGENTS.md](AGENTS.md)。

错误码(tip)不在这里手写,由导表器从 `data/tip/Tip.xlsx` 生成,见 [data/README.md](../data/README.md)。

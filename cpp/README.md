# cpp/ —— C++ 运行时节点与引擎

三个可运行的节点进程(gate、scene、battle),以及它们共用的引擎库、玩法模块和测试。

## 目录

```
cpp/
├── nodes/                  可运行的节点:进程入口,加节点自己的 RPC / 事件处理器
│   ├── gate/               客户端 TCP 入口:会话、token 校验、消息转发
│   ├── scene/              ECS 世界模拟:玩家玩法、AOI、场景
│   ├── battle/             回合制战斗房间:客户端凭票据直连
│   └── _template/          新节点的 main.cpp 模板与检查清单
├── libs/
│   ├── engine/             与玩法无关的基础层
│   │   ├── core/           网络编解码、节点注册发现、日志、时间、限流、追踪
│   │   ├── config/         读 base_deploy_config.yaml 与 game_config.yaml
│   │   ├── infra/          Kafka、Redis 客户端,Agones 生命周期
│   │   ├── session/        从 gRPC 上下文取会话信息
│   │   ├── thread_context/ 线程局部的 ECS registry、Redis、雪花 ID 等
│   │   └── muduo_windows/  muduo 网络库的 Windows 移植
│   ├── modules/            多个节点可复用的玩法模块:背包、货币、任务、奖励、条件……
│   └── services/           各节点的领域逻辑
│       ├── scene/          场景玩法:actor、player、combat、spatial、world……
│       ├── gate/           gate 的会话管理
│       ├── battle/         回合制战斗引擎与结算
│       └── player/         跨节点的玩家 RPC 接口
├── generated/              生成的代码,勿手改
│   ├── proto/              protobuf 消息
│   ├── grpc_client/        gRPC 异步客户端封装
│   ├── rpc/                服务元数据与消息号注册
│   ├── proto_helpers/      proto 辅助代码
│   └── table/              配置表访问代码(导表器产出)
├── tests/                  GoogleTest 工程,一个目录一个测试程序
└── plugin/                 Clang 工具 no_raw_ptr_check:构建前检查裸指针成员
```

## 一个节点内部怎么分

以 scene 为例,一条客户端请求经过三层:

| 层 | 位置 | 职责 |
|----|------|------|
| 处理器 | `nodes/scene/handler/rpc/` 的 `*Handler` | 薄适配:解包、校验、转调,不放业务 |
| 领域逻辑 | `libs/services/scene/` 的 `*System` | 玩法规则与状态变化 |
| 数据 | `libs/services/scene/` 的 `*Comp` | ECS 组件,只有数据 |

异步应答的处理器在 `nodes/<node>/rpc_replies/`,命名为 `On<Domain><Method>Reply`;
事件处理器在 `nodes/<node>/handler/event/`。生成的处理器骨架里,自定义逻辑只写在
`///<<< BEGIN WRITING YOUR CODE` 守护段内,重新生成时这一段会保留。

## 构建与测试

```powershell
# Windows:解决方案在仓库根目录,保持串行
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64

# 测试(GoogleTest),产物在 build\cpp\tests
pwsh tools/scripts/run_cpp_tests.ps1 -Build        # 先串行编译再跑
pwsh tools/scripts/run_cpp_tests.ps1 -Filter aoi   # 只跑名字含 aoi 的测试工程
```

```bash
# Linux:也是容器镜像构建用的入口
bash tools/scripts/build_linux.sh --release
```

节点可执行文件输出到仓库根的 `bin/`,静态库到 `lib/`,中间文件与测试程序到 `build/cpp/`。

## 新增一个节点

从 [`nodes/_template/`](nodes/_template/README.md) 开始:无长生命周期状态的节点用
`main.simple.cpp.example`,需要定时器、编解码器等运行时状态的用
`main.with_context.cpp.example`。

编码约定与排查入口见 [AGENTS.md](AGENTS.md)。

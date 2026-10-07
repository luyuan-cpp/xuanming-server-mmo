# generated/ —— 入库的生成产物

这里的每个文件都是工具生成的,**不要手改**:改源头(`proto/` 或 `data/`),再运行 `dev.bat gen`。

产物进版本库,所以协议和配表的每次变化都能在代码评审里看到差异;CI 的导表器测试会检查产物是否
与源头一致。

## 目录

| 目录 | 由谁生成 | 内容 | 谁在用 |
|------|----------|------|--------|
| `tables/` | 导表器 | 每张配置表的 JSON 与 `.pb` 数据,以及 `manifest.json` | C++ 节点、Go 服务、机器人在启动时加载 |
| `code/cpp/` | 导表器 | C++ 查表代码 | 部署到 `cpp/generated/table/` |
| `code/java/` | 导表器 | Java 查表代码 | 部署到 `java/config_node/` |
| `code/csharp/` | 导表器 | C# 查表代码 | 部署到客户端仓库 |
| `code/proto/` | 导表器 | 各表的 `.proto` 及 tip 错误码枚举 | 各语言的 protobuf 编译输入 |
| `code/python/`、`code/ue/` | 导表器 | Python、UE 查表代码 | 目前没有消费者 |
| `proto/` | 协议生成器 | 生成 Go 代码前的暂存树 | `go/build.ps1` 与协议生成器自身 |
| `robot/proto/` | 协议生成器 | 给机器人的 proto 副本 | `robot/` |
| `data/` | 生成器 | 区库的表清单 `mysql_database_table_list.json` | `go/db` |

## 其他存放生成代码的位置

生成代码按语言放在各自的源码树里,而不是全部集中在这里:

| 位置 | 内容 |
|------|------|
| `cpp/generated/` | C++ 的 protobuf、gRPC 客户端、服务元数据、查表代码 |
| `go/proto/` | Go 的全部 `.pb.go` |
| `go/shared/generated/` | Go 的查表代码与 tip 错误码 |
| `go/<service>/generated/` | 各服务用到的消息号常量 |

生成器本身在 [`tools/proto_generator/`](../tools/proto_generator/) 与
[`tools/data_table_exporter/`](../tools/data_table_exporter/)。

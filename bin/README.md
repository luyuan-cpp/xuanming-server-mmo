# bin/ —— C++ 节点的工作目录

gate、scene、battle 三个节点在本机运行时以这里为当前目录:它们按相对路径读 `etc/` 下的配置,
并经 `../generated/tables/` 加载配置表。所以这个目录不是纯粹的编译输出目录,里面的配置文件是
入库的。

## 入库的内容

| 路径 | 内容 |
|------|------|
| `etc/base_deploy_config.yaml` | 部署配置:etcd 与 Kafka 地址、配置表目录、日志级别、连接上限等 |
| `etc/game_config.yaml` | 节点配置:区号、scene 节点角色、区 Redis 地址 |
| `etc/game_config_instance.yaml` | 副本类 scene 节点的示例配置 |
| `logs/cpp_nodes/.gitkeep` | 占位:节点的日志库不会自己创建目录,缺了会在启动时直接中止 |

## 编译和运行后出现的内容(不入库)

| 路径 | 内容 |
|------|------|
| `gate.exe`、`scene.exe`、`battle.exe` | 节点可执行文件,由 MSBuild 的生成后事件复制过来 |
| `*.dll` | 节点运行需要的动态库(librdkafka、zlib) |
| `go_services/` | 编译好的 Go 服务,让全部进程都能从这里启动 |
| `zoneinfo/` | Linux 运行时镜像用的时区数据 |

其余编译产物不在这里:中间文件与测试程序在 `build/cpp/`,静态库在 `lib/`。
日志、pid 文件等运行时产物在 `run/`,见 [docs/ops/run-directory.md](../docs/ops/run-directory.md)。

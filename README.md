# MMORPG Server Framework

A polyglot MMORPG server-side framework built with **C++**, **Go**, and **Java**.  
Designed for learning, experimentation, and prototyping of scalable game server architectures.

---

## Features

- **Entity-Component System (ECS)** — flexible game object modeling via [EnTT](https://github.com/skypjack/entt)
- **Polyglot backend** — C++ runtime nodes, Go microservices (go-zero), Java auth (Spring Boot + sa-token)
- **Proto-first contract design** — all service contracts defined in `proto/`, generated outputs checked in
- **Kafka-based routing** — gate/scene control messaging decoupled via Kafka topics
- **gRPC service mesh** — cross-service communication through gRPC with etcd discovery
- **Lua scripting** — game logic extension and rapid iteration
- **Kubernetes-ready** — deployment manifests and zone lifecycle management

---

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Runtime nodes | C++17, EnTT ECS, muduo networking |
| Microservices | Go (go-zero), gRPC, Kafka |
| Auth/Web | Java (Spring Boot, sa-token, gRPC) |
| Messaging | Kafka (gate/scene control), gRPC (service mesh) |
| Storage | MySQL, Redis |
| Discovery | etcd |
| Build | MSBuild (C++), Go modules, Maven (Java) |
| Deploy | Docker Compose (dev), Kubernetes (prod) |

---

## Project Structure

```
mmorpg/
├── cpp/                # C++ runtime nodes (scene, gate, centre) and shared libraries
│   ├── nodes/          #   Node entrypoints and transport handlers
│   ├── libs/           #   Shared engine, ECS services, and game modules
│   ├── generated/      #   Auto-generated proto/grpc/table outputs (do not edit)
│   └── tests/          #   C++ test suites (GTest)
├── go/                 # Go microservices (go-zero based)
│   ├── login/          #   Player auth + node registration
│   ├── scene_manager/  #   Scene allocation + load balancing
│   ├── data_service/   #   Data layer RPC service
│   ├── db/             #   Kafka consumer + MySQL persistence
│   └── player_locator/ #   Player location lookup service
├── java/               # Java gateway services
│   └── gateway_node/   #   Spring Boot gateway (zone directory)
├── proto/              # Authoritative service contract definitions
├── generated/          # Checked-in generated proto/table outputs
├── deploy/             # Docker Compose (dev) + Kubernetes (prod) manifests
├── tools/              # Proto generation, dev scripts, deployment utilities
│   ├── scripts/        #   Central PowerShell tooling (dev_tools.ps1)
│   ├── proto_generator/#   Canonical proto-gen source (Go)
│   └── dev/            #   mprocs dashboard configs
└── third_party/        # Vendored C++ dependencies (source)
```

---

## Getting Started

### Prerequisites

- **C++**: Visual Studio 2022+ (Windows) or GCC 11+ (Linux), CMake 3.20+
- **Go**: Go 1.21+, [go-zero](https://go-zero.dev/)
- **Java**: JDK 17+, Maven 3.8+
- **Infrastructure**: Docker + Docker Compose (for Kafka, Redis, MySQL, etcd)

### Build

```powershell
# C++ — build the full solution (Windows)
msbuild game.sln /m /p:Configuration=Debug /p:Platform=x64

# Go — regenerate proto stubs and build
cd go && build.bat

# Java — build gateway service
cd java/gateway_node && mvn clean install
```

### Run (Local Dev)

```powershell
# Start infrastructure (Kafka, Redis, MySQL, etcd)
docker compose -f deploy/docker-compose.yml up -d

# Start all Go services + C++ nodes
pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start

# Check status
pwsh -File tools/scripts/dev_tools.ps1 -Command dev-status

# Stop everything
pwsh -File tools/scripts/dev_tools.ps1 -Command dev-stop
```

Or use the unified mprocs dashboard:

```powershell
mprocs -c tools/dev/mprocs.yaml
```

### Proto Generation

```powershell
# Show help
pwsh -File tools/scripts/dev_tools.ps1 -Command help

# Build proto-gen tool
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build

# Run proto generation
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run
```

More details: [tools/README.md](tools/README.md), [tools/scripts/README.md](tools/scripts/README.md)

---

## C++ Node Templates

To add a new C++ node, use the templates under `cpp/nodes/_template/`:

| Template | Use case |
|----------|----------|
| `main.simple.cpp.example` | Nodes without long-lived runtime state |
| `main.with_context.cpp.example` | Nodes needing runtime state (timers, codecs, bridges) |
| `README.md` | Step-by-step checklist and placeholder guide |

---

## Testing

```powershell
# C++ tests (example)
build\cpp\tests\bag_test.exe --gtest_filter=BagTest.AddNewGridItem

# Go tests
cd go\login && go test ./...

# Java tests
cd java\gateway_node && mvn test
```

---

## Architecture Overview

```
Client ──TCP──▶ Gate Node (C++) ──Kafka──▶ Scene Node (C++)
                     │                          │
                     │ gRPC                     │ gRPC
                     ▼                          ▼
              Login Service (Go)        Scene Manager (Go)
                     │                          │
                     │                   Player Locator (Go)
                     ▼                          │
              Data Service (Go) ◀──────────────┘
                     │
                Kafka topics
                     ▼
              DB Consumer (Go) ──▶ MySQL
```

- **Gate Node**: accepts client TCP connections, authenticates via Login, routes messages via Kafka
- **Scene Node**: runs ECS game simulation, handles player actions and scene logic
- **Go services**: stateless microservices for auth, data, scene management, player location
- **Kafka**: decouples gate/scene control messages (`gate-{id}` topic pattern)
- **etcd**: service discovery for gRPC endpoints

For detailed design docs, see [docs/](docs/).

---

## Contributing

1. Edit `.proto` files in `proto/` first; regenerate consumers with `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`
2. Do **not** hand-edit files under `generated/` or generated proto output trees
3. Keep RPC handlers thin — delegate business logic to `*System` classes
4. Follow naming conventions: `*Comp` suffix for ECS components, trailing underscore for private members
5. C++ runtime code splits: node-facing adapters (`cpp/nodes/*`) vs reusable services (`cpp/libs/services/*`)

---

## License

This project is licensed under the **MIT License** — see [LICENSE](./LICENSE) for details.

## 客户端仓库

客户端统一使用服务端同级的独立仓库 `../mmorpg-client/`（本机为 `E:/work/mmorpg-client`）。服务端不再包含 `client/` 子模块、旧 UI 素材及 FairyGUI 构建脚本。协议生成器和配表导出器的客户端输出均指向该独立仓库；运行生成前应先准备好客户端工作区。目录布局不同时，分别调整 `tools/proto_generator/protogen/etc/proto_gen.yaml` 的 `paths.unity_client_dir` 和 `tools/data_table_exporter/exporter_config.yaml` 的 C# `deploy` 目标。

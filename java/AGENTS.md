# JAVA KNOWLEDGE BASE

## OVERVIEW
`java/` contains Spring Boot services: `gateway_node/` for the Zone Directory / Gateway service (server list, gate assignment, zone health probing, admin APIs) and `springboot_satoken_auth_starter/` as an example/starter app.

## STRUCTURE
```text
java/
├── gateway_node/                    # Spring Boot gateway (zone directory + gate assignment)
│   ├── src/main/java/com/game/gateway/
│   │   ├── config/                  # EtcdProperties, GateProperties, AdminApiKeyFilter
│   │   ├── controller/              # Client APIs + Admin APIs
│   │   ├── dto/                     # Request/Response DTOs, enums
│   │   ├── entity/                  # JPA entities (ZoneConfig, ZoneWhitelist, Announcement)
│   │   ├── etcd/                    # GateWatcher, NodeInfoRecord, NodeType
│   │   ├── repository/              # JPA repositories
│   │   └── service/                 # Zone probe, server list, gate assign, CDN sign
│   └── src/test/                    # Unit tests
├── config_node/                     # 配置表查询服务;导表器 Java 产物的落点,日常不启动
└── springboot_satoken_auth_starter/ # Example/starter app with local auth stack
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Zone directory / gateway | `gateway_node/` | Maven Spring Boot app (port 8081) |
| Server list API | `gateway_node/.../controller/ServerListController.java` | GET /api/server-list |
| Gate assignment API | `gateway_node/.../controller/AssignGateController.java` | POST /api/assign-gate |
| Zone health probing | `gateway_node/.../service/ZoneHealthProbeService.java` | @Scheduled etcd+Redis check |
| Admin zone management | `gateway_node/.../controller/AdminZoneController.java` | CRUD + maintenance/open |
| Admin API key filter | `gateway_node/.../config/AdminApiKeyFilter.java` | X-Admin-Key header for /admin/** |
| etcd gate/scene watcher | `gateway_node/.../etcd/GateWatcher.java` | jetcd-based node discovery |
| Dependency/runtime wiring | `gateway_node/pom.xml` | Spring Boot, jetcd, Redis |
| Tests | `*/src/test/` | Maven test entry |
| Example/starter | `springboot_satoken_auth_starter/` | Standalone example, not main repo runtime |

## CONVENTIONS
- `gateway_node/` uses JDK 23 and Spring Boot 3.4.3.
- `gateway_node/` replaces the former Go gateway (`go/gateway/`, removed 2026-04-14).
- Admin APIs require `X-Admin-Key` header matching `admin.api-key` in application.yaml.
- Zone status uses dual-path: manual (MySQL) + auto (etcd/Redis probe). Manual always overrides auto.
- Gate token signing uses HMAC-SHA256 matching the Go implementation for wire compatibility.
- Use Maven commands from each subtree (`cd java/gateway_node && ./mvnw test`).
- Contract changes still originate in top-level `proto/`; regenerate and rebuild consumers after changes.

## ANTI-PATTERNS
- Editing generated contract consumers without first updating `proto/`.
- Launching the Go gateway alongside the Java gateway (they serve the same role).

## COMMANDS
```bash
cd java\gateway_node && mvn clean install
cd java\gateway_node && mvn test
cd java\springboot_satoken_auth_starter && mvn spring-boot:run
```

## NOTES
- `springboot_satoken_auth_starter/README.md` documents a local example flow with Redis/H2 and optional Docker-backed production setup.

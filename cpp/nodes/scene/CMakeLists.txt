cmake_minimum_required(VERSION 3.22)
project(scene)

set(CMAKE_VERBOSE_MAKEFILE ON)
set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -O0 -g -ggdb")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wall -Wextra")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wno-unused-parameter -Wno-sign-conversion")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -pthread -fPIC")

set(EXECUTABLE_OUTPUT_PATH "${CMAKE_CURRENT_SOURCE_DIR}/../../../bin")

add_definitions(-D__LINUX__)
add_definitions(-D__linux__)
add_definitions(-DNOMINMAX)
add_definitions(-DENTT_ID_TYPE=uint64_t)
add_definitions(-DABSL_PROPAGATE_CXX_STD=TRUE)

include_directories(
    ../../../third_party/
    ../../../third_party/boost/
    # ── OpenSSL 头路径(本节点这批首次需要)────────────────────────────────────
    # scene 节点有两条路会吃到 <openssl/crypto.h> / <evp.h> / <hmac.h>:
    #   1) handler/rpc/player/player_gm_guard.h → ../../../../gate/gate_security.h,
    #      被 player_{attribute,currency,pet,rollback}_handler.cpp 与 scene_handler.cpp
    #      包含(scene_admin_handler.cpp 早就直接包 gate_security.h);
    #   2) handler/grpc/scene_node_service.cpp → services/scene/player/system/asset_op_system.h
    #      → asset_op_auth.h → engine/core/security/token_security.h(header-only,直接包 OpenSSL)。
    # 本节点此前从不显式给 OpenSSL 头路径,靠的是 libssl-dev 装在 /usr/include 的系统头
    # (deploy/k8s/Dockerfile.cpp:44)。而下面 link_directories 把 /usr/local/lib 排在
    # /usr/lib/x86_64-linux-gnu 前面,`-lssl -lcrypto` 取到的是 gRPC 装过去的 BoringSSL
    # 静态库(Dockerfile.cpp:122-130 以默认 gRPC_SSL_PROVIDER=module 编译并 install)。
    # 头取系统 OpenSSL 3.x、库取 BoringSSL = 同一进程里两份 EVP_MD/HMAC_CTX 布局。补上这
    # 两条,让头也走 BoringSSL,与 gate 一致。
    #
    # ⚠️ 这只在直接跑 tools/scripts/build_linux.sh 的宿主机构建上生效:Dockerfile 镜像里
    # .dockerignore 排掉了 third_party/grpc 与 third_party/openssl,builder 阶段也不 COPY
    # 它们,所以镜像内这两条路径不存在(不存在的 -I 被编译器静默忽略),镜像构建仍是系统头
    # + BoringSSL 库。那是 gate / battle 今天就有的同一个问题,不在本次改动范围内,已在交付
    # 说明里单列。
    #
    # **顺序敏感:boringssl 必须排在 openssl 之前。**third_party/openssl/include/openssl/
    # 里 crypto.h 只有未 configure 的 crypto.h.in(evp.h / hmac.h 倒是真文件),三个头能凑齐
    # 的只有 boringssl。顺序反过来会变成 evp/hmac 取 OpenSSL 3.x、crypto 取 BoringSSL 的
    # 混搭 ABI —— 编译期未必报错,运行期是未定义行为。同序写法见
    # cpp/nodes/gate/CMakeLists.txt 与 cpp/libs/services/scene/CMakeLists.txt。
    ../../../third_party/grpc/third_party/boringssl-with-bazel/include/
    ../../../third_party/librdkafka/src-cpp/
    ../../../third_party/muduo-linux/
    # 与上面 boringssl 配对,见那里的顺序说明(必须排在 boringssl 之后)。
    ../../../third_party/openssl/include/
    ../../../third_party/redis/
    ../../../third_party/redis/deps/
    ../../../third_party/spdlog/include/
    ../../../third_party/ue5navmesh/Public/
    ../../../third_party/xxhash/
    ../../../third_party/zlib/
    ../../generated/
    ../../libs/
    ../../libs/engine/
    ../../libs/engine/config/
    ../../libs/engine/core/
    ../../libs/engine/infra/
    ../../libs/engine/modules/
    ../../libs/services/player/
    ../../libs/services/scene/
    ./
    /usr/local/include
)

# Agones 生命周期适配器只在 Linux 构建里接 libcurl。
# MMORPG_AGONES_CURL 没定义时,MakeDefaultHttpTransport() 返回 nullptr,
# 整套逻辑退化成 Disabled(Windows / 本地开发构建走的就是这条路)。
add_definitions(-DMMORPG_AGONES_CURL=1)

set(SOURCE_FILES
    agones/agones_rest_client.cpp
    agones/agones_scene_lifecycle.cpp
    handler/event/scene_kafka_command_router.cpp
    handler/event/actor_combat_state_event_handler.cpp
    handler/event/actor_event_handler.cpp
    handler/event/buff_event_handler.cpp
    handler/event/combat_event_handler.cpp
    handler/event/event_handler.cpp
    handler/event/mission_event_handler.cpp
    handler/event/node_event_handler.cpp
    handler/event/npc_event_handler.cpp
    handler/event/player_event_handler.cpp
    handler/event/player_migration_event_handler.cpp
    handler/event/scene_event_handler.cpp
    handler/event/server_event_handler.cpp
    handler/event/skill_event_handler.cpp
    handler/event/team_event_handler.cpp
    handler/rpc/scene_admin_handler.cpp
    handler/rpc/player/player_currency_handler.cpp
    handler/rpc/player/player_attribute_handler.cpp
    handler/rpc/player/player_pet_handler.cpp
    handler/rpc/player/player_rollback_handler.cpp
    handler/rpc/scene_handler.cpp
    handler/rpc/player/client_player_common_handler.cpp
    handler/rpc/player/player_lifecycle_handler.cpp
    handler/rpc/player/s2s_player_scene_handler.cpp
    handler/rpc/player/player_scene_handler.cpp
    handler/rpc/player/player_service_interface.cpp
    handler/rpc/player/player_skill_handler.cpp
    handler/rpc/player/player_state_attribute_sync_handler.cpp
    handler/rpc/register_handler.cpp
    main.cpp
    rpc_replies/gate_service_response_handler.cpp
    rpc_replies/scene_manager_response_handler.cpp
    rpc_replies/player/player_lifecycle_response_handler.cpp
    rpc_replies/player/s2s_player_scene_response_handler.cpp
    rpc_replies/player/player_currency_response_handler.cpp
    rpc_replies/player/player_rollback_response_handler.cpp
    rpc_replies/player/player_rpc_response_handler.cpp
    rpc_replies/player/player_state_attribute_sync_response_handler.cpp
    rpc_replies/register_response_handler.cpp
    # 2026-09-18 补登记:这 7 个在 scene.vcxproj 里都有,只有 Linux CMake 清单漏了。
    handler/event/battle_event_handler.cpp
    handler/grpc/scene_node_service.cpp
    handler/rpc/player/player_activity_handler.cpp
    handler/rpc/player/player_bag_handler.cpp
    handler/rpc/player/player_mission_handler.cpp
    handler/rpc/player/player_movement_handler.cpp
    id_segment_bootstrap.cpp
)

find_package(absl CONFIG REQUIRED)
find_package(protobuf CONFIG REQUIRED)
find_package(gRPC CONFIG REQUIRED)
# 用系统 libcurl(deps 阶段 apt 装的 libcurl4-openssl-dev)。
# 刻意不 vendor curl 源码:third_party/curl 子模块当前未初始化,而且
# Agones REST 只需要几个回环 GET/POST,再引一份第三方源码得不偿失。
find_package(CURL REQUIRED)

link_directories(
    "${CMAKE_CURRENT_SOURCE_DIR}/../../../lib"
    /usr/local/lib
    /usr/lib/x86_64-linux-gnu
)

add_executable(scene ${SOURCE_FILES})

target_link_libraries(scene
    -Wl,--start-group
    -lgate
    -lscene
    -lmodules
    -lthread_context
    -linfra
    -lsession
    -lconfig
    -lcore
    -lgrpc_client
    -ltable
    -lproto_helpers
    -lrpc
    -lproto
    -lthird_party
    -Wl,--end-group
    gRPC::grpc++
    gRPC::grpc++_reflection
    gRPC::grpc
    gRPC::gpr
    CURL::libcurl
    protobuf::libprotobuf
    muduo_net
    muduo_base
    rdkafka++
    rdkafka
    yaml-cpp
    hiredis
    z
    ssl
    crypto
    pthread
    dl
    absl::absl_check
    absl::absl_log
    absl::algorithm
    absl::base
    absl::bind_front
    absl::bits
    absl::btree
    absl::cleanup
    absl::cord
    absl::core_headers
    absl::debugging
    absl::die_if_null
    absl::dynamic_annotations
    absl::flags
    absl::flags_parse
    absl::flat_hash_map
    absl::flat_hash_set
    absl::function_ref
    absl::hash
    absl::layout
    absl::log_initialize
    absl::log_severity
    absl::memory
    absl::node_hash_map
    absl::node_hash_set
    absl::optional
    absl::span
    absl::status
    absl::statusor
    absl::strings
    absl::synchronization
    absl::time
    absl::type_traits
    absl::utility
    absl::variant
)


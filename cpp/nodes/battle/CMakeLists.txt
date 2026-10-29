# === GENERATED FILE -- DO NOT EDIT ===
# Produced by tools/archived/vcxproj2cmake.py from battle.vcxproj.
# Every Linux build regenerates it; hand edits are lost without warning.
# Change the generator instead.

cmake_minimum_required(VERSION 3.22)
project(battle)

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
add_definitions(-DGOOGLE_CHECK_EQ=ABSL_CHECK_EQ)
add_definitions(-DGOOGLE_LOG=ABSL_LOG)
add_definitions(-DGOOGLE_DCHECK=ABSL_DCHECK)
add_definitions(-DMMORPG_AGONES_CURL=1)

include_directories(
    ../../../third_party/
    ../../../third_party/boost/
    ../../../third_party/entt/single_include
    ../../../third_party/fmt/include
    ../../../third_party/librdkafka/src-cpp/
    ../../../third_party/muduo-linux/
    ../../../third_party/redis/
    ../../../third_party/redis/deps/
    ../../../third_party/spdlog/include
    ../../../third_party/spdlog/include/
    ../../../third_party/ue5navmesh/Public/
    ../../../third_party/xxhash/
    ../../../third_party/yaml-cpp/include/
    ../../../third_party/zlib/
    ../../generated/
    ../../libs/
    ../../libs/engine/
    ../../libs/engine/config/
    ../../libs/engine/core/
    ../../libs/engine/infra/
    ../../libs/engine/modules/
    ../../libs/services/battle/
    ../../libs/services/player/
    ./
    /usr/local/include
)

set(SOURCE_FILES
    handler/grpc/battle_node.cpp
    handler/rpc/battle_handler.cpp
    handler/rpc/player/player_service_interface.cpp
    logic/battle_room_manager.cpp
    client/battle_client_edge.cpp
    main.cpp
    rpc_replies/register_handler.cpp
)

find_package(absl CONFIG REQUIRED)
find_package(protobuf CONFIG REQUIRED)
find_package(OpenSSL CONFIG REQUIRED PATHS /usr/local/lib/cmake/OpenSSL NO_DEFAULT_PATH)

link_directories(
    "${CMAKE_CURRENT_SOURCE_DIR}/../../../lib"
    /usr/local/lib
    /usr/lib/x86_64-linux-gnu
)

add_executable(battle ${SOURCE_FILES})

target_link_libraries(battle
    -Wl,--start-group
    -lgate
    -lscene
    -lbattle
    -lmodules
    -lmuduo_grpc
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
    protobuf::libprotobuf
    -Wl,--exclude-libs,libnghttp2.a
    nghttp2
    muduo_protobuf_codec
    muduo_hiredis
    muduo_http
    muduo_net
    muduo_base
    rdkafka++
    rdkafka
    yaml-cpp
    hiredis
    curl
    z
    zstd
    OpenSSL::SSL
    OpenSSL::Crypto
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


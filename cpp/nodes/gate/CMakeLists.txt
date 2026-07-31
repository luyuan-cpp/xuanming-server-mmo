cmake_minimum_required(VERSION 3.22)
project(gate)

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
add_definitions(-DMMORPG_AGONES_CURL=1)

include_directories(
    ../../../third_party/
    ../../../third_party/boost/
    ../../../third_party/entt/single_include
    ../../../third_party/fmt/include
    ../../../third_party/grpc/third_party/boringssl-with-bazel/include/
    ../../../third_party/librdkafka/src-cpp/
    ../../../third_party/muduo-linux/
    ../../../third_party/openssl/include/
    ../../../third_party/redis/
    ../../../third_party/redis/deps/
    ../../../third_party/spdlog/include
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
    ../../libs/services/gate/
    ../../libs/services/player/
    ../common/
    ../common/src/pb/pbc/
    ../common/src/pb/pbc/common_proto/
    ../third_party/
    ../third_party/boost/
    ../third_party/muduo-linux/
    ../third_party/protobuf/src/
    ../third_party/protobuf/third_party/abseil-cpp/
    ../third_party/redis/deps/
    ../third_party/zlib/
    ./
    /usr/local/include
)

set(SOURCE_FILES
    handler/event/event_handler.cpp
    handler/event/gate_event_handler.cpp
    handler/event/gate_kafka_command_router.cpp
    handler/event/player_event_handler.cpp
    handler/rpc/client_message_processor.cpp
    handler/rpc/gate_service_handler.cpp
    handler/rpc/player/player_service_interface.cpp
    main.cpp
    rpc_replies/scene_admin_response_handler.cpp
    rpc_replies/scene_response_handler.cpp
    rpc_replies/player/player_rpc_response_handler.cpp
    rpc_replies/register_handler.cpp
    rpc_replies/register_response_handler.cpp
    rpc_replies/route_message_response_handler.cpp
)

find_package(absl CONFIG REQUIRED)
find_package(protobuf CONFIG REQUIRED)
find_package(gRPC CONFIG REQUIRED)

link_directories(
    "${CMAKE_CURRENT_SOURCE_DIR}/../../../lib"
    /usr/local/lib
    /usr/lib/x86_64-linux-gnu
)

add_executable(gate ${SOURCE_FILES})

target_link_libraries(gate
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
    protobuf::libprotobuf
    muduo_protobuf_codec
    muduo_hiredis
    muduo_net
    muduo_base
    rdkafka++
    rdkafka
    yaml-cpp
    hiredis
    curl
    z
    zstd
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


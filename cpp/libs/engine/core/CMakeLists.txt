cmake_minimum_required(VERSION 3.22)
project(core)

set(CMAKE_VERBOSE_MAKEFILE ON)
set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -O0 -g -ggdb")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wall -Wextra")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wno-unused-parameter -Wno-sign-conversion")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -pthread -fPIC")

set(LIBRARY_OUTPUT_PATH "${CMAKE_CURRENT_SOURCE_DIR}/../../../../lib")

add_definitions(-D__LINUX__)
add_definitions(-D__linux__)
add_definitions(-DNOMINMAX)
add_definitions(-DENTT_ID_TYPE=uint64_t)
add_definitions(-DABSL_PROPAGATE_CXX_STD=TRUE)
add_definitions(-DMMORPG_AGONES_CURL=1)

include_directories(
    ../
    ../../
    ../../....//third_party/redis/
    ../../../../third_party/
    ../../../../third_party/boost/
    ../../../../third_party/entt/single_include
    ../../../../third_party/fmt/include
    ../../../../third_party/grpc/third_party/
    ../../../../third_party/librdkafka/src-cpp/
    ../../../../third_party/lua/src/
    ../../../../third_party/muduo-linux/
    ../../../../third_party/openssl/include/
    ../../../../third_party/redis/
    ../../../../third_party/redis/deps/
    ../../../../third_party/sol2/include/
    ../../../../third_party/spdlog/include
    ../../../../third_party/zlib/
    ../../../generated/
    ../../../generated/grpc_client/
    ../config/
    ../configs/
    ../infra/
    ./
    /usr/local/include
)

set(SOURCE_FILES
    log/system/console_log.cpp
    message_limiter/message_limiter.cpp
    network/codec/codec.cpp
    network/game_channel.cpp
    network/message_statistics.cpp
    network/network_utils.cpp
    network/node_message_utils.cpp
    network/node_utils.cpp
    network/player_message_utils.cpp
    network/process_info.cpp
    network/rpc_server.cc
    network/traffic_statistics.cpp
    node/system/etcd/etcd_helper.cpp
    node/system/etcd/etcd_manager.cpp
    node/system/etcd/etcd_service.cpp
    node/system/node/node.cpp
    node/system/node/node_allocator.cpp
    node/system/node/node_connector.cpp
    node/system/node/node_util.cpp
    node/system/registration/registration_manager.cpp
    node/system/discovery/service_discovery_manager.cpp
    time/comp/timer_task_boost_comp.cpp
    time/comp/timer_task_comp.cpp
    time/system/time.cpp
    node/system/zone_utils.cpp
    utils/encode/base64.cpp
    utils/encode/sha256.cpp
    utils/file/file2string.cpp
    utils/log/spdlog_file.cpp
    utils/proto/message_util.cpp
    utils/proto/proto_field_checker.cpp
    utils/random/random.cpp
    utils/registry/game_registry.cpp
    utils/stat/stat.cpp
)

add_library(core STATIC ${SOURCE_FILES})


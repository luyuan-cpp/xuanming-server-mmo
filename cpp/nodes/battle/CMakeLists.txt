cmake_minimum_required(VERSION 3.22)
project(battle_node)

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
    ../../../third_party/librdkafka/src-cpp/
    ../../../third_party/muduo-linux/
    ../../../third_party/redis/
    ../../../third_party/redis/deps/
    ../../../third_party/spdlog/include/
    ../../../third_party/xxhash/
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
    client/battle_client_edge.cpp
    handler/event/event_handler.cpp
    # handler/grpc/battle_node.cpp 由 proto 生成器写出(gRPC handler 骨架,
    # 守护段内容见任务返回的 guard_fills),生成前本工程不可编译。
    handler/grpc/battle_node.cpp
    handler/grpc/battle_client_player_service.cpp
    handler/rpc/battle_handler.cpp
    handler/rpc/player/player_service_interface.cpp
    logic/battle_room_manager.cpp
    main.cpp
    rpc_replies/player/player_rpc_response_handler.cpp
    rpc_replies/register_handler.cpp
    rpc_replies/register_response_handler.cpp
)

find_package(absl CONFIG REQUIRED)
find_package(protobuf CONFIG REQUIRED)
find_package(gRPC CONFIG REQUIRED)

link_directories(
    "${CMAKE_CURRENT_SOURCE_DIR}/../../../lib"
    /usr/local/lib
    /usr/lib/x86_64-linux-gnu
)

add_executable(battle ${SOURCE_FILES})

target_link_libraries(battle
    -Wl,--start-group
    -lbattle
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
)

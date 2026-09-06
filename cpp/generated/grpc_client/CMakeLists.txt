cmake_minimum_required(VERSION 3.22)
project(grpc_client)

set(CMAKE_VERBOSE_MAKEFILE ON)
set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -O0 -g -ggdb")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wall -Wextra")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wno-unused-parameter -Wno-sign-conversion")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -pthread -fPIC")

set(LIBRARY_OUTPUT_PATH "${CMAKE_CURRENT_SOURCE_DIR}/../../../lib")

add_definitions(-D__LINUX__)
add_definitions(-D__linux__)
add_definitions(-DNOMINMAX)
add_definitions(-DENTT_ID_TYPE=uint64_t)
add_definitions(-DABSL_PROPAGATE_CXX_STD=TRUE)
add_definitions(-DMMORPG_AGONES_CURL=1)

include_directories(
    ../
    ../../../third_party/
    ../../../third_party/boost/
    ../../../third_party/entt/single_include
    ../../../third_party/fmt/include
    ../../../third_party/muduo-linux/
    ../../../third_party/spdlog/include
    ../../libs/engine/
    ../third_party/protobuf/src/
    ../third_party/protobuf/third_party/abseil-cpp/
    ./
    ./src/
    /usr/local/include
)

set(SOURCE_FILES
    chat/chat_grpc_client.cpp
    client_rpc_router/client_rpc_router_grpc_client.cpp
    data_service/data_service_grpc_client.cpp
    db/db_grpc_client.cpp
    etcd/etcd_grpc_client.cpp
    friend/friend_grpc_client.cpp
    grpc_init_client.cpp
    guild/guild_grpc_client.cpp
    login/login_grpc_client.cpp
    scene_manager/scene_manager_service_grpc_client.cpp
    scene_manager/scene_node_service_grpc_client.cpp
)

add_library(grpc_client STATIC ${SOURCE_FILES})


cmake_minimum_required(VERSION 3.22)
project(gate)

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
    ../../
    ../../....//third_party/redis/
    ../../../../third_party/
    ../../../../third_party/boost/
    ../../../../third_party/entt/single_include
    ../../../../third_party/fmt/include
    ../../../../third_party/grpc/third_party/
    ../../../../third_party/hexagons_grids/
    ../../../../third_party/librdkafka/src-cpp/
    ../../../../third_party/lua/src/
    ../../../../third_party/muduo-linux/
    ../../../../third_party/recastnavigation/
    ../../../../third_party/redis/
    ../../../../third_party/redis/deps/
    ../../../../third_party/sol2/include/
    ../../../../third_party/spdlog/include
    ../../../../third_party/ue5navmesh/Public
    ../../../../third_party/ue5navmesh/Public/
    ../../../../third_party/xxhash/
    ../../../../third_party/zlib/
    ../../../generated/
    ../../engine/
    ../../engine/config/
    ../../engine/core/
    ../../engine/infra/
    ../../engine/infran/
    ../../engine/modules/
    ../third_party/
    ../third_party/boost/
    ../third_party/lua/src/
    ../third_party/muduo-linux/
    ../third_party/mysql/include/
    ../third_party/protobuf/src/
    ../third_party/protobuf/third_party/abseil-cpp/
    ../third_party/redis/
    ../third_party/redis/deps/
    ../third_party/sol2/include/
    ../third_party/zlib/
    ./
    /usr/local/include
    src/pb/pbc/common_proto/
    src/pb/pbc/logic_proto/
)

set(SOURCE_FILES
    session/manager/session_manager.cpp
)

add_library(gate STATIC ${SOURCE_FILES})


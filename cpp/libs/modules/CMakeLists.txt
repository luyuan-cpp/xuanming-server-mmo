cmake_minimum_required(VERSION 3.22)
project(modules)

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
    ../../
    ../../../third_party/
    ../../../third_party/boost/
    ../../../third_party/entt/single_include
    ../../../third_party/fmt/include
    ../../../third_party/grpc/third_party/
    ../../../third_party/librdkafka/src-cpp/
    ../../../third_party/lua/src/
    ../../../third_party/muduo-linux/
    ../../../third_party/redis/
    ../../../third_party/redis/deps/
    ../../../third_party/sol2/include/
    ../../../third_party/spdlog/include
    ../../../third_party/zlib/
    ../../generated/
    ../engine/
    ../engine/config/
    ../engine/infra/
    ../engine/thread_context/
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
    bag/bag_system.cpp
    bag/bag_service.cpp
    bag/bag_profile_registry.cpp
    bag/container_layout.cpp
    bag/eviction_policy.cpp
    bag/item_store.cpp
    condition/condition_util.cpp
    currency/system/currency_system.cpp
    gain_block/gain_block_service.cpp
    mission/comp/mission_comp.cpp
    mission/system/mission.cpp
    snapshot/snapshot_system.cpp
    transaction_log/anomaly_detector.cpp
    transaction_log/transaction_log_system.cpp
)

add_library(modules STATIC ${SOURCE_FILES})


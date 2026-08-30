cmake_minimum_required(VERSION 3.22)
project(scene)

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
    actor/action_state/system/actor_action_state.cpp
    actor/attribute/system/actor_attribute_calculator.cpp
    actor/attribute/system/actor_state_attribute_sync.cpp
    combat/buff/system/buff.cpp
    combat/buff/system/buff_impl.cpp
    combat/buff/system/modifier_buff_impl.cpp
    combat/buff/system/motion_modifier_impl.cpp
    combat/skill/system/skill.cpp
    combat_state/system/combat_state.cpp
    core/config/config.cpp
    core/system/id_generator.cpp
    core/system/redis.cpp
    frame/manager/frame_time.cpp
    generated/attribute/actorbaseattributess2c_attribute_sync.cpp
    generated/attribute/attributedelta10framess2c_attribute_sync.cpp
    generated/attribute/attributedelta2framess2c_attribute_sync.cpp
    generated/attribute/attributedelta30framess2c_attribute_sync.cpp
    generated/attribute/attributedelta5framess2c_attribute_sync.cpp
    generated/attribute/attributedelta60framess2c_attribute_sync.cpp
    kafka/system/kafka.cpp
    player/system/afk.cpp
    player/system/player_database_loader.cpp
    player/system/player_database_1_loader.cpp
    player/system/player_lifecycle.cpp
    player/system/player_scene.cpp
    player/system/player_skill.cpp
    player/system/player_tip.cpp
    player/system/stress_test_probe.cpp
    player/system/bag_marshal.cpp
    player/system/cross_zone_reaper.cpp
    spatial/system/aoi.cpp
    spatial/system/grid.cpp
    spatial/system/interest.cpp
    spatial/system/movement.cpp
    spatial/system/movement_acceleration.cpp
    spatial/system/nav_query.cpp
    spatial/system/navigation.cpp
    spatial/system/recast.cpp
    spatial/system/scene_crowd.cpp
    spatial/system/view.cpp
    world/world.cpp
)

add_library(scene STATIC ${SOURCE_FILES})


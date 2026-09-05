cmake_minimum_required(VERSION 3.22)
project(table)

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
    ../../../third_party/
    ../../../third_party/boost/
    ../../../third_party/entt/single_include
    ../../../third_party/exprtk/
    ../../../third_party/fmt/include
    ../../../third_party/muduo-linux
    ../../../third_party/spdlog/include
    ../../generated/
    ../../libs/
    ../../libs/base/common/
    ../../libs/engine/
    ../../libs/engine/config/
    ../third_party/protobuf/src/
    ../third_party/protobuf/third_party/abseil-cpp/
    ./src/
    /usr/local/include
)

set(SOURCE_FILES
    code/actoractioncombatstate_table.cpp
    code/actoractionstate_table.cpp
    code/all_table.cpp
    code/basescene_table.cpp
    code/buff_table.cpp
    code/class_table.cpp
    code/condition_table.cpp
    code/cooldown_table.cpp
    code/dungeon_table.cpp
    code/equipslot_table.cpp
    code/dungeon_table_fk.cpp
    code/globalvariable_table.cpp
    code/item_table.cpp
    code/messagelimiter_table.cpp
    code/mirror_table.cpp
    code/mirror_table_fk.cpp
    code/mission_table.cpp
    code/mission_table_fk.cpp
    code/monster_table.cpp
    code/reward_table.cpp
    code/skillpermission_table.cpp
    code/skill_table.cpp
    code/testmultikey_table.cpp
    code/testmultikey_table_fk.cpp
    code/test_table.cpp
    code/world_table.cpp
    code/world_table_fk.cpp
    proto/actoractioncombatstate_table.pb.cc
    proto/actoractionstate_table.pb.cc
    proto/basescene_table.pb.cc
    proto/buff_table.pb.cc
    proto/class_table.pb.cc
    proto/condition_table.pb.cc
    proto/cooldown_table.pb.cc
    proto/dungeon_table.pb.cc
    proto/equipslot_table.pb.cc
    proto/globalvariable_table.pb.cc
    proto/item_table.pb.cc
    proto/messagelimiter_table.pb.cc
    proto/mirror_table.pb.cc
    proto/mission_table.pb.cc
    proto/monster_table.pb.cc
    proto/operator/ability_operator.pb.cc
    proto/operator/bag_operator.pb.cc
    proto/operator/scene_operator.pb.cc
    proto/reward_table.pb.cc
    proto/skillpermission_table.pb.cc
    proto/skill_table.pb.cc
    proto/testmultikey_table.pb.cc
    proto/test_table.pb.cc
    proto/tip/actor_action_error_tip.pb.cc
    proto/tip/bag_error_tip.pb.cc
    proto/tip/buff_error_tip.pb.cc
    proto/tip/common_error_tip.pb.cc
    proto/tip/cross_server_error_tip.pb.cc
    proto/tip/entity_error_tip.pb.cc
    proto/tip/friend_error_tip.pb.cc
    proto/tip/guild_error_tip.pb.cc
    proto/tip/login_error_tip.pb.cc
    proto/tip/match_error_tip.pb.cc
    proto/tip/mission_error_tip.pb.cc
    proto/tip/mount_error_tip.pb.cc
    proto/tip/reward_error_tip.pb.cc
    proto/tip/scene_error_tip.pb.cc
    proto/tip/skill_error_tip.pb.cc
    proto/tip/team_error_tip.pb.cc
    proto/world_table.pb.cc
)

add_library(table STATIC ${SOURCE_FILES})


cmake_minimum_required(VERSION 3.22)
project(proto)

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
    ../../../third_party/entt/single_include
    ../../../third_party/fmt/include
    ../../../third_party/spdlog/include
    ../third_party/protobuf/src/
    ../third_party/protobuf/third_party/abseil-cpp/
    ./src/
    /usr/local/include
)

set(SOURCE_FILES
    chat/chat.pb.cc
    chat/chat.grpc.pb.cc
    common/base/common.pb.cc
    common/base/config.pb.cc
    common/base/empty.pb.cc
    common/base/gm_admin.pb.cc
    common/base/rpc_message.pb.cc
    common/base/message.pb.cc
    common/base/node.pb.cc
    common/base/session.pb.cc
    common/base/statistics.pb.cc
    common/base/tip.pb.cc
    common/base/user_accounts.pb.cc
    common/component/actor_attribute_state_comp.pb.cc
    common/component/actor_combat_state_comp.pb.cc
    common/component/actor_comp.pb.cc
    common/component/buff_comp.pb.cc
    common/component/base_comp.pb.cc
    common/component/currency_comp.pb.cc
    common/component/frame_comp.pb.cc
    common/component/node_player_comp.pb.cc
    common/component/item_base_comp.pb.cc
    common/component/item_comp.pb.cc
    common/component/mission_comp.pb.cc
    common/component/npc_comp.pb.cc
    common/component/player_async_comp.pb.cc
    common/component/player_comp.pb.cc
    common/component/player_login_comp.pb.cc
    common/component/player_network_comp.pb.cc
    common/component/player_scene_comp.pb.cc
    common/component/player_skill_comp.pb.cc
    common/component/scene_comp.pb.cc
    common/component/skill_comp.pb.cc
    common/component/team_comp.pb.cc
    common/component/time_comp.pb.cc
    common/constants/etcd_grpc.pb.cc
    common/constants/function_switch.pb.cc
    common/constants/scene.pb.cc
    common/constants/test_switch.pb.cc
    common/database/mysql_database_table.pb.cc
    common/database/player_cache.pb.cc
    common/database/rollback_database_table.pb.cc
    common/database/bag_quest_mail_data.pb.cc
    common/event/actor_combat_state_event.pb.cc
    common/event/actor_event.pb.cc
    common/event/buff_event.pb.cc
    common/event/combat_event.pb.cc
    common/event/mission_event.pb.cc
    common/event/node_event.pb.cc
    common/event/npc_event.pb.cc
    common/event/player_event.pb.cc
    common/event/player_migration_event.pb.cc
    common/event/scene_event.pb.cc
    common/event/server_event.pb.cc
    common/event/skill_event.pb.cc
    common/rollback/player_snapshot.pb.cc
    common/rollback/transaction_log.pb.cc
    contracts/kafka/centre_command.pb.cc
    contracts/kafka/gate_command.pb.cc
    contracts/kafka/gate_event.pb.cc
    contracts/kafka/player_event.pb.cc
    contracts/kafka/scene_command.pb.cc
    data_service/data_service.pb.cc
    data_service/data_service.grpc.pb.cc
    db/db.pb.cc
    db/db.grpc.pb.cc
    db/db_task.pb.cc
    db/db_task.grpc.pb.cc
    db/proto_option.grpc.pb.cc
    db/proto_option.pb.cc
    etcd/etcd.grpc.pb.cc
    etcd/etcd.pb.cc
    etcd/kv.grpc.pb.cc
    etcd/kv.pb.cc
    etcd/version.grpc.pb.cc
    etcd/version.pb.cc
    friend/friend.grpc.pb.cc
    friend/friend.pb.cc
    gate/gate_service.pb.cc
    guild/guild.grpc.pb.cc
    guild/guild.pb.cc
    login/login.grpc.pb.cc
    login/login.pb.cc
    player/player.pb.cc
    scene/client_player_common.pb.cc
    scene/player_lifecycle.pb.cc
    scene/player_movement.pb.cc
    scene/s2s_player_scene.pb.cc
    scene/scene_admin.pb.cc
    scene/player_currency.pb.cc
    scene/player_rollback.pb.cc
    scene/player_scene.pb.cc
    scene/player_skill.pb.cc
    scene/player_state_attribute_sync.pb.cc
    scene/scene.pb.cc
    scene/scene_info.pb.cc
    scene_manager/scene_manager_service.grpc.pb.cc
    scene_manager/scene_manager_service.pb.cc
    scene_manager/scene_node_service.grpc.pb.cc
    scene_manager/scene_node_service.pb.cc
    scene_manager/storage.grpc.pb.cc
    scene_manager/storage.pb.cc
)

add_library(proto STATIC ${SOURCE_FILES})


cmake_minimum_required(VERSION 3.22)
project(third_party)

set(CMAKE_VERBOSE_MAKEFILE ON)
set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -O0 -g -ggdb")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wall -Wextra")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -Wno-unused-parameter -Wno-sign-conversion")
set(CMAKE_CXX_FLAGS "${CMAKE_CXX_FLAGS} -pthread -fPIC")

set(LIBRARY_OUTPUT_PATH "${CMAKE_CURRENT_SOURCE_DIR}/../lib")

add_definitions(-D__LINUX__)
add_definitions(-D__linux__)
add_definitions(-DNOMINMAX)
add_definitions(-DENTT_ID_TYPE=uint64_t)
add_definitions(-DABSL_PROPAGATE_CXX_STD=TRUE)
add_definitions(-DMMORPG_AGONES_CURL=1)

include_directories(
    ../third_party/
    ../third_party/entt/single_include
    ../third_party/fmt/include
    ../third_party/googletest/googletest/include/
    ../third_party/muduo-linux/
    ../third_party/muduo-linux/contrib/
    ../third_party/protobuf/third_party/abseil-cpp/
    ../third_party/protobuf/third_party/utf8_range/
    ../third_party/redis
    ../third_party/redis/deps/
    ../third_party/spdlog/include
    ../third_party/ue5navmesh/Public/
    ../third_party/zlib/
    ./
    ./boost/
    ./protobuf/src/
    /usr/local/include
)

set(SOURCE_FILES
    hexagons_grids/hexagons_grid.cpp
    ue5navmesh/Private/DetourCrowd/DetourCrowd.cpp
    ue5navmesh/Private/DetourCrowd/DetourLocalBoundary.cpp
    ue5navmesh/Private/DetourCrowd/DetourObstacleAvoidance.cpp
    ue5navmesh/Private/DetourCrowd/DetourPathCorridor.cpp
    ue5navmesh/Private/DetourCrowd/DetourPathQueue.cpp
    ue5navmesh/Private/DetourCrowd/DetourProximityGrid.cpp
    ue5navmesh/Private/DetourCrowd/DetourSharedBoundary.cpp
    ue5navmesh/Private/DetourTileCache/DetourTileCache.cpp
    ue5navmesh/Private/DetourTileCache/DetourTileCacheBuilder.cpp
    ue5navmesh/Private/DetourTileCache/DetourTileCacheDetail.cpp
    ue5navmesh/Private/DetourTileCache/DetourTileCacheRegion.cpp
    ue5navmesh/Private/Detour/DetourAlloc.cpp
    ue5navmesh/Private/Detour/DetourCommon.cpp
    ue5navmesh/Private/Detour/DetourNavMesh.cpp
    ue5navmesh/Private/Detour/DetourNavMeshBuilder.cpp
    ue5navmesh/Private/Detour/DetourNavMeshQuery.cpp
    ue5navmesh/Private/Detour/DetourNode.cpp
)

add_library(third_party STATIC ${SOURCE_FILES})


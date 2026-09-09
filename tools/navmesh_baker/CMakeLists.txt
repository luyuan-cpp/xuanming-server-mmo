# 离线导航网格烘焙工具(见 navmesh_baker.cpp 文件头说明)。
#
# 必须与运行时同一套 ue5navmesh 源码/布局编译(dtReal=double、
# WITH_NAVMESH_* 全部不定义),否则产出的 bin 与 scene 节点的
# LoadNavMesh 二进制不兼容。
#
# 构建(Windows,由 Codex 执行):
#   cmake -S tools/navmesh_baker -B tools/navmesh_baker/build
#   cmake --build tools/navmesh_baker/build --config Release
# Linux:
#   cmake -S tools/navmesh_baker -B tools/navmesh_baker/build && cmake --build tools/navmesh_baker/build

cmake_minimum_required(VERSION 3.22)
project(navmesh_baker CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

set(UE5NAVMESH_DIR ${CMAKE_CURRENT_SOURCE_DIR}/../../third_party/ue5navmesh)

file(GLOB RECAST_SOURCES ${UE5NAVMESH_DIR}/Private/Recast/*.cpp)
set(DETOUR_SOURCES
    ${UE5NAVMESH_DIR}/Private/Detour/DetourAlloc.cpp
    ${UE5NAVMESH_DIR}/Private/Detour/DetourCommon.cpp
    ${UE5NAVMESH_DIR}/Private/Detour/DetourNavMesh.cpp
    ${UE5NAVMESH_DIR}/Private/Detour/DetourNavMeshBuilder.cpp
    ${UE5NAVMESH_DIR}/Private/Detour/DetourNavMeshQuery.cpp
    ${UE5NAVMESH_DIR}/Private/Detour/DetourNode.cpp
)

add_executable(navmesh_baker
    navmesh_baker.cpp
    ${RECAST_SOURCES}
    ${DETOUR_SOURCES}
)

target_include_directories(navmesh_baker PRIVATE ${UE5NAVMESH_DIR}/Public)

if(WIN32)
    # 宏对齐 scene.vcxproj / third_party.vcxproj 里 ue5navmesh 的编译配置:
    #   WIN32            CoreMinimal.h 用它决定 TEXT/Windows.h 分支;
    #   UNICODE/_UNICODE + _TCHAR_DEFINED
    #                    CoreMinimal.h 先 #include <Windows.h> 再 `using TCHAR = wchar_t;`,
    #                    没有 UNICODE 时 winnt.h 会先 typedef char TCHAR,与之冲突(C2371);
    #                    vcxproj 那边靠 CharacterSet=Unicode + 显式 _TCHAR_DEFINED 躲过,这里照抄。
    target_compile_definitions(navmesh_baker PRIVATE
        WIN32 UNICODE _UNICODE _TCHAR_DEFINED NOMINMAX WIN32_LEAN_AND_MEAN _CRT_SECURE_NO_WARNINGS)
endif()

if(MSVC)
    # 源码是无 BOM 的 UTF-8(含中文注释与字符串):不加 /utf-8 在 CP936 机器上会按 GBK
    # 读,C4819 满屏、字符串里的多字节字符被改写。scene.vcxproj 同样带 /utf-8。
    # /FIcassert:ue5navmesh 的 DetourNavMeshBuilder.cpp 裸用 assert(false),而
    # DetourAssert.h 只在 Debug 才 #include <assert.h>;scene 是 Debug 构建碰不到,
    # 烘焙器 Release 构建会 C3861 "assert 找不到标识符",强制包含一次即可
    # (NDEBUG 下 assert 展开为空,不改变行为)。
    target_compile_options(navmesh_baker PRIVATE /utf-8 /bigobj /FIcassert)
else()
    target_compile_options(navmesh_baker PRIVATE -include cassert)
endif()

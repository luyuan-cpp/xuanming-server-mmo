cmake_minimum_required(VERSION 3.20)
project(redis_lifecycle_test LANGUAGES CXX)

if(NOT CMAKE_SYSTEM_NAME STREQUAL "Linux")
    message(FATAL_ERROR "该回归工程只适用于 Linux 的 timerfd 事件分发")
endif()

set(MMORPG_SOURCE_ROOT "/src" CACHE PATH "本轮仓库源码根目录")
set(MMORPG_SDK_ROOT "/src" CACHE PATH "已锁定依赖的 SDK 源码根目录")
set(MMORPG_GTEST_ROOT
    "${MMORPG_SOURCE_ROOT}/third_party/googletest/googletest"
    CACHE PATH "锁定的 GoogleTest 源码目录")
option(MMORPG_ENABLE_SANITIZERS "启用地址及未定义行为检查" ON)

if(NOT CMAKE_BUILD_TYPE)
    set(CMAKE_BUILD_TYPE Debug CACHE STRING "构建类型" FORCE)
endif()
if(NOT CMAKE_BUILD_TYPE STREQUAL "Debug")
    message(FATAL_ERROR "生命周期回归必须使用保留断言的 Debug 构建")
endif()
set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
find_package(Threads REQUIRED)

set(boost_root "${MMORPG_SDK_ROOT}/third_party/boost")
file(STRINGS "${boost_root}/boost/version.hpp" boost_version
    REGEX "^#define BOOST_VERSION 109200$")
if(NOT boost_version)
    message(FATAL_ERROR "必须使用 SDK 锁定的 Boost 1.92.0 头文件")
endif()
if(NOT EXISTS "${MMORPG_GTEST_ROOT}/src/gtest-all.cc")
    message(FATAL_ERROR "缺少锁定的 GoogleTest 源码，不回退到系统版本")
endif()

# SDK 中的静态库带 NDEBUG，不能验证 removeChannel 断言；在独立目录重编原源码及本轮覆盖件。
set(muduo_root "${CMAKE_CURRENT_BINARY_DIR}/muduo-source")
set(sdk_muduo "${MMORPG_SDK_ROOT}/third_party/muduo-linux")
file(GLOB_RECURSE overlay_files CONFIGURE_DEPENDS
    RELATIVE "${MMORPG_SOURCE_ROOT}/tools/archived/muduo_linux_overlay"
    "${MMORPG_SOURCE_ROOT}/tools/archived/muduo_linux_overlay/*")
file(GLOB_RECURSE sdk_files CONFIGURE_DEPENDS RELATIVE "${sdk_muduo}"
    "${sdk_muduo}/muduo/*.h" "${sdk_muduo}/muduo/*.cc"
    "${sdk_muduo}/contrib/*.h" "${sdk_muduo}/contrib/*.cc")
# COPY 会保留源时间戳，新覆盖件或删除覆盖件后还原的源码可能比已有 .o 更旧。
# 基线与覆盖件均逐项 COPYONLY，使实际内容变化更新时间并驱动增量重编。
foreach(relative IN LISTS sdk_files)
    if(NOT relative IN_LIST overlay_files)
        configure_file("${sdk_muduo}/${relative}" "${muduo_root}/${relative}" COPYONLY)
    endif()
endforeach()
# 删除仅存在于 overlay 的文件时也清除独立构建树的旧副本，不能继续编译残留实现。
set(overlay_manifest "${CMAKE_CURRENT_BINARY_DIR}/overlay-files.txt")
if(EXISTS "${overlay_manifest}")
    file(STRINGS "${overlay_manifest}" previous_overlay_files)
    foreach(relative IN LISTS previous_overlay_files)
        if(NOT relative IN_LIST overlay_files AND NOT EXISTS "${sdk_muduo}/${relative}")
            file(REMOVE "${muduo_root}/${relative}")
        endif()
    endforeach()
endif()
foreach(relative IN LISTS overlay_files)
    configure_file("${MMORPG_SOURCE_ROOT}/tools/archived/muduo_linux_overlay/${relative}"
        "${muduo_root}/${relative}" COPYONLY)
endforeach()
string(REPLACE ";" "\n" overlay_manifest_content "${overlay_files}")
file(WRITE "${overlay_manifest}" "${overlay_manifest_content}\n")
if(NOT EXISTS "${muduo_root}/muduo/contrib")
    file(CREATE_LINK "../contrib" "${muduo_root}/muduo/contrib" SYMBOLIC)
endif()
file(GLOB muduo_sources CONFIGURE_DEPENDS
    "${muduo_root}/muduo/base/*.cc"
    "${muduo_root}/muduo/net/*.cc"
    "${muduo_root}/muduo/net/poller/*.cc")

add_library(lifecycle_muduo STATIC ${muduo_sources}
    "${muduo_root}/contrib/hiredis/Hiredis.cc")
target_include_directories(lifecycle_muduo PUBLIC "${muduo_root}")
target_include_directories(lifecycle_muduo SYSTEM PUBLIC "${boost_root}")
find_library(hiredis_library NAMES hiredis PATHS /usr/local/lib NO_DEFAULT_PATH REQUIRED)
target_link_libraries(lifecycle_muduo PUBLIC Threads::Threads rt "${hiredis_library}")

add_library(lifecycle_gtest STATIC
    "${MMORPG_GTEST_ROOT}/src/gtest-all.cc"
    "${MMORPG_GTEST_ROOT}/src/gtest_main.cc")
target_include_directories(lifecycle_gtest PUBLIC "${MMORPG_GTEST_ROOT}/include"
    PRIVATE "${MMORPG_GTEST_ROOT}")
target_link_libraries(lifecycle_gtest PUBLIC Threads::Threads)

add_executable(redis_lifecycle_test
    redis_manager_event_batch_test.cpp
    hiredis_event_batch_test.cpp
    "${MMORPG_SOURCE_ROOT}/cpp/tests/rpc_controller_test/hiredis_command_lifecycle_test.cpp"
    "${MMORPG_SOURCE_ROOT}/cpp/tests/rpc_controller_test/redis_manager_auth_test.cpp"
    "${MMORPG_SOURCE_ROOT}/cpp/libs/engine/thread_context/redis_manager.cpp")
target_include_directories(redis_lifecycle_test PRIVATE
    "${MMORPG_SOURCE_ROOT}/cpp/libs"
    "${MMORPG_SOURCE_ROOT}/cpp/libs/engine"
    "${MMORPG_SDK_ROOT}/third_party")
target_link_libraries(redis_lifecycle_test PRIVATE lifecycle_muduo lifecycle_gtest)

add_executable(timer_queue_lifecycle_test
    "${MMORPG_SOURCE_ROOT}/cpp/tests/timer_queue_unit_test/timer_queue_unit_test.cpp"
    "${MMORPG_SOURCE_ROOT}/cpp/tests/timer_queue_unit_test/muduo_base_contract_test.cpp"
    "${MMORPG_SOURCE_ROOT}/cpp/libs/engine/core/time/comp/timer_task_comp.cpp")
target_include_directories(timer_queue_lifecycle_test PRIVATE
    "${MMORPG_SOURCE_ROOT}/cpp/libs/engine/core"
    "${MMORPG_SOURCE_ROOT}/cpp/libs/services/scene"
    "${MMORPG_SOURCE_ROOT}/cpp/generated"
    "${MMORPG_SOURCE_ROOT}/third_party")
target_link_libraries(timer_queue_lifecycle_test PRIVATE lifecycle_muduo lifecycle_gtest)

foreach(target lifecycle_muduo lifecycle_gtest redis_lifecycle_test timer_queue_lifecycle_test)
    target_compile_options(${target} PRIVATE -UNDEBUG -fno-omit-frame-pointer)
    if(MMORPG_ENABLE_SANITIZERS)
        target_compile_options(${target} PRIVATE -fsanitize=address,undefined)
        target_link_options(${target} PRIVATE -fsanitize=address,undefined)
    endif()
endforeach()

enable_testing()
add_test(NAME redis_lifecycle_test COMMAND redis_lifecycle_test)
add_test(NAME timer_queue_lifecycle_test COMMAND timer_queue_lifecycle_test)
set_tests_properties(redis_lifecycle_test timer_queue_lifecycle_test PROPERTIES TIMEOUT 60)

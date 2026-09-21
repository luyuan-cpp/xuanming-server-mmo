cmake_minimum_required(VERSION 3.20)
project(NoRawPointerCheck LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)

# ------ LLVM location (override: cmake -DLLVM_ROOT=<path>) ------
if(NOT DEFINED LLVM_ROOT AND DEFINED ENV{LLVM_ROOT})
    set(LLVM_ROOT "$ENV{LLVM_ROOT}")
endif()
if(NOT DEFINED LLVM_ROOT)
    set(LLVM_ROOT "D:/game/llvm")
endif()

set(LLVM_DIR  "${LLVM_ROOT}/lib/cmake/llvm"  CACHE PATH "")
set(Clang_DIR "${LLVM_ROOT}/lib/cmake/clang"  CACHE PATH "")

find_package(LLVM REQUIRED CONFIG)
find_package(Clang REQUIRED CONFIG)

message(STATUS "LLVM ${LLVM_PACKAGE_VERSION} from ${LLVM_DIR}")

include_directories(SYSTEM ${LLVM_INCLUDE_DIRS} ${CLANG_INCLUDE_DIRS})

# ------ Standalone checker executable ------
add_executable(no_raw_ptr_check
    main.cpp
    NoMemberRawPointerCheck.cpp
    NoMemberRawPointerCheck.h
)

# LLVM support component (pulled transitively by Clang libs, listed for safety)
llvm_map_components_to_libnames(LLVM_LIBS support)

target_link_libraries(no_raw_ptr_check PRIVATE
    clangTooling
    clangASTMatchers
    clangFrontend
    clangDriver
    clangSerialization
    clangParse
    clangSema
    clangAnalysis
    clangAST
    clangBasic
    clangLex
    clangEdit
    clangRewrite
    ${LLVM_LIBS}
)

if(MSVC)
    target_compile_options(no_raw_ptr_check PRIVATE /bigobj /EHsc /utf-8)
endif()

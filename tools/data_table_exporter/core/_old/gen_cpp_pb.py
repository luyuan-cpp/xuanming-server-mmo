#!/usr/bin/env python
# coding=utf-8
import os
import subprocess
import logging
from pathlib import Path
import paths

# 配置日志
logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s")
logger = logging.getLogger(__name__)


def compile_protobuf_files(source_dir: Path, include_dirs: list[Path], output_dir: Path):
    """
    编译 source_dir 下所有 .proto 文件到 C++ 代码。

    Args:
    - source_dir (Path): .proto 文件所在目录
    - include_dirs (list[Path]): 依赖的 proto 头文件目录
    - output_dir (Path): 生成的 C++ 文件输出目录
    """
    proto_files = []

    for dirpath, _, filenames in os.walk(source_dir):
        for filename in filenames:
            if filename.endswith(".proto"):
                abs_file = Path(dirpath) / filename
                rel_path = abs_file.relative_to(source_dir).as_posix()
                proto_files.append(rel_path)

    if not proto_files:
        logger.warning(f"No .proto files found in {source_dir}")
        return

    output_dir.mkdir(parents=True, exist_ok=True)

    # 构建 protoc 命令
    command = ["protoc", f"--proto_path={source_dir.resolve()}"]
    for inc in include_dirs:
        command.append(f"--proto_path={inc.resolve()}")

    command.append(f"--cpp_out={output_dir.resolve()}")
    command.extend(proto_files)

    try:
        # 在 source_dir 下运行，保证相对路径正确
        result = subprocess.run(
            command,
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            cwd=source_dir
        )
        logger.info(f"Compiled all proto files in {source_dir} successfully.")
        if result.stdout:
            logger.info(result.stdout)
        if result.stderr:
            logger.warning(result.stderr)
    except subprocess.CalledProcessError as e:
        logger.error(f"Failed to compile proto files in {source_dir}. Error: {e.stderr}")


def check_protoc_exists():
    """检查系统是否安装 protoc"""
    import shutil
    if not shutil.which("protoc"):
        logger.error("protoc 编译器未找到，请先安装并确保在 PATH 中")
        exit(1)


if __name__ == "__main__":
    check_protoc_exists()

    # 公共依赖目录（protobuf 源码路径、第三方 proto 等）
    common_includes = [
        Path("../../third_party/protobuf/src"),
        Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto",
    ]

    # 多个 proto 源目录配置
    proto_jobs = [
        {
            "source_dir": Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto",
            "output_dir": Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto/cpp",
        },
        {
            "source_dir": Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto/tip",
            "output_dir": Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto/cpp/tip",
        },
        {
            "source_dir": Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto/operator",
            "output_dir": Path(paths.PROJECT_GENERATED_CODE_DIR) / "proto/cpp/operator",
        },
    ]

    for job in proto_jobs:
        compile_protobuf_files(job["source_dir"], common_includes, job["output_dir"])

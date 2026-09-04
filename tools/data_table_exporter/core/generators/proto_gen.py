"""Proto file generator + protoc compiler.

1. Reads each ``.xlsx`` schema and writes a ``{name}_table.proto`` message.
2. Runs ``protoc`` to compile protos → C++ and Go outputs.
"""

from __future__ import annotations

import logging
import subprocess
import tempfile
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from jinja2 import Environment, FileSystemLoader

from core.config_loader import ExporterConfig
from core.file_utils import ensure_dirs, md5_copy, write_file
from core.schema import TableSchema

logger = logging.getLogger(__name__)


class ProtoCompileError(RuntimeError):
    """protoc 未产出完整结果；调用方必须中止，不能继续部署旧文件。"""


# ---------------------------------------------------------------------------
# .proto file generation
# ---------------------------------------------------------------------------

def generate_proto_files(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate ``*_table.proto`` for every data table."""
    ensure_dirs(cfg.proto_dir)
    env = Environment(loader=FileSystemLoader(str(cfg.template_dir), encoding="utf-8"))
    template = env.get_template("proto_table.proto.j2")

    with ThreadPoolExecutor() as pool:
        java_pkg = cfg.java.package if cfg.java.enabled else ""
        for _ in pool.map(
            lambda t: _gen_one_proto(t, template, cfg, java_pkg), tables
        ):
            pass


def _gen_one_proto(table: TableSchema, template, cfg: ExporterConfig, java_package: str = "") -> None:
    try:
        content = template.render(table=table, java_package=java_package)
        out = cfg.proto_dir / f"{table.name.lower()}_table.proto"
        write_file(out, content)
        logger.info("Generated %s", out)
    except Exception as exc:
        raise RuntimeError(
            f"Proto generation failed for {table.name}: {exc}"
        ) from exc


# ---------------------------------------------------------------------------
# protoc compilation
# ---------------------------------------------------------------------------

def compile_proto_python(cfg: ExporterConfig) -> None:
    """Compile all ``.proto`` -> Python ``*_pb2.py`` for binary serialisation."""
    ensure_dirs(cfg.proto_python_output_dir)
    _compile(cfg.proto_dir, cfg.proto_python_output_dir, "python_out", cfg)


def compile_proto_cpp(cfg: ExporterConfig) -> None:
    """Compile all ``.proto`` → C++ using *protoc*."""
    if not cfg.cpp.enabled:
        return
    ensure_dirs(cfg.cpp.proto_output_dir)
    # 根目录与 tip/operator 分开编译。若根调用递归，同一批子目录文件会在数毫秒内
    # 被写两次；Windows 上杀毒/索引器可能占住刚写完的 .pb.cc，第二次覆盖便报
    # Invalid argument，并留下 include 路径不同的半套产物。
    _compile(cfg.proto_dir, cfg.cpp.proto_output_dir, "cpp_out", cfg, recursive=False)

    # Sub-directories (tip, operator)
    for sub in ("tip", "operator"):
        src = cfg.proto_dir / sub
        if src.exists():
            dst = cfg.cpp.proto_output_dir / sub
            ensure_dirs(dst)
            _compile(src, dst, "cpp_out", cfg)


def compile_proto_go(cfg: ExporterConfig) -> None:
    """Compile all ``.proto`` → Go using *protoc*."""
    if not cfg.go.enabled:
        return
    ensure_dirs(cfg.go.proto_output_dir)
    _compile(cfg.proto_dir, cfg.go.proto_output_dir, "go_out", cfg, recursive=False)

    for sub in ("tip", "operator"):
        src = cfg.proto_dir / sub
        if src.exists():
            _compile(src, cfg.go.proto_output_dir, "go_out", cfg)


def compile_proto_java(cfg: ExporterConfig) -> None:
    """Compile all ``.proto`` → Java using *protoc*.

    Java 的 ``java_multiple_files`` 会随着 message 增删改变输出文件集合。直接
    覆盖正式目录只会更新仍存在的类，已删除 message 的旧 ``.java`` 会永久残留，
    甚至与新的 OuterClass 一起把消费工程编译炸掉。因此三批 proto 先全部写入
    同一个空 staging 目录；只有 protoc 全部成功且正式目录没有孤儿文件时才同步。

    孤儿文件不在这里自动删除：生成目录可能正有并行工作，静默删除的风险高于
    一次明确失败。调用者审计并删除报告的生成文件后重跑即可。
    """
    if not cfg.java.enabled:
        return
    ensure_dirs(cfg.java.proto_output_dir)
    with tempfile.TemporaryDirectory(prefix="table-exporter-java-") as temp_dir:
        staging = Path(temp_dir)
        _compile(
            cfg.proto_dir,
            staging,
            "java_out",
            cfg,
            recursive=False,
        )

        for sub in ("tip", "operator"):
            src = cfg.proto_dir / sub
            if src.exists():
                # Java 的目录由 proto 中的 java_package 决定。三批都写同一个输出根，
                # 避免默认包与 tip/operator 子目录各生成一套互不部署的重复类。
                _compile(src, staging, "java_out", cfg)

        expected = {
            path.relative_to(staging)
            for path in staging.rglob("*.java")
            if path.is_file()
        }
        existing = {
            path.relative_to(cfg.java.proto_output_dir)
            for path in cfg.java.proto_output_dir.rglob("*.java")
            if path.is_file()
        }
        orphans = sorted(existing - expected, key=lambda path: path.as_posix())
        if orphans:
            details = "\n  ".join(path.as_posix() for path in orphans)
            raise ProtoCompileError(
                "Java proto 输出目录存在当前 proto 不再生成的陈旧文件；"
                "拒绝部署混合世代产物。请审计并删除：\n  " + details
            )

        md5_copy(staging, cfg.java.proto_output_dir)


def _compile(
    source_dir: Path,
    output_dir: Path,
    out_flag: str,
    cfg: ExporterConfig,
    *,
    recursive: bool = True,
) -> None:
    """Run *protoc* on all ``.proto`` files in *source_dir*."""
    proto_files = _collect_protos(source_dir, recursive=recursive)
    if not proto_files:
        logger.warning("No .proto files in %s", source_dir)
        return

    cmd = [cfg.protoc_command, f"--proto_path={source_dir.resolve()}"]
    for inc in cfg.protoc_extra_includes:
        cmd.append(f"--proto_path={inc}")
    cmd.append(f"--proto_path={cfg.proto_dir.resolve()}")
    cmd.append(f"--{out_flag}={output_dir.resolve()}")

    if out_flag == "go_out":
        cmd.append(f"--go-grpc_out={output_dir.resolve()}")

    cmd.extend(proto_files)

    try:
        result = subprocess.run(
            cmd, check=True, capture_output=True, text=True, cwd=str(source_dir)
        )
        if result.stderr:
            logger.info("protoc stderr: %s", result.stderr.strip())
        logger.info("Compiled %d proto files → %s", len(proto_files), output_dir)
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or exc.stdout or str(exc)).strip()
        raise ProtoCompileError(f"protoc failed in {source_dir}: {detail}") from exc
    except OSError as exc:
        raise ProtoCompileError(f"无法启动 protoc ({cfg.protoc_command}): {exc}") from exc


def _collect_protos(source_dir: Path, *, recursive: bool = True) -> list[str]:
    protos: list[str] = []
    if recursive:
        candidates = source_dir.rglob("*.proto")
    else:
        candidates = source_dir.glob("*.proto")
    for path in candidates:
        if path.is_file():
            protos.append(path.relative_to(source_dir).as_posix())
    return sorted(protos)



def compile_proto_csharp(cfg: ExporterConfig) -> None:
    """Compile all ``.proto`` → C# using *protoc*（供 Unity 客户端使用）。"""
    if not cfg.csharp.enabled:
        return
    ensure_dirs(cfg.csharp.proto_output_dir)
    # 只编根目录下的表 proto。tip/ 与 operator/ 见模块注释：交给客户端自己的
    # 协议生成链，避免 Unity 单 Assembly 里出现两份同名类型。
    _compile(cfg.proto_dir, cfg.csharp.proto_output_dir, "csharp_out", cfg, recursive=False)


# 另外在 core/orchestrator.py 的生成序列里加一行（compile_proto_java 之后）：
#     compile_proto_csharp(cfg)
# 以及 _deploy() 里加：
#     if cfg.csharp.enabled:
#         tasks.extend((d["src"], d["dst"]) for d in cfg.csharp.deploy)

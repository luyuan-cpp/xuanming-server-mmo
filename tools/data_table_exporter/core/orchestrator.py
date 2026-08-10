"""Main orchestrator — coordinates the full export pipeline."""

from __future__ import annotations

import logging
from pathlib import Path
import sys

import argparse

from core.config_loader import ExporterConfig, LangConfig, load_config
from core.excel_reader import read_all_tables
from core.file_utils import ensure_dirs, md5_copy, report_orphans
from core.foreign_key import ForeignKeyReport, validate_foreign_keys
from core.manifest import generate_manifest
from core.generators.bit_index_gen import BitIndexStateError, generate_bit_indexes
from core.generators.comp_gen import generate_comp_headers
from core.generators.config_gen import generate_config_classes
from core.generators.constants_gen import generate_constants
from core.generators.enum_gen import generate_operator_enums, generate_tip_enums
from core.generators.json_gen import generate_json
from core.generators.binary_gen import generate_binary
from core.generators.proto_gen import (
    compile_proto_cpp,
    compile_proto_go,
    compile_proto_java,
    compile_proto_python,
    generate_proto_files,
)
from core.generators.table_id_gen import generate_table_ids
from core.schema import TableSchema

logger: logging.Logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Pipeline
# ---------------------------------------------------------------------------

def run(cfg: ExporterConfig) -> None:
    """Execute the full export pipeline."""
    logger.info("===== Data Table Exporter: START =====")
    _ensure_output_dirs(cfg)

    # Read schemas once — every generator receives this list.
    tables: list[TableSchema] = read_all_tables(cfg)
    logger.info("Read %d table schema(s)", len(tables))

    # 外键校验必须跑在所有生成之前:失配一律**产出不落盘**,
    # 否则一批带坏引用的表会先覆盖掉上一批好产物,再让人去查运行时的空指针。
    report: ForeignKeyReport = validate_foreign_keys(tables, cfg)
    for msg in report.warnings:
        logger.warning("FK: %s", msg)
    if not report.ok:
        for msg in report.errors:
            logger.error("FK: %s", msg)
        logger.error("外键校验失败:%d 条错误,导表中止,产出未落盘", len(report.errors))
        sys.exit(1)

    # Generate
    generate_json(cfg, tables)
    generate_proto_files(cfg, tables)
    generate_operator_enums(cfg)
    generate_tip_enums(cfg)
    compile_proto_cpp(cfg)
    compile_proto_go(cfg)
    compile_proto_java(cfg)
    compile_proto_python(cfg)
    generate_binary(cfg, tables)
    generate_config_classes(cfg, tables)
    generate_comp_headers(cfg, tables)
    generate_table_ids(cfg, tables)
    generate_constants(cfg, tables)
    generate_bit_indexes(cfg, tables)

    # 批次清单必须在所有表数据产物写完之后生成——它记的是产物 sha256,早一步就对不上。
    generate_manifest(cfg, tables)

    # Deploy
    _deploy(cfg)
    logger.info("===== Data Table Exporter: DONE =====")


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _ensure_output_dirs(cfg: ExporterConfig) -> None:
    dirs: list[Path] = [cfg.json_dir, cfg.binary_dir, cfg.proto_dir, cfg.proto_python_output_dir, cfg.state_dir]
    for lang in (cfg.cpp, cfg.go, cfg.java):
        if lang.enabled:
            dirs.extend([
                lang.code_dir, lang.proto_output_dir,
                lang.constants_dir, lang.table_id_dir, lang.bit_index_dir,
            ])
    ensure_dirs(*[d for d in dirs if d and str(d)])


def _deploy(cfg: ExporterConfig) -> None:
    """Copy generated outputs to final destinations (MD5-checked)."""
    logger.info("--- Deploying outputs ---")
    tasks: list[tuple] = []

    if cfg.cpp.enabled:
        tasks.extend((d["src"], d["dst"]) for d in cfg.cpp.deploy)

    if cfg.go.enabled:
        if cfg.go.deploy:
            tasks.extend((d["src"], d["dst"]) for d in cfg.go.deploy)
        else:
            scan: Path = cfg.go.grpc_service_scan_dir
            base: Path = cfg.go.grpc_deploy_base
            if scan.exists() and scan.is_dir():
                for svc in scan.iterdir():
                    if svc.is_dir():
                        tasks.append((cfg.go.code_dir.parent, base / svc.name / "generated"))

    if cfg.java.enabled:
        tasks.extend((d["src"], d["dst"]) for d in cfg.java.deploy)

    ok, fail = 0, 0
    # 同一个 dst 可能有多个 src(Java 的代码树与 proto 产物树就部署到同一个包目录),
    # 陈旧判定必须按 dst 归并全部 src 后再做,否则会把另一对的产物全报成陈旧。
    srcs_by_dst: dict[Path, list[Path]] = {}
    for src, dst in tasks:
        try:
            md5_copy(src, dst)
            ok += 1
            srcs_by_dst.setdefault(Path(dst), []).append(Path(src))
        except Exception as exc:
            logger.error("Deploy failed %s -> %s: %s", src, dst, exc)
            fail += 1
    logger.info("Deploy: %d OK, %d failed", ok, fail)

    orphans: list[Path] = []
    for dst, srcs in srcs_by_dst.items():
        orphans.extend(report_orphans(srcs, dst))

    # 陈旧产物:md5_copy 只覆盖不删除,布局一变旧文件就永久残留(详见 file_utils.report_orphans)。
    # 只告警不自动删——目标树里合法混着手写文件。确认无用后手工删,手写文件登记进 _DEPLOY_KEEP。
    if orphans:
        logger.warning(
            "发现 %d 个疑似陈旧产物(源树已无、目标树仍在)。确认无用请手工删除;"
            "若是手写文件请登记到 core/file_utils.py 的 _DEPLOY_KEEP:", len(orphans)
        )
        for path in orphans:
            logger.warning("  陈旧产物? %s", path)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="data_table_exporter",
        description="Excel 配置表导出器",
    )
    parser.add_argument(
        "config", nargs="?", default=None,
        help="配置文件路径,省略则用 exporter_config.yaml",
    )
    parser.add_argument(
        "--bitindex-bootstrap", action="store_true",
        help="允许位序状态文件缺失时从空初始化。"
             "**只有确认该表从未发布过**才可以用:位序是存量玩家位图的下标口径,"
             "重新分配会让所有存量玩家的位图整体错位。",
    )
    return parser.parse_args(argv)


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )
    args: argparse.Namespace = _parse_args()
    cfg: ExporterConfig = load_config(args.config)
    cfg.bit_index_bootstrap = args.bitindex_bootstrap
    try:
        run(cfg)
    except BitIndexStateError as exc:
        # 位序状态不可用是"必须人工处理"的错,给一句能照着做的话,不要甩一整屏 traceback。
        logger.error("位序状态校验失败,导表中止:%s", exc)
        sys.exit(1)


if __name__ == "__main__":
    main()

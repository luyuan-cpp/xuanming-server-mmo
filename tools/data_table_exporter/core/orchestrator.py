"""Main orchestrator — coordinates the full export pipeline."""

from __future__ import annotations

import logging
from pathlib import Path
import sys

from core.config_loader import ExporterConfig, LangConfig, load_config
from core.excel_reader import read_all_tables
from core.file_utils import ensure_dirs, md5_copy, report_orphans
from core.foreign_key import validate_foreign_keys
from core.generators.bit_index_gen import generate_bit_indexes
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

    warnings: list[str] = validate_foreign_keys(tables)
    if warnings:
        logger.warning("FK validation: %d warning(s)", len(warnings))

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

def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )
    config_path: str | None = sys.argv[1] if len(sys.argv) > 1 else None
    cfg: ExporterConfig = load_config(config_path)
    run(cfg)


if __name__ == "__main__":
    main()

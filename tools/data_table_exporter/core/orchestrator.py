"""Main orchestrator — coordinates the full export pipeline."""

from __future__ import annotations

import logging
from pathlib import Path
import sys

import argparse

from core.config_loader import ExporterConfig, LangConfig, load_config
from core.table_source import TableSourceError, read_all_tables
from core.file_utils import ensure_dirs, md5_copy, report_orphans
from core.foreign_key import ForeignKeyReport, validate_foreign_keys
from core.manifest import generate_manifest
from core.primary_key import PrimaryKeyReport, validate_primary_keys
from core.generators.bit_index_gen import BitIndexStateError, generate_bit_indexes
from core.generators.comp_gen import generate_comp_headers
from core.generators.config_gen import generate_config_classes
from core.generators.constants_gen import generate_constants
from core.generators.enum_gen import (
    generate_operator_enums,
    generate_tip_enums,
    validate_tip_references,
)
from core.generators.json_gen import generate_json
from core.generators.binary_gen import generate_binary
from core.generators.proto_gen import (
    ProtoCompileError,
    compile_proto_cpp,
    compile_proto_csharp,
    compile_proto_go,
    compile_proto_java,
    compile_proto_python,
    generate_proto_files,
)
from core.generators.table_id_gen import generate_table_ids
from core.schema import TableSchema

logger: logging.Logger = logging.getLogger(__name__)


class DeployError(RuntimeError):
    """至少一个部署目标未更新；调用方必须以失败退出。"""


class GeneratedOutputError(RuntimeError):
    """生成源目录与当前表集不一致；不得部署混合世代产物。"""


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

    # 普通表里也可能直接存 Tip ID（状态许可矩阵就是实消费点）。旧轴重排后若
    # 这些数字没迁，编译仍会绿但运行时会返回 unknown。必须和 FK 一样在任何
    # 生成前 fail-closed。
    validate_tip_references(tables, cfg)

    # 主键重复从前是静默丢索引(FindById 只拿到其中一行、Count 少算、零报错)。
    # 现在:主键列写了 (cfg_multi) 才允许重复,否则整批不产出。
    pk: PrimaryKeyReport = validate_primary_keys(tables, cfg)
    if not pk.ok:
        for msg in pk.errors:
            logger.error("主键: %s", msg)
        logger.error("主键校验失败:%d 条错误,导表中止,产出未落盘", len(pk.errors))
        sys.exit(1)

    # Generate
    generate_json(cfg, tables)
    generate_proto_files(cfg, tables)
    generate_operator_enums(cfg)
    generate_tip_enums(cfg)
    compile_proto_cpp(cfg)
    compile_proto_go(cfg)
    compile_proto_java(cfg)
    compile_proto_csharp(cfg)
    compile_proto_python(cfg)
    generate_binary(cfg, tables)
    generate_config_classes(cfg, tables)
    generate_comp_headers(cfg, tables)
    _validate_java_code_outputs(cfg, tables)
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
    for lang in (cfg.cpp, cfg.go, cfg.java, cfg.csharp, cfg.python, cfg.ue):
        if lang.enabled:
            dirs.extend([
                lang.code_dir, lang.proto_output_dir,
                lang.constants_dir, lang.table_id_dir, lang.bit_index_dir,
            ])
    ensure_dirs(*[d for d in dirs if d and str(d)])


def _validate_java_code_outputs(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """校验 Java 表管理器/组件源目录没有已下线表的陈旧类。

    部署闸门只能比较「源目录并集」与目标目录。如果生成源本身就残留
    旧文件，它会被误当成当前产物再次部署。这里用当前 TableSchema
    推导出直接由 config/comp generator 拥有的完整文件集。
    """
    if not cfg.java.enabled:
        return

    expected = {"AllTable.java"}
    for table in tables:
        expected.add(f"{table.name}TableManager.java")
        if table.has_foreign_keys:
            expected.add(f"{table.name}TableForeignKeys.java")
        if table.scalar_comp_columns or table.repeated_comp_arrays:
            expected.add(f"{table.name}TableComp.java")

    existing = {
        path.name
        for path in cfg.java.code_dir.glob("*.java")
        if path.is_file()
    }
    unexpected = sorted(existing - expected)
    missing = sorted(expected - existing)
    if unexpected or missing:
        details: list[str] = []
        details.extend(f"陈旧: {name}" for name in unexpected)
        details.extend(f"缺失: {name}" for name in missing)
        raise GeneratedOutputError(
            "Java 表代码生成源目录与当前表集不一致；"
            "请审计并清理后重跑:\n  " + "\n  ".join(details)
        )


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

    if cfg.csharp.enabled:
        tasks.extend((d["src"], d["dst"]) for d in cfg.csharp.deploy)

    if cfg.python.enabled:
        tasks.extend((d["src"], d["dst"]) for d in cfg.python.deploy)

    if cfg.ue.enabled:
        tasks.extend((d["src"], d["dst"]) for d in cfg.ue.deploy)

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

    if fail:
        raise DeployError(f"{fail} 个部署任务失败（{ok} 个成功），拒绝报告导表完成")

    orphans: list[Path] = []
    for dst, srcs in srcs_by_dst.items():
        orphans.extend(report_orphans(srcs, dst))

    # 陈旧产物:md5_copy 只覆盖不删除,布局一变旧文件就永久残留(详见 file_utils.report_orphans)。
    # 目标树可能合法混着手写或并行工作，所以不自动删；但也不能只告警后报 DONE，
    # 否则消费工程会继续编译旧类。审计并删除报告的生成文件后重跑。
    if orphans:
        logger.error(
            "发现 %d 个疑似陈旧产物(源树已无、目标树仍在)，拒绝报告导表完成。"
            "确认无用请删除；若是手写文件请登记到 core/file_utils.py 的 _DEPLOY_KEEP:",
            len(orphans),
        )
        for path in orphans:
            logger.error("  陈旧产物? %s", path)
        raise DeployError(f"发现 {len(orphans)} 个陈旧部署产物，拒绝混合世代输出")


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
    parser.add_argument(
        "--tip-axis-bootstrap", action="store_true",
        help="允许 tip 码轴 state 缺失时从空初始化。"
             "只有确认该码轴从未发布过才可以用；误用会重新分配客户端可见 ID。",
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
    cfg.tip_axis_bootstrap = args.tip_axis_bootstrap
    try:
        run(cfg)
    except TableSourceError as exc:
        logger.error("配表来源层校验失败,导表中止:%s", exc)
        sys.exit(1)
    except BitIndexStateError as exc:
        # 位序状态不可用是"必须人工处理"的错,给一句能照着做的话,不要甩一整屏 traceback。
        logger.error("位序状态校验失败,导表中止:%s", exc)
        sys.exit(1)
    except ProtoCompileError as exc:
        logger.error("Proto 编译失败,导表中止:%s", exc)
        sys.exit(1)
    except DeployError as exc:
        logger.error("部署失败,导表中止:%s", exc)
        sys.exit(1)
    except GeneratedOutputError as exc:
        logger.error("生成产物校验失败,导表中止:%s", exc)
        sys.exit(1)


if __name__ == "__main__":
    main()

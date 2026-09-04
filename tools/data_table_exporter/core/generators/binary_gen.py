"""Proto binary data generator.

Generates ``{table_name}.pb`` files by serialising each table's JSON data
into its corresponding protobuf binary wire format.  The compiled Python
protobuf modules (``*_table_pb2``) are imported dynamically from the
``proto_output_dir`` that ``compile_proto_*`` already populated.
"""

from __future__ import annotations

import importlib
import logging
import sys
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from google.protobuf import json_format

from core.config_loader import ExporterConfig
from core.file_utils import ensure_dirs, write_file_bytes
from core.schema import TableSchema

logger = logging.getLogger(__name__)


def generate_binary(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate proto-binary ``.pb`` files for every table.

    Requires that ``generate_json`` and ``compile_proto_*`` (Python) have
    already run so that both ``*.json`` and ``*_table_pb2.py`` exist.
    """
    ensure_dirs(cfg.binary_dir)
    if not tables:
        logger.warning("No tables to generate binary for")
        return

    # Make the compiled *_pb2.py importable.
    pb2_dir = str(cfg.proto_python_output_dir)
    if pb2_dir not in sys.path:
        sys.path.insert(0, pb2_dir)

    with ThreadPoolExecutor() as pool:
        for _ in pool.map(lambda t: _export_one(t, cfg), tables):
            pass


def _export_one(table: TableSchema, cfg: ExporterConfig) -> None:
    json_path = cfg.json_dir / f"{table.name}.json"
    if not json_path.exists():
        raise RuntimeError(
            f"Binary generation failed for {table.name}: JSON not found: {json_path}"
        )

    module_name = f"{table.name.lower()}_table_pb2"
    try:
        mod = importlib.import_module(module_name)
    except ModuleNotFoundError as exc:
        raise RuntimeError(
            f"Binary generation failed for {table.name}: proto module "
            f"{module_name} not found; run compile_proto first"
        ) from exc

    # The wrapper message is always ``{SheetName}TableData``
    msg_cls_name = f"{table.name}TableData"
    msg_cls = getattr(mod, msg_cls_name, None)
    if msg_cls is None:
        raise RuntimeError(
            f"Binary generation failed for {table.name}: message class "
            f"{msg_cls_name} not found in {module_name}"
        )

    try:
        with open(json_path, "r", encoding="utf-8") as f:
            json_text = f.read()

        msg = msg_cls()
        json_format.Parse(json_text, msg)

        out = cfg.binary_dir / f"{table.name.lower()}.pb"
        # 产物会在 CI 中逐字节做漂移检查；protobuf map 的默认序列化顺序没有
        # 契约保证，必须显式 deterministic 才能跨 Windows/Linux 可复现。
        write_file_bytes(out, msg.SerializeToString(deterministic=True))
        logger.info("Generated %s (%d bytes)", out.name, out.stat().st_size)
    except Exception as exc:
        raise RuntimeError(
            f"Binary generation failed for {table.name}: {exc}"
        ) from exc

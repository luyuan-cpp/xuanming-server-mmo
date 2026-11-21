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


def _load_table_message_class(table: TableSchema):
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
    return msg_cls


def _export_one(table: TableSchema, cfg: ExporterConfig) -> None:
    json_path = cfg.json_dir / f"{table.name.lower()}.json"
    if not json_path.exists():
        json_path = cfg.json_dir / f"{table.name}.json"
    if not json_path.exists():
        raise RuntimeError(
            f"Binary generation failed for {table.name}: JSON not found: {json_path}"
        )

    msg_cls = _load_table_message_class(table)

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


def generate_client_binary(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate client-filtered proto-binary ``.pb`` files under ``cfg.client_binary_dir``.

    Strips every field whose ``owner`` is not in ``cfg.client_owner_types`` (for example
    ``owner == "server"``) before serialising so server-private columns never appear on
    the wire in client bundles, while keeping the exact same proto3 schema for ``TableLoader``.
    """
    if not cfg.client_binary_dir or str(cfg.client_binary_dir) in ("", "."):
        return
    ensure_dirs(cfg.client_binary_dir)
    if not tables:
        return

    pb2_dir = str(cfg.proto_python_output_dir)
    if pb2_dir not in sys.path:
        sys.path.insert(0, pb2_dir)

    client_owners = {o.lower() for o in cfg.client_owner_types}
    with ThreadPoolExecutor() as pool:
        for _ in pool.map(lambda t: _export_one_client(t, cfg, client_owners), tables):
            pass


def _strip_non_client_fields(msg, table: TableSchema, client_owners: set[str]) -> tuple[bool, set[str], dict[str, set[str]]]:
    """Clear all non-client fields from *msg* in place.

    Returns ``(cleared_all_rows, top_level_stripped, partial_group_stripped)``.
    """
    server_cols = table.server_columns
    if not any(c.owner.lower() in client_owners for c in server_cols):
        msg.ClearField("data")
        return True, set(), {}

    col_to_group = table.col_to_group
    map_names = set(table.maps.keys())
    strip_fields: set[str] = set()
    group_allowed: dict[str, set[str]] = {}
    group_blocked: dict[str, set[str]] = {}

    for col in server_cols:
        allowed = col.owner.lower() in client_owners
        if col.map_role in ("map_key", "map_value"):
            if not allowed:
                suffix = "_key" if col.map_role == "map_key" else "_value"
                prefix = col.name[:-len(suffix)] if col.name.endswith(suffix) else col.name
                if prefix in map_names:
                    strip_fields.add(prefix)
        elif col.excel_index in col_to_group:
            gname = col_to_group[col.excel_index]
            if allowed:
                group_allowed.setdefault(gname, set()).add(col.name)
            else:
                group_blocked.setdefault(gname, set()).add(col.name)
        elif not allowed:
            strip_fields.add(col.name)

    partial_groups: dict[str, set[str]] = {}
    for gname, blocked_cols in group_blocked.items():
        if not group_allowed.get(gname):
            strip_fields.add(gname)
        else:
            partial_groups[gname] = blocked_cols

    if strip_fields or partial_groups:
        for row in msg.data:
            for fname in strip_fields:
                row.ClearField(fname)
            for gname, sub_names in partial_groups.items():
                for sub in getattr(row, gname):
                    for sname in sub_names:
                        sub.ClearField(sname)

    return False, strip_fields, partial_groups


def _export_one_client(table: TableSchema, cfg: ExporterConfig, client_owners: set[str]) -> None:
    src_pb = cfg.binary_dir / f"{table.name.lower()}.pb"
    if not src_pb.exists():
        raise RuntimeError(
            f"Client binary generation failed for {table.name}: server .pb not found: {src_pb}"
        )

    msg_cls = _load_table_message_class(table)
    try:
        msg = msg_cls()
        msg.ParseFromString(src_pb.read_bytes())
        cleared_all, stripped, partial_groups = _strip_non_client_fields(msg, table, client_owners)
        out = cfg.client_binary_dir / f"{table.name.lower()}.pb"
        write_file_bytes(out, msg.SerializeToString(deterministic=True))
        if cleared_all:
            logger.info("Generated client %s (0 rows; server-only table)", out.name)
        elif stripped or partial_groups:
            logger.info(
                "Generated client %s (%d bytes; stripped=%s)",
                out.name, out.stat().st_size, sorted(stripped) + sorted(partial_groups.keys()),
            )
    except Exception as exc:
        raise RuntimeError(
            f"Client binary generation failed for {table.name}: {exc}"
        ) from exc

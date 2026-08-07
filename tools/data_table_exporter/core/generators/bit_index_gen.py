"""Bit-index generator.

For tables whose columns carry a ``bit_index`` marker, generates a stable
ID-to-bit-position mapping (persisted in ``state/``) and the corresponding
C++ / Go source files.
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

from jinja2 import Environment, FileSystemLoader

from core.config_loader import ExporterConfig
from core.excel_reader import open_worksheet, read_id_column
from core.file_utils import ensure_dirs, write_file
from core.schema import TableSchema

logger = logging.getLogger(__name__)


def generate_bit_indexes(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate bit-index mapping files for all applicable tables."""
    # keep_trailing_newline:Jinja 默认吃掉模板末尾换行,产出的 .go / .h 会缺行尾换行
    # (gofmt 与多数工具都视为脏)。位序模板的排版靠 `{%-` 空白控制,不再走 _clean_output。
    env = Environment(
        loader=FileSystemLoader(str(cfg.template_dir), encoding="utf-8"),
        keep_trailing_newline=True,
    )
    mapping_dir = cfg.state_dir / "mapping" / "table_index_mapping"
    ensure_dirs(mapping_dir)

    for schema in tables:
        if not schema.bit_index_columns:
            continue

        mapping_file = mapping_dir / f"{schema.name.lower()}_mapping.json"
        id_to_index = _load_mapping(mapping_file)
        ids = read_id_column(schema, cfg)
        id_to_index = _update_mapping(ids, id_to_index)
        _save_mapping(mapping_file, id_to_index)

        max_bit = _find_max_bit(schema, cfg)

        if cfg.cpp.enabled:
            _gen_cpp(schema.name, id_to_index, max_bit, env, cfg)
        if cfg.go.enabled:
            _gen_go(schema.name, id_to_index, max_bit, env, cfg)


# ---------------------------------------------------------------------------
# Mapping persistence
# ---------------------------------------------------------------------------

def _load_mapping(path: Path) -> dict[int, int]:
    if path.exists():
        try:
            with open(path, "r", encoding="utf-8") as f:
                return {int(k): v for k, v in json.load(f).items()}
        except Exception:
            pass
    return {}


def _save_mapping(path: Path, mapping: dict[int, int]) -> None:
    with open(path, "w", encoding="utf-8") as f:
        json.dump(mapping, f, indent=4)


def _update_mapping(ids: list[int], existing: dict[int, int]) -> dict[int, int]:
    """Assign stable bit positions to new IDs, reusing gaps from removed IDs."""
    used = set(existing.values())
    next_idx = max(existing.values(), default=-1) + 1
    unused = sorted(set(range(next_idx)) - used)

    for row_id in ids:
        if row_id not in existing:
            if unused:
                existing[row_id] = unused.pop(0)
            else:
                existing[row_id] = next_idx
                next_idx += 1
    return existing


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _find_max_bit(schema: TableSchema, cfg: ExporterConfig) -> int:
    """Find the maximum bit_index value from data rows."""
    bit_col = schema.bit_index_columns[0].excel_index
    ws = open_worksheet(schema)
    max_val = 0
    for row in ws.iter_rows(min_row=cfg.data_begin_row, values_only=True):
        v = row[bit_col] if bit_col < len(row) else None
        if isinstance(v, (int, float)):
            max_val = max(max_val, int(v))
    return max_val


# ---------------------------------------------------------------------------
# Code generation
# ---------------------------------------------------------------------------

def _gen_cpp(name, mapping, max_bit, env, cfg):
    ensure_dirs(cfg.cpp.bit_index_dir)
    tpl = env.get_template("cpp_bit_index.h.j2")
    content = tpl.render(sheet=name, id_to_index=mapping, max_bit_index=max_bit)
    write_file(cfg.cpp.bit_index_dir / f"{name.lower()}_table_id_bit_index.h", content)
    logger.info("Generated C++ bit_index: %s", name)


def _gen_go(name, mapping, max_bit, env, cfg):
    """Go 位序常量:每张表落到与包同名的子目录 bit_index/<pkg>/。

    不能平铺:模板声明 `package <sheet>`,Go 要求「一个目录一个包」。两张以上 bit_index 表
    平铺在同一个 bit_index/ 下,整个模块会直接编译失败——
    `found packages mission (...) and reward (...) in .../generated/bit_index`
    (实测 go/shared 模块曾因此 `go build ./...` exit=1,而各服务模块因为没 import 它
    反而是绿的,所以这个坑只在全模块构建 / CI 才现形)。
    """
    pkg = name.lower()
    out_dir = cfg.go.bit_index_dir / pkg
    ensure_dirs(out_dir)
    tpl = env.get_template("go_bit_index.go.j2")
    # name_width:常量名最长长度,模板据此 ljust 补齐,使产物与 gofmt 的 `=` 对齐一致。
    name_width = max((len(f"ID_{row_id}") for row_id in mapping), default=0)
    content = tpl.render(
        sheet=name, id_to_index=mapping, max_bit_index=max_bit, name_width=name_width
    )
    write_file(out_dir / f"{pkg}_table_id_bit_index.go", content)
    logger.info("Generated Go bit_index: %s -> %s", name, out_dir)

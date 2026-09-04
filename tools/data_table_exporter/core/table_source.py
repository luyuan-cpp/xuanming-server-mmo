"""schema 来源派发:一张表有权威 schema 就走 schema-first,没有就走旧的 5 行表头。

这一层存在的唯一理由是**迁移可以一张表一张表地做**,任意时刻整条流水线都能跑完:

    data/schema/<sheet>_table.proto 存在  -> core.schema_proto.build_table_schema
                              不存在      -> core.excel_reader.read_table

两条路产出同一个 ``TableSchema``,所以 9 个生成器与 24 个模板一行都不用改。

放在独立模块而不是塞进 ``excel_reader``,是因为 ``schema_proto`` 要 import
``excel_reader._detect_layout``;反向再 import 就成环了。

派发按**sheet 名**匹配,不按文件名:``Renamed.xlsx`` 里放着 sheet ``Good`` 是合法的,
「文件名等于表名」在本仓只是巧合,不能当约定用。
"""

from __future__ import annotations

import logging
from pathlib import Path

import openpyxl

from core.config_loader import ExporterConfig
from core.excel_reader import read_table
from core.file_utils import list_xlsx
from core.schema import TableSchema
from core.schema_proto import (
    SchemaProtoError,
    build_table_schema,
    load_vocabulary,
    parse_schema_proto,
)

logger = logging.getLogger(__name__)


class TableSourceError(RuntimeError):
    """来源层出错。一律中止,不让坏 schema 覆盖上一批好产物。"""


def index_schema_protos(schema_dir: Path) -> dict[str, Path]:
    """扫描权威 schema 目录,返回 ``{sheet 名: proto 路径}``。

    重复声明同一个 sheet 一律报错 —— 那种情况下「用哪一份」没有正确答案。
    """
    if not schema_dir.exists():
        return {}
    vocab_path = schema_dir / "cfg_options.proto"
    if not vocab_path.exists():
        raise TableSourceError("%s 下有 schema 却没有 cfg_options.proto,词表缺失" % schema_dir)
    vocab = load_vocabulary(vocab_path)

    index: dict[str, Path] = {}
    for proto in sorted(schema_dir.glob("*.proto")):
        if proto.name == "cfg_options.proto":
            continue
        try:
            messages = parse_schema_proto(proto, vocab)
        except SchemaProtoError as exc:
            raise TableSourceError("解析 %s 失败:%s" % (proto.name, exc)) from None
        sheets = [m.opts["cfg_sheet"] for m in messages.values() if "cfg_sheet" in m.opts]
        if not sheets:
            raise TableSourceError("%s 里没有带 (cfg_sheet) 的 message" % proto.name)
        if len(sheets) > 1:
            raise TableSourceError("%s 里有 %d 个 (cfg_sheet)" % (proto.name, len(sheets)))
        sheet = sheets[0]
        if sheet in index:
            raise TableSourceError("sheet %r 被 %s 与 %s 同时声明"
                                   % (sheet, index[sheet].name, proto.name))
        index[sheet] = proto
    return index


def _first_sheet_name(xlsx: Path) -> str:
    wb = openpyxl.load_workbook(xlsx, read_only=True)
    try:
        return wb.sheetnames[0] if wb.sheetnames else ""
    finally:
        wb.close()


def read_all_tables(cfg: ExporterConfig) -> list[TableSchema]:
    """读全部数据表,逐张按来源派发。

    任何一张表读失败都直接中止:从前 ``read_table`` 返回 None 会被静默跳过,
    而 ``md5_copy`` 只覆盖不删除,于是那张表的产物停留在上一次成功导出的版本,
    看上去一切正常(实测:把 A1 从 ``id`` 改成 ``ID`` 就能复现)。
    """
    index = index_schema_protos(cfg.schema_dir)
    tables: list[TableSchema] = []
    via_schema = 0

    for xlsx in list_xlsx(cfg.data_dir):
        sheet = _first_sheet_name(xlsx)
        proto = index.get(sheet)
        if proto is not None:
            try:
                tables.append(build_table_schema(proto, xlsx))
            except SchemaProtoError as exc:
                raise TableSourceError("[%s] 权威 schema 与源表不符:%s" % (sheet, exc)) from None
            via_schema += 1
            logger.debug("%s <- %s(权威 schema)", sheet, proto.name)
            continue
        schema = read_table(xlsx, cfg)
        if schema is None:
            raise TableSourceError(
                "%s 读不出 schema。它既没有权威 schema(%s),旧的 5 行表头也解析失败。"
                % (xlsx.name, cfg.schema_dir / ("%s_table.proto" % sheet.lower())))
        tables.append(schema)
        logger.debug("%s <- %s(旧表头)", schema.name, xlsx.name)

    unused = sorted(set(index) - {t.name for t in tables})
    if unused:
        raise TableSourceError("这些权威 schema 没有对应的源表:%s"
                               % ", ".join("%s(%s)" % (s, index[s].name) for s in unused))

    logger.info("读到 %d 张表:%d 张走权威 schema,%d 张走旧表头",
                len(tables), via_schema, len(tables) - via_schema)
    return tables


def read_one_table(xlsx: Path, cfg: ExporterConfig) -> TableSchema:
    """按同一套派发规则读**一张**表。给冒烟测试与排障用。

    与 :func:`read_all_tables` 共用派发逻辑,避免测试和生产各走各的路 ——
    那正是「测试全绿但导表是坏的」的常见来源。
    """
    index = index_schema_protos(cfg.schema_dir)
    sheet = _first_sheet_name(xlsx)
    proto = index.get(sheet)
    if proto is not None:
        return build_table_schema(proto, xlsx)
    schema = read_table(xlsx, cfg)
    if schema is None:
        raise TableSourceError("%s 既没有权威 schema,旧表头也解析失败" % xlsx.name)
    return schema

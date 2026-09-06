"""Config class generator.

Produces per-table config manager classes for enabled languages (C++, Go,
Java) using Jinja2 templates, plus aggregated "all_table" files.
"""

from __future__ import annotations

import logging
import re

from jinja2 import Environment, FileSystemLoader, Template, Template, Template, Template, Template, Template, Template, Template

from core.config_loader import ExporterConfig
from core.file_utils import ensure_dirs, write_file
from core.schema import TableSchema
from core import type_mapping

logger: logging.Logger = logging.getLogger(__name__)


def generate_config_classes(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate config manager classes for all tables and all enabled languages."""
    env = Environment(loader=FileSystemLoader(str(cfg.template_dir), encoding="utf-8"))

    if cfg.cpp.enabled:
        ensure_dirs(cfg.cpp.code_dir)
        _gen_all_cpp(tables, env, cfg)

    if cfg.go.enabled:
        ensure_dirs(cfg.go.code_dir)
        _gen_all_go(tables, env, cfg)

    if cfg.java.enabled:
        ensure_dirs(cfg.java.code_dir)
        _gen_all_java(tables, env, cfg)

    if cfg.csharp.enabled:
        ensure_dirs(cfg.csharp.code_dir)
        _gen_all_csharp(tables, env, cfg)

    if cfg.python.enabled:
        ensure_dirs(cfg.python.code_dir)
        _gen_all_python(tables, env, cfg)

    if cfg.ue.enabled:
        ensure_dirs(cfg.ue.code_dir)
        _gen_all_ue(tables, env, cfg)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_RE_EXCESS_BLANKS: re.Pattern[str] = re.compile(r'\n{3,}')


def _clean_output(text: str) -> str:
    """Collapse runs of 3+ newlines into 2 (at most one blank line) and strip leading/trailing whitespace."""
    text = text.lstrip('\n')
    text = _RE_EXCESS_BLANKS.sub('\n\n', text)
    return text.rstrip() + '\n'


# ---------------------------------------------------------------------------
# C++
# ---------------------------------------------------------------------------

def _gen_all_cpp(tables: list[TableSchema], env: Environment, cfg: ExporterConfig) -> None:
    h_tpl: Template = env.get_template("cpp_config.h.j2")
    cpp_tpl: Template = env.get_template("cpp_config.cpp.j2")
    fk_tpl: Template = env.get_template("cpp_config_fk.h.j2")
    fk_cpp_tpl: Template = env.get_template("cpp_config_fk.cpp.j2")

    for t in tables:
        ctx = _cpp_ctx(t)
        write_file(cfg.cpp.code_dir / f"{t.name.lower()}_table.h", _clean_output(h_tpl.render(**ctx)))
        write_file(cfg.cpp.code_dir / f"{t.name.lower()}_table.cpp", _clean_output(cpp_tpl.render(**ctx)))
        if t.has_foreign_keys:
            fk_ctx = _cpp_fk_ctx(t)
            write_file(cfg.cpp.code_dir / f"{t.name.lower()}_table_fk.h", _clean_output(fk_tpl.render(**fk_ctx)))
            write_file(cfg.cpp.code_dir / f"{t.name.lower()}_table_fk.cpp", _clean_output(fk_cpp_tpl.render(**fk_ctx)))
            logger.info("Generated C++ FK: %s", t.name)
        logger.info("Generated C++ config: %s", t.name)

    # all_table aggregator
    names: list[str] = sorted(t.name for t in tables)
    ah_tpl: Template = env.get_template("cpp_all_table.h.j2")
    ac_tpl: Template = env.get_template("cpp_all_table.cpp.j2")
    write_file(cfg.cpp.code_dir / "all_table.h", ah_tpl.render())
    write_file(cfg.cpp.code_dir / "all_table.cpp", ac_tpl.render(sheetnames=names))


def _cpp_ctx(table: TableSchema) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        to_cpp_type=type_mapping.to_cpp_type,
        to_cpp_param_type=type_mapping.to_cpp_param_type,
        cpp_map_type=type_mapping.cpp_map_type,
        composite_cpp_struct_name=type_mapping.composite_cpp_struct_name,
        to_pascal_case=type_mapping.to_pascal_case,
        to_camel_case=type_mapping.to_camel_case,
    )


def _cpp_fk_ctx(table: TableSchema) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        fk_targets=table.fk_target_tables,
        to_pascal_case=type_mapping.to_pascal_case,
        to_cpp_type=type_mapping.to_cpp_type,
        to_cpp_param_type=type_mapping.to_cpp_param_type,
    )


# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

def _gen_all_go(tables: list[TableSchema], env: Environment, cfg: ExporterConfig) -> None:
    go_tpl: Template = env.get_template("go_config.go.j2")
    fk_tpl: Template = env.get_template("go_config_fk.go.j2")

    for t in tables:
        ctx = _go_ctx(t, cfg)
        write_file(cfg.go.code_dir / f"{t.name.lower()}_table.go", go_tpl.render(**ctx))
        if t.has_foreign_keys:
            fk_ctx = _go_fk_ctx(t, cfg)
            write_file(cfg.go.code_dir / f"{t.name.lower()}_table_fk.go", _clean_output(fk_tpl.render(**fk_ctx)))
            logger.info("Generated Go FK: %s", t.name)
        logger.info("Generated Go config: %s", t.name)

    names: list[str] = sorted(t.name for t in tables)
    all_tpl: Template = env.get_template("go_all_table.go.j2")
    write_file(cfg.go.code_dir / "all_table.go", all_tpl.render(sheetnames=names))


def _go_ctx(table: TableSchema, cfg: ExporterConfig) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        to_go_type=type_mapping.to_go_type,
        to_go_proto_field=type_mapping.to_go_proto_field,
        proto_import_path=cfg.go.proto_import_path,
        composite_go_struct_name=type_mapping.composite_go_struct_name,
    )


def _go_fk_ctx(table: TableSchema, cfg: ExporterConfig) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        to_go_proto_field=type_mapping.to_go_proto_field,
        to_go_type=type_mapping.to_go_type,
        proto_import_path=cfg.go.proto_import_path,
    )


# ---------------------------------------------------------------------------
# Java
# ---------------------------------------------------------------------------

def _gen_all_java(tables: list[TableSchema], env: Environment, cfg: ExporterConfig) -> None:
    java_tpl: Template = env.get_template("java_config.java.j2")
    fk_tpl: Template = env.get_template("java_config_fk.java.j2")

    for t in tables:
        ctx = _java_ctx(t, cfg)
        write_file(cfg.java.code_dir / f"{t.name}TableManager.java", java_tpl.render(**ctx))
        if t.has_foreign_keys:
            fk_ctx = _java_fk_ctx(t, cfg)
            write_file(cfg.java.code_dir / f"{t.name}TableForeignKeys.java", _clean_output(fk_tpl.render(**fk_ctx)))
            logger.info("Generated Java FK: %s", t.name)
        logger.info("Generated Java config: %s", t.name)

    names: list[str] = sorted(t.name for t in tables)
    all_tpl: Template = env.get_template("java_all_table.java.j2")
    write_file(cfg.java.code_dir / "AllTable.java", all_tpl.render(
        sheetnames=names, package=cfg.java.package
    ))


def _java_ctx(table: TableSchema, cfg: ExporterConfig) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        to_java_type=type_mapping.to_java_type,
        to_java_boxed=type_mapping.to_java_boxed,
        to_java_proto_getter=type_mapping.to_java_proto_getter,
        to_java_repeated_elem_type=type_mapping.to_java_repeated_elem_type,
        package=cfg.java.package,
        composite_go_struct_name=type_mapping.composite_go_struct_name,
        to_pascal_case=type_mapping.to_pascal_case,
        to_camel_case=type_mapping.to_camel_case,
    )


def _java_fk_ctx(table: TableSchema, cfg: ExporterConfig) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        to_pascal_case=type_mapping.to_pascal_case,
        to_java_proto_getter=type_mapping.to_java_proto_getter,
        to_java_type=type_mapping.to_java_type,
        package=cfg.java.package,
    )



# ---------------------------------------------------------------------------
# C#  (Unity 客户端)
# ---------------------------------------------------------------------------

def _gen_all_csharp(tables: list[TableSchema], env: Environment, cfg: ExporterConfig) -> None:
    if not cfg.csharp.package:
        # 空 namespace 会生成语法错误的 .cs（`namespace {`），与其让消费工程炸在
        # Unity 编译期，不如在导表期就断。
        raise ValueError(
            "languages.csharp.package 未配置：C# 产物的 namespace 没有可用默认值"
        )

    cs_tpl: Template = env.get_template("csharp_config.cs.j2")
    fk_tpl: Template = env.get_template("csharp_config_fk.cs.j2")

    # 哪些表的主键可重复。FK 助手要用：主键可重复的表**没有 FindById**，
    # 外键模板若照 Java 那样无脑调目标表的 FindById，C# 侧会直接编译不过
    # （Java 模板只挡了「源表」可重复，没挡「目标表」可重复）。
    multi_pk_tables: set[str] = {t.name for t in tables if t.multi_primary_key}

    for t in tables:
        ctx = _csharp_ctx(t, cfg)
        write_file(
            cfg.csharp.code_dir / f"{t.name}TableManager.cs",
            _clean_output(cs_tpl.render(**ctx)),
        )
        if t.has_foreign_keys:
            fk_ctx = _csharp_fk_ctx(t, cfg, multi_pk_tables)
            write_file(
                cfg.csharp.code_dir / f"{t.name}TableForeignKeys.cs",
                _clean_output(fk_tpl.render(**fk_ctx)),
            )
            logger.info("Generated C# FK: %s", t.name)
        logger.info("Generated C# config: %s", t.name)

    names: list[str] = sorted(t.name for t in tables)
    all_tpl: Template = env.get_template("csharp_all_table.cs.j2")
    write_file(
        cfg.csharp.code_dir / "AllTable.cs",
        _clean_output(all_tpl.render(sheetnames=names, package=cfg.csharp.package)),
    )


def _csharp_ctx(table: TableSchema, cfg: ExporterConfig) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        to_csharp_type=type_mapping.to_csharp_type,
        to_csharp_repeated_elem_type=type_mapping.to_csharp_repeated_elem_type,
        to_csharp_proto_prop=type_mapping.to_csharp_proto_prop,
        csharp_composite_struct_name=type_mapping.csharp_composite_struct_name,
        to_pascal_case=type_mapping.to_pascal_case,
        to_camel_case=type_mapping.to_camel_case,
        package=cfg.csharp.package,
        # protoc 把消息类放进哪个 namespace。生成的 .proto 既没有 package 也没有
        # option csharp_namespace -> 全局 namespace -> 这里留空，模板就不写 using。
        # 与独立客户端 mmorpg-client/Assets/Scripts/Net/Generated 现有网络协议产物口径一致。
        proto_namespace=cfg.csharp.proto_import_path,
    )


def _csharp_fk_ctx(table: TableSchema, cfg: ExporterConfig, multi_pk_tables: set[str]) -> dict:
    ctx: dict = _csharp_ctx(table, cfg)
    ctx["multi_pk_tables"] = multi_pk_tables
    return ctx




# ---------------------------------------------------------------------------
# Python
# ---------------------------------------------------------------------------

# PEP 8 要求顶层定义之间空**两**行（= 3 个 \n），所以 Python 产物不能复用
# ``_clean_output``：那个把 3 个以上的 \n 一律压成 2 个，等于把每个函数之间
# 的空行删掉一个。这里只压 4 个以上。
_RE_PY_EXCESS_BLANKS: re.Pattern[str] = re.compile(r'\n{4,}')


def _clean_python_output(text: str) -> str:
    """Collapse runs of 4+ newlines into 3 (PEP 8 wants two blank lines between top-level defs)."""
    text = text.lstrip('\n')
    text = _RE_PY_EXCESS_BLANKS.sub('\n\n\n', text)
    return text.rstrip() + '\n'


def _gen_all_python(tables: list[TableSchema], env: Environment, cfg: ExporterConfig) -> None:
    py_tpl: Template = env.get_template("python_config.py.j2")
    fk_tpl: Template = env.get_template("python_config_fk.py.j2")

    # 外键的**目标表**主键可重复时，目标表只有 find_all_by_id、没有 find_by_id。
    # 模板要按目标表的形态选调用方式，所以先把全集算出来传进去。
    multi_pk_targets: set[str] = {t.name for t in tables if t.multi_primary_key}

    for t in tables:
        module: str = type_mapping.to_python_module(t.name)
        ctx = _python_ctx(t, cfg, multi_pk_targets)
        write_file(cfg.python.code_dir / f"{module}.py", _clean_python_output(py_tpl.render(**ctx)))
        if t.has_foreign_keys:
            fk_ctx = _python_fk_ctx(t, cfg, multi_pk_targets)
            write_file(cfg.python.code_dir / f"{module}_fk.py", _clean_python_output(fk_tpl.render(**fk_ctx)))
            logger.info("Generated Python FK: %s", t.name)
        logger.info("Generated Python config: %s", t.name)

    names: list[str] = sorted(t.name for t in tables)
    all_tpl: Template = env.get_template("python_all_table.py.j2")
    write_file(
        cfg.python.code_dir / "all_table.py",
        _clean_python_output(all_tpl.render(sheetnames=names)),
    )


def _python_ctx(table: TableSchema, cfg: ExporterConfig, multi_pk_targets: set[str]) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        pb_module=type_mapping.to_python_pb_module(table.name),
        proto_import_path=cfg.python.proto_import_path,
        to_python_type=type_mapping.to_python_type,
        to_python_field=type_mapping.to_python_field,
        to_pascal_case=type_mapping.to_pascal_case,
        multi_pk_targets=multi_pk_targets,
    )


def _python_fk_ctx(table: TableSchema, cfg: ExporterConfig, multi_pk_targets: set[str]) -> dict:
    ctx: dict = _python_ctx(table, cfg, multi_pk_targets)
    ctx["fk_targets"] = table.fk_target_tables
    return ctx


# ---------------------------------------------------------------------------
# UE（Unreal Engine）
# ---------------------------------------------------------------------------

def _ue_comment(text: str) -> str:
    """Excel 注释压成一行 —— 生成的是 // 注释，里面带换行会把代码截断。"""
    return " ".join(str(text).split()) if text else ""


def _ue_row_fields(table: TableSchema) -> list[dict]:
    """走一遍列，产出 USTRUCT 的字段表。

    这条路必须与 ``core/schema.positional_field_numbers`` 和
    ``templates/proto_table.proto.j2`` **逐步一致**：先按列序走 server_columns
    （按名去重）收 set / 数组 / 非分组标量，再收全部 map，最后收全部子消息。
    走岔一步，产物里就会少一个 UPROPERTY —— 而少了的那一列不会报错，
    只会在运行时静默变成默认值。
    """
    fields: list[dict] = []
    seen: set[str] = set()
    col_to_group = table.col_to_group
    for col in table.server_columns:
        if col.name in seen:
            continue
        seen.add(col.name)
        if col.map_role == "set":
            fields.append(dict(
                kind="set", name=col.name,
                ue_type=f"TMap<{type_mapping.to_ue_type(col.data_type)}, bool>",
                init="", bp=type_mapping.ue_map_key_bp_safe(col.data_type),
                raw_key_type=col.data_type, comment=_ue_comment(col.comment)))
        elif col.map_role in ("map_key", "map_value"):
            continue
        elif col.name in table.arrays:
            elem: str = table.arrays[col.name].data_type
            fields.append(dict(
                kind="array", name=col.name,
                ue_type=f"TArray<{type_mapping.to_ue_type(elem)}>",
                init="", bp=True, raw_key_type="", comment=_ue_comment(col.comment)))
        elif col.excel_index not in col_to_group:
            fields.append(dict(
                kind="scalar", name=col.name,
                ue_type=type_mapping.to_ue_type(col.data_type),
                init=type_mapping.ue_default_init(col.data_type),
                bp=True, raw_key_type="", comment=_ue_comment(col.comment)))
    for m in table.maps.values():
        fields.append(dict(
            kind="map", name=m.name,
            ue_type=f"TMap<{type_mapping.to_ue_type(m.key_type)}, {type_mapping.to_ue_type(m.value_type)}>",
            init="", bp=type_mapping.ue_map_key_bp_safe(m.key_type),
            raw_key_type=m.key_type, comment=""))
    for g in table.groups.values():
        fields.append(dict(
            kind="group", name=g.name,
            ue_type=f"TArray<{type_mapping.ue_group_struct_name(table.name, g.name)}>",
            init="", bp=True, raw_key_type="", comment=""))
    return fields


def _ue_groups(table: TableSchema) -> list[dict]:
    """子消息 -> 嵌套 USTRUCT 的定义表。"""
    out: list[dict] = []
    for g in table.groups.values():
        out.append(dict(
            name=g.name,
            struct=type_mapping.ue_group_struct_name(table.name, g.name),
            fields=[dict(
                name=c.name,
                ue_type=type_mapping.to_ue_type(c.data_type),
                init=type_mapping.ue_default_init(c.data_type),
                bp=True,
                comment=_ue_comment(c.comment),
            ) for c in g.columns],
        ))
    return out


def _ue_ctx(table: TableSchema, cfg: ExporterConfig, multi_pk_tables: set[str]) -> dict:
    return dict(
        table=table,
        sheetname=table.name,
        # UE 这门把 languages.ue.package 当作**模块导出宏**（UCLASS 前的 XXX_API）用。
        # 单模块工程留空即可，模板就不写宏。
        api=cfg.ue.package,
        row_struct=type_mapping.ue_row_struct_name(table.name),
        row_header=type_mapping.ue_row_header_name(table.name),
        mgr_class=type_mapping.ue_manager_class_name(table.name),
        mgr_header=type_mapping.ue_manager_header_name(table.name),
        snapshot_struct=type_mapping.ue_snapshot_struct_name(table.name),
        json_file=type_mapping.ue_json_file_name(table.name),
        id_type=type_mapping.to_ue_type(table.id_column.data_type),
        fields=_ue_row_fields(table),
        groups=_ue_groups(table),
        # 外键的**目标表**主键可重复时它只有 FindAllById，没有 FindByIdSilent。
        # 与 C# 那门同一个坑：只挡源表是挡不住的。
        multi_pk_tables=multi_pk_tables,
        to_ue_type=type_mapping.to_ue_type,
        to_ue_param_type=type_mapping.to_ue_param_type,
        ue_default_init=type_mapping.ue_default_init,
        ue_composite_struct_name=type_mapping.ue_composite_struct_name,
        to_pascal_case=type_mapping.to_pascal_case,
        to_camel_case=type_mapping.to_camel_case,
    )


def _ue_fk_ctx(table: TableSchema, cfg: ExporterConfig, multi_pk_tables: set[str]) -> dict:
    ctx: dict = _ue_ctx(table, cfg, multi_pk_tables)
    ctx["fk_targets"] = table.fk_target_tables
    return ctx


def _gen_all_ue(tables: list[TableSchema], env: Environment, cfg: ExporterConfig) -> None:
    row_tpl: Template = env.get_template("ue_row.h.j2")
    h_tpl: Template = env.get_template("ue_config.h.j2")
    cpp_tpl: Template = env.get_template("ue_config.cpp.j2")
    fk_tpl: Template = env.get_template("ue_config_fk.h.j2")

    multi_pk_tables: set[str] = {t.name for t in tables if t.multi_primary_key}

    for t in tables:
        ctx = _ue_ctx(t, cfg, multi_pk_tables)
        write_file(cfg.ue.code_dir / f"Cfg{t.name}Row.h", _clean_output(row_tpl.render(**ctx)))
        write_file(cfg.ue.code_dir / f"{t.name}Table.h", _clean_output(h_tpl.render(**ctx)))
        write_file(cfg.ue.code_dir / f"{t.name}Table.cpp", _clean_output(cpp_tpl.render(**ctx)))
        if t.has_foreign_keys:
            fk_ctx = _ue_fk_ctx(t, cfg, multi_pk_tables)
            write_file(cfg.ue.code_dir / f"{t.name}TableFK.h", _clean_output(fk_tpl.render(**fk_ctx)))
            logger.info("Generated UE FK: %s", t.name)
        logger.info("Generated UE config: %s", t.name)

    names: list[str] = sorted(t.name for t in tables)
    ah_tpl: Template = env.get_template("ue_all_table.h.j2")
    ac_tpl: Template = env.get_template("ue_all_table.cpp.j2")
    write_file(cfg.ue.code_dir / "ConfigSubsystem.h",
               _clean_output(ah_tpl.render(sheetnames=names, api=cfg.ue.package)))
    write_file(cfg.ue.code_dir / "ConfigSubsystem.cpp",
               _clean_output(ac_tpl.render(sheetnames=names, api=cfg.ue.package)))

"""Schema models for data tables.

Defines the data structures that represent parsed Excel table metadata.
All generators consume these models instead of raw Excel data.

Excel header format (5 rows):
    Row 1: field name — one name per logical field span (first col of span)
    Row 2: type declaration — ``uint32``, ``map<K,V>``, ``set<T>``,
           ``repeated uint32``, ``repeated { uint32 f1; uint32 f2 }``
    Row 3: owner — "server", "client", "common", "design", or "constants_name"
    Row 4: options — space-separated tokens: bit_index, key, multi, tip_ref,
           fk:T, gfk:T, expr:type, expr_params:a,b
    Row 5: comment
    Row 6+: data
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional


@dataclass
class ForeignKeyRef:
    """A column-level foreign key reference.

    Parsed from option token ``fk:Table`` or ``fk:Table.column``.
    """
    target_table: str
    target_column: str = "id"

    @staticmethod
    def parse(text: str) -> Optional[ForeignKeyRef]:
        """Parse 'fk:Table' or 'fk:Table.column' syntax."""
        if not text or not text.strip().lower().startswith("fk:"):
            return None
        body: str = text.strip()[3:]
        if "." in body:
            table, column = body.split(".", 1)
            return ForeignKeyRef(target_table=table.strip(), target_column=column.strip())
        return ForeignKeyRef(target_table=body.strip())


@dataclass
class GroupForeignKeyRef:
    """A group-level foreign key reference.

    Parsed from option token ``gfk:Table``.
    """
    target_table: str

    @staticmethod
    def parse(text: str) -> Optional[GroupForeignKeyRef]:
        """Parse 'gfk:Table' syntax."""
        if not text or not text.strip().lower().startswith("gfk:"):
            return None
        body: str = text.strip()[4:]
        return GroupForeignKeyRef(target_table=body.strip())


@dataclass
class ColumnDef:
    """Metadata for a single Excel column, extracted from the 6-row header."""
    name: str
    data_type: str                                  # proto type: int32, string, …
    owner: str = ""                                 # server | client | common | design
    struct: str = ""                                # repeated | set | map_key | map_value | message:Name | ""
    options: list[str] = field(default_factory=list)  # space-separated tokens
    comment: str = ""
    excel_index: int = 0                            # 0-based column index
    constants_name: Optional[str] = None

    # ----- struct-derived properties -----

    @property
    def map_role(self) -> str:
        if self.struct in ("set", "map_key", "map_value"):
            return self.struct
        return ""

    @property
    def is_repeated(self) -> bool:
        return self.struct == "repeated"

    @property
    def is_set(self) -> bool:
        return self.struct == "set"

    @property
    def message_name(self) -> str:
        if self.struct.startswith("message:"):
            return self.struct.split(":", 1)[1]
        return ""

    # ----- option-derived properties -----

    @property
    def is_table_key(self) -> bool:
        return "key" in self.options

    @property
    def is_multi_key(self) -> bool:
        return "multi" in self.options

    @property
    def composite_key_group(self) -> str:
        """Return group name if this column participates in a composite key, else ''."""
        for opt in self.options:
            if opt.startswith("composite:"):
                return opt[10:]
        return ""

    @property
    def has_bit_index(self) -> bool:
        return "bit_index" in self.options

    @property
    def expression_type(self) -> str:
        for opt in self.options:
            if opt.startswith("expr:"):
                return opt[5:]
        return ""

    @property
    def expression_params(self) -> list[str]:
        for opt in self.options:
            if opt.startswith("expr_params:"):
                return [p.strip() for p in opt[12:].split(",") if p.strip()]
        return []

    @property
    def foreign_key(self) -> Optional[ForeignKeyRef]:
        for opt in self.options:
            if opt.lower().startswith("fk:"):
                return ForeignKeyRef.parse(opt)
        return None

    @property
    def group_foreign_key(self) -> Optional[GroupForeignKeyRef]:
        for opt in self.options:
            if opt.lower().startswith("gfk:"):
                return GroupForeignKeyRef.parse(opt)
        return None

    @property
    def is_indexed(self) -> bool:
        """True if this column has an explicit ``idx`` option."""
        return "idx" in self.options

    # ----- owner-derived properties -----

    @property
    def is_server(self) -> bool:
        return self.owner in ("server", "common")

    @property
    def is_excluded(self) -> bool:
        return self.owner in ("client", "design")


@dataclass
class ArrayField:
    """Columns with the same name and ``struct: repeated`` → repeated proto field."""
    name: str
    data_type: str
    indices: list[int] = field(default_factory=list)


@dataclass
class MapField:
    """Consecutive ``map_key`` + ``map_value`` column pairs → proto ``map`` field."""
    name: str           # common prefix (e.g. "tag" from "tag_key"/"tag_value")
    key_type: str
    value_type: str


@dataclass
class GroupField:
    """Columns sharing the same ``message:Name`` → repeated sub-message field."""
    name: str
    columns: list[ColumnDef] = field(default_factory=list)   # unique column defs
    indices: list[int] = field(default_factory=list)          # all column positions


@dataclass
class CompositeKeyDef:
    """Two or more columns forming a composite key lookup.

    Parsed from columns having ``composite:group_name`` in their options.
    """
    group: str
    columns: list[ColumnDef] = field(default_factory=list)


@dataclass
class TableSchema:
    """Complete schema for one Excel sheet / data table."""
    name: str
    source_path: Optional[Path] = None
    columns: list[ColumnDef] = field(default_factory=list)
    arrays: dict[str, ArrayField] = field(default_factory=dict)
    groups: dict[str, GroupField] = field(default_factory=dict)
    maps: dict[str, MapField] = field(default_factory=dict)
    # 主键可重复:同一个 id 允许出现在多行。由主键列上的 (cfg_multi) 决定。
    #
    # 这不是新概念 —— 旧生成器读 A5=='multi' 时就有,重写导表器时模板没接上,
    # 于是这个字段变成了零消费者的悬空标记(`_old/gen_xls_to_language_config_file.py:57`)。
    # 现在它真的会改变产物:主键索引变多值,并且**不生成 FindById,只生成 FindAllById**,
    # 让「把可重复主键当单值用」在编译期就断,而不是悄悄只看第一行。
    multi_primary_key: bool = False
    has_constants_name: bool = False
    constants_name_index: Optional[int] = None

    # proto 字段号台账 {字段名: 号}。**产物模板只认这一份**,不再自己发号。
    #
    # 从前 proto_table.proto.j2 用一个自增计数器按列序发号,于是「在表中间插一列」
    # 会把它后面所有字段的号整体 +1,而 .pb 是 git-tracked 的二进制:旧数据被
    # ParseFromString 无异常地整体错读。现在号由来源层显式给出:
    #   - 权威 schema 路径:直接取 .proto 里人写的 `= N`
    #   - 旧表头路径:用 positional_field_numbers() 复算出与历史完全一致的号
    field_numbers: dict[str, int] = field(default_factory=dict)

    # ----- convenience properties -----

    @property
    def server_columns(self) -> list[ColumnDef]:
        return [c for c in self.columns if c.is_server]

    @property
    def id_column(self) -> ColumnDef:
        """The ``id`` column (always first server column named 'id')."""
        for c in self.columns:
            if c.name == "id" and c.is_server:
                return c
        # fallback: first server column
        return self.server_columns[0]

    @property
    def table_keys(self) -> list[ColumnDef]:
        return [c for c in self.columns if c.is_table_key]

    @property
    def expression_columns(self) -> list[ColumnDef]:
        return [c for c in self.columns if c.expression_type]

    @property
    def foreign_key_columns(self) -> list[ColumnDef]:
        seen: set[str] = set()
        result: list[ColumnDef] = []
        for c in self.columns:
            if c.foreign_key and c.name not in seen:
                seen.add(c.name)
                result.append(c)
        return result

    @property
    def group_foreign_key_columns(self) -> list[ColumnDef]:
        seen: set[str] = set()
        result: list[ColumnDef] = []
        for c in self.columns:
            if c.group_foreign_key and c.name not in seen:
                seen.add(c.name)
                result.append(c)
        return result

    @property
    def has_foreign_keys(self) -> bool:
        return bool(self.foreign_key_columns or self.group_foreign_key_columns)

    @property
    def fk_target_tables(self) -> list[str]:
        """Unique sorted list of target table names referenced by FK/GFK."""
        targets: set[str] = set()
        for c in self.foreign_key_columns:
            fk: ForeignKeyRef | None = c.foreign_key
            if fk:
                targets.add(fk.target_table)
        for c in self.group_foreign_key_columns:
            gfk: GroupForeignKeyRef | None = c.group_foreign_key
            if gfk:
                targets.add(gfk.target_table)
        return sorted(targets)

    @property
    def index_columns(self) -> list[ColumnDef]:
        """Scalar columns that should have a secondary index.

        Includes columns with explicit ``idx`` option and scalar FK columns
        (auto-indexed).  Deduplicates by column name.
        """
        seen: set[str] = set()
        result: list[ColumnDef] = []
        for c in self.columns:
            if c.name in seen:
                continue
            if c.is_indexed or (c.foreign_key and not c.is_repeated):
                seen.add(c.name)
                result.append(c)
        return result

    @property
    def has_indexes(self) -> bool:
        return bool(self.index_columns)

    @property
    def composite_keys(self) -> list[CompositeKeyDef]:
        """Columns grouped by ``composite:group_name`` option."""
        groups: dict[str, list[ColumnDef]] = {}
        for c in self.columns:
            grp: str = c.composite_key_group
            if grp:
                groups.setdefault(grp, []).append(c)
        return [CompositeKeyDef(group=g, columns=cols) for g, cols in groups.items() if len(cols) >= 2]

    @property
    def bit_index_columns(self) -> list[ColumnDef]:
        return [c for c in self.columns if c.has_bit_index]

    @property
    def map_keys(self) -> list[ColumnDef]:
        return [c for c in self.columns if c.map_role == "map_key"]

    @property
    def set_columns(self) -> list[ColumnDef]:
        return [c for c in self.columns if c.map_role == "set"]

    @property
    def col_to_group(self) -> dict[int, str]:
        """Map column index → group name for grouped columns."""
        result: dict[int, str] = {}
        for g in self.groups.values():
            for idx in g.indices:
                result[idx] = g.name
        return result

    # ----- component generation helpers -----

    @property
    def scalar_comp_columns(self) -> list[ColumnDef]:
        """Server columns eligible for scalar ECS component generation.

        Excludes: map, set, repeated, grouped, and excluded (client/design) columns.
        Returns one ColumnDef per unique field name (deduped).
        """
        group_indices = set()
        for g in self.groups.values():
            group_indices.update(g.indices)
        seen: set[str] = set()
        result: list[ColumnDef] = []
        for c in self.columns:
            if not c.is_server:
                continue
            if c.map_role or c.is_repeated or c.is_set:
                continue
            if c.excel_index in group_indices:
                continue
            if c.name in seen or c.name in self.arrays:
                continue
            seen.add(c.name)
            result.append(c)
        return result

    @property
    def repeated_comp_arrays(self) -> list["ArrayField"]:
        """Repeated scalar arrays eligible for ECS component + value index.

        Returns ArrayField objects (not grouped sub-messages).
        """
        return [a for a in self.arrays.values()]


def positional_field_numbers(schema: TableSchema) -> dict[str, int]:
    """按历史规则(列序)复算 proto 字段号。

    这是 ``templates/proto_table.proto.j2`` 从前那个自增发号器的等价实现:
    先按列序走 ``server_columns``(按名去重)发给 set / 数组 / 非分组标量,
    再发给全部 map,最后发给全部子消息。**不是声明序。**

    只有两个用途:(1) 尚未迁移到权威 schema 的表在导表时取号;
    (2) 播种权威 schema 时核对人写的号是否等于历史号。
    新表不该依赖它 —— 号应当写在 ``data/schema/<Sheet>_table.proto`` 里。
    """
    out: dict[str, int] = {}
    seen: set[str] = set()
    col_to_group = schema.col_to_group
    idx = 1
    for col in schema.server_columns:
        if col.name in seen:
            continue
        seen.add(col.name)
        if col.map_role == "set":
            out[col.name] = idx
            idx += 1
        elif col.map_role in ("map_key", "map_value"):
            continue
        elif col.name in schema.arrays:
            out[col.name] = idx
            idx += 1
        elif col.excel_index not in col_to_group:
            out[col.name] = idx
            idx += 1
    for name in schema.maps:
        out[name] = idx
        idx += 1
    for name in schema.groups:
        out[name] = idx
        idx += 1
    return out

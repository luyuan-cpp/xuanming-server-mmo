"""新旧两条 schema 路径的等价性判据。

放在 ``core/`` 而不是 ``tools/`` 下,是为了让播种器与对拍脚本共用同一份定义 ——
两个 ``tools/`` 脚本互相 import 会靠 sys.path hack 把对方降级成库。

等价的定义是**下游看得见的一切都相同**:物理列展开(名字/类型/owner/struct/options/
注释/列下标),以及全部被生成器与模板消费的派生属性。

注意这份 diff 的证明力边界:``arrays``/``groups``/``maps`` 与那 15 项派生属性,
两路走的是**同一个** ``_detect_layout`` 和**同一批** dataclass property,
所以真正独立的维度只有 ``name``、``columns`` 七元组、``multi_primary_key`` 三条。
比较其余项是为了在将来任一路自己算这些东西时仍能兜住,不是当前的增量证据。
"""

from __future__ import annotations

from core.schema import TableSchema

_COL_FIELD_NAMES = ("列下标", "名字", "类型", "owner", "struct", "options", "注释")


def _col_tuple(c) -> tuple:
    return (c.excel_index, c.name, c.data_type, c.owner, c.struct,
            tuple(sorted(c.options)), c.comment)


def _fk(c) -> tuple:
    f, g = c.foreign_key, c.group_foreign_key
    return ((f.target_table, f.target_column) if f else None,
            g.target_table if g else None)


def derived(s: TableSchema) -> dict:
    """全部被模板/生成器消费的派生属性。新增模板属性时这里要同步加。"""
    return {
        "server_columns": [c.name for c in s.server_columns],
        "id_column": s.id_column.name if s.server_columns else None,
        "table_keys": [(c.name, c.is_multi_key) for c in s.table_keys],
        "index_columns": [c.name for c in s.index_columns],
        "foreign_key_columns": [(c.name, _fk(c)) for c in s.foreign_key_columns],
        "group_foreign_key_columns": [(c.name, _fk(c)) for c in s.group_foreign_key_columns],
        "fk_target_tables": s.fk_target_tables,
        "composite_keys": [(k.group, [c.name for c in k.columns]) for k in s.composite_keys],
        "bit_index_columns": [c.name for c in s.bit_index_columns],
        "expression_columns": [(c.name, c.expression_type, c.expression_params)
                               for c in s.expression_columns],
        "map_keys": [c.name for c in s.map_keys],
        "set_columns": [c.name for c in s.set_columns],
        "scalar_comp_columns": [c.name for c in s.scalar_comp_columns],
        "repeated_comp_arrays": [a.name for a in s.repeated_comp_arrays],
        "col_to_group": dict(sorted(s.col_to_group.items())),
    }


def diff(old: TableSchema, new: TableSchema) -> list[str]:
    """返回人可读的差异列表;空列表 = 等价。"""
    out: list[str] = []

    if old.name != new.name:
        out.append("表名: %r != %r" % (old.name, new.name))

    a = [_col_tuple(c) for c in old.columns]
    b = [_col_tuple(c) for c in new.columns]
    if len(a) != len(b):
        out.append("物理列数: 旧 %d != 新 %d" % (len(a), len(b)))
    for i, (x, y) in enumerate(zip(a, b)):
        for k, (xv, yv) in enumerate(zip(x, y)):
            if xv != yv:
                out.append("第 %d 个物理列 %s: 旧 %r != 新 %r"
                           % (i, _COL_FIELD_NAMES[k], xv, yv))

    for label, ov, nv in (
        ("arrays", {k: (v.data_type, v.indices) for k, v in old.arrays.items()},
                   {k: (v.data_type, v.indices) for k, v in new.arrays.items()}),
        ("maps", {k: (v.key_type, v.value_type) for k, v in old.maps.items()},
                 {k: (v.key_type, v.value_type) for k, v in new.maps.items()}),
        ("groups", {k: ([c.name for c in v.columns], v.indices) for k, v in old.groups.items()},
                   {k: ([c.name for c in v.columns], v.indices) for k, v in new.groups.items()}),
        ("multi_primary_key", old.multi_primary_key, new.multi_primary_key),
        ("has_constants_name", old.has_constants_name, new.has_constants_name),
        ("constants_name_index", old.constants_name_index, new.constants_name_index),
    ):
        if ov != nv:
            out.append("%s: 旧 %r != 新 %r" % (label, ov, nv))

    do, dn = derived(old), derived(new)
    for k in do:
        if do[k] != dn[k]:
            out.append("派生属性 %s: 旧 %r != 新 %r" % (k, do[k], dn[k]))
    return out

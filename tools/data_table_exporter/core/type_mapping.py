"""Language-specific type conversions.

Centralises the mapping from proto/Excel types to C++, Go, and Java types.
"""

from __future__ import annotations

import keyword
import re

# ---------------------------------------------------------------------------
# C++
# ---------------------------------------------------------------------------

_CPP_TYPE_MAP: dict[str, str] = {
    "int32":  "int32_t",
    "int64":  "int64_t",
    "uint32": "uint32_t",
    "uint64": "uint64_t",
    "float":  "float",
    "double": "double",
    "bool":   "bool",
    "string": "std::string",
}


def to_cpp_type(proto_type: str) -> str:
    return _CPP_TYPE_MAP.get(proto_type, proto_type)


def to_cpp_param_type(proto_type: str) -> str:
    """C++ function parameter type (const ref for strings)."""
    if proto_type == "string":
        return "const std::string&"
    return to_cpp_type(proto_type)


def cpp_map_type(is_multi: bool) -> str:
    return "unordered_multimap" if is_multi else "unordered_map"


# --- Component helpers (for per-column ECS component generation) ---

_CPP_COMP_TYPE_MAP: dict[str, str] = {
    "int32":  "int32_t",
    "int64":  "int64_t",
    "uint32": "uint32_t",
    "uint64": "uint64_t",
    "float":  "float",
    "double": "double",
    "bool":   "bool",
    "string": "std::string_view",
}


def to_cpp_comp_type(proto_type: str) -> str:
    """C++ type for a per-column ECS component value (string → string_view)."""
    return _CPP_COMP_TYPE_MAP.get(proto_type, proto_type)


def to_cpp_repeated_elem_type(proto_type: str) -> str:
    """C++ element type inside a proto RepeatedField (for span)."""
    return _CPP_TYPE_MAP.get(proto_type, proto_type)


# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

_GO_TYPE_MAP: dict[str, str] = {
    "int32":  "int32",
    "int64":  "int64",
    "uint32": "uint32",
    "uint64": "uint64",
    "float":  "float32",
    "double": "float64",
    "bool":   "bool",
    "string": "string",
}


def to_go_type(proto_type: str) -> str:
    return _GO_TYPE_MAP.get(proto_type, "interface{}")


def to_go_proto_field(snake_name: str) -> str:
    """Convert snake_case proto field name to Go CamelCase struct field.

    Example: ``sub_buff`` → ``SubBuff``, ``id`` → ``Id``.
    """
    return "".join(word.capitalize() for word in snake_name.split("_"))


def to_go_repeated_elem_type(proto_type: str) -> str:
    """Go element type for repeated fields (same as to_go_type)."""
    return _GO_TYPE_MAP.get(proto_type, "interface{}")


# ---------------------------------------------------------------------------
# Java
# ---------------------------------------------------------------------------

_JAVA_TYPE_MAP: dict[str, str] = {
    "int32":  "int",
    "int64":  "long",
    "uint32": "int",
    "uint64": "long",
    "float":  "float",
    "double": "double",
    "bool":   "boolean",
    "string": "String",
}

_JAVA_BOXED_MAP: dict[str, str] = {
    "int32":  "Integer",
    "int64":  "Long",
    "uint32": "Integer",
    "uint64": "Long",
    "float":  "Float",
    "double": "Double",
    "bool":   "Boolean",
    "string": "String",
}


def to_java_type(proto_type: str) -> str:
    return _JAVA_TYPE_MAP.get(proto_type, proto_type)


def to_java_boxed(proto_type: str) -> str:
    return _JAVA_BOXED_MAP.get(proto_type, proto_type)


def to_java_proto_getter(snake_name: str) -> str:
    """Convert snake_case field name to Java proto getter (e.g. ``sub_buff`` → ``getSubBuff``)."""
    camel: str = "".join(word.capitalize() for word in snake_name.split("_"))
    return f"get{camel}"


def to_java_repeated_elem_type(proto_type: str) -> str:
    """Java element type inside a proto repeated field (boxed for generics)."""
    return _JAVA_BOXED_MAP.get(proto_type, proto_type)


# ---------------------------------------------------------------------------
# Shared case-conversion helpers
# ---------------------------------------------------------------------------

def to_pascal_case(snake_name: str) -> str:
    """Convert snake_case to PascalCase.  e.g. ``string_key`` → ``StringKey``."""
    return "".join(word.capitalize() for word in snake_name.split("_"))


def to_camel_case(snake_name: str) -> str:
    """Convert snake_case to camelCase.  e.g. ``string_key`` → ``stringKey``."""
    parts: list[str] = snake_name.split("_")
    return parts[0] + "".join(word.capitalize() for word in parts[1:])


# ---------------------------------------------------------------------------
# Composite key helpers
# ---------------------------------------------------------------------------

def composite_go_struct_name(group: str) -> str:
    """Go struct name for a composite key: ``class_level`` → ``ClassLevelKey``."""
    return "".join(word.capitalize() for word in group.split("_")) + "Key"


def composite_cpp_struct_name(group: str) -> str:
    """C++ struct name for a composite key: ``class_level`` → ``ClassLevelKey``."""
    return "".join(word.capitalize() for word in group.split("_")) + "Key"



# ---------------------------------------------------------------------------
# C#  (Unity 客户端，Google.Protobuf 运行时)
# ---------------------------------------------------------------------------

_CSHARP_TYPE_MAP: dict[str, str] = {
    "int32":  "int",
    "int64":  "long",
    # 注意与 Java 的差异：Java 把 uint32/uint64 映射成 int/long（无符号靠约定），
    # 而 protoc --csharp_out 生成的属性类型就是 uint/ulong。这里必须跟 protoc 走，
    # 否则 Dictionary<int, T> 的键类型和 row.Id 的 uint 对不上，编译期就断。
    "uint32": "uint",
    "uint64": "ulong",
    "float":  "float",
    "double": "double",
    "bool":   "bool",
    "string": "string",
}

# protoc csharp 生成器保留的成员名；字段属性名撞上它们（或撞上外层 message 名）
# 会被追加一个下划线。见 protobuf/src/google/protobuf/compiler/csharp/csharp_helpers.cc
# 的 GetPropertyName()。现网 21 张表里 GlobalVariable.to_string 就命中 ToString -> ToString_。
_CSHARP_RESERVED_MEMBERS: frozenset[str] = frozenset({
    "Types", "Descriptor", "Equals", "ToString", "GetHashCode",
    "WriteTo", "Clone", "CalculateSize", "MergeFrom", "OnConstruction",
    "Parser",
})


def to_csharp_type(proto_type: str) -> str:
    """proto 标量类型 -> C# 类型（与 protoc --csharp_out 生成的属性类型一致）。"""
    return _CSHARP_TYPE_MAP.get(proto_type, proto_type)


def to_csharp_repeated_elem_type(proto_type: str) -> str:
    """RepeatedField<T> 里的元素类型。C# 没有装箱式泛型限制，与标量映射相同。"""
    return _CSHARP_TYPE_MAP.get(proto_type, proto_type)


def _underscores_to_pascal_case(name: str) -> str:
    """protoc csharp 的 UnderscoresToPascalCase 的等价实现。

    照抄 protobuf/src/google/protobuf/compiler/csharp/names.cc 的 UnderscoresToCamelCase：
    **数字也会把下一个字母顶成大写**（``testobj1_key`` -> ``Testobj1Key``），
    这一条和通用的 to_pascal_case() 不同，不能拿 to_pascal_case 顶替 ——
    顶替了就会生成 ``row.Testobj1key`` 这种不存在的属性。
    """
    out: list[str] = []
    cap_next: bool = True
    for i, ch in enumerate(name):
        if "a" <= ch <= "z":
            out.append(ch.upper() if cap_next else ch)
            cap_next = False
        elif "A" <= ch <= "Z":
            out.append(ch.lower() if (i == 0 and not cap_next) else ch)
            cap_next = False
        elif "0" <= ch <= "9":
            out.append(ch)
            cap_next = True
        else:
            cap_next = True
    if name.endswith("#"):
        out.append("_")
    result: str = "".join(out)
    # protobuf issue #8101：_1abc 这种要保留前导下划线，否则标识符以数字开头。
    if result and "0" <= result[0] <= "9" and name.startswith("_"):
        result = "_" + result
    return result


def to_csharp_proto_prop(snake_name: str, message_name: str = "") -> str:
    """snake_case 字段名 -> protoc 生成的 C# 属性名。

    ``message_name`` 传外层消息类名（如 ``ItemTable``）以复现 protoc 的撞名规则：
    属性名等于外层类名或落在保留成员集合里时追加 ``_``。
    """
    prop: str = _underscores_to_pascal_case(snake_name)
    if prop == message_name or prop in _CSHARP_RESERVED_MEMBERS:
        prop += "_"
    return prop


def csharp_composite_struct_name(group: str) -> str:
    """复合键的 C# struct 名：``class_level`` -> ``ClassLevelKey``。"""
    return "".join(word.capitalize() for word in group.split("_")) + "Key"




# ---------------------------------------------------------------------------
# Python
# ---------------------------------------------------------------------------

_PYTHON_TYPE_MAP: dict[str, str] = {
    "int32":  "int",
    "int64":  "int",
    "uint32": "int",
    "uint64": "int",
    "float":  "float",
    "double": "float",
    "bool":   "bool",
    "string": "str",
}


def to_python_type(proto_type: str) -> str:
    """proto 标量类型 -> Python 类型标注名。

    proto 的六种整数在 Python 里都是 ``int``，``float``/``double`` 都是 ``float``：
    protobuf-python 不区分宽度，标注里写宽度是假精度。未知类型回落到 ``object``
    而不是 ``Any`` —— 让类型检查器把它报出来，而不是悄悄放行。
    """
    return _PYTHON_TYPE_MAP.get(proto_type, "object")


_PY_NON_IDENT: re.Pattern[str] = re.compile(r"\W")


def to_python_field(snake_name: str) -> str:
    """proto 字段名 -> protobuf-python 的属性名。

    protobuf-python 的属性名**就是** proto 里的字段名（不做 CamelCase 转换），
    所以这个函数在正常输入下是恒等的。它存在只为兜两件事：

    1. 非标识符字符 —— 旧表头那条路的字段名是从 Excel 里抠出来的，没人保证干净；
    2. Python 关键字 —— proto 里合法的 ``class`` / ``from`` / ``lambda``，
       写成 ``row.class`` 就是语法错误。protobuf-python 对这种字段没有改名规则，
       这里加下划线**只能让产物编译过、并不能真读到那一列**，所以真撞上了必须改表。
       留这个显式转换点，是为了那天报错发生在生成期而不是运行期。
    """
    name = _PY_NON_IDENT.sub("_", snake_name)
    if not name or name[0].isdigit():
        name = "_" + name
    if keyword.iskeyword(name):
        name += "_"
    return name


def to_python_pb_module(sheet_name: str) -> str:
    """表名 -> protoc ``--python_out`` 产出的模块名。

    ``TestMultiKey`` -> ``testmultikey_table_pb2``。口径必须和
    ``core/generators/binary_gen.py`` 里 import 的那个名字一致。
    """
    return f"{sheet_name.lower()}_table_pb2"


def to_python_module(sheet_name: str) -> str:
    """表名 -> 生成的管理器模块名。``TestMultiKey`` -> ``testmultikey_table``。

    刻意与 Go/C++ 的文件名口径（全小写、不插下划线）一致，也和 ``*_table_pb2``
    对得上；改成 snake_case 会让同一张表在两棵树里叫两个名字。
    """
    return f"{sheet_name.lower()}_table"


# ---------------------------------------------------------------------------
# UE（Unreal Engine，不引 protobuf；产物读 generated/tables/<表名小写>.json）
# ---------------------------------------------------------------------------

_UE_TYPE_MAP: dict[str, str] = {
    "int32":  "int32",
    "int64":  "int64",
    # uint32/uint64 **刻意映射成有符号**。UHT 对带 Blueprint 说明符的无符号整数报
    # "Type 'uint32' is not supported by blueprint."：Blueprint 的整数只认
    # uint8 / int32 / int64。配置行不能被 Blueprint 读就没什么用，所以宁可牺牲
    # 无符号语义。代价：>= 2^31 的 uint32 会读成负数（当前 21 张表的 id/计数远在
    # 这条线以下）。真要放大数，改成 int64，**不要**改回 uint32。
    # 注意这一条与 C# 那门相反：C# 必须跟 protoc --csharp_out 的 uint/ulong 对齐，
    # UE 这门不碰 protobuf，没有这个约束，跟 Java 走同一口径。
    "uint32": "int32",
    "uint64": "int64",
    "float":  "float",
    "double": "double",
    "bool":   "bool",
    "string": "FString",
}


def to_ue_type(proto_type: str) -> str:
    """proto 标量类型 -> UE 类型。未知类型原样返回，让 UE 编译期把它顶出来。"""
    return _UE_TYPE_MAP.get(proto_type, proto_type)


def to_ue_param_type(proto_type: str) -> str:
    """UE 函数参数类型（FString 传 const 引用，其余按值）。"""
    return "const FString&" if proto_type == "string" else to_ue_type(proto_type)


def ue_default_init(proto_type: str) -> str:
    """USTRUCT 字段的类内初值。

    **不是洁癖**：导表器只把「Excel 里填了东西的格子」写进 JSON，空格子整个键不出现；
    而 FJsonObjectConverter 对 JSON 里没有的属性是跳过、不清零。行结构体是在栈上现构的，
    不给初值就会把脏内存当配置读。
    """
    ue: str = to_ue_type(proto_type)
    if ue in ("int32", "int64"):
        return " = 0"
    if ue in ("float", "double"):
        return " = 0.0"
    if ue == "bool":
        return " = false"
    return ""   # FString / TArray / TMap 默认构造即空


# Blueprint 能做 TMap 键的类型（UE 类型名）。float/double/bool 做键 UHT 会拒。
_UE_BP_KEY_TYPES: frozenset[str] = frozenset({"int32", "int64", "FString"})


def ue_map_key_bp_safe(proto_type: str) -> bool:
    """这个键类型能不能让 TMap 属性挂 BlueprintReadOnly。"""
    return to_ue_type(proto_type) in _UE_BP_KEY_TYPES


def ue_row_struct_name(sheet_name: str) -> str:
    """``TestMultiKey`` -> ``FCfgTestMultiKeyRow``。"""
    return f"FCfg{sheet_name}Row"


def ue_row_header_name(sheet_name: str) -> str:
    """行结构体的头文件基名（.generated.h 必须与它同名）。"""
    return f"Cfg{sheet_name}Row"


def ue_manager_class_name(sheet_name: str) -> str:
    return f"U{sheet_name}Table"


def ue_manager_header_name(sheet_name: str) -> str:
    return f"{sheet_name}Table"


def ue_snapshot_struct_name(sheet_name: str) -> str:
    return f"FCfg{sheet_name}Snapshot"


def ue_group_struct_name(sheet_name: str, group: str) -> str:
    """子消息 USTRUCT 名：``TestMultiKey`` + ``testobj1`` -> ``FCfgTestMultiKeyTestobj1``。"""
    return f"FCfg{sheet_name}{to_pascal_case(group)}"


def ue_composite_struct_name(sheet_name: str, group: str) -> str:
    """复合键 struct 名（**不是** USTRUCT，只做 TMap 的键）。"""
    return f"FCfg{sheet_name}{to_pascal_case(group)}Key"


def ue_json_file_name(sheet_name: str) -> str:
    """``TestMultiKey`` -> ``testmultikey.json``。与 json_gen.py 的全小写口径一致。"""
    return f"{sheet_name.lower()}.json"

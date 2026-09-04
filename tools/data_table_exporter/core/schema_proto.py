"""权威 schema 来源:从 ``data/schema/<Sheet>_table.proto`` 构造 :class:`TableSchema`。

这是 ``excel_reader._parse_columns`` 的替代路径。两者产出**同一个** ``TableSchema``,
所以下游 9 个生成器与全部 Jinja 模板一行都不用改(实测:``templates/*.j2`` 对
``.options`` / ``.owner`` / ``.struct`` / ``.comment`` 零引用,只吃派生属性)。

与旧路的分工::

    旧路  xlsx 第 1~5 行       -> TableSchema   (类型/owner/选项/注释 全靠数格子和位置)
    新路  data/schema/*.proto  -> TableSchema   (全部显式书写)

两路都用 xlsx 第 6 行起的数据,``excel_reader.read_data_rows`` 不变。

绑定按**列名**做,不按声明序:每个字段用 ``(cfg_col)``(缺省=字段名)去 xlsx 第 1 行
找自己的起始列。策划在任意位置插列/删列/重排列序都不影响其它字段的解析结果。

不依赖 protoc。本文件自带一个只认本词表的文本解析器;protoc 只在 CI 里当语法 lint。

**option 词表不在本文件里硬编码**,而是运行时解析同目录的 ``cfg_options.proto``
(``extend`` 块与 ``enum`` 块)。这条改造的主题就是「不要有两处需要同步维护的真相」,
词表自己更没有资格例外:改 proto 词表即改解析器认识的东西,不存在漏同步。

失败一律 fail-closed —— 抛 :class:`SchemaProtoError`,产出不落盘。这个模块里
**没有一条静默降级路径**:认不出的行、认不出的 option、对不上的字段号,全部报错。
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass, field as dc_field
from pathlib import Path
from typing import Optional

import openpyxl

from core.excel_reader import _detect_layout
from core.schema import ColumnDef, TableSchema, positional_field_numbers

logger = logging.getLogger(__name__)


class SchemaProtoError(ValueError):
    """权威 schema 自身有问题,或与源表对不上。一律 fail-closed,产出不落盘。"""


# 被 owner 白名单丢弃的列不进产物,自然也没有产物字段号。它们在权威 schema 里仍要
# 声明(否则物理列展开会缺位),号从这一段发。不变式:``号 >= 这个基数`` 当且仅当
# 「这一列不进产物」—— build_table_schema 会双向核对。
EXCLUDED_FIELD_NUMBER_BASE = 9000

# protobuf 保留段,任何字段都不能用。
_PROTO_RESERVED_RANGE = (19000, 19999)
_PROTO_MAX_FIELD_NUMBER = 536870911

# owner 白名单与 schema.ColumnDef.is_server 保持一致。
#
# 空串是**已知的坏数据**,不是设计:owner 格漏填时 is_server 为假,整列被静默丢弃。
# data/GlobalVariable.xlsx 的 D~H 五列今天就是这个状态,表里有真值却一个都没进产物。
# 这里保留它是为了让新旧两路等价、迁移可对拍;构造时会逐列告警。修它会改变产物形状,
# 属于独立决策,不能夹带在格式迁移里。
_MISSING_OWNER = ""
_KNOWN_OWNERS = {"server", "common", "client", "design", "designer", "constants_name",
                 _MISSING_OWNER}

# proto3 内置标量。用来判定 ``repeated X`` 里的 X 到底是标量还是子消息。
_SCALAR_TYPES = {
    "double", "float", "int32", "int64", "uint32", "uint64", "sint32", "sint64",
    "fixed32", "fixed64", "sfixed32", "sfixed64", "bool", "string", "bytes",
}


# ---------------------------------------------------------------------------
# option 词表:运行时从 cfg_options.proto 解析,不在 Python 侧硬编码
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class OptionSpec:
    name: str
    type_name: str          # string | bool | uint32 | <枚举名>
    repeated: bool


@dataclass
class Vocabulary:
    message_options: dict[str, OptionSpec] = dc_field(default_factory=dict)
    field_options: dict[str, OptionSpec] = dc_field(default_factory=dict)
    enums: dict[str, set[str]] = dc_field(default_factory=dict)


_RE_EXTEND = re.compile(r"^\s*extend\s+([\w.]+)\s*\{")
_RE_EXTEND_FIELD = re.compile(r"^\s*(repeated\s+)?([\w.]+)\s+(\w+)\s*=\s*(\d+)\s*;")
_RE_ENUM = re.compile(r"^\s*enum\s+(\w+)\s*\{")
_RE_ENUM_VALUE = re.compile(r"^\s*(\w+)\s*=\s*(-?\d+)\s*;")

_VOCAB_CACHE: dict[Path, Vocabulary] = {}


def load_vocabulary(path: Path) -> Vocabulary:
    """解析 ``cfg_options.proto`` 的 extend / enum 块,得到合法 option 名与取值。"""
    key = path.resolve()
    cached = _VOCAB_CACHE.get(key)
    if cached is not None:
        return cached
    if not path.exists():
        raise SchemaProtoError("找不到 option 词表 %s;每份权威 schema 都 import 它" % path)

    vocab = Vocabulary()
    scope: Optional[str] = None       # 'message' | 'field' | 'enum:<名字>'
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = _strip_line_comment(raw)
        stripped = line.strip()
        if not stripped:
            continue
        m = _RE_EXTEND.match(line)
        if m:
            target = m.group(1)
            scope = ("message" if target.endswith("MessageOptions")
                     else "field" if target.endswith("FieldOptions") else None)
            continue
        m = _RE_ENUM.match(line)
        if m:
            scope = "enum:%s" % m.group(1)
            vocab.enums.setdefault(m.group(1), set())
            continue
        if stripped.startswith("}"):
            scope = None
            continue
        if scope in ("message", "field"):
            m = _RE_EXTEND_FIELD.match(line)
            if m:
                spec = OptionSpec(name=m.group(3), type_name=m.group(2),
                                  repeated=bool(m.group(1)))
                target = vocab.message_options if scope == "message" else vocab.field_options
                target[spec.name] = spec
        elif scope and scope.startswith("enum:"):
            m = _RE_ENUM_VALUE.match(line)
            if m:
                vocab.enums[scope[5:]].add(m.group(1))

    if not vocab.field_options:
        raise SchemaProtoError("%s 里没有解析到任何字段 option,词表不可用" % path)
    _VOCAB_CACHE[key] = vocab
    return vocab


# ---------------------------------------------------------------------------
# 极简 .proto 文本解析(只认本仓 data/schema 下的写法)
# ---------------------------------------------------------------------------

@dataclass
class ProtoField:
    name: str
    number: int
    type_expr: str          # 'uint32' | 'map<string, string>' | 'Testtestobj'
    repeated: bool
    opts: dict = dc_field(default_factory=dict)
    comment: str = ""
    lineno: int = 0


@dataclass
class ProtoMessage:
    name: str
    opts: dict = dc_field(default_factory=dict)
    fields: list[ProtoField] = dc_field(default_factory=list)
    reserved_numbers: set[int] = dc_field(default_factory=set)
    reserved_names: set[str] = dc_field(default_factory=set)


_RE_MESSAGE = re.compile(r"^\s*message\s+(\w+)\s*\{")
_RE_MSG_OPTION = re.compile(r"^\s*option\s*\(\s*([\w.]+)\s*\)\s*=\s*(.+?)\s*;")
_RE_FIELD = re.compile(
    r"^\s*(repeated\s+)?"
    r"(map\s*<[^>]+>|[\w.]+)\s+"
    r"(\w+)\s*=\s*(\d+)\s*"
    r"(?:\[(.*)\])?\s*;\s*$"
)
_RE_OPT_KV = re.compile(r"\(\s*([\w.]+)\s*\)\s*=\s*")
_RE_RESERVED = re.compile(r"^\s*reserved\s+(.+?)\s*;")
_RE_IGNORABLE = re.compile(
    r"^\s*(syntax\s*=|package\s+|import\s+|extensions\s+|option\s+\w)")


def _parse_reserved(body: str, msg: ProtoMessage, where: str) -> None:
    """``reserved 5, 7 to 9;`` / ``reserved "old_name";`` —— 删字段后防复用的唯一机制。"""
    for item in _split_top_level(body):
        if item.startswith('"') and item.endswith('"'):
            msg.reserved_names.add(_unquote(item))
            continue
        m = re.fullmatch(r"(\d+)\s+to\s+(\d+)", item)
        if m:
            lo, hi = int(m.group(1)), int(m.group(2))
            if hi < lo:
                raise SchemaProtoError("%s reserved 区间 %s 上下界颠倒" % (where, item))
            msg.reserved_numbers.update(range(lo, hi + 1))
            continue
        if re.fullmatch(r"\d+", item):
            msg.reserved_numbers.add(int(item))
            continue
        raise SchemaProtoError("%s 无法解析 reserved 项:%r" % (where, item))


def _scan_outside_quotes(text: str, needle: str) -> int:
    """返回 *needle* 在引号外的首个位置,没有则 -1。"""
    in_str = False
    i = 0
    n = len(needle)
    while i < len(text):
        ch = text[i]
        if ch == "\\" and in_str:
            i += 2
            continue
        if ch == '"':
            in_str = not in_str
        elif not in_str and text.startswith(needle, i):
            return i
        i += 1
    return -1


def _strip_line_comment(line: str) -> str:
    """去掉引号外的行尾 ``//`` 注释。返回左半部分(不 rstrip)。"""
    pos = _scan_outside_quotes(line, "//")
    return line if pos < 0 else line[:pos]


def _split_top_level(text: str, sep: str = ",") -> list[str]:
    """按 *sep* 切分,跳过引号内部,并正确跳过 ``\\`` 转义。"""
    out: list[str] = []
    buf: list[str] = []
    in_str = False
    i = 0
    while i < len(text):
        ch = text[i]
        if in_str and ch == "\\" and i + 1 < len(text):
            buf.append(ch)
            buf.append(text[i + 1])
            i += 2
            continue
        if ch == '"':
            in_str = not in_str
            buf.append(ch)
        elif ch == sep and not in_str:
            out.append("".join(buf))
            buf = []
        else:
            buf.append(ch)
        i += 1
    out.append("".join(buf))
    return [s.strip() for s in out if s.strip()]


def _unquote(raw: str) -> str:
    """逐字符还原带引号字面量的转义。不能用链式 replace —— 顺序敏感会拆错。"""
    body = raw[1:-1]
    out: list[str] = []
    i = 0
    while i < len(body):
        ch = body[i]
        if ch == "\\" and i + 1 < len(body):
            out.append(body[i + 1])
            i += 2
            continue
        out.append(ch)
        i += 1
    return "".join(out)


def quote(text: str) -> str:
    """与 :func:`_unquote` 严格对称的转义。播种器也用这一份。"""
    return '"%s"' % text.replace("\\", "\\\\").replace('"', '\\"')


def _parse_value(raw: str):
    raw = raw.strip()
    if len(raw) >= 2 and raw.startswith('"') and raw.endswith('"'):
        return _unquote(raw)
    if raw == "true":
        return True
    if raw == "false":
        return False
    if re.fullmatch(r"-?\d+", raw):
        return int(raw)
    return raw            # 枚举名等裸标识符


def _check_option(spec: OptionSpec, value, vocab: Vocabulary, where: str) -> None:
    t = spec.type_name
    if t == "bool":
        if not isinstance(value, bool):
            raise SchemaProtoError("%s (%s) 必须是 true/false,实为 %r(带引号的 \"false\" 是真值)"
                                   % (where, spec.name, value))
    elif t == "string":
        if not isinstance(value, str):
            raise SchemaProtoError("%s (%s) 必须是带引号的字符串,实为 %r" % (where, spec.name, value))
    elif t in ("uint32", "uint64", "int32", "int64"):
        if isinstance(value, bool) or not isinstance(value, int):
            raise SchemaProtoError("%s (%s) 必须是整数字面量(不要加引号),实为 %r"
                                   % (where, spec.name, value))
        if t.startswith("uint") and value < 0:
            raise SchemaProtoError("%s (%s) 不能是负数,实为 %r" % (where, spec.name, value))
    elif t in vocab.enums:
        if value not in vocab.enums[t]:
            raise SchemaProtoError("%s (%s) 的取值 %r 不是 %s 的成员(合法:%s)"
                                   % (where, spec.name, value, t,
                                      "/".join(sorted(vocab.enums[t]))))
    else:
        raise SchemaProtoError("%s (%s) 的词表类型 %s 解析器不认识" % (where, spec.name, t))


def _parse_options(body: Optional[str], specs: dict[str, OptionSpec],
                   vocab: Vocabulary, where: str) -> dict:
    """解析 option 列表。**未知名字、类型不符、非 repeated 重复出现一律报错。**"""
    if not body:
        return {}
    opts: dict = {}
    for item in _split_top_level(body):
        m = _RE_OPT_KV.match(item)
        if not m:
            raise SchemaProtoError("%s 无法解析 option:%r" % (where, item))
        key = m.group(1).split(".")[-1]
        spec = specs.get(key)
        if spec is None:
            raise SchemaProtoError("%s 未知 option (%s);词表里有:%s"
                                   % (where, key, "/".join(sorted(specs))))
        val = _parse_value(item[m.end():])
        _check_option(spec, val, vocab, where)
        if key in opts:
            if not spec.repeated:
                raise SchemaProtoError("%s option (%s) 不是 repeated,却出现了多次" % (where, key))
            opts[key].append(val)
        else:
            opts[key] = [val] if spec.repeated else val
    return opts


def parse_schema_proto(path: Path, vocab: Optional[Vocabulary] = None) -> dict[str, ProtoMessage]:
    """解析一份权威 schema proto,返回 ``{message 名: ProtoMessage}``(保持声明序)。"""
    if vocab is None:
        vocab = load_vocabulary(path.parent / "cfg_options.proto")

    messages: dict[str, ProtoMessage] = {}
    cur: Optional[ProtoMessage] = None
    pending: list[str] = []

    for lineno, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        stripped = raw.strip()
        where = "%s:%d" % (path.name, lineno)

        # 注释:从原始行里取,不能用 stripped —— 注释承载的是 xlsx 第 5 行的原文,
        # 里面的缩进(Buff 那段 C++ enum 就是靠缩进读的)必须逐字符还原。
        if stripped.startswith("//"):
            body = raw[raw.index("//") + 2:]
            pending.append(body[1:] if body.startswith(" ") else body)
            continue

        # 空行等价于一条空注释行,不打断注释块(播种器把空行写成 "//",
        # 两种写法必须等价,否则「空行写成什么」就成了承重约定)。
        if not stripped:
            pending.append("")
            continue

        if "/*" in stripped or "*/" in stripped:
            raise SchemaProtoError("%s 不支持块注释,请改用 // :%r" % (where, stripped))

        m = _RE_MESSAGE.match(raw)
        if m:
            if m.group(1) in messages:
                raise SchemaProtoError("%s message %s 重复定义" % (where, m.group(1)))
            cur = ProtoMessage(name=m.group(1))
            messages[cur.name] = cur
            pending.clear()
            continue

        if stripped.startswith("}"):
            cur = None
            pending.clear()
            continue

        if cur is None:
            # 文件头的 syntax / package / import 等,顺带把文件头注释丢掉。
            if not _RE_IGNORABLE.match(raw):
                raise SchemaProtoError("%s message 之外出现无法解析的行:%r" % (where, stripped))
            pending.clear()
            continue

        line = _strip_line_comment(raw)
        tail = raw[len(line):].lstrip()
        tail_comment = tail[2:].lstrip() if tail.startswith("//") else ""

        m = _RE_MSG_OPTION.match(line)
        if m:
            key = m.group(1).split(".")[-1]
            spec = vocab.message_options.get(key)
            if spec is None:
                raise SchemaProtoError("%s 未知表级 option (%s);词表里有:%s"
                                       % (where, key, "/".join(sorted(vocab.message_options))))
            val = _parse_value(m.group(2))
            _check_option(spec, val, vocab, where)
            if key in cur.opts:
                raise SchemaProtoError("%s 表级 option (%s) 重复" % (where, key))
            cur.opts[key] = val
            pending.clear()
            continue

        m = _RE_RESERVED.match(line)
        if m:
            _parse_reserved(m.group(1), cur, where)
            pending.clear()
            continue

        m = _RE_FIELD.match(line)
        if m:
            opts = _parse_options(m.group(5), vocab.field_options, vocab, where)
            comment = "\n".join(pending).strip() or tail_comment
            cur.fields.append(ProtoField(
                name=m.group(3),
                number=int(m.group(4)),
                type_expr=re.sub(r"\s+", " ", m.group(2).strip()),
                repeated=bool(m.group(1)),
                opts=opts,
                comment=comment,
                lineno=lineno,
            ))
            pending.clear()
            continue

        if _RE_IGNORABLE.match(line):
            pending.clear()
            continue

        raise SchemaProtoError("%s 无法解析的行:%r" % (where, stripped))

    if not messages:
        raise SchemaProtoError("%s 里没有任何 message" % path)

    for msg in messages.values():
        _check_message_wellformed(msg, path)
    return messages


def _check_message_wellformed(msg: ProtoMessage, path: Path) -> None:
    names: dict[str, int] = {}
    numbers: dict[int, str] = {}
    for f in msg.fields:
        if f.name in names:
            raise SchemaProtoError("%s:%d message %s 的字段名 %s 重复(第 %d 行已出现)"
                                   % (path.name, f.lineno, msg.name, f.name, names[f.name]))
        names[f.name] = f.lineno
        if f.number in numbers:
            raise SchemaProtoError("%s:%d message %s 的字段号 %d 重复(已被 %s 占用)"
                                   % (path.name, f.lineno, msg.name, f.number, numbers[f.number]))
        numbers[f.number] = f.name
        if not 1 <= f.number <= _PROTO_MAX_FIELD_NUMBER:
            raise SchemaProtoError("%s:%d 字段 %s 的号 %d 越界"
                                   % (path.name, f.lineno, f.name, f.number))
        if _PROTO_RESERVED_RANGE[0] <= f.number <= _PROTO_RESERVED_RANGE[1]:
            raise SchemaProtoError("%s:%d 字段 %s 的号 %d 落在 protobuf 保留段 %d-%d"
                                   % (path.name, f.lineno, f.name, f.number,
                                      *_PROTO_RESERVED_RANGE))
        # reserved 是删字段之后防号/名被复用的唯一机制。复用一个退休号 = 让所有
        # 存量 .pb 里那一段字节被当成新字段解读,而且不会有任何报错。
        if f.number in msg.reserved_numbers:
            raise SchemaProtoError("%s:%d 字段 %s 用了已 reserved 的号 %d"
                                   % (path.name, f.lineno, f.name, f.number))
        if f.name in msg.reserved_names:
            raise SchemaProtoError("%s:%d 字段名 %s 已被 reserved"
                                   % (path.name, f.lineno, f.name))


# ---------------------------------------------------------------------------
# 类型解析
# ---------------------------------------------------------------------------

_RE_MAP_TYPE = re.compile(r"^map\s*<\s*([\w.]+)\s*,\s*([\w.]+)\s*>$")

SCALAR = "CFG_SHAPE_SCALAR"
LIST = "CFG_SHAPE_LIST"
SET = "CFG_SHAPE_SET"
MAP = "CFG_SHAPE_MAP"
STRUCT_LIST = "CFG_SHAPE_STRUCT_LIST"


def _map_types(type_expr: str) -> Optional[tuple[str, str]]:
    m = _RE_MAP_TYPE.match(type_expr)
    return (m.group(1), m.group(2)) if m else None


def _infer_shape(f: ProtoField, element_names: set[str]) -> str:
    """推断物理形态。**认不出来一律报错,绝不回落成 LIST。**"""
    declared = f.opts.get("cfg_shape")
    kv = _map_types(f.type_expr)
    if declared:
        if declared in (SCALAR, LIST) and kv:
            raise SchemaProtoError("字段 %s 声明为 %s,proto 类型却是 %s"
                                   % (f.name, declared, f.type_expr))
        if declared in (MAP, SET) and not kv:
            raise SchemaProtoError("字段 %s 声明为 %s,proto 类型必须是 map<K, V>,实为 %s"
                                   % (f.name, declared, f.type_expr))
        if declared == STRUCT_LIST and f.type_expr not in element_names:
            raise SchemaProtoError("字段 %s 声明为 STRUCT_LIST,但找不到子消息 %s"
                                   % (f.name, f.type_expr))
        return declared
    if kv:
        # map<K, bool> 既可能是 MAP 也可能是 SET,必须显式表态,不许猜。
        if kv[1] == "bool":
            raise SchemaProtoError(
                "字段 %s 的类型是 map<%s, bool>,MAP 与 SET 都长这样,必须显式写 (cfg_shape)"
                % (f.name, kv[0]))
        return MAP
    if f.repeated:
        if f.type_expr in element_names:
            return STRUCT_LIST
        if f.type_expr in _SCALAR_TYPES:
            return LIST
        raise SchemaProtoError(
            "字段 %s 是 repeated %s,但 %s 既不是内置标量也不是本文件里的子消息"
            % (f.name, f.type_expr, f.type_expr))
    if f.type_expr not in _SCALAR_TYPES:
        raise SchemaProtoError("字段 %s 的类型 %s 不是内置标量" % (f.name, f.type_expr))
    return SCALAR


# ---------------------------------------------------------------------------
# options token 还原
# ---------------------------------------------------------------------------

_BOOL_TOKENS = (
    ("cfg_bit_index", "bit_index"),
    ("cfg_multi", "multi"),
    ("cfg_key", "key"),
    ("cfg_index", "idx"),
    ("cfg_tip_ref", "tip_ref"),
)
_VALUE_TOKENS = (
    ("cfg_composite", "composite:%s"),
    ("cfg_fk", "fk:%s"),
    ("cfg_gfk", "gfk:%s"),
    ("cfg_expr_type", "expr:%s"),
)


def _tokens(f: ProtoField) -> list[str]:
    """把 ``cfg_*`` option 还原成旧的 options token 串。

    顺序固定。ColumnDef 上那些属性都是按 token 前缀取值的,与顺序无关;
    固定顺序只是为了让对拍与 diff 稳定。
    """
    o = f.opts
    toks = [tok for key, tok in _BOOL_TOKENS if o.get(key)]
    toks += [fmt % o[key] for key, fmt in _VALUE_TOKENS if o.get(key)]
    params = o.get("cfg_expr_param")
    if params:
        toks.append("expr_params:%s" % ",".join(str(p) for p in params))
    return toks


# ---------------------------------------------------------------------------
# 字段号校验:与 templates/proto_table.proto.j2 的发号规则对齐
# ---------------------------------------------------------------------------

def _check_field_numbers(main: ProtoMessage, schema: TableSchema, proto_name: str,
                         check_positional: bool) -> dict[str, int]:
    """校验权威 schema 写的字段号,并返回产物要用的号表 ``{字段名: 号}``。

    **恒查的结构不变式**(号一旦成为权威,这些就是它的全部约束):
      - 会进产物的字段必须声明,且号 < 9000、互不相同(唯一性在解析期已查);
      - 不进产物的列必须用 >= 9000 的号 —— 这段号永远不上 wire。

    ``check_positional`` 是**迁移期**的额外断言:号必须恰好等于历史发号规则算出来的值。
    播种与对拍时打开它,证明这批 schema 是忠实固化了现状;日常导表关掉它 ——
    否则「在表中间插一列」又会因为历史规则位移而报错,那正是这次改造要消灭的东西。
    """
    exported = positional_field_numbers(schema)
    numbers: dict[str, int] = {}
    problems: list[str] = []
    for f in main.fields:
        want = exported.get(f.name)
        if want is None:
            if f.number < EXCLUDED_FIELD_NUMBER_BASE:
                problems.append(
                    "字段 %s 不进产物(owner=%s),号 %d 却在正常段;不进产物的列请用 >= %d 的号"
                    % (f.name, f.opts.get("cfg_owner", "common"), f.number,
                       EXCLUDED_FIELD_NUMBER_BASE))
            continue
        if f.number >= EXCLUDED_FIELD_NUMBER_BASE:
            problems.append("字段 %s 会进产物,却拿了 >= %d 的号 %d"
                            % (f.name, EXCLUDED_FIELD_NUMBER_BASE, f.number))
        elif check_positional and f.number != want:
            problems.append("字段 %s 的号是 %d,但按历史发号规则应为 %d"
                            % (f.name, f.number, want))
        numbers[f.name] = f.number
    for name in exported:
        if name not in numbers:
            problems.append("产物里会有字段 %s,但权威 schema 没声明它" % name)
    if problems:
        raise SchemaProtoError("%s 的字段号有问题:\n  - %s"
                               % (proto_name, "\n  - ".join(problems)))
    return numbers


# ---------------------------------------------------------------------------
# 主入口
# ---------------------------------------------------------------------------

def schema_proto_path(schema_dir: Path, sheet: str) -> Path:
    return schema_dir / ("%s_table.proto" % sheet.lower())


def _header_spans(ws, max_col: int) -> list[tuple[int, int, str]]:
    """xlsx 第 1 行 -> ``[(起列, 止列(不含), 名字)]``。与 excel_reader 的口径一致。"""
    starts: list[tuple[int, str]] = []
    for i in range(max_col):
        v = ws.cell(row=1, column=i + 1).value
        text = str(v).strip() if v is not None else ""
        if text:
            starts.append((i, text))
    spans: list[tuple[int, int, str]] = []
    for k, (s, name) in enumerate(starts):
        end = starts[k + 1][0] if k + 1 < len(starts) else max_col
        spans.append((s, end, name))
    return spans


def _element_subfields(msg: ProtoMessage, field_name: str) -> list[tuple[str, str]]:
    """子消息 -> ``[(类型, 裸子字段名)]``,**按字段号取序,不按声明行序**。

    产物模板用 ``loop.index`` 按位置发号,所以子消息的号必须恰好是 1..N。
    钉死这条之后,「在 proto 里对调两行」这种公认无害的操作不会再静默转置数据 ——
    它会直接撞断言;真要改顺序就得同时改号,那是 diff 里看得见的显式改动。
    """
    fields = sorted(msg.fields, key=lambda f: f.number)
    want = list(range(1, len(fields) + 1))
    if [f.number for f in fields] != want:
        raise SchemaProtoError("子消息 %s 的字段号必须是 1..%d 的连续序列,实为 %s"
                               % (msg.name, len(fields), [f.number for f in fields]))
    prefix = field_name + "_"
    out: list[tuple[str, str]] = []
    for sf in fields:
        explicit = sf.opts.get("cfg_col")
        if explicit:
            bare = explicit
        elif sf.name.startswith(prefix):
            bare = sf.name[len(prefix):]
        else:
            raise SchemaProtoError(
                "子消息 %s 的字段 %s 既不以 %r 开头,也没写 (cfg_col) 说明它对应哪个子列"
                % (msg.name, sf.name, prefix))
        out.append((sf.type_expr, bare))
    if not out:
        raise SchemaProtoError("子消息 %s 一个字段都没有" % msg.name)
    return out


def build_table_schema(proto_path: Path, xlsx_path: Path,
                       check_positional: bool = False) -> TableSchema:
    """从权威 schema proto + 源表第 1 行,构造与旧路等价的 :class:`TableSchema`。

    *check_positional* 见 :func:`_check_field_numbers`:迁移期的对拍与播种打开它,
    日常导表关掉。
    """
    messages = parse_schema_proto(proto_path)

    mains = [m for m in messages.values() if "cfg_sheet" in m.opts]
    if not mains:
        raise SchemaProtoError("%s 里没有带 (cfg_sheet) 的 message" % proto_path.name)
    if len(mains) > 1:
        raise SchemaProtoError("%s 里有 %d 个带 (cfg_sheet) 的 message:%s"
                               % (proto_path.name, len(mains),
                                  ", ".join(m.name for m in mains)))
    main = mains[0]
    sheet = main.opts["cfg_sheet"]
    element_msgs = {n: m for n, m in messages.items() if m is not main}

    # 不用 read_only:被 openpyxl 重写过的工作簿在只读模式下 max_column 可能拿不到
    # (实测 ActorActionCombatState / MessageLimiter 等 4 张),而列数必须与
    # excel_reader.read_table 的口径逐格一致,否则 span 推导会静默错位。
    wb = openpyxl.load_workbook(xlsx_path)
    try:
        if sheet not in wb.sheetnames:
            raise SchemaProtoError(
                "%s 声明 (cfg_sheet)=%r,但 %s 里没有这个 sheet(实有:%s)"
                % (proto_path.name, sheet, xlsx_path.name, ", ".join(wb.sheetnames)))
        ws = wb[sheet]
        spans = _header_spans(ws, ws.max_column or 0)
    finally:
        wb.close()

    # ---- 按列名绑定(不按声明序) ----
    by_header: dict[str, tuple[int, int]] = {}
    for s, e, name in spans:
        if name in by_header:
            raise SchemaProtoError("%s 第 1 行有重复表头名 %r" % (xlsx_path.name, name))
        by_header[name] = (s, e)

    claimed: dict[int, ProtoField] = {}
    errors: list[str] = []
    for f in main.fields:
        header = f.opts.get("cfg_col") or f.name
        if header not in by_header:
            errors.append("字段 %s 期望表头 %r,%s 第 1 行没有这一列"
                          % (f.name, header, xlsx_path.name))
            continue
        start = by_header[header][0]
        if start in claimed:
            errors.append("表头 %r 被字段 %s 与 %s 同时认领"
                          % (header, claimed[start].name, f.name))
            continue
        claimed[start] = f
    for s, _e, name in spans:
        if s not in claimed:
            errors.append("表头 %r(第 %d 列)在 %s 里没有对应字段"
                          % (name, s + 1, proto_path.name))
    if errors:
        raise SchemaProtoError("%s <-> %s 不匹配:\n  - %s"
                               % (proto_path.name, xlsx_path.name, "\n  - ".join(errors)))

    # ---- 逐 span 展开成物理列(顺序即列序,下游 read_data_rows 按下标取名) ----
    columns: list[ColumnDef] = []
    for start, end, _name in spans:
        f = claimed[start]
        span = end - start
        shape = _infer_shape(f, set(element_msgs))
        owner = str(f.opts.get("cfg_owner", "common")).lower()
        if owner not in _KNOWN_OWNERS:
            raise SchemaProtoError("字段 %s 的 cfg_owner=%r 不是已知取值(%s)"
                                   % (f.name, owner,
                                      "/".join(sorted(o for o in _KNOWN_OWNERS if o))))
        if owner == _MISSING_OWNER:
            logger.warning(
                "[%s] 字段 %s 的 owner 是空的 -> 整列被丢弃,不进 proto/JSON。"
                "源表里如果这一列有数据,那些数据现在就在丢。", sheet, f.name)
        toks = _tokens(f)
        cmt = f.comment
        slots = f.opts.get("cfg_slots")

        def _need(expected: int, unit: str, _f=f, _span=span, _slots=slots,
                  _shape=shape) -> None:
            if _slots is None:
                raise SchemaProtoError("字段 %s 是 %s,必须写 (cfg_slots)" % (_f.name, _shape))
            if expected != _span:
                raise SchemaProtoError(
                    "字段 %s 的 (cfg_slots)=%d 换算成 %d 列,但 %s 第 1 行给了 %d 列(%s)"
                    % (_f.name, _slots, expected, xlsx_path.name, _span, unit))

        if shape == SCALAR:
            if span != 1:
                raise SchemaProtoError(
                    "字段 %s 声明为 SCALAR 却占了 %d 列;定长数组请写 (cfg_shape)=CFG_SHAPE_LIST"
                    % (f.name, span))
            columns.append(ColumnDef(name=f.name, data_type=f.type_expr, owner=owner,
                                     struct="", options=toks, comment=cmt,
                                     excel_index=start))

        elif shape in (LIST, SET):
            _need(slots or 0, "每槽 1 列")
            if shape == SET:
                kv = _map_types(f.type_expr)
                if kv[1] != "bool":
                    raise SchemaProtoError(
                        "字段 %s 是 SET,proto 类型必须是 map<T, bool>,实为 %s"
                        % (f.name, f.type_expr))
                elem, struct = kv[0], "set"
            else:
                elem, struct = f.type_expr, "repeated"
            for i in range(span):
                columns.append(ColumnDef(
                    name=f.name, data_type=elem, owner=owner, struct=struct,
                    options=toks if i == 0 else [], comment=cmt if i == 0 else "",
                    excel_index=start + i))

        elif shape == MAP:
            kv = _map_types(f.type_expr)
            _need((slots or 0) * 2, "每对 2 列")
            for i in range(0, span, 2):
                columns.append(ColumnDef(
                    name="%s_key" % f.name, data_type=kv[0], owner=owner, struct="map_key",
                    options=toks if i == 0 else [], comment=cmt if i == 0 else "",
                    excel_index=start + i))
                columns.append(ColumnDef(
                    name="%s_value" % f.name, data_type=kv[1], owner=owner,
                    struct="map_value", options=[], comment="",
                    excel_index=start + i + 1))

        elif shape == STRUCT_LIST:
            subs = _element_subfields(element_msgs[f.type_expr], f.name)
            _need((slots or 0) * len(subs), "每条 %d 列" % len(subs))
            for i in range(span):
                sf_type, sf_name = subs[i % len(subs)]
                columns.append(ColumnDef(
                    name="%s_%s" % (f.name, sf_name), data_type=sf_type, owner=owner,
                    struct="message:%s" % f.name,
                    options=toks if i == 0 else [], comment=cmt if i == 0 else "",
                    excel_index=start + i))

        else:                                        # pragma: no cover
            raise SchemaProtoError("字段 %s 的形态 %r 无法处理" % (f.name, shape))

    # 旧路的硬闸门:read_table 要求 A1 字面等于 'id'。新路把它做成显式声明,
    # 并且是旧闸门的超集 —— 主键是谁、在不在第一列,都写出来、都校验。
    primary = main.opts.get("cfg_primary_key", "id")
    if not columns or columns[0].name != primary:
        raise SchemaProtoError("%s 声明 (cfg_primary_key)=%r,但 %s 的第一列是 %r"
                               % (proto_path.name, primary, xlsx_path.name,
                                  columns[0].name if columns else "(无列)"))

    arrays, groups, maps = _detect_layout(columns)
    constants_idx = next((c.excel_index for c in columns if c.name == "constants_name"), None)
    first = columns[0] if columns else None

    schema = TableSchema(
        name=sheet,
        source_path=xlsx_path,
        columns=columns,
        arrays=arrays,
        groups=groups,
        maps=maps,
        # 主键可重复只有一个来源:主键列上的 (cfg_multi)。不设第二个表级开关,
        # 否则两处说法不一致时没有正确答案。
        multi_primary_key=first is not None and first.is_multi_key,
        has_constants_name=constants_idx is not None,
        constants_name_index=constants_idx,
    )
    # 位序是「ID -> 位下标」,主键可重复时这个映射没有定义。
    # 不靠自觉,直接互斥。
    if schema.multi_primary_key and schema.bit_index_columns:
        raise SchemaProtoError(
            "%s 的主键声明了 (cfg_multi)(id 可重复),就不能再有 (cfg_bit_index):"
            "位序是 ID 到存档位图下标的映射,重复 ID 没有定义。冲突的列:%s"
            % (proto_path.name, ", ".join(c.name for c in schema.bit_index_columns)))

    schema.field_numbers = _check_field_numbers(main, schema, proto_path.name,
                                                check_positional)
    return schema

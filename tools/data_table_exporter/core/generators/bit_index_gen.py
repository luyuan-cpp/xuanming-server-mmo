"""位序(bit_index)生成器。

对带 ``bit_index`` 标记列的表,维护一份 ID → 位序映射(持久化在
``state/mapping/table_index_mapping/`` 下),并据此生成 C++ / Go 常量。

**位序是玩家存档位图的下标口径**(消费方见 ``cpp/libs/modules/mission/comp/mission_comp.h``
的 ``std::bitset<kMissionBitmapCapacity>``、``reward_comp.h`` 的 ``RewardBitset``)。
一旦发布,这个下标就写进了所有存量玩家的档案,因此有两条不可协商的红线:

1. **位序只增不改、永不复用**。删一张表里的某个 ID 再加一个新 ID,若新 ID 捡走
   旧 ID 腾出的空位,所有存量玩家这一位的含义会静默改变——已完成旧任务的玩家会
   凭空"完成"新任务、已领过旧奖励的玩家会被判定为已领新奖励。所以退休的 ID
   **在 mapping json 里保留原条目当占位**,新 ID 一律追加到当前最大位之后。
2. **状态文件不可静默重建**。mapping json 缺失 / 解析失败时若退化成空表重新分配,
   整张表的位序会整体重排,后果同上但波及全表。所以读取一律 fail-closed:
   文件损坏 → 抛异常中止导表;文件缺失 → 中止并提示从 git 恢复;
   只有显式传 ``--bitindex-bootstrap``(确认该表从未发布过)才允许从空初始化。
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

from jinja2 import Environment, FileSystemLoader

from core.config_loader import ExporterConfig
from core.excel_reader import read_id_column
from core.file_utils import ensure_dirs, write_file
from core.schema import TableSchema

logger = logging.getLogger(__name__)


class BitIndexStateError(RuntimeError):
    """位序状态文件不可用。

    这个异常**必须**冒泡到 orchestrator 让整次导表失败:位序状态丢了还继续导,
    产出的位图口径和存量玩家档案对不上,属于静默数据损坏。
    """


def generate_bit_indexes(cfg: ExporterConfig, tables: list[TableSchema]) -> None:
    """Generate bit-index mapping files for all applicable tables."""
    # keep_trailing_newline:Jinja 默认吃掉模板末尾换行,产出的 .go / .h 会缺行尾换行
    # (gofmt 与多数工具都视为脏)。位序模板的排版靠 `{%-` 空白控制,不再走 _clean_output。
    env = Environment(
        loader=FileSystemLoader(str(cfg.template_dir), encoding="utf-8"),
        keep_trailing_newline=True,
    )
    # ljust:Jinja 内置过滤器里**没有** ljust(只有 center/indent),go_bit_index.go.j2 却在用它
    # 做常量名补齐。缺这一行整个 Go 位序生成会直接抛 TemplateAssertionError: No filter named 'ljust'
    # 把导表管线打断在 generate_bit_indexes 这一步(C++ 已经写完、Go 一个字节没写),
    # 而 go/shared/generated/bit_index/ 下留着上一版的陈旧产物,看起来一切正常。
    env.filters["ljust"] = lambda value, width: str(value).ljust(int(width))

    mapping_dir = cfg.state_dir / "mapping" / "table_index_mapping"
    ensure_dirs(mapping_dir)

    for schema in tables:
        if not schema.bit_index_columns:
            continue

        mapping_file = mapping_dir / f"{schema.name.lower()}_mapping.json"
        id_to_index = _load_mapping(mapping_file, schema.name, cfg.bit_index_bootstrap)
        ids = read_id_column(schema, cfg)
        id_to_index = _update_mapping(schema.name, ids, id_to_index)
        # 统一按位序升序排一遍:状态文件和 C++/Go 产物用同一个顺序,产出与手改过顺序的
        # 状态文件无关,diff 才稳定。
        id_to_index = dict(sorted(id_to_index.items(), key=lambda kv: kv[1]))
        _save_mapping(mapping_file, id_to_index)

        bitmap_capacity = _bitmap_capacity(id_to_index)

        if cfg.cpp.enabled:
            _gen_cpp(schema.name, id_to_index, bitmap_capacity, env, cfg)
        if cfg.go.enabled:
            _gen_go(schema.name, id_to_index, bitmap_capacity, env, cfg)


# ---------------------------------------------------------------------------
# Mapping persistence
# ---------------------------------------------------------------------------

def _load_mapping(path: Path, table_name: str, bootstrap: bool) -> dict[int, int]:
    """读取位序状态文件,**fail-closed**。

    这里以前是 ``except Exception: pass`` 然后返回 ``{}``:文件被截断 / 编码坏了 /
    只是临时没 checkout 出来,都会被当成"这张表还没分配过位序",于是整表重排。
    现在任何异常都升级成 :class:`BitIndexStateError`。
    """
    if path.exists():
        try:
            with open(path, "r", encoding="utf-8") as f:
                raw = json.load(f)
        except Exception as exc:
            raise BitIndexStateError(
                f"[{table_name}] 位序状态文件解析失败:{path}({exc})。"
                f"位序是存量玩家位图的下标口径,不能靠重建糊过去——请从 git 恢复该文件后重试。"
            ) from exc

        if not isinstance(raw, dict):
            raise BitIndexStateError(
                f"[{table_name}] 位序状态文件格式错误(顶层不是 object):{path}"
            )

        mapping: dict[int, int] = {}
        for key, value in raw.items():
            try:
                row_id, index = int(key), int(value)
            except (TypeError, ValueError) as exc:
                raise BitIndexStateError(
                    f"[{table_name}] 位序状态文件含非整数条目 {key!r}: {value!r}:{path}"
                ) from exc
            if index < 0:
                raise BitIndexStateError(
                    f"[{table_name}] 位序状态文件含负位序 {key}={value}:{path}"
                )
            if row_id in mapping:
                raise BitIndexStateError(
                    f"[{table_name}] 位序状态文件含重复 ID {row_id}:{path}"
                )
            mapping[row_id] = index

        used = list(mapping.values())
        if len(set(used)) != len(used):
            raise BitIndexStateError(
                f"[{table_name}] 位序状态文件里有两个 ID 占同一位(位序已被复用过):{path}。"
                f"这会让存量玩家位图串味,必须人工确认哪一个 ID 是后加的、把它改到最大位之后。"
            )
        return mapping

    if bootstrap:
        logger.warning(
            "[%s] Bit-index state file is missing; initializing from empty with --bitindex-bootstrap: %s. This is allowed only if the table has never been released; otherwise all existing player bitmaps will be misaligned.",
            table_name, path,
        )
        return {}

    raise BitIndexStateError(
        f"[{table_name}] 位序状态文件缺失:{path}。"
        f"该文件是位序的唯一权威,请先从 git 恢复;"
        f"确认这张表从未发布过(没有任何存量玩家位图依赖它)才可以加 --bitindex-bootstrap 从空初始化。"
    )


def _save_mapping(path: Path, mapping: dict[int, int]) -> None:
    """写回位序状态文件。

    - 按位序升序落盘:分配是只增的,所以这个顺序等于历史分配顺序,产出稳定、diff 干净。
    - ``newline="\\n"``:默认文本模式在 Windows 上会把 ``\\n`` 翻成 ``\\r\\n``,
      而仓库里这些 json 是 LF,导致每次在 Windows 上导表都整文件"被修改"。
    """
    ordered: dict[str, int] = {
        str(row_id): index for row_id, index in sorted(mapping.items(), key=lambda kv: kv[1])
    }
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(ordered, f, indent=4)


def _update_mapping(table_name: str, ids: list[int], existing: dict[int, int]) -> dict[int, int]:
    """给新 ID 追加位序;**绝不复用**已退休 ID 腾出的空位。

    以前这里会把 ``set(range(next_idx)) - used`` 的空位排队分给新 ID(docstring 原文
    "reusing gaps from removed IDs")。位序是存量玩家位图的下标,复用 = 静默改写
    所有玩家这一位的含义,是 P0 级数据事故。现在一律 ``max(已分配)+1`` 往后追加。
    """
    next_idx = max(existing.values(), default=-1) + 1

    added: list[int] = []
    for row_id in ids:
        if row_id in existing:
            continue
        existing[row_id] = next_idx
        added.append(row_id)
        next_idx += 1

    if added:
        logger.info("[%s] Added %d bit indices: %s", table_name, len(added),
                    ", ".join(f"{i}->{existing[i]}" for i in added))

    # 退休 ID:表里已经没有这一行,但它的位序**永久保留**当占位,不回收也不复用。
    retired = sorted(set(existing) - set(ids))
    if retired:
        logger.warning(
            "[%s] %d IDs have been removed from the table; their bit indices remain permanently reserved and will never be reclaimed: %s",
            table_name, len(retired),
            ", ".join(f"{i}(bit {existing[i]})" for i in retired),
        )
    return existing


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _bitmap_capacity(mapping: dict[int, int]) -> int:
    """位图容量 = 已分配的最大位 + 1。

    位图按位序下标(消费方一律经 ``<Sheet>BitMap`` 把 ID 换成位序),与 ID 的数值大小无关。2026-11-20 之前
    取的是 ``max(表里最大 ID, 最大位 + 1)``:ID 稀疏或很大(例如 100001 起编)时,每个玩家内存里的
    ``std::bitset`` 会按最大 ID 开到几十 KB,实际只用到前几位。退休 ID 的位仍在 mapping 里占位,
    所以这样算出的容量也覆盖它们。
    """
    return max(mapping.values(), default=-1) + 1


# ---------------------------------------------------------------------------
# Code generation
# ---------------------------------------------------------------------------

def _gen_cpp(name, mapping, bitmap_capacity, env, cfg):
    ensure_dirs(cfg.cpp.bit_index_dir)
    tpl = env.get_template("cpp_bit_index.h.j2")
    content = tpl.render(sheet=name, id_to_index=mapping, bitmap_capacity=bitmap_capacity)
    write_file(cfg.cpp.bit_index_dir / f"{name.lower()}_table_id_bit_index.h", content)
    logger.info("Generated C++ bit_index: %s", name)


def _gen_go(name, mapping, bitmap_capacity, env, cfg):
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
        sheet=name, id_to_index=mapping, bitmap_capacity=bitmap_capacity, name_width=name_width
    )
    write_file(out_dir / f"{pkg}_table_id_bit_index.go", content)
    logger.info("Generated Go bit_index: %s -> %s", name, out_dir)

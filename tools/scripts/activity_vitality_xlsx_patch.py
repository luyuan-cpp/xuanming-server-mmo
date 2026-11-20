#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""活跃度领取「本期已结束」提示码(docs/design/player-features-ui.md「领取身份、提交与重启恢复」)要对 Tip.xlsx 做的改动,
写成可重复执行的脚本。

写法与理由同 equip_xlsx_patch.py 的 forge-tip-codes(这里只留结论):
  - data/tip/Tip.xlsx 多个会话同时在写,它是二进制、不能三方合并:从磁盘重新载入 → 只往空格子里写 → save →
    读回来逐格核对,再按导表器的口径逐组核对一遍。
  - 新码追加在既有 //reward_error 组的组尾;组后面还有别的组时在组尾下一行插入,其后各行整体下移。

幂等:已经是目标状态就只报告、不改文件;发现与预期不符的现状就中止,绝不覆盖。

用法(仓库根目录执行;需要 Python 3 + openpyxl + pyyaml,与导表器同一套依赖):

    python tools/scripts/activity_vitality_xlsx_patch.py tip-codes

加 --dry-run 只打印将要做什么,不写文件。通用的 xlsx 读写与核对函数复用 equip_xlsx_patch.py(同目录,作为模块导入)。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    import openpyxl
    from equip_xlsx_patch import (
        HEADER_ROW,
        TIP_STATE,
        TIP_XLSX,
        Patch,
        apply_patches_and_verify,
        fail,
        only_sheet,
        predict_tip_ids,
        rel,
        sheet_matrix,
        tip_parse,
    )
except ImportError as exc:  # pragma: no cover - 环境问题,给一句能照着做的话
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first, and make sure equip_xlsx_patch.py is in the same directory")


# 既有的奖励域。码名不带 k 前缀:导表器生成枚举时自己加(RewardPeriodEnded -> kRewardPeriodEnded)。
TIP_GROUP = "reward_error"
TIP_BASE = 12000
TIP_WIDTH = 1000
# 业务拒绝,不是服务端故障(fault 留空):客户端收到后清掉本机的待确认领取并刷新活动与背包。
# 文案同时覆盖两种情况:跨日边界上发出的领取,以及原领取记录被裁掉之后的重放(当初领没领到无法判断)。
TIP_CODES = [
    # 码名                  文案                                              fault
    ("RewardPeriodEnded", "本期活跃已结束，不能再领取；已领到的奖励以背包为准", False),
]


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} row 1 should be name / commen / ... / fault (exactly one fault column), got {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    group_text = f"//{TIP_GROUP}"
    wanted = [(name, text, 1 if fault else None) for name, text, fault in TIP_CODES]
    new_names = [name for name, _t, _f in TIP_CODES]

    mine = next((g for g in groups if g["name"] == TIP_GROUP), None)
    if mine is None:
        fail(f"{TIP_XLSX.name} has no group {group_text} -- this script only appends to the existing reward group; find out who removed it first.")
    if (mine["base"], mine["width"]) != (TIP_BASE, TIP_WIDTH):
        fail(f"{TIP_XLSX.name} row {mine['row']}: group {group_text} is declared as base={mine['base']} width={mine['width']}, "
             f"expected base={TIP_BASE} width={TIP_WIDTH} -- find out who changed it first.")
    present = [code for code in mine["codes"] if code[0] in new_names]
    if present == wanted:
        print(f"[ALREADY CURRENT] group {group_text} (row {mine['row']}) already has {new_names} with the same text / fault; file unchanged.")
        return
    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{n} (in {owner[n]})" for n in new_names if n in owner]
    if clash:
        fail(f"these code names are already in the table but not in the expected state: {clash} -- "
             f"code names are globally unique; maybe the text was changed by hand; find out first.")

    names_after = [n for n, _t, _f in mine["codes"]] + new_names
    state_ids = {}
    if TIP_STATE.exists():
        # 只读核对,不写 state:已发的号按名保留,新名字取本段最小空位。
        state_ids = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {}).get(TIP_GROUP, {}).get("ids", {})
    predicted = predict_tip_ids(names_after, TIP_BASE, state_ids)[-len(new_names):]
    if any(not TIP_BASE <= code_id < TIP_BASE + TIP_WIDTH for code_id in predicted):
        fail(f"group {group_text} has no free id left in [{TIP_BASE},{TIP_BASE + TIP_WIDTH}) for {new_names} -- find out first.")

    if not mine["code_rows"]:
        fail(f"group {group_text} has no codes; this script expects at least the existing RewardAlreadyClaimed -- find out first.")
    last_code_row = mine["code_rows"][-1]
    insert_at = last_code_row + 1
    trailing = [r for r in range(insert_at, len(before) + 1) if any(v is not None for v in before[r - 1])]
    if trailing:
        how = f"there is content from row {trailing[0]} on, so {len(wanted)} row(s) will be inserted before row {insert_at} (later rows shift down unchanged)"
    else:
        how = f"everything after it is empty, so row(s) {insert_at}..{insert_at + len(wanted) - 1} will be written directly"
    print(f"{TIP_XLSX.name}: group {group_text} has {len(mine['codes'])} code(s), its last code is on row {last_code_row}; {how}:")
    patches: list[Patch] = []
    for offset, (name, text, fault) in enumerate(wanted):
        row_idx = insert_at + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
        print(f"  row {row_idx}: {name} | {text}" + (" | fault=1" if fault else "") + f"  (expected id {predicted[offset]})")
    if dry_run:
        print("[dry-run] No files written.")
        return

    shifted = before
    if trailing:
        ws.insert_rows(insert_at, amount=len(wanted))
        # 预期矩阵由原矩阵纯列表运算得到,读回核对同时验证了「插入点之后的每一格都原样下移」。
        blank = [None] * max(len(r) for r in before)
        shifted = before[:insert_at - 1] + [list(blank) for _ in wanted] + before[insert_at - 1:]
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, shifted, patches, bind_schema=False)

    # 再按导表器的口径(组 / 码 / fault)核一遍:fault 标记若与码名错了行,就是静默改了故障分类。
    check_ws = only_sheet(openpyxl.load_workbook(TIP_XLSX), TIP_XLSX)
    groups_after, notes_after = tip_parse(sheet_matrix(check_ws), fault_col)
    expected_groups = [(g["name"], g["base"], g["width"], g["codes"] + wanted if g is mine else g["codes"]) for g in groups]
    if [(g["name"], g["base"], g["width"], g["codes"]) for g in groups_after] != expected_groups \
            or [text for _row, text in notes_after] != [text for _row, text in notes]:
        fail(f"after saving, the codes per group are not as expected -- restore it now with git checkout -- {rel(TIP_XLSX)}")
    print(f"[DONE] {group_text} {len(mine['codes'])} -> {len(mine['codes']) + len(wanted)} codes; the other {len(groups) - 1} groups are unchanged. "
          f"After exporting, check that {new_names} got ids {predicted}; the same domain needs no project file registration.")


COMMANDS = {
    "tip-codes": cmd_tip_codes,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true", help="只打印将要做什么,不写文件")
    args = parser.parse_args()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    # Windows 默认控制台编码打不出部分字符时会在 print 上抛 UnicodeEncodeError,而那时文件可能已经写完 ——
    # 统一成 utf-8,免得"改成功了却以非 0 退出"误导人。
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()

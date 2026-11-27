#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""悬赏求书迁 trade 托管合同(数据覆写审计 r2 第 4 轮 r2-craft#1,docs/design/skill-book-wanted-order-trade-escrow.md)的配表补丁。

两件事,各自幂等:
1. Tip.xlsx:在 //trade_error 段末尾追加两条码(排在本段最后,已有码号不变),写法同 craft_bookshelf_cap_xlsx_patch.py:
   从磁盘重新载入 → 码名全表唯一才写 → 在下一个段头之前插行 → 保存 → 读回核对。两条都在就只报告、不改文件。
2. MessageLimiter.xlsx:给 trade 的三条求购单客户端消息加限流行(每秒 5 次,超限 tip 1000,与 scene 旧的四条悬赏消息同档)。
   消息号不手写:按名字从 proto/message_id.txt 里查(proto-gen-run 之后才有),查不到就报错退出,先生成协议再跑。
   已有这一号的行:值相同只报告,值不同报错(别人改过,先查清楚)。

用法(仓库根目录):python tools/scripts/trade_wanted_skill_book_xlsx_patch.py [--dry-run] [--tips-only]
之后照常导表(dev.bat gen),Go 引用的 kTradeWantedOrderLimitReached / kTradeWantedBookMismatch 才有定义。
"""

from __future__ import annotations

import argparse
from pathlib import Path

import openpyxl

ROOT = Path(__file__).resolve().parents[2]
TIP_XLSX = ROOT / "data" / "tip" / "Tip.xlsx"
LIMITER_XLSX = ROOT / "data" / "MessageLimiter.xlsx"
MESSAGE_IDS = ROOT / "proto" / "message_id.txt"
GROUP = "//trade_error"
TIPS: list[tuple[str, str]] = [
    ("TradeWantedOrderLimitReached", "同时最多挂 3 张悬赏求书单，请先撤掉一张再发榜"),
    ("TradeWantedBookMismatch", "交上的书不符合这张悬赏单的要求，已原样退回"),
]
LIMITED_MESSAGES = [
    "ClientPlayerJubaozhaiCreateWantedSkillBookOrder",
    "ClientPlayerJubaozhaiFulfillWantedSkillBookOrder",
    "ClientPlayerJubaozhaiListWantedSkillBookOrders",
]
LIMIT_ROW = (5, 1, 1000)  # max_requests, time_window(秒), tip_message
LIMITER_FIRST_DATA_ROW = 6  # 第 1 行列名,2~5 行空,数据从第 6 行起(与现表一致)


def tip_names_by_row(ws) -> dict[str, int]:
    out: dict[str, int] = {}
    for row in range(1, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if isinstance(value, str) and value.strip() and not value.strip().startswith("//"):
            if value.strip() in out:
                raise SystemExit(f"Tip.xlsx: code name {value.strip()} appears twice (rows {out[value.strip()]} and {row}); fix the table first")
            out[value.strip()] = row
    return out


def tip_group_end(ws) -> int:
    inside = False
    for row in range(1, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if isinstance(value, str) and value.strip().startswith("//"):
            if value.strip().startswith(GROUP + " "):
                inside = True
            elif inside:
                return row
    raise SystemExit(f"Tip.xlsx: cannot find the end of the {GROUP} group")


def patch_tips(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = wb.active
    existing = tip_names_by_row(ws)
    present = [name for name, _ in TIPS if name in existing]
    if len(present) == len(TIPS):
        print(f"[ALREADY CURRENT] Tip.xlsx: {', '.join(present)} present; file unchanged")
        return
    if present:
        raise SystemExit(f"Tip.xlsx: only some of the codes are present ({present}); find out who added them before patching")

    insert_at = tip_group_end(ws)
    print(f"Tip.xlsx: inserting {len(TIPS)} codes at the end of {GROUP} (before row {insert_at})")
    if dry_run:
        print("[dry-run] Tip.xlsx not written")
        return
    ws.insert_rows(insert_at, len(TIPS))
    for offset, (name, text) in enumerate(TIPS):
        ws.cell(insert_at + offset, 1, name)
        ws.cell(insert_at + offset, 2, text)
    wb.save(TIP_XLSX)

    check = openpyxl.load_workbook(TIP_XLSX).active
    after = tip_names_by_row(check)
    for offset, (name, text) in enumerate(TIPS):
        row = insert_at + offset
        if after.get(name) != row or check.cell(row, 2).value != text or check.cell(row, 3).value is not None:
            raise SystemExit(f"Tip.xlsx: read-back mismatch for {name} at row {row}")
    if tip_group_end(check) != insert_at + len(TIPS):
        raise SystemExit("Tip.xlsx: read-back mismatch, the next group header did not move down by the inserted rows")
    if len(after) != len(existing) + len(TIPS):
        raise SystemExit("Tip.xlsx: read-back mismatch, code count changed unexpectedly")
    print(f"[DONE] Tip.xlsx: inserted {', '.join(name for name, _ in TIPS)} at rows {insert_at}-{insert_at + len(TIPS) - 1}")


def message_ids() -> dict[str, int]:
    out: dict[str, int] = {}
    for line in MESSAGE_IDS.read_text(encoding="utf-8").splitlines():
        number, sep, name = line.strip().partition("=")
        if sep and number.isdigit() and not name.startswith("reserved:"):
            out[name] = int(number)
    return out


def patch_limiter(dry_run: bool) -> None:
    ids = message_ids()
    missing = [name for name in LIMITED_MESSAGES if name not in ids]
    if missing:
        raise SystemExit(f"MessageLimiter.xlsx: {missing} not in proto/message_id.txt yet; run proto-gen-run first")
    wb = openpyxl.load_workbook(LIMITER_XLSX)
    ws = wb.worksheets[0]
    rows: dict[int, int] = {}
    last = LIMITER_FIRST_DATA_ROW - 1
    for row in range(LIMITER_FIRST_DATA_ROW, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if value is None:
            continue
        if not isinstance(value, int):
            raise SystemExit(f"MessageLimiter.xlsx: row {row} id {value!r} is not an integer; fix the table first")
        if value in rows:
            raise SystemExit(f"MessageLimiter.xlsx: id {value} appears twice (rows {rows[value]} and {row}); fix the table first")
        rows[value] = row
        last = row
    todo = []
    for name in LIMITED_MESSAGES:
        message_id = ids[name]
        if message_id in rows:
            current = tuple(ws.cell(rows[message_id], c).value for c in range(2, 5))
            if current != LIMIT_ROW:
                raise SystemExit(f"MessageLimiter.xlsx: id {message_id} ({name}) already has {current}, expected {LIMIT_ROW}; find out who changed it")
            continue
        todo.append((name, message_id))
    if not todo:
        print("[ALREADY CURRENT] MessageLimiter.xlsx: all three wanted skill book messages limited; file unchanged")
        return
    print(f"MessageLimiter.xlsx: appending {', '.join(f'{name}={message_id}' for name, message_id in todo)} after row {last}")
    if dry_run:
        print("[dry-run] MessageLimiter.xlsx not written")
        return
    for offset, (_, message_id) in enumerate(todo, start=1):
        for column, value in enumerate((message_id,) + LIMIT_ROW, start=1):
            ws.cell(last + offset, column, value)
    wb.save(LIMITER_XLSX)

    check = openpyxl.load_workbook(LIMITER_XLSX).worksheets[0]
    for offset, (name, message_id) in enumerate(todo, start=1):
        got = tuple(check.cell(last + offset, c).value for c in range(1, 5))
        if got != (message_id,) + LIMIT_ROW:
            raise SystemExit(f"MessageLimiter.xlsx: read-back mismatch for {name} at row {last + offset}: {got}")
    print(f"[DONE] MessageLimiter.xlsx: appended {len(todo)} rows at {last + 1}-{last + len(todo)}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--tips-only", action="store_true", help="only patch Tip.xlsx (MessageLimiter needs generated message ids)")
    args = parser.parse_args()
    patch_tips(args.dry_run)
    if not args.tips_only:
        patch_limiter(args.dry_run)


if __name__ == "__main__":
    main()

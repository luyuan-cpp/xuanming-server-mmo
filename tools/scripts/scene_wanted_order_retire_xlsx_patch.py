#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""悬赏求书迁 trade 托管合同批 B(数据覆写审计 r2-craft#1,docs/design/skill-book-wanted-order-trade-escrow.md §11.2)的配表补丁。

scene 的四条旧悬赏消息(SceneSkillClientPlayer 的 PostSkillBookWantedOrder / CancelSkillBookWantedOrder /
ListSkillBookWantedOrders / FulfillSkillBookWantedOrder)退役之后,生成器把它们的号转成墓碑(message_id.txt 里
`N=reserved:<原名>`),墓碑号永远不派发。导表器不拿限流行对 message_id.txt(MessageLimiter 的 id 列没有引用约束),
留着这四行不会报错,但它们是永远用不上的死配置,还会让人以为这几个号仍在用,所以同批删掉。

幂等:按名字从 proto/message_id.txt 查号(生成前是活号行,生成后是墓碑行,两种都认),查不到就报错退出;
四行都不在就只报告、不改文件;在的行限流值必须是旧值 (5, 1, 1000),不是就报错(别人改过,先查清楚)。
删行之后读回核对:四个号都不在了,其余行逐行不变。

用法(仓库根目录):python tools/scripts/scene_wanted_order_retire_xlsx_patch.py [--dry-run]
之后照常导表(dev.bat gen)。
"""

from __future__ import annotations

import argparse
from pathlib import Path

import openpyxl

ROOT = Path(__file__).resolve().parents[2]
LIMITER_XLSX = ROOT / "data" / "MessageLimiter.xlsx"
MESSAGE_IDS = ROOT / "proto" / "message_id.txt"
RETIRED_MESSAGES = [
    "SceneSkillClientPlayerPostSkillBookWantedOrder",
    "SceneSkillClientPlayerCancelSkillBookWantedOrder",
    "SceneSkillClientPlayerListSkillBookWantedOrders",
    "SceneSkillClientPlayerFulfillSkillBookWantedOrder",
]
OLD_LIMIT_ROW = (5, 1, 1000)  # max_requests, time_window(秒), tip_message
LIMITER_FIRST_DATA_ROW = 6  # 第 1 行列名,2~5 行空,数据从第 6 行起(与现表一致)
LIMITER_COLUMNS = 4


def message_ids_by_name() -> dict[str, int]:
    out: dict[str, int] = {}
    for line in MESSAGE_IDS.read_text(encoding="utf-8").splitlines():
        number, sep, name = line.strip().partition("=")
        if sep and number.isdigit():
            out[name.removeprefix("reserved:")] = int(number)
    return out


def limiter_rows(ws) -> dict[int, int]:
    rows: dict[int, int] = {}
    for row in range(LIMITER_FIRST_DATA_ROW, ws.max_row + 1):
        value = ws.cell(row, 1).value
        if value is None:
            continue
        if not isinstance(value, int):
            raise SystemExit(f"MessageLimiter.xlsx: row {row} id {value!r} is not an integer; fix the table first")
        if value in rows:
            raise SystemExit(f"MessageLimiter.xlsx: id {value} appears twice (rows {rows[value]} and {row}); fix the table first")
        rows[value] = row
    return rows


def snapshot(ws, skip_ids: set[int]) -> list[tuple]:
    return [
        tuple(ws.cell(row, c).value for c in range(1, LIMITER_COLUMNS + 1))
        for row in range(1, ws.max_row + 1)
        if ws.cell(row, 1).value not in skip_ids
    ]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    ids = message_ids_by_name()
    missing = [name for name in RETIRED_MESSAGES if name not in ids]
    if missing:
        raise SystemExit(f"MessageLimiter.xlsx: {missing} not in proto/message_id.txt; check the message names first")
    targets = {ids[name]: name for name in RETIRED_MESSAGES}

    wb = openpyxl.load_workbook(LIMITER_XLSX)
    ws = wb.worksheets[0]
    rows = limiter_rows(ws)
    todo = []
    for message_id, name in targets.items():
        if message_id not in rows:
            continue
        current = tuple(ws.cell(rows[message_id], c).value for c in range(2, LIMITER_COLUMNS + 1))
        if current != OLD_LIMIT_ROW:
            raise SystemExit(f"MessageLimiter.xlsx: id {message_id} ({name}) has {current}, expected {OLD_LIMIT_ROW}; find out who changed it")
        todo.append((rows[message_id], message_id, name))
    if not todo:
        print("[ALREADY CURRENT] MessageLimiter.xlsx: no rows for the four retired wanted order messages; file unchanged")
        return

    print("MessageLimiter.xlsx: deleting " + ", ".join(f"{name}={message_id} (row {row})" for row, message_id, name in todo))
    if args.dry_run:
        print("[dry-run] MessageLimiter.xlsx not written")
        return
    before = snapshot(ws, set(targets))
    for row, _, _ in sorted(todo, reverse=True):
        ws.delete_rows(row)
    wb.save(LIMITER_XLSX)

    check = openpyxl.load_workbook(LIMITER_XLSX).worksheets[0]
    after_rows = limiter_rows(check)
    still = [message_id for message_id in targets if message_id in after_rows]
    if still:
        raise SystemExit(f"MessageLimiter.xlsx: read-back mismatch, ids {still} still present")
    if len(after_rows) != len(rows) - len(todo):
        raise SystemExit("MessageLimiter.xlsx: read-back mismatch, row count changed unexpectedly")
    if snapshot(check, set(targets)) != before:
        raise SystemExit("MessageLimiter.xlsx: read-back mismatch, other rows changed")
    print(f"[DONE] MessageLimiter.xlsx: deleted {len(todo)} rows ({', '.join(str(message_id) for _, message_id, _ in todo)})")


if __name__ == "__main__":
    main()

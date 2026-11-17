#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""移植artifact-tool所写单元格，原xlsx包其它内容逐字节保留。

不使用openpyxl写表。单元格值必须与授权增量计划相符，源哈希不匹配就拒绝写入。
保留原列宽、行高、合并、样式、批注、验证、隐藏行列和所有非目标单元格。
"""
import copy
import hashlib
import io
import json
import re
import sys
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

NS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
TAG = "{" + NS + "}"
ET.register_namespace("", NS)
ET.register_namespace("r", "http://schemas.openxmlformats.org/officeDocument/2006/relationships")


def decode(cell, strings):
    if cell is None:
        return None
    if cell.get("t") == "inlineStr":
        return "".join(cell.itertext())
    value = cell.find(TAG + "v")
    if value is None:
        return None
    if cell.get("t") == "s":
        return strings[int(value.text)]
    if cell.get("t") in ("str", "e"):
        return value.text
    return float(value.text)


def col_number(address):
    n = 0
    for ch in re.match(r"[A-Z]+", address)[0]:
        n = n * 26 + ord(ch) - 64
    return n


def main():
    plan = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    source = Path(plan["source"])
    before = source.read_bytes()
    assert hashlib.sha256(before).hexdigest() == plan["sha256"], "Source changed during authoring"
    with zipfile.ZipFile(io.BytesIO(before)) as original, zipfile.ZipFile(plan["staged"]) as staged:
        # 本项目三张源表均唯一sheet，不改表次序；名称关系也随原包保留。
        sheet_path = "xl/worksheets/sheet1.xml"
        root = ET.fromstring(original.read(sheet_path))
        authored = ET.fromstring(staged.read(sheet_path))
        strings = []
        if "xl/sharedStrings.xml" in staged.namelist():
            strings = ["".join(si.itertext()) for si in ET.fromstring(staged.read("xl/sharedStrings.xml"))]
        authored_cells = {c.attrib["r"]: c for c in authored.iter(TAG + "c")}
        data = root.find(TAG + "sheetData")
        rows = {int(r.attrib["r"]): r for r in data}
        for address, expected in plan["cells"].items():
            cell = authored_cells.get(address)
            assert decode(cell, strings) == expected, f"Authored value mismatch: {address}"
            n = int(re.search(r"\d+", address)[0])
            if n not in rows:
                row = ET.Element(TAG + "row", {"r": str(n)})
                template = rows[max(rows)]
                row.attrib.update({k: v for k, v in template.attrib.items() if k != "r" and k != "spans"})
                data.append(row); rows[n] = row
            row = rows[n]
            prior = next((c for c in row if c.get("r") == address), None)
            if prior is not None:
                row.remove(prior)
            if expected is None:
                continue
            result = copy.deepcopy(cell)
            result.attrib.pop("s", None)
            # 旧目标格原样式优先，新格延续同列最近数据格/同行末格样式。
            template = prior
            if template is None:
                same_col = re.match(r"[A-Z]+", address)[0]
                for rn in sorted((r for r in rows if r < n), reverse=True):
                    template = next((c for c in rows[rn] if c.get("r") == f"{same_col}{rn}"), None)
                    if template is not None:
                        break
                if template is None and len(row):
                    template = row[-1]
            if template is not None and "s" in template.attrib:
                result.set("s", template.attrib["s"])
            if result.get("t") == "s":
                result.set("t", "inlineStr")
                for child in list(result):
                    result.remove(child)
                inline = ET.SubElement(result, TAG + "is")
                ET.SubElement(inline, TAG + "t").text = expected
            row.append(result)
            row[:] = sorted(row, key=lambda c: col_number(c.attrib["r"]))
        data[:] = sorted(data, key=lambda r: int(r.attrib["r"]))
        cells = [c for row in data for c in row]
        last_col = max(cells, key=lambda c: col_number(c.attrib["r"])).attrib["r"]
        last_col = re.match(r"[A-Z]+", last_col)[0]
        dimension = root.find(TAG + "dimension")
        if dimension is not None:
            dimension.set("ref", f"A1:{last_col}{max(rows)}")
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED) as target:
            for info in original.infolist():
                target.writestr(info, ET.tostring(root, encoding="utf-8") if info.filename == sheet_path else original.read(info.filename))
    assert source.read_bytes() == before, "Source changed before publication"
    source.write_bytes(output.getvalue())
    print(f"Updated {source.name}: {len(plan['cells'])} authored cells; unrelated package entries preserved")


if __name__ == "__main__":
    main()

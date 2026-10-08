#!/usr/bin/env python3
"""Generate clean redraw FairyGUI slices for the qdao server screen.

The generated assets are transparent PNG components, not full-screen overlays.
They are authored for FairyGUI 9-grid scaling and then arranged in
ServersV3.xml with transparent hit controls on top.
"""
from __future__ import annotations

import math
import pathlib
import xml.etree.ElementTree as ET

from PIL import Image, ImageDraw, ImageFilter


REPO = pathlib.Path(__file__).resolve().parents[2]
PKG = REPO / "client" / "fairygui" / "qdao" / "assets" / "qdao"
SLICE_DIR = PKG / "v3" / "slices"
PACKAGE_XML = PKG / "package.xml"

INK = (74, 46, 22, 255)
GOLD = (210, 154, 70, 255)
GOLD_DARK = (129, 78, 30, 255)
CREAM = (255, 244, 226, 255)
CREAM_2 = (238, 213, 180, 255)
JADE = (32, 126, 109, 255)
JADE_DARK = (19, 83, 75, 255)
RED = (216, 31, 22, 255)


ASSETS: list[dict[str, object]] = [
    {"id": "00021001", "name": "redraw_top_tab_active.png", "size": (320, 74), "kind": "top_active", "scale9": (58, 20, 210, 34)},
    {"id": "00021002", "name": "redraw_top_tab_idle.png", "size": (300, 70), "kind": "top_idle", "scale9": (54, 18, 192, 32)},
    {"id": "00021003", "name": "redraw_search.png", "size": (250, 62), "kind": "search", "scale9": (58, 18, 140, 26)},
    {"id": "00021004", "name": "redraw_left_active.png", "size": (256, 80), "kind": "left_active", "scale9": (46, 20, 164, 36)},
    {"id": "00021005", "name": "redraw_left_idle.png", "size": (256, 80), "kind": "left_idle", "scale9": (46, 20, 164, 36)},
    {"id": "00021006", "name": "redraw_server_card_0.png", "size": (514, 112), "kind": "server_active", "idx": 0, "scale9": (62, 24, 316, 54)},
    {"id": "00021007", "name": "redraw_server_card_1.png", "size": (514, 112), "kind": "server", "idx": 1, "scale9": (62, 24, 316, 54)},
    {"id": "00021008", "name": "redraw_server_card_2.png", "size": (514, 112), "kind": "server", "idx": 2, "scale9": (62, 24, 316, 54)},
    {"id": "00021009", "name": "redraw_server_card_3.png", "size": (514, 112), "kind": "server", "idx": 3, "scale9": (62, 24, 316, 54)},
    {"id": "00021010", "name": "redraw_server_card_4.png", "size": (514, 112), "kind": "server", "idx": 4, "scale9": (62, 24, 316, 54)},
    {"id": "00021011", "name": "redraw_server_card_5.png", "size": (514, 112), "kind": "server", "idx": 5, "scale9": (62, 24, 316, 54)},
    {"id": "00021012", "name": "redraw_server_card_6.png", "size": (514, 112), "kind": "server", "idx": 0, "scale9": (62, 24, 316, 54)},
    {"id": "00021013", "name": "redraw_server_card_7.png", "size": (514, 112), "kind": "server", "idx": 1, "scale9": (62, 24, 316, 54)},
    {"id": "00021014", "name": "redraw_bottom_strip.png", "size": (1514, 166), "kind": "bottom", "scale9": (190, 44, 1120, 64)},
    {"id": "00021015", "name": "redraw_status_red.png", "size": (34, 34), "kind": "status_red"},
]


def lerp(a: int, b: int, t: float) -> int:
    return round(a + (b - a) * t)


def gradient(size: tuple[int, int], top: tuple[int, int, int, int], bottom: tuple[int, int, int, int]) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size)
    px = img.load()
    for y in range(h):
        t = y / max(1, h - 1)
        col = tuple(lerp(top[i], bottom[i], t) for i in range(4))
        for x in range(w):
            px[x, y] = col
    return img


def rounded_mask(size: tuple[int, int], radius: int, inset: int = 0) -> Image.Image:
    w, h = size
    mask = Image.new("L", size, 0)
    d = ImageDraw.Draw(mask)
    d.rounded_rectangle((inset, inset, w - inset - 1, h - inset - 1), radius=radius, fill=255)
    return mask


def alpha_composite_shape(base: Image.Image, fill: Image.Image, mask: Image.Image) -> None:
    layer = Image.new("RGBA", base.size, (0, 0, 0, 0))
    layer.paste(fill, (0, 0), mask)
    base.alpha_composite(layer)


def draw_gold_frame(d: ImageDraw.ImageDraw, box: tuple[int, int, int, int], radius: int, width: int = 3) -> None:
    x0, y0, x1, y1 = box
    for i, col in enumerate((GOLD_DARK, GOLD, (255, 235, 166, 255))):
        d.rounded_rectangle((x0 + i, y0 + i, x1 - i, y1 - i), radius=radius, outline=col, width=width)


def draw_cloud(d: ImageDraw.ImageDraw, x: int, y: int, scale: float = 1.0, alpha: int = 95) -> None:
    col = (255, 255, 255, alpha)
    pts = [
        (x, y + 12 * scale, x + 26 * scale, y + 12 * scale),
        (x + 20 * scale, y + 12 * scale, x + 46 * scale, y + 12 * scale),
        (x + 42 * scale, y + 12 * scale, x + 70 * scale, y + 12 * scale),
    ]
    for x0, y0, x1, y1 in pts:
        d.arc((x0, y0 - 22 * scale, x1, y1 + 22 * scale), 190, 350, fill=col, width=max(1, round(2 * scale)))


def draw_ink_stroke(d: ImageDraw.ImageDraw, x: int, y: int, w: int) -> None:
    col = (105, 83, 62, 150)
    for i in range(4):
        yy = y + i * 3
        d.line((x + i * 8, yy, x + w - i * 14, yy + math.sin(i) * 2), fill=col, width=max(1, 3 - i // 2))


def draw_badge(d: ImageDraw.ImageDraw, cx: int, cy: int, r: int, idx: int) -> None:
    palettes = [
        ((27, 122, 105, 255), (236, 202, 102, 255)),
        ((125, 82, 33, 255), (245, 210, 122, 255)),
        ((29, 120, 110, 255), (239, 193, 63, 255)),
        ((25, 91, 125, 255), (123, 210, 221, 255)),
        ((39, 100, 105, 255), (239, 111, 116, 255)),
        ((76, 105, 47, 255), (232, 191, 95, 255)),
    ]
    c1, c2 = palettes[idx % len(palettes)]
    d.ellipse((cx - r, cy - r, cx + r, cy + r), fill=c1, outline=GOLD_DARK, width=4)
    d.ellipse((cx - r + 6, cy - r + 6, cx + r - 6, cy + r - 6), outline=(255, 230, 162, 255), width=3)
    mode = idx % 6
    if mode == 0:  # pavilion
        d.polygon((cx - 28, cy - 2, cx, cy - 30, cx + 28, cy - 2), fill=c2)
        d.rectangle((cx - 20, cy - 1, cx + 20, cy + 24), fill=(36, 116, 96, 255), outline=(255, 226, 140, 255), width=3)
        d.line((cx - 30, cy + 26, cx + 30, cy + 26), fill=c2, width=5)
    elif mode == 1:  # pagoda
        for k, yy in enumerate((-24, -6, 12)):
            d.polygon((cx - 24 + k * 3, cy + yy, cx, cy + yy - 14, cx + 24 - k * 3, cy + yy), fill=c2)
        d.rectangle((cx - 10, cy - 3, cx + 10, cy + 28), fill=(111, 67, 28, 255))
    elif mode == 2:  # sword
        d.polygon((cx, cy - 34, cx + 8, cy + 6, cx, cy + 34, cx - 8, cy + 6), fill=c2)
        d.line((cx - 25, cy + 8, cx + 25, cy + 8), fill=(255, 232, 156, 255), width=5)
    elif mode == 3:  # swirl
        for rr in (30, 22, 14):
            d.arc((cx - rr, cy - rr, cx + rr, cy + rr), 35, 305, fill=c2, width=6)
    elif mode == 4:  # lotus
        for ang in range(0, 360, 72):
            rad = math.radians(ang)
            px = cx + math.cos(rad) * 16
            py = cy + math.sin(rad) * 10
            d.ellipse((px - 13, py - 18, px + 13, py + 18), fill=(244, 118, 122, 245), outline=c2, width=2)
    else:  # talisman coin
        d.rounded_rectangle((cx - 24, cy - 24, cx + 24, cy + 24), radius=8, fill=(68, 121, 74, 255), outline=c2, width=4)
        d.line((cx, cy - 18, cx, cy + 18), fill=c2, width=3)
        d.line((cx - 18, cy, cx + 18, cy), fill=c2, width=3)


def add_shadow(img: Image.Image, radius: int = 5, dy: int = 3) -> Image.Image:
    alpha = img.getchannel("A")
    shadow = Image.new("RGBA", img.size, (0, 0, 0, 0))
    shadow.putalpha(alpha.filter(ImageFilter.GaussianBlur(radius)).point(lambda v: min(110, v)))
    out = Image.new("RGBA", img.size, (0, 0, 0, 0))
    out.alpha_composite(shadow, (0, dy))
    out.alpha_composite(img)
    return out


def draw_top(kind: str, size: tuple[int, int]) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size, (0, 0, 0, 0))
    mask = rounded_mask(size, 14, 2)
    if kind == "top_active":
        fill = gradient(size, (139, 218, 190, 255), (32, 126, 109, 255))
    else:
        fill = gradient(size, (255, 249, 239, 255), (235, 210, 181, 255))
    alpha_composite_shape(img, fill, mask)
    d = ImageDraw.Draw(img)
    draw_gold_frame(d, (2, 2, w - 3, h - 3), 14)
    draw_ink_stroke(d, 70, h // 2 - 2, w - 130)
    d.polygon((w - 36, h // 2, w - 18, h // 2 - 14, w - 18, h // 2 + 14), fill=GOLD)
    return add_shadow(img, 3, 2)


def draw_search(size: tuple[int, int]) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size, (0, 0, 0, 0))
    mask = rounded_mask(size, 18, 2)
    alpha_composite_shape(img, gradient(size, (255, 249, 238, 245), (232, 212, 184, 245)), mask)
    d = ImageDraw.Draw(img)
    draw_gold_frame(d, (2, 2, w - 3, h - 3), 18, 2)
    d.ellipse((18, 17, 40, 39), outline=(116, 89, 66, 220), width=4)
    d.line((36, 36, 50, 50), fill=(116, 89, 66, 220), width=4)
    draw_cloud(d, w - 76, h - 34, 0.7, 80)
    return add_shadow(img, 3, 2)


def draw_left(kind: str, size: tuple[int, int]) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size, (0, 0, 0, 0))
    mask = rounded_mask(size, 22, 2)
    if kind == "left_active":
        fill = gradient(size, (96, 212, 178, 255), (25, 126, 110, 255))
    else:
        fill = gradient(size, (255, 247, 233, 250), (233, 208, 180, 250))
    alpha_composite_shape(img, fill, mask)
    d = ImageDraw.Draw(img)
    draw_gold_frame(d, (2, 2, w - 3, h - 3), 22, 2)
    if kind == "left_active":
        d.ellipse((26, 22, 62, 58), fill=(239, 250, 224, 230))
        d.arc((32, 28, 56, 52), 40, 320, fill=JADE, width=4)
    else:
        d.ellipse((26, 22, 62, 58), outline=(125, 84, 50, 220), width=3)
    draw_cloud(d, w - 74, h - 34, 0.7, 95)
    return add_shadow(img, 4, 2)


def draw_status(size: tuple[int, int]) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size, (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.ellipse((1, 1, w - 2, h - 2), fill=(145, 56, 24, 255), outline=GOLD_DARK, width=2)
    d.ellipse((5, 5, w - 6, h - 6), fill=RED)
    d.ellipse((8, 6, 20, 18), fill=(255, 235, 218, 230))
    return add_shadow(img, 2, 1)


def draw_server(size: tuple[int, int], active: bool, idx: int = 0) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size, (0, 0, 0, 0))
    mask = rounded_mask(size, 26, 2)
    fill = gradient(size, (255, 250, 240, 252), (240, 220, 191, 252))
    alpha_composite_shape(img, fill, mask)
    d = ImageDraw.Draw(img)
    draw_gold_frame(d, (2, 2, w - 3, h - 3), 26, 2)
    if active:
        d.rounded_rectangle((8, 8, w - 9, h - 9), radius=21, outline=(43, 145, 118, 220), width=4)
    d.line((70, h - 10, w - 88, h - 10), fill=(255, 255, 255, 95), width=2)
    d.ellipse((30, h // 2 - 15, 60, h // 2 + 15), fill=(145, 56, 24, 255), outline=GOLD_DARK, width=2)
    d.ellipse((35, h // 2 - 10, 55, h // 2 + 10), fill=RED)
    d.ellipse((38, h // 2 - 9, 48, h // 2 + 1), fill=(255, 235, 218, 230))
    draw_ink_stroke(d, 86, h // 2 - 8, 145)
    draw_cloud(d, w - 240, h - 45, 1.0, 85)
    draw_badge(d, w - 62, h // 2, 44, idx)
    return add_shadow(img, 4, 2)


def draw_bottom(size: tuple[int, int]) -> Image.Image:
    w, h = size
    img = Image.new("RGBA", size, (0, 0, 0, 0))
    outer_mask = rounded_mask(size, 42, 1)
    alpha_composite_shape(img, gradient(size, (36, 138, 119, 255), (21, 101, 89, 255)), outer_mask)
    d = ImageDraw.Draw(img)
    draw_gold_frame(d, (2, 2, w - 3, h - 3), 42, 3)
    inner = (150, 42, w - 70, h - 42)
    d.rounded_rectangle(inner, radius=28, fill=(255, 244, 226, 250), outline=GOLD, width=3)
    d.ellipse((32, 12, 156, 136), fill=(255, 248, 232, 255), outline=GOLD, width=4)
    d.polygon((94, 40, 128, 110, 58, 110), fill=(68, 126, 139, 255))
    for i, x in enumerate((360, 520, 680, 840)):
        d.ellipse((x, h // 2 - 15, x + 30, h // 2 + 15), fill=(61, 57, 41, 255), outline=GOLD, width=3)
        draw_ink_stroke(d, x + 55, h // 2 - 6, 90)
    d.rounded_rectangle((w - 250, 50, w - 70, h - 50), radius=24, fill=(255, 246, 232, 250), outline=GOLD, width=3)
    return add_shadow(img, 5, 3)


def make_asset(row: dict[str, object]) -> Image.Image:
    kind = str(row["kind"])
    size = row["size"]  # type: ignore[assignment]
    if kind.startswith("top_"):
        return draw_top(kind, size)  # type: ignore[arg-type]
    if kind == "search":
        return draw_search(size)  # type: ignore[arg-type]
    if kind.startswith("left_"):
        return draw_left(kind, size)  # type: ignore[arg-type]
    if kind == "server":
        return draw_server(size, False, int(row.get("idx", 0)))  # type: ignore[arg-type]
    if kind == "server_active":
        return draw_server(size, True, int(row.get("idx", 0)))  # type: ignore[arg-type]
    if kind == "bottom":
        return draw_bottom(size)  # type: ignore[arg-type]
    if kind == "status_red":
        return draw_status(size)  # type: ignore[arg-type]
    raise ValueError(kind)


def write_meta(path: pathlib.Path, row: dict[str, object], w: int, h: int, flat: bool) -> str:
    scale9 = row.get("scale9")
    attrs = {
        "id": str(row["id"]),
        "name": str(row["name"]),
        "path": "/" if flat else "/v3/slices/",
        "exported": "true",
        "size": f"{w},{h}",
        "qualityOption": "source",
    }
    if scale9:
        attrs["scale"] = "9grid"
        attrs["scale9grid"] = ",".join(str(v) for v in scale9)
    elem = ET.Element("image", attrs)
    xml = ET.tostring(elem, encoding="unicode", short_empty_elements=True)
    path.write_text('<?xml version="1.0" encoding="utf-8"?>\n' + xml + "\n", encoding="utf-8")
    return xml


def replace_entries(entries: list[str]) -> None:
    root = ET.parse(PACKAGE_XML).getroot()
    resources = root.find("resources")
    assert resources is not None
    for child in list(resources):
        cid = child.attrib.get("id", "")
        if cid.startswith("000210"):
            resources.remove(child)
    for entry in entries:
        resources.append(ET.fromstring(entry))
    ET.indent(root, space="  ")
    ET.ElementTree(root).write(PACKAGE_XML, encoding="utf-8", xml_declaration=True)


def write_servers_xml() -> None:
    positions = [
        (956, 296, 514, 112),
        (1452, 296, 520, 112),
        (956, 420, 514, 112),
        (1452, 420, 520, 112),
        (956, 544, 514, 112),
        (1452, 544, 520, 112),
        (956, 668, 514, 112),
        (1452, 668, 520, 112),
    ]
    server_lines = []
    server_text_lines = []
    server_names = ["云海宗", "清风谷", "碧落渊", "紫霄峰", "沧浪洲", "昆仑墟", "蓬莱岛", "瀛洲海"]
    for idx, (x, y, w, h) in enumerate(positions):
        src = f"000210{6 + idx:02d}"
        name = f"redraw_server_card_{idx}.png"
        server_lines.append(f'    <image id="sv{idx}" name="serverVisual{idx}" src="{src}" fileName="{name}" xy="{x},{y}" size="{w},{h}"/>')
        server_text_lines.append(
            f'    <text id="st{idx}a" name="serverName{idx}" xy="{x + 92},{y + 22}" size="220,34" fontSize="26" color="#5a4025" align="left" vAlign="middle" autoSize="none" singleLine="true" text="{server_names[idx]}"/>'
        )
        server_text_lines.append(
            f'    <text id="st{idx}b" name="serverState{idx}" xy="{x + 270},{y + 60}" size="90,28" fontSize="20" color="#2f8171" align="center" vAlign="middle" autoSize="none" singleLine="true" text="流畅"/>'
        )

    xml = f'''<?xml version="1.0" encoding="utf-8"?>
<component size="2560,1080" opaque="false">
  <displayList>
    <image id="n0" name="bg" src="00020001" fileName="bg_scene.png" xy="0,0" size="2560,1080"/>
    <image id="t0" name="tabRecentVisual" src="00021001" fileName="redraw_top_tab_active.png" xy="720,158" size="320,74"/>
    <image id="t1" name="tabRecommendVisual" src="00021002" fileName="redraw_top_tab_idle.png" xy="1056,158" size="300,70"/>
    <image id="t2" name="tabAllVisual" src="00021002" fileName="redraw_top_tab_idle.png" xy="1370,158" size="310,70"/>
    <image id="n8" name="search" src="00021003" fileName="redraw_search.png" xy="646,306" size="250,62"/>
    <image id="lv0" name="leftActiveVisual" src="00021004" fileName="redraw_left_active.png" xy="644,372" size="256,80"/>
    <image id="lv1" name="leftIdle1Visual" src="00021005" fileName="redraw_left_idle.png" xy="644,448" size="256,80"/>
    <image id="lv2" name="leftIdle2Visual" src="00021005" fileName="redraw_left_idle.png" xy="644,524" size="256,80"/>
    <image id="lv3" name="leftIdle3Visual" src="00021005" fileName="redraw_left_idle.png" xy="644,600" size="256,80"/>
    <image id="lv4" name="leftIdle4Visual" src="00021005" fileName="redraw_left_idle.png" xy="644,676" size="256,80"/>
    <image id="lv5" name="leftIdle5Visual" src="00021005" fileName="redraw_left_idle.png" xy="644,752" size="256,80"/>
{chr(10).join(server_lines)}
    <image id="n12" name="bottomPane" src="00021014" fileName="redraw_bottom_strip.png" xy="610,806" size="1514,166"/>
    <text id="tt0" name="tabRecentText" xy="720,174" size="320,40" fontSize="28" color="#fff6d8" align="center" vAlign="middle" autoSize="none" singleLine="true" text="最近登录"/>
    <text id="tt1" name="tabRecommendText" xy="1056,174" size="300,40" fontSize="28" color="#654625" align="center" vAlign="middle" autoSize="none" singleLine="true" text="推荐服务器"/>
    <text id="tt2" name="tabAllText" xy="1370,174" size="310,40" fontSize="28" color="#654625" align="center" vAlign="middle" autoSize="none" singleLine="true" text="全部区服"/>
    <text id="lt0" name="leftText0" xy="704,394" size="150,34" fontSize="25" color="#fff6d8" align="left" vAlign="middle" autoSize="none" singleLine="true" text="近期"/>
    <text id="lt1" name="leftText1" xy="704,470" size="150,34" fontSize="25" color="#654625" align="left" vAlign="middle" autoSize="none" singleLine="true" text="推荐"/>
    <text id="lt2" name="leftText2" xy="704,546" size="150,34" fontSize="25" color="#654625" align="left" vAlign="middle" autoSize="none" singleLine="true" text="全部"/>
    <text id="lt3" name="leftText3" xy="704,622" size="150,34" fontSize="25" color="#654625" align="left" vAlign="middle" autoSize="none" singleLine="true" text="新服"/>
    <text id="lt4" name="leftText4" xy="704,698" size="150,34" fontSize="25" color="#654625" align="left" vAlign="middle" autoSize="none" singleLine="true" text="官方"/>
    <text id="lt5" name="leftText5" xy="704,774" size="150,34" fontSize="25" color="#654625" align="left" vAlign="middle" autoSize="none" singleLine="true" text="渠道"/>
    <text id="searchText" name="searchText" xy="708,321" size="170,32" fontSize="22" color="#7c6043" align="left" vAlign="middle" autoSize="none" singleLine="true" text="搜索服务器"/>
{chr(10).join(server_text_lines)}
    <text id="bt0" name="lastLoginText" xy="796,870" size="420,34" fontSize="24" color="#5a4025" align="left" vAlign="middle" autoSize="none" singleLine="true" text="最近登录：云海宗"/>
    <text id="bt1" name="selectedText" xy="1268,870" size="320,34" fontSize="24" color="#5a4025" align="left" vAlign="middle" autoSize="none" singleLine="true" text="当前选择"/>
    <text id="bt2" name="btnRefreshText" xy="1700,878" size="170,32" fontSize="24" color="#654625" align="center" vAlign="middle" autoSize="none" singleLine="true" text="刷新"/>
    <text id="bt3" name="btnEnterText" xy="1874,874" size="250,38" fontSize="30" color="#654625" align="center" vAlign="middle" autoSize="none" singleLine="true" text="进入"/>
    <component id="n5" name="tabRecent" src="c0000004" fileName="common/V3Tab.xml" xy="720,158" size="320,74" alpha="0"><Button title="最近登录"/></component>
    <component id="n6" name="tabRecommend" src="c0000004" fileName="common/V3Tab.xml" xy="1056,158" size="300,70" alpha="0"><Button title="推荐服务器"/></component>
    <component id="n7" name="tabAll" src="c0000004" fileName="common/V3Tab.xml" xy="1370,158" size="310,70" alpha="0"><Button title="全部区服"/></component>
    <list id="n10" name="tabs" xy="644,372" size="256,460" alpha="0" overflow="hidden" lineGap="0" defaultItem="ui://qdao/V3Tab" selectionMode="Single" align="center">
      <item title="近期"/>
      <item title="推荐"/>
      <item title="全部"/>
      <item title="新服"/>
      <item title="官方"/>
    </list>
    <list id="n11" name="servers" xy="956,296" size="1016,484" alpha="0" layout="pagination" overflow="scroll" lineGap="12" columnGap="18" defaultItem="ui://qdao/V3Card" selectionMode="Single" align="center">
      <item title="云海宗"/>
      <item title="清风谷"/>
      <item title="碧落渊"/>
      <item title="紫霄峰"/>
      <item title="沧浪洲"/>
      <item title="昆仑墟"/>
      <item title="蓬莱岛"/>
      <item title="瀛洲海"/>
    </list>
    <component id="n15" name="btnBack" src="c0000003" fileName="common/V3BtnAlt.xml" xy="610,806" size="170,166" alpha="0"><Button title="返回"/></component>
    <component id="n16" name="btnRefresh" src="c0000003" fileName="common/V3BtnAlt.xml" xy="1700,852" size="170,82" alpha="0"><Button title="刷新"/></component>
    <component id="n17" name="btnEnter" src="c0000002" fileName="common/V3Btn.xml" xy="1874,846" size="250,92" alpha="0"><Button title="进入"/></component>
    <text id="n18" name="status" xy="1100,884" size="520,28" fontSize="22" color="#fff0bd" align="left" vAlign="middle" autoSize="none" singleLine="true" text=""/>
  </displayList>
</component>
'''
    (PKG / "ServersV3.xml").write_text(xml, encoding="utf-8")


def main() -> int:
    SLICE_DIR.mkdir(parents=True, exist_ok=True)
    for folder in (PKG, SLICE_DIR):
        for old in folder.glob("redraw_*.png*"):
            old.unlink()
    entries: list[str] = []
    for row in ASSETS:
        img = make_asset(row)
        name = str(row["name"])
        nested = SLICE_DIR / name
        flat = PKG / name
        img.save(nested, "PNG", optimize=True)
        img.save(flat, "PNG", optimize=True)
        write_meta(nested.with_suffix(".png.xml"), row, img.width, img.height, flat=False)
        entries.append(write_meta(flat.with_suffix(".png.xml"), row, img.width, img.height, flat=True))
    replace_entries(entries)
    write_servers_xml()
    print(f"[slice-qdao] redrew {len(ASSETS)} clean FairyGUI 9-grid assets")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""从真实落盘 MSET 查询洞、断岛和完整路线，并拒绝损坏输入。"""
import json
import os
import ctypes
from pathlib import Path
import subprocess
import struct
import tempfile
import unittest


TOOL_DIR = Path(__file__).resolve().parent
BAKER = Path(os.environ.get("NAVMESH_BAKER", TOOL_DIR / "build/Release/navmesh_baker.exe"))
QUERY = Path(os.environ.get("NAVMESH_QUERY", BAKER.with_name("navmesh_query.exe")))


class MsetQueryContractTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mset-query-")
        self.folder = Path(self.temporary.name)
        self.nav = self.folder / "scene.bin"
        vertices, triangles = [], []
        for x0, z0, x1, z1 in ((0, 0, 20, 8), (0, 12, 20, 20),
                               (0, 8, 8, 12), (12, 8, 20, 12), (32, 0, 36, 4)):
            first = len(vertices) + 1
            vertices += [(x0, 0, z0), (x0, 0, z1), (x1, 0, z1), (x1, 0, z0)]
            triangles += [(first, first + 1, first + 2), (first, first + 2, first + 3)]
        obj = self.folder / "ground.obj"
        obj.write_text("".join(f"v {x} {y} {z}\n" for x, y, z in vertices) +
                       "".join(f"f {a} {b} {c}\n" for a, b, c in triangles), encoding="ascii")
        result = subprocess.run([str(BAKER), "--obj", str(obj), "--profile", "authored-flat",
                                 "--cs", "0.1", "--agent-radius", "0.3", "--out", str(self.nav),
                                 "--probe", "2,0,2"], capture_output=True, text=True,
                                encoding="utf-8", timeout=30)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)

    def tearDown(self):
        self.temporary.cleanup()

    def invoke(self, *args):
        return subprocess.run([str(QUERY), "--nav", str(self.nav), *args],
                              capture_output=True, text=True, encoding="utf-8", timeout=30)

    def test_original_points_hole_detour_and_disconnected_island(self):
        result = self.invoke("--point", "2,0,2", "--point", "2,0.2,2", "--point", "10,0,10",
                             "--route", "2,0,10,18,0,10", "--route", "2,0,2,34,0,2")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual([True, False, False], [point["onMesh"] for point in report["points"]])
        self.assertEqual([2, 0, 2], report["points"][0]["nearest"])
        detour, disconnected = report["routes"]
        self.assertTrue(detour["complete"])
        self.assertGreater(len(detour["corners"]), 2)
        self.assertTrue(detour["raycast"]["hit"])
        self.assertLess(detour["raycast"]["t"], 1)
        self.assertTrue(disconnected["startOnMesh"] and disconnected["endOnMesh"])
        self.assertFalse(disconnected["complete"])

    def test_bad_headers_truncation_and_trailing_bytes_are_rejected(self):
        original = self.nav.read_bytes()
        for malformed in (b"", b"broken", b"BAD!" + original[4:],
                          original[:12], original[:-1], original + b"unexpected"):
            with self.subTest(length=len(malformed)):
                self.nav.write_bytes(malformed)
                result = self.invoke("--point", "2,0,2")
                self.assertNotEqual(0, result.returncode)
                self.assertIn("error:", result.stderr)
                self.assertEqual("", result.stdout)

    def test_nonfinite_partial_or_trailing_coordinates_are_rejected(self):
        for point in ("nan,0,2", "2,inf,2", "2,0", "2,0,2garbage", "2,0,2,3"):
            with self.subTest(point=point):
                result = self.invoke("--point", point)
                self.assertNotEqual(0, result.returncode)
                self.assertEqual("", result.stdout)

    def test_oversized_tile_section_counts_are_rejected_before_native_load(self):
        # 当前 UE5 double ABI：MSET 头为三个 int32、对齐和 80 字节参数，tile 头为
        # uint64 引用加 int32 长度；tile 计数字段是 uint16，不能据此盲读数组。
        class Params(ctypes.Structure):
            _fields_ = [("values", ctypes.c_double * 9), ("maxTiles", ctypes.c_int32),
                        ("maxPolys", ctypes.c_int32)]

        class Header(ctypes.Structure):
            _fields_ = [("magic", ctypes.c_int32), ("version", ctypes.c_int32),
                        ("numTiles", ctypes.c_int32), ("params", Params)]

        class TileHeader(ctypes.Structure):
            _fields_ = [("tileRef", ctypes.c_uint64), ("dataSize", ctypes.c_int32)]

        payload = ctypes.sizeof(Header) + ctypes.sizeof(TileHeader)
        original = self.nav.read_bytes()
        for offset in (6, 16, 20, 22, 24):
            with self.subTest(offset=offset):
                malformed = bytearray(original)
                struct.pack_into("<H", malformed, payload + offset, 65535)
                self.nav.write_bytes(malformed)
                result = self.invoke("--point", "2,0,2")
                self.assertNotEqual(0, result.returncode)
                self.assertIn("error:", result.stderr)
                self.assertEqual("", result.stdout)


if __name__ == "__main__":
    unittest.main()

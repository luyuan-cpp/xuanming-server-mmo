"""验证独立地图位图入口、失败不落盘以及旧天墉入口兼容。"""
import base64
from pathlib import Path
import subprocess
import tempfile
import unittest

BAKER = Path(__file__).resolve().parent / "build/Release/navmesh_baker.exe"

class BakerContractTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="city-nav-contract-")
        self.path = Path(self.temp.name)
        bits = bytearray(2813)
        for y in range(90, 116):
            for x in range(90, 116):
                i = y * 150 + x
                bits[i // 8] |= 0x80 >> (i % 8)
        self.encoded = base64.b64encode(bits).decode("ascii")
        self.mask = self.path / "walkmask.txt"
        self.mask.write_text(self.encoded, encoding="ascii")
        self.out = self.path / "scene.bin"

    def tearDown(self):
        self.temp.cleanup()

    def invoke(self, *args):
        return subprocess.run([str(BAKER), *map(str, args), "--out", str(self.out)],
                              capture_output=True, text=True, encoding="utf-8", timeout=30)

    def test_standalone_mask_matches_legacy_geometry_without_legacy_spawn(self):
        result = self.invoke("--mask-base64", self.mask, "--probe", "250,0,100")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn("OFF MESH", self.invoke("--mask-base64", self.mask, "--probe", "200,0,180").stdout)
        standalone = self.out.read_bytes()
        legacy = self.path / "city.cs"
        legacy.write_text('const string WalkMaskBase64 = "' + self.encoded + '";', encoding="ascii")
        result = self.invoke("--painted-city", legacy, "--no-default-probe", "--probe", "250,0,100")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertEqual(standalone, self.out.read_bytes())

    def test_missing_spawn_is_rejected_before_output(self):
        result = self.invoke("--mask-base64", self.mask)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("requires at least one", result.stderr)
        self.assertFalse(self.out.exists())

    def test_off_mesh_spawn_is_rejected_without_output(self):
        result = self.invoke("--mask-base64", self.mask, "--probe", "200,0,180")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("OFF MESH", result.stdout)
        self.assertFalse(self.out.exists())

    def test_malformed_and_wrong_length_masks_are_rejected(self):
        for bad in ["?" * len(self.encoded), self.encoded[:-4],
                    self.encoded + "AAAA", "=" + self.encoded[1:]]:
            with self.subTest(bad=bad[:8]):
                self.mask.write_text(bad, encoding="ascii")
                result = self.invoke("--mask-base64", self.mask, "--probe", "250,0,100")
                self.assertNotEqual(0, result.returncode)
                self.assertFalse(self.out.exists())

    def test_empty_geometry_is_rejected(self):
        self.mask.write_text(base64.b64encode(bytes(2813)).decode("ascii"), encoding="ascii")
        result = self.invoke("--mask-base64", self.mask, "--probe", "250,0,100")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("no walkable geometry", result.stderr)
        self.assertFalse(self.out.exists())

    def test_nonfinite_and_trailing_probe_values_are_rejected(self):
        for probe in ["nan,0,100", "250,inf,100", "250,0,100garbage", "250,0"]:
            result = self.invoke("--mask-base64", self.mask, "--probe", probe)
            self.assertNotEqual(0, result.returncode)
            self.assertIn("bad --probe", result.stderr)
            self.assertFalse(self.out.exists())

if __name__ == "__main__":
    unittest.main()

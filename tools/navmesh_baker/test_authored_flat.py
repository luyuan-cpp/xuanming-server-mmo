"""手绘平面 OBJ 的原脚点、角色参数及旧模式隔离回归。"""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


BAKER = Path(os.environ.get("NAVMESH_BAKER", Path(__file__).resolve().parent / "build/Release/navmesh_baker.exe"))


class AuthoredFlatContractTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="authored-flat-nav-")
        self.folder = Path(self.temporary.name)
        self.obj = self.folder / "ground.obj"
        self.out = self.folder / "scene.bin"
        self.write_plane()

    def tearDown(self):
        self.temporary.cleanup()

    def write_plane(self, y=0, raised_corner=0):
        self.obj.write_text(
            f"v 0 {y} 0\nv 0 {y} 20\nv 20 {y} 20\nv 20 {y + raised_corner} 0\n"
            "f 1 2 3\nf 1 3 4\n", encoding="ascii")

    def invoke(self, *args, authored=True):
        command = [str(BAKER), "--obj", str(self.obj), "--out", str(self.out)]
        if authored:
            command += ["--profile", "authored-flat"]
        return subprocess.run(command + list(map(str, args)), capture_output=True,
                              text=True, encoding="utf-8", timeout=30)

    def test_original_ground_and_nonzero_planes_keep_their_exact_height(self):
        for y in (0, 2.3, -1.3):
            with self.subTest(y=y):
                self.write_plane(y)
                result = self.invoke("--cs", "0.1", "--ch", "0.2", "--agent-radius", "0.3",
                                     "--agent-height", "1.8", "--agent-climb", "0.3",
                                     "--agent-slope", "45", "--tile-size", "256",
                                     "--min-region-area", "0", "--probe", f"3,{y},6")
                self.assertEqual(0, result.returncode, result.stdout + result.stderr)
                self.assertIn(f"nearest=(3.00,{y:.2f},6.00)", result.stdout)
                self.assertIn("ON MESH", result.stdout)
                self.assertTrue(self.out.is_file())

    def test_diagnostic_raised_foot_point_stays_rejected(self):
        result = self.invoke("--probe", "3,0.2,6")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("OFF MESH", result.stdout)
        self.assertFalse(self.out.exists())

    def test_generic_obj_keeps_legacy_quantized_height(self):
        result = self.invoke("--probe", "3,0,6", authored=False)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("OFF MESH", result.stdout)
        self.assertFalse(self.out.exists())
        result = self.invoke("--probe", "3,0.2,6", authored=False)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn("nearest=(3.00,0.20,6.00)", result.stdout)

    def test_nonplanar_obj_is_rejected_before_output(self):
        self.write_plane(raised_corner=0.001)
        result = self.invoke("--probe", "3,0,6")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("coplanar", result.stderr)
        self.assertFalse(self.out.exists())

    def test_malformed_nonfinite_vertices_and_invalid_indices_are_rejected(self):
        original = self.obj.read_text(encoding="ascii")
        for malformed in (original.replace("v 0 0 0", "v 0 nan 0"),
                          original.replace("v 0 0 0", "v inf 0 0"),
                          original.replace("v 0 0 0", "v 0 0"),
                          original.replace("f 1 2 3", "f 1 2 99"),
                          original.replace("f 1 2 3", "f 1 2 3bad")):
            with self.subTest(malformed=malformed.splitlines()[0]):
                self.obj.write_text(malformed, encoding="ascii")
                result = self.invoke("--probe", "3,0,6")
                self.assertNotEqual(0, result.returncode)
                self.assertIn("OBJ", result.stderr)
                self.assertFalse(self.out.exists())

    def test_profile_requires_explicit_original_spawn_probe(self):
        result = self.invoke()
        self.assertNotEqual(0, result.returncode)
        self.assertIn("requires at least one explicit --probe", result.stderr)
        self.assertFalse(self.out.exists())

    def test_invalid_profile_parameters_fail_without_output(self):
        for args in (("--agent-slope", "-1"), ("--agent-slope", "90"),
                     ("--agent-slope", "nan"), ("--agent-slope", "45trailing"),
                     ("--min-region-area", "-1"), ("--min-region-area", "inf"),
                     ("--merge-region-area", "-1"), ("--tile-size", "16.5"),
                     ("--profile", "unknown"), ("--ch", "0.1")):
            with self.subTest(args=args):
                result = self.invoke(*args, "--probe", "3,0,6")
                self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
                self.assertFalse(self.out.exists())

    def test_profile_reports_shared_and_server_only_parameters(self):
        result = self.invoke("--cs", "0.1", "--agent-radius", "0.3", "--agent-height", "2",
                             "--agent-climb", "0.3", "--agent-slope", "42", "--tile-size", "256",
                             "--min-region-area", "1.25", "--merge-region-area", "2.5",
                             "--probe", "3,0,6")
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        for value in ("profile=authored-flat", "tile_size=256", "agent_slope=42",
                      "min_region_area=1.25", "merge_region_area=2.5", "ground_y=0",
                      "walkable_height_voxels=10", "walkable_climb_voxels=1"):
            self.assertIn(value, result.stdout)

    def test_explicit_slope_changes_generic_ramp_walkability(self):
        self.obj.write_text("v 0 0 0\nv 0 10 20\nv 20 10 20\nv 20 0 0\n"
                            "f 1 2 3\nf 1 3 4\n", encoding="ascii")
        result = self.invoke("--agent-slope", "10", authored=False)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("no tiles built", result.stderr)
        self.assertFalse(self.out.exists())
        result = self.invoke("--agent-slope", "45", authored=False)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertTrue(self.out.is_file())


if __name__ == "__main__":
    unittest.main()

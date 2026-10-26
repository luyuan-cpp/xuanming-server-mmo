"""约束 C++ 的实际构建与源码边界，防止官方 SDK 被生成或链接入口重新带回。"""
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[3]
CPP = ROOT / "cpp"


class CppSdkDependenciesTest(unittest.TestCase):
    def test_no_official_generated_stubs(self):
        self.assertEqual([], list((CPP / "generated").rglob("*.grpc.pb.*")))

    def test_submodules_do_not_include_official_sdk(self):
        modules = (ROOT / ".gitmodules").read_text("utf-8")
        self.assertNotIn("https://github.com/grpc/grpc.git", modules)
        for dependency in ("protobuf", "abseil-cpp", "googletest", "boringssl"):
            self.assertIn(f"path = third_party/{dependency}", modules)


if __name__ == "__main__":
    unittest.main()

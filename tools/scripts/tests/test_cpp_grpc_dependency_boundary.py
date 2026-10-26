"""C++ 官方 gRPC SDK 的依赖边界；不编译、不访问网络或运行服务。"""

import contextlib
import importlib.util
import io
from pathlib import Path
import re
import tempfile
import unittest
import xml.etree.ElementTree as ET


REPO = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location(
    "vcxproj2cmake", REPO / "tools/archived/vcxproj2cmake.py"
)
EXPORTER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EXPORTER)


def official_library(name):
    name = name.strip().lower()
    return (
        name in {
            "address_sorting.lib", "cares.lib", "gpr.lib", "re2.lib",
            "libupb.lib", "libupbd.lib", "utf8_range_lib.lib",
        }
        or name.startswith("upb_")
        or (name.startswith("grpc") and name != "grpc_client.lib")
    )


class CppDependencyBoundaryTest(unittest.TestCase):
    def test_all_project_configurations_and_inherited_targets_drop_official_sdk(self):
        paths = list((REPO / "cpp").rglob("*.vcxproj"))
        paths += list((REPO / "cpp").rglob("*.vcxproj.filters"))
        paths.append(REPO / "Directory.Build.targets")
        self.assertGreater(len(paths), 40, "必须检查整个 C++ 工程树，不能只检查一个节点")
        for path in paths:
            with self.subTest(path=str(path.relative_to(REPO))):
                document = ET.parse(path)
                for element in document.iter():
                    tag = element.tag.rsplit("}", 1)[-1]
                    value = (element.text or "").replace("\\", "/")
                    if tag in {"AdditionalIncludeDirectories", "AdditionalLibraryDirectories"}:
                        self.assertNotRegex(value.lower(), r"third_party/grpc(?:/|;|$)")
                    elif tag == "AdditionalDependencies":
                        self.assertFalse(
                            any(official_library(item) for item in value.split(";")),
                            "任一配置都不能链接官方 SDK；自有 grpc_client.lib 仍允许",
                        )
                    elif tag in {"ClCompile", "ClInclude"}:
                        self.assertNotRegex(element.get("Include", ""), r"\.grpc\.pb\.(?:cc|h)$")

    def test_cpp_source_and_generated_outputs_do_not_use_official_runtime(self):
        for path in (REPO / "cpp").rglob("*"):
            if not path.is_file() or path.suffix not in {".h", ".hpp", ".cpp", ".cc"}:
                continue
            if any(part in {"build", "Debug", "Release", "x64"} for part in path.relative_to(REPO).parts):
                continue
            with self.subTest(path=str(path.relative_to(REPO))):
                self.assertNotRegex(path.name, r"\.grpc\.pb\.(?:cc|h)$")
                text = path.read_text(encoding="utf-8-sig")
                text = re.sub(r"/\*.*?\*/|//[^\n]*", "", text, flags=re.S)
                self.assertNotRegex(text, r'#\s*include\s*[<"]grpc(?:pp)?/')
                self.assertNotRegex(text, r"\bgrpc_(?:init|shutdown|is_initialized)\s*\(")

    def test_independent_windows_installs_match_configuration_and_architecture(self):
        for path in (REPO / "cpp").rglob("*.vcxproj"):
            document = ET.parse(path)
            for group in document.iter():
                if not group.tag.endswith("ItemDefinitionGroup"):
                    continue
                condition = group.get("Condition", "")
                if "Debug" not in condition and "Release" not in condition:
                    continue
                expected = "install_vs2026_"
                expected += "x86_" if "Win32" in condition else ""
                expected += "dbg" if "Debug" in condition else "rel"
                for element in group.iter():
                    if not element.tag.endswith(("AdditionalIncludeDirectories", "AdditionalLibraryDirectories")):
                        continue
                    for dependency, prefix in re.findall(
                        r"third_party/(protobuf|boringssl|googletest)/(install_vs2026_[a-z0-9_]+)(?:/|;|$)",
                        (element.text or "").replace("\\", "/"),
                    ):
                        with self.subTest(path=str(path.relative_to(REPO)), condition=condition, dependency=dependency):
                            self.assertEqual(prefix, expected, "不能将 x64 Debug 库默默链接进其它配置")

    def test_checked_in_linux_projects_do_not_require_official_sdk(self):
        for path in (REPO / "cpp").rglob("CMakeLists.txt"):
            with self.subTest(path=str(path.relative_to(REPO))):
                text = path.read_text(encoding="utf-8-sig")
                self.assertNotRegex(text, r"find_package\s*\(\s*gRPC\b|\bgRPC::")
                self.assertNotRegex(text, r"third_party[/\\]grpc[/\\]|\.grpc\.pb\.cc\b")

    def test_exporter_rejects_sdk_reintroduction_in_inactive_configurations(self):
        for tag, value in [
            ("AdditionalIncludeDirectories", r"..\third_party\grpc\include"),
            ("AdditionalLibraryDirectories", "../third_party/grpc/install_vs2026_dbg/lib"),
            ("AdditionalDependencies", "grpc.lib;grpc_client.lib;muduo_grpc.lib"),
            ("AdditionalDependencies", "gpr.lib"),
            ("AdditionalDependencies", "upb_message_lib.lib"),
        ]:
            with self.subTest(tag=tag, value=value), tempfile.TemporaryDirectory() as directory:
                project = Path(directory) / "fixture.vcxproj"
                project.write_text(
                    '<Project><ItemDefinitionGroup Condition="Release|Win32"><Link>'
                    f"<{tag}>{value}</{tag}>"
                    "</Link></ItemDefinitionGroup></Project>",
                    encoding="utf-8",
                )
                with self.assertRaises(ValueError):
                    EXPORTER.parse_vcxproj(project)
        with tempfile.TemporaryDirectory() as directory:
            project = Path(directory) / "fixture.vcxproj"
            project.write_text('<Project><ItemGroup><ClCompile Include="mail/mail.grpc.pb.cc" /></ItemGroup></Project>', encoding="utf-8")
            with self.assertRaises(ValueError):
                EXPORTER.parse_vcxproj(project)

    def test_exporter_preserves_custom_protocol_and_independent_dependencies(self):
        with tempfile.TemporaryDirectory() as directory:
            project = Path(directory) / "fixture.vcxproj"
            project.write_text(
                '<Project><ItemGroup><ClCompile Include="main.cpp" /></ItemGroup>'
                '<ItemDefinitionGroup><Link><AdditionalDependencies>'
                'grpc_client.lib;muduo_grpc.lib;libprotobufd.lib;crypto.lib'
                '</AdditionalDependencies></Link></ItemDefinitionGroup></Project>',
                encoding="utf-8",
            )
            sources, includes = EXPORTER.parse_vcxproj(project)
            previous = list(EXPORTER.built_libs)
            try:
                EXPORTER.built_libs[:] = ["core", "grpc_client", "muduo_grpc"]
                with contextlib.redirect_stdout(io.StringIO()):
                    EXPORTER.write_cmake(directory, "fixture", sources, includes, "exe")
            finally:
                EXPORTER.built_libs[:] = previous
            text = (Path(directory) / "CMakeLists.txt").read_text(encoding="utf-8")
            self.assertIn("-lgrpc_client", text)
            self.assertIn("-lmuduo_grpc", text)
            self.assertIn("protobuf::libprotobuf", text)
            self.assertIn("OpenSSL::Crypto", text)
            self.assertIn("/usr/local/lib/cmake/OpenSSL NO_DEFAULT_PATH", text)
            self.assertNotRegex(text, r"find_package\s*\(\s*gRPC\b|\bgRPC::")


if __name__ == "__main__":
    unittest.main()

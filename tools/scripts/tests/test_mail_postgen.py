import contextlib
import io
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import mail_postgen
import rank_postgen


class ProjectRegistrationTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.repo_patch = patch.object(mail_postgen, "REPO", self.root)
        self.repo_patch.start()
        self.addCleanup(self.repo_patch.stop)

    def write(self, name, text, bom=False):
        path = self.root / name
        path.write_bytes((b"\xef\xbb\xbf" if bom else b"") + text.encode("utf-8"))
        return path

    def save(self, edit):
        with contextlib.redirect_stdout(io.StringIO()):
            edit.save(False)

    def test_merged_registrations_keep_original_bytes_and_unique_sources(self):
        for newline, bom in (("\n", False), ("\r\n", False), ("\r\n", True)):
            with self.subTest(newline=repr(newline), bom=bom):
                original = newline.join([
                    '<Project>',
                    '  <!-- 保留已有登记和独有产物 -->',
                    '  <ItemGroup>',
                    mail_postgen.compile_line('mail/mail.pb.cc'),
                    '  </ItemGroup>',
                    '  <ItemGroup Label="ProtoGeneratorSources">',
                    mail_postgen.compile_line('mail/mail.pb.cc'),
                    mail_postgen.compile_line('mail/mail_table.pb.cc'),
                    '  </ItemGroup>',
                    '</Project>',
                    '',
                ])
                path = self.write('proto.vcxproj', original, bom)
                edit = mail_postgen.ProjectEdit(path.name)
                edit.plan([], [mail_postgen.compile_entry('mail/mail.pb.cc')])
                self.save(edit)
                duplicate = mail_postgen.compile_line('mail/mail.pb.cc') + newline
                position = original.rindex(duplicate)
                expected = original[:position] + original[position + len(duplicate):]
                expected_bytes = (b"\xef\xbb\xbf" if bom else b"") + expected.encode('utf-8')
                self.assertEqual(path.read_bytes(), expected_bytes, '合并后的重复编译项必须消除，其余字节不变')
                second = mail_postgen.ProjectEdit(path.name)
                second.plan([], [mail_postgen.compile_entry('mail/mail.pb.cc')])
                self.save(second)
                self.assertEqual(path.read_bytes(), expected_bytes, '重复运行必须零变化')

    def test_cmake_only_deduplicates_the_source_list(self):
        text = '# 保留构建参数\nset(SOURCE_FILES\n    mail/mail.pb.cc\n    mail/mail.pb.cc\n)\nset(OTHER_FILES\n    mail/mail.pb.cc\n)\n'
        path = self.write('CMakeLists.txt', text)
        edit = mail_postgen.ProjectEdit(path.name)
        edit.plan([], [mail_postgen.cmake_entry('mail/mail.pb.cc')])
        self.save(edit)
        self.assertEqual(path.read_text(encoding='utf-8'), text.replace('    mail/mail.pb.cc\n', '', 1))

    def test_filters_keep_the_original_unfiltered_entry(self):
        line = mail_postgen.compile_line('mail/mail_grpc_client.cpp')
        filtered = '\n'.join(mail_postgen.filter_lines('ClCompile', 'mail/mail_grpc_client.cpp', mail_postgen.SRC))
        text = '<Project>\n  <ItemGroup>\n' + line + '\n' + filtered + '\n  </ItemGroup>\n</Project>\n'
        path = self.write('grpc_client.vcxproj.filters', text)
        edit = mail_postgen.ProjectEdit(path.name)
        edit.plan([], [mail_postgen.compile_entry('mail/mail_grpc_client.cpp')])
        self.save(edit)
        self.assertEqual(path.read_text(encoding='utf-8'), text.replace(filtered + '\n', ''))

    def test_namespaced_header_entries_keep_the_original_bytes(self):
        line = mail_postgen.include_line('mail/mail.pb.h')
        text = ('<?xml version="1.0" encoding="utf-8"?>\r\n'
                '<Project xmlns="http://schemas.microsoft.com/developer/msbuild/2003">\r\n'
                '  <ItemGroup>\r\n' + line + '\r\n' + line + '\r\n'
                '  </ItemGroup>\r\n</Project>\r\n')
        path = self.write('proto.vcxproj', text)
        edit = mail_postgen.ProjectEdit(path.name)
        edit.plan([], [mail_postgen.include_entry('mail/mail.pb.h')])
        self.save(edit)
        self.assertEqual(path.read_bytes(), text.replace(line + '\r\n', '', 1).encode('utf-8'))

    def test_missing_registration_is_added_once(self):
        friend = mail_postgen.compile_line('friend/friend.pb.cc')
        mail = mail_postgen.compile_line('mail/mail.pb.cc')
        text = '<Project>\n  <ItemGroup>\n' + friend + '\n  </ItemGroup>\n</Project>\n'
        path = self.write('proto.vcxproj', text)
        expected = text.replace(friend + '\n', friend + '\n' + mail + '\n')
        for _ in range(2):
            edit = mail_postgen.ProjectEdit(path.name)
            edit.plan([friend], [mail_postgen.compile_entry('mail/mail.pb.cc')])
            self.save(edit)
            self.assertEqual(path.read_bytes(), expected.encode('utf-8'))

    def test_dry_run_preserves_duplicate_input(self):
        line = mail_postgen.compile_line('mail/mail.pb.cc')
        text = '<Project>\n  <ItemGroup>\n' + line + '\n' + line + '\n  </ItemGroup>\n</Project>\n'
        path = self.write('proto.vcxproj', text)
        edit = mail_postgen.ProjectEdit(path.name)
        edit.plan([], [mail_postgen.compile_entry('mail/mail.pb.cc')])
        with contextlib.redirect_stdout(io.StringIO()):
            edit.save(True)
        self.assertEqual(path.read_bytes(), text.encode('utf-8'))

    def test_conflicting_build_metadata_fails_before_writing(self):
        for suffix in (' Condition="true" />', '><ExcludedFromBuild>true</ExcludedFromBuild></ClCompile>'):
            with self.subTest(suffix=suffix):
                key = '<ClCompile Include="mail\\mail.pb.cc"'
                text = '<Project><ItemGroup>\n    ' + key + ' />\n    ' + key + suffix + '\n</ItemGroup></Project>'
                path = self.write('proto.vcxproj', text)
                before = path.read_bytes()
                edit = mail_postgen.ProjectEdit(path.name)
                with self.assertRaises(SystemExit, msg='不同编译语义不能静默丢弃'):
                    edit.plan([], [mail_postgen.compile_entry('mail/mail.pb.cc')])
                self.assertEqual(path.read_bytes(), before)

    def test_conditional_group_is_not_flattened(self):
        line = mail_postgen.compile_line('mail/mail.pb.cc')
        text = '<Project><ItemGroup>\n' + line + '\n</ItemGroup><ItemGroup Condition="true">\n' + line + '\n</ItemGroup></Project>'
        path = self.write('proto.vcxproj', text)
        edit = mail_postgen.ProjectEdit(path.name)
        with self.assertRaises(SystemExit):
            edit.plan([], [mail_postgen.compile_entry('mail/mail.pb.cc')])
        self.assertEqual(path.read_bytes(), text.encode('utf-8'))

    def test_registration_preserves_unrelated_domains(self):
        friend = mail_postgen.compile_line('friend/friend.pb.cc')
        mail = mail_postgen.compile_line('mail/mail.pb.cc')
        text = '<Project>\n<ItemGroup>\n' + friend + '\n' + friend + '\n' + mail + '\n</ItemGroup>\n</Project>\n'
        path = self.write('proto.vcxproj', text)
        edit = mail_postgen.ProjectEdit(path.name)
        edit.plan([], [mail_postgen.compile_entry('mail/mail.pb.cc')])
        self.save(edit)
        self.assertEqual(path.read_bytes(), text.encode('utf-8'))

    def test_mail_and_rank_register_without_official_grpc_stubs(self):
        for module, domain, names in (
            (mail_postgen, 'mail', ('mail', 'mail_admin', 'mail_table')),
            (rank_postgen, 'rank', rank_postgen.PROTO_STEMS),
        ):
            with self.subTest(domain=domain), patch.object(module, 'REPO', self.root):
                base = self.root / 'cpp/generated/proto'
                (base / domain).mkdir(parents=True, exist_ok=True)
                for name in names:
                    for extension in ('.pb.cc', '.pb.h'):
                        (base / domain / (name + extension)).write_text('', encoding='utf-8')
                (base / 'CMakeLists.txt').write_text('set(SOURCE_FILES\n    friend/friend.pb.cc\n)\n', encoding='utf-8')
                xml = ('<Project>\n  <ItemGroup>\n'
                       '    <ClCompile Include="friend\\friend.pb.cc" />\n'
                       '    <ClInclude Include="friend\\friend.pb.h" />\n'
                       '  </ItemGroup>\n</Project>\n')
                (base / 'proto.vcxproj').write_text(xml, encoding='utf-8')
                filters = ('<Project>\n  <ItemGroup>\n' +
                           '\n'.join(mail_postgen.filter_lines('ClCompile', 'friend/friend.pb.cc', mail_postgen.SRC)) + '\n' +
                           '\n'.join(mail_postgen.filter_lines('ClInclude', 'friend/friend.pb.h', mail_postgen.HDR)) +
                           '\n  </ItemGroup>\n</Project>\n')
                (base / 'proto.vcxproj.filters').write_text(filters, encoding='utf-8')
                edits, missing = [], []
                module.plan_proto(edits, missing)
                self.assertEqual(missing, [], '只应要求普通 Protobuf 消息产物')
                for edit in edits:
                    self.save(edit)
                for filename in ('CMakeLists.txt', 'proto.vcxproj', 'proto.vcxproj.filters'):
                    text = (base / filename).read_text(encoding='utf-8')
                    self.assertNotIn('.grpc.pb.', text)
                    self.assertIn(domain + ('/' if filename == 'CMakeLists.txt' else '\\'), text)


if __name__ == '__main__':
    unittest.main()

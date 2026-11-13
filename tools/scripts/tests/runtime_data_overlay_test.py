"""离线核验显式开发表的来源、挂载和产物权限；不调用 Docker。"""
import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import rebuild_docker_debug as subject


class RuntimeDataOverlayTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.tables = self.root / 'tables'
        self.tables.mkdir()
        (self.tables / 'basescene.json').write_text('{"rows":[]}')
        artifact = dict(file='basescene.json', size=(self.tables / 'basescene.json').stat().st_size,
                        sha256=subject.digest(self.tables / 'basescene.json'))
        self.delivery = self.root / 'delivery.json'
        self.write(self.delivery, dict(maps=[]))
        self.books = {}
        for name in ('BaseScene.xlsx', 'World.xlsx'):
            path = self.root / name
            path.write_bytes(b'copied workbook fixture')
            self.books[name] = dict(path=str(path), sha256=subject.digest(path))
        self.write(self.tables / 'manifest.json', dict(table_count=2, tables=[
            dict(source=dict(file=name, sha256=item['sha256']), artifacts=[artifact] if index == 0 else [])
            for index, (name, item) in enumerate(self.books.items())]))
        self.hashes = {p.name: subject.digest(p) for p in self.tables.iterdir()}
        self.report = self.root / 'export.json'
        self.report_value = dict(developmentOnly=True, productionTravelEnabled=False,
                                 exportDeliverySha256=subject.digest(self.delivery), generatedTables=str(self.tables),
                                 officialTableManifestSha256=self.hashes['manifest.json'], tableSha256=self.hashes,
                                 sourceWorkbookSha256={n: v['sha256'] for n, v in self.books.items()})
        self.write(self.report, self.report_value)
        self.value = dict(schemaVersion=1, developmentOnly=True, productionTravelEnabled=False,
                          exportDelivery=dict(path=str(self.delivery), sha256=subject.digest(self.delivery)),
                          sourceExport=dict(path=str(self.report), sha256=subject.digest(self.report)),
                          sourceWorkbooks=self.books, tableDirectory=str(self.tables), tableSha256=self.hashes,
                          officialTableManifestSha256=self.hashes['manifest.json'],
                          services={'login': dict(source=str(self.tables), target='/generated/tables', readOnly=True)})
        self.manifest = self.root / 'runtime.json'
        self.save()

    @staticmethod
    def write(path, value):
        path.write_text(json.dumps(value), encoding='utf-8')

    def save(self):
        self.write(self.manifest, self.value)

    def test_complete_source_and_official_table_manifest_required(self):
        loaded = subject.load_runtime_data_manifest(self.manifest)
        self.assertEqual(loaded['tableSha256'], self.hashes)
        (self.tables / 'basescene.json').write_text('changed')
        with self.assertRaises(RuntimeError):
            subject.load_runtime_data_manifest(self.manifest)

    def test_missing_bad_and_non_development_manifests_are_rejected(self):
        with self.assertRaises(RuntimeError):
            subject.load_runtime_data_manifest(self.root / 'missing.json')
        for value in ({}, {**self.value, 'developmentOnly': False}, {**self.value, 'sourceWorkbooks': {}}):
            self.write(self.manifest, value)
            with self.assertRaises(RuntimeError):
                subject.load_runtime_data_manifest(self.manifest)

    def test_source_binding_and_unauthorized_paths_are_rejected(self):
        for change in ('source', 'target', 'table'):
            with self.subTest(change=change):
                value = copy.deepcopy(self.value)
                if change == 'source':
                    value['exportDelivery']['sha256'] = '0' * 64
                elif change == 'target':
                    value['services']['login']['target'] = '/app/service'
                else:
                    value['tableSha256']['../service'] = '0' * 64
                self.write(self.manifest, value)
                with self.assertRaises(RuntimeError):
                    subject.load_runtime_data_manifest(self.manifest)

    def test_only_exact_readonly_bind_can_override_table_artifacts(self):
        data = subject.load_runtime_data_manifest(self.manifest)
        binary = {'/app/service': 'binary-sha', '/app/service.debug': 'symbol-sha'}
        mount = dict(Type='bind', Source=str(self.tables), Destination='/generated/tables', RW=False)
        with patch.object(subject, 'execute', return_value=json.dumps([mount])):
            result = subject.runtime_table_artifacts('login', 'container', binary, data)
        self.assertEqual({k: result[k] for k in binary}, binary)
        self.assertEqual(set(result) - set(binary), {'/generated/tables/basescene.json', '/generated/tables/manifest.json'})
        for bad in ({**mount, 'RW': True}, {**mount, 'Source': str(self.root)}, {**mount, 'Type': 'volume'}):
            with patch.object(subject, 'execute', return_value=json.dumps([bad])):
                with self.assertRaises(RuntimeError):
                    subject.runtime_table_artifacts('login', 'container', binary, data)
        self.assertIs(subject.runtime_table_artifacts('login', 'container', binary, None), binary)
        self.assertIs(subject.runtime_table_artifacts('db', 'container', binary, data), binary)

    def test_default_check_still_checks_original_sources_and_artifacts(self):
        artifact_path, artifact_sha = '/app/service', 'a' * 64
        image_manifest = dict(schema=1, language='go', files={'go/login/main.go': 'source-sha'},
                              artifacts={artifact_path: artifact_sha})

        def execute(args, **kwargs):
            if 'ps' in args:
                return 'container\n'
            if 'inspect' in args:
                return json.dumps(dict(Image='image-id', State=dict(Running=True)))
            if 'cat' in args:
                return json.dumps(image_manifest)
            if 'sha256sum' in args:
                return artifact_sha + '  ' + artifact_path + '\n'
            raise AssertionError(args)

        with patch.object(subject, 'execute', side_effect=execute), patch.object(subject, 'check_config_inputs'), \
                patch.object(subject, 'current_sources', return_value={'go/login/main.go': 'source-sha'}):
            self.assertTrue(subject.check(['login']))
        with patch.object(subject, 'execute', side_effect=execute), patch.object(subject, 'check_config_inputs'), \
                patch.object(subject, 'current_sources', return_value={'go/login/main.go': 'changed-source'}):
            self.assertFalse(subject.check(['login']))

    def test_supplemental_tip_text_cannot_replace_compiled_content(self):
        data = subject.load_runtime_data_manifest(self.manifest)
        data['supplementalTableSha256'] = {'tip_text.json': 'new-sha'}
        mount = dict(Type='bind', Source=str(self.tables), Destination='/generated/tables', RW=False)
        with patch.object(subject, 'execute', return_value=json.dumps([mount])):
            with self.assertRaisesRegex(RuntimeError, 'compiled image'):
                subject.runtime_table_artifacts('login', 'container', {'/generated/tables/tip_text.json': 'old-sha'}, data)


if __name__ == '__main__':
    unittest.main()

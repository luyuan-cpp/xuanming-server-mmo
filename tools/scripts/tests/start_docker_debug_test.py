"""离线复现手绘节庆部署后日常启动的挂载校验。"""
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import start_docker_debug as startup


@unittest.skipUnless(os.name == 'nt', 'Windows Docker Desktop bind paths')
class AuthoredComposeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        root_patch = patch.object(startup, 'ROOT', self.root)
        root_patch.start()
        self.addCleanup(root_patch.stop)
        self.batch = self.root / 'build/verification/authored-festival-test'
        self.tables = self.batch / 'tables/generated/tables'
        self.nav = self.root / 'data/scene_nav_bin/authored_maps'
        self.staged = self.root / 'build/docker-debug-current'
        for path in (self.tables, self.nav, self.staged):
            path.mkdir(parents=True)
            (path / 'test.bin').write_bytes(b'fixture')
        self.overlay = self.batch / 'compose.authored.json'
        self.mounts = {
            name: [(self.tables, '/app/generated/generated_tables'),
                   (self.nav, '/app/data/scene_nav_bin/authored_maps')]
            for name in ('gate', 'scene', 'battle')
        }
        self.mounts.update({name: [(self.tables, '/generated/tables')]
                            for name in ('login', 'scene-manager', 'player-locator')})
        self.config = {'services': {name: {'volumes': [
            dict(type='bind', source=source.as_posix(), target=target, read_only=True)
            for source, target in mounts]} for name, mounts in self.mounts.items()}}
        self.save_overlay()

    def save_overlay(self):
        self.overlay.write_text(json.dumps(self.config), encoding='utf-8')

    def container(self, name, mounts, *, overlay=True):
        files = [*startup.COMPOSE_FILES, self.overlay] if overlay else startup.COMPOSE_FILES
        return {'Config': {'Labels': {'com.docker.compose.project': startup.PROJECT,
                                     'com.docker.compose.service': name,
                                     'com.docker.compose.project.config_files': ','.join(map(str, files))}},
                'Mounts': mounts}

    @staticmethod
    def bind(source, target, *, writable=False):
        return dict(Type='bind', Source=source, Destination=target, RW=writable)

    def test_deployed_overlay_is_used_and_its_read_only_sources_are_hashed(self):
        mapped = '/run/desktop/mnt/host/' + self.tables.as_posix()[0].lower() + self.tables.as_posix()[2:]
        rows = [self.container('gate', [self.bind(mapped, '/app/generated/generated_tables'),
                                       self.bind(str(self.nav), '/app/data/scene_nav_bin/authored_maps')]),
                self.container('mysql', [self.bind(str(self.staged), '/mysql-config')], overlay=False)]
        overlay, content, compose_input, allowed = startup.authored_overlay(rows)
        self.assertEqual(overlay, self.overlay)
        self.assertEqual(content, self.overlay.read_bytes())
        self.assertEqual(json.loads(compose_input), self.config)
        hashes = startup.binding_hashes(rows, allowed)
        self.assertEqual(len(hashes), 3)
        (self.tables / 'test.bin').write_bytes(b'changed')
        self.assertNotEqual(startup.binding_hashes(rows, allowed), hashes)

    def test_no_overlay_preserves_the_staging_boundary(self):
        rows = [self.container('gate', [self.bind(str(self.staged), '/config')], overlay=False)]
        self.assertEqual(startup.authored_overlay(rows), (None, None, None, {}))
        self.assertEqual(len(startup.binding_hashes(rows)), 1)
        rows[0]['Mounts'][0]['Source'] = str(self.tables)
        with self.assertRaisesRegex(RuntimeError, 'Bind source outside staged directory'):
            startup.binding_hashes(rows)

    def test_overlay_cannot_add_other_compose_settings(self):
        self.config['services']['gate']['entrypoint'] = ['/unexpected']
        self.save_overlay()
        with self.assertRaisesRegex(RuntimeError, 'Unauthorized authored Compose overlay'):
            startup.authored_overlay([self.container('gate', [])])

    def test_overlay_outside_the_authored_batch_is_rejected(self):
        self.overlay = self.staged / self.overlay.name
        self.save_overlay()
        with self.assertRaisesRegex(RuntimeError, 'Unexpected authored Compose overlay path'):
            startup.authored_overlay([self.container('gate', [])])

    def test_overlay_sources_are_bound_to_the_service_target_and_read_only_mode(self):
        target = '/app/generated/generated_tables'
        _, _, _, allowed = startup.authored_overlay([self.container('gate', [])])
        for name, mount in [('mysql', self.bind(str(self.tables), target)),
                            ('gate', self.bind(str(self.tables), '/unexpected')),
                            ('gate', self.bind(str(self.tables), target, writable=True)),
                            ('gate', self.bind(str(self.nav), target))]:
            with self.subTest(name=name, mount=mount):
                with self.assertRaisesRegex(RuntimeError, 'Bind source outside staged directory'):
                    startup.binding_hashes([self.container(name, [mount])], allowed)


if __name__ == '__main__':
    unittest.main()

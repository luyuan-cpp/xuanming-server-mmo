"""验证 RPC 桥的回执、实时身份和端口边界，测试不访问 Docker 或游戏。"""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch

spec = importlib.util.spec_from_file_location('gm_rpc_bridge_under_test', Path(__file__).with_name('rpc-bridge.py'))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class RpcBridgeReadinessTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / 'private').mkdir()
        (self.root / 'context').mkdir()
        (self.root / 'context/bridge').write_bytes(b'compiled-bridge')
        self.row = {'Id': 'bridge-id', 'Image': 'sha256:bridge', 'Name': '/' + bridge.NAME, 'RestartCount': 0,
            'Config': {'Labels': {'com.docker.compose.project': bridge.PROJECT, 'com.docker.compose.service': 'bridge',
                'com.docker.compose.project.config_files': str(bridge.COMPOSE)},
                'User': '10001:10001', 'Entrypoint': ['/app/gm-rpc-bridge'], 'Cmd': None},
            'HostConfig': {'ReadonlyRootfs': True}, 'Mounts': [],
            'State': {'Status': 'running', 'Health': {'Status': 'healthy'}, 'ExitCode': 0, 'StartedAt': 'start',
                'FinishedAt': '', 'OOMKilled': False},
            'NetworkSettings': {'Networks': {bridge.NETWORK: {}}, 'Ports': {
                '50901/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '2356'}],
                '50902/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '2357'}]}}}
        self.upstream = {'data-service': {'id': 'data-id'}, 'player-locator': {'id': 'locator-id'}}
        self.receipt = {'schema': 1, 'passed': True, 'targets': bridge.TARGETS, 'sources': {'main.go': 'hash'},
            'binarySha256': bridge.startup.sha(b'compiled-bridge'), 'imageId': self.row['Image'],
            'upstreams': self.upstream, 'bridge': bridge.bridge_snapshot(self.row)}
        self.write_receipt()
        for context in (patch.object(bridge, 'OUT', self.root),
                patch.object(bridge, 'source_hashes', return_value={'main.go': 'hash'}),
                patch.object(bridge, 'upstreams', return_value=self.upstream),
                patch.object(bridge.startup, 'inspect', return_value=[self.row]),
                patch.object(bridge.socket, 'create_connection', return_value=MagicMock())):
            context.start()
            self.addCleanup(context.stop)

    def write_receipt(self):
        raw = json.dumps(self.receipt).encode()
        (self.root / 'ready.json').write_bytes(raw)
        (self.root / 'private/ready.sha256').write_text(bridge.startup.sha(raw), encoding='ascii')

    def test_verified_bridge_returns_only_two_fixed_read_targets(self):
        self.assertEqual(bridge.TARGETS, bridge.verify_ready())

    def test_changed_receipt_is_rejected(self):
        (self.root / 'ready.json').write_text('{}')
        with self.assertRaises(RuntimeError): bridge.verify_ready()

    def test_replaced_upstream_is_rejected(self):
        bridge.upstreams.return_value = {'data-service': {'id': 'replacement'}}
        with self.assertRaises(RuntimeError): bridge.verify_ready()

    def test_bridge_restart_is_rejected(self):
        self.row['State']['StartedAt'] = 'restarted'
        with self.assertRaises(RuntimeError): bridge.verify_ready()

    def test_wildcard_public_port_is_rejected(self):
        self.row['NetworkSettings']['Ports']['50901/tcp'][0]['HostIp'] = '0.0.0.0'
        with self.assertRaises(RuntimeError): bridge.verify_ready()

    def test_changed_binary_is_rejected(self):
        (self.root / 'context/bridge').write_bytes(b'other')
        with self.assertRaises(RuntimeError): bridge.verify_ready()

    def test_changed_sources_are_rejected(self):
        bridge.source_hashes.return_value = {'main.go': 'changed'}
        with self.assertRaises(RuntimeError): bridge.verify_ready()

    def test_unhealthy_bridge_is_rejected(self):
        self.row['State']['Health']['Status'] = 'unhealthy'
        with self.assertRaises(RuntimeError): bridge.verify_ready()


if __name__ == '__main__':
    unittest.main()

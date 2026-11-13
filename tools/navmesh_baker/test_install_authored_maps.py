#!/usr/bin/env python3
"""手绘导航导入器的身份、参数及原子安装回归。"""

import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import install_authored_maps as installer


class AuthoredMapInstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.export = self.root / "export"
        self.export.mkdir()
        maps = []
        for key, scene in installer.MAPS.items():
            folder = self.export / key
            folder.mkdir()
            nav = dict(format="hand-authored-navmesh-v2", mapKey=key, spawnSet=True, spawn=[5, 0, 5],
                       worldRect=dict(x=0, z=0, width=10, depth=10),
                       agent=dict(radius=.4, height=1.8, climb=.3, slope=45, voxelSize=.1),
                       bakeContract=dict(revision="authored-flat-v2", serverProfile="authored-flat",
                                         tileSizeVoxels=256, minRegionAreaWorld=0, unity=dict(heightMesh=True)))
            args = ["--obj", "world-y-up.obj", "--out", "scene-nav.bin", "--profile", "authored-flat",
                    "--cs", ".1", "--ch", ".2", "--agent-radius", ".4", "--agent-height", "1.8",
                    "--agent-climb", ".3", "--agent-slope", "45", "--tile-size", "256",
                    "--min-region-area", "0", "--filter-ledges", "1", "--probe", "5,0,5"]
            (folder / "world-y-up.obj").write_text("v 0 0 0\nv 10 0 0\nv 10 0 10\nf 1 3 2\n")
            self.write(folder / "navigation.json", nav)
            self.write(folder / "server-bake-args.json", args)
            self.write(folder / "source.json", dict(mapKey=key, agent=nav["agent"], bakeContract=nav["bakeContract"]))
            self.resign(folder)
            maps.append(dict(mapKey=key, sceneId=scene, sceneConfigId=scene, exportDirectory=str(folder),
                             isBakeCurrent=True, agent=nav['agent'], bakeContract=nav['bakeContract'],
                             worldRect=nav['worldRect'], spawnUnity=nav['spawn'], **self.delivery_hashes(folder)))
        self.write(self.export / "delivery.json", dict(maps=maps))

    def tearDown(self):
        self.temporary.cleanup()

    @staticmethod
    def write(path, value):
        path.write_text(json.dumps(value), encoding="utf-8")

    def resign(self, folder):
        source = installer.document(folder / "source.json")
        for name, field in (("world-y-up.obj", "geometrySha256"), ("navigation.json", "authoringSha256"),
                            ("server-bake-args.json", "serverBakeArgumentsSha256")):
            source[field] = installer.digest(folder / name)
        self.write(folder / "source.json", source)
        if (self.export / 'delivery.json').exists():
            delivery = installer.document(self.export / 'delivery.json')
            for item in delivery['maps']:
                if item['mapKey'] == folder.name:
                    item.update(self.delivery_hashes(folder))
            self.write(self.export / 'delivery.json', delivery)

    @staticmethod
    def delivery_hashes(folder):
        return {field: installer.digest(folder / name) for name, field in
                [('world-y-up.obj', 'objSha256'), ('navigation.json', 'authoringJsonSha256'),
                 ('source.json', 'sourceJsonSha256'), ('server-bake-args.json', 'serverBakeArgumentsSha256')]}

    def test_exact_two_scene_mapping_and_source_sha(self):
        _, packages = installer.load_delivery(self.export)
        self.assertEqual([p["sceneConfigId"] for p in packages], [26000, 26008])
        (packages[0]["folder"] / "world-y-up.obj").write_text("v 0 0 0\n")
        with self.assertRaisesRegex(ValueError, "source SHA mismatch"):
            installer.load_delivery(self.export)

    def test_signed_non_flat_geometry_and_parameter_mismatch_are_rejected(self):
        folder = self.export / "26000-mid-autumn"
        path = folder / "world-y-up.obj"
        path.write_text(path.read_text().replace("v 0 0 0", "v 0 0.2 0"))
        self.resign(folder)
        with self.assertRaisesRegex(ValueError, "not original flat ground"):
            installer.load_delivery(self.export)
        path.write_text(path.read_text().replace("v 0 0.2 0", "v 0 0 0"))
        args = installer.document(folder / "server-bake-args.json")
        args[args.index("--agent-radius") + 1] = "0.5"
        self.write(folder / "server-bake-args.json", args)
        self.resign(folder)
        with self.assertRaisesRegex(ValueError, "Shared argument mismatch"):
            installer.load_delivery(self.export)

    def candidate(self):
        evidence = self.root / "evidence"
        evidence.mkdir()
        items = []
        for key, scene in installer.MAPS.items():
            folder = evidence / "candidate" / str(scene)
            folder.mkdir(parents=True)
            for name in installer.SOURCE_FILES:
                (folder / name).write_bytes((self.export / key / name).read_bytes())
            (folder / "scene.bin").write_bytes(b"MSET-test")
            (folder / "scene.bin.authored").write_text("ANAV 1\n0 0 10 10 0\n")
            navsha, boundsha = installer.digest(folder / "scene.bin"), installer.digest(folder / "scene.bin.authored")
            (folder / "scene.bin.sha256").write_text(navsha + "\n" + boundsha + "\n")
            self.write(folder / "delivery-source.json", dict(mapKey=key))
            items.append(dict(mapKey=key, sceneConfigId=scene, candidateDirectory=str(folder),
                              sourceFileSha256={n: installer.digest(folder / n) for n in installer.SOURCE_FILES},
                              navigationSha256=navsha, boundsSha256=boundsha, sourceAsset=dict(mapKey=key)))
        return evidence, dict(result="PASS", installed=False, maps=items)

    def test_install_verifies_candidate_and_rolls_back_partial_failure(self):
        evidence, receipt = self.candidate()
        output = self.root / "data" / "authored_maps"
        old = output / "26000" / "scene.bin"
        old.parent.mkdir(parents=True)
        old.write_bytes(b"original navigation")
        original = installer.atomic_bytes
        calls = 0

        def fail_once(path, data, owned):
            nonlocal calls
            calls += 1
            if calls == 6:
                raise OSError("simulated disk failure")
            original(path, data, owned)

        with patch.object(installer, "atomic_bytes", side_effect=fail_once):
            with self.assertRaisesRegex(OSError, "simulated disk failure"):
                installer.install_navigation(receipt, evidence, output)
        self.assertEqual(old.read_bytes(), b"original navigation")
        self.assertEqual([p.name for p in old.parent.iterdir()], ["scene.bin"])
        self.assertFalse(receipt["installed"])

    def test_candidate_tamper_rejected_before_any_output(self):
        evidence, receipt = self.candidate()
        Path(receipt["maps"][1]["candidateDirectory"], "scene.bin").write_bytes(b"changed")
        output = self.root / "data" / "authored_maps"
        with self.assertRaisesRegex(ValueError, "Candidate changed"):
            installer.install_navigation(receipt, evidence, output)
        self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()

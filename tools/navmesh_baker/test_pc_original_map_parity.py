"""Unit tests for server PC original static background parity audit."""

from __future__ import annotations

import json
from pathlib import Path
import shutil
import sys
import tempfile
import unittest

TOOLS_DIR = Path(__file__).resolve().parent
if str(TOOLS_DIR) not in sys.path:
    sys.path.insert(0, str(TOOLS_DIR))

import verify_pc_original_map_parity as parity


class PcOriginalMapParityTests(unittest.TestCase):
    def test_committed_manifest_passes_server_and_client_parity(self) -> None:
        client_catalog = parity.DEFAULT_CLIENT_CATALOG
        result = parity.verify_manifest(
            parity.SERVER_ROOT,
            client_catalog if client_catalog.is_file() else None,
        )
        self.assertEqual(result["result"], "PASS")
        self.assertEqual(result["readyCount"], 36)

    def test_tampered_production_travel_or_gap_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            fake_server = Path(tmp) / "server"
            for rel in (
                parity.MANIFEST_REL,
                parity.BASESCENE_JSON_REL,
                parity.WORLD_JSON_REL,
                parity.FESTIVAL_MANIFEST_REL,
                parity.AUTHORED_MANIFEST_REL,
            ):
                dst = fake_server / rel
                dst.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(parity.SERVER_ROOT / rel, dst)

            self.assertEqual(parity.verify_manifest(fake_server)["result"], "PASS")

            # Tamper 1: mark productionTravelEnabled=True on a map
            manifest_path = fake_server / parity.MANIFEST_REL
            doc = parity.read_json(manifest_path)
            doc["maps"][0]["productionTravelEnabled"] = True
            manifest_path.write_bytes(parity.json_bytes(doc))
            with self.assertRaises(ValueError):
                parity.verify_manifest(fake_server)

            # Tamper 2: mark an unavailable gap as integrated=True
            shutil.copy2(parity.SERVER_ROOT / parity.MANIFEST_REL, manifest_path)
            doc = parity.read_json(manifest_path)
            doc["unavailableMaps"][0]["integrated"] = True
            manifest_path.write_bytes(parity.json_bytes(doc))
            with self.assertRaises(ValueError):
                parity.verify_manifest(fake_server)

            # Tamper 3: mark a HOLD candidate as integrated=True
            shutil.copy2(parity.SERVER_ROOT / parity.MANIFEST_REL, manifest_path)
            doc = parity.read_json(manifest_path)
            doc["specialHoldCandidates"][0]["integrated"] = True
            manifest_path.write_bytes(parity.json_bytes(doc))
            with self.assertRaises(ValueError):
                parity.verify_manifest(fake_server)

            # Unrelated BaseScene edit (e.g. scene 99) must not trigger false-positive failure
            shutil.copy2(parity.SERVER_ROOT / parity.MANIFEST_REL, manifest_path)
            bs_path = fake_server / parity.BASESCENE_JSON_REL
            bs = parity.read_json(bs_path)
            bs["data"].append({"id": 99, "nav_bin_file": "other.bin", "spawn_x": 0.0, "spawn_y": 0.0, "spawn_z": 0.0})
            bs_path.write_bytes(parity.json_bytes(bs))
            self.assertEqual(parity.verify_manifest(fake_server)["result"], "PASS")

            # Tamper 4: inject a conflicting BaseScene id (5000) without updating audit
            bs["data"].append({"id": 5000, "nav_bin_file": "fake.bin", "spawn_x": 0.0, "spawn_y": 0.0, "spawn_z": 0.0})
            bs_path.write_bytes(parity.json_bytes(bs))
            with self.assertRaises(ValueError):
                parity.verify_manifest(fake_server)


if __name__ == "__main__":
    unittest.main()

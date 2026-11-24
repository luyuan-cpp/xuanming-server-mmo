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

        manifest = parity.read_json(parity.SERVER_ROOT / parity.MANIFEST_REL)
        m28002 = next(m for m in manifest["maps"] if m["mapId"] == "28002")
        self.assertEqual(m28002["artStage"], "qdao_redrawn_static_back")
        self.assertFalse(m28002["isTemporaryOriginal"])
        self.assertTrue(m28002["clientHasAlpha"])
        self.assertEqual(m28002["clientDefaultAppearance"], "spring")
        self.assertEqual([a["id"] for a in m28002["clientAppearances"]], ["spring", "midautumn"])
        self.assertEqual(
            m28002["pcOriginalSourceNativeSha256"],
            "d4f9e5d5556fb8bccc0ef27f112e956b253e35daa4965641a87a47f32a81748f",
        )
        self.assertFalse(m28002["serverBaseSceneRegistered"])
        self.assertFalse(m28002["serverWorldRegistered"])
        self.assertFalse(m28002["productionTravelEnabled"])
        self.assertEqual(m28002["serverNavigationStatus"], "UNPROVEN_STATIC_BACK_ONLY")

        m08001 = next(m for m in manifest["maps"] if m["mapId"] == "08001")
        self.assertEqual(m08001["artStage"], "pc_original_static_back")
        self.assertTrue(m08001["isTemporaryOriginal"])
        self.assertEqual(m08001["clientDefaultAppearance"], "normal")
        self.assertEqual([a["id"] for a in m08001["clientAppearances"]], ["normal", "spring", "midautumn"])
        hd_spring = next(a for a in m08001["clientAppearances"] if a["id"] == "spring")
        self.assertEqual(hd_spring["nativePixels"], [6400, 4800])
        self.assertEqual(hd_spring["worldGrid"], {"widthPixels": 3200, "heightPixels": 2400, "tileWidthPixels": 400, "tileHeightPixels": 300})
        self.assertFalse(m08001["serverBaseSceneRegistered"])
        self.assertFalse(m08001["productionTravelEnabled"])

        m16001 = next(m for m in manifest["maps"] if m["mapId"] == "16001")
        self.assertEqual(m16001["artStage"], "qdao_redrawn_static_back")
        self.assertFalse(m16001["isTemporaryOriginal"])
        self.assertEqual(m16001["nativePixels"], [1920, 1286])
        self.assertEqual(m16001["clientDefaultAppearance"], "spring")
        self.assertEqual([a["id"] for a in m16001["clientAppearances"]], ["spring", "midautumn", "normal"])
        self.assertFalse(m16001["serverBaseSceneRegistered"])
        self.assertFalse(m16001["productionTravelEnabled"])

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

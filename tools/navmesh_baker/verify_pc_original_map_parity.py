"""Generate and verify the server-side audit manifest for the 2026-11-23 PC original static backgrounds.

Verifies existing server authoritative data (BaseScene, World, festival_maps,
authored_maps) without inferring collision, navigation, spawns, or teleports from
static background PNGs. Optionally cross-checks 1-to-1 parity against the sibling
mmorpg-client PcOriginalPreview20261123/catalog.json.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import sys

sys.dont_write_bytecode = True

SERVER_ROOT = Path(__file__).resolve().parents[2]
MANIFEST_REL = Path("data/scene_nav_bin/pc_original_static_backgrounds_20261123.json")
BASESCENE_JSON_REL = Path("generated/tables/basescene.json")
WORLD_JSON_REL = Path("generated/tables/world.json")
FESTIVAL_MANIFEST_REL = Path("data/scene_nav_bin/festival_maps_20261109.json")
AUTHORED_MANIFEST_REL = Path("data/scene_nav_bin/authored_maps_20261112.json")
DEFAULT_CLIENT_CATALOG = SERVER_ROOT.parent / "mmorpg-client/Assets/Resources/World/PcOriginalPreview20261123/catalog.json"
EXPECTED_CONTAINER_SHA256 = "724b3f9b1018951ac21d502a798d54b1b86d0a04250f01e1524add33525dc07f"

RELATED_EXISTING_SCENE_NOTES = {
    "05000": (
        "Existing formal BaseScene.id=1 (main_scene.bin) is the legacy 300x300m city; "
        "PC original 05000 (10000x6774) is NOT registered as BaseScene 5000 and must not be conflated with BaseScene 1."
    ),
    "02000": (
        "Existing formal BaseScene.id=4 (lanxian_scene.bin) is Lanxian Town; "
        "PC original 02000 (3634x4000, Lanxian Outskirts) is NOT registered as BaseScene 2000 or BaseScene 4."
    ),
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def read_json(path: Path) -> dict:
    return json.loads(path.read_bytes().decode("utf-8-sig"))


def json_bytes(payload: object) -> bytes:
    return (json.dumps(payload, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


def load_server_inventory(server_root: Path) -> dict:
    bs_path = server_root / BASESCENE_JSON_REL
    world_path = server_root / WORLD_JSON_REL
    fest_path = server_root / FESTIVAL_MANIFEST_REL
    auth_path = server_root / AUTHORED_MANIFEST_REL

    require(bs_path.is_file(), f"Missing {bs_path}")
    require(world_path.is_file(), f"Missing {world_path}")
    require(fest_path.is_file(), f"Missing {fest_path}")
    require(auth_path.is_file(), f"Missing {auth_path}")

    bs_rows = read_json(bs_path).get("data") or []
    world_rows = read_json(world_path).get("data") or []
    fest_maps = read_json(fest_path).get("maps") or []
    auth_maps = read_json(auth_path).get("maps") or []

    return {
        "baseSceneIds": sorted({int(r["id"]) for r in bs_rows}),
        "worldSceneIds": sorted({int(r["scene_id"]) for r in world_rows}),
        "festivalNavSceneIds": sorted({int(m["sceneId"]) for m in fest_maps}),
        "authoredNavSceneIds": sorted({int(m["sceneConfigId"]) for m in auth_maps}),
        "baseSceneSha256": sha256_bytes(bs_path.read_bytes()),
        "worldSha256": sha256_bytes(world_path.read_bytes()),
        "teleportRequestStatus": "UNIMPLEMENTED_STUB_IN_PLAYER_MOVEMENT_HANDLER",
        "sceneEntryMechanism": "EnterSceneC2S(scene_config_id, scene_id=0) validated via go/scene_manager against World and BaseScene",
    }


def build_manifest(server_root: Path, client_catalog_path: Path) -> dict:
    require(client_catalog_path.is_file(), f"Missing client catalog: {client_catalog_path}")
    catalog_raw = client_catalog_path.read_bytes()
    catalog_sha = sha256_bytes(catalog_raw)
    catalog = json.loads(catalog_raw.decode("utf-8-sig"))

    require(catalog.get("kind") == "CLIENT_PC_ORIGINAL_STATIC_BACK_PREVIEW_CATALOG", "Unexpected client catalog kind")
    require(catalog.get("staticBackOnly") is True, "Client catalog must be staticBackOnly=true")
    require(catalog.get("completeGameplaySceneClaimed") is False, "Client catalog cannot claim complete gameplay scenes")
    require(catalog.get("productionTravelEnabled") is False, "Client catalog cannot enable production travel")
    require(catalog["sourceContainer"]["sha256"] == EXPECTED_CONTAINER_SHA256, "Unexpected container SHA-256")

    inv = load_server_inventory(server_root)
    bs_set = set(inv["baseSceneIds"])
    world_set = set(inv["worldSceneIds"])
    fest_set = set(inv["festivalNavSceneIds"])
    auth_set = set(inv["authoredNavSceneIds"])

    requested = catalog["requestedMapIds"]
    require(len(requested) == 40 and len(set(requested)) == 40, "Expected 40 unique requestedMapIds")
    require(len(catalog["maps"]) == 36, "Expected 36 ready maps in client catalog")
    require(len(catalog["unavailableMaps"]) == 4, "Expected 4 unavailable gaps in client catalog")
    require(len(catalog["specialHoldCandidates"]) == 2, "Expected 2 special HOLD candidates in client catalog")

    maps_out = []
    for entry in catalog["maps"]:
        map_id = entry["mapId"]
        num_id = int(map_id)
        require(num_id == int(entry["numericMapId"]), f"{map_id}: numericMapId mismatch")
        require(num_id not in bs_set, f"{map_id}: unexpected collision with BaseScene.id")
        require(num_id not in world_set, f"{map_id}: unexpected collision with World.scene_id")
        require(num_id not in fest_set, f"{map_id}: unexpected collision with festival_maps")
        require(num_id not in auth_set, f"{map_id}: unexpected collision with authored_maps")

        map_item = {
            "mapId": map_id,
            "numericMapId": num_id,
            "name": entry["name"],
            "clientPreviewStatus": "ready_local_preview_only",
            "artStage": entry["artStage"],
            "isTemporaryOriginal": bool(entry["isTemporaryOriginal"]),
            "replaceableByQdaoArt": bool(entry["replaceableByQdaoArt"]),
            "nativePixels": [int(entry["nativeWidth"]), int(entry["nativeHeight"])],
            "sourceBranch": entry["sourceBranch"],
            "sourceNativeSha256": entry["sourceNativeSha256"],
            "sourceNativeBytes": int(entry["sourceNativeBytes"]),
            "clientCatalogRevision": entry["revision"],
            "clientResourceRoot": entry["resourceRoot"],
            "clientTileFormat": entry["tileFormat"],
            "clientTileCount": int(entry["tileCount"]),
            "clientHasAlpha": bool(entry["hasAlpha"]),
            "clientTilesManifestSha256": entry["tilesManifestSha256"],
            "serverBaseSceneRegistered": False,
            "serverWorldRegistered": False,
            "serverFestivalNavRegistered": False,
            "serverAuthoredNavRegistered": False,
            "serverNavigationStatus": "UNPROVEN_STATIC_BACK_ONLY",
            "serverCollisionStatus": "UNPROVEN_STATIC_BACK_ONLY",
            "serverEntryTeleportStatus": "NOT_REGISTERED_IN_WORLD_OR_BASESCENE",
            "productionTravelEnabled": False,
            "relatedExistingServerSceneNote": RELATED_EXISTING_SCENE_NOTES.get(map_id, ""),
        }
        if entry.get("pcOriginalSourceNativeSha256"):
            map_item["pcOriginalSourceNativeSha256"] = entry["pcOriginalSourceNativeSha256"]
            map_item["pcOriginalSourceNativeBytes"] = int(entry["pcOriginalSourceNativeBytes"])
        if entry.get("appearances"):
            map_item["clientDefaultAppearance"] = entry["defaultAppearance"]
            map_item["clientAppearances"] = [
                {
                    "id": a["id"],
                    "displayName": a.get("displayName", a["id"]),
                    "artStage": a["artStage"],
                    "revision": a["revision"],
                    "resourceRoot": a["resourceRoot"],
                    "hasAlpha": bool(a["hasAlpha"]),
                    "tileFormat": a["tileFormat"],
                    "sourceNativeSha256": a["sourceNativeSha256"],
                    "sourceNativeBytes": int(a["sourceNativeBytes"]),
                    "tilesManifestSha256": a["tilesManifestSha256"],
                }
                for a in entry["appearances"]
            ]
        maps_out.append(map_item)

    unavailable_out = []
    for gap in catalog["unavailableMaps"]:
        num_id = int(gap["mapId"])
        require(num_id not in bs_set and num_id not in world_set, f"Gap {gap['mapId']} unexpectedly registered on server")
        unavailable_out.append({
            "mapId": gap["mapId"],
            "numericMapId": num_id,
            "status": "unavailable_gap",
            "reasonCode": gap["reasonCode"],
            "detail": gap["detail"],
            "integrated": False,
            "serverBaseSceneRegistered": False,
            "serverWorldRegistered": False,
            "productionTravelEnabled": False,
        })

    special_out = []
    for cand in catalog["specialHoldCandidates"]:
        special_out.append({
            "mapId": cand["mapId"],
            "numericMapId": int(cand["mapId"]),
            "branch": cand["branch"],
            "sourceStatus": cand["sourceStatus"],
            "nativePixels": [int(cand["nativeWidth"]), int(cand["nativeHeight"])],
            "sourceNativeSha256": cand["sourceNativeSha256"],
            "reason": cand["reason"],
            "integrated": False,
            "productionTravelEnabled": False,
        })

    return {
        "schemaVersion": 1,
        "kind": "SERVER_PC_ORIGINAL_STATIC_BACK_PARITY_AUDIT",
        "status": "CLIENT_LOCAL_PREVIEW_ONLY_SERVER_UNREGISTERED",
        "staticBackOnly": True,
        "collisionOrNavInferredFromPng": False,
        "productionTravelEnabled": False,
        "sourceContainerSha256": EXPECTED_CONTAINER_SHA256,
        "clientCatalogPath": "Assets/Resources/World/PcOriginalPreview20261123/catalog.json",
        "clientCatalogSha256": catalog_sha,
        "clientCatalogRevision": catalog["catalogRevision"],
        "existingServerInventory": inv,
        "requestedMapIds": requested,
        "readyPreviewCount": len(maps_out),
        "unavailableGapCount": len(unavailable_out),
        "specialHoldCount": len(special_out),
        "javaServerParityNote": (
            "Java server (luyuan-java/xuanming-server-mmo) likewise has zero BaseScene/World/nav "
            "registrations for these 40 PC original IDs; no schema or protocol changed in this handoff."
        ),
        "maps": maps_out,
        "unavailableMaps": unavailable_out,
        "specialHoldCandidates": special_out,
    }


def verify_manifest(server_root: Path, client_catalog_path: Path | None = None) -> dict:
    manifest_path = server_root / MANIFEST_REL
    require(manifest_path.is_file(), f"Missing server audit manifest: {manifest_path}")
    manifest = read_json(manifest_path)

    require(manifest.get("schemaVersion") == 1, "Invalid schemaVersion")
    require(manifest.get("kind") == "SERVER_PC_ORIGINAL_STATIC_BACK_PARITY_AUDIT", "Invalid manifest kind")
    require(manifest.get("staticBackOnly") is True, "staticBackOnly must be true")
    require(manifest.get("collisionOrNavInferredFromPng") is False, "collisionOrNavInferredFromPng must be false")
    require(manifest.get("productionTravelEnabled") is False, "productionTravelEnabled must be false")
    require(manifest.get("sourceContainerSha256") == EXPECTED_CONTAINER_SHA256, "Unexpected container SHA-256")

    inv = load_server_inventory(server_root)
    recorded_inv = manifest.get("existingServerInventory")
    require(isinstance(recorded_inv, dict), "Missing existingServerInventory snapshot")
    for key in ("baseSceneIds", "worldSceneIds", "festivalNavSceneIds", "authoredNavSceneIds"):
        require(isinstance(recorded_inv.get(key), list), f"Invalid existingServerInventory.{key}")

    requested = manifest.get("requestedMapIds") or []
    maps = manifest.get("maps") or []
    unavail = manifest.get("unavailableMaps") or []
    special = manifest.get("specialHoldCandidates") or []

    require(len(requested) == 40 and len(set(requested)) == 40, "Expected 40 unique requestedMapIds")
    require(len(maps) == 36 and manifest.get("readyPreviewCount") == 36, "Expected 36 ready preview entries")
    require(len(unavail) == 4 and manifest.get("unavailableGapCount") == 4, "Expected 4 unavailable gaps")
    require(len(special) == 2 and manifest.get("specialHoldCount") == 2, "Expected 2 special HOLD candidates")

    bs_set = set(inv["baseSceneIds"])
    world_set = set(inv["worldSceneIds"])
    fest_set = set(inv["festivalNavSceneIds"])
    auth_set = set(inv["authoredNavSceneIds"])

    ready_ids = []
    for m in maps:
        map_id = m["mapId"]
        num_id = int(map_id)
        require(num_id == int(m["numericMapId"]), f"{map_id}: numericMapId mismatch")
        require(m.get("serverBaseSceneRegistered") is False and num_id not in bs_set, f"{map_id}: BaseScene parity error")
        require(m.get("serverWorldRegistered") is False and num_id not in world_set, f"{map_id}: World parity error")
        require(m.get("serverFestivalNavRegistered") is False and num_id not in fest_set, f"{map_id}: festival nav parity error")
        require(m.get("serverAuthoredNavRegistered") is False and num_id not in auth_set, f"{map_id}: authored nav parity error")
        require(m.get("serverNavigationStatus") == "UNPROVEN_STATIC_BACK_ONLY", f"{map_id}: invalid serverNavigationStatus")
        require(m.get("productionTravelEnabled") is False, f"{map_id}: productionTravelEnabled must be false")
        ready_ids.append(map_id)

    unavail_ids = []
    for g in unavail:
        num_id = int(g["mapId"])
        require(g.get("integrated") is False, f"Gap {g.get('mapId')} must not be integrated")
        require(g.get("productionTravelEnabled") is False, f"Gap {g.get('mapId')} productionTravelEnabled must be false")
        require(num_id not in bs_set and num_id not in world_set and num_id not in fest_set and num_id not in auth_set,
                f"Gap {g.get('mapId')} unexpectedly registered on server")
        unavail_ids.append(g["mapId"])

    for c in special:
        require(c.get("integrated") is False, f"HOLD {c.get('mapId')} must not be integrated")
        require(c.get("productionTravelEnabled") is False, f"HOLD {c.get('mapId')} productionTravelEnabled must be false")

    require(len(set(ready_ids)) == 36, "Duplicate ready mapId")
    require(len(set(unavail_ids)) == 4, "Duplicate unavailable mapId")
    require(set(ready_ids).isdisjoint(set(unavail_ids)), "Ready and unavailable mapIds overlap")
    require(set(ready_ids) | set(unavail_ids) == set(requested), "Ready + unavailable != requestedMapIds")

    client_checked = False
    if client_catalog_path is not None:
        expected = build_manifest(server_root, client_catalog_path)
        manifest_cmp = {k: v for k, v in manifest.items() if k != "existingServerInventory"}
        expected_cmp = {k: v for k, v in expected.items() if k != "existingServerInventory"}
        require(manifest_cmp == expected_cmp, "Server parity manifest does not match current client catalog")
        client_checked = True

    print(
        f"SERVER PARITY VERIFY PASS: 36 ready preview IDs, 4 unavailable gaps, 2 special HOLD, "
        f"0 collisions with existing server IDs (BaseScene={len(bs_set)}, World={len(world_set)}, "
        f"festival={len(fest_set)}, authored={len(auth_set)}), clientCrossChecked={client_checked}"
    )
    return {"result": "PASS", "clientCrossChecked": client_checked, "readyCount": len(maps)}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify server parity manifest for PC original maps.")
    parser.add_argument("--server-root", type=Path, default=SERVER_ROOT)
    parser.add_argument("--client-catalog", type=Path, default=None, help="Optional path to client catalog.json for cross-repo parity check.")
    parser.add_argument("--write-manifest", action="store_true", help="Write data/scene_nav_bin/pc_original_static_backgrounds_20261123.json.")
    args = parser.parse_args(argv)

    server_root = args.server_root.resolve()
    if args.write_manifest:
        catalog_path = (args.client_catalog or DEFAULT_CLIENT_CATALOG).resolve()
        payload = build_manifest(server_root, catalog_path)
        out_path = server_root / MANIFEST_REL
        out_path.write_bytes(json_bytes(payload))
        print(f"WROTE {out_path} ({len(payload['maps'])} ready, {len(payload['unavailableMaps'])} gaps, {len(payload['specialHoldCandidates'])} HOLD)")
        verify_manifest(server_root, catalog_path)
        return 0

    catalog_path = args.client_catalog.resolve() if args.client_catalog is not None else None
    verify_manifest(server_root, catalog_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""刷新活动地图表集时，守住旧地图、当前正式数据及显式变更范围。"""

import unittest

import refresh_authored_runtime_tables as subject


class RefreshAuthoredTablesTests(unittest.TestCase):
    def setUp(self):
        self.previous = {name: "authored" for name in subject.MAP_ARTIFACTS}
        self.previous.update({"manifest.json": "old", "pet.json": "old-pet", "pet.pb": "old-pet-pb",
                              "item.json": "same", "item.pb": "same"})
        self.official = dict(self.previous, **{"pet.json": "new-pet", "pet.pb": "new-pet-pb"})
        for name in subject.MAP_ARTIFACTS:
            self.official[name] = "formal-map"
        self.candidate = dict(self.official, **{name: "authored" for name in subject.MAP_ARTIFACTS})
        self.candidate["manifest.json"] = "fresh-manifest"

    def test_only_approved_tables_change_and_authored_maps_survive(self):
        self.assertEqual(subject.compare_artifacts(self.previous, self.official, self.candidate, ["Pet"]),
                         ["manifest.json", "pet.json", "pet.pb"])

    def test_replacing_authored_map_with_current_official_map_is_rejected(self):
        self.candidate["basescene.pb"] = self.official["basescene.pb"]
        with self.assertRaisesRegex(RuntimeError, "Authored map artifact changed"):
            subject.compare_artifacts(self.previous, self.official, self.candidate, ["Pet"])

    def test_unchanged_stale_pet_table_cannot_pass(self):
        self.candidate["pet.pb"] = self.previous["pet.pb"]
        with self.assertRaisesRegex(RuntimeError, "current official table"):
            subject.compare_artifacts(self.previous, self.official, self.candidate, ["Pet"])

    def test_unapproved_change_and_unrecorded_artifact_are_rejected(self):
        self.candidate["item.pb"] = self.official["item.pb"] = "unexpected-item"
        with self.assertRaisesRegex(RuntimeError, "Unapproved table changes"):
            subject.compare_artifacts(self.previous, self.official, self.candidate, ["Pet"])
        self.candidate["extra.pb"] = "extra"
        with self.assertRaisesRegex(RuntimeError, "artifact set changed"):
            subject.compare_artifacts(self.previous, self.official, self.candidate, ["Pet"])


if __name__ == "__main__":
    unittest.main()

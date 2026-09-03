
package com.game.table;

import com.google.protobuf.util.JsonFormat;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
import java.util.function.Predicate;

/**
 * Auto-generated config manager for Dungeon.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class DungeonTableManager {

    private static final DungeonTableManager INSTANCE = new DungeonTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final DungeonTableData data;
        final Map<Integer, DungeonTable> kvData;



        final Map<Integer, List<DungeonTable>> idxMonster;


        final Map<Integer, List<DungeonTable>> idxSceneId;


        Snapshot(DungeonTableData data,
                 Map<Integer, DungeonTable> kvData,
                 Map<Integer, List<DungeonTable>> idxMonster,
                 Map<Integer, List<DungeonTable>> idxSceneId) {
            this.data = data;
            this.kvData = kvData;
            this.idxMonster = idxMonster;
            this.idxSceneId = idxSceneId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            DungeonTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static DungeonTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        DungeonTableData.Builder builder = DungeonTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "dungeon.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "dungeon.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        DungeonTableData data = builder.build();

        Map<Integer, DungeonTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<DungeonTable>> idxMonster = new HashMap<>();
        Map<Integer, List<DungeonTable>> idxSceneId = new HashMap<>();

        for (DungeonTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getMonsterList()) {
                idxMonster.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            idxSceneId.computeIfAbsent(row.getSceneId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxMonster, idxSceneId);
    }

    public DungeonTableData findAll() {
        return snapshot.data;
    }

    public DungeonTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, DungeonTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<DungeonTable> findByMonsterIndex(int key) {
        return snapshot.idxMonster.getOrDefault(key, Collections.emptyList());
    }



    public List<DungeonTable> getBySceneId(int key) {
        return snapshot.idxSceneId.getOrDefault(key, Collections.emptyList());
    }



    // FK: scene_id → BaseScene.id

    // FK: monster → Monster.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByMonsterIndex(int key) {
        return snapshot.idxMonster.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countBySceneIdIndex(int key) {
        return snapshot.idxSceneId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<DungeonTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<DungeonTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            DungeonTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public DungeonTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<DungeonTable> where(Predicate<DungeonTable> pred) {
        Snapshot snap = this.snapshot;
        List<DungeonTable> result = new ArrayList<>();
        for (DungeonTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public DungeonTable first(Predicate<DungeonTable> pred) {
        Snapshot snap = this.snapshot;
        for (DungeonTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
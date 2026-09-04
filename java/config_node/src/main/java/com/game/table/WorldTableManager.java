
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
 * Auto-generated config manager for World.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class WorldTableManager {

    private static final WorldTableManager INSTANCE = new WorldTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final WorldTableData data;
        final Map<Integer, WorldTable> kvData;




        final Map<Integer, List<WorldTable>> idxSceneId;


        Snapshot(WorldTableData data,
                 Map<Integer, WorldTable> kvData,
                 Map<Integer, List<WorldTable>> idxSceneId) {
            this.data = data;
            this.kvData = kvData;
            this.idxSceneId = idxSceneId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            WorldTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static WorldTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        WorldTableData.Builder builder = WorldTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "world.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "world.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        WorldTableData data = builder.build();

        Map<Integer, WorldTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<WorldTable>> idxSceneId = new HashMap<>();

        for (WorldTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxSceneId.computeIfAbsent(row.getSceneId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxSceneId);
    }

    public WorldTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public WorldTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, WorldTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<WorldTable> getBySceneId(int key) {
        return snapshot.idxSceneId.getOrDefault(key, Collections.emptyList());
    }



    // FK: scene_id → BaseScene.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countBySceneIdIndex(int key) {
        return snapshot.idxSceneId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<WorldTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<WorldTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            WorldTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public WorldTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<WorldTable> where(Predicate<WorldTable> pred) {
        Snapshot snap = this.snapshot;
        List<WorldTable> result = new ArrayList<>();
        for (WorldTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public WorldTable first(Predicate<WorldTable> pred) {
        Snapshot snap = this.snapshot;
        for (WorldTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
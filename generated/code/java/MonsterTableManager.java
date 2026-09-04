
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
 * Auto-generated config manager for Monster.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class MonsterTableManager {

    private static final MonsterTableManager INSTANCE = new MonsterTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final MonsterTableData data;
        final Map<Integer, MonsterTable> kvData;





        Snapshot(MonsterTableData data,
                 Map<Integer, MonsterTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            MonsterTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static MonsterTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        MonsterTableData.Builder builder = MonsterTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "monster.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "monster.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        MonsterTableData data = builder.build();

        Map<Integer, MonsterTable> kvData = new HashMap<>(data.getDataCount());

        for (MonsterTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public MonsterTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public MonsterTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, MonsterTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }










    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }





    // ---- FindByIds (IN) ----

    public List<MonsterTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<MonsterTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            MonsterTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public MonsterTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<MonsterTable> where(Predicate<MonsterTable> pred) {
        Snapshot snap = this.snapshot;
        List<MonsterTable> result = new ArrayList<>();
        for (MonsterTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public MonsterTable first(Predicate<MonsterTable> pred) {
        Snapshot snap = this.snapshot;
        for (MonsterTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
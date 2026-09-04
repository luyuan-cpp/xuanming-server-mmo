
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
 * Auto-generated config manager for Cooldown.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class CooldownTableManager {

    private static final CooldownTableManager INSTANCE = new CooldownTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final CooldownTableData data;
        final Map<Integer, CooldownTable> kvData;





        Snapshot(CooldownTableData data,
                 Map<Integer, CooldownTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            CooldownTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static CooldownTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        CooldownTableData.Builder builder = CooldownTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "cooldown.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "cooldown.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        CooldownTableData data = builder.build();

        Map<Integer, CooldownTable> kvData = new HashMap<>(data.getDataCount());

        for (CooldownTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public CooldownTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public CooldownTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, CooldownTable> getKvData() {
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

    public List<CooldownTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<CooldownTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            CooldownTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public CooldownTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<CooldownTable> where(Predicate<CooldownTable> pred) {
        Snapshot snap = this.snapshot;
        List<CooldownTable> result = new ArrayList<>();
        for (CooldownTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public CooldownTable first(Predicate<CooldownTable> pred) {
        Snapshot snap = this.snapshot;
        for (CooldownTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
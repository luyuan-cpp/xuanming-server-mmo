
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
 * Auto-generated config manager for ZoneGroup.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ZoneGroupTableManager {

    private static final ZoneGroupTableManager INSTANCE = new ZoneGroupTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ZoneGroupTableData data;
        final Map<Integer, ZoneGroupTable> kvData;





        Snapshot(ZoneGroupTableData data,
                 Map<Integer, ZoneGroupTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ZoneGroupTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static ZoneGroupTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ZoneGroupTableData.Builder builder = ZoneGroupTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "zonegroup.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "zonegroup.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ZoneGroupTableData data = builder.build();

        Map<Integer, ZoneGroupTable> kvData = new HashMap<>(data.getDataCount());

        for (ZoneGroupTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public ZoneGroupTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ZoneGroupTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ZoneGroupTable> getKvData() {
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

    public List<ZoneGroupTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ZoneGroupTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ZoneGroupTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ZoneGroupTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ZoneGroupTable> where(Predicate<ZoneGroupTable> pred) {
        Snapshot snap = this.snapshot;
        List<ZoneGroupTable> result = new ArrayList<>();
        for (ZoneGroupTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ZoneGroupTable first(Predicate<ZoneGroupTable> pred) {
        Snapshot snap = this.snapshot;
        for (ZoneGroupTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
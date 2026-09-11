
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
 * Auto-generated config manager for ActivitySchedule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ActivityScheduleTableManager {

    private static final ActivityScheduleTableManager INSTANCE = new ActivityScheduleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ActivityScheduleTableData data;
        final Map<Integer, ActivityScheduleTable> kvData;




        final Map<Integer, List<ActivityScheduleTable>> idxId;


        Snapshot(ActivityScheduleTableData data,
                 Map<Integer, ActivityScheduleTable> kvData,
                 Map<Integer, List<ActivityScheduleTable>> idxId) {
            this.data = data;
            this.kvData = kvData;
            this.idxId = idxId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ActivityScheduleTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static ActivityScheduleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ActivityScheduleTableData.Builder builder = ActivityScheduleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "activityschedule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "activityschedule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ActivityScheduleTableData data = builder.build();

        Map<Integer, ActivityScheduleTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<ActivityScheduleTable>> idxId = new HashMap<>();

        for (ActivityScheduleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxId.computeIfAbsent(row.getId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxId);
    }

    public ActivityScheduleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ActivityScheduleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ActivityScheduleTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<ActivityScheduleTable> getById(int key) {
        return snapshot.idxId.getOrDefault(key, Collections.emptyList());
    }



    // FK: id → Mission.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByIdIndex(int key) {
        return snapshot.idxId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<ActivityScheduleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ActivityScheduleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ActivityScheduleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ActivityScheduleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ActivityScheduleTable> where(Predicate<ActivityScheduleTable> pred) {
        Snapshot snap = this.snapshot;
        List<ActivityScheduleTable> result = new ArrayList<>();
        for (ActivityScheduleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ActivityScheduleTable first(Predicate<ActivityScheduleTable> pred) {
        Snapshot snap = this.snapshot;
        for (ActivityScheduleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
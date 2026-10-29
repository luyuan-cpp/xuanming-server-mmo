
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
 * Auto-generated config manager for ActivityVitalityRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ActivityVitalityRuleTableManager {

    private static final ActivityVitalityRuleTableManager INSTANCE = new ActivityVitalityRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ActivityVitalityRuleTableData data;
        final Map<Integer, ActivityVitalityRuleTable> kvData;





        Snapshot(ActivityVitalityRuleTableData data,
                 Map<Integer, ActivityVitalityRuleTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ActivityVitalityRuleTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static ActivityVitalityRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ActivityVitalityRuleTableData.Builder builder = ActivityVitalityRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "activityvitalityrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "activityvitalityrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ActivityVitalityRuleTableData data = builder.build();

        Map<Integer, ActivityVitalityRuleTable> kvData = new HashMap<>(data.getDataCount());

        for (ActivityVitalityRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public ActivityVitalityRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ActivityVitalityRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ActivityVitalityRuleTable> getKvData() {
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

    public List<ActivityVitalityRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ActivityVitalityRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ActivityVitalityRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ActivityVitalityRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ActivityVitalityRuleTable> where(Predicate<ActivityVitalityRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<ActivityVitalityRuleTable> result = new ArrayList<>();
        for (ActivityVitalityRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ActivityVitalityRuleTable first(Predicate<ActivityVitalityRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (ActivityVitalityRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}

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
 * Auto-generated config manager for ActivityVitalityReward.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ActivityVitalityRewardTableManager {

    private static final ActivityVitalityRewardTableManager INSTANCE = new ActivityVitalityRewardTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ActivityVitalityRewardTableData data;
        final Map<Integer, ActivityVitalityRewardTable> kvData;




        final Map<Integer, List<ActivityVitalityRewardTable>> idxRuleId;

        final Map<Integer, List<ActivityVitalityRewardTable>> idxRewardId;


        Snapshot(ActivityVitalityRewardTableData data,
                 Map<Integer, ActivityVitalityRewardTable> kvData,
                 Map<Integer, List<ActivityVitalityRewardTable>> idxRuleId,
                 Map<Integer, List<ActivityVitalityRewardTable>> idxRewardId) {
            this.data = data;
            this.kvData = kvData;
            this.idxRuleId = idxRuleId;
            this.idxRewardId = idxRewardId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ActivityVitalityRewardTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static ActivityVitalityRewardTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ActivityVitalityRewardTableData.Builder builder = ActivityVitalityRewardTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "activityvitalityreward.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "activityvitalityreward.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ActivityVitalityRewardTableData data = builder.build();

        Map<Integer, ActivityVitalityRewardTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<ActivityVitalityRewardTable>> idxRuleId = new HashMap<>();
        Map<Integer, List<ActivityVitalityRewardTable>> idxRewardId = new HashMap<>();

        for (ActivityVitalityRewardTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxRuleId.computeIfAbsent(row.getRuleId(), k -> new ArrayList<>()).add(row);
            idxRewardId.computeIfAbsent(row.getRewardId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxRuleId, idxRewardId);
    }

    public ActivityVitalityRewardTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ActivityVitalityRewardTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ActivityVitalityRewardTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<ActivityVitalityRewardTable> getByRuleId(int key) {
        return snapshot.idxRuleId.getOrDefault(key, Collections.emptyList());
    }

    public List<ActivityVitalityRewardTable> getByRewardId(int key) {
        return snapshot.idxRewardId.getOrDefault(key, Collections.emptyList());
    }



    // FK: rule_id → ActivityVitalityRule.id

    // FK: reward_id → Reward.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByRuleIdIndex(int key) {
        return snapshot.idxRuleId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByRewardIdIndex(int key) {
        return snapshot.idxRewardId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<ActivityVitalityRewardTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ActivityVitalityRewardTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ActivityVitalityRewardTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ActivityVitalityRewardTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ActivityVitalityRewardTable> where(Predicate<ActivityVitalityRewardTable> pred) {
        Snapshot snap = this.snapshot;
        List<ActivityVitalityRewardTable> result = new ArrayList<>();
        for (ActivityVitalityRewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ActivityVitalityRewardTable first(Predicate<ActivityVitalityRewardTable> pred) {
        Snapshot snap = this.snapshot;
        for (ActivityVitalityRewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}

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
 * Auto-generated config manager for Mission.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class MissionTableManager {

    private static final MissionTableManager INSTANCE = new MissionTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final MissionTableData data;
        final Map<Integer, MissionTable> kvData;



        final Map<Integer, List<MissionTable>> idxConditionId;

        final Map<Integer, List<MissionTable>> idxNextMissionId;

        final Map<Integer, List<MissionTable>> idxTargetCount;


        final Map<Integer, List<MissionTable>> idxRewardId;


        Snapshot(MissionTableData data,
                 Map<Integer, MissionTable> kvData,
                 Map<Integer, List<MissionTable>> idxConditionId,
                 Map<Integer, List<MissionTable>> idxNextMissionId,
                 Map<Integer, List<MissionTable>> idxTargetCount,
                 Map<Integer, List<MissionTable>> idxRewardId) {
            this.data = data;
            this.kvData = kvData;
            this.idxConditionId = idxConditionId;
            this.idxNextMissionId = idxNextMissionId;
            this.idxTargetCount = idxTargetCount;
            this.idxRewardId = idxRewardId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            MissionTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static MissionTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        MissionTableData.Builder builder = MissionTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "mission.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "mission.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        MissionTableData data = builder.build();

        Map<Integer, MissionTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<MissionTable>> idxConditionId = new HashMap<>();
        Map<Integer, List<MissionTable>> idxNextMissionId = new HashMap<>();
        Map<Integer, List<MissionTable>> idxTargetCount = new HashMap<>();
        Map<Integer, List<MissionTable>> idxRewardId = new HashMap<>();

        for (MissionTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getConditionIdList()) {
                idxConditionId.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getNextMissionIdList()) {
                idxNextMissionId.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getTargetCountList()) {
                idxTargetCount.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            idxRewardId.computeIfAbsent(row.getRewardId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxConditionId, idxNextMissionId, idxTargetCount, idxRewardId);
    }

    public MissionTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public MissionTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, MissionTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<MissionTable> findByConditionIdIndex(int key) {
        return snapshot.idxConditionId.getOrDefault(key, Collections.emptyList());
    }

    public List<MissionTable> findByNextMissionIdIndex(int key) {
        return snapshot.idxNextMissionId.getOrDefault(key, Collections.emptyList());
    }

    public List<MissionTable> findByTargetCountIndex(int key) {
        return snapshot.idxTargetCount.getOrDefault(key, Collections.emptyList());
    }



    public List<MissionTable> getByRewardId(int key) {
        return snapshot.idxRewardId.getOrDefault(key, Collections.emptyList());
    }



    // FK: reward_id → Reward.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByConditionIdIndex(int key) {
        return snapshot.idxConditionId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByNextMissionIdIndex(int key) {
        return snapshot.idxNextMissionId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByTargetCountIndex(int key) {
        return snapshot.idxTargetCount.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countByRewardIdIndex(int key) {
        return snapshot.idxRewardId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<MissionTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<MissionTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            MissionTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public MissionTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<MissionTable> where(Predicate<MissionTable> pred) {
        Snapshot snap = this.snapshot;
        List<MissionTable> result = new ArrayList<>();
        for (MissionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public MissionTable first(Predicate<MissionTable> pred) {
        Snapshot snap = this.snapshot;
        for (MissionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
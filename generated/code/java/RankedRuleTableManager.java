
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
 * Auto-generated config manager for RankedRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class RankedRuleTableManager {

    private static final RankedRuleTableManager INSTANCE = new RankedRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final RankedRuleTableData data;
        final Map<Integer, RankedRuleTable> kvData;




        final Map<Integer, List<RankedRuleTable>> idxBattleConfigId;

        final Map<Integer, List<RankedRuleTable>> idxReferenceClassId;


        Snapshot(RankedRuleTableData data,
                 Map<Integer, RankedRuleTable> kvData,
                 Map<Integer, List<RankedRuleTable>> idxBattleConfigId,
                 Map<Integer, List<RankedRuleTable>> idxReferenceClassId) {
            this.data = data;
            this.kvData = kvData;
            this.idxBattleConfigId = idxBattleConfigId;
            this.idxReferenceClassId = idxReferenceClassId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            RankedRuleTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static RankedRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        RankedRuleTableData.Builder builder = RankedRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "rankedrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "rankedrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        RankedRuleTableData data = builder.build();

        Map<Integer, RankedRuleTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<RankedRuleTable>> idxBattleConfigId = new HashMap<>();
        Map<Integer, List<RankedRuleTable>> idxReferenceClassId = new HashMap<>();

        for (RankedRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxBattleConfigId.computeIfAbsent(row.getBattleConfigId(), k -> new ArrayList<>()).add(row);
            idxReferenceClassId.computeIfAbsent(row.getReferenceClassId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxBattleConfigId, idxReferenceClassId);
    }

    public RankedRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public RankedRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, RankedRuleTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<RankedRuleTable> getByBattleConfigId(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList());
    }

    public List<RankedRuleTable> getByReferenceClassId(int key) {
        return snapshot.idxReferenceClassId.getOrDefault(key, Collections.emptyList());
    }



    // FK: battle_config_id → Dungeon.id

    // FK: reference_class_id → Class.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByBattleConfigIdIndex(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByReferenceClassIdIndex(int key) {
        return snapshot.idxReferenceClassId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<RankedRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<RankedRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            RankedRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public RankedRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<RankedRuleTable> where(Predicate<RankedRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<RankedRuleTable> result = new ArrayList<>();
        for (RankedRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public RankedRuleTable first(Predicate<RankedRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (RankedRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
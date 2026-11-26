
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
 * Auto-generated config manager for BattleProgressionRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class BattleProgressionRuleTableManager {

    private static final BattleProgressionRuleTableManager INSTANCE = new BattleProgressionRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final BattleProgressionRuleTableData data;
        final Map<Integer, BattleProgressionRuleTable> kvData;





        Snapshot(BattleProgressionRuleTableData data,
                 Map<Integer, BattleProgressionRuleTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            BattleProgressionRuleTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static BattleProgressionRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        BattleProgressionRuleTableData.Builder builder = BattleProgressionRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "battleprogressionrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "battleprogressionrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        BattleProgressionRuleTableData data = builder.build();

        Map<Integer, BattleProgressionRuleTable> kvData = new HashMap<>(data.getDataCount());

        for (BattleProgressionRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public BattleProgressionRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public BattleProgressionRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, BattleProgressionRuleTable> getKvData() {
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

    public List<BattleProgressionRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<BattleProgressionRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            BattleProgressionRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public BattleProgressionRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<BattleProgressionRuleTable> where(Predicate<BattleProgressionRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<BattleProgressionRuleTable> result = new ArrayList<>();
        for (BattleProgressionRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public BattleProgressionRuleTable first(Predicate<BattleProgressionRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (BattleProgressionRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}

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
 * Auto-generated config manager for AfkPassRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AfkPassRuleTableManager {

    private static final AfkPassRuleTableManager INSTANCE = new AfkPassRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AfkPassRuleTableData data;
        final Map<Integer, AfkPassRuleTable> kvData;





        Snapshot(AfkPassRuleTableData data,
                 Map<Integer, AfkPassRuleTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AfkPassRuleTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static AfkPassRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AfkPassRuleTableData.Builder builder = AfkPassRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "afkpassrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "afkpassrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AfkPassRuleTableData data = builder.build();

        Map<Integer, AfkPassRuleTable> kvData = new HashMap<>(data.getDataCount());

        for (AfkPassRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public AfkPassRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public AfkPassRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AfkPassRuleTable> getKvData() {
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

    public List<AfkPassRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AfkPassRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AfkPassRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AfkPassRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AfkPassRuleTable> where(Predicate<AfkPassRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<AfkPassRuleTable> result = new ArrayList<>();
        for (AfkPassRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AfkPassRuleTable first(Predicate<AfkPassRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (AfkPassRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
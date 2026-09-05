
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
 * Auto-generated config manager for AttributeRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AttributeRuleTableManager {

    private static final AttributeRuleTableManager INSTANCE = new AttributeRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AttributeRuleTableData data;
        final Map<Integer, AttributeRuleTable> kvData;





        Snapshot(AttributeRuleTableData data,
                 Map<Integer, AttributeRuleTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AttributeRuleTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static AttributeRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AttributeRuleTableData.Builder builder = AttributeRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "attributerule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "attributerule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AttributeRuleTableData data = builder.build();

        Map<Integer, AttributeRuleTable> kvData = new HashMap<>(data.getDataCount());

        for (AttributeRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public AttributeRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public AttributeRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AttributeRuleTable> getKvData() {
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

    public List<AttributeRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AttributeRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AttributeRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AttributeRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AttributeRuleTable> where(Predicate<AttributeRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<AttributeRuleTable> result = new ArrayList<>();
        for (AttributeRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AttributeRuleTable first(Predicate<AttributeRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (AttributeRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
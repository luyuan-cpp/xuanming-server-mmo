
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
 * Auto-generated config manager for JianghuRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class JianghuRuleTableManager {

    private static final JianghuRuleTableManager INSTANCE = new JianghuRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final JianghuRuleTableData data;
        final Map<Integer, JianghuRuleTable> kvData;




        final Map<Integer, List<JianghuRuleTable>> idxGoldWritItemId;

        final Map<Integer, List<JianghuRuleTable>> idxClearItemId;


        Snapshot(JianghuRuleTableData data,
                 Map<Integer, JianghuRuleTable> kvData,
                 Map<Integer, List<JianghuRuleTable>> idxGoldWritItemId,
                 Map<Integer, List<JianghuRuleTable>> idxClearItemId) {
            this.data = data;
            this.kvData = kvData;
            this.idxGoldWritItemId = idxGoldWritItemId;
            this.idxClearItemId = idxClearItemId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            JianghuRuleTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static JianghuRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        JianghuRuleTableData.Builder builder = JianghuRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "jianghurule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "jianghurule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        JianghuRuleTableData data = builder.build();

        Map<Integer, JianghuRuleTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<JianghuRuleTable>> idxGoldWritItemId = new HashMap<>();
        Map<Integer, List<JianghuRuleTable>> idxClearItemId = new HashMap<>();

        for (JianghuRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxGoldWritItemId.computeIfAbsent(row.getGoldWritItemId(), k -> new ArrayList<>()).add(row);
            idxClearItemId.computeIfAbsent(row.getClearItemId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxGoldWritItemId, idxClearItemId);
    }

    public JianghuRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public JianghuRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, JianghuRuleTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<JianghuRuleTable> getByGoldWritItemId(int key) {
        return snapshot.idxGoldWritItemId.getOrDefault(key, Collections.emptyList());
    }

    public List<JianghuRuleTable> getByClearItemId(int key) {
        return snapshot.idxClearItemId.getOrDefault(key, Collections.emptyList());
    }



    // FK: gold_writ_item_id → Item.id

    // FK: clear_item_id → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByGoldWritItemIdIndex(int key) {
        return snapshot.idxGoldWritItemId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByClearItemIdIndex(int key) {
        return snapshot.idxClearItemId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<JianghuRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<JianghuRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            JianghuRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public JianghuRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<JianghuRuleTable> where(Predicate<JianghuRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<JianghuRuleTable> result = new ArrayList<>();
        for (JianghuRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public JianghuRuleTable first(Predicate<JianghuRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (JianghuRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}

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
 * Auto-generated config manager for Item.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ItemTableManager {

    private static final ItemTableManager INSTANCE = new ItemTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ItemTableData data;
        final Map<Integer, ItemTable> kvData;




        final Map<Integer, List<ItemTable>> idxAffixRule;

        final Map<Integer, List<ItemTable>> idxForgeMaterialId;

        final Map<Integer, List<ItemTable>> idxArtifactId;


        Snapshot(ItemTableData data,
                 Map<Integer, ItemTable> kvData,
                 Map<Integer, List<ItemTable>> idxAffixRule,
                 Map<Integer, List<ItemTable>> idxForgeMaterialId,
                 Map<Integer, List<ItemTable>> idxArtifactId) {
            this.data = data;
            this.kvData = kvData;
            this.idxAffixRule = idxAffixRule;
            this.idxForgeMaterialId = idxForgeMaterialId;
            this.idxArtifactId = idxArtifactId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ItemTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static ItemTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ItemTableData.Builder builder = ItemTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "item.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "item.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ItemTableData data = builder.build();

        Map<Integer, ItemTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<ItemTable>> idxAffixRule = new HashMap<>();
        Map<Integer, List<ItemTable>> idxForgeMaterialId = new HashMap<>();
        Map<Integer, List<ItemTable>> idxArtifactId = new HashMap<>();

        for (ItemTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxAffixRule.computeIfAbsent(row.getAffixRule(), k -> new ArrayList<>()).add(row);
            idxForgeMaterialId.computeIfAbsent(row.getForgeMaterialId(), k -> new ArrayList<>()).add(row);
            idxArtifactId.computeIfAbsent(row.getArtifactId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxAffixRule, idxForgeMaterialId, idxArtifactId);
    }

    public ItemTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ItemTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ItemTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<ItemTable> getByAffixRule(int key) {
        return snapshot.idxAffixRule.getOrDefault(key, Collections.emptyList());
    }

    public List<ItemTable> getByForgeMaterialId(int key) {
        return snapshot.idxForgeMaterialId.getOrDefault(key, Collections.emptyList());
    }

    public List<ItemTable> getByArtifactId(int key) {
        return snapshot.idxArtifactId.getOrDefault(key, Collections.emptyList());
    }



    // FK: affix_rule → EquipAffixRule.id

    // FK: forge_material_id → Item.id

    // FK: artifact_id → EquipArtifact.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByAffixRuleIndex(int key) {
        return snapshot.idxAffixRule.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByForgeMaterialIdIndex(int key) {
        return snapshot.idxForgeMaterialId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByArtifactIdIndex(int key) {
        return snapshot.idxArtifactId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<ItemTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ItemTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ItemTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ItemTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ItemTable> where(Predicate<ItemTable> pred) {
        Snapshot snap = this.snapshot;
        List<ItemTable> result = new ArrayList<>();
        for (ItemTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ItemTable first(Predicate<ItemTable> pred) {
        Snapshot snap = this.snapshot;
        for (ItemTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
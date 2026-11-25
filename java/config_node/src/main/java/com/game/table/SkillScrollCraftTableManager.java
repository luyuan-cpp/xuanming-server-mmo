
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
 * Auto-generated config manager for SkillScrollCraft.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SkillScrollCraftTableManager {

    private static final SkillScrollCraftTableManager INSTANCE = new SkillScrollCraftTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SkillScrollCraftTableData data;
        final Map<Integer, SkillScrollCraftTable> kvData;




        final Map<Integer, List<SkillScrollCraftTable>> idxFragmentItemId;


        Snapshot(SkillScrollCraftTableData data,
                 Map<Integer, SkillScrollCraftTable> kvData,
                 Map<Integer, List<SkillScrollCraftTable>> idxFragmentItemId) {
            this.data = data;
            this.kvData = kvData;
            this.idxFragmentItemId = idxFragmentItemId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SkillScrollCraftTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static SkillScrollCraftTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SkillScrollCraftTableData.Builder builder = SkillScrollCraftTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "skillscrollcraft.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "skillscrollcraft.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SkillScrollCraftTableData data = builder.build();

        Map<Integer, SkillScrollCraftTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<SkillScrollCraftTable>> idxFragmentItemId = new HashMap<>();

        for (SkillScrollCraftTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxFragmentItemId.computeIfAbsent(row.getFragmentItemId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxFragmentItemId);
    }

    public SkillScrollCraftTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SkillScrollCraftTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SkillScrollCraftTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<SkillScrollCraftTable> getByFragmentItemId(int key) {
        return snapshot.idxFragmentItemId.getOrDefault(key, Collections.emptyList());
    }



    // FK: fragment_item_id → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByFragmentItemIdIndex(int key) {
        return snapshot.idxFragmentItemId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<SkillScrollCraftTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SkillScrollCraftTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SkillScrollCraftTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SkillScrollCraftTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SkillScrollCraftTable> where(Predicate<SkillScrollCraftTable> pred) {
        Snapshot snap = this.snapshot;
        List<SkillScrollCraftTable> result = new ArrayList<>();
        for (SkillScrollCraftTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SkillScrollCraftTable first(Predicate<SkillScrollCraftTable> pred) {
        Snapshot snap = this.snapshot;
        for (SkillScrollCraftTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
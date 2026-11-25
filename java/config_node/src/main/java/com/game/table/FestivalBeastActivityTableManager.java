
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
 * Auto-generated config manager for FestivalBeastActivity.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class FestivalBeastActivityTableManager {

    private static final FestivalBeastActivityTableManager INSTANCE = new FestivalBeastActivityTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final FestivalBeastActivityTableData data;
        final Map<Integer, FestivalBeastActivityTable> kvData;




        final Map<Integer, List<FestivalBeastActivityTable>> idxBattleConfigId;

        final Map<Integer, List<FestivalBeastActivityTable>> idxRewardPetTableId;


        Snapshot(FestivalBeastActivityTableData data,
                 Map<Integer, FestivalBeastActivityTable> kvData,
                 Map<Integer, List<FestivalBeastActivityTable>> idxBattleConfigId,
                 Map<Integer, List<FestivalBeastActivityTable>> idxRewardPetTableId) {
            this.data = data;
            this.kvData = kvData;
            this.idxBattleConfigId = idxBattleConfigId;
            this.idxRewardPetTableId = idxRewardPetTableId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            FestivalBeastActivityTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static FestivalBeastActivityTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        FestivalBeastActivityTableData.Builder builder = FestivalBeastActivityTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "festivalbeastactivity.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "festivalbeastactivity.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        FestivalBeastActivityTableData data = builder.build();

        Map<Integer, FestivalBeastActivityTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<FestivalBeastActivityTable>> idxBattleConfigId = new HashMap<>();
        Map<Integer, List<FestivalBeastActivityTable>> idxRewardPetTableId = new HashMap<>();

        for (FestivalBeastActivityTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxBattleConfigId.computeIfAbsent(row.getBattleConfigId(), k -> new ArrayList<>()).add(row);
            idxRewardPetTableId.computeIfAbsent(row.getRewardPetTableId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxBattleConfigId, idxRewardPetTableId);
    }

    public FestivalBeastActivityTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public FestivalBeastActivityTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, FestivalBeastActivityTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<FestivalBeastActivityTable> getByBattleConfigId(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList());
    }

    public List<FestivalBeastActivityTable> getByRewardPetTableId(int key) {
        return snapshot.idxRewardPetTableId.getOrDefault(key, Collections.emptyList());
    }



    // FK: battle_config_id → Dungeon.id

    // FK: reward_pet_table_id → Pet.id


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

    public int countByRewardPetTableIdIndex(int key) {
        return snapshot.idxRewardPetTableId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<FestivalBeastActivityTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<FestivalBeastActivityTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            FestivalBeastActivityTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public FestivalBeastActivityTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<FestivalBeastActivityTable> where(Predicate<FestivalBeastActivityTable> pred) {
        Snapshot snap = this.snapshot;
        List<FestivalBeastActivityTable> result = new ArrayList<>();
        for (FestivalBeastActivityTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public FestivalBeastActivityTable first(Predicate<FestivalBeastActivityTable> pred) {
        Snapshot snap = this.snapshot;
        for (FestivalBeastActivityTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
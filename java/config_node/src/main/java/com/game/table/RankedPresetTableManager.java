
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
 * Auto-generated config manager for RankedPreset.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class RankedPresetTableManager {

    private static final RankedPresetTableManager INSTANCE = new RankedPresetTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final RankedPresetTableData data;
        final Map<Integer, RankedPresetTable> kvData;



        final Map<Integer, List<RankedPresetTable>> idxSkillIds;

        final Map<Integer, List<RankedPresetTable>> idxPetEntryIds;

        final Map<Integer, List<RankedPresetTable>> idxPetEntryPoints;


        final Map<Integer, List<RankedPresetTable>> idxPetTableId;


        Snapshot(RankedPresetTableData data,
                 Map<Integer, RankedPresetTable> kvData,
                 Map<Integer, List<RankedPresetTable>> idxSkillIds,
                 Map<Integer, List<RankedPresetTable>> idxPetEntryIds,
                 Map<Integer, List<RankedPresetTable>> idxPetEntryPoints,
                 Map<Integer, List<RankedPresetTable>> idxPetTableId) {
            this.data = data;
            this.kvData = kvData;
            this.idxSkillIds = idxSkillIds;
            this.idxPetEntryIds = idxPetEntryIds;
            this.idxPetEntryPoints = idxPetEntryPoints;
            this.idxPetTableId = idxPetTableId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            RankedPresetTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static RankedPresetTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        RankedPresetTableData.Builder builder = RankedPresetTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "rankedpreset.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "rankedpreset.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        RankedPresetTableData data = builder.build();

        Map<Integer, RankedPresetTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<RankedPresetTable>> idxSkillIds = new HashMap<>();
        Map<Integer, List<RankedPresetTable>> idxPetEntryIds = new HashMap<>();
        Map<Integer, List<RankedPresetTable>> idxPetEntryPoints = new HashMap<>();
        Map<Integer, List<RankedPresetTable>> idxPetTableId = new HashMap<>();

        for (RankedPresetTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getSkillIdsList()) {
                idxSkillIds.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getPetEntryIdsList()) {
                idxPetEntryIds.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getPetEntryPointsList()) {
                idxPetEntryPoints.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            idxPetTableId.computeIfAbsent(row.getPetTableId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxSkillIds, idxPetEntryIds, idxPetEntryPoints, idxPetTableId);
    }

    public RankedPresetTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public RankedPresetTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, RankedPresetTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<RankedPresetTable> findBySkillIdsIndex(int key) {
        return snapshot.idxSkillIds.getOrDefault(key, Collections.emptyList());
    }

    public List<RankedPresetTable> findByPetEntryIdsIndex(int key) {
        return snapshot.idxPetEntryIds.getOrDefault(key, Collections.emptyList());
    }

    public List<RankedPresetTable> findByPetEntryPointsIndex(int key) {
        return snapshot.idxPetEntryPoints.getOrDefault(key, Collections.emptyList());
    }



    public List<RankedPresetTable> getByPetTableId(int key) {
        return snapshot.idxPetTableId.getOrDefault(key, Collections.emptyList());
    }



    // FK: pet_table_id → Pet.id

    // FK: skill_ids → Skill.id

    // FK: pet_entry_ids → PetEntry.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countBySkillIdsIndex(int key) {
        return snapshot.idxSkillIds.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByPetEntryIdsIndex(int key) {
        return snapshot.idxPetEntryIds.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByPetEntryPointsIndex(int key) {
        return snapshot.idxPetEntryPoints.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countByPetTableIdIndex(int key) {
        return snapshot.idxPetTableId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<RankedPresetTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<RankedPresetTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            RankedPresetTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public RankedPresetTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<RankedPresetTable> where(Predicate<RankedPresetTable> pred) {
        Snapshot snap = this.snapshot;
        List<RankedPresetTable> result = new ArrayList<>();
        for (RankedPresetTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public RankedPresetTable first(Predicate<RankedPresetTable> pred) {
        Snapshot snap = this.snapshot;
        for (RankedPresetTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}

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
 * Auto-generated config manager for Pet.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PetTableManager {

    private static final PetTableManager INSTANCE = new PetTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PetTableData data;
        final Map<Integer, PetTable> kvData;



        final Map<Integer, List<PetTable>> idxAptitudeMin;

        final Map<Integer, List<PetTable>> idxAptitudeMax;

        final Map<Integer, List<PetTable>> idxSkill;



        Snapshot(PetTableData data,
                 Map<Integer, PetTable> kvData,
                 Map<Integer, List<PetTable>> idxAptitudeMin,
                 Map<Integer, List<PetTable>> idxAptitudeMax,
                 Map<Integer, List<PetTable>> idxSkill) {
            this.data = data;
            this.kvData = kvData;
            this.idxAptitudeMin = idxAptitudeMin;
            this.idxAptitudeMax = idxAptitudeMax;
            this.idxSkill = idxSkill;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PetTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static PetTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PetTableData.Builder builder = PetTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "pet.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "pet.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PetTableData data = builder.build();

        Map<Integer, PetTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<PetTable>> idxAptitudeMin = new HashMap<>();
        Map<Integer, List<PetTable>> idxAptitudeMax = new HashMap<>();
        Map<Integer, List<PetTable>> idxSkill = new HashMap<>();

        for (PetTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getAptitudeMinList()) {
                idxAptitudeMin.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getAptitudeMaxList()) {
                idxAptitudeMax.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getSkillList()) {
                idxSkill.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
        }

        this.snapshot = new Snapshot(data, kvData, idxAptitudeMin, idxAptitudeMax, idxSkill);
    }

    public PetTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PetTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PetTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<PetTable> findByAptitudeMinIndex(int key) {
        return snapshot.idxAptitudeMin.getOrDefault(key, Collections.emptyList());
    }

    public List<PetTable> findByAptitudeMaxIndex(int key) {
        return snapshot.idxAptitudeMax.getOrDefault(key, Collections.emptyList());
    }

    public List<PetTable> findBySkillIndex(int key) {
        return snapshot.idxSkill.getOrDefault(key, Collections.emptyList());
    }






    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByAptitudeMinIndex(int key) {
        return snapshot.idxAptitudeMin.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByAptitudeMaxIndex(int key) {
        return snapshot.idxAptitudeMax.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countBySkillIndex(int key) {
        return snapshot.idxSkill.getOrDefault(key, Collections.emptyList()).size();
    }



    // ---- FindByIds (IN) ----

    public List<PetTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PetTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PetTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PetTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PetTable> where(Predicate<PetTable> pred) {
        Snapshot snap = this.snapshot;
        List<PetTable> result = new ArrayList<>();
        for (PetTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PetTable first(Predicate<PetTable> pred) {
        Snapshot snap = this.snapshot;
        for (PetTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
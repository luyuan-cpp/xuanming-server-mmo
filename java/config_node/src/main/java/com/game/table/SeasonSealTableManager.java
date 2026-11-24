
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
 * Auto-generated config manager for SeasonSeal.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SeasonSealTableManager {

    private static final SeasonSealTableManager INSTANCE = new SeasonSealTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SeasonSealTableData data;
        final Map<Integer, SeasonSealTable> kvData;




        final Map<Integer, List<SeasonSealTable>> idxSeasonId;


        Snapshot(SeasonSealTableData data,
                 Map<Integer, SeasonSealTable> kvData,
                 Map<Integer, List<SeasonSealTable>> idxSeasonId) {
            this.data = data;
            this.kvData = kvData;
            this.idxSeasonId = idxSeasonId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SeasonSealTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static SeasonSealTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SeasonSealTableData.Builder builder = SeasonSealTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "seasonseal.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "seasonseal.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SeasonSealTableData data = builder.build();

        Map<Integer, SeasonSealTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<SeasonSealTable>> idxSeasonId = new HashMap<>();

        for (SeasonSealTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxSeasonId.computeIfAbsent(row.getSeasonId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxSeasonId);
    }

    public SeasonSealTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SeasonSealTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SeasonSealTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<SeasonSealTable> getBySeasonId(int key) {
        return snapshot.idxSeasonId.getOrDefault(key, Collections.emptyList());
    }



    // FK: season_id → SeasonRule.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countBySeasonIdIndex(int key) {
        return snapshot.idxSeasonId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<SeasonSealTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SeasonSealTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SeasonSealTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SeasonSealTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SeasonSealTable> where(Predicate<SeasonSealTable> pred) {
        Snapshot snap = this.snapshot;
        List<SeasonSealTable> result = new ArrayList<>();
        for (SeasonSealTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SeasonSealTable first(Predicate<SeasonSealTable> pred) {
        Snapshot snap = this.snapshot;
        for (SeasonSealTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}

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
 * Auto-generated config manager for TravelPassTier.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TravelPassTierTableManager {

    private static final TravelPassTierTableManager INSTANCE = new TravelPassTierTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TravelPassTierTableData data;
        final Map<Integer, TravelPassTierTable> kvData;




        final Map<Integer, List<TravelPassTierTable>> idxPassId;


        Snapshot(TravelPassTierTableData data,
                 Map<Integer, TravelPassTierTable> kvData,
                 Map<Integer, List<TravelPassTierTable>> idxPassId) {
            this.data = data;
            this.kvData = kvData;
            this.idxPassId = idxPassId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TravelPassTierTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TravelPassTierTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TravelPassTierTableData.Builder builder = TravelPassTierTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "travelpasstier.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "travelpasstier.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TravelPassTierTableData data = builder.build();

        Map<Integer, TravelPassTierTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TravelPassTierTable>> idxPassId = new HashMap<>();

        for (TravelPassTierTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxPassId.computeIfAbsent(row.getPassId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxPassId);
    }

    public TravelPassTierTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TravelPassTierTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TravelPassTierTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<TravelPassTierTable> getByPassId(int key) {
        return snapshot.idxPassId.getOrDefault(key, Collections.emptyList());
    }



    // FK: pass_id → TravelPassSeason.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByPassIdIndex(int key) {
        return snapshot.idxPassId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TravelPassTierTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TravelPassTierTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TravelPassTierTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TravelPassTierTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TravelPassTierTable> where(Predicate<TravelPassTierTable> pred) {
        Snapshot snap = this.snapshot;
        List<TravelPassTierTable> result = new ArrayList<>();
        for (TravelPassTierTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TravelPassTierTable first(Predicate<TravelPassTierTable> pred) {
        Snapshot snap = this.snapshot;
        for (TravelPassTierTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
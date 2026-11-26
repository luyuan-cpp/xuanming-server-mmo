
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
 * Auto-generated config manager for TravelPassSeason.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TravelPassSeasonTableManager {

    private static final TravelPassSeasonTableManager INSTANCE = new TravelPassSeasonTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TravelPassSeasonTableData data;
        final Map<Integer, TravelPassSeasonTable> kvData;





        Snapshot(TravelPassSeasonTableData data,
                 Map<Integer, TravelPassSeasonTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TravelPassSeasonTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static TravelPassSeasonTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TravelPassSeasonTableData.Builder builder = TravelPassSeasonTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "travelpassseason.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "travelpassseason.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TravelPassSeasonTableData data = builder.build();

        Map<Integer, TravelPassSeasonTable> kvData = new HashMap<>(data.getDataCount());

        for (TravelPassSeasonTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public TravelPassSeasonTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TravelPassSeasonTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TravelPassSeasonTable> getKvData() {
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

    public List<TravelPassSeasonTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TravelPassSeasonTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TravelPassSeasonTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TravelPassSeasonTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TravelPassSeasonTable> where(Predicate<TravelPassSeasonTable> pred) {
        Snapshot snap = this.snapshot;
        List<TravelPassSeasonTable> result = new ArrayList<>();
        for (TravelPassSeasonTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TravelPassSeasonTable first(Predicate<TravelPassSeasonTable> pred) {
        Snapshot snap = this.snapshot;
        for (TravelPassSeasonTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
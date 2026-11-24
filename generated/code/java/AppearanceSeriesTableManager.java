
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
 * Auto-generated config manager for AppearanceSeries.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AppearanceSeriesTableManager {

    private static final AppearanceSeriesTableManager INSTANCE = new AppearanceSeriesTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AppearanceSeriesTableData data;
        final Map<Integer, AppearanceSeriesTable> kvData;





        Snapshot(AppearanceSeriesTableData data,
                 Map<Integer, AppearanceSeriesTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AppearanceSeriesTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static AppearanceSeriesTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AppearanceSeriesTableData.Builder builder = AppearanceSeriesTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "appearanceseries.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "appearanceseries.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AppearanceSeriesTableData data = builder.build();

        Map<Integer, AppearanceSeriesTable> kvData = new HashMap<>(data.getDataCount());

        for (AppearanceSeriesTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public AppearanceSeriesTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public AppearanceSeriesTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AppearanceSeriesTable> getKvData() {
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

    public List<AppearanceSeriesTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AppearanceSeriesTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AppearanceSeriesTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AppearanceSeriesTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AppearanceSeriesTable> where(Predicate<AppearanceSeriesTable> pred) {
        Snapshot snap = this.snapshot;
        List<AppearanceSeriesTable> result = new ArrayList<>();
        for (AppearanceSeriesTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AppearanceSeriesTable first(Predicate<AppearanceSeriesTable> pred) {
        Snapshot snap = this.snapshot;
        for (AppearanceSeriesTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
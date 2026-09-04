
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
 * Auto-generated config manager for GlobalVariable.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GlobalVariableTableManager {

    private static final GlobalVariableTableManager INSTANCE = new GlobalVariableTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GlobalVariableTableData data;
        final Map<Integer, GlobalVariableTable> kvData;





        Snapshot(GlobalVariableTableData data,
                 Map<Integer, GlobalVariableTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GlobalVariableTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static GlobalVariableTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GlobalVariableTableData.Builder builder = GlobalVariableTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "globalvariable.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "globalvariable.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GlobalVariableTableData data = builder.build();

        Map<Integer, GlobalVariableTable> kvData = new HashMap<>(data.getDataCount());

        for (GlobalVariableTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public GlobalVariableTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GlobalVariableTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GlobalVariableTable> getKvData() {
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

    public List<GlobalVariableTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GlobalVariableTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GlobalVariableTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GlobalVariableTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GlobalVariableTable> where(Predicate<GlobalVariableTable> pred) {
        Snapshot snap = this.snapshot;
        List<GlobalVariableTable> result = new ArrayList<>();
        for (GlobalVariableTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GlobalVariableTable first(Predicate<GlobalVariableTable> pred) {
        Snapshot snap = this.snapshot;
        for (GlobalVariableTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
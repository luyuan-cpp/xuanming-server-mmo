
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
 * Auto-generated config manager for TrialJournalMilestone.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TrialJournalMilestoneTableManager {

    private static final TrialJournalMilestoneTableManager INSTANCE = new TrialJournalMilestoneTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TrialJournalMilestoneTableData data;
        final Map<Integer, TrialJournalMilestoneTable> kvData;




        final Map<Integer, List<TrialJournalMilestoneTable>> idxScopeChapterId;


        Snapshot(TrialJournalMilestoneTableData data,
                 Map<Integer, TrialJournalMilestoneTable> kvData,
                 Map<Integer, List<TrialJournalMilestoneTable>> idxScopeChapterId) {
            this.data = data;
            this.kvData = kvData;
            this.idxScopeChapterId = idxScopeChapterId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TrialJournalMilestoneTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TrialJournalMilestoneTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TrialJournalMilestoneTableData.Builder builder = TrialJournalMilestoneTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "trialjournalmilestone.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "trialjournalmilestone.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TrialJournalMilestoneTableData data = builder.build();

        Map<Integer, TrialJournalMilestoneTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TrialJournalMilestoneTable>> idxScopeChapterId = new HashMap<>();

        for (TrialJournalMilestoneTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxScopeChapterId.computeIfAbsent(row.getScopeChapterId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxScopeChapterId);
    }

    public TrialJournalMilestoneTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TrialJournalMilestoneTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TrialJournalMilestoneTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<TrialJournalMilestoneTable> getByScopeChapterId(int key) {
        return snapshot.idxScopeChapterId.getOrDefault(key, Collections.emptyList());
    }




    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByScopeChapterIdIndex(int key) {
        return snapshot.idxScopeChapterId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TrialJournalMilestoneTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TrialJournalMilestoneTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TrialJournalMilestoneTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TrialJournalMilestoneTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TrialJournalMilestoneTable> where(Predicate<TrialJournalMilestoneTable> pred) {
        Snapshot snap = this.snapshot;
        List<TrialJournalMilestoneTable> result = new ArrayList<>();
        for (TrialJournalMilestoneTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TrialJournalMilestoneTable first(Predicate<TrialJournalMilestoneTable> pred) {
        Snapshot snap = this.snapshot;
        for (TrialJournalMilestoneTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
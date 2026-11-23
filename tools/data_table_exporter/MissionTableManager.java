
package ;

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
 * Auto-generated config manager for Mission.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class MissionTableManager {

    private static final MissionTableManager INSTANCE = new MissionTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final MissionTableData data;
        final Map<Integer, MissionTable> kvData;

        final Map<Integer, MissionTable> kvIdData;





        Snapshot(MissionTableData data,
                 Map<Integer, MissionTable> kvData,
                 Map<Integer, MissionTable> kvIdData) {
            this.data = data;
            this.kvData = kvData;
            this.kvIdData = kvIdData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            MissionTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static MissionTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        MissionTableData.Builder builder = MissionTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "mission.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "mission.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        MissionTableData data = builder.build();

        Map<Integer, MissionTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, MissionTable> kvIdData = new HashMap<>(data.getDataCount());

        for (MissionTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            kvIdData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData, kvIdData);
    }

    public MissionTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public MissionTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, MissionTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }


    public MissionTable findById(int key) {
        return snapshot.kvIdData.get(key);
    }









    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }


    public boolean existsById(int key) {
        return snapshot.kvIdData.containsKey(key);
    }


    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }





    // ---- FindByIds (IN) ----

    public List<MissionTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<MissionTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            MissionTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public MissionTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<MissionTable> where(Predicate<MissionTable> pred) {
        Snapshot snap = this.snapshot;
        List<MissionTable> result = new ArrayList<>();
        for (MissionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public MissionTable first(Predicate<MissionTable> pred) {
        Snapshot snap = this.snapshot;
        for (MissionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}
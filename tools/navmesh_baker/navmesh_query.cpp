// 离线读取真实 MSET，验证原脚点、完整路径和直线阻挡；输入保持 Unity Y-up 坐标。
// 仅支持本工具链生成的地面多边形，不加载 off-mesh 连接或任何运行服务。
#include <array>
#include <cmath>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <sstream>
#include <stdexcept>
#include <string>
#include <vector>

#include "Detour/DetourCommon.h"
#include "Detour/DetourNavMesh.h"
#include "Detour/DetourNavMeshQuery.h"

namespace
{
using Point = std::array<dtReal, 3>;
constexpr std::size_t kMaxFileBytes = 64 * 1024 * 1024;
constexpr int kMaxTiles = 65536;
constexpr int kMaxPolys = 1 << 22;
constexpr int kMaxQueries = 4096;
constexpr dtReal kPointTolerance = 0.02;
constexpr dtReal kExtents[3] = {2, 4, 2};

struct NavMeshSetHeader
{
    int32_t magic, version, numTiles;
    dtNavMeshParams params;
};
struct NavMeshTileHeader
{
    dtTileRef tileRef;
    int32_t dataSize;
};

void Require(bool valid, const char* message)
{
    if (!valid) throw std::runtime_error(message);
}

template<class T>
T Read(const std::vector<unsigned char>& bytes, std::size_t offset)
{
    Require(offset <= bytes.size() && sizeof(T) <= bytes.size() - offset,
            "truncated MSET structure");
    T result;
    std::memcpy(&result, bytes.data() + offset, sizeof(T));
    return result;
}

std::size_t Section(std::size_t& offset, std::size_t count, std::size_t elementSize,
                    std::size_t payloadBytes)
{
    Require(payloadBytes <= kMaxFileBytes && elementSize > 0 &&
            count <= payloadBytes / elementSize, "tile section count exceeds payload size");
    const auto rawBytes = count * elementSize;
    // 与当前 double ABI 的 dtAlign 相同；使用 size_t，不把不可信长度窄化成 int。
    const auto alignment = static_cast<std::size_t>(dtAlign(1));
    const auto sectionBytes = (rawBytes + alignment - 1) & ~(alignment - 1);
    Require(offset <= payloadBytes && sectionBytes <= payloadBytes - offset,
            "tile section exceeds payload size");
    const auto start = offset;
    offset += sectionBytes;
    return start;
}

// 原生 addTile 不验证数组长度和全部索引；必须先在受限字节缓冲上逐项检查。
void ValidateTile(const std::vector<unsigned char>& bytes)
{
    const auto header = Read<dtMeshHeader>(bytes, 0);
    // UE5 分支已移除 tile magic，仅保留 version；外层 MSET magic 在 LoadMesh 校验。
    Require(header.version == DT_NAVMESH_VERSION, "unsupported Detour tile version");
    Require(header.polyCount > 0 && header.vertCount >= 3 && header.maxLinkCount > 0,
            "invalid tile counts");
    Require(header.offMeshConCount == 0 && header.offMeshBase == header.polyCount &&
            header.detailMeshCount == header.polyCount, "only ground polygon tiles are supported");
    Require(std::abs(static_cast<int64_t>(header.x)) <= 1000000 &&
            std::abs(static_cast<int64_t>(header.y)) <= 1000000, "tile coordinates exceed limit");
    for (int axis = 0; axis < 3; ++axis)
        Require(std::isfinite(header.bmin[axis]) && std::isfinite(header.bmax[axis]) &&
                header.bmin[axis] <= header.bmax[axis], "invalid tile bounds");

    std::size_t offset = 0;
    Section(offset, 1, sizeof(dtMeshHeader), bytes.size());
    const auto vertices = Section(offset, header.vertCount, 3 * sizeof(dtReal), bytes.size());
    const auto polygons = Section(offset, header.polyCount, sizeof(dtPoly), bytes.size());
    Section(offset, header.maxLinkCount, sizeof(dtLink), bytes.size());
    const auto details = Section(offset, header.detailMeshCount, sizeof(dtPolyDetail), bytes.size());
    const auto detailVertices = Section(offset, header.detailVertCount, 3 * sizeof(dtReal), bytes.size());
    const auto detailTriangles = Section(offset, header.detailTriCount, 4, bytes.size());
    const auto bvNodes = Section(offset, header.bvNodeCount, sizeof(dtBVNode), bytes.size());
    Section(offset, header.offMeshConCount, sizeof(dtOffMeshConnection), bytes.size());
    Require(offset == bytes.size(), "tile payload size does not match its counts");
    for (std::size_t i = 0; i < static_cast<std::size_t>(header.vertCount) * 3; ++i)
        Require(std::isfinite(Read<dtReal>(bytes, vertices + i * sizeof(dtReal))),
                "nonfinite tile vertex");
    for (std::size_t i = 0; i < static_cast<std::size_t>(header.detailVertCount) * 3; ++i)
        Require(std::isfinite(Read<dtReal>(bytes, detailVertices + i * sizeof(dtReal))),
                "nonfinite detail vertex");
    for (int i = 0; i < header.polyCount; ++i)
    {
        const auto poly = Read<dtPoly>(bytes, polygons + i * sizeof(dtPoly));
        Require(poly.getType() == DT_POLYTYPE_GROUND && poly.vertCount >= 3 &&
                poly.vertCount <= DT_VERTS_PER_POLYGON, "invalid ground polygon");
        for (int vertex = 0; vertex < poly.vertCount; ++vertex)
        {
            Require(poly.verts[vertex] < header.vertCount, "polygon vertex index out of range");
            const auto neighbor = poly.neis[vertex];
            Require((neighbor & DT_EXT_LINK) ? ((neighbor & ~DT_EXT_LINK) <= 7) :
                    (neighbor <= header.polyCount), "polygon neighbor index out of range");
        }
        const auto detail = Read<dtPolyDetail>(bytes, details + i * sizeof(dtPolyDetail));
        const auto vertexBegin = static_cast<std::size_t>(detail.vertBase);
        const auto triangleBegin = static_cast<std::size_t>(detail.triBase);
        Require(vertexBegin <= header.detailVertCount &&
                detail.vertCount <= header.detailVertCount - vertexBegin &&
                triangleBegin <= header.detailTriCount &&
                detail.triCount <= header.detailTriCount - triangleBegin &&
                detail.triCount > 0, "detail mesh index out of range");
        const auto triangleEnd = triangleBegin + detail.triCount;
        for (std::size_t triangle = triangleBegin; triangle < triangleEnd; ++triangle)
            for (int vertex = 0; vertex < 3; ++vertex)
                Require(bytes[detailTriangles + triangle * 4 + vertex] < poly.vertCount + detail.vertCount,
                        "detail triangle index out of range");
    }
    for (int i = 0; i < header.bvNodeCount; ++i)
    {
        const auto node = Read<dtBVNode>(bytes, bvNodes + i * sizeof(dtBVNode));
        Require(node.i >= 0 ? node.i < header.polyCount :
                -static_cast<int64_t>(node.i) <= header.bvNodeCount - i,
                "BV tree index out of range");
        for (int axis = 0; axis < 3; ++axis)
            Require(node.bmin[axis] <= node.bmax[axis], "invalid BV tree bounds");
    }
}

void LoadMesh(const std::string& path, dtNavMesh& mesh)
{
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    Require(static_cast<bool>(input), "cannot open MSET file");
    const auto length = input.tellg();
    Require(length > 0 && length <= static_cast<std::streamoff>(kMaxFileBytes),
            "MSET file size exceeds limit or is empty");
    std::vector<unsigned char> bytes(static_cast<std::size_t>(length));
    input.seekg(0);
    Require(static_cast<bool>(input.read(reinterpret_cast<char*>(bytes.data()), length)),
            "cannot read MSET file");
    const auto header = Read<NavMeshSetHeader>(bytes, 0);
    Require(header.magic == ('M' << 24 | 'S' << 16 | 'E' << 8 | 'T') && header.version == 1,
            "unsupported MSET header");
    const auto& params = header.params;
    Require(header.numTiles > 0 && header.numTiles <= kMaxTiles &&
            params.maxTiles >= header.numTiles && params.maxTiles <= kMaxTiles &&
            params.maxPolys > 0 && params.maxPolys <= kMaxPolys, "invalid MSET allocation counts");
    Require((params.maxTiles & (params.maxTiles - 1)) == 0 &&
            (params.maxPolys & (params.maxPolys - 1)) == 0, "MSET capacities must be powers of two");
    Require(std::isfinite(params.walkableHeight) && params.walkableHeight > 0 &&
            std::isfinite(params.walkableRadius) && params.walkableRadius >= 0 &&
            std::isfinite(params.walkableClimb) && params.walkableClimb >= 0 &&
            std::isfinite(params.bvQuantFactor) && params.bvQuantFactor > 0 &&
            std::isfinite(params.tileWidth) && params.tileWidth > 0 &&
            std::isfinite(params.tileHeight) && params.tileHeight > 0, "invalid MSET parameters");
    for (const auto coordinate : params.orig)
        Require(std::isfinite(coordinate), "nonfinite MSET origin");
    Require(dtStatusSucceed(mesh.init(&params)), "Detour mesh initialization failed");
    std::size_t offset = sizeof(header);
    for (int i = 0; i < header.numTiles; ++i)
    {
        const auto tile = Read<NavMeshTileHeader>(bytes, offset);
        offset += sizeof(tile);
        Require(tile.tileRef != 0 && tile.dataSize > 0 &&
                static_cast<std::size_t>(tile.dataSize) <= bytes.size() - offset,
                "invalid or truncated MSET tile");
        const auto salt = mesh.decodePolyIdSalt(tile.tileRef);
        const auto tileIndex = mesh.decodePolyIdTile(tile.tileRef);
        Require(salt != 0 && tileIndex < static_cast<unsigned int>(params.maxTiles) &&
                mesh.encodePolyId(salt, tileIndex, 0) == tile.tileRef, "invalid MSET tile reference");
        std::vector<unsigned char> payload(bytes.begin() + offset, bytes.begin() + offset + tile.dataSize);
        ValidateTile(payload);
        const auto tileHeader = Read<dtMeshHeader>(payload, 0);
        Require(tileHeader.polyCount <= params.maxPolys, "tile polygon count exceeds capacity");
        auto* allocation = static_cast<unsigned char*>(dtAlloc(tile.dataSize, DT_ALLOC_TEMP));
        Require(allocation != nullptr, "tile allocation failed");
        std::memcpy(allocation, payload.data(), tile.dataSize);
        const auto status = mesh.addTile(allocation, tile.dataSize, DT_TILE_FREE_DATA, tile.tileRef, nullptr);
        if (dtStatusFailed(status)) dtFree(allocation, DT_ALLOC_TEMP);
        Require(dtStatusSucceed(status), "Detour addTile failed");
        offset += tile.dataSize;
    }
    Require(offset == bytes.size(), "unexpected trailing MSET bytes");
}

std::vector<dtReal> ParseCoordinates(const std::string& text, std::size_t count)
{
    std::vector<dtReal> result;
    const char* cursor = text.c_str();
    for (std::size_t i = 0; i < count; ++i)
    {
        char* end = nullptr;
        const auto value = std::strtod(cursor, &end);
        Require(cursor != end && std::isfinite(value), "invalid coordinate argument");
        result.push_back(value);
        Require(i + 1 < count ? *end == ',' : *end == '\0', "invalid coordinate count or suffix");
        cursor = end + (i + 1 < count ? 1 : 0);
    }
    return result;
}

void WritePoint(std::ostream& output, const dtReal* value)
{
    output << '[' << value[0] << ',' << value[1] << ',' << value[2] << ']';
}

struct PointResult
{
    dtPolyRef ref = 0;
    Point nearest{};
    bool onMesh = false;
};

PointResult QueryPoint(const dtNavMeshQuery& query, const Point& point)
{
    const dtQueryFilter filter;
    PointResult result;
    Require(dtStatusSucceed(query.findNearestPoly(point.data(), kExtents, &filter,
                                                  &result.ref, result.nearest.data())),
            "Detour nearest-point query failed");
    result.onMesh = result.ref != 0;
    for (int axis = 0; axis < 3; ++axis)
        result.onMesh = result.onMesh && std::abs(result.nearest[axis] - point[axis]) <= kPointTolerance;
    return result;
}

void WritePointResult(std::ostream& output, const Point& point, const PointResult& result)
{
    output << "{\"input\":";
    WritePoint(output, point.data());
    output << ",\"found\":" << (result.ref != 0) << ",\"onMesh\":" << result.onMesh
           << ",\"polyRef\":" << result.ref << ",\"nearest\":";
    if (result.ref != 0) WritePoint(output, result.nearest.data());
    else output << "null";
    output << '}';
}

void WriteRoute(std::ostream& output, const dtNavMeshQuery& query, const std::array<Point, 2>& route)
{
    const auto start = QueryPoint(query, route[0]);
    const auto end = QueryPoint(query, route[1]);
    const dtQueryFilter filter;
    dtQueryResult corridor, straight;
    dtStatus pathStatus = 0;
    bool complete = false;
    dtReal hitT = 0;
    Point normal{};
    bool raycastRun = false;
    if (start.onMesh && end.onMesh)
    {
        pathStatus = query.findPath(start.ref, end.ref, start.nearest.data(), end.nearest.data(),
                                    DT_REAL_MAX, &filter, corridor, nullptr);
        Require(dtStatusSucceed(pathStatus) && !(pathStatus & DT_OUT_OF_NODES),
                "Detour path query failed or exhausted nodes");
        complete = corridor.size() > 0 && !(pathStatus & DT_PARTIAL_RESULT) &&
                   corridor.getRef(corridor.size() - 1) == end.ref;
        if (complete)
        {
            std::vector<dtPolyRef> refs(corridor.size());
            corridor.copyRefs(refs.data(), static_cast<int>(refs.size()));
            const auto status = query.findStraightPath(start.nearest.data(), end.nearest.data(),
                                                       refs.data(), static_cast<int>(refs.size()), straight);
            Require(dtStatusSucceed(status) && !(status & DT_BUFFER_TOO_SMALL),
                    "Detour straight-path query failed");
        }
    }
    if (start.onMesh)
    {
        std::array<dtPolyRef, 4096> visited{};
        int count = 0;
        const auto status = query.raycast(start.ref, start.nearest.data(), route[1].data(), &filter,
                                          &hitT, normal.data(), visited.data(), &count,
                                          static_cast<int>(visited.size()));
        Require(dtStatusSucceed(status) && !(status & DT_BUFFER_TOO_SMALL), "Detour raycast failed");
        raycastRun = true;
    }
    output << "{\"start\":";
    WritePoint(output, route[0].data());
    output << ",\"end\":";
    WritePoint(output, route[1].data());
    output << ",\"startOnMesh\":" << start.onMesh << ",\"endOnMesh\":" << end.onMesh
           << ",\"complete\":" << complete << ",\"pathStatus\":" << pathStatus
           << ",\"polyCount\":" << corridor.size() << ",\"corners\":[";
    for (int i = 0; i < straight.size(); ++i)
    {
        if (i > 0) output << ',';
        WritePoint(output, straight.getPos(i));
    }
    output << "],\"raycast\":";
    if (!raycastRun) output << "null";
    else
    {
        const bool hit = hitT <= 1;
        const auto t = hit ? hitT : static_cast<dtReal>(1);
        Point position{};
        for (int axis = 0; axis < 3; ++axis)
            position[axis] = start.nearest[axis] + (route[1][axis] - start.nearest[axis]) * t;
        output << "{\"hit\":" << hit << ",\"t\":" << t << ",\"position\":";
        WritePoint(output, position.data());
        output << ",\"normal\":";
        WritePoint(output, normal.data());
        output << '}';
    }
    output << '}';
}
} // namespace

int main(int argc, char** argv)
{
    try
    {
        std::string navPath;
        std::vector<Point> points;
        std::vector<std::array<Point, 2>> routes;
        for (int i = 1; i < argc; ++i)
        {
            const std::string argument = argv[i];
            Require(i + 1 < argc, "missing argument value");
            const std::string value = argv[++i];
            if (argument == "--nav")
            {
                Require(navPath.empty(), "duplicate --nav argument");
                navPath = value;
            }
            else if (argument == "--point")
            {
                const auto values = ParseCoordinates(value, 3);
                points.push_back({values[0], values[1], values[2]});
            }
            else if (argument == "--route")
            {
                const auto values = ParseCoordinates(value, 6);
                routes.push_back({Point{values[0], values[1], values[2]},
                                  Point{values[3], values[4], values[5]}});
            }
            else throw std::runtime_error("unknown argument");
            Require(points.size() + routes.size() <= kMaxQueries, "query count exceeds limit");
        }
        Require(!navPath.empty() && (!points.empty() || !routes.empty()),
                "usage: navmesh_query --nav <MSET> [--point x,y,z ...] [--route x,y,z,x,y,z ...]");
        dtNavMesh mesh;
        LoadMesh(navPath, mesh);
        dtNavMeshQuery query;
        Require(dtStatusSucceed(query.init(&mesh, 65536)), "Detour query initialization failed");
        // 先完成全部查询再写 stdout；失败不能留下外观完整的半份 JSON。
        std::ostringstream output;
        output << std::setprecision(17) << std::boolalpha
               << "{\"schemaVersion\":1,\"coordinateSystem\":\"Unity Y-up X/Z\",\"points\":[";
        for (std::size_t i = 0; i < points.size(); ++i)
        {
            if (i > 0) output << ',';
            WritePointResult(output, points[i], QueryPoint(query, points[i]));
        }
        output << "],\"routes\":[";
        for (std::size_t i = 0; i < routes.size(); ++i)
        {
            if (i > 0) output << ',';
            WriteRoute(output, query, routes[i]);
        }
        output << "]}\n";
        std::cout << output.str();
        return 0;
    }
    catch (const std::exception& error)
    {
        std::cerr << "error: " << error.what() << '\n';
        return 1;
    }
}

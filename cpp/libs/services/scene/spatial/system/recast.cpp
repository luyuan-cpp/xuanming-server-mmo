#include "recast.h"

#include "muduo/base/Logging.h"

#include "Detour/DetourNavMeshQuery.h"

struct NavMeshSetHeader
{
	int32_t magic{0};
	int32_t version{ 0 };
	int32_t numTiles{ 0 };
	dtNavMeshParams params;
};

struct NavMeshTileHeader
{
	dtTileRef tileRef{ 0 };
	int32_t dataSize{ 0 };
};

constexpr int NAVMESHSET_MAGIC = 'M' << 24 | 'S' << 16 | 'E' << 8 | 'T'; //'MSET';
constexpr int NAVMESHSET_VERSION = 1;

class StdFilePtrDeleter {
public:
	void operator()(std::FILE* fp) { std::fclose(fp); }
};

using StdFilePtr = std::unique_ptr<std::FILE, StdFilePtrDeleter>;

bool RecastSystem::LoadNavMesh(const char* path, dtNavMesh* mesh)
{
	StdFilePtr fp(std::fopen(path, "rb"));
	if (fp == nullptr)
	{
		LOG_ERROR << "nav bin file not found: " << path;
		return false;
	}

	// Read header.
	NavMeshSetHeader header;
	size_t readLen = std::fread(&header, sizeof(NavMeshSetHeader), 1, fp.get());
	if (readLen != 1)
	{
		LOG_ERROR << "load nav bin header " << path;
		return false;
	}
	if (header.magic != NAVMESHSET_MAGIC)
	{
		LOG_ERROR << "load nav bin header magic" << path;
		return false;
	}
	if (header.version != NAVMESHSET_VERSION)
	{
		LOG_ERROR << "load nav bin header version " << path;
		return false;
	}

	dtStatus status = mesh->init(&header.params);
	if (dtStatusFailed(status))
	{
		LOG_ERROR << "load nav init nav mesh " << path;
		return false;
	}

	// Read tiles.
	for (int i = 0; i < header.numTiles; ++i)
	{
		NavMeshTileHeader tileHeader;
		readLen = std::fread(&tileHeader, sizeof(tileHeader), 1, fp.get());
		if (readLen != 1)
		{
			LOG_ERROR << "load nav read tile header " << path;
			return false;
		}

		if (!tileHeader.tileRef || !tileHeader.dataSize)
			break;

		unsigned char* data = static_cast<unsigned char*>(dtAlloc(tileHeader.dataSize, DT_ALLOC_TEMP));
		if (!data) break;
		std::memset(data, 0, tileHeader.dataSize);
		readLen = fread(data, tileHeader.dataSize, 1, fp.get());
		if (readLen != 1)
		{
			dtFree(data, DT_ALLOC_TEMP);
			LOG_ERROR << "load nav read navdata " << path;
			return false;
		}
		mesh->addTile(data, tileHeader.dataSize, DT_TILE_FREE_DATA, tileHeader.tileRef, 0);
	}

	return true;
}


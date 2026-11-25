#include "table_bundle/table_bundle_manager.h"

#include <algorithm>
#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <utility>

#ifdef __linux__
#include <unistd.h>
#else
#include <process.h>
#endif

#include "google/protobuf/struct.pb.h"
#include "google/protobuf/util/json_util.h"
#include "muduo/base/Logging.h"

#include "config.h"
#include "metrics/metrics_registry.h"
#include "metrics/prometheus_text.h"
#include <node_config_manager.h>
#include "utils/encode/sha256.h"

namespace table_bundle
{

namespace
{

using google::protobuf::Struct;
using google::protobuf::Value;

Hooks &GlobalHooks()
{
	static Hooks hooks;
	return hooks;
}

std::string NonEmptyEnv(const char *name)
{
	const char *value = std::getenv(name);
	return (value != nullptr && value[0] != '\0') ? std::string(value) : std::string{};
}

int CurrentPid()
{
#ifdef __linux__
	return static_cast<int>(::getpid());
#else
	return ::_getpid();
#endif
}

bool IsLowerHex(std::string_view value, std::size_t expectedLen)
{
	if (value.size() != expectedLen)
	{
		return false;
	}
	for (const char c : value)
	{
		const bool ok = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
		if (!ok)
		{
			return false;
		}
	}
	return true;
}

bool IsSafeRelativeFilename(std::string_view file)
{
	if (file.empty() || file == "." || file == "..")
	{
		return false;
	}
	for (const char c : file)
	{
		if (c == '/' || c == '\\' || c == '\0')
		{
			return false;
		}
	}
	return true;
}

bool ReadWholeFile(const std::filesystem::path &path, std::string &out, std::string &err)
{
	std::ifstream in(path, std::ios::binary);
	if (!in.is_open())
	{
		err = "failed to open file: " + path.string();
		return false;
	}
	std::ostringstream ss;
	ss << in.rdbuf();
	if (in.bad())
	{
		err = "failed to read file: " + path.string();
		return false;
	}
	out = ss.str();
	return true;
}

const Value *FindField(const Struct &obj, const std::string &key)
{
	const auto it = obj.fields().find(key);
	return it != obj.fields().end() ? &it->second : nullptr;
}

bool ReadString(const Struct &obj, const std::string &key, std::string &out)
{
	const Value *v = FindField(obj, key);
	if (v == nullptr || v->kind_case() != Value::kStringValue)
	{
		return false;
	}
	out = v->string_value();
	return true;
}

bool ReadUint64(const Struct &obj, const std::string &key, uint64_t &out)
{
	const Value *v = FindField(obj, key);
	if (v == nullptr || v->kind_case() != Value::kNumberValue)
	{
		return false;
	}
	const double num = v->number_value();
	if (num < 0.0)
	{
		return false;
	}
	out = static_cast<uint64_t>(num);
	return true;
}

bool ReadBoolOrDefault(const Struct &obj, const std::string &key, bool fallback)
{
	const Value *v = FindField(obj, key);
	if (v == nullptr || v->kind_case() != Value::kBoolValue)
	{
		return fallback;
	}
	return v->bool_value();
}

bool ParsePointerTarget(const Value *v, std::optional<PointerTarget> &out, std::string &err)
{
	out.reset();
	if (v == nullptr || v->kind_case() == Value::kNullValue)
	{
		return true;
	}
	if (v->kind_case() != Value::kStructValue)
	{
		err = "pointer target entry must be a JSON object";
		return false;
	}
	const Struct &s = v->struct_value();
	PointerTarget target;
	if (!ReadString(s, "content_digest", target.contentDigest) || target.contentDigest.empty())
	{
		return true;
	}
	if (!IsLowerHex(target.contentDigest, 64))
	{
		err = "pointer target content_digest must be 64 lowercase hex characters";
		return false;
	}
	ReadString(s, "bundle_sha256", target.bundleSha256);
	ReadUint64(s, "version", target.version);
	ReadUint64(s, "activate_at_ms", target.activateAtMs);
	target.immediate = ReadBoolOrDefault(s, "immediate", false);
	out = std::move(target);
	return true;
}

std::string ArtifactDigestFor(const ManifestTableEntry &entry, bool useBinary)
{
	const std::string_view wantKind = useBinary ? "binary" : "json";
	for (const auto &art : entry.artifacts)
	{
		if (art.kind == wantKind)
		{
			return art.sha256;
		}
	}
	return {};
}

std::string DigestPrefix12(std::string_view digest)
{
	return std::string(digest.substr(0, std::min<std::size_t>(12, digest.size())));
}

std::string EscapeJson(std::string_view s)
{
	std::string out;
	out.reserve(s.size() + 8);
	for (const char c : s)
	{
		switch (c)
		{
		case '\\': out += "\\\\"; break;
		case '"': out += "\\\""; break;
		case '\n': out += "\\n"; break;
		case '\r': out += "\\r"; break;
		case '\t': out += "\\t"; break;
		default:
			if (static_cast<unsigned char>(c) >= 0x20)
			{
				out.push_back(c);
			}
			break;
		}
	}
	return out;
}

bool AtomicWriteFile(const std::filesystem::path &targetPath, const std::string &content, std::string &err)
{
	std::error_code ec;
	if (targetPath.has_parent_path())
	{
		std::filesystem::create_directories(targetPath.parent_path(), ec);
		if (ec)
		{
			err = "failed to create status directory: " + ec.message();
			return false;
		}
	}
	const auto tmpPath = targetPath.parent_path() /
						 (targetPath.filename().string() + ".tmp." + std::to_string(CurrentPid()));
	{
		std::ofstream out(tmpPath, std::ios::binary | std::ios::trunc);
		if (!out.is_open())
		{
			err = "failed to open temp status file: " + tmpPath.string();
			return false;
		}
		out.write(content.data(), static_cast<std::streamsize>(content.size()));
		out.flush();
		if (!out.good())
		{
			err = "failed to write temp status file: " + tmpPath.string();
			return false;
		}
	}
	std::filesystem::rename(tmpPath, targetPath, ec);
	if (ec)
	{
		std::filesystem::remove(tmpPath, ec);
		err = "failed to atomically replace status file: " + targetPath.string();
		return false;
	}
	return true;
}

} // namespace

bool VerifyTableDirManifest(const std::string &dir,
							const bool useBinary,
							const std::string_view expectedSchemaDigest,
							BundleManifest &out,
							std::string &err)
{
	const std::filesystem::path root(dir);
	const std::filesystem::path manifestPath = root / kManifestFilename;
	std::string raw;
	if (!ReadWholeFile(manifestPath, raw, err))
	{
		return false;
	}

	Struct doc;
	const auto parsed = google::protobuf::util::JsonStringToMessage(raw, &doc);
	if (!parsed.ok())
	{
		err = std::string("invalid manifest.json: ") + std::string(parsed.message());
		return false;
	}

	BundleManifest manifest;
	if (!ReadUint64(doc, "schema_version", manifest.schemaVersion) || manifest.schemaVersion < 2)
	{
		err = "manifest.json schema_version must be >= 2";
		return false;
	}
	if (!ReadString(doc, "schema_digest", manifest.schemaDigest) || !IsLowerHex(manifest.schemaDigest, 16))
	{
		err = "manifest.json missing valid 16-hex schema_digest";
		return false;
	}
	if (!expectedSchemaDigest.empty() && manifest.schemaDigest != expectedSchemaDigest)
	{
		err = "manifest schema_digest mismatch: expected=" + std::string(expectedSchemaDigest) +
			  " actual=" + manifest.schemaDigest;
		return false;
	}
	if (!ReadString(doc, "content_digest", manifest.contentDigest) || !IsLowerHex(manifest.contentDigest, 64))
	{
		err = "manifest.json missing valid 64-hex content_digest";
		return false;
	}
	ReadUint64(doc, "version", manifest.version);

	const Value *tablesVal = FindField(doc, "tables");
	if (tablesVal == nullptr || tablesVal->kind_case() != Value::kListValue)
	{
		err = "manifest.json missing tables array";
		return false;
	}

	const std::string_view activeKind = useBinary ? "binary" : "json";
	for (const Value &item : tablesVal->list_value().values())
	{
		if (item.kind_case() != Value::kStructValue)
		{
			err = "manifest.json table entry must be an object";
			return false;
		}
		const Struct &ts = item.struct_value();
		ManifestTableEntry entry;
		if (!ReadString(ts, "name", entry.name) || entry.name.empty())
		{
			err = "manifest.json table entry missing name";
			return false;
		}
		entry.hotReload = ReadBoolOrDefault(ts, "hot_reload", false);
		entry.idsPermanent = ReadBoolOrDefault(ts, "ids_permanent", false);
		ReadUint64(ts, "rows", entry.rows);

		const Value *artsVal = FindField(ts, "artifacts");
		if (artsVal == nullptr || artsVal->kind_case() != Value::kListValue)
		{
			err = "manifest.json table " + entry.name + " missing artifacts";
			return false;
		}

		bool verifiedActiveFormat = false;
		for (const Value &artVal : artsVal->list_value().values())
		{
			if (artVal.kind_case() != Value::kStructValue)
			{
				err = "manifest.json artifact entry for " + entry.name + " must be an object";
				return false;
			}
			const Struct &as = artVal.struct_value();
			ManifestArtifact art;
			if (!ReadString(as, "kind", art.kind) ||
				!ReadString(as, "file", art.file) ||
				!ReadUint64(as, "size", art.size) ||
				!ReadString(as, "sha256", art.sha256))
			{
				err = "manifest.json artifact entry incomplete for table " + entry.name;
				return false;
			}
			if (!IsSafeRelativeFilename(art.file) || !IsLowerHex(art.sha256, 64))
			{
				err = "manifest.json invalid artifact filename or sha256 for table " + entry.name;
				return false;
			}
			if (art.kind == activeKind)
			{
				std::string fileBytes;
				if (!ReadWholeFile(root / art.file, fileBytes, err))
				{
					return false;
				}
				if (static_cast<uint64_t>(fileBytes.size()) != art.size)
				{
					err = "table artifact size mismatch for " + art.file +
						  ": expected=" + std::to_string(art.size) +
						  " actual=" + std::to_string(fileBytes.size());
					return false;
				}
				const std::string actualHash = Sha256::HashToHex(fileBytes);
				if (actualHash != art.sha256)
				{
					err = "table artifact sha256 mismatch for " + art.file;
					return false;
				}
				verifiedActiveFormat = true;
			}
			entry.artifacts.push_back(std::move(art));
		}
		if (!verifiedActiveFormat)
		{
			err = "manifest.json missing active format artifact (" + std::string(activeKind) +
				  ") for table " + entry.name;
			return false;
		}
		manifest.tables.emplace(entry.name, std::move(entry));
	}

	out = std::move(manifest);
	return true;
}

bool ReadPointerLine(const std::string &pointerPath,
					 const std::string_view schemaDigest,
					 std::optional<PointerLine> &out,
					 std::string &err)
{
	out.reset();
	std::string raw;
	if (!ReadWholeFile(pointerPath, raw, err))
	{
		return false;
	}

	Struct doc;
	const auto parsed = google::protobuf::util::JsonStringToMessage(raw, &doc);
	if (!parsed.ok())
	{
		err = std::string("invalid POINTER.json: ") + std::string(parsed.message());
		return false;
	}

	const Struct *lineStruct = nullptr;
	if (const Value *linesVal = FindField(doc, "lines"); linesVal != nullptr)
	{
		if (linesVal->kind_case() != Value::kStructValue)
		{
			err = "POINTER.json lines field must be a JSON object";
			return false;
		}
		const Value *lineVal = FindField(linesVal->struct_value(), std::string(schemaDigest));
		if (lineVal == nullptr || lineVal->kind_case() == Value::kNullValue)
		{
			return true;
		}
		if (lineVal->kind_case() != Value::kStructValue)
		{
			err = "POINTER.json line entry must be a JSON object";
			return false;
		}
		lineStruct = &lineVal->struct_value();
	}
	else
	{
		// 兼容单线扁平 POINTER.json(本地调试夹具):若带 schema_digest 且不匹配则视为无本线指针。
		std::string docSchema;
		if (ReadString(doc, "schema_digest", docSchema) && !docSchema.empty() && docSchema != schemaDigest)
		{
			return true;
		}
		lineStruct = &doc;
	}

	PointerLine line;
	if (!ParsePointerTarget(FindField(*lineStruct, "current"), line.current, err))
	{
		return false;
	}
	if (!ParsePointerTarget(FindField(*lineStruct, "next"), line.next, err))
	{
		return false;
	}
	ReadUint64(*lineStruct, "updated_at_ms", line.updatedAtMs);
	ReadString(*lineStruct, "updated_by", line.updatedBy);
	ReadString(*lineStruct, "reason", line.reason);
	out = std::move(line);
	return true;
}

std::vector<std::string> DiffChangedTables(const BundleManifest &current,
										   const BundleManifest &candidate,
										   const bool useBinary)
{
	std::vector<std::string> changed;
	for (const auto &[name, candEntry] : candidate.tables)
	{
		const auto it = current.tables.find(name);
		if (it == current.tables.end() ||
			ArtifactDigestFor(it->second, useBinary) != ArtifactDigestFor(candEntry, useBinary))
		{
			changed.push_back(name);
		}
	}
	for (const auto &[name, _] : current.tables)
	{
		if (candidate.tables.find(name) == candidate.tables.end())
		{
			changed.push_back(name);
		}
	}
	std::sort(changed.begin(), changed.end());
	return changed;
}

std::optional<int> RunValidateTablesCli(int argc,
										char *argv[],
										const Hooks::ValidateFn &validateFn,
										const bool useBinary)
{
	std::string targetDir;
	bool found = false;
	for (int i = 1; i < argc; ++i)
	{
		const std::string_view arg(argv[i]);
		constexpr std::string_view kPrefix = "--validate-tables=";
		if (arg == "--validate-tables")
		{
			if (i + 1 >= argc)
			{
				std::cerr << "table validation failed: --validate-tables requires a directory argument\n";
				return 1;
			}
			targetDir = argv[++i];
			found = true;
			break;
		}
		if (arg.size() > kPrefix.size() && arg.substr(0, kPrefix.size()) == kPrefix)
		{
			targetDir = std::string(arg.substr(kPrefix.size()));
			found = true;
			break;
		}
	}
	if (!found)
	{
		return std::nullopt;
	}

	std::string err;
	BundleManifest manifest;
	if (!VerifyTableDirManifest(targetDir, useBinary, kTableSchemaDigest, manifest, err))
	{
		std::cerr << "table validation failed (manifest): " << err << "\n";
		return 1;
	}

	std::shared_ptr<TableSet> staged;
	if (!PrepareTableSetSync(targetDir, useBinary, staged, err))
	{
		std::cerr << "table validation failed (parse): " << err << "\n";
		return 1;
	}
	staged->identity.schemaDigest = manifest.schemaDigest;
	staged->identity.contentDigest = manifest.contentDigest;
	staged->identity.version = manifest.version;
	staged->identity.source = "bundle";

	if (validateFn)
	{
		ScopedTableSetOverride guard(staged);
		if (!validateFn(err))
		{
			std::cerr << "table validation failed (hook): " << err << "\n";
			return 1;
		}
	}

	std::cout << "table validation passed: schema_digest=" << manifest.schemaDigest
			  << " content_digest=" << manifest.contentDigest
			  << " version=" << manifest.version
			  << " tables=" << GetTableCount() << "\n";
	return 0;
}

TableBundleManager::TableBundleManager(muduo::net::EventLoop &loop)
	: eventLoop(loop)
{
}

void TableBundleManager::SetProcessHooks(Hooks hooks)
{
	GlobalHooks() = std::move(hooks);
}

const Hooks &TableBundleManager::ProcessHooks()
{
	return GlobalHooks();
}

uint64_t TableBundleManager::NowUnixMs()
{
	return static_cast<uint64_t>(
		std::chrono::duration_cast<std::chrono::milliseconds>(
			std::chrono::system_clock::now().time_since_epoch())
			.count());
}

void TableBundleManager::LoadAtStartup(const std::string &imageConfigDir, const bool useBinary)
{
	eventLoop.get().assertInLoopThread();
	this->imageConfigDir = imageConfigDir;
	this->useBinary = useBinary;
	bundleRoot = NonEmptyEnv(kBundleRootEnv);
	statusRoot = NonEmptyEnv(kStatusRootEnv);
	bundleMode = !bundleRoot.empty();

	const auto selfWeak = weak_from_this();
	GlobalMetricsRegistry().Register("table_bundle", [selfWeak](prometheus_text::Writer &writer)
									 {
										 if (const auto self = selfWeak.lock())
										 {
											 self->WriteMetrics(writer);
										 }
									 });

	std::string loadDir = this->imageConfigDir;
	std::string source = "image";
	std::optional<PointerLine> initialLine;

	if (bundleMode)
	{
		const std::string pointerPath = (std::filesystem::path(bundleRoot) / kPointerFilename).string();
		std::string err;
		if (!ReadPointerLine(pointerPath, kTableSchemaDigest, initialLine, err))
		{
			LOG_FATAL << "Table bundle mode enabled (" << kBundleRootEnv << "=" << bundleRoot
					  << ") but failed to read " << pointerPath << ": " << err;
		}
		if (initialLine)
		{
			const uint64_t nowMs = NowUnixMs();
			std::optional<PointerTarget> effective;
			if (initialLine->next && (initialLine->next->immediate || nowMs >= initialLine->next->activateAtMs))
			{
				effective = initialLine->next;
			}
			else if (initialLine->current)
			{
				effective = initialLine->current;
			}
			if (effective)
			{
				loadDir = (std::filesystem::path(bundleRoot) / "bundles" / effective->contentDigest).string();
				source = "bundle";
			}
		}
	}

	const std::filesystem::path manifestPath = std::filesystem::path(loadDir) / kManifestFilename;
	if (bundleMode || std::filesystem::exists(manifestPath))
	{
		std::string err;
		BundleManifest manifest;
		if (!VerifyTableDirManifest(loadDir, this->useBinary, kTableSchemaDigest, manifest, err))
		{
			LOG_FATAL << "Startup table manifest verification failed dir=" << loadDir << ": " << err;
		}
		hasActiveManifest = true;
		activeManifest = std::move(manifest);
	}

	if (source == "bundle")
	{
		std::shared_ptr<TableSet> loaded;
		std::string err;
		if (!PrepareTableSetSync(loadDir, this->useBinary, loaded, err))
		{
			LOG_FATAL << "Startup bundle table load failed dir=" << loadDir << ": " << err;
		}
		loaded->identity.schemaDigest = activeManifest.schemaDigest;
		loaded->identity.contentDigest = activeManifest.contentDigest;
		loaded->identity.version = activeManifest.version;
		loaded->identity.source = "bundle";
		CommitTableSet(std::move(loaded));
		// 启动路径与 LoadTablesAsync 一致:首次加载完成触发单表 LoadSuccess 与节点 OnLoaded 启动钩子。
		InvokeTablesLoadSuccess();
	}
	else
	{
		LoadTablesAsync();
		if (hasActiveManifest)
		{
			TableBundleIdentity id;
			id.schemaDigest = activeManifest.schemaDigest;
			id.contentDigest = activeManifest.contentDigest;
			id.version = activeManifest.version;
			id.source = "image";
			SetCurrentTableBundleIdentity(id);
		}
	}

	activeIdentity = CurrentTableSet()->identity;
	if (activeIdentity.schemaDigest.empty())
	{
		activeIdentity.schemaDigest = std::string(kTableSchemaDigest);
	}
	if (activeIdentity.source.empty())
	{
		activeIdentity.source = source;
	}

	LOG_INFO << "Config tables loaded at startup: source=" << activeIdentity.source
			 << " schema_digest=" << activeIdentity.schemaDigest
			 << " content_digest=" << (activeIdentity.contentDigest.empty() ? "none" : activeIdentity.contentDigest)
			 << " version=" << activeIdentity.version
			 << " tables=" << GetTableCount();

	if (const auto &onCommitted = GlobalHooks().onCommitted)
	{
		onCommitted(CurrentTableSet());
	}

	WriteStatusFile(NowUnixMs());

	if (bundleMode)
	{
		const auto weakSelf = weak_from_this();
		pollTimer.RunEvery(kPollIntervalSeconds, [weakSelf]
							{
								if (const auto self = weakSelf.lock())
								{
									self->PollOnce(NowUnixMs());
								}
							});
		// 启动时指针里已有未到点的 next,立即触发首拍准备(不白等 2 秒)。
		PollOnce(NowUnixMs());
	}
}

void TableBundleManager::Stop()
{
	stopped = true;
	++stagingGeneration;
	staging.reset();
	pollTimer.Cancel();
}

void TableBundleManager::PollOnce(const uint64_t nowMs)
{
	if (stopped || !bundleMode)
	{
		return;
	}

	ReapRetired(std::chrono::steady_clock::now());

	const std::string pointerPath = (std::filesystem::path(bundleRoot) / kPointerFilename).string();
	std::optional<PointerLine> line;
	std::string err;
	if (!ReadPointerLine(pointerPath, kTableSchemaDigest, line, err))
	{
		LOG_WARN << "Table hot-reload poll skipped: cannot read " << pointerPath << ": " << err;
		return;
	}
	if (!line)
	{
		return;
	}

	std::optional<PointerTarget> desired;
	if (line->next && line->next->contentDigest != activeIdentity.contentDigest)
	{
		desired = line->next;
	}
	else if (!line->next && line->current && line->current->contentDigest != activeIdentity.contentDigest)
	{
		desired = line->current;
		desired->immediate = true;
	}

	if (!desired)
	{
		// 指针撤销了尚未生效的 next:清理正在准备或等待提交的暂存表。
		if (staging)
		{
			LOG_INFO << "Table hot-reload staging cancelled because pointer no longer targets content_digest="
					 << staging->target.contentDigest;
			++stagingGeneration;
			staging.reset();
			pendingContentDigest.clear();
			pendingVersion = 0;
			pendingState.clear();
			pendingReason.clear();
			WriteStatusFile(nowMs);
		}
		return;
	}

	// 已经在准备或已准备完毕同一包:只需刷新激活时间并检查是否到点。
	if (staging && staging->target.contentDigest == desired->contentDigest)
	{
		staging->target.activateAtMs = desired->activateAtMs;
		staging->target.immediate = desired->immediate;
		if (staging->prepared && (staging->target.immediate || nowMs >= staging->target.activateAtMs))
		{
			CommitStaged(nowMs);
		}
		return;
	}

	// 同一摘要已被拒绝或标记 needs_restart,不重复尝试,直到指针换新包。
	if (terminalDigest == desired->contentDigest)
	{
		return;
	}

	// 准备途中指针切向另一包:丢弃旧暂存,从头开始。
	if (staging)
	{
		++stagingGeneration;
		staging.reset();
	}

	const std::string candidateDir = (std::filesystem::path(bundleRoot) / "bundles" / desired->contentDigest).string();
	BundleManifest candidateManifest;
	if (!VerifyTableDirManifest(candidateDir, useBinary, kTableSchemaDigest, candidateManifest, err))
	{
		RejectCandidate(*desired, "rejected", "manifest verification failed: " + err, false, nowMs);
		return;
	}
	if (candidateManifest.contentDigest != desired->contentDigest)
	{
		RejectCandidate(*desired, "rejected",
						"bundle manifest content_digest mismatch: expected=" + desired->contentDigest +
							" actual=" + candidateManifest.contentDigest,
						false, nowMs);
		return;
	}

	// 逐表比对摘要,检查是否触及不可热更的表(以二进制内编译期名单为准,不以 manifest 为准,设计 §5.3)。
	if (hasActiveManifest)
	{
		const auto changedTables = DiffChangedTables(activeManifest, candidateManifest, useBinary);
		std::vector<std::string> nonHotReloadable;
		for (const auto &tableName : changedTables)
		{
			if (!IsTableHotReloadable(tableName))
			{
				nonHotReloadable.push_back(tableName);
			}
		}
		if (!nonHotReloadable.empty())
		{
			std::string joined;
			for (std::size_t i = 0; i < nonHotReloadable.size(); ++i)
			{
				if (i > 0)
				{
					joined += ",";
				}
				joined += nonHotReloadable[i];
			}
			RejectCandidate(*desired, "needs_restart",
							"bundle modifies non-hot-reloadable tables: " + joined,
							true, nowMs);
			return;
		}
	}

	StagingState state;
	state.target = *desired;
	state.bundleDir = candidateDir;
	state.manifest = std::move(candidateManifest);
	state.tableSet = std::make_shared<TableSet>();
	state.tableSet->identity.schemaDigest = state.manifest.schemaDigest;
	state.tableSet->identity.contentDigest = state.manifest.contentDigest;
	state.tableSet->identity.version = state.manifest.version;
	state.tableSet->identity.source = "bundle";
	state.startedAt = std::chrono::steady_clock::now();

	const uint64_t gen = ++stagingGeneration;
	staging = std::move(state);
	pendingContentDigest = desired->contentDigest;
	pendingVersion = desired->version;
	pendingState = "preparing";
	pendingReason.clear();

	LOG_INFO << "Table hot-reload prepare started: schema_digest=" << std::string(kTableSchemaDigest)
			 << " content_digest=" << desired->contentDigest
			 << " version=" << desired->version
			 << " activate_at_ms=" << desired->activateAtMs
			 << " immediate=" << (desired->immediate ? "true" : "false");

	WriteStatusFile(nowMs);
	ScheduleNextSlice(gen);
}

void TableBundleManager::ScheduleNextSlice(const uint64_t generation)
{
	const auto weakSelf = weak_from_this();
	eventLoop.get().queueInLoop([weakSelf, generation]
								 {
									 if (const auto self = weakSelf.lock())
									 {
										 self->RunOnePrepareSlice(generation);
									 }
								 });
}

void TableBundleManager::RunOnePrepareSlice(const uint64_t generation)
{
	if (stopped || !staging || generation != stagingGeneration)
	{
		return;
	}

	if (staging->nextTableIndex >= GetTableCount())
	{
		FinishPrepare(generation);
		return;
	}

	const auto sliceStart = std::chrono::steady_clock::now();
	std::string err;
	const bool ok = LoadTableIntoSetAt(staging->nextTableIndex,
									   staging->bundleDir,
									   useBinary,
									   *staging->tableSet,
									   err);
	const double sliceSec = std::chrono::duration<double>(std::chrono::steady_clock::now() - sliceStart).count();
	if (sliceSec > staging->maxSliceSeconds)
	{
		staging->maxSliceSeconds = sliceSec;
	}
	if (sliceSec > lastMaxSliceSeconds)
	{
		lastMaxSliceSeconds = sliceSec;
	}

	if (!ok)
	{
		const PointerTarget failedTarget = staging->target;
		staging.reset();
		RejectCandidate(failedTarget, "rejected", "table parse failed: " + err, false, NowUnixMs());
		return;
	}

	++staging->nextTableIndex;
	if (staging->nextTableIndex < GetTableCount())
	{
		ScheduleNextSlice(generation);
	}
	else
	{
		FinishPrepare(generation);
	}
}

void TableBundleManager::FinishPrepare(const uint64_t generation)
{
	if (stopped || !staging || generation != stagingGeneration)
	{
		return;
	}

	if (const auto &validate = GlobalHooks().validate)
	{
		std::string err;
		ScopedTableSetOverride guard(staging->tableSet);
		if (!validate(err))
		{
			const PointerTarget failedTarget = staging->target;
			staging.reset();
			RejectCandidate(failedTarget, "rejected", "validation failed: " + err, false, NowUnixMs());
			return;
		}
	}

	const double totalSec = std::chrono::duration<double>(std::chrono::steady_clock::now() - staging->startedAt).count();
	ObservePrepareDuration(totalSec);
	lastMaxSliceSeconds = staging->maxSliceSeconds;
	staging->prepared = true;

	const uint64_t nowMs = NowUnixMs();
	LOG_INFO << "Table hot-reload prepared: schema_digest=" << std::string(kTableSchemaDigest)
			 << " content_digest=" << staging->target.contentDigest
			 << " version=" << staging->target.version
			 << " prepare_seconds=" << totalSec
			 << " max_slice_seconds=" << staging->maxSliceSeconds;

	if (staging->target.immediate || nowMs >= staging->target.activateAtMs)
	{
		CommitStaged(nowMs);
		return;
	}

	pendingState = "prepared";
	pendingReason.clear();
	WriteStatusFile(nowMs);
}

void TableBundleManager::CommitStaged(const uint64_t nowMs)
{
	if (!staging || !staging->prepared)
	{
		return;
	}

	auto nextSet = std::move(staging->tableSet);
	BundleManifest nextManifest = std::move(staging->manifest);
	const PointerTarget committedTarget = staging->target;
	staging.reset();

	auto previous = CommitTableSet(std::move(nextSet));
	if (previous)
	{
		retiredSets.push_back(RetiredEntry{
			std::move(previous),
			std::chrono::steady_clock::now() + kRetainDuration,
		});
	}

	hasActiveManifest = true;
	activeManifest = std::move(nextManifest);
	activeIdentity = CurrentTableSet()->identity;
	terminalDigest.clear();
	pendingContentDigest.clear();
	pendingVersion = 0;
	pendingState.clear();
	pendingReason.clear();
	++committedCount;

	if (const auto &onCommitted = GlobalHooks().onCommitted)
	{
		onCommitted(CurrentTableSet());
	}

	LOG_INFO << "Table hot-reload committed: schema_digest=" << activeIdentity.schemaDigest
			 << " content_digest=" << committedTarget.contentDigest
			 << " version=" << committedTarget.version
			 << " load_seq=" << activeIdentity.loadSeq
			 << " retained_sets=" << retiredSets.size();

	WriteStatusFile(nowMs);
}

void TableBundleManager::RejectCandidate(const PointerTarget &target,
										 const std::string_view pendingState,
										 const std::string &reason,
										 const bool isNeedsRestart,
										 const uint64_t nowMs)
{
	terminalDigest = target.contentDigest;
	pendingContentDigest = target.contentDigest;
	pendingVersion = target.version;
	this->pendingState = std::string(pendingState);
	pendingReason = reason;

	if (isNeedsRestart)
	{
		++needsRestartCount;
		LOG_WARN << "Table hot-reload needs restart: schema_digest=" << std::string(kTableSchemaDigest)
				 << " content_digest=" << target.contentDigest
				 << " version=" << target.version
				 << " reason=" << reason;
	}
	else
	{
		++rejectedCount;
		LOG_ERROR << "Table hot-reload rejected: schema_digest=" << std::string(kTableSchemaDigest)
				  << " content_digest=" << target.contentDigest
				  << " version=" << target.version
				  << " reason=" << reason;
	}
	WriteStatusFile(nowMs);
}

void TableBundleManager::ReapRetired(const std::chrono::steady_clock::time_point now)
{
	retiredSets.erase(
		std::remove_if(retiredSets.begin(), retiredSets.end(),
					   [now](const RetiredEntry &entry)
					   { return now >= entry.expireAt; }),
		retiredSets.end());
}

void TableBundleManager::ObservePrepareDuration(const double seconds)
{
	prepareDurationSumSeconds += seconds;
	for (std::size_t i = 0; i < kPrepareBuckets.size(); ++i)
	{
		if (seconds <= kPrepareBuckets[i])
		{
			++prepareBucketCounts[i];
			return;
		}
	}
	++prepareBucketCounts.back();
}

void TableBundleManager::WriteStatusFile(const uint64_t nowMs) const
{
	if (statusRoot.empty())
	{
		return;
	}
	std::ostringstream json;
	json << "{"
		 << "\"schema_digest\":\"" << EscapeJson(activeIdentity.schemaDigest) << "\","
		 << "\"content_digest\":\"" << EscapeJson(activeIdentity.contentDigest) << "\","
		 << "\"version\":" << activeIdentity.version << ","
		 << "\"source\":\"" << EscapeJson(activeIdentity.source) << "\","
		 << "\"load_seq\":" << activeIdentity.loadSeq << ","
		 << "\"state\":\"loaded\","
		 << "\"pending_content_digest\":\"" << EscapeJson(pendingContentDigest) << "\","
		 << "\"pending_version\":" << pendingVersion << ","
		 << "\"pending_state\":\"" << EscapeJson(pendingState) << "\","
		 << "\"reason\":\"" << EscapeJson(pendingReason) << "\","
		 << "\"updated_at_ms\":" << nowMs
		 << "}\n";

	std::string err;
	const auto target = std::filesystem::path(statusRoot) / kStatusFilename;
	if (!AtomicWriteFile(target, json.str(), err))
	{
		LOG_WARN << "Failed to write table status file " << target.string() << ": " << err;
	}
}

void TableBundleManager::WriteMetrics(prometheus_text::Writer &writer) const
{
	const std::string shortDigest = DigestPrefix12(activeIdentity.contentDigest);
	writer.BeginFamily("table_bundle_info",
					   "Active config table bundle identity (always 1).",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({{"schema_digest", activeIdentity.schemaDigest},
				   {"content_digest", shortDigest},
				   {"source", activeIdentity.source}},
				  1);

	writer.BeginFamily("table_reload_total",
					   "Total number of table hot-reload outcomes by result.",
					   prometheus_text::MetricType::kCounter);
	writer.Sample({{"result", "committed"}}, committedCount);
	writer.Sample({{"result", "rejected"}}, rejectedCount);
	writer.Sample({{"result", "needs_restart"}}, needsRestartCount);

	writer.BeginFamily("table_reload_prepare_seconds",
					   "Duration of full table hot-reload staging preparation in seconds.",
					   prometheus_text::MetricType::kHistogram);
	writer.HistogramDouble({}, kPrepareBuckets, prepareBucketCounts, prepareDurationSumSeconds);

	writer.BeginFamily("table_reload_slice_max_seconds",
					   "Maximum single-slice table parse duration on the main loop in seconds.",
					   prometheus_text::MetricType::kGauge);
	writer.SampleDouble({}, lastMaxSliceSeconds);

	writer.BeginFamily("table_sets_retained",
					   "Number of retired TableSet snapshots still retained for in-flight safety.",
					   prometheus_text::MetricType::kGauge);
	writer.Sample({}, static_cast<uint64_t>(retiredSets.size()));
}

} // namespace table_bundle

#include "NoMemberRawPointerCheck.h"

#include "clang/AST/Decl.h"
#include "clang/Basic/SourceManager.h"
#include "llvm/Support/raw_ostream.h"

#include <algorithm>
#include <cctype>

namespace {
std::string NormalizePath(std::string path) {
    std::replace(path.begin(), path.end(), '\\', '/');
#ifdef _WIN32
    std::transform(path.begin(), path.end(), path.begin(),
        [](unsigned char ch) { return static_cast<char>(std::tolower(ch)); });
#endif
    return path;
}
}

NoMemberRawPointerCheck::NoMemberRawPointerCheck(
    std::vector<std::string> skipPatterns)
    : SkipPatterns(std::move(skipPatterns)) {
    for (auto& pattern : SkipPatterns) pattern = NormalizePath(std::move(pattern));
}

void NoMemberRawPointerCheck::run(
    const clang::ast_matchers::MatchFinder::MatchResult& Result) {
    const auto* FD = Result.Nodes.getNodeAs<clang::FieldDecl>("ptrField");
    if (!FD) return;
    if (FD->isImplicit()) return;

    auto& SM = *Result.SourceManager;
    auto Loc = SM.getExpansionLoc(FD->getLocation());

    if (SM.isInSystemHeader(Loc)) return;

    std::string File = SM.getFilename(Loc).str();
    const auto NormalizedFile = "/" + NormalizePath(File);
    // 只按成员声明所在的源码目录排除库；业务类持有第三方类型仍须检查。
    // 保留目录边界，避免误放 third_party_adapter.cpp 等自有代码。
    if (NormalizedFile.find("/third_party/") != std::string::npos ||
        NormalizedFile.find("/cpp/libs/engine/muduo_windows/") != std::string::npos) return;
    for (const auto& Pat : SkipPatterns) {
        if (NormalizedFile.find(Pat) != std::string::npos) return;
    }

    unsigned Line = SM.getSpellingLineNumber(Loc);
    if (!Seen.insert({NormalizedFile, Line}).second) return; // 同一文件的不同分隔符只报一次。

    llvm::errs() << File << ":" << Line
                 << ": error: raw pointer member '" << FD->getName()
                 << "' (type '" << FD->getType().getAsString()
                 << "') is prohibited\n";
    ++Count;
}

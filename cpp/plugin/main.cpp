#include "NoMemberRawPointerCheck.h"

#include "clang/ASTMatchers/ASTMatchers.h"
#include "clang/ASTMatchers/ASTMatchFinder.h"
#include "clang/Frontend/FrontendActions.h"
#include "clang/Tooling/Tooling.h"
#include "clang/Tooling/CompilationDatabase.h"
#include "llvm/Support/CommandLine.h"

using namespace clang;
using namespace clang::ast_matchers;
using namespace clang::tooling;

static llvm::cl::OptionCategory ToolCat("no-raw-pointer-member options");

static llvm::cl::list<std::string> SourcePaths(
    llvm::cl::Positional,
    llvm::cl::desc("<source files>"),
    llvm::cl::OneOrMore,
    llvm::cl::cat(ToolCat));

static llvm::cl::list<std::string> ExtraArgs(
    "extra-arg",
    llvm::cl::desc("Additional compiler flag forwarded to Clang"),
    llvm::cl::ZeroOrMore,
    llvm::cl::cat(ToolCat));

static llvm::cl::list<std::string> SkipPaths(
    "skip-path",
    llvm::cl::desc("Path substring -- any FieldDecl in a matching file is skipped"),
    llvm::cl::ZeroOrMore,
    llvm::cl::cat(ToolCat));

int main(int argc, const char** argv) {
    llvm::cl::HideUnrelatedOptions(ToolCat);
    llvm::cl::ParseCommandLineOptions(argc, argv,
        "Checks that class/struct fields are not raw pointers.\n");

    // Flags that let Clang parse MSVC-flavoured C++20 without a full build env.
    std::vector<std::string> CompileArgs = {
        "-xc++",
        "-std=c++20",
        "-fsyntax-only",
        "-fms-extensions",
        "-fms-compatibility",
        "-Wno-everything",
    };
    for (const auto& A : ExtraArgs)
        CompileArgs.push_back(A);

    FixedCompilationDatabase CDB(".", CompileArgs);
    std::vector<std::string> Srcs(SourcePaths.begin(), SourcePaths.end());

    ClangTool Tool(CDB, Srcs);

    std::vector<std::string> SkipVec(SkipPaths.begin(), SkipPaths.end());
    NoMemberRawPointerCheck Checker(std::move(SkipVec));

    MatchFinder Finder;
    Finder.addMatcher(
        fieldDecl(hasType(pointerType())).bind("ptrField"),
        &Checker);

    // 解析失败时 AST 不完整，不能把“没有匹配到字段”当作检查通过。
    if (Tool.run(newFrontendActionFactory(&Finder).get()) != 0) {
        llvm::errs() << "[no-raw-pointer-member] FAILED: source parsing failed\n";
        return 2;
    }

    if (Checker.violationCount() > 0) {
        llvm::errs() << "\nFound " << Checker.violationCount()
                     << " raw pointer member violation(s).\n";
        return 1;
    }

    llvm::outs() << "[no-raw-pointer-member] PASSED\n";
    return 0;
}

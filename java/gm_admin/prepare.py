"""准备固定版本 JeecgBoot 扩展；只改 ignored build 工作树，不触碰游戏生成物。"""
from pathlib import Path
import argparse
import json
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[2]
MODULE = Path(__file__).resolve().parent
UPSTREAM = ROOT / "build/gm-admin-upstream"
RUNTIME = ROOT / "build/gm-admin-runtime"
REVISION = "49c29032e18df784f1a75123d90647bc8f74cedb"


def run(*args):
    subprocess.run([str(a) for a in args], cwd=ROOT, check=True)


def patch_login(frontend):
    """从固定提交重建最小登录补丁，重复准备不会叠加替换。"""
    relative = "jeecgboot-vue3/src/views/sys/login/LoginForm.vue"
    source = subprocess.check_output(
        ["git", "-C", str(UPSTREAM), "show", f"{REVISION}:{relative}"], text=True, encoding="utf-8"
    )

    def replace_once(old, new):
        nonlocal source
        if source.count(old) != 1:
            raise SystemExit("Unexpected pinned login form; refusing an unchecked replacement")
        source = source.replace(old, new, 1)

    def remove_block(start, end):
        if source.count(start) != 1:
            raise SystemExit("Unexpected pinned login entry; refusing an unchecked replacement")
        first = source.index(start)
        last = source.find(end, first)
        if last < 0:
            raise SystemExit("Unexpected pinned login entry boundary")
        replace_once(source[first:last + len(end)], "")

    replace_once("    password: '123456',", "    password: '',")
    # 保留上游账号、密码、验证码、AES 加密与 userStore 登录逻辑，仅去掉未开放入口。
    remove_block('    <ARow class="enter-x">\n      <ACol :span="12">\n        <FormItem>\n          <!-- No logic, you need to deal with it yourself -->', '    </ARow>')
    remove_block('    <ARow class="enter-x">\n      <ACol :md="8" :xs="24">', '    </div>')
    replace_once('  <!-- 第三方登录相关弹框 -->\n  <ThirdModal ref="thirdModalRef"></ThirdModal>\n', '')
    (frontend / "src/views/sys/login/LoginForm.vue").write_text(source, encoding="utf-8")
    # 当前 /login 路由使用 MiniLogin，旧表单还用于会话过期重登，因此两处都按同一规则收敛。
    relative = "jeecgboot-vue3/src/views/system/loginmini/MiniLogin.vue"
    source = subprocess.check_output(
        ["git", "-C", str(UPSTREAM), "show", f"{REVISION}:{relative}"], text=True, encoding="utf-8"
    )
    replace_once("    password: '123456',", "    password: '',")
    remove_block('                  <div class="aui-flex-box" :class="activeIndex === \'phoneLogin\'', '                  </div>')
    remove_block('                      <div class="aui-flex">\n                        <div class="aui-flex-box">\n                          <div class="aui-choice">', '                        </div>\n                      </div>')
    remove_block('                  <a-form v-else ref="phoneFormRef"', '                  </a-form>')
    remove_block('                  <div class="aui-flex">\n                    <a class="aui-linek-code aui-flex-box" @click="codeHandleClick"', '                  </div>')
    remove_block('                  <div class="aui-flex">\n                    <a class="aui-linek-code aui-flex-box" @click="registerHandleClick"', '                  </div>')
    remove_block('              <a-form @keyup.enter.native="loginHandleClick">', '              </a-form>')
    remove_block('    <div v-if="forgotLoaded"', '    <ThirdModal ref="thirdModalRef"></ThirdModal>')
    department_start = "  //初始化数据\n  let deptTimer;\n  function checkAccount() {"
    department_end = " //**********************查询部门逻辑end*************************************************"
    if source.count(department_start) != 1 or source.count(department_end) != 1:
        raise SystemExit("Unexpected pinned login department lookup; refusing an unchecked replacement")
    department_code = source[source.index(department_start):source.index(department_end)]
    if department_code.count("url: '/sys/loginGetUserDeparts'") != 1:
        raise SystemExit("Unexpected pinned login department request")
    replace_once(department_code, "  // 独立 GM 仅使用原生账号登录，验证码输入期间不发起部门预查询。\n  function checkAccount() {}\n")
    # 企业登录（docs/design/admin-identity-and-dual-control.md §5.2）：问到登录模式之前、以及 OIDC 模式下，
    # 登录框里只有企业登录组件；只有后台明确回答是本机口令模式，才显示上游的口令表单。
    replace_once('            <div class="aui-formBox">\n',
                 '            <GmSsoLogin v-if="identityMode !== \'native\'" />\n            <div class="aui-formBox" v-else>\n')
    replace_once("  import { useUserStore } from '/@/store/modules/user';\n",
                 "  import { useUserStore } from '/@/store/modules/user';\n"
                 "  import GmSsoLogin from '/@/views/gm/sso/GmSsoLogin.vue';\n"
                 "  import { identityMode } from '/@/api/gm/sso';\n")
    (frontend / "src/views/system/loginmini/MiniLogin.vue").write_text(source, encoding="utf-8")


def patch_home(frontend):
    """GM 部署使用自己的根路由，避免原生登录查询参数覆盖服务端配置的首页。"""
    relative = "jeecgboot-vue3/src/enums/pageEnum.ts"
    source = subprocess.check_output(
        ["git", "-C", str(UPSTREAM), "show", f"{REVISION}:{relative}"], text=True, encoding="utf-8"
    )
    original = "  BASE_HOME = '/dashboard/analysis',"
    if source.count(original) != 1:
        raise SystemExit("Unexpected pinned home route; refusing an unchecked replacement")
    (frontend / "src/enums/pageEnum.ts").write_text(source.replace(original, "  BASE_HOME = '/gm/console',", 1), encoding="utf-8")


def patch_ai_entry(frontend):
    """原生 aiIconShow 默认已关闭，但页签栏链接未使用该设置；GM 部署关闭此固定入口。"""
    relative = "jeecgboot-vue3/src/layouts/default/tabs/index.vue"
    source = subprocess.check_output(
        ["git", "-C", str(UPSTREAM), "show", f"{REVISION}:{relative}"], text=True, encoding="utf-8"
    )
    original = '<router-link v-if="!getIsSimpleTheme" to="/ai" class="ai-icon">'
    if source.count(original) != 1:
        raise SystemExit("Unexpected pinned AI link; refusing an unchecked replacement")
    (frontend / "src/layouts/default/tabs/index.vue").write_text(source.replace(original, '<router-link v-if="false" to="/ai" class="ai-icon">', 1), encoding="utf-8")


def patch_icon_lock(frontend):
    """只增补原生页面静态引用的 Ion 集合，保留固定提交的其余依赖解析。"""
    lock = subprocess.check_output(
        ["git", "-C", str(UPSTREAM), "show", f"{REVISION}:jeecgboot-vue3/pnpm-lock.yaml"], text=True, encoding="utf-8"
    ).replace("https://registry.npmmirror.com/", "https://registry.npmjs.org/")
    whole_collection = "      '@iconify/json':\n        specifier: ^2.2.498\n        version: 2.2.498\n"
    if lock.count(whole_collection) != 1 or lock.count("\npackages:\n") != 1 or lock.count("\nsnapshots:\n") != 1:
        raise SystemExit("Unexpected pinned dependency lock; refusing unchecked icon changes")
    lock = lock.replace(whole_collection, "      '@iconify-json/ion':\n        specifier: 1.2.7\n        version: 1.2.7\n", 1)
    lock = lock.replace("\npackages:\n", "\npackages:\n\n  '@iconify-json/ion@1.2.7':\n    resolution: {integrity: sha512-ZJNG5kLCbTPr68mdP0jx6Q3lB9AMEQKO2+VvIjuJ2llGFICBLD+cvESn/qGDRIqSfk2D3PrFMf/Qg8WKIUV7cg==, tarball: https://registry.npmjs.org/@iconify-json/ion/-/ion-1.2.7.tgz}\n", 1)
    lock = lock.replace("\nsnapshots:\n", "\nsnapshots:\n\n  '@iconify-json/ion@1.2.7':\n    dependencies:\n      '@iconify/types': 2.0.0\n", 1)
    (frontend / "pnpm-lock.yaml").write_text(lock, encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--protoc", default=str(ROOT / "third_party/protobuf/install_vs2026_dbg/bin/protoc.exe"))
    options = parser.parse_args()
    if not UPSTREAM.exists():
        run("git", "clone", "--depth=1", "--branch=v3.9.6", "https://github.com/jeecgboot/JeecgBoot.git", UPSTREAM)
    revision = subprocess.check_output(["git", "-C", str(UPSTREAM), "rev-parse", "HEAD"], text=True).strip()
    if revision != REVISION:
        raise SystemExit("Unexpected JeecgBoot revision; refusing to overwrite this checkout")
    start = UPSTREAM / "jeecg-boot/jeecg-module-system/jeecg-system-start"
    frontend = UPSTREAM / "jeecgboot-vue3"
    for part, destination in [(MODULE / "src/main", start / "src/main"), (MODULE / "frontend/src", frontend / "src")]:
        shutil.copytree(part, destination, dirs_exist_ok=True)
    patch_login(frontend)
    patch_home(frontend)
    patch_ai_entry(frontend)
    # GM 使用在线图标，不运行上游交互式全量图标库生成器。
    package_file = frontend / "package.json"
    package = json.loads(package_file.read_text(encoding="utf-8"))
    package["devDependencies"].pop("@iconify/json", None)
    package["devDependencies"]["@iconify-json/ion"] = "1.2.7"
    package["scripts"].pop("gen:icon", None)
    package_file.write_text(json.dumps(package, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    generated = MODULE / "target/generated-resources"
    generated.mkdir(parents=True, exist_ok=True)
    run(options.protoc, "--proto_path=.", "--proto_path=third_party/protobuf/src", "--include_imports",
        f"--descriptor_set_out={generated / 'gm-contracts.pb'}", "proto/mail/mail_admin.proto",
        "proto/data_service/data_service.proto", "proto/player_locator/player_locator.proto",
        "proto/common/base/currency_type.proto", "proto/scene_manager/scene_node_service.proto",
        # 交易人工处置(GameTradeAdmin);批准记录 common/base/admin_approval.proto 随 --include_imports 带上。
        "proto/trade/trade_admin.proto",
        # 运营处置的 gRPC 下游(GameOpsRpc):账号类、收件箱处置、榜单删除、滞留阵容终结;回收在上面的 data_service.proto 里。
        "proto/login/login_admin.proto", "proto/player_inbox/player_inbox_admin.proto", "proto/rank/rank_admin.proto",
        "proto/match/match_internal.proto")
    shutil.copy2(generated / "gm-contracts.pb", start / "src/main/resources/gm-contracts.pb")
    # 原生模块不引入独立测试 POM 的框架版本；只补入所需的传输依赖。
    pom = start / "pom.xml"
    content = pom.read_text(encoding="utf-8")
    dependencies = [("io.grpc", "grpc-netty-shaded", "1.80.0"), ("io.grpc", "grpc-protobuf", "1.80.0"),
                    ("io.grpc", "grpc-stub", "1.80.0"), ("com.google.protobuf", "protobuf-java", "4.34.2"),
                    ("com.google.protobuf", "protobuf-java-util", "4.34.2"),
                    # ID Token 验签（GmOidcProvider）；与 java/gm_admin/pom.xml 同版本。
                    ("com.nimbusds", "nimbus-jose-jwt", "10.0.2")]
    # 与固定上游 BOM 对齐，禁止 gRPC core/API 与 transport 混用版本。
    content = content.replace("<version>1.70.0</version>", "<version>1.80.0</version>")
    content = content.replace("<version>4.29.3</version>", "<version>4.34.2</version>")
    additions = "".join(f"\n        <dependency><groupId>{group}</groupId><artifactId>{name}</artifactId><version>{version}</version></dependency>"
                        for group, name, version in dependencies if f"<artifactId>{name}</artifactId>" not in content)
    content = content.replace("    </dependencies>", additions + "\n    </dependencies>", 1)
    if "<nonFilteredFileExtension>pb</nonFilteredFileExtension>" not in content:
        content = content.replace("        <plugins>", """        <plugins>
            <plugin><groupId>org.apache.maven.plugins</groupId><artifactId>maven-resources-plugin</artifactId>
                <configuration><nonFilteredFileExtensions combine.children="append"><nonFilteredFileExtension>pb</nonFilteredFileExtension></nonFilteredFileExtensions></configuration>
            </plugin>""", 1)
    pom.write_text(content, encoding="utf-8")
    (frontend / ".env.gm").write_text('''VITE_PORT=3101
VITE_GLOB_APP_TITLE=玄冥游戏管理
VITE_GLOB_APP_SHORT_NAME=Xuanming_GM
VITE_GLOB_APP_OPEN_SSO=false
VITE_GLOB_APP_OPEN_QIANKUN=false
VITE_USE_MOCK=false
VITE_PUBLIC_PATH=/
VITE_BUILD_COMPRESS=none
VITE_BUILD_COMPRESS_DELETE_ORIGIN_FILE=false
VITE_PROXY=[["/jeecgboot","http://127.0.0.1:18090/jeecg-boot"]]
VITE_GLOB_DOMAIN_URL=http://127.0.0.1:18090/jeecg-boot
VITE_GLOB_API_URL=/jeecgboot
VITE_GLOB_API_URL_PREFIX=
VITE_GLOB_ICONIFY_USE_TYPE=online
''', encoding="utf-8")
    patch_icon_lock(frontend)
    RUNTIME.mkdir(parents=True, exist_ok=True)
    (RUNTIME / "maven-settings.xml").write_text('''<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0">
  <mirrors><mirror><id>official-central</id><mirrorOf>aliyun</mirrorOf><url>https://repo.maven.apache.org/maven2</url></mirror></mirrors>
</settings>
''', encoding="utf-8")
    sql = UPSTREAM / "jeecg-boot/db/jeecgboot-mysql-5.7.sql"
    schema = sql.read_text(encoding="utf-8").replace("`jeecg-boot`", "`gm_admin`")
    if "CREATE DATABASE" not in schema.upper() or "USE `gm_admin`" not in schema:
        raise SystemExit("Unexpected upstream schema layout")
    (RUNTIME / "sql").mkdir(exist_ok=True)
    (RUNTIME / "sql/001-jeecg.sql").write_text(schema, encoding="utf-8")
    print("Prepared pinned JeecgBoot extension, descriptors, frontend and dedicated GM schema")


if __name__ == "__main__":
    main()

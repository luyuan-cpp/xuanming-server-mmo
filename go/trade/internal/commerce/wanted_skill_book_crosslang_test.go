package commerce

// wanted_skill_book_crosslang_test.go —— 求购单的金额规则与 C++ 是同一条业务规则(docs/design/skill-book-wanted-order-trade-escrow.md §3.1)。
// 5% 集市税(kProxyTaxBps 与 ProxyTaxGold 的向上取整)和封卷道籍模板号在 C++ 里一直有别的用处(名匠代打分成、scene 封卷),
// 两边任何一个漂移,揭榜人实得与界面不符或核对认错道具,所以逐字比对。悬赏额区间与每人 3 单随 scene 旧揭榜退役(批 B,
// 2026-11-27)从 C++ 删掉,规则只剩 trade 这一份,不再比对。纯文本比对,不连库。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 路径相对仓库根。go test 的工作目录是本包目录(go/trade/internal/commerce),仓库根在上四级。
const cppEquipCraftRules = "cpp/libs/services/scene/player/system/equip_craft_quality_rules.h"

func readCppRules(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(cppEquipCraftRules)))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("找不到 %s(只拷了 go/ 的构建环境),跳过逐字比对", cppEquipCraftRules)
	}
	if err != nil {
		t.Fatalf("读 %s: %v", cppEquipCraftRules, err)
	}
	return string(raw)
}

func TestWantedRulesMatchTheCppConstants(t *testing.T) {
	source := readCppRules(t)
	for _, literal := range []string{
		"kProxyTaxBps = " + strconv.FormatUint(WantedTaxBps, 10) + ";",
		"kSealedSkillBookConfigId = " + strconv.FormatUint(uint64(SealedSkillBookConfigID), 10) + ";",
		// 向上取整的写法:Go 的 WantedTaxGold 是 (reward*bps + 9999) / 10000。
		"(grossFee * kProxyTaxBps + 9999u) / 10000u",
	} {
		if !strings.Contains(source, literal) {
			t.Fatalf("%s 里找不到 %q:C++ 侧的集市税或封卷道籍模板变了,trade 的求购单常量要同批改", cppEquipCraftRules, literal)
		}
	}
}

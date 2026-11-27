# 赛季十季永久纪念徽章（S3–S10）作图交接

> 用途：为十季无限循环赛季玩法（S1–S10）补齐第 3 季至第 10 季共 **8 枚** 永久纪念徽章独立图标（第 1 季 `honor_s1_pozhen.png` 与第 2 季 `honor_s2_xuanmai.png` 已确认完成）。其余十季荣誉墙界面与往季追忆交互均复用既有界面控件，本次无需新增整屏框体或重复底板。

---

## 可直接复制给 Codex 的作图正文

```text
任务：为赛季玩法补齐第 3 季至第 10 季共 8 枚「永久纪念徽章」独立图标（用于十季荣誉墙 2×5 徽章格子、满级提示条与战后解锁提示条）。

一、开工前必做（核实并逐张展示实图）
开始绘制前，请先核实并实际查看图仓中的以下 5 张参考图，并在当前聊天逐张显示实图、写明名称与准确相对路径（缺少任意一张时先如实报告，不要用文字、文件名或其他图片冒充）：
1. 邮件界面：`designs/mail-ui-v1/01-mail-event.png`
2. 组队界面：`designs/team-ui-v2/team-ui-v2.png`
3. 帮会界面（首要标准）：`designs/ranked-play-v2-ui-20261125/assets/approved-guild-reference.png`
4. 同功能已确认成图 · 第一季破阵之季荣誉徽章：`designs/season-honor-badges-v1/icons/honor_s1_pozhen.png`
5. 同功能已确认成图 · 第二季玄脉之季荣誉徽章：`designs/season-honor-badges-v1/icons/honor_s2_xuanmai.png`

二、输出目录（新建独立专题目录）
- 图仓输出目录：`designs/season-honor-badges-s3-s10-v1/icons/`（本机路径：`D:\work\image\designs\season-honor-badges-s3-s10-v1\icons\`）

三、功能规格与交付要求（8 枚均须满足）
- 格式：PNG-32（带完整 RGBA 透明底，严禁任何不透明底色或方形外框背景）。
- 尺寸：每张 512×512 正方形，主体居中，四周预留 ≥10% 安全留白（主体不裁边、不贴边）。
- 无文字规则：画面内严禁出现任何汉字、字母、数字、印章文字、角标文字或编号（赛季名与称谓由界面文本单独叠加显示）。
- 识别度与缩放要求：运行时会缩小展示在 48×48、64×64 与 80×80 三个显示格内，且未解锁状态会被整体压暗半透明展示；8 枚徽章的主体轮廓需彼此清晰区分，并与已确认的 S1、S2 徽章保持同系列尺寸占比与视觉重心一致，小图下仍能一眼分辨各季主题。
- 不拉伸保护：8 枚均为固定 1:1 比例独立徽章图标，不需要九宫格拉伸切边。

四、本次需交付的 8 枚图标清单

| 序号 | 输出文件名 | 对应赛季与称谓（仅供理解题材，勿画进图内） | 画面主体意象（仅功能区分） | 尺寸 |
|---|---|---|---|---|
| 1 | `honor_s3_lihuo.png` | 第三季 · 离火之季（离火尊者） | 火焰与羽翼/飞鸟轮廓组合 | 512×512 |
| 2 | `honor_s4_qingmu.png` | 第四季 · 青木之季（青木济世） | 枝叶新芽与葫芦轮廓组合 | 512×512 |
| 3 | `honor_s5_xuanshui.png` | 第五季 · 玄水之季（玄水清流） | 涌泉水波与净瓶轮廓组合 | 512×512 |
| 4 | `honor_s6_houtu.png` | 第六季 · 厚土之季（厚土宗师） | 山岳与重盾轮廓组合 | 512×512 |
| 5 | `honor_s7_gengjin.png` | 第七季 · 庚金之季（庚金剑首） | 交叉双剑与剑锋轮廓组合 | 512×512 |
| 6 | `honor_s8_taiyin.png` | 第八季 · 太阴之季（太阴巡夜） | 弯月与圆镜/云环轮廓组合 | 512×512 |
| 7 | `honor_s9_tianlei.png` | 第九季 · 天雷之季（天雷御使） | 雷鼓与闪电纹轮廓组合 | 512×512 |
| 8 | `honor_s10_hunyuan.png` | 第十季 · 混元之季（混元大宗） | 两仪太极与五点环绕轮廓组合 | 512×512 |

五、自检与交付输出
1. 全部 8 张文件按准确文件名落盘至 `designs/season-honor-badges-s3-s10-v1/icons/`。
2. 逐张核验：512×512 像素、RGBA 透明底无杂边、无任何文字/数字、主体居中且四周留白 ≥10%、与 S1/S2 两枚已确认徽章并排及缩至 64×64 时轮廓清晰且互不雷同。
3. 可附一张总览拼图放在 `designs/season-honor-badges-s3-s10-v1/preview_8_badges.png` 便于人工一次性验收（仅作验收预览，不替代 8 张独立透明底图标）。
```

---

## 本机研发导入备忘（仅本机研发参考，勿复制给 Codex）

1. **已验收落地（2026-11-26，图仓提交 `70a5622fa202e91119b84734c6072b299cd6d3f1`）**：8 枚 512×512 RGBA 透明 PNG-32（`honor_s3_lihuo.png` 至 `honor_s10_hunyuan.png`）及对应 `.meta`（Sprite Single、FullRect、MipMaps 关、AlphaIsTransparency 开、全仓 0 GUID 冲突）均已落地 `mmorpg-client/Assets/Resources/UI/SeasonHonors/`，8/8 经逐张目视检查与 SHA-256 逐字节核验一致。
2. 客户端 `SeasonV1UiArt.LoadHonorBadgeIcon`、`SeasonSealPanel`（荣誉墙 2×5 + 休赛期荣誉条 + 满级条）、`CoreSettlementRewardSection`（结算解锁/追忆反馈）与 `Assets/StreamingAssets/Tables/seasonrule.pb` 10/10 键已完成端到端核验。
3. 编译与单测验证：C++ `SeasonBattleTest` 24/24、`PlayerSeasonSystemTest` 12/12；Java `SeasonBattleEngineTest` 7/7、`PlayerStateSchemaLedgerTest` 1/1（第 22 代）、`SeasonFeatureAndServiceTest` + `CoreGameplayLoopParityTest` 12/12；Python `pytest` 29/29；Unity `client_compile_check.ps1` **816/816 0 错误**、`SeasonClientTests` **8/8 PASS**。

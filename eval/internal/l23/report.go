package l23

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/metrics"
)

var layerNames = map[string]string{LayerL2: "L2 回答层", LayerL3: "L3 工具调用层"}

// Markdown renders the report. The headers and footnotes say what a "success" is, what
// the two denominators are, and what pass^k means, so no column has to be guessed.
func Markdown(stats *Stats, meta Meta, opts Options) string {
	var b strings.Builder
	b.WriteString("# L2 / L3 评测报告\n\n")
	if !onlyReal(stats.Envs) {
		b.WriteString("> **离线替身环境（" + strings.Join(stats.Envs, "、") + "）**：本报告只证明链路与断言计算正确，其中的数字不是质量结论，不得引用。\n\n")
	}

	b.WriteString("## 运行信息\n\n")
	fmt.Fprintf(&b, "- 臂：%s", strings.Join(stats.Arms, "、"))
	if opts.Baseline != "" {
		fmt.Fprintf(&b, "（基线：`%s`）", opts.Baseline)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- 记录 %d 条，评出 %d 个结果；语料哈希 `%s`\n", meta.Records, meta.Outcomes, short(meta.CorpusHash))
	fmt.Fprintf(&b, "- 题集哈希 `%s`，标注哈希 `%s`，仅 L3 题哈希 `%s`\n\n", shortPtr(meta.DatasetSHA256), shortPtr(meta.LabelsSHA256), shortPtr(meta.L3OnlySHA256))

	b.WriteString("## 表 1　成功率与 pass^k\n\n")
	b.WriteString("| 臂 | 层 | 题数 | 次数 | 每题重复 | 通过 | 失败 | 待判 | 错误 | 单次成功率（待判与错误按失败） | 单次成功率（只算已判定） | pass^k |\n")
	b.WriteString("|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|\n")
	for _, al := range stats.ArmLayers {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d | %.3f | %s | %s |\n",
			al.Arm, layerNames[al.Layer], al.Questions, al.Trials, al.Repeats, al.Pass, al.Fail, al.NeedsJudge, al.Error,
			al.Pass1Strict, optional(al.Pass1Decided), passKText(al.PassK))
	}
	b.WriteString("\n")
	b.WriteString("- **成功**：该层全部适用的断言都是 `pass`。`needs_judge`（结构状态判不了，要由判分模型看）和 `error`（服务没答出来或记录残缺，与回答好坏无关）都不算成功，但**单列**，不混进失败。\n")
	b.WriteString("- **两个分母**：第一种把待判和错误都按失败算（最保守）；第二种只在 `通过 + 失败` 里算，待判与错误不进分母，要结合这两列的个数来读。\n")
	b.WriteString("- **pass^k**：同一道题的 k 次尝试**全部**成功的概率，从该题的各次重复里不放回地取 k 次估计，再对题取平均；k = 每题重复次数时就是“全部重复都成功的题占比”。括号里写明参与的题数，重复次数不足 k 的题被排除并写出。\n\n")

	b.WriteString("## 表 2　各断言通过情况\n\n")
	b.WriteString("| 臂 | 层 | 断言 | 评了 | 通过 | 失败 | 待判 | 错误 |\n|---|---|---|--:|--:|--:|--:|--:|\n")
	for _, al := range stats.ArmLayers {
		for _, a := range al.Assertions {
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %d |\n", al.Arm, layerNames[al.Layer], a.Name, a.Evaluated, a.Pass, a.Fail, a.NeedsJudge, a.Error)
		}
	}
	b.WriteString("\n- 同一次尝试里一条断言失败不会阻止别的断言被评；唯一的例外是状态值非法或工具步骤与计数对不上，这时后面的断言没有意义，只评前面那一条。\n\n")

	b.WriteString("## 表 3　失败、待判与错误的归类\n\n")
	b.WriteString("| 臂 | 层 | 归类 | 次数 |\n|---|---|---|--:|\n")
	for _, al := range stats.ArmLayers {
		for _, class := range sortedClassKeys(al.Classes) {
			fmt.Fprintf(&b, "| %s | %s | %s | %d |\n", al.Arm, layerNames[al.Layer], class, al.Classes[class])
		}
		for _, tier := range sortedClassKeys(al.Tiers) {
			fmt.Fprintf(&b, "| %s | %s | 无答案题档位：%s | %d |\n", al.Arm, layerNames[al.Layer], tierName(tier), al.Tiers[tier])
		}
	}
	b.WriteString("\n- 无答案题的 `abstained`：系统自己声明没有证据（结构上的如实说明，计 0 档）；`pending`：状态判不了是诚实还是编造，留给判分模型。**这里不给 +1 / −1 档，那要等判分。**\n\n")

	b.WriteString("## 表 4　与基线的配对比较\n\n")
	if len(stats.Comparisons) == 0 {
		b.WriteString("没有可比较的臂。\n\n")
	} else {
		b.WriteString("| 臂 vs 基线 | 层 | 题数/簇数 | 每题重复 | 每题成功率之差 [95% CI] | 仅该臂全部成功 / 仅基线全部成功 | McNemar 精确 p | 最小可检测效应（全部成功 / 成功率） |\n|---|---|--:|--:|--:|--:|--:|--:|\n")
		for _, c := range stats.Comparisons {
			fmt.Fprintf(&b, "| %s vs %s | %s | %d / %d | %d | %s | %d / %d | %.4f | %s / %s |\n",
				c.Arm, c.Baseline, layerNames[c.Layer], c.Questions, c.Clusters, c.Repeats, diff(c.RateDifference), c.ArmOnly, c.BaselineOnly, c.McNemarP, optional(c.AllKMDE), optional(c.RateMDE))
		}
		fmt.Fprintf(&b, "\n- 逐题配对。每题的成功率 = 该题通过的重复次数 ÷ 重复次数；区间用按簇（文档或题类）的自助法，重抽样 %d 次，种子 %d，取 2.5%% 与 97.5%% 分位，同一簇里的题不独立。只有一个簇时给不出区间（n/a）。\n", opts.Resamples, opts.Seed)
		b.WriteString("- McNemar 精确检验只看一边全部重复都成功、另一边没有的题（双侧，对称二项，没有这样的题时 p = 1）。两臂重复次数不同的题被排除。\n")
		b.WriteString("- 最小可检测效应：α = 0.05、效能 80% 的正态近似，是规划用的估计，不是检验结果；差值小于它时“不显著”不等于“没有差异”。\n\n")
	}

	b.WriteString("## 表 5　代价（运行自己报告的数）\n\n")
	b.WriteString("| 臂 | 次数 | 平均模型调用 | 平均工具调用 | 耗时中位数（毫秒） |\n|---|--:|--:|--:|--:|\n")
	for _, c := range stats.Costs {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n", c.Arm, c.Trials, optional1(c.MeanModelCalls), optional1(c.MeanToolCalls), optional0(c.MedianDurationMs))
	}
	b.WriteString("\n- 模型调用数与工具调用数是 `/agent/run` 自己返回的；它不含后台任务（记忆抽取、摘要）的调用，也没有 token 用量，这些要看计数代理的账本。耗时是一次 `/agent/run` 的整体耗时。\n")
	return b.String()
}

// JSON is the machine-readable form of the statistics.
func JSON(stats *Stats) ([]byte, error) {
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func onlyReal(envs []string) bool { return len(envs) == 1 && envs[0] == "real" }

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func shortPtr(hash *string) string {
	if hash == nil {
		return "—"
	}
	return short(*hash)
}

func passKText(pks []PassK) string {
	parts := make([]string, 0, len(pks))
	for _, p := range pks {
		text := fmt.Sprintf("k=%d：%.3f（%d 题", p.K, p.Value, p.Questions)
		if p.LeftOut > 0 {
			text += fmt.Sprintf("，排除 %d 题", p.LeftOut)
		}
		parts = append(parts, text+"）")
	}
	return strings.Join(parts, "；")
}

func tierName(tier string) string {
	switch tier {
	case "abstained":
		return "如实说明没有证据（0 档）"
	case "pending":
		return "待判"
	}
	return tier
}

func sortedClassKeys(m map[string]int) []string {
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return sortedKeys(set)
}

func optional(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", *v)
}

func optional1(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", *v)
}

func optional0(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f", *v)
}

func diff(d metrics.Difference) string {
	if d.N == 0 {
		return "n/a"
	}
	if !d.CI.OK {
		return fmt.Sprintf("%+.3f [n/a]", d.Mean)
	}
	return fmt.Sprintf("%+.3f [%+.3f, %+.3f]", d.Mean, d.CI.Low, d.CI.High)
}

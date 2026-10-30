package report

import (
	"fmt"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/metrics"
	"github.com/xiaopang456/GopherLearn/eval/internal/runner"
)

// Column headers that name each measure's criterion. They are constants because
// the two criteria differ and a reader must never have to guess which one a
// column uses.
const (
	hitCriterion = "覆盖率 ≥ %s"
	mrrCriterion = "至少共有 1 行"
)

// Markdown renders the report. Table headers and footnotes state the criterion of
// hit@K and of MRR separately.
func (r *Report) Markdown() string {
	var b strings.Builder
	threshold := formatThreshold(r.Input.Threshold)
	hitHeader := "命中@K（" + fmt.Sprintf(hitCriterion, threshold) + "）"
	mrrHeader := "MRR（首个" + mrrCriterion + "的结果）"

	b.WriteString("# L1 检索评测报告\n\n")
	if r.Input.Manifest.Environment != runner.EnvReal {
		b.WriteString("> **离线替身环境（" + r.Input.Manifest.Environment + "）**：本报告只证明链路与指标计算正确，" +
			"其中的数字不是检索质量结论，不得引用。\n\n")
	}
	if r.Incomplete {
		b.WriteString("> **运行不完整**：部分（单元格, 问题）没有记录，下表只统计有记录的问题，“缺记录”列给出缺口。\n\n")
	}

	m := r.Input.Manifest
	b.WriteString("## 运行信息\n\n")
	fmt.Fprintf(&b, "- 运行：`%s`，环境 `%s`，commit `%s`\n", m.RunID, m.Environment, m.Commit)
	fmt.Fprintf(&b, "- 语料：`%s`（%d 篇，哈希 `%s`）\n", m.Corpus.Name, m.Corpus.Documents, short(m.Corpus.Hash))
	fmt.Fprintf(&b, "- 题集：版本 `%s`，split `%s`；可答题 %d、不可答题 %d\n", short(m.Dataset.Version), m.Split, r.Answerable, r.Unanswerable)
	fmt.Fprintf(&b, "- 嵌入模型 `%s`（%d 维）；重排：%s\n", m.Embedding.Model, m.Embedding.Dimension, rerankText(m))
	fmt.Fprintf(&b, "- 切块：最大 %d token，重叠 %d；索引：%s\n", m.Chunker.MaxTokens, m.Chunker.OverlapTokens, m.IndexDecision)
	fmt.Fprintf(&b, "- 付费调用：嵌入 %d 次、重排 %d 次（计划值，见 manifest）\n\n", m.Estimate.EmbeddingCalls, m.Estimate.RerankCalls)

	b.WriteString("## 表 1　汇总\n\n")
	fmt.Fprintf(&b, "| 单元格 | K | 可答题 | %s | 平均覆盖率@K | %s | 平均纯度@K | 失败 | 缺记录 |\n", hitHeader, mrrHeader)
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, c := range r.Cells {
		fmt.Fprintf(&b, "| %s | %d | %d | %s | %.3f | %.3f | %.3f | %d | %d |\n",
			c.Cell, c.TopK, c.Answerable, rate(c.Hits, c.Answerable), c.MeanCoverage, c.MRR, c.MeanPurity, c.Errors, c.Missing)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- **命中@K 的口径**：按行计。取该题各个证据集中被前 K 个结果覆盖得最好的一个，其行覆盖率 ≥ %s 即为命中；"+
		"同一证据集内的各段都要计入，多个证据集取最好的，覆盖率相同时取先列出的。\n", threshold)
	b.WriteString("- **MRR 的口径**：取前 K 个结果里第一个与**任一**证据集**至少共有 1 行**的结果，计 1/名次，没有则计 0；" +
		"对可答题取平均。\n")
	b.WriteString("- 两个口径不同：一个结果只要与证据共有 1 行就能让该题的 MRR 大于 0，却不一定使该题命中。\n")
	b.WriteString("- 平均覆盖率@K：各题最佳证据集的行覆盖率的平均值。平均纯度@K：前 K 个结果的行并集中，属于最佳证据集的比例（并集为空记 0）。\n")
	b.WriteString("- 行以（文件, 行号）计，相邻块重叠的行只算一次。失败的检索按 0 分计入，并单列“失败”，不会被丢弃。\n\n")

	b.WriteString("## 表 2　候选召回与失败分类\n\n")
	fmt.Fprintf(&b, "| 单元格 | K | 候选池命中（%s） | 正常（命中@K） | 排得不好 | 召回不到 | 失败 |\n", fmt.Sprintf(hitCriterion, threshold))
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|\n")
	for _, c := range r.Cells {
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %d | %d | %d |\n", c.Cell, c.TopK, rate(c.PoolHits, c.Answerable), c.Normal, c.Misranked, c.NotRecalled, c.Errors)
	}
	b.WriteString("\n")
	b.WriteString("- 候选池：重排之前的融合候选（至多 24 条），按与命中@K 相同的口径计覆盖率。\n")
	b.WriteString("- **排得不好**：候选池已覆盖证据，前 K 个结果却没有命中（证据被召回后又被排出）。**召回不到**：连候选池也没有覆盖证据。\n\n")

	b.WriteString("## 表 3　配对比较（挑战者 − 基线，基线为同 K 的 hybrid）\n\n")
	if len(r.Comparisons) == 0 {
		b.WriteString("没有可比较的单元格对。\n\n")
	} else {
		fmt.Fprintf(&b, "| 挑战者 | K | 题数/文档数 | 命中率差（%s）[95%% CI] | 仅挑战者命中 / 仅基线命中 | McNemar 精确 p | 覆盖率差 [95%% CI] | MRR 差（%s）[95%% CI] | 最小可检测效应（命中 / 覆盖率） |\n",
			fmt.Sprintf(hitCriterion, threshold), mrrCriterion)
		b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|\n")
		for _, c := range r.Comparisons {
			fmt.Fprintf(&b, "| %s | %s | %d / %d | %s | %d / %d | %.4f | %s | %s | %s / %s |\n",
				c.Challenger, kOf(c.Challenger), c.N, c.Clusters, diff(c.Hit), c.Discordant.AOnly, c.Discordant.BOnly,
				c.McNemarP, diff(c.Coverage), diff(c.MRR), optional(c.HitMDE), optional(c.CoverageMDE))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "- 区间：按文档做簇的自助法（cluster bootstrap），重抽样 %d 次，种子 %d，取 2.5%% 与 97.5%% 分位；"+
			"同一文档的问题不独立，所以重抽的是文档而不是问题。只有一个文档时无法给出区间（n/a）。\n", r.Input.Resamples, r.Input.Seed)
		b.WriteString("- McNemar 精确检验只看两边命中不一致的题：双侧、对称二项，无不一致题时 p = 1。\n")
		b.WriteString("- 最小可检测效应：在 α = 0.05、检验效能 80% 的正态近似下，这么多题大约能可靠分辨的最小差值；" +
			"它是规划用的估计，不是检验结果。差值小于它时，“没有显著差异”不等于“没有差异”。\n")
		b.WriteString("- 命中率差用命中@K 的口径，MRR 差用 MRR 的口径，两者不能互换解读。\n\n")
	}

	b.WriteString("## 表 4　不可答题的 top-1 分数分布（仅描述）\n\n")
	b.WriteString("| 单元格 | K | 不可答题 | 空结果 | 不可答题 top-1 分数（最小 / 中位 / 最大） | 可答题 top-1 中位 |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|\n")
	for _, c := range r.Cells {
		if c.Unanswerable == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %s | %s |\n", c.Cell, c.TopK, c.Unanswerable, c.UnanswerableEmpty, triple(c.UnanswerableTopOne), optional(c.AnswerableTopOneMed))
	}
	b.WriteString("\n- 不同方法的分数含义不同（向量距离、融合分、重排分），不能跨方法比较；这里只记录分布，不据此定拒答阈值。\n")
	return b.String()
}

func rate(hits, n int) string {
	if n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.3f (%d/%d)", float64(hits)/float64(n), hits, n)
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

func optional(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", *v)
}

func triple(v []float64) string {
	if len(v) != 3 {
		return "n/a"
	}
	return fmt.Sprintf("%.3f / %.3f / %.3f", v[0], v[1], v[2])
}

func kOf(cell string) string {
	if i := strings.LastIndex(cell, "k="); i >= 0 {
		return cell[i+2:]
	}
	return ""
}

func formatThreshold(t float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", t), "0"), ".")
}

func rerankText(m runner.Manifest) string {
	if !m.Rerank.Attached {
		return "未启用"
	}
	return "`" + m.Rerank.Model + "`"
}

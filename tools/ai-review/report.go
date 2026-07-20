package main

import (
	"fmt"
	"strings"
	"time"
)

// ReportMeta 报告元信息。
type ReportMeta struct {
	Base, Head   string
	Model        string
	Files        int
	Batches      int
	Redactions   int
	FailedBatchs int
	GeneratedAt  time.Time
}

var severityLabel = map[string]string{"high": "高危", "medium": "中危", "low": "低危"}

// zeroWidthSpace 插在 @ 之后，使 @all / @user 不会在 MR 评论中触发提醒。
var zeroWidthSpace = string(rune(0x200B))

// safeInline 处理将要进入 MR 评论的模型输出：模型可能被 diff 中的注入内容诱导输出
// @all 提醒、HTML 或伪造的“审查通过”标记，统一转义。
func safeInline(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", "@", "@"+zeroWidthSpace, "|", "\\|").Replace(oneLine(s))
}

func safeBlock(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", "@", "@"+zeroWidthSpace).Replace(strings.TrimSpace(s))
}

// fence 选择比内容中最长反引号串更长的围栏，防止证据代码提前闭合代码块。
func fence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

func renderReport(meta ReportMeta, findings []Finding, staticOnly []SemgrepFinding, summaries []string) string {
	var b strings.Builder
	b.WriteString("# AI 辅助代码审查报告（初稿）\n\n")
	b.WriteString("> 本报告由 `tools/ai-review` 自动生成，所有结论**待人工复核**。AI 结论不作为合并门禁依据，")
	b.WriteString("门禁以 gosec / Semgrep / govulncheck / Trivy 为准；分级标准见 `docs/audit/risk-rating.md`。\n\n")
	fmt.Fprintf(&b, "- 范围：`%s...%s`，%d 个文件，%d 个批次\n", meta.Base, meta.Head, meta.Files, meta.Batches)
	fmt.Fprintf(&b, "- 模型：`%s`；外发前脱敏替换 %d 处\n", meta.Model, meta.Redactions)
	if meta.FailedBatchs > 0 {
		fmt.Fprintf(&b, "- **%d 个批次审查失败，对应文件需完全人工审查**\n", meta.FailedBatchs)
	}
	fmt.Fprintf(&b, "- 生成时间：%s\n\n", meta.GeneratedAt.Format(time.RFC3339))

	counts := map[string][2]int{}
	for _, f := range findings {
		c := counts[f.Severity]
		if f.Corroboration == CorroboratedBySemgrep {
			c[0]++
		} else {
			c[1]++
		}
		counts[f.Severity] = c
	}
	b.WriteString("## 概览\n\n| 等级 | AI+Semgrep 交叉命中 | 仅 AI |\n|---|---|---|\n")
	for _, s := range severities {
		fmt.Fprintf(&b, "| %s | %d | %d |\n", severityLabel[s], counts[s][0], counts[s][1])
	}
	fmt.Fprintf(&b, "| 仅静态扫描 | - | %d |\n\n", len(staticOnly))

	b.WriteString("## 发现明细\n\n")
	if len(findings) == 0 {
		b.WriteString("AI 未发现需要报告的问题（不代表不存在问题）。\n\n")
	}
	for i, f := range findings {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, severityLabel[f.Severity], safeInline(f.Title))
		b.WriteString("| 位置 | 类别 | CWE | 置信度 | 交叉验证 |\n|---|---|---|---|---|\n")
		rules := f.Corroboration
		if len(f.SemgrepRules) > 0 {
			rules += "（" + safeInline(strings.Join(f.SemgrepRules, ", ")) + "）"
		}
		fmt.Fprintf(&b, "| `%s:%d-%d` | %s | %s | %s | %s |\n\n",
			safeInline(f.File), f.Line, f.EndLine, safeInline(f.Category), safeInline(f.CWE), safeInline(f.Confidence), rules)
		fmt.Fprintf(&b, "**问题**：%s\n\n", safeBlock(f.Description))
		if ev := strings.TrimSpace(f.Evidence); ev != "" {
			fn := fence(ev)
			fmt.Fprintf(&b, "%sgo\n%s\n%s\n\n", fn, ev, fn)
		}
		fmt.Fprintf(&b, "**建议**：%s\n\n", safeBlock(f.Recommendation))
		b.WriteString("**人工复核**：[ ] 确认  [ ] 误报  [ ] 调整等级 → ____　复核人：____\n\n")
	}

	if len(staticOnly) > 0 {
		b.WriteString("## 仅静态扫描命中（AI 未提及）\n\n| 位置 | 规则 | 等级 | 说明 |\n|---|---|---|---|\n")
		for _, s := range staticOnly {
			fmt.Fprintf(&b, "| `%s:%d` | %s | %s | %s |\n", safeInline(s.Path), s.Line, safeInline(s.RuleID), safeInline(s.Severity), safeInline(s.Message))
		}
		b.WriteString("\n")
	}
	if len(summaries) > 0 {
		b.WriteString("## 模型摘要\n\n")
		for _, s := range summaries {
			b.WriteString("- " + safeInline(s) + "\n")
		}
	}
	return b.String()
}

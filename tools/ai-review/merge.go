package main

import (
	"cmp"
	"slices"
)

// 交叉验证结果。
const (
	CorroboratedBySemgrep = "ai+semgrep" // AI 与静态扫描在同一位置命中，优先复核
	AIOnly                = "ai-only"    // 仅 AI 提出，误报概率更高，需人工确认
)

const lineSlack = 3

func severityRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}

// crossValidate 将 AI 结论与 Semgrep 结果按文件 + 行号区间（±3 行）匹配。
// 返回带交叉验证标记的 AI 结论，以及 AI 未提及的静态扫描结果（同样进入报告，避免被 AI “淹没”）。
func crossValidate(findings []Finding, sg []SemgrepFinding) ([]Finding, []SemgrepFinding) {
	used := make([]bool, len(sg))
	for i := range findings {
		f := &findings[i]
		f.Corroboration = AIOnly
		for j, s := range sg {
			end := max(s.EndLine, s.Line)
			if s.Path == f.File && s.Line <= f.EndLine+lineSlack && end >= f.Line-lineSlack {
				f.Corroboration = CorroboratedBySemgrep
				if !slices.Contains(f.SemgrepRules, s.RuleID) {
					f.SemgrepRules = append(f.SemgrepRules, s.RuleID)
				}
				used[j] = true
			}
		}
	}
	var staticOnly []SemgrepFinding
	for j, s := range sg {
		if !used[j] {
			staticOnly = append(staticOnly, s)
		}
	}
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)),
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
		)
	})
	return findings, staticOnly
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// SemgrepFinding semgrep --json 输出中的一条结果。
type SemgrepFinding struct {
	RuleID   string
	Path     string
	Line     int
	EndLine  int
	Message  string
	Severity string
}

type semgrepOutput struct {
	Results []struct {
		CheckID string `json:"check_id"`
		Path    string `json:"path"`
		Start   struct {
			Line int `json:"line"`
		} `json:"start"`
		End struct {
			Line int `json:"line"`
		} `json:"end"`
		Extra struct {
			Message  string `json:"message"`
			Severity string `json:"severity"`
		} `json:"extra"`
	} `json:"results"`
}

func loadSemgrep(path string) ([]SemgrepFinding, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- 路径来自 CI 配置
	if err != nil {
		return nil, fmt.Errorf("read semgrep output: %w", err)
	}
	var out semgrepOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse semgrep output: %w", err)
	}
	findings := make([]SemgrepFinding, 0, len(out.Results))
	for _, r := range out.Results {
		findings = append(findings, SemgrepFinding{
			RuleID: r.CheckID, Path: r.Path, Line: r.Start.Line, EndLine: r.End.Line,
			Message: r.Extra.Message, Severity: r.Extra.Severity,
		})
	}
	return findings, nil
}

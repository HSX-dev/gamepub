// ai-review MR 级 AI 辅助代码审查：收集 diff → 脱敏 → Claude 结构化审查 → 与 Semgrep 交叉验证 → 生成报告初稿。
//
// 定位是“初筛 + 分类 + 起草”，不是门禁：任何结论都需要人工复核，CI 中该任务 allow_failure。
//
// 用法：
//
//	semgrep scan --config security/semgrep/rules --json -o semgrep.json
//	ai-review -base origin/main -head HEAD -semgrep semgrep.json -out ai-review.md
//	ai-review -dry-run   # 只做 diff 收集与脱敏，不调用模型
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

type options struct {
	base, head   string
	semgrep      string
	checklist    string
	out, jsonOut string
	model        string
	maxDiffBytes int
	dryRun       bool
	postGitLab   bool
}

func main() {
	var o options
	flag.StringVar(&o.base, "base", envOr("CI_MERGE_REQUEST_DIFF_BASE_SHA", "origin/main"), "对比基准")
	flag.StringVar(&o.head, "head", envOr("CI_COMMIT_SHA", "HEAD"), "待审查版本")
	flag.StringVar(&o.semgrep, "semgrep", "", "semgrep --json 输出文件（可选）")
	flag.StringVar(&o.checklist, "checklist", "docs/audit/checklist.md", "审查清单，作为系统提示的一部分")
	flag.StringVar(&o.out, "out", "ai-review.md", "Markdown 报告输出路径")
	flag.StringVar(&o.jsonOut, "json", "", "结构化结果输出路径（可选）")
	flag.StringVar(&o.model, "model", envOr("AI_REVIEW_MODEL", "claude-opus-5-5"), "模型 ID")
	flag.IntVar(&o.maxDiffBytes, "max-diff-bytes", 400_000, "单批 diff 上限（字节）")
	flag.BoolVar(&o.dryRun, "dry-run", false, "不调用模型，只输出 diff 统计与脱敏结果")
	flag.BoolVar(&o.postGitLab, "gitlab-note", false, "将报告发布为 MR 评论（需要 GITLAB_REVIEW_TOKEN）")
	flag.Parse()

	if err := runWithSignals(o); err != nil {
		fmt.Fprintln(os.Stderr, "ai-review:", err)
		os.Exit(1)
	}
}

func runWithSignals(o options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx, o)
}

func run(ctx context.Context, o options) error {
	raw, err := collectDiff(ctx, o.base, o.head)
	if err != nil {
		return err
	}
	redacted, redactions := redact(raw)
	files := splitDiff(redacted)
	if len(files) == 0 {
		fmt.Println("no reviewable changes")
		return nil
	}
	batches := chunk(files, o.maxDiffBytes)
	sg, err := loadSemgrep(o.semgrep)
	if err != nil {
		return err
	}
	fmt.Printf("files=%d batches=%d redactions=%d semgrep=%d\n", len(files), len(batches), redactions, len(sg))
	if o.dryRun {
		return nil
	}

	checklist := ""
	if data, err := os.ReadFile(o.checklist); err == nil { // #nosec G304 -- 仓库内文件
		checklist = string(data)
	}
	// 凭据由 SDK 从环境解析（ANTHROPIC_API_KEY 等），CI 中以受保护、仅 MR 流水线可见的变量注入
	reviewer := &Reviewer{client: anthropic.NewClient(), model: o.model, checklist: checklist}

	var (
		all       []Finding
		summaries []string
		failed    int
	)
	for i, batch := range batches {
		res, err := reviewer.Review(ctx, batch, sg)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 单批失败不影响其他批次；报告中显式标注，提醒人工补审
			fmt.Fprintf(os.Stderr, "batch %d/%d failed: %v\n", i+1, len(batches), err)
			failed++
			continue
		}
		all = append(all, res.Findings...)
		if res.Summary != "" {
			summaries = append(summaries, res.Summary)
		}
	}
	merged, staticOnly := crossValidate(all, sg)

	report := renderReport(ReportMeta{
		Base: o.base, Head: o.head, Model: o.model, Files: len(files), Batches: len(batches),
		Redactions: redactions, FailedBatchs: failed, GeneratedAt: time.Now(),
	}, merged, staticOnly, summaries)
	if err := os.WriteFile(o.out, []byte(report), 0o600); err != nil {
		return err
	}
	if o.jsonOut != "" {
		data, _ := json.MarshalIndent(map[string]any{"findings": merged, "static_only": staticOnly}, "", "  ")
		if err := os.WriteFile(o.jsonOut, data, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("findings=%d static_only=%d report=%s\n", len(merged), len(staticOnly), o.out)

	if o.postGitLab {
		return postMRNote(ctx, report)
	}
	return nil
}

// postMRNote 把报告发布为 MR 评论。使用权限最小化的项目访问令牌（仅 api 范围、Reporter 角色）。
func postMRNote(ctx context.Context, body string) error {
	api, project, mr, token := os.Getenv("CI_API_V4_URL"), os.Getenv("CI_PROJECT_ID"), os.Getenv("CI_MERGE_REQUEST_IID"), os.Getenv("GITLAB_REVIEW_TOKEN")
	if api == "" || project == "" || mr == "" || token == "" {
		return errors.New("gitlab-note requires CI_API_V4_URL, CI_PROJECT_ID, CI_MERGE_REQUEST_IID and GITLAB_REVIEW_TOKEN")
	}
	// 令牌随请求发出，必须确认目标是 HTTPS，防止被篡改的变量把令牌送到明文或其他地址
	base, err := url.Parse(api)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return errors.New("gitlab-note: CI_API_V4_URL must be an https URL")
	}
	endpoint := base.JoinPath("projects", project, "merge_requests", mr, "notes").String()
	payload, _ := json.Marshal(map[string]string{"body": body})
	// #nosec G704 -- 目标为 GitLab Runner 注入的 CI_API_V4_URL（已校验 https），非用户输入
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req) // #nosec G704 -- 同上
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("gitlab note: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

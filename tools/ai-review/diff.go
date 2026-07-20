package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// FileDiff 单个文件的变更。
type FileDiff struct {
	Path  string
	Patch string // 已标注新文件行号的 diff
}

// gitRefPattern 限制 git 引用格式。base/head 来自 CI 变量或命令行参数，
// 若允许以 "-" 开头，攻击者可构造 "--output=/path" 之类的参数注入（CWE-88）。
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^-]{0,199}$`)

func validRef(ref string) bool { return gitRefPattern.MatchString(ref) && !strings.Contains(ref, "..") }

// collectDiff 获取 base...head 之间的变更（三点语法：相对合并基准），排除生成代码与测试数据。
func collectDiff(ctx context.Context, base, head string) (string, error) {
	if !validRef(base) || !validRef(head) {
		return "", errors.New("invalid git ref")
	}
	args := []string{
		"diff", "--no-color", "--no-ext-diff", "--unified=20", "--diff-filter=AMR",
		base + "..." + head, "--",
		"*.go", "*.proto", "*.yaml", "*.yml", "*Dockerfile*", "*.sql",
		":(exclude)api/gen/**", ":(exclude)**/testdata/**", ":(exclude)vendor/**",
	}
	out, err := exec.CommandContext(ctx, "git", args...).Output() // #nosec G204 -- 参数为固定选项 + 已校验的引用
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git diff: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// splitDiff 按文件拆分 unified diff，并为每行标注新文件行号，帮助模型准确定位。
func splitDiff(raw string) []FileDiff {
	var (
		files []FileDiff
		cur   *FileDiff
		buf   strings.Builder
		line  int
	)
	flush := func() {
		if cur != nil && cur.Path != "" {
			cur.Patch = buf.String()
			files = append(files, *cur)
		}
		buf.Reset()
	}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			cur = &FileDiff{}
		case cur == nil:
			continue
		case strings.HasPrefix(l, "+++ "):
			cur.Path = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "new file"),
			strings.HasPrefix(l, "similarity"), strings.HasPrefix(l, "rename "):
			continue
		case strings.HasPrefix(l, "@@"):
			line = hunkStart(l)
			buf.WriteString(l + "\n")
		case strings.HasPrefix(l, "+"):
			fmt.Fprintf(&buf, "L%-5d+%s\n", line, l[1:])
			line++
		case strings.HasPrefix(l, "-"):
			fmt.Fprintf(&buf, "      -%s\n", l[1:])
		default:
			fmt.Fprintf(&buf, "L%-5d %s\n", line, strings.TrimPrefix(l, " "))
			line++
		}
	}
	flush()
	return files
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func hunkStart(h string) int {
	m := hunkHeader.FindStringSubmatch(h)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// chunk 将文件按大小分组，单个请求不超过 maxBytes。超大的单文件单独成组。
func chunk(files []FileDiff, maxBytes int) [][]FileDiff {
	var (
		out  [][]FileDiff
		cur  []FileDiff
		size int
	)
	for _, f := range files {
		if size > 0 && size+len(f.Patch) > maxBytes {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += len(f.Patch)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

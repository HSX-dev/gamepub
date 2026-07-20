package main

import "regexp"

// 外发给第三方模型前的兜底脱敏。即使 Secret 不应出现在代码里，审查工具也必须假设它会出现：
// 一次误提交的密钥若随 diff 发出，影响面就从仓库扩大到了外部服务。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                         // AWS Access Key
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`),                                  // Anthropic API key
	regexp.MustCompile(`\b(?:ghp|gho|ghs)_[A-Za-z0-9]{30,}\b`),                         // GitHub token
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),                                 // GitLab token
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^:@\s/"']+:[^@\s/"']+@`),            // URL 内嵌凭据（DSN 等）
	// 赋值给敏感名字的字符串字面量；只匹配引号内的值，不误伤 req.Password 这类代码
	regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|private_?key|dsn)([A-Za-z0-9_]*"?\s*[:=]\s*)"[^"\s]{8,}"`),
}

// redact 返回脱敏后的文本与替换次数。
func redact(s string) (string, int) {
	total := 0
	for i, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			total++
			if i == len(secretPatterns)-1 {
				// 保留字段名，便于模型理解“这里硬编码了密钥”本身就是问题
				return re.ReplaceAllString(m, `${1}${2}"<REDACTED>"`)
			}
			return "<REDACTED>"
		})
	}
	return s, total
}

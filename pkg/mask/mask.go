// Package mask 提供统一的敏感信息脱敏能力，供日志、错误响应、审计报告与 AI 审查外发前的预处理复用。
//
// 设计原则：
//   - 按字段名识别敏感数据（password/token/phone/card...），业务代码无需逐处手写脱敏；
//   - 短值整体打码，避免“保留首尾”后仍可被还原；
//   - 无法解析的报文不原样输出，宁可丢弃也不泄露。
package mask

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

type kind int

const (
	kindSecret kind = iota + 1
	kindToken
	kindPhone
	kindIDCard
	kindBankCard
	kindEmail
)

var sensitiveKeys = map[string]kind{
	"password":         kindSecret,
	"passwd":           kindSecret,
	"pwd":              kindSecret,
	"old_password":     kindSecret,
	"new_password":     kindSecret,
	"secret":           kindSecret,
	"client_secret":    kindSecret,
	"api_key":          kindSecret,
	"apikey":           kindSecret,
	"private_key":      kindSecret,
	"dsn":              kindSecret,
	"cvv":              kindSecret,
	"sign":             kindSecret,
	"signature":        kindSecret,
	"x_pay_signature":  kindSecret,
	"cookie":           kindSecret,
	"set_cookie":       kindSecret,
	"x_internal_token": kindSecret,
	"token":            kindToken,
	"access_token":     kindToken,
	"refresh_token":    kindToken,
	"authorization":    kindToken,
	"phone":            kindPhone,
	"mobile":           kindPhone,
	"phone_number":     kindPhone,
	"id_card":          kindIDCard,
	"idcard":           kindIDCard,
	"id_no":            kindIDCard,
	"card_no":          kindBankCard,
	"bank_card":        kindBankCard,
	"card_number":      kindBankCard,
	"email":            kindEmail,
}

func normalize(key string) string {
	return strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(key)))
}

// IsSensitiveKey 判断字段名是否属于敏感字段。
func IsSensitiveKey(key string) bool {
	_, ok := sensitiveKeys[normalize(key)]
	return ok
}

// ByKey 根据字段名对值脱敏；第二个返回值表示该字段是否敏感。
func ByKey(key, value string) (string, bool) {
	k, ok := sensitiveKeys[normalize(key)]
	if !ok {
		return value, false
	}
	switch k {
	case kindPhone:
		return Phone(value), true
	case kindIDCard:
		return IDCard(value), true
	case kindBankCard:
		return BankCard(value), true
	case kindEmail:
		return Email(value), true
	case kindToken:
		return Token(value), true
	default:
		return Secret(value), true
	}
}

// Middle 保留首 head 个、尾 tail 个字符，中间替换为 *。长度不足时整体打码。
func Middle(s string, head, tail int) string {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return ""
	}
	if n <= head+tail {
		return strings.Repeat("*", n)
	}
	r := []rune(s)
	return string(r[:head]) + strings.Repeat("*", n-head-tail) + string(r[n-tail:])
}

// Phone 13812345678 → 138****5678
func Phone(s string) string { return Middle(s, 3, 4) }

// IDCard 110101199001011234 → 110***********1234
func IDCard(s string) string { return Middle(s, 3, 4) }

// BankCard 6222020000001234 → ************1234
func BankCard(s string) string { return Middle(s, 0, 4) }

// Email alice@example.com → a****@example.com
func Email(s string) string {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 {
		return Middle(s, 0, 0)
	}
	return Middle(s[:at], 1, 0) + s[at:]
}

// Token 仅保留前缀便于排查，剩余部分不足以重放。
func Token(s string) string {
	s = strings.TrimSpace(s)
	if scheme, rest, ok := strings.Cut(s, " "); ok && strings.EqualFold(scheme, "bearer") {
		return "Bearer " + Token(rest)
	}
	if len(s) <= 16 {
		return "***"
	}
	return s[:6] + "***"
}

// Secret 口令、密钥等不保留任何字符。
func Secret(string) string { return "***" }

// JSON 递归脱敏 JSON 报文中的敏感字段；非 JSON 报文直接丢弃正文，避免原样落日志。
func JSON(data []byte) []byte {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return []byte(`"<non-json body omitted>"`)
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return []byte(`"<body omitted>"`)
	}
	return out
}

func walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if !IsSensitiveKey(k) {
				t[k] = walk(val)
				continue
			}
			if s, ok := val.(string); ok {
				t[k], _ = ByKey(k, s)
			} else {
				t[k] = "***"
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = walk(t[i])
		}
		return t
	default:
		return v
	}
}

var (
	phoneInText  = regexp.MustCompile(`(^|\D)(1[3-9]\d)\d{4}(\d{4})($|\D)`)
	bearerInText = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9\-_.=]+`)
	kvInText     = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key)(\s*[:=]\s*)("[^"]*"|\S+)`)
)

// Text 对自由文本（错误信息、异常堆栈、外发给 LLM 的代码片段）做兜底脱敏。
func Text(s string) string {
	s = phoneInText.ReplaceAllString(s, "${1}${2}****${3}${4}")
	s = bearerInText.ReplaceAllString(s, "${1}***")
	return kvInText.ReplaceAllString(s, "${1}${2}***")
}

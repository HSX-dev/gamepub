// Package sqlsafe 动态 SQL 片段的安全构造：排序字段白名单、LIKE 转义、分页上限。
//
// 参数化查询（? 占位）只能保护“值”，保护不了列名、排序方向、表名等标识符。
// 审计中最常见的 GORM 注入正是 db.Order(c.Query("sort")) 或 Raw/Where + fmt.Sprintf 拼接，
// 这里统一用白名单把外部输入映射为代码内的常量列名。
package sqlsafe

import (
	"errors"
	"strings"

	"gorm.io/gorm/clause"
)

// ErrInvalidSortField 排序字段不在白名单内。
var ErrInvalidSortField = errors.New("sqlsafe: sort field not allowed")

// SortFields API 字段名 → 数据库列名。列名只来自代码常量。
type SortFields map[string]string

// OrderBy 返回 GORM 排序子句；非法或为空的字段回落到 fallback 列。
// clause.Column 会被方言正确转义（`col`），不会被当作 SQL 片段解析。
func (f SortFields) OrderBy(field string, desc bool, fallback string) clause.OrderByColumn {
	col, ok := f[field]
	if !ok {
		col = fallback
	}
	return clause.OrderByColumn{Column: clause.Column{Name: col}, Desc: desc}
}

// Resolve 严格模式：非空且不在白名单时返回错误，便于向调用方提示参数非法。
func (f SortFields) Resolve(field string) (string, error) {
	if field == "" {
		return "", nil
	}
	col, ok := f[field]
	if !ok {
		return "", ErrInvalidSortField
	}
	return col, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike 转义 LIKE 通配符，防止用户输入 "%" 触发全表扫描或越权模糊匹配。
// 使用方式：db.Where("name LIKE ?", "%"+sqlsafe.EscapeLike(kw)+"%")
func EscapeLike(s string) string { return likeEscaper.Replace(s) }

const (
	// MaxPageSize 单页上限，防止 page_size=100000 一次拖垮数据库与内存。
	MaxPageSize = 100
	// MaxOffset 深分页上限，更深的翻页应改用游标分页。
	MaxOffset = 10000
)

// Page 规范化分页参数，返回 offset 与 limit。
func Page(page, size int32, defaultSize int) (offset, limit int) {
	limit = int(size)
	if limit <= 0 {
		limit = defaultSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	p := int(page)
	if p <= 0 {
		p = 1
	}
	offset = (p - 1) * limit
	if offset > MaxOffset {
		offset = MaxOffset
	}
	return offset, limit
}

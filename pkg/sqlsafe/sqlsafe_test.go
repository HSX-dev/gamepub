package sqlsafe

import (
	"math"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type ledger struct {
	ID        uint64
	Amount    int64
	CreatedAt int64
}

var ledgerSort = SortFields{"created_at": "created_at", "amount": "amount"}

func dryRun(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOrderByWhitelist(t *testing.T) {
	db := dryRun(t)
	cases := map[string]string{
		"amount":                          "ORDER BY `amount` DESC",
		"created_at; DROP TABLE ledger--": "ORDER BY `id` DESC",
		"(CASE WHEN 1=1 THEN id END)":     "ORDER BY `id` DESC",
		"":                                "ORDER BY `id` DESC",
	}
	for input, want := range cases {
		var rows []ledger
		stmt := db.Order(ledgerSort.OrderBy(input, true, "id")).Find(&rows).Statement
		if sql := stmt.SQL.String(); !strings.Contains(sql, want) {
			t.Errorf("OrderBy(%q) produced %q, want contains %q", input, sql, want)
		}
	}
	if _, err := ledgerSort.Resolve("password"); err == nil {
		t.Error("Resolve should reject fields outside whitelist")
	}
}

func TestEscapeLike(t *testing.T) {
	if got := EscapeLike(`50%_off\`); got != `50\%\_off\\` {
		t.Fatalf("EscapeLike = %q", got)
	}
}

func TestPage(t *testing.T) {
	cases := []struct {
		page, size  int32
		off, limit  int
		description string
	}{
		{0, 0, 0, 20, "defaults"},
		{3, 10, 20, 10, "normal"},
		{1, 100000, 0, MaxPageSize, "page size capped"},
		{math.MaxInt32, 100, MaxOffset, 100, "deep pagination capped without overflow"},
		{-5, -1, 0, 20, "negative values"},
	}
	for _, c := range cases {
		off, limit := Page(c.page, c.size, 20)
		if off != c.off || limit != c.limit {
			t.Errorf("%s: Page(%d,%d) = (%d,%d), want (%d,%d)", c.description, c.page, c.size, off, limit, c.off, c.limit)
		}
	}
}

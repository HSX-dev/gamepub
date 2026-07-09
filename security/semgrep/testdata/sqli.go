package testdata

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Order struct{ ID uint64 }

func vulnerable(c *gin.Context, db *gorm.DB) {
	var rows []Order
	uid := c.Query("uid")

	// ruleid: gorm-sql-string-concat
	db.Where("user_id = " + uid).Find(&rows)

	// ruleid: gorm-sql-string-concat
	db.Raw(fmt.Sprintf("SELECT * FROM order_order WHERE status = '%s'", c.Query("status"))).Scan(&rows)

	q := "SELECT * FROM order_order WHERE game_id = '" + c.Query("game") + "'"
	// ruleid: gorm-sql-string-concat
	db.Raw(q).Scan(&rows)

	sort := c.Query("sort")
	// ruleid: gorm-identifier-from-request
	db.Order(sort).Find(&rows)
}

func vulnerableGRPC(db *gorm.DB, req interface{ GetSortBy() string }) {
	var rows []Order
	// ruleid: gorm-identifier-from-request, gorm-sql-string-concat
	db.Order(req.GetSortBy() + " DESC").Find(&rows)
}

func safe(c *gin.Context, db *gorm.DB, whitelist interface {
	OrderBy(string, bool, string) any
}) {
	var rows []Order
	// ok: gorm-sql-string-concat
	db.Where("user_id = ?", c.Query("uid")).Find(&rows)
	// ok: gorm-sql-string-concat
	db.Order("id DESC").Find(&rows)
	// ok: gorm-sql-string-concat
	db.Where("available + ? >= 0", 10).Find(&rows)
	// ok: gorm-identifier-from-request
	db.Order(whitelist.OrderBy(c.Query("sort"), true, "id")).Find(&rows)
}

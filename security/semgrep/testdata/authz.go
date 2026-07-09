package testdata

import (
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

type handlers struct{ db *gorm.DB }

func (h *handlers) getOrder(c *gin.Context) {
	var o Order
	// ruleid: order-lookup-without-owner
	h.db.Where("order_no = ?", c.Param("order_no")).Take(&o)

	// ok: order-lookup-without-owner
	h.db.Where("order_no = ? AND user_id = ?", c.Param("order_no"), 1).Take(&o)
}

func (h *handlers) batch(nos []string) {
	var rows []Order
	// ruleid: order-lookup-without-owner
	h.db.Where("order_no IN ?", nos).Find(&rows)
	// ok: order-lookup-without-owner
	h.db.Where("user_id = ? AND order_no IN ?", 1, nos).Find(&rows)
}

func (h *handlers) payNotify(orderNo string) {
	var o Order
	// 回调由渠道签名鉴权，不存在用户会话
	// nosemgrep: order-lookup-without-owner
	h.db.Where("order_no = ?", orderNo).Take(&o)
}

func (h *handlers) listOrders(c *gin.Context) {
	// ruleid: user-id-from-request
	uid := c.Query("user_id")
	_ = uid
}

func (h *handlers) adminOrders(c *gin.Context) {
	// ok: user-id-from-request
	uid := c.Query("user_id")
	_ = uid
}

func parse(raw string, key []byte) {
	keyFunc := func(*jwt.Token) (any, error) { return key, nil }
	// ruleid: jwt-parse-without-valid-methods
	_, _ = jwt.Parse(raw, keyFunc)
	// ok: jwt-parse-without-valid-methods
	_, _ = jwt.Parse(raw, keyFunc, jwt.WithValidMethods([]string{"EdDSA"}))
}

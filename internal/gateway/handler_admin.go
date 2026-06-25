package gateway

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/gin-gonic/gin"

	orderv1 "gamepub/api/gen/order/v1"
	walletv1 "gamepub/api/gen/wallet/v1"
)

func operator(c *gin.Context) string { return "staff:" + strconv.FormatUint(userID(c), 10) }

func (h *handlers) adminOrders(c *gin.Context) {
	var uid uint64
	if s := c.Query("user_id"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			badRequest(c, "invalid user_id")
			return
		}
		uid = n
	}
	resp, err := h.Order.AdminListOrders(c.Request.Context(), &orderv1.AdminListOrdersRequest{
		UserId: uid, Status: c.Query("status"), GameId: c.Query("game_id"),
		SortBy: c.Query("sort_by"), Desc: c.Query("order") == "desc",
		Page: queryInt32(c, "page"), PageSize: queryInt32(c, "page_size"),
	})
	if err != nil {
		grpcFail(c, err)
		return
	}
	type adminOrder struct {
		orderDTO
		UserID uint64 `json:"user_id"`
	}
	out := make([]adminOrder, 0, len(resp.GetOrders()))
	for _, o := range resp.GetOrders() {
		out = append(out, adminOrder{orderDTO: toOrderDTO(o), UserID: o.GetUserId()})
	}
	ok(c, gin.H{"orders": out, "total": resp.GetTotal()})
}

type upsertGameReq struct {
	Name        string `json:"name" binding:"required,max=64"`
	DeliveryURL string `json:"delivery_url" binding:"omitempty,max=512"`
}

func (h *handlers) adminUpsertGame(c *gin.Context) {
	var req upsertGameReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body")
		return
	}
	resp, err := h.Order.UpsertGameApp(c.Request.Context(), &orderv1.UpsertGameAppRequest{
		GameId: c.Param("game_id"), Name: req.Name, DeliveryUrl: req.DeliveryURL, Operator: operator(c),
	})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, resp.GetApp())
}

func (h *handlers) adminTestDelivery(c *gin.Context) {
	resp, err := h.Order.TestDelivery(c.Request.Context(), &orderv1.TestDeliveryRequest{GameId: c.Param("game_id")})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"status_code": resp.GetStatusCode(), "latency_ms": resp.GetLatencyMs(), "reachable": resp.GetStatusCode() != 0})
}

// maxManualAdjust 单笔人工调账上限，超出需走审批流程（不在系统内直接放行）。
const maxManualAdjust = 100_000

var adjustKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,56}$`)

type adjustReq struct {
	UserID         uint64 `json:"user_id" binding:"required"`
	Amount         int64  `json:"amount" binding:"required"` // 正数补发，负数扣回
	Reason         string `json:"reason" binding:"required,min=4,max=200"`
	IdempotencyKey string `json:"idempotency_key" binding:"required"`
}

// adminAdjustWallet 人工调账：高风险操作，要求独立权限点、填写原因、携带幂等键并记录审计日志。
func (h *handlers) adminAdjustWallet(c *gin.Context) {
	var req adjustReq
	if err := c.ShouldBindJSON(&req); err != nil || !adjustKeyPattern.MatchString(req.IdempotencyKey) {
		badRequest(c, "invalid request body")
		return
	}
	if req.Amount == 0 || req.Amount > maxManualAdjust || req.Amount < -maxManualAdjust {
		badRequest(c, fmt.Sprintf("amount must be non-zero and within ±%d", maxManualAdjust))
		return
	}
	ctx := c.Request.Context()
	bizID := "adj:" + req.IdempotencyKey
	remark := fmt.Sprintf("%s reason=%s", operator(c), req.Reason)
	var (
		balanceAfter int64
		duplicated   bool
		err          error
	)
	if req.Amount > 0 {
		var resp *walletv1.CreditResponse
		resp, err = h.Wallet.Credit(ctx, &walletv1.CreditRequest{UserId: req.UserID, Amount: req.Amount, BizType: "admin_adjust", BizId: bizID, Remark: remark})
		balanceAfter, duplicated = resp.GetBalanceAfter(), resp.GetDuplicated()
	} else {
		var resp *walletv1.DebitResponse
		resp, err = h.Wallet.Debit(ctx, &walletv1.DebitRequest{UserId: req.UserID, Amount: -req.Amount, BizType: "admin_adjust", BizId: bizID, Remark: remark})
		balanceAfter, duplicated = resp.GetBalanceAfter(), resp.GetDuplicated()
	}
	if err != nil {
		grpcFail(c, err)
		return
	}
	h.Logger.InfoContext(ctx, "audit: wallet adjusted",
		"operator", operator(c), "target_user", req.UserID, "amount", req.Amount, "biz_id", bizID, "duplicated", duplicated)
	ok(c, gin.H{"balance_after": balanceAfter, "duplicated": duplicated})
}

func (h *handlers) adminReconcileDiffs(c *gin.Context) {
	resp, err := h.Wallet.ListReconcileDiffs(c.Request.Context(), &walletv1.ListReconcileDiffsRequest{Limit: 100})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"diffs": resp.GetDiffs()})
}

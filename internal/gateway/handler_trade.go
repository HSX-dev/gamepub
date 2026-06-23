package gateway

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	orderv1 "gamepub/api/gen/order/v1"
	taskv1 "gamepub/api/gen/task/v1"
	walletv1 "gamepub/api/gen/wallet/v1"
	"gamepub/pkg/signature"
)

// 所有玩家接口的 user_id 只取自令牌（userID(c)），绝不读取 query/body/path 中的 user_id。

func (h *handlers) balance(c *gin.Context) {
	resp, err := h.Wallet.GetBalance(c.Request.Context(), &walletv1.GetBalanceRequest{UserId: userID(c)})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"available": resp.GetAvailable(), "frozen": resp.GetFrozen()})
}

func (h *handlers) ledger(c *gin.Context) {
	resp, err := h.Wallet.ListLedger(c.Request.Context(), &walletv1.ListLedgerRequest{
		UserId:   userID(c),
		BizType:  c.Query("biz_type"),
		SortBy:   c.Query("sort_by"), // 服务端白名单映射，非法值回落默认排序
		Desc:     c.Query("order") == "desc",
		Page:     queryInt32(c, "page"),
		PageSize: queryInt32(c, "page_size"),
	})
	if err != nil {
		grpcFail(c, err)
		return
	}
	type entry struct {
		BizType      string `json:"biz_type"`
		Amount       int64  `json:"amount"`
		BalanceAfter int64  `json:"balance_after"`
		Remark       string `json:"remark"`
		CreatedAt    int64  `json:"created_at"`
	}
	out := make([]entry, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		out = append(out, entry{e.GetBizType(), e.GetAmount(), e.GetBalanceAfter(), e.GetRemark(), e.GetCreatedAt()})
	}
	ok(c, gin.H{"entries": out, "total": resp.GetTotal()})
}

type orderDTO struct {
	OrderNo   string `json:"order_no"`
	GameID    string `json:"game_id"`
	ProductID string `json:"product_id"`
	Amount    int64  `json:"amount"`
	Coin      int64  `json:"coin"`
	Status    string `json:"status"`
	Channel   string `json:"channel"`
	CreatedAt int64  `json:"created_at"`
	PaidAt    int64  `json:"paid_at,omitempty"`
}

func toOrderDTO(o *orderv1.Order) orderDTO {
	return orderDTO{
		OrderNo: o.GetOrderNo(), GameID: o.GetGameId(), ProductID: o.GetProductId(), Amount: o.GetAmount(),
		Coin: o.GetCoin(), Status: o.GetStatus(), Channel: o.GetChannel(), CreatedAt: o.GetCreatedAt(), PaidAt: o.GetPaidAt(),
	}
}

func toOrderDTOs(list []*orderv1.Order) []orderDTO {
	out := make([]orderDTO, 0, len(list))
	for _, o := range list {
		out = append(out, toOrderDTO(o))
	}
	return out
}

// createOrderReq 不包含金额字段：金额由订单服务按商品目录计算。
type createOrderReq struct {
	GameID    string `json:"game_id" binding:"required,max=32"`
	ProductID string `json:"product_id" binding:"required,max=32"`
	Channel   string `json:"channel" binding:"required,max=16"`
}

func (h *handlers) createOrder(c *gin.Context) {
	var req createOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body")
		return
	}
	resp, err := h.Order.CreateOrder(c.Request.Context(), &orderv1.CreateOrderRequest{
		UserId: userID(c), GameId: req.GameID, ProductId: req.ProductID, Channel: req.Channel,
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
	})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"order": toOrderDTO(resp.GetOrder()), "pay_url": resp.GetPayUrl()})
}

func (h *handlers) listOrders(c *gin.Context) {
	resp, err := h.Order.ListOrders(c.Request.Context(), &orderv1.ListOrdersRequest{
		UserId: userID(c), Status: c.Query("status"), Page: queryInt32(c, "page"), PageSize: queryInt32(c, "page_size"),
	})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"orders": toOrderDTOs(resp.GetOrders()), "total": resp.GetTotal()})
}

func (h *handlers) getOrder(c *gin.Context) {
	resp, err := h.Order.GetOrder(c.Request.Context(), &orderv1.GetOrderRequest{UserId: userID(c), OrderNo: c.Param("order_no")})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, toOrderDTO(resp.GetOrder()))
}

type batchOrdersReq struct {
	OrderNos []string `json:"order_nos" binding:"required,min=1,max=50,dive,max=32"`
}

func (h *handlers) batchOrders(c *gin.Context) {
	var req batchOrdersReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "order_nos must contain 1-50 items")
		return
	}
	resp, err := h.Order.BatchGetOrders(c.Request.Context(), &orderv1.BatchGetOrdersRequest{UserId: userID(c), OrderNos: req.OrderNos})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"orders": toOrderDTOs(resp.GetOrders())})
}

func (h *handlers) listTasks(c *gin.Context) {
	resp, err := h.Task.ListTasks(c.Request.Context(), &taskv1.ListTasksRequest{UserId: userID(c)})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"tasks": resp.GetTasks()})
}

func (h *handlers) claimTask(c *gin.Context) {
	resp, err := h.Task.ClaimReward(c.Request.Context(), &taskv1.ClaimRewardRequest{UserId: userID(c), TaskId: c.Param("task_id")})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"reward_coin": resp.GetRewardCoin(), "balance_after": resp.GetBalanceAfter()})
}

// payNotify 支付渠道异步通知。原始报文与签名头原样透传给订单服务验签，网关不解析、不持有渠道密钥。
func (h *handlers) payNotify(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.String(http.StatusRequestEntityTooLarge, "fail")
			return
		}
		c.String(http.StatusBadRequest, "fail")
		return
	}
	headers := map[string]string{}
	for _, k := range []string{signature.HeaderTimestamp, signature.HeaderNonce, signature.HeaderSignature} {
		headers[k] = c.GetHeader(k)
	}
	resp, err := h.Order.HandlePayNotify(c.Request.Context(), &orderv1.HandlePayNotifyRequest{
		Channel: c.Param("channel"), Body: body, Headers: headers,
	})
	if err != nil {
		h.Logger.WarnContext(c.Request.Context(), "pay notify failed", "channel", c.Param("channel"), "err", err)
		// 渠道协议：非 success 应答即视为失败并按策略重试
		c.String(http.StatusBadRequest, "fail")
		return
	}
	c.String(http.StatusOK, resp.GetReply())
}

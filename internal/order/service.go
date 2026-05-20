// Package order 支付订单、支付回调、事务性发件箱与游戏发货通知。
package order

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gamepub/pkg/cryptox"
	"gamepub/pkg/dbx"
	"gamepub/pkg/netguard"
	"gamepub/pkg/signature"
	"gamepub/pkg/sqlsafe"
)

var (
	ErrInvalidArgument     = errors.New("order: invalid argument")
	ErrProductNotFound     = errors.New("order: product not found")
	ErrGameNotFound        = errors.New("order: game not found")
	ErrChannelNotSupported = errors.New("order: payment channel not supported")
	ErrOrderNotFound       = errors.New("order: order not found")
	ErrTooManyOrders       = errors.New("order: too many order numbers in one request")
	ErrInvalidDeliveryURL  = errors.New("order: delivery url not allowed")
)

// MaxBatch 批量查询上限：批量接口是越权遍历与拖库的放大器，必须限量（网关另有限流）。
const MaxBatch = 50

var (
	orderNoPattern = regexp.MustCompile(`^[0-9A-F]{26}$`)
	gameIDPattern  = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)
	idemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	validStatus    = map[string]bool{StatusPending: true, StatusPaid: true, StatusClosed: true}
	adminSort      = sqlsafe.SortFields{"created_at": "created_at", "amount": "amount", "paid_at": "paid_at", "id": "id"}
)

// Options 服务依赖。
type Options struct {
	DB           *gorm.DB
	PayVerifiers map[string]*signature.Verifier // 渠道 → 验签器
	Guard        *netguard.Guard
	Notifier     *Notifier
	PayBaseURL   string
	Logger       *slog.Logger
}

// Service 订单领域服务。
type Service struct {
	db         *gorm.DB
	verifiers  map[string]*signature.Verifier
	guard      *netguard.Guard
	notifier   *Notifier
	payBaseURL string
	logger     *slog.Logger
	now        func() time.Time
}

// NewService 创建服务。
func NewService(o Options) *Service {
	return &Service{
		db: o.DB, verifiers: o.PayVerifiers, guard: o.Guard, notifier: o.Notifier,
		payBaseURL: o.PayBaseURL, logger: o.Logger, now: time.Now,
	}
}

// newOrderNo 时间前缀 + 48 位密码学随机数。随机部分不可预测，但越权防护不依赖它——
// 订单号可能出现在截图、客服工单里，真正的防线是查询时的归属校验。
func newOrderNo(now time.Time) string {
	return now.UTC().Format("20060102150405") + strings.ToUpper(cryptox.RandomHex(6))
}

// CreateOrderInput 下单参数。
type CreateOrderInput struct {
	UserID         uint64
	GameID         string
	ProductID      string
	Channel        string
	IdempotencyKey string
}

// CreateOrder 创建充值订单，金额取自服务端商品目录。
func (s *Service) CreateOrder(ctx context.Context, in CreateOrderInput) (*Order, string, error) {
	if in.UserID == 0 || !gameIDPattern.MatchString(in.GameID) {
		return nil, "", ErrInvalidArgument
	}
	if in.IdempotencyKey != "" && !idemKeyPattern.MatchString(in.IdempotencyKey) {
		return nil, "", ErrInvalidArgument
	}
	if _, ok := s.verifiers[in.Channel]; !ok {
		return nil, "", ErrChannelNotSupported
	}
	p, ok := LookupProduct(in.ProductID)
	if !ok {
		return nil, "", ErrProductNotFound
	}
	db := s.db.WithContext(ctx)
	var games int64
	if err := db.Model(&GameApp{}).Where("game_id = ?", in.GameID).Count(&games).Error; err != nil {
		return nil, "", err
	}
	if games == 0 {
		return nil, "", ErrGameNotFound
	}
	if in.IdempotencyKey != "" {
		if o, err := s.byIdempotencyKey(ctx, in.UserID, in.IdempotencyKey); err == nil {
			return o, s.payURL(o), nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
	}

	o := &Order{
		OrderNo: newOrderNo(s.now()), UserID: in.UserID, GameID: in.GameID, ProductID: p.ID,
		Amount: p.Price, Coin: p.Coin, Channel: in.Channel, Status: StatusPending,
	}
	if in.IdempotencyKey != "" {
		key := in.IdempotencyKey
		o.IdempotencyKey = &key
	}
	if err := db.Create(o).Error; err != nil {
		if dbx.IsDuplicate(err) && in.IdempotencyKey != "" {
			// 并发重复提交：以先落库的那笔为准
			existing, rerr := s.byIdempotencyKey(ctx, in.UserID, in.IdempotencyKey)
			if rerr != nil {
				return nil, "", rerr
			}
			return existing, s.payURL(existing), nil
		}
		return nil, "", err
	}
	return o, s.payURL(o), nil
}

func (s *Service) byIdempotencyKey(ctx context.Context, userID uint64, key string) (*Order, error) {
	var o Order
	err := s.db.WithContext(ctx).Where("user_id = ? AND idempotency_key = ?", userID, key).Take(&o).Error
	return &o, err
}

func (s *Service) payURL(o *Order) string {
	return s.payBaseURL + "?" + url.Values{"order_no": {o.OrderNo}, "channel": {o.Channel}}.Encode()
}

// GetOrder 查询本人订单。WHERE 条件同时包含 order_no 与 user_id：
// 他人订单与不存在的订单返回同一个 NotFound，既防越权读取，也不泄露订单号是否存在。
func (s *Service) GetOrder(ctx context.Context, userID uint64, orderNo string) (*Order, error) {
	if userID == 0 || !orderNoPattern.MatchString(orderNo) {
		return nil, ErrOrderNotFound
	}
	var o Order
	err := s.db.WithContext(ctx).Where("order_no = ? AND user_id = ?", orderNo, userID).Take(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrOrderNotFound
	}
	return &o, err
}

// ListOrders 本人订单分页。
func (s *Service) ListOrders(ctx context.Context, userID uint64, status string, page, size int32) ([]Order, int64, error) {
	if userID == 0 || (status != "" && !validStatus[status]) {
		return nil, 0, ErrInvalidArgument
	}
	db := s.db.WithContext(ctx).Model(&Order{}).Where("user_id = ?", userID)
	if status != "" {
		db = db.Where("status = ?", status)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset, limit := sqlsafe.Page(page, size, 20)
	var rows []Order
	err := db.Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, err
}

// BatchGetOrders 批量查询本人订单，非本人的订单号被静默过滤。
func (s *Service) BatchGetOrders(ctx context.Context, userID uint64, orderNos []string) ([]Order, error) {
	if userID == 0 {
		return nil, ErrInvalidArgument
	}
	if len(orderNos) > MaxBatch {
		return nil, ErrTooManyOrders
	}
	seen := make(map[string]bool, len(orderNos))
	nos := make([]string, 0, len(orderNos))
	for _, n := range orderNos {
		if orderNoPattern.MatchString(n) && !seen[n] {
			seen[n] = true
			nos = append(nos, n)
		}
	}
	if len(nos) == 0 {
		return nil, nil
	}
	var rows []Order
	err := s.db.WithContext(ctx).Where("user_id = ? AND order_no IN ?", userID, nos).Order("id DESC").Find(&rows).Error
	return rows, err
}

// AdminQuery 运营后台查询条件。
type AdminQuery struct {
	UserID   uint64
	Status   string
	GameID   string
	SortBy   string
	Desc     bool
	Page     int32
	PageSize int32
}

// AdminListOrders 运营后台订单查询（网关已校验 order:read 权限）。
// 所有过滤条件参数化，排序字段走白名单——这是后台“高级筛选”最容易出现拼接注入的位置。
func (s *Service) AdminListOrders(ctx context.Context, q AdminQuery) ([]Order, int64, error) {
	db := s.db.WithContext(ctx).Model(&Order{})
	if q.UserID != 0 {
		db = db.Where("user_id = ?", q.UserID)
	}
	if q.Status != "" {
		if !validStatus[q.Status] {
			return nil, 0, ErrInvalidArgument
		}
		db = db.Where("status = ?", q.Status)
	}
	if q.GameID != "" {
		if !gameIDPattern.MatchString(q.GameID) {
			return nil, 0, ErrInvalidArgument
		}
		db = db.Where("game_id = ?", q.GameID)
	}
	if _, err := adminSort.Resolve(q.SortBy); err != nil {
		return nil, 0, ErrInvalidArgument
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	offset, limit := sqlsafe.Page(q.Page, q.PageSize, 20)
	var rows []Order
	err := db.Order(adminSort.OrderBy(q.SortBy, q.Desc, "id")).Offset(offset).Limit(limit).Find(&rows).Error
	return rows, total, err
}

func validName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if unicode.IsControl(r) || strings.ContainsRune(`<>"'&`, r) {
			return false
		}
	}
	return true
}

// UpsertGameApp 保存游戏接入配置。发货地址在保存时做 SSRF 前置校验，投递时由受保护的客户端再次校验。
func (s *Service) UpsertGameApp(ctx context.Context, gameID, name, deliveryURL, operator string) (*GameApp, error) {
	if !gameIDPattern.MatchString(gameID) || !validName(name) || operator == "" {
		return nil, ErrInvalidArgument
	}
	if deliveryURL != "" {
		u, err := s.guard.ValidateURL(ctx, deliveryURL)
		if err != nil {
			s.logger.WarnContext(ctx, "game app: delivery url rejected", "game_id", gameID, "operator", operator, "err", err)
			return nil, ErrInvalidDeliveryURL
		}
		deliveryURL = u.String()
	}
	app := &GameApp{GameID: gameID, Name: name, DeliveryURL: deliveryURL, UpdatedBy: operator}
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "game_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "delivery_url", "updated_by", "updated_at"}),
	}).Create(app).Error
	if err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "game app saved", "game_id", gameID, "operator", operator)
	return app, nil
}

// TestDelivery 向游戏发货地址发送签名的探测请求，只返回状态码与耗时。
func (s *Service) TestDelivery(ctx context.Context, gameID string) (int, time.Duration, error) {
	if !gameIDPattern.MatchString(gameID) {
		return 0, 0, ErrInvalidArgument
	}
	var app GameApp
	if err := s.db.WithContext(ctx).Where("game_id = ?", gameID).Take(&app).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, ErrGameNotFound
		}
		return 0, 0, err
	}
	if app.DeliveryURL == "" {
		return 0, 0, ErrInvalidDeliveryURL
	}
	if _, err := s.guard.ValidateURL(ctx, app.DeliveryURL); err != nil {
		return 0, 0, ErrInvalidDeliveryURL
	}
	start := s.now()
	code, err := s.notifier.Ping(ctx, app.DeliveryURL)
	latency := s.now().Sub(start)
	if errors.Is(err, netguard.ErrBlocked) {
		return 0, 0, ErrInvalidDeliveryURL
	}
	if err != nil {
		// 不可达统一返回状态码 0：超时、拒绝连接、TLS 失败等细节只记日志，
		// 否则后台接口可被当作内网端口探测的“回显”
		s.logger.WarnContext(ctx, "test delivery failed", "game_id", gameID, "err", err)
		return 0, latency, nil
	}
	return code, latency, nil
}

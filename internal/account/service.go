// Package account 账户中心：注册、登录、资料与角色。
package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"gamepub/internal/rbac"
	"gamepub/pkg/auth"
	"gamepub/pkg/cryptox"
	"gamepub/pkg/dbx"
	"gamepub/pkg/mask"
)

var (
	ErrInvalidPhone       = errors.New("account: invalid phone number")
	ErrWeakPassword       = errors.New("account: password must be 8-72 bytes and contain letters and digits")
	ErrInvalidNickname    = errors.New("account: invalid nickname")
	ErrInvalidAvatar      = errors.New("account: invalid avatar key")
	ErrInvalidRole        = errors.New("account: invalid role")
	ErrPhoneTaken         = errors.New("account: phone already registered")
	ErrInvalidCredentials = errors.New("account: invalid phone or password")
	ErrUserNotFound       = errors.New("account: user not found")
)

var (
	phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)
	phoneAAD     = []byte("account_user.phone")
)

// Options 服务依赖。
type Options struct {
	DB          *gorm.DB
	Redis       redis.UniversalClient
	Issuer      *auth.Issuer
	Sealer      *cryptox.Sealer
	PhonePepper []byte
	BcryptCost  int
	Logger      *slog.Logger
}

// Service 账户领域服务。
type Service struct {
	db        *gorm.DB
	cache     *profileCache
	issuer    *auth.Issuer
	sealer    *cryptox.Sealer
	pepper    []byte
	cost      int
	dummyHash []byte
	logger    *slog.Logger
}

// NewService 创建服务。
func NewService(o Options) (*Service, error) {
	if len(o.PhonePepper) < 16 {
		return nil, errors.New("account: phone pepper too short")
	}
	cost := o.BcryptCost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	// 用户不存在时也做一次等价的 bcrypt 比较，使响应时间一致，防止通过耗时枚举已注册手机号
	dummy, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer-not-a-real-password"), cost)
	if err != nil {
		return nil, err
	}
	return &Service{
		db: o.DB, cache: newProfileCache(o.Redis, o.Logger), issuer: o.Issuer, sealer: o.Sealer,
		pepper: o.PhonePepper, cost: cost, dummyHash: dummy, logger: o.Logger,
	}, nil
}

func validatePassword(pw string) error {
	// bcrypt 只取前 72 字节，超长口令会被静默截断，必须显式拒绝
	if len(pw) < 8 || len(pw) > 72 {
		return ErrWeakPassword
	}
	var letter, digit bool
	for _, r := range pw {
		letter = letter || unicode.IsLetter(r)
		digit = digit || unicode.IsDigit(r)
	}
	if !letter || !digit {
		return ErrWeakPassword
	}
	return nil
}

func validateNickname(n string) error {
	if n == "" || utf8.RuneCountInString(n) > 20 || !utf8.ValidString(n) {
		return ErrInvalidNickname
	}
	for _, r := range n {
		// 拒绝控制字符与 HTML 元字符：昵称会出现在运营后台、游戏内，属于存储型 XSS 的高发输入点
		if unicode.IsControl(r) || strings.ContainsRune(`<>"'&`, r) {
			return ErrInvalidNickname
		}
	}
	return nil
}

// Register 注册玩家账户。
func (s *Service) Register(ctx context.Context, phone, password, nickname string) (uint64, error) {
	if !phonePattern.MatchString(phone) {
		return 0, ErrInvalidPhone
	}
	if err := validatePassword(password); err != nil {
		return 0, err
	}
	if nickname == "" {
		nickname = "玩家" + phone[len(phone)-4:]
	}
	if err := validateNickname(nickname); err != nil {
		return 0, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost)
	if err != nil {
		return 0, fmt.Errorf("account: hash password: %w", err)
	}
	enc, err := s.sealer.Seal([]byte(phone), phoneAAD)
	if err != nil {
		return 0, err
	}
	u := User{
		PhoneHash: cryptox.BlindIndex(s.pepper, phone), PhoneEnc: enc, PhoneMasked: mask.Phone(phone),
		PasswordHash: string(hash), Nickname: nickname, Roles: rbac.RolePlayer, Status: StatusActive,
	}
	if err := s.db.WithContext(ctx).Create(&u).Error; err != nil {
		if dbx.IsDuplicate(err) {
			return 0, ErrPhoneTaken
		}
		return 0, err
	}
	return u.ID, nil
}

// Login 校验口令并签发令牌。手机号不存在、口令错误、账号停用统一返回 ErrInvalidCredentials。
func (s *Service) Login(ctx context.Context, phone, password string) (string, time.Time, error) {
	if !phonePattern.MatchString(phone) || len(password) > 72 {
		return "", time.Time{}, ErrInvalidCredentials
	}
	var u User
	err := s.db.WithContext(ctx).Where("phone_hash = ?", cryptox.BlindIndex(s.pepper, phone)).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil || u.Status != StatusActive {
		s.logger.InfoContext(ctx, "login failed", "user_id", u.ID)
		return "", time.Time{}, ErrInvalidCredentials
	}
	token, exp, err := s.issuer.Issue(u.ID, u.RoleList())
	if err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// GetProfile 查询资料（走缓存）。
func (s *Service) GetProfile(ctx context.Context, id uint64) (*Profile, error) {
	if id == 0 {
		return nil, ErrUserNotFound
	}
	return s.cache.get(ctx, id, func(ctx context.Context) (*Profile, error) {
		var u User
		err := s.db.WithContext(ctx).Take(&u, id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		if err != nil {
			return nil, err
		}
		return u.profile(), nil
	})
}

// UpdateProfile 更新昵称/头像。avatarKey 必须是该用户自己上传目录下的对象，防止引用他人或任意路径的文件。
func (s *Service) UpdateProfile(ctx context.Context, id uint64, nickname, avatarKey string) (*Profile, error) {
	updates := map[string]any{}
	if nickname != "" {
		if err := validateNickname(nickname); err != nil {
			return nil, err
		}
		updates["nickname"] = nickname
	}
	if avatarKey != "" {
		if !strings.HasPrefix(avatarKey, fmt.Sprintf("avatars/%d/", id)) || strings.Contains(avatarKey, "..") {
			return nil, ErrInvalidAvatar
		}
		updates["avatar_key"] = avatarKey
	}
	if len(updates) > 0 {
		r := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", id).Updates(updates)
		if r.Error != nil {
			return nil, r.Error
		}
		if r.RowsAffected == 0 {
			return nil, ErrUserNotFound
		}
		s.cache.invalidate(ctx, id)
	}
	return s.GetProfile(ctx, id)
}

// GrantRole 授予后台角色。仅供运维命令行使用，不暴露为任何网络接口。
func (s *Service) GrantRole(ctx context.Context, phone, role string) error {
	if !rbac.ValidRole(role) {
		return ErrInvalidRole
	}
	var u User
	if err := s.db.WithContext(ctx).Where("phone_hash = ?", cryptox.BlindIndex(s.pepper, phone)).Take(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrUserNotFound
		}
		return err
	}
	roles := u.RoleList()
	if slices.Contains(roles, role) {
		return nil
	}
	roles = append(roles, role)
	if err := s.db.WithContext(ctx).Model(&User{}).Where("id = ?", u.ID).Update("roles", strings.Join(roles, ",")).Error; err != nil {
		return err
	}
	s.cache.invalidate(ctx, u.ID)
	s.logger.InfoContext(ctx, "role granted", "user_id", u.ID, "role", role)
	return nil
}

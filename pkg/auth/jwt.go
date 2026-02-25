// Package auth JWT 签发与校验。
//
// 采用 Ed25519（EdDSA）非对称签名：只有账户服务持有私钥，网关只持有公钥，
// 网关被攻破也无法伪造令牌；校验时强制指定算法，杜绝 alg=none / 算法混淆攻击。
package auth

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gamepub/pkg/cryptox"
)

// ErrInvalidToken 令牌无效（签名错误、过期、签发方或受众不符等），对外统一返回此错误。
var ErrInvalidToken = errors.New("auth: invalid token")

// Claims 令牌载荷。角色仅作展示参考，后台权限判定以账户服务实时数据为准。
type Claims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles,omitempty"`
}

// UserID 解析 sub 中的用户 ID。
func (c *Claims) UserID() (uint64, error) {
	id, err := strconv.ParseUint(c.Subject, 10, 64)
	if err != nil || id == 0 {
		return 0, ErrInvalidToken
	}
	return id, nil
}

// Issuer 令牌签发方（仅账户服务持有）。
type Issuer struct {
	key      ed25519.PrivateKey
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time
}

// NewIssuer seed 为 32 字节 Ed25519 种子。
func NewIssuer(seed []byte, issuer, audience string, ttl time.Duration) (*Issuer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("auth: ed25519 seed must be %d bytes", ed25519.SeedSize)
	}
	return &Issuer{key: ed25519.NewKeyFromSeed(seed), issuer: issuer, audience: audience, ttl: ttl, now: time.Now}, nil
}

// PublicKey 返回对应公钥，供网关配置。
func (i *Issuer) PublicKey() ed25519.PublicKey {
	pub, _ := i.key.Public().(ed25519.PublicKey)
	return pub
}

// Issue 签发访问令牌。
func (i *Issuer) Issue(userID uint64, roles []string) (string, time.Time, error) {
	now := i.now()
	exp := now.Add(i.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   strconv.FormatUint(userID, 10),
			Audience:  jwt.ClaimStrings{i.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        cryptox.RandomHex(16),
		},
		Roles: roles,
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(i.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign: %w", err)
	}
	return token, exp, nil
}

// Verifier 令牌校验方（网关）。
type Verifier struct {
	key    ed25519.PublicKey
	parser *jwt.Parser
}

// NewVerifier 创建校验器。
func NewVerifier(pub ed25519.PublicKey, issuer, audience string) (*Verifier, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("auth: ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	return &Verifier{
		key: pub,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
			jwt.WithIssuer(issuer),
			jwt.WithAudience(audience),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithLeeway(30*time.Second),
		),
	}, nil
}

// Verify 校验令牌并返回载荷；任何失败都折叠为 ErrInvalidToken，不向调用方暴露细节。
func (v *Verifier) Verify(raw string) (*Claims, error) {
	claims := &Claims{}
	tok, err := v.parser.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return v.key, nil })
	if err != nil || !tok.Valid {
		return nil, ErrInvalidToken
	}
	if _, err := claims.UserID(); err != nil {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

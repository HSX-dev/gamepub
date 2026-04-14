package account

import (
	"strings"
	"time"
)

// 用户状态。
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// User 用户表。手机号不存明文：phone_hash 为带 pepper 的盲索引（唯一约束 + 登录查询），
// phone_enc 为 AES-GCM 密文（仅在需要时解密），phone_masked 用于展示。
type User struct {
	ID           uint64 `gorm:"primaryKey"`
	PhoneHash    string `gorm:"size:64;not null;uniqueIndex:uk_phone_hash"`
	PhoneEnc     string `gorm:"size:255;not null"`
	PhoneMasked  string `gorm:"size:20;not null"`
	PasswordHash string `gorm:"size:100;not null"`
	Nickname     string `gorm:"size:64;not null"`
	AvatarKey    string `gorm:"size:128"`
	Roles        string `gorm:"size:128;not null"` // 逗号分隔
	Status       string `gorm:"size:16;not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TableName 表名。
func (User) TableName() string { return "account_user" }

// RoleList 角色列表。
func (u *User) RoleList() []string {
	var out []string
	for _, r := range strings.Split(u.Roles, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// Profile 对外/缓存使用的用户视图：不含口令哈希与手机号密文。
type Profile struct {
	ID          uint64    `json:"id"`
	PhoneMasked string    `json:"phone_masked"`
	Nickname    string    `json:"nickname"`
	AvatarKey   string    `json:"avatar_key"`
	Roles       []string  `json:"roles"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

func (u *User) profile() *Profile {
	return &Profile{
		ID: u.ID, PhoneMasked: u.PhoneMasked, Nickname: u.Nickname, AvatarKey: u.AvatarKey,
		Roles: u.RoleList(), Status: u.Status, CreatedAt: u.CreatedAt,
	}
}

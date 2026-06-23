package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	accountv1 "gamepub/api/gen/account/v1"
	"gamepub/pkg/upload"
)

type registerReq struct {
	Phone    string `json:"phone" binding:"required,max=20"`
	Password string `json:"password" binding:"required,max=128"`
	Nickname string `json:"nickname" binding:"max=64"`
}

func (h *handlers) register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body")
		return
	}
	resp, err := h.Account.Register(c.Request.Context(), &accountv1.RegisterRequest{
		Phone: req.Phone, Password: req.Password, Nickname: req.Nickname,
	})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"user_id": resp.GetUserId()})
}

type loginReq struct {
	Phone    string `json:"phone" binding:"required,max=20"`
	Password string `json:"password" binding:"required,max=128"`
}

func (h *handlers) login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body")
		return
	}
	// 按 IP 之外再按手机号限流，防止分布式代理池对单个账号撞库。
	// 限流 key 使用手机号摘要，避免在 Redis 中留下明文手机号。
	sum := sha256.Sum256([]byte(req.Phone))
	if d, err := h.Limiters.Login.Allow(c.Request.Context(), "login-phone|"+hex.EncodeToString(sum[:8])); err == nil && !d.Allowed {
		fail(c, http.StatusTooManyRequests, "rate_limited", "too many login attempts")
		return
	}
	resp, err := h.Account.Login(c.Request.Context(), &accountv1.LoginRequest{Phone: req.Phone, Password: req.Password})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"access_token": resp.GetAccessToken(), "expires_at": resp.GetExpiresAt()})
}

// userDTO 显式列出返回字段，避免把内部字段（角色、状态等）原样暴露给玩家。
type userDTO struct {
	UserID      uint64 `json:"user_id"`
	PhoneMasked string `json:"phone_masked"`
	Nickname    string `json:"nickname"`
	AvatarKey   string `json:"avatar_key,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

func toUserDTO(u *accountv1.User) userDTO {
	return userDTO{UserID: u.GetUserId(), PhoneMasked: u.GetPhoneMasked(), Nickname: u.GetNickname(), AvatarKey: u.GetAvatarKey(), CreatedAt: u.GetCreatedAt()}
}

func (h *handlers) me(c *gin.Context) {
	resp, err := h.Account.GetUser(c.Request.Context(), &accountv1.GetUserRequest{UserId: userID(c)})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, toUserDTO(resp.GetUser()))
}

// updateMeReq 只声明允许修改的字段：请求体里额外携带的 avatar_key / roles / user_id 会被忽略（防批量赋值）。
type updateMeReq struct {
	Nickname string `json:"nickname" binding:"required,max=64"`
}

func (h *handlers) updateMe(c *gin.Context) {
	var req updateMeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "invalid request body")
		return
	}
	resp, err := h.Account.UpdateProfile(c.Request.Context(), &accountv1.UpdateProfileRequest{UserId: userID(c), Nickname: req.Nickname})
	if err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, toUserDTO(resp.GetUser()))
}

func (h *handlers) uploadAvatar(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(c, http.StatusRequestEntityTooLarge, "too_large", "file too large")
			return
		}
		badRequest(c, "missing file")
		return
	}
	f, err := fh.Open()
	if err != nil {
		badRequest(c, "invalid file")
		return
	}
	defer func() { _ = f.Close() }() // 只读的上传临时文件，关闭错误无可处理

	img, err := upload.AvatarPolicy().Inspect(f)
	switch {
	case errors.Is(err, upload.ErrTooLarge):
		fail(c, http.StatusRequestEntityTooLarge, "too_large", "file too large")
		return
	case err != nil:
		badRequest(c, "only png/jpeg images up to 2048x2048 are allowed")
		return
	}
	uid := userID(c)
	key := upload.NewKey(fmt.Sprintf("avatars/%d", uid), img.Ext)
	if err := h.Storage.Put(c.Request.Context(), key, img.Data, img.ContentType); err != nil {
		h.Logger.ErrorContext(c.Request.Context(), "avatar store failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if _, err := h.Account.UpdateProfile(c.Request.Context(), &accountv1.UpdateProfileRequest{UserId: uid, AvatarKey: key}); err != nil {
		grpcFail(c, err)
		return
	}
	ok(c, gin.H{"avatar_key": key})
}

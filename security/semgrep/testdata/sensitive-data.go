package testdata

import (
	"crypto/md5"
	"crypto/sha256"
	"log/slog"

	"github.com/gin-gonic/gin"
)

type loginReq struct {
	Phone    string
	Password string
}

func logging(logger *slog.Logger, c *gin.Context, req loginReq) {
	// ruleid: log-sensitive-value
	logger.Info("login attempt", "phone", req.Phone)
	// ruleid: log-sensitive-value
	logger.Warn("login failed", "password", req.Password)
	// ruleid: log-sensitive-value
	logger.Info("request", "auth", c.GetHeader("Authorization"))
	// ok: log-sensitive-value
	logger.Info("login failed", "user_id", 42)
}

// ruleid: hardcoded-credential
const PaySecret = "mockpay-0123456789abcdef"

// ok: hardcoded-credential
const MDInternalToken = "x-internal-token"

type cfg struct{ DSN string }

func config() {
	// ruleid: hardcoded-credential
	c := cfg{DSN: "root:Passw0rd@tcp(10.0.0.5:3306)/gamepub"}
	// ruleid: hardcoded-credential
	apiKey := "sk-test-9f8e7d6c5b4a"
	_, _ = c, apiKey
}

func hashing(req loginReq) {
	// ruleid: password-fast-hash
	_ = md5.Sum([]byte(req.Password))
	// ruleid: password-fast-hash
	_ = sha256.Sum256([]byte(req.Password))
	// ok: password-fast-hash
	_ = sha256.Sum256([]byte(req.Phone))
}

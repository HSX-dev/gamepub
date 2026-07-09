package testdata

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type guard interface {
	ValidateURL(ctx context.Context, raw string) (string, error)
}

func fetchAvatar(c *gin.Context) {
	target := c.Query("url")
	// ruleid: ssrf-unvalidated-outbound-url, http-client-without-timeout
	resp, _ := http.Get(target)
	_ = resp
}

func saveWebhook(ctx context.Context, req interface{ GetDeliveryUrl() string }) {
	// ruleid: ssrf-unvalidated-outbound-url
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, req.GetDeliveryUrl(), nil)
	_ = r
}

func saveWebhookSafely(ctx context.Context, g guard, req interface{ GetDeliveryUrl() string }) {
	u, err := g.ValidateURL(ctx, req.GetDeliveryUrl())
	if err != nil {
		return
	}
	// ok: ssrf-unvalidated-outbound-url
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	_ = r
}

func clients() {
	// ruleid: http-client-without-timeout
	c1 := &http.Client{}
	// ok: http-client-without-timeout
	c2 := &http.Client{Timeout: 5 * time.Second}
	// ruleid: tls-insecure-skip-verify
	cfg := &tls.Config{InsecureSkipVerify: true}
	// ok: tls-insecure-skip-verify
	cfg2 := &tls.Config{MinVersion: tls.VersionTLS12}
	_, _, _, _ = c1, c2, cfg, cfg2
}

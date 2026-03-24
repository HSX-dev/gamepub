package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false, // 云厂商元数据
		"100.100.100.200":      false, // 阿里云元数据（CGNAT 段）
		"0.0.0.0":              false,
		"::1":                  false,
		"::ffff:127.0.0.1":     false,
		"fd00::1":              false,
		"fe80::1":              false,
		"64:ff9b::7f00:1":      false,
		"2002:7f00:1::":        false,
	}
	for ip, want := range cases {
		if got := IsPublic(netip.MustParseAddr(ip)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	g := Default()
	g.LookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "game.example.com":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		case "internal.example.com":
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		case "mixed.example.com":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, errors.New("nxdomain")
	}

	ok := []string{
		"https://game.example.com/delivery",
		"http://8.8.8.8/x",
		"https://1.1.1.1:443/",
	}
	bad := []string{
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://169.254.169.254/latest/meta-data/",
		"https://internal.example.com/",
		"https://mixed.example.com/",
		"https://nxdomain.example.com/",
		"http://8.8.8.8:6379/",
		"ftp://8.8.8.8/",
		"file:///etc/passwd",
		"gopher://8.8.8.8:70/_",
		"http://user:pass@8.8.8.8/",
		"http:///nohost",
	}
	for _, u := range ok {
		if _, err := g.ValidateURL(context.Background(), u); err != nil {
			t.Errorf("ValidateURL(%q) unexpected error: %v", u, err)
		}
	}
	for _, u := range bad {
		if _, err := g.ValidateURL(context.Background(), u); !errors.Is(err, ErrBlocked) {
			t.Errorf("ValidateURL(%q) = %v, want ErrBlocked", u, err)
		}
	}
}

func TestHTTPClientBlocksLoopbackAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, err := Default().HTTPClient(2 * time.Second).Get(srv.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected ErrBlocked when dialing loopback, got %v", err)
	}
}

func TestRedirectToInternalBlocked(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret metadata"))
	}))
	defer internal.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer public.Close()

	// 模拟：只有 public 服务器被视为“公网”，internal 视为内网
	pu, _ := url.Parse(public.URL)
	allowed := netip.MustParseAddrPort(pu.Host)
	g := Default()
	g.AllowAddr = func(ap netip.AddrPort) bool { return ap == allowed }

	_, err := g.HTTPClient(2 * time.Second).Get(public.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to internal address must be blocked, got %v", err)
	}
}

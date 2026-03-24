// Package netguard 防御 SSRF：服务端代用户/运营发起的出站请求（Webhook、URL 抓取）统一走这里。
//
// 两道防线：
//  1. ValidateURL：保存配置时校验协议、凭据、端口与解析结果，给运营即时反馈；
//  2. HTTPClient：真正连接时在 Dialer.Control 中校验最终 IP。
//
// 只做第 1 步是常见审计问题——DNS Rebinding（校验时解析到公网、请求时解析到内网）
// 与 302 重定向到内网都能绕过；第 2 步在 TCP 建连前拦截，二者都无法绕过。
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked 目标地址不被允许。
var ErrBlocked = errors.New("netguard: destination not allowed")

// blockedPrefixes netip 内置判定之外额外拦截的保留网段。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT，云厂商内部常用
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64，可映射到内网 IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"), // 本地 NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4，可内嵌私网 IPv4
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("100::/64"),
}

// IsPublic 判断地址是否为可访问的公网单播地址。IPv4-mapped IPv6（::ffff:127.0.0.1）会先还原再判定。
func IsPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Guard 出站访问策略。
type Guard struct {
	// Schemes 允许的协议，默认仅 http/https（拒绝 file/gopher/dict 等）
	Schemes map[string]bool
	// Ports 允许的端口，默认仅 80/443（拒绝探测 6379/3306 等内部服务端口）
	Ports map[uint16]bool
	// AllowAddr 自定义地址判定，测试时注入；为空时使用 IsPublic + Ports
	AllowAddr func(netip.AddrPort) bool
	// LookupIP 自定义解析，测试时注入；为空时使用系统解析
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
}

// Default 生产默认策略。
func Default() *Guard {
	return &Guard{
		Schemes: map[string]bool{"http": true, "https": true},
		Ports:   map[uint16]bool{80: true, 443: true},
	}
}

func (g *Guard) allow(ap netip.AddrPort) bool {
	if g.AllowAddr != nil {
		return g.AllowAddr(ap)
	}
	return IsPublic(ap.Addr()) && g.Ports[ap.Port()]
}

func (g *Guard) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if g.LookupIP != nil {
		return g.LookupIP(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func blocked(reason string) error { return fmt.Errorf("%w: %s", ErrBlocked, reason) }

// ValidateURL 前置校验：协议、无凭据、端口白名单、解析出的所有 IP 均为公网。
func (g *Guard) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	if len(raw) > 2048 {
		return nil, blocked("url too long")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, blocked("malformed url")
	}
	scheme := strings.ToLower(u.Scheme)
	if !g.Schemes[scheme] {
		return nil, blocked("scheme not allowed")
	}
	if u.User != nil {
		return nil, blocked("credentials in url")
	}
	host := u.Hostname()
	if host == "" {
		return nil, blocked("empty host")
	}
	port := uint16(443)
	if scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return nil, blocked("invalid port")
		}
		port = uint16(n)
	}

	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if addrs, err = g.lookup(lctx, host); err != nil {
			return nil, blocked("dns lookup failed")
		}
	}
	if len(addrs) == 0 {
		return nil, blocked("no address")
	}
	// 任一解析结果不合规即拒绝：攻击者可让域名同时解析到公网与内网地址
	for _, a := range addrs {
		if !g.allow(netip.AddrPortFrom(a.Unmap(), port)) {
			return nil, blocked("address not public or port not allowed")
		}
	}
	return u, nil
}

// Dialer 返回在建连前校验最终 IP 的 Dialer。
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if network != "tcp4" && network != "tcp6" {
				return blocked("network not allowed")
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return blocked("unparseable address")
			}
			if !g.allow(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())) {
				return blocked("address not public or port not allowed")
			}
			return nil
		},
	}
}

// HTTPClient 返回受保护的 HTTP 客户端：不走环境代理、限制重定向、强制总超时。
func (g *Guard) HTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		// 显式禁用代理：走 HTTP(S)_PROXY 时连接的是代理而非目标，Dialer 校验会失效
		Proxy:                 nil,
		DialContext:           g.Dialer(3 * time.Second).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return blocked("too many redirects")
			}
			if !g.Schemes[req.URL.Scheme] {
				return blocked("redirect scheme not allowed")
			}
			// 重定向目标的 IP 由 Dialer 在建连时校验
			return nil
		},
	}
}

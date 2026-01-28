// Package config 从环境变量读取配置。
//
// 密钥类配置（DSN、签名密钥、JWT 私钥等）只能通过 K8s Secret 注入环境变量，
// 缺失或强度不足时直接启动失败——宁可起不来，也不带着“默认密钥”上线。
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// String 读取普通配置，未设置时返回默认值。
func String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// Int 读取整数配置，格式错误时返回默认值。
func Int(key string, def int) int {
	if n, err := strconv.Atoi(String(key, "")); err == nil {
		return n
	}
	return def
}

// Bool 读取布尔配置。
func Bool(key string, def bool) bool {
	if b, err := strconv.ParseBool(String(key, "")); err == nil {
		return b
	}
	return def
}

// Duration 读取时长配置，如 "3s"、"500ms"。
func Duration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(String(key, "")); err == nil && d > 0 {
		return d
	}
	return def
}

// List 读取逗号分隔的列表配置。
func List(key string, def []string) []string {
	v := String(key, "")
	if v == "" {
		return def
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Secret 读取必填密钥，长度不足 minLen 视为配置错误。错误信息只包含变量名，不包含值。
func Secret(key string, minLen int) (string, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", fmt.Errorf("config: required secret %s is not set", key)
	}
	if len(v) < minLen {
		return "", fmt.Errorf("config: secret %s is too short (min %d)", key, minLen)
	}
	return v, nil
}

// SecretBytes 读取 base64 编码的二进制密钥（如 AES key、Ed25519 seed），并校验精确长度。
func SecretBytes(key string, size int) ([]byte, error) {
	v, err := Secret(key, 1)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("config: secret %s is not valid base64", key)
	}
	if len(b) != size {
		return nil, fmt.Errorf("config: secret %s must decode to %d bytes", key, size)
	}
	return b, nil
}

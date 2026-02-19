// Package cryptox 敏感字段加密存储（AES-256-GCM）与盲索引（HMAC-SHA256）。
//
// 手机号等字段：密文用于必要时解密（如发短信），盲索引用于唯一约束与登录查询，
// 数据库泄露时攻击者既拿不到明文，也无法用彩虹表反推（盲索引带 pepper）。
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrDecrypt 解密失败（密文被篡改或密钥不匹配）。
var ErrDecrypt = errors.New("cryptox: decrypt failed")

// Sealer AES-256-GCM 加解密器。
type Sealer struct{ aead cipher.AEAD }

// NewSealer key 必须为 32 字节。
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cryptox: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal 加密明文，aad 绑定字段语义（如 "user.phone"），防止密文被挪用到其它字段。
func (s *Sealer) Seal(plaintext, aad []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plaintext, aad)
	return base64.RawStdEncoding.EncodeToString(out), nil
}

// Open 解密。
func (s *Sealer) Open(enc string, aad []byte) ([]byte, error) {
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return nil, ErrDecrypt
	}
	ns := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, raw[:ns], raw[ns:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// BlindIndex 计算带 pepper 的 HMAC-SHA256 盲索引。
func BlindIndex(pepper []byte, value string) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

// RandomHex 生成 n 字节密码学安全随机数的十六进制串，用于 token、nonce、文件名。
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败意味着系统熵源不可用，继续运行会产生可预测值，直接崩溃
		panic(fmt.Sprintf("cryptox: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

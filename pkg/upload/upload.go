// Package upload 文件上传安全校验与存储。
//
// 审计常见问题：信任客户端文件名后缀/Content-Type、无大小限制、使用用户文件名落盘（路径穿越）、
// 存储目录可被 Web 服务器直接解析执行、图片头伪造的多语言文件（polyglot）、像素炸弹。
package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gamepub/pkg/cryptox"
)

var (
	ErrTooLarge   = errors.New("upload: file too large")
	ErrType       = errors.New("upload: file type not allowed")
	ErrDimensions = errors.New("upload: image dimensions not allowed")
	ErrCorrupt    = errors.New("upload: file is corrupt")
	ErrBadKey     = errors.New("upload: invalid storage key")
)

var allowedImages = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
}

// ImagePolicy 图片上传策略。
type ImagePolicy struct {
	MaxBytes  int64
	MaxWidth  int
	MaxHeight int
}

// AvatarPolicy 头像默认策略。
func AvatarPolicy() ImagePolicy {
	return ImagePolicy{MaxBytes: 2 << 20, MaxWidth: 2048, MaxHeight: 2048}
}

// Image 校验并重新编码后的图片。
type Image struct {
	ContentType string
	Ext         string
	Width       int
	Height      int
	Data        []byte
}

// Inspect 读取并校验图片：
//  1. 按上限读取，超限拒绝；
//  2. 只依据文件头魔数判断类型，忽略客户端声明的文件名与 Content-Type；
//  3. 先读尺寸再解码，拒绝像素炸弹；
//  4. 完整解码后重新编码，剥离 EXIF（含 GPS 等隐私）与附加在图片后的任意载荷。
func (p ImagePolicy) Inspect(r io.Reader) (*Image, error) {
	data, err := io.ReadAll(io.LimitReader(r, p.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("upload: read: %w", err)
	}
	if int64(len(data)) > p.MaxBytes {
		return nil, ErrTooLarge
	}
	if len(data) == 0 {
		return nil, ErrCorrupt
	}
	ct := http.DetectContentType(data)
	ext, ok := allowedImages[ct]
	if !ok {
		return nil, ErrType
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	if "image/"+format != ct {
		return nil, ErrType
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > p.MaxWidth || cfg.Height > p.MaxHeight {
		return nil, ErrDimensions
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	var out bytes.Buffer
	switch ct {
	case "image/png":
		err = png.Encode(&out, img)
	default:
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		return nil, fmt.Errorf("upload: re-encode: %w", err)
	}
	return &Image{ContentType: ct, Ext: ext, Width: cfg.Width, Height: cfg.Height, Data: out.Bytes()}, nil
}

// Storage 对象存储抽象（生产对接 OSS/S3，私有桶 + 签名 URL 访问）。
type Storage interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
}

var keyPattern = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9_-]+)*\.(png|jpg)$`)

// NewKey 生成服务端随机文件名，绝不使用用户上传的文件名。
func NewKey(prefix, ext string) string {
	return strings.Trim(prefix, "/") + "/" + cryptox.RandomHex(16) + ext
}

// ValidKey 校验存储 key 格式。
func ValidKey(key string) bool { return keyPattern.MatchString(key) && !strings.Contains(key, "..") }

// LocalStorage 本地磁盘存储（开发环境）。目录不应位于任何 Web 服务器可执行/可直接访问的路径下。
type LocalStorage struct{ Root string }

// Put 写入文件，权限 0600（仅服务进程可读写）。
func (s LocalStorage) Put(_ context.Context, key string, data []byte, _ string) error {
	if !ValidKey(key) {
		return ErrBadKey
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return err
	}
	p := filepath.Join(root, filepath.FromSlash(key))
	if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
		return ErrBadKey
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

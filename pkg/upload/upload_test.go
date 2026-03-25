package upload

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngBomb 构造只有 IHDR 声明超大尺寸的 PNG（像素炸弹）。
func pngBomb(t *testing.T) []byte {
	t.Helper()
	data := pngBytes(t, 1, 1)
	// PNG 签名 8 字节 + 长度 4 字节 + "IHDR" 4 字节后是宽高
	binary.BigEndian.PutUint32(data[16:20], 100000)
	binary.BigEndian.PutUint32(data[20:24], 100000)
	crc := crc32.ChecksumIEEE(data[12:29])
	binary.BigEndian.PutUint32(data[29:33], crc)
	return data
}

func TestInspect(t *testing.T) {
	p := AvatarPolicy()

	img, err := p.Inspect(bytes.NewReader(pngBytes(t, 16, 16)))
	if err != nil || img.Ext != ".png" || img.Width != 16 {
		t.Fatalf("valid png rejected: %+v %v", img, err)
	}

	withPayload := append(pngBytes(t, 4, 4), []byte("<?php system($_GET['c']); ?>")...)
	img, err = p.Inspect(bytes.NewReader(withPayload))
	if err != nil {
		t.Fatalf("png with trailing payload should decode: %v", err)
	}
	if bytes.Contains(img.Data, []byte("<?php")) {
		t.Fatal("re-encoding must strip trailing payload")
	}

	cases := map[string]struct {
		data []byte
		want error
	}{
		"html disguised as png": {[]byte("<html><script>alert(1)</script></html>"), ErrType},
		"svg with script":       {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), ErrType},
		"truncated png":         {pngBytes(t, 8, 8)[:40], ErrCorrupt},
		"pixel bomb":            {pngBomb(t), ErrDimensions},
		"too large":             {bytes.Repeat([]byte{0}, int(p.MaxBytes)+1), ErrTooLarge},
		"empty":                 {nil, ErrCorrupt},
	}
	for name, c := range cases {
		if _, err := p.Inspect(bytes.NewReader(c.data)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

func TestLocalStorageRejectsTraversal(t *testing.T) {
	s := LocalStorage{Root: t.TempDir()}
	for _, key := range []string{"../../etc/cron.d/x.png", "avatars/../../x.png", "/abs/x.png", "avatars/x.php", `avatars\..\x.png`} {
		if err := s.Put(context.Background(), key, []byte("x"), "image/png"); !errors.Is(err, ErrBadKey) {
			t.Errorf("Put(%q) = %v, want ErrBadKey", key, err)
		}
	}
	key := NewKey("avatars/42", ".png")
	if err := s.Put(context.Background(), key, []byte("x"), "image/png"); err != nil {
		t.Fatalf("Put(%q) failed: %v", key, err)
	}
}

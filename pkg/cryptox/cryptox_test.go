package cryptox

import (
	"bytes"
	"errors"
	"testing"
)

func TestSealOpen(t *testing.T) {
	s, err := NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.Seal([]byte("13812345678"), []byte("user.phone"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := s.Open(enc, []byte("user.phone"))
	if err != nil || string(pt) != "13812345678" {
		t.Fatalf("Open() = %q, %v", pt, err)
	}
	// 密文挪用到其它字段（AAD 不同）必须解密失败
	if _, err := s.Open(enc, []byte("user.id_card")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("expected ErrDecrypt for wrong aad, got %v", err)
	}
}

func TestBlindIndexDeterministic(t *testing.T) {
	a := BlindIndex([]byte("pepper"), "13812345678")
	b := BlindIndex([]byte("pepper"), "13812345678")
	c := BlindIndex([]byte("other"), "13812345678")
	if a != b || a == c {
		t.Fatalf("blind index should be deterministic per pepper: %s %s %s", a, b, c)
	}
}

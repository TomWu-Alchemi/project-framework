package util

import (
	"crypto/hmac"
	"encoding/hex"
	"hash"
	"io"
)

func CalcAndCompareHmac(h func() hash.Hash, secretKey string, msg string, compare string) bool {
	w := hmac.New(h, []byte(secretKey))
	_, _ = io.WriteString(w, msg)
	sum := w.Sum(nil)
	decode, err := hex.DecodeString(compare)
	if err != nil {
		return false
	}
	return hmac.Equal(sum, decode)
}

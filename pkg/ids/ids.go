package ids

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func New(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	if prefix == "" {
		prefix = "id"
	}
	return prefix + "_" + time.Now().UTC().Format("20060102T150405.000000000") + "_" + hex.EncodeToString(b[:])
}

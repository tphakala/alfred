package server

import (
	"encoding/base64"
	"strconv"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

func parsePageSize(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultPageSize
	}
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}

// encodePageToken encodes an integer offset as an opaque cursor.
func encodePageToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodePageToken(tok string) (int, bool) {
	if tok == "" {
		return 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

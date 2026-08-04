package server

import "testing"

func TestPaginationRoundTrip(t *testing.T) {
	size := parsePageSize("0") // invalid -> default
	if size != defaultPageSize {
		t.Fatalf("size = %d", size)
	}
	if parsePageSize("250") != maxPageSize {
		t.Fatal("over-max should clamp")
	}
	tok := encodePageToken(42)
	off, ok := decodePageToken(tok)
	if !ok || off != 42 {
		t.Fatalf("decode = %d ok=%v", off, ok)
	}
	if _, ok := decodePageToken("not-base64!!"); ok {
		t.Fatal("garbage token must fail")
	}
}

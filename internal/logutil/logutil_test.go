package logutil

import (
	"bytes"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestTruncateBytes(t *testing.T) {
	tests := []struct {
		name          string
		in            []byte
		wantTruncated bool
	}{
		{name: "nil 输入", in: nil},
		{name: "空输入", in: []byte{}},
		{name: "小于上限", in: bytes.Repeat([]byte("a"), MaxLogBytes-1)},
		{name: "等于上限", in: bytes.Repeat([]byte("b"), MaxLogBytes)},
		{name: "大于上限", in: bytes.Repeat([]byte("c"), MaxLogBytes+10), wantTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logged, truncated, size := TruncateBytes(tt.in)

			if truncated != tt.wantTruncated {
				t.Fatalf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
			if size != len(tt.in) {
				t.Fatalf("size = %d, want %d", size, len(tt.in))
			}
			if !tt.wantTruncated {
				if len(logged) != len(tt.in) {
					t.Fatalf("len(logged) = %d, want %d", len(logged), len(tt.in))
				}
				return
			}
			if len(logged) != MaxLogBytes {
				t.Fatalf("len(logged) = %d, want %d", len(logged), MaxLogBytes)
			}
			if !bytes.Equal(logged, tt.in[:MaxLogBytes]) {
				t.Fatal("logged is not the input prefix")
			}
			// logged must be a clone: mutating the input after the call must
			// not change what was handed to the logger.
			first := logged[0]
			tt.in[0] = 'X'
			if logged[0] != first {
				t.Fatal("logged must not alias the input slice")
			}
		})
	}
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantTruncated bool
	}{
		{name: "空输入", in: ""},
		{name: "小于上限", in: strings.Repeat("a", MaxLogBytes-1)},
		{name: "等于上限", in: strings.Repeat("b", MaxLogBytes)},
		{name: "大于上限", in: strings.Repeat("c", MaxLogBytes+3), wantTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logged, truncated, size := TruncateString(tt.in)

			if truncated != tt.wantTruncated {
				t.Fatalf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
			if size != len(tt.in) {
				t.Fatalf("size = %d, want %d", size, len(tt.in))
			}
			wantLen := len(tt.in)
			if tt.wantTruncated {
				wantLen = MaxLogBytes
			}
			if len(logged) != wantLen {
				t.Fatalf("len(logged) = %d, want %d", len(logged), wantLen)
			}
			if logged != tt.in[:wantLen] {
				t.Fatal("logged is not the input prefix")
			}
		})
	}
}

func TestZapTruncatedBytes(t *testing.T) {
	tests := []struct {
		name          string
		in            []byte
		wantTruncated bool
	}{
		{name: "nil 输入", in: nil},
		{name: "空输入", in: []byte{}},
		{name: "小于上限", in: bytes.Repeat([]byte("a"), MaxLogBytes-1)},
		{name: "等于上限", in: bytes.Repeat([]byte("b"), MaxLogBytes)},
		{name: "大于上限", in: bytes.Repeat([]byte("c"), MaxLogBytes+10), wantTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeFields(t, ZapTruncatedBytes("data", tt.in))
			if len(got) != 3 {
				t.Fatalf("fields = %v, want exactly 3 keys", got)
			}

			wantLogged := tt.in
			if tt.wantTruncated {
				wantLogged = tt.in[:MaxLogBytes]
			}
			// zapcore.MapObjectEncoder.AddByteString 落库为 string
			// (zapcore/memory_encoder.go:63)，故 ByteString 载荷在此表现为 string。
			payload, ok := got["data"].(string)
			if !ok || payload != string(wantLogged) {
				t.Fatalf("data = %v (isString=%v), want %d bytes", got["data"], ok, len(wantLogged))
			}
			if truncated, ok := got["data_truncated"].(bool); !ok || truncated != tt.wantTruncated {
				t.Fatalf("data_truncated = %v, want %v", got["data_truncated"], tt.wantTruncated)
			}
			if size, ok := got["data_size"].(int64); !ok || size != int64(len(tt.in)) {
				t.Fatalf("data_size = %v, want %d", got["data_size"], len(tt.in))
			}
		})
	}
}

func TestZapTruncatedString(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantTruncated bool
	}{
		{name: "空输入", in: ""},
		{name: "小于上限", in: strings.Repeat("a", MaxLogBytes-1)},
		{name: "等于上限", in: strings.Repeat("b", MaxLogBytes)},
		{name: "大于上限", in: strings.Repeat("c", MaxLogBytes+3), wantTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encodeFields(t, ZapTruncatedString("header", tt.in))
			if len(got) != 3 {
				t.Fatalf("fields = %v, want exactly 3 keys", got)
			}

			wantLogged := tt.in
			if tt.wantTruncated {
				wantLogged = tt.in[:MaxLogBytes]
			}
			if payload, ok := got["header"].(string); !ok || payload != wantLogged {
				t.Fatalf("header = %v (isString=%v), want %d bytes", got["header"], ok, len(wantLogged))
			}
			if truncated, ok := got["header_truncated"].(bool); !ok || truncated != tt.wantTruncated {
				t.Fatalf("header_truncated = %v, want %v", got["header_truncated"], tt.wantTruncated)
			}
			if size, ok := got["header_size"].(int64); !ok || size != int64(len(tt.in)) {
				t.Fatalf("header_size = %v, want %d", got["header_size"], len(tt.in))
			}
		})
	}
}

// encodeFields runs the given fields through a zapcore.MapObjectEncoder so
// that assertions observe the same key/value pairs a zap encoder would.
func encodeFields(t *testing.T, fields []zap.Field) map[string]interface{} {
	t.Helper()
	enc := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(enc)
	}
	return enc.Fields
}

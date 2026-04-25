package detect

import "testing"

func TestParseEchoResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"plain v6", "2001:db8::1\n", "2001:db8::1", false},
		{"v6 no newline", "2001:db8::1", "2001:db8::1", false},
		{"v6 with whitespace", "  2001:db8::1  \r\n", "2001:db8::1", false},
		{"reject v4", "1.2.3.4", "", true},
		{"reject empty", "", "", true},
		{"reject garbage", "not-an-ip", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, err := ParseEchoResponse(tt.body)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && ip.String() != tt.want {
				t.Errorf("ip = %s, want %s", ip, tt.want)
			}
		})
	}
}

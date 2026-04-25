package detect

import "testing"

func TestParseEchoResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"plain v6", "2606:4700:4700::1111\n", "2606:4700:4700::1111", false},
		{"v6 no newline", "2606:4700:4700::1111", "2606:4700:4700::1111", false},
		{"v6 with whitespace", "  2606:4700:4700::1111  \r\n", "2606:4700:4700::1111", false},
		{"reject v4", "1.2.3.4", "", true},
		{"reject empty", "", "", true},
		{"reject garbage", "not-an-ip", "", true},
		{"reject ULA", "fd7d::1", "", true},
		{"reject link-local", "fe80::1", "", true},
		{"reject loopback", "::1", "", true},
		{"reject unspecified", "::", "", true},
		{"reject multicast", "ff02::1", "", true},
		{"reject ipv4-mapped v6", "::ffff:1.2.3.4", "", true},
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

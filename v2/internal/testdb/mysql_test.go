package testdb

import "testing"

func TestValidateMySQLDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{name: "suffix token", dsn: "root:secret@tcp(127.0.0.1:3306)/GopherAI_stabilize_test?parseTime=true"},
		{name: "middle token", dsn: "root:secret@tcp(127.0.0.1:3306)/GopherAI_test_stabilize?parseTime=true"},
		{name: "production", dsn: "root:secret@tcp(127.0.0.1:3306)/GopherAI?parseTime=true", wantErr: true},
		{name: "embedded letters", dsn: "root:secret@tcp(127.0.0.1:3306)/GopherAI_contest?parseTime=true", wantErr: true},
		{name: "missing database", dsn: "root:secret@tcp(127.0.0.1:3306)/?parseTime=true", wantErr: true},
		{name: "malformed", dsn: "%", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateMySQLDSN(tt.dsn)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateMySQLDSN() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

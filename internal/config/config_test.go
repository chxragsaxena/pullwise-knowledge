package config

import (
	"strings"
	"testing"
)

func setAll(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"ADDR": "", "DATABASE_URL": "postgres://x", "COHERE_API_KEY": "k",
		"JWT_SECRET": "0123456789abcdef0123456789abcdef", "JWT_ISSUER": "", "JWT_AUDIENCE": "",
		"TLS_CERT_FILE": "", "TLS_KEY_FILE": "", "TLS_CLIENT_CA_FILE": "",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setAll(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8081" || c.JWTIssuer != "pullwise" || c.JWTAudience != "pullwise-knowledge" {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setAll(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("COHERE_API_KEY", "")
	t.Setenv("JWT_SECRET", "short")
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DATABASE_URL", "COHERE_API_KEY", "JWT_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %s: %v", want, err)
		}
	}
}

func TestTLSSettingsMustBeConsistent(t *testing.T) {
	tests := []struct {
		name          string
		cert, key, ca string
		wantErr       bool
	}{
		{"none", "", "", "", false},
		{"cert and key", "c", "k", "", false},
		{"mutual TLS", "c", "k", "ca", false},
		{"cert without key", "c", "", "", true},
		{"key without cert", "", "k", "", true},
		{"client CA without TLS", "", "", "ca", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setAll(t)
			t.Setenv("TLS_CERT_FILE", tt.cert)
			t.Setenv("TLS_KEY_FILE", tt.key)
			t.Setenv("TLS_CLIENT_CA_FILE", tt.ca)
			if _, err := Load(); (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

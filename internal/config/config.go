// Package config reads the service's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/chxragsaxena/pullwise-knowledge/internal/auth"
)

// Config holds process-wide settings.
type Config struct {
	Addr        string
	DatabaseURL string
	// DatabaseSchema keeps this service's tables in their own schema, to share a database.
	DatabaseSchema string
	CohereKey      string

	// JWTSecret signs and verifies tokens; share it only with allowed callers.
	JWTSecret   []byte
	JWTIssuer   string
	JWTAudience string

	// TLSCertFile and TLSKeyFile enable HTTPS; TLSClientCAFile adds mutual TLS.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string
}

// Load reads configuration, reporting every problem at once.
func Load() (Config, error) {
	var errs []error
	c := Config{
		Addr:            getenv("ADDR", ":8081"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DatabaseSchema:  os.Getenv("DATABASE_SCHEMA"),
		CohereKey:       os.Getenv("COHERE_API_KEY"),
		JWTSecret:       []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:       getenv("JWT_ISSUER", "pullwise"),
		JWTAudience:     getenv("JWT_AUDIENCE", "pullwise-knowledge"),
		TLSCertFile:     os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile:      os.Getenv("TLS_KEY_FILE"),
		TLSClientCAFile: os.Getenv("TLS_CLIENT_CA_FILE"),
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.CohereKey == "" {
		errs = append(errs, errors.New("COHERE_API_KEY is required"))
	}
	if len(c.JWTSecret) < auth.MinSecretBytes {
		errs = append(errs, fmt.Errorf("JWT_SECRET is required and must be at least %d bytes", auth.MinSecretBytes))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together"))
	}
	if c.TLSClientCAFile != "" && c.TLSCertFile == "" {
		errs = append(errs, errors.New("TLS_CLIENT_CA_FILE needs TLS_CERT_FILE and TLS_KEY_FILE (mutual TLS runs over TLS)"))
	}
	return c, errors.Join(errs...)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

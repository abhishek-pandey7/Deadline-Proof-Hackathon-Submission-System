// Package config reads the server's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
)

// Environments the server knows how to run in.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
	EnvTest        = "test"
)

// Config holds every setting the server needs at startup.
type Config struct {
	Env         string
	HTTPAddr    string
	DatabaseURL string
	Storage     storage.Config
}

// IsDevelopment reports whether the server runs on a developer machine.
func (c Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// Load builds a Config from getenv (os.Getenv in production, a map in tests).
// HTTP_ADDR wins over PORT, which Render sets; the default is :8080.
func Load(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	c := Config{
		Env:         get("APP_ENV"),
		HTTPAddr:    get("HTTP_ADDR"),
		DatabaseURL: get("DATABASE_URL"),
		Storage: storage.Config{
			Endpoint:        get("R2_ENDPOINT"),
			Region:          get("R2_REGION"),
			AccessKeyID:     get("R2_ACCESS_KEY_ID"),
			SecretAccessKey: get("R2_SECRET_ACCESS_KEY"),
			Bucket:          get("R2_BUCKET"),
		},
	}
	if c.Env == "" {
		c.Env = EnvProduction
	}
	if c.HTTPAddr == "" {
		if port := get("PORT"); port != "" {
			c.HTTPAddr = ":" + port
		} else {
			c.HTTPAddr = ":8080"
		}
	}

	var problems []error
	switch c.Env {
	case EnvDevelopment, EnvProduction, EnvTest:
	default:
		problems = append(problems, fmt.Errorf("APP_ENV must be development, production or test, got %q", c.Env))
	}
	required := []struct{ name, value string }{
		{"DATABASE_URL", c.DatabaseURL},
		{"R2_ENDPOINT", c.Storage.Endpoint},
		{"R2_ACCESS_KEY_ID", c.Storage.AccessKeyID},
		{"R2_SECRET_ACCESS_KEY", c.Storage.SecretAccessKey},
		{"R2_BUCKET", c.Storage.Bucket},
	}
	for _, r := range required {
		if r.value == "" {
			problems = append(problems, fmt.Errorf("%s is required", r.name))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

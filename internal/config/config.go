// Package config reads the server's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"strings"
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
}

// IsDevelopment reports whether the server runs on a developer machine.
func (c Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// Load builds a Config from getenv (os.Getenv in production, a map in tests).
// HTTP_ADDR wins over PORT, which Render sets; the default is :8080.
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		Env:         strings.TrimSpace(getenv("APP_ENV")),
		HTTPAddr:    strings.TrimSpace(getenv("HTTP_ADDR")),
		DatabaseURL: strings.TrimSpace(getenv("DATABASE_URL")),
	}
	if c.Env == "" {
		c.Env = EnvProduction
	}
	if c.HTTPAddr == "" {
		if port := strings.TrimSpace(getenv("PORT")); port != "" {
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
	if c.DatabaseURL == "" {
		problems = append(problems, errors.New("DATABASE_URL is required"))
	}
	if err := errors.Join(problems...); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

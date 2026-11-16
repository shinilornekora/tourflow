// Package config читает настройки сервиса из переменных окружения.
// Перенесено из orderflow (ПКП-1) и обобщено: каждый сервис объявляет
// свою структуру и заполняет её через helpers ниже.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Common — настройки, одинаковые для всех сервисов.
type Common struct {
	ServiceName     string
	HTTPAddr        string
	ShutdownTimeout time.Duration
	LogLevel        string
}

// LoadCommon читает общие переменные: HTTP_ADDR, SHUTDOWN_TIMEOUT, LOG_LEVEL.
func LoadCommon(service, defaultAddr string) (Common, error) {
	timeout, err := Duration("SHUTDOWN_TIMEOUT", 10*time.Second)
	if err != nil {
		return Common{}, err
	}
	if timeout <= 0 {
		return Common{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be positive")
	}
	return Common{
		ServiceName:     service,
		HTTPAddr:        String("HTTP_ADDR", defaultAddr),
		ShutdownTimeout: timeout,
		LogLevel:        String("LOG_LEVEL", "info"),
	}, nil
}

// String возвращает значение переменной или значение по умолчанию.
func String(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// Required возвращает ошибку, если переменная не задана.
func Required(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

// Duration разбирает time.Duration ("250ms", "5s").
func Duration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return v, nil
}

// Int разбирает целое число.
func Int(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return v, nil
}

// List разбирает список через запятую.
func List(name string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

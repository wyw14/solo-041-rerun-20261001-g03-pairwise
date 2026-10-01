package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address            string
	DataDir            string
	WebDir             string
	ShutdownTimeout    time.Duration
	SampleWindow       int
	PowerLimitKW       float64
	PowerReserveKW     float64
	PowerStaleAfter    time.Duration
	PowerStaggerWindow time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Address:            env("BIOTREAT_ADDR", "127.0.0.1:19697"),
		DataDir:            env("BIOTREAT_DATA", filepath.Join("data", "events")),
		WebDir:             env("BIOTREAT_WEB", "web"),
		ShutdownTimeout:    8 * time.Second,
		SampleWindow:       10,
		PowerLimitKW:       envFloat("BIOTREAT_POWER_LIMIT_KW", 160),
		PowerReserveKW:     envFloat("BIOTREAT_POWER_RESERVE_KW", 15),
		PowerStaleAfter:    envDuration("BIOTREAT_POWER_STALE_AFTER", 45*time.Second),
		PowerStaggerWindow: envDuration("BIOTREAT_POWER_STAGGER_WINDOW", 90*time.Second),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Address) == "" {
		return errors.New("listen address is required")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return err
	}
	if c.DataDir == "" || c.WebDir == "" {
		return errors.New("data and web directories are required")
	}
	if c.SampleWindow < 3 || c.SampleWindow > 120 {
		return errors.New("sample window must be between 3 and 120")
	}
	if c.PowerLimitKW <= 0 || c.PowerReserveKW < 0 || c.PowerReserveKW >= c.PowerLimitKW {
		return errors.New("power limit and reserve are invalid")
	}
	if c.PowerStaleAfter <= 0 || c.PowerStaggerWindow <= 0 {
		return errors.New("power telemetry and stagger windows are invalid")
	}
	return nil
}

func (c Config) EnsureDirectories() error {
	return os.MkdirAll(c.DataDir, 0o755)
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envFloat(name string, fallback float64) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return value
}

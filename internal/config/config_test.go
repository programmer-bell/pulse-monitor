package config

import (
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PORT",
		"DATABASE_URL",
		"MAX_WORKERS",
		"CHECK_INTERVAL_SECONDS",
		"CHECK_TIMEOUT_SECONDS",
		"DOMAIN_RPS",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@host/db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.MaxWorkers != defaultMaxWorkers {
		t.Errorf("MaxWorkers = %d, want %d", cfg.MaxWorkers, defaultMaxWorkers)
	}
	if cfg.CheckInterval != defaultCheckInterval {
		t.Errorf("CheckInterval = %s, want %s", cfg.CheckInterval, defaultCheckInterval)
	}
	if cfg.CheckTimeout != defaultCheckTimeout {
		t.Errorf("CheckTimeout = %s, want %s", cfg.CheckTimeout, defaultCheckTimeout)
	}
	if cfg.DomainRPS != defaultDomainRPS {
		t.Errorf("DomainRPS = %d, want %d", cfg.DomainRPS, defaultDomainRPS)
	}
}

func TestLoad_Overrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://user:pass@host/db")
	t.Setenv("MAX_WORKERS", "8")
	t.Setenv("CHECK_INTERVAL_SECONDS", "5")
	t.Setenv("CHECK_TIMEOUT_SECONDS", "2")
	t.Setenv("DOMAIN_RPS", "20")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.MaxWorkers != 8 {
		t.Errorf("MaxWorkers = %d, want 8", cfg.MaxWorkers)
	}
	if cfg.CheckInterval != 5*time.Second {
		t.Errorf("CheckInterval = %s, want 5s", cfg.CheckInterval)
	}
	if cfg.CheckTimeout != 2*time.Second {
		t.Errorf("CheckTimeout = %s, want 2s", cfg.CheckTimeout)
	}
	if cfg.DomainRPS != 20 {
		t.Errorf("DomainRPS = %d, want 20", cfg.DomainRPS)
	}
}

func TestLoad_Errors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{
			name:    "missing database url",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "non-integer port",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "PORT": "not-a-number"},
			wantErr: true,
		},
		{
			name:    "port out of range",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "PORT": "70000"},
			wantErr: true,
		},
		{
			name:    "zero max workers",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "MAX_WORKERS": "0"},
			wantErr: true,
		},
		{
			name:    "zero check interval",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "CHECK_INTERVAL_SECONDS": "0"},
			wantErr: true,
		},
		{
			name:    "zero check timeout",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "CHECK_TIMEOUT_SECONDS": "0"},
			wantErr: true,
		},
		{
			name:    "zero domain rps",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "DOMAIN_RPS": "0"},
			wantErr: true,
		},
		{
			name:    "non-integer domain rps",
			env:     map[string]string{"DATABASE_URL": "postgres://x", "DOMAIN_RPS": "fast"},
			wantErr: true,
		},
		{
			name:    "valid minimal config",
			env:     map[string]string{"DATABASE_URL": "postgres://x"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

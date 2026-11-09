package reload

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
)

func TestRestartRequired(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Mode:     config.ModeL7,
			Listen:   ":8443",
			Balancer: config.AlgRoundRobin,
			TLS:      &config.TLS{Cert: "a.crt", Key: "a.key"},
		}
	}

	tests := []struct {
		name    string
		change  func(c *config.Config)
		wantErr string // empty: the reload is allowed
	}{
		{"nothing changed", func(c *config.Config) {}, ""},
		{"mode changed", func(c *config.Config) { c.Mode = config.ModeL4 }, "mode"},
		{"listen changed", func(c *config.Config) { c.Listen = ":9443" }, "listen"},
		{"tls removed", func(c *config.Config) { c.TLS = nil }, "tls"},
		{"cert rotated", func(c *config.Config) { c.TLS = &config.TLS{Cert: "b.crt", Key: "b.key"} }, ""},
		{"balancer changed", func(c *config.Config) { c.Balancer = config.AlgLeastConns }, ""},
		{"health changed", func(c *config.Config) { c.Health.Active.Path = "/healthz" }, "health"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, next := base(), base()
			tt.change(next)

			err := restartRequired(old, next)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want reload allowed, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}

	// Turning TLS on is refused too (the base config already has it, so
	// start from a copy without it).
	old, next := base(), base()
	old.TLS = nil
	if err := restartRequired(old, next); err == nil {
		t.Error("tls added: want error, got nil")
	}
}

// A valid file that changes the mode must be refused and the running config must be kept
func TestApply_RejectsModeChangeAndKeepsOldConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lb.yaml")

	writeFile(t, path, `mode: l4
listen: "127.0.0.1:8080"
pools:
  default:
    - addr: "127.0.0.1:9001"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(cfg)
	reg := health.NewRegistry()
	reg.Reconcile(cfg.BackendAddrs())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := health.NewManager(reg, cfg.Health, log, nil)
	defer mgr.Stop()

	writeFile(t, path, `mode: l7
listen: "127.0.0.1:8080"
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9001"
`)

	err = Apply(path, store, reg, mgr, log)
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Apply error = %v, want a mode error", err)
	}
	if store.Load() != cfg {
		t.Error("store holds a new config; the refused reload was published")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

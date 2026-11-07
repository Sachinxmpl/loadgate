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

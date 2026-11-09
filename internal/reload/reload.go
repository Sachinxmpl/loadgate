package reload

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/health"
)

// Loads and validates the config at the path. If valid, swapt it into store and reconciles health
func Apply(path string, store *config.Store, reg *health.Registry, mgr *health.Manager, log *slog.Logger) error {
	newCfg, err := config.Load(path)
	if err != nil {
		log.Error("config reload failed", "path", path, "err", err)
		return err
	}
	old := store.Load()
	if err := restartRequired(old, newCfg); err != nil {
		return err
	}
	warnRestartRequired(old, newCfg, log)

	// atomic swatp
	store.Publish(newCfg)

	added, removed := reg.Reconcile(newCfg.BackendAddrs())
	mgr.Sync(added, removed)

	log.Info("config reloaded", "backends", len(newCfg.BackendAddrs()), "added", len(added), "removed", len(removed))

	return nil
}

func restartRequired(old, new *config.Config) error {
	if old.Mode != new.Mode {
		return fmt.Errorf("mode: changing %q to %q needs a restart", old.Mode, new.Mode)
	}
	if old.Listen != new.Listen {
		return fmt.Errorf("listen: changing %q to %q needs a restart", old.Listen, new.Listen)
	}
	if (old.TLS == nil) != (new.TLS == nil) {
		return errors.New("tls: turning TLS on or off needs a restart")
	}
	return nil
}

// Warn when reload changes a field that is harmless to keep running with
func warnRestartRequired(old, new *config.Config, log *slog.Logger) {
	if old.Balancer != new.Balancer {
		log.Warn("balancer algorithm change needs a restart to take effect", "old", old.Balancer, "new", new.Balancer)
	}
	if old.Health != new.Health {
		log.Warn("health settings change needs a restart to take effect")
	}
}

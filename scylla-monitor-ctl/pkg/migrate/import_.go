package migrate

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/grafana"

	"gopkg.in/yaml.v3"
)

// RestoreOptions holds options for importing a monitoring stack from an archive.
type RestoreOptions struct {
	ArchivePath     string
	DataDir         string // where to place Prometheus data
	GrafanaDataDir  string
	GrafanaURL      string
	GrafanaUser     string
	GrafanaPassword string
	GrafanaPort     int
	PrometheusURL   string // if set, rewrite Prometheus datasource URLs to this value
}

// RestoreStack restores a monitoring stack from an export archive or directory.
func RestoreStack(opts RestoreOptions) error {
	var extractDir string
	var cleanup func()

	info, err := os.Stat(opts.ArchivePath)
	if err != nil {
		return fmt.Errorf("accessing %s: %w", opts.ArchivePath, err)
	}

	if info.IsDir() {
		// Already unpacked — use directly
		extractDir = opts.ArchivePath
		cleanup = func() {}
		slog.Info("using unpacked export directory", "path", extractDir)
	} else {
		// Archive — extract to temp dir
		tmpDir, err := os.MkdirTemp("", "scylla-monitor-import-*")
		if err != nil {
			return fmt.Errorf("creating extract directory: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(tmpDir) }

		if err := UnpackArchive(opts.ArchivePath, tmpDir); err != nil {
			cleanup()
			return fmt.Errorf("unpacking archive: %w", err)
		}
		extractDir = tmpDir
	}
	defer cleanup()

	// Read metadata
	metaPath := filepath.Join(extractDir, "metadata.yaml")
	metaData, err := os.ReadFile(metaPath) //nolint:gosec // constructed from extract dir
	if err != nil {
		return fmt.Errorf("reading metadata: %w", err)
	}
	var meta Metadata
	if err := yaml.Unmarshal(metaData, &meta); err != nil {
		return fmt.Errorf("parsing metadata: %w", err)
	}

	fmt.Printf("Importing export from %s (%d dashboards, %d datasources)\n",
		meta.ExportTimestamp, meta.DashboardCount, meta.DatasourceCount)

	// Copy config files to local paths
	promConfigSrc := filepath.Join(extractDir, "prometheus", "prometheus.yml")
	if _, err := os.Stat(promConfigSrc); err == nil {
		_ = os.MkdirAll("prometheus/build", 0750)
		_ = copyFile(promConfigSrc, "prometheus/build/prometheus.yml")
	}

	rulesDir := filepath.Join(extractDir, "prometheus", "prom_rules")
	if _, err := os.Stat(rulesDir); err == nil {
		_ = copyDir(rulesDir, "prometheus/prom_rules")
	}

	amConfigSrc := filepath.Join(extractDir, "alertmanager", "config.yml")
	if _, err := os.Stat(amConfigSrc); err == nil {
		_ = copyFile(amConfigSrc, "prometheus/rule_config.yml")
	}

	// Copy target files
	targetsDir := filepath.Join(extractDir, "targets")
	if _, err := os.Stat(targetsDir); err == nil {
		entries, _ := os.ReadDir(targetsDir)
		_ = os.MkdirAll("prometheus", 0750)
		for _, e := range entries {
			_ = copyFile(filepath.Join(targetsDir, e.Name()), filepath.Join("prometheus", e.Name()))
		}
	}

	// Upload dashboards and datasources to Grafana if URL provided
	if opts.GrafanaURL != "" {
		gc := grafana.NewClient(opts.GrafanaURL, opts.GrafanaUser, opts.GrafanaPassword)

		// Wait for Grafana to be ready
		if err := gc.Health(); err != nil {
			return fmt.Errorf("grafana not ready: %w", err)
		}

		// Import datasources
		dsDir := filepath.Join(extractDir, "datasources")
		if entries, err := os.ReadDir(dsDir); err == nil {
			for _, e := range entries {
				data, err := os.ReadFile(filepath.Join(dsDir, e.Name())) //nolint:gosec // reading extracted archive files
				if err != nil {
					continue
				}
				var ds grafana.APIDatasource
				if err := json.Unmarshal(data, &ds); err != nil {
					continue
				}
				ds.ID = 0 // Clear ID for create
				if opts.PrometheusURL != "" && ds.Type == "prometheus" {
					slog.Info("rewriting datasource URL", "datasource", ds.Name, "old", ds.URL, "new", opts.PrometheusURL)
					ds.URL = opts.PrometheusURL
				}
				if err := gc.UpsertDatasource(ds); err != nil {
					slog.Warn("upserting datasource", "datasource", ds.Name, "error", err)
				}
			}
		}

		// Import dashboards
		dashDir := filepath.Join(extractDir, "dashboards")
		if entries, err := os.ReadDir(dashDir); err == nil {
			for _, e := range entries {
				data, err := os.ReadFile(filepath.Join(dashDir, e.Name())) //nolint:gosec // reading extracted archive files
				if err != nil {
					continue
				}
				// Extract the dashboard object from the wrapper
				var wrapper map[string]json.RawMessage
				if err := json.Unmarshal(data, &wrapper); err != nil {
					continue
				}
				dashJSON := data
				if dash, ok := wrapper["dashboard"]; ok {
					dashJSON = dash
				}
				if err := gc.UploadDashboard(dashJSON, 0, true); err != nil {
					slog.Warn("uploading dashboard", "file", e.Name(), "error", err)
				}
			}
		}
	}

	return nil
}

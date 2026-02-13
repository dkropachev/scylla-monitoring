package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/docker"
	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/migrate"
)

var migrateExportFlags struct {
	GrafanaConnFlags
	PrometheusURL      string
	Output             string
	PrometheusConfig   string
	AlertRulesDir      string
	AlertManagerConfig string
	LokiConfig         string
	TargetFiles        []string
}

var migrateImportFlags struct {
	GrafanaConnFlags
	DataDir        string
	GrafanaDataDir string
	GrafanaPort    int
	PrometheusURL  string
}

var migrateCloneFlags struct {
	GrafanaConnFlags
	PrometheusURL    string
	PrometheusPort   int
	GrafanaPort      int
	AlertManagerPort int
	StackID          int
}

var migrateCopyFlags struct {
	Source             GrafanaConnFlags
	Target             GrafanaConnFlags
	IncludeDashboards  bool
	IncludeDatasources bool
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Stack migration operations",
	Long:  `Export, import, or copy monitoring stack configurations and data.`,
}

var migrateExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export a monitoring stack",
	Long: `Export dashboards, datasources, configs, and optionally data to an archive.

Prometheus metric data is included automatically when --prometheus-url is provided.
Without it, only configuration files and Grafana dashboards/datasources are exported.`,
	SilenceUsage: true,
	RunE:         runMigrateExport,
}

var migrateImportCmd = &cobra.Command{
	Use:   "import PATH",
	Short: "Import a monitoring stack from an archive or directory",
	Long: `Restore dashboards, configs, and optionally data from an export archive
or an unpacked export directory.

PATH can be a .tar.gz archive or a directory containing the unpacked export.

When --prometheus-url is provided, imported Prometheus datasource URLs are
rewritten to point to the given address instead of the original source.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runMigrateImport,
}

var migrateCloneCmd = &cobra.Command{
	Use:   "clone",
	Short: "Clone a running stack to new ports",
	Long: `Clone a monitoring stack by exporting from a running source and deploying
a new stack on different ports. The new stack scrapes the same targets as the source.

Target files are auto-discovered from the source Prometheus API. Datasource URLs
are automatically rewritten to use the new container-internal addresses.`,
	SilenceUsage: true,
	RunE:         runMigrateClone,
}

var migrateCopyCmd = &cobra.Command{
	Use:          "copy",
	Short:        "Live copy from one stack to another",
	Long:         `Copy dashboards and datasources from a source Grafana to a target.`,
	SilenceUsage: true,
	RunE:         runMigrateCopy,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
	migrateCmd.AddCommand(migrateExportCmd)
	migrateCmd.AddCommand(migrateImportCmd)
	migrateCmd.AddCommand(migrateCloneCmd)
	migrateCmd.AddCommand(migrateCopyCmd)

	// Export flags
	migrateExportFlags.Register(migrateExportCmd, "")
	ef := migrateExportCmd.Flags()
	ef.StringVar(&migrateExportFlags.PrometheusURL, "prometheus-url", "", "Prometheus URL (enables metric data export)")
	ef.StringVar(&migrateExportFlags.Output, "output", "stack-export.tar.gz", "Output archive path")
	ef.StringVar(&migrateExportFlags.PrometheusConfig, "prometheus-config", "prometheus/build/prometheus.yml", "Path to prometheus.yml")
	ef.StringVar(&migrateExportFlags.AlertRulesDir, "alert-rules-dir", "prometheus/prom_rules", "Path to alert rules directory")
	ef.StringVar(&migrateExportFlags.AlertManagerConfig, "alertmanager-config", "prometheus/rule_config.yml", "Path to AlertManager config")
	ef.StringVar(&migrateExportFlags.LokiConfig, "loki-config", "", "Path to Loki config")
	ef.StringSliceVar(&migrateExportFlags.TargetFiles, "target-files", nil, "Target files to include")

	// Import flags
	migrateImportFlags.Register(migrateImportCmd, "")
	imf := migrateImportCmd.Flags()
	imf.StringVar(&migrateImportFlags.PrometheusURL, "prometheus-url", "", "Rewrite Prometheus datasource URLs to this address")
	imf.StringVar(&migrateImportFlags.DataDir, "data-dir", "", "Prometheus data directory")
	imf.StringVar(&migrateImportFlags.GrafanaDataDir, "grafana-data-dir", "", "Grafana data directory")
	imf.IntVar(&migrateImportFlags.GrafanaPort, "grafana-port", 3000, "Grafana port")

	// Clone flags
	migrateCloneFlags.Register(migrateCloneCmd, "http://localhost:3000")
	clf := migrateCloneCmd.Flags()
	clf.StringVar(&migrateCloneFlags.PrometheusURL, "prometheus-url", "http://localhost:9090", "Source Prometheus URL")
	clf.IntVar(&migrateCloneFlags.PrometheusPort, "prometheus-port", 9091, "Target Prometheus port")
	clf.IntVar(&migrateCloneFlags.GrafanaPort, "grafana-port", 3001, "Target Grafana port")
	clf.IntVar(&migrateCloneFlags.AlertManagerPort, "alertmanager-port", 9095, "Target AlertManager port")
	clf.IntVar(&migrateCloneFlags.StackID, "stack", 1, "Target stack ID")

	// Copy flags
	migrateCopyFlags.Source.RegisterWithPrefix(migrateCopyCmd, "source-", "Source")
	migrateCopyFlags.Target.RegisterWithPrefix(migrateCopyCmd, "target-", "Target")
	cf := migrateCopyCmd.Flags()
	cf.BoolVar(&migrateCopyFlags.IncludeDashboards, "include-dashboards", true, "Copy dashboards")
	cf.BoolVar(&migrateCopyFlags.IncludeDatasources, "include-datasources", true, "Copy datasources")
	_ = migrateCopyCmd.MarkFlagRequired("source-grafana-url")
	_ = migrateCopyCmd.MarkFlagRequired("target-grafana-url")
}

func runMigrateExport(cmd *cobra.Command, args []string) error {
	if migrateExportFlags.PrometheusURL == "" {
		slog.Warn("no --prometheus-url provided, metric data will not be included in the export")
	}

	opts := migrate.ArchiveOptions{
		PrometheusURL:      migrateExportFlags.PrometheusURL,
		GrafanaURL:         migrateExportFlags.URL,
		GrafanaUser:        migrateExportFlags.User,
		GrafanaPassword:    migrateExportFlags.Password,
		OutputPath:         migrateExportFlags.Output,
		PrometheusConfig:   migrateExportFlags.PrometheusConfig,
		AlertRulesDir:      migrateExportFlags.AlertRulesDir,
		AlertManagerConfig: migrateExportFlags.AlertManagerConfig,
		LokiConfig:         migrateExportFlags.LokiConfig,
		TargetFiles:        migrateExportFlags.TargetFiles,
	}

	if err := migrate.ArchiveStack(opts); err != nil {
		return fmt.Errorf("export failed: %w", err)
	}
	fmt.Printf("Stack exported to %s\n", opts.OutputPath)
	return nil
}

func runMigrateImport(cmd *cobra.Command, args []string) error {
	opts := migrate.RestoreOptions{
		ArchivePath:     args[0],
		PrometheusURL:   migrateImportFlags.PrometheusURL,
		DataDir:         migrateImportFlags.DataDir,
		GrafanaDataDir:  migrateImportFlags.GrafanaDataDir,
		GrafanaURL:      migrateImportFlags.URL,
		GrafanaUser:     migrateImportFlags.User,
		GrafanaPassword: migrateImportFlags.Password,
		GrafanaPort:     migrateImportFlags.GrafanaPort,
	}

	if err := migrate.RestoreStack(opts); err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	fmt.Println("Stack imported successfully.")
	return nil
}

func runMigrateClone(cmd *cobra.Command, args []string) error {
	runtime, _ := docker.DetectRuntime(cmd.Context())

	opts := migrate.CloneOptions{
		SourceGrafanaURL:      migrateCloneFlags.URL,
		SourceGrafanaUser:     migrateCloneFlags.User,
		SourceGrafanaPassword: migrateCloneFlags.Password,
		SourcePrometheusURL:   migrateCloneFlags.PrometheusURL,
		PrometheusPort:        migrateCloneFlags.PrometheusPort,
		GrafanaPort:           migrateCloneFlags.GrafanaPort,
		AlertManagerPort:      migrateCloneFlags.AlertManagerPort,
		StackID:               migrateCloneFlags.StackID,
		PrometheusImage:       "prom/prometheus:v3.9.1",
		GrafanaImage:          "grafana/grafana:12.3.2",
		AlertManagerImage:     "prom/alertmanager:v0.30.1",
		Runtime:               runtime,
	}

	ctx := context.Background()
	return migrate.CloneStack(ctx, opts)
}

func runMigrateCopy(cmd *cobra.Command, args []string) error {
	opts := migrate.CopyOptions{
		SourceGrafanaURL:      migrateCopyFlags.Source.URL,
		SourceGrafanaUser:     migrateCopyFlags.Source.User,
		SourceGrafanaPassword: migrateCopyFlags.Source.Password,
		TargetGrafanaURL:      migrateCopyFlags.Target.URL,
		TargetGrafanaUser:     migrateCopyFlags.Target.User,
		TargetGrafanaPassword: migrateCopyFlags.Target.Password,
		IncludeDashboards:     migrateCopyFlags.IncludeDashboards,
		IncludeDatasources:    migrateCopyFlags.IncludeDatasources,
	}

	if err := migrate.Copy(opts); err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}
	fmt.Println("Stack copied successfully.")
	return nil
}

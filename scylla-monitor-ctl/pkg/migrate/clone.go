package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/docker"
	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/grafana"
	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/prometheus"

	"gopkg.in/yaml.v3"
)

// CloneOptions holds options for cloning a monitoring stack.
type CloneOptions struct {
	// Source
	SourceGrafanaURL      string
	SourceGrafanaUser     string
	SourceGrafanaPassword string
	SourcePrometheusURL   string

	// Target ports
	PrometheusPort   int
	GrafanaPort      int
	AlertManagerPort int
	StackID          int

	// Images
	PrometheusImage   string
	GrafanaImage      string
	AlertManagerImage string

	// Runtime
	Runtime docker.Runtime
}

// CloneStack clones a monitoring stack to a new set of ports.
func CloneStack(ctx context.Context, opts CloneOptions) error {
	// 1. Export dashboards + datasources from source Grafana
	slog.Info("exporting from source stack")
	gc := grafana.NewClient(opts.SourceGrafanaURL, opts.SourceGrafanaUser, opts.SourceGrafanaPassword)
	if err := gc.Health(); err != nil {
		return fmt.Errorf("source Grafana not reachable: %w", err)
	}

	dashboards, err := exportDashboards(gc)
	if err != nil {
		return fmt.Errorf("exporting dashboards: %w", err)
	}
	slog.Info("exported dashboards", "count", len(dashboards))

	datasources, err := gc.ListDatasources()
	if err != nil {
		return fmt.Errorf("exporting datasources: %w", err)
	}
	slog.Info("exported datasources", "count", len(datasources))

	// 2. Get targets from source Prometheus
	pc := prometheus.NewClient(opts.SourcePrometheusURL)
	if err := pc.Health(); err != nil {
		return fmt.Errorf("source Prometheus not reachable: %w", err)
	}

	targetGroups, err := pc.QueryTargetGroups()
	if err != nil {
		return fmt.Errorf("querying source targets: %w", err)
	}
	slog.Info("discovered target files", "count", len(targetGroups))

	// 3. Write target files to a local staging directory
	stageDir, err := os.MkdirTemp("", "scylla-monitor-clone-*")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	// Write each target file to staging, preserving the container-internal path structure
	targetMounts, err := writeTargetFiles(stageDir, targetGroups)
	if err != nil {
		return fmt.Errorf("writing target files: %w", err)
	}

	// 4. Copy prometheus.yml and fix internal references
	promConfigSrc := "prometheus/build/prometheus.yml"
	promConfigData, err := os.ReadFile(promConfigSrc)
	if err != nil {
		return fmt.Errorf("reading prometheus.yml: %w", err)
	}

	// Fix grafana and prometheus self-scrape addresses for the new ports
	promName := docker.ContainerName("aprom", opts.PrometheusPort, 9090)
	grafName := docker.ContainerName("agraf", opts.GrafanaPort, 3000)
	amName := docker.ContainerName("aalert", opts.AlertManagerPort, 9093)

	promConfig := string(promConfigData)
	// Replace static grafana target
	promConfig = replaceStaticTarget(promConfig, "grafana", grafName+":3000")
	// Replace static prometheus target
	promConfig = replaceStaticTarget(promConfig, "prometheus", "localhost:9090")
	// Replace alertmanager target
	promConfig = replaceAlertmanagerTarget(promConfig, amName+":9093")

	clonedConfigPath := filepath.Join(stageDir, "prometheus.yml")
	if err := os.WriteFile(clonedConfigPath, []byte(promConfig), 0644); err != nil {
		return fmt.Errorf("writing cloned prometheus.yml: %w", err)
	}

	// 5. Deploy new stack
	slog.Info("deploying cloned stack",
		"prometheus", opts.PrometheusPort,
		"grafana", opts.GrafanaPort,
		"alertmanager", opts.AlertManagerPort)

	networkName := docker.NetworkName(opts.StackID)
	if err := docker.CreateNetwork(ctx, opts.Runtime, opts.StackID); err != nil {
		return fmt.Errorf("creating network: %w", err)
	}

	// AlertManager
	amCfg := docker.ContainerConfig{
		Name:         amName,
		Image:        opts.AlertManagerImage,
		NetworkName:  networkName,
		PortBindings: map[string]string{"9093/tcp": fmt.Sprintf("%d", opts.AlertManagerPort)},
		Cmd:          []string{"--config.file=/etc/alertmanager/config.yml"},
		Mounts: []docker.MountConfig{
			{Source: absPath("prometheus/rule_config.yml"), Target: "/etc/alertmanager/config.yml", ReadOnly: true},
		},
	}
	if _, err := docker.StartContainer(ctx, opts.Runtime, amCfg); err != nil {
		return fmt.Errorf("starting AlertManager: %w", err)
	}

	// Prometheus — mount config + all target files at correct container paths
	promMounts := []docker.MountConfig{
		{Source: clonedConfigPath, Target: "/etc/prometheus/prometheus.yml", ReadOnly: true},
		{Source: absPath("prometheus/prom_rules"), Target: "/etc/prometheus/prom_rules", ReadOnly: true},
	}
	for hostPath, containerPath := range targetMounts {
		promMounts = append(promMounts, docker.MountConfig{
			Source: hostPath, Target: containerPath, ReadOnly: true,
		})
	}

	promCfg := docker.ContainerConfig{
		Name:         promName,
		Image:        opts.PrometheusImage,
		NetworkName:  networkName,
		PortBindings: map[string]string{"9090/tcp": fmt.Sprintf("%d", opts.PrometheusPort)},
		Cmd: []string{
			"--config.file=/etc/prometheus/prometheus.yml",
			"--storage.tsdb.path=/prometheus",
			"--web.listen-address=0.0.0.0:9090",
			"--web.enable-lifecycle",
		},
		Mounts: promMounts,
	}
	if _, err := docker.StartContainer(ctx, opts.Runtime, promCfg); err != nil {
		return fmt.Errorf("starting Prometheus: %w", err)
	}

	// Grafana
	grafMounts := []docker.MountConfig{
		{Source: absPath("grafana/build"), Target: "/var/lib/grafana/dashboards"},
		{Source: absPath("grafana/plugins"), Target: "/var/lib/grafana/plugins"},
		{Source: absPath("grafana/provisioning"), Target: "/var/lib/grafana/provisioning"},
	}
	grafCfg := docker.ContainerConfig{
		Name:         grafName,
		Image:        opts.GrafanaImage,
		NetworkName:  networkName,
		PortBindings: map[string]string{"3000/tcp": fmt.Sprintf("%d", opts.GrafanaPort)},
		Env: []string{
			"GF_PATHS_PROVISIONING=/var/lib/grafana/provisioning",
			"GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=scylladb-scylla-datasource",
			"GF_DATABASE_WAL=true",
			"GF_AUTH_ANONYMOUS_ENABLED=true",
			"GF_AUTH_ANONYMOUS_ORG_ROLE=Admin",
		},
		Mounts: grafMounts,
	}
	if _, err := docker.StartContainer(ctx, opts.Runtime, grafCfg); err != nil {
		return fmt.Errorf("starting Grafana: %w", err)
	}

	// 6. Wait for services
	slog.Info("waiting for services to start")
	promURL := fmt.Sprintf("http://localhost:%d/-/ready", opts.PrometheusPort)
	if err := docker.WaitForHealth(ctx, promURL, 35, time.Second); err != nil {
		return fmt.Errorf("Prometheus health check: %w", err)
	}
	grafURL := fmt.Sprintf("http://localhost:%d/api/health", opts.GrafanaPort)
	if err := docker.WaitForHealth(ctx, grafURL, 35, time.Second); err != nil {
		return fmt.Errorf("Grafana health check: %w", err)
	}

	// 7. Import datasources with rewritten Prometheus URL
	targetGC := grafana.NewClient(
		fmt.Sprintf("http://localhost:%d", opts.GrafanaPort),
		opts.SourceGrafanaUser,
		opts.SourceGrafanaPassword,
	)

	for _, ds := range datasources {
		ds.ID = 0
		if ds.Type == "prometheus" {
			newURL := fmt.Sprintf("http://%s:9090", promName)
			slog.Info("rewriting datasource URL", "datasource", ds.Name, "old", ds.URL, "new", newURL)
			ds.URL = newURL
		}
		if ds.Type == "alertmanager" {
			newURL := fmt.Sprintf("http://%s:9093", amName)
			slog.Info("rewriting datasource URL", "datasource", ds.Name, "old", ds.URL, "new", newURL)
			ds.URL = newURL
		}
		if err := targetGC.UpsertDatasource(ds); err != nil {
			slog.Warn("upserting datasource", "datasource", ds.Name, "error", err)
		}
	}

	// 8. Upload dashboards
	for uid, data := range dashboards {
		if err := targetGC.UploadDashboard(data, 0, true); err != nil {
			slog.Warn("uploading dashboard", "uid", uid, "error", err)
		}
	}
	slog.Info("imported dashboards", "count", len(dashboards))

	fmt.Printf("\nCloned stack is running:\n")
	fmt.Printf("  Grafana:      http://localhost:%d\n", opts.GrafanaPort)
	fmt.Printf("  Prometheus:   http://localhost:%d\n", opts.PrometheusPort)
	fmt.Printf("  AlertManager: http://localhost:%d\n", opts.AlertManagerPort)
	return nil
}

// exportDashboards downloads all dashboards, returns map[uid] → dashboard JSON (inner object).
func exportDashboards(gc *grafana.Client) (map[string]json.RawMessage, error) {
	results, err := gc.SearchDashboards()
	if err != nil {
		return nil, err
	}
	dashboards := make(map[string]json.RawMessage, len(results))
	for _, r := range results {
		data, err := gc.DownloadDashboard(r.UID)
		if err != nil {
			slog.Warn("downloading dashboard", "uid", r.UID, "error", err)
			continue
		}
		// Extract inner dashboard object, strip id for portability
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(data, &wrapper); err == nil {
			if dash, ok := wrapper["dashboard"]; ok {
				var parsed map[string]interface{}
				if err := json.Unmarshal(dash, &parsed); err == nil {
					delete(parsed, "id")
					stripped, _ := json.Marshal(parsed)
					dashboards[r.UID] = stripped
					continue
				}
			}
		}
		dashboards[r.UID] = data
	}
	return dashboards, nil
}

// writeTargetFiles writes discovered target groups to files in stageDir,
// preserving the container-internal directory structure.
// Returns a map of hostPath → containerPath for mount bindings.
func writeTargetFiles(stageDir string, targetGroups map[string][]prometheus.TargetGroup) (map[string]string, error) {
	mounts := make(map[string]string)

	// Group target files by parent directory to create directory-level mounts
	dirFiles := make(map[string]map[string][]prometheus.TargetGroup) // dir → filename → groups
	for containerPath, groups := range targetGroups {
		dir := filepath.Dir(containerPath)
		base := filepath.Base(containerPath)
		if dirFiles[dir] == nil {
			dirFiles[dir] = make(map[string][]prometheus.TargetGroup)
		}
		dirFiles[dir][base] = groups
	}

	for containerDir, files := range dirFiles {
		// Create a local directory for this container directory
		localDir := filepath.Join(stageDir, "targets", strings.ReplaceAll(containerDir, "/", "_"))
		if err := os.MkdirAll(localDir, 0755); err != nil {
			return nil, fmt.Errorf("creating target dir: %w", err)
		}

		for filename, groups := range files {
			data, err := yaml.Marshal(groups)
			if err != nil {
				return nil, fmt.Errorf("marshaling targets: %w", err)
			}
			localPath := filepath.Join(localDir, filename)
			if err := os.WriteFile(localPath, data, 0644); err != nil {
				return nil, fmt.Errorf("writing target file: %w", err)
			}
			slog.Info("wrote target file", "file", filename, "targets", len(groups))
		}

		mounts[localDir] = containerDir
	}

	return mounts, nil
}

var staticTargetRe = regexp.MustCompile(`(- targets:\s*\n\s+- )(\S+)`)

// replaceStaticTarget replaces the target address in a named static_configs job.
func replaceStaticTarget(config, jobName, newTarget string) string {
	// Find the job block and replace its static target
	jobRe := regexp.MustCompile(`(?m)(- job_name:\s*'?` + regexp.QuoteMeta(jobName) + `'?\s*\n(?:.*\n)*?\s+- targets:\s*\n\s+- )(\S+)`)
	return jobRe.ReplaceAllString(config, "${1}"+newTarget)
}

// replaceAlertmanagerTarget replaces the alertmanager target address.
func replaceAlertmanagerTarget(config, newTarget string) string {
	re := regexp.MustCompile(`(alertmanagers:\s*\n\s+- static_configs:\s*\n\s+- targets:\s*\n\s+- )(\S+)`)
	return re.ReplaceAllString(config, "${1}"+newTarget)
}

func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

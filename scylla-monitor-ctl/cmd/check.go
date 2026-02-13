package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/grafana"
	"github.com/scylladb/scylla-monitoring/scylla-monitor-ctl/pkg/prometheus"
)

var checkFlags struct {
	GrafanaURL      string
	GrafanaUser     string
	GrafanaPassword string
	PrometheusURL   string
	AlertManagerURL string
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Check health of the monitoring stack",
	Long: `Perform API-level health checks on all monitoring stack components.

Unlike 'status' which checks container state, 'check' validates that services
are actually responding and working correctly: Grafana, Prometheus, AlertManager,
datasource connectivity, and firing alerts.`,
	SilenceUsage: true,
	RunE:         runCheck,
}

func init() {
	rootCmd.AddCommand(checkCmd)
	f := checkCmd.Flags()
	f.StringVar(&checkFlags.GrafanaURL, "grafana-url", "http://localhost:3000", "Grafana URL")
	f.StringVar(&checkFlags.GrafanaUser, "grafana-user", "admin", "Grafana user")
	f.StringVar(&checkFlags.GrafanaPassword, "grafana-password", "admin", "Grafana password")
	f.StringVar(&checkFlags.PrometheusURL, "prometheus-url", "http://localhost:9090", "Prometheus URL")
	f.StringVar(&checkFlags.AlertManagerURL, "alertmanager-url", "http://localhost:9093", "AlertManager URL")
}

type checkResult struct {
	Component string
	Check     string
	Status    string // "ok", "warn", "fail"
	Detail    string
}

func runCheck(cmd *cobra.Command, args []string) error {
	var results []checkResult

	results = append(results, checkGrafana()...)
	results = append(results, checkPrometheus()...)
	results = append(results, checkAlertManager()...)
	results = append(results, checkDatasources()...)
	results = append(results, checkAlerts()...)

	printResults(results)

	for _, r := range results {
		if r.Status == "fail" {
			return fmt.Errorf("one or more checks failed")
		}
	}
	return nil
}

func checkGrafana() []checkResult {
	gc := grafana.NewClient(checkFlags.GrafanaURL, checkFlags.GrafanaUser, checkFlags.GrafanaPassword)
	if err := gc.Health(); err != nil {
		return []checkResult{{
			Component: "Grafana",
			Check:     "API Health",
			Status:    "fail",
			Detail:    err.Error(),
		}}
	}
	results := []checkResult{{
		Component: "Grafana",
		Check:     "API Health",
		Status:    "ok",
		Detail:    checkFlags.GrafanaURL,
	}}

	// Check dashboard count
	dashboards, err := gc.SearchDashboards()
	if err != nil {
		results = append(results, checkResult{
			Component: "Grafana",
			Check:     "Dashboards",
			Status:    "warn",
			Detail:    fmt.Sprintf("could not list dashboards: %v", err),
		})
	} else if len(dashboards) == 0 {
		results = append(results, checkResult{
			Component: "Grafana",
			Check:     "Dashboards",
			Status:    "warn",
			Detail:    "no dashboards found",
		})
	} else {
		results = append(results, checkResult{
			Component: "Grafana",
			Check:     "Dashboards",
			Status:    "ok",
			Detail:    fmt.Sprintf("%d dashboards loaded", len(dashboards)),
		})
	}

	return results
}

func checkPrometheus() []checkResult {
	pc := prometheus.NewClient(checkFlags.PrometheusURL)
	if err := pc.Health(); err != nil {
		return []checkResult{{
			Component: "Prometheus",
			Check:     "API Health",
			Status:    "fail",
			Detail:    err.Error(),
		}}
	}
	results := []checkResult{{
		Component: "Prometheus",
		Check:     "API Health",
		Status:    "ok",
		Detail:    checkFlags.PrometheusURL,
	}}

	// Check if Prometheus has any targets with data
	data, err := pc.QueryInstant("up")
	if err != nil {
		results = append(results, checkResult{
			Component: "Prometheus",
			Check:     "Scrape targets",
			Status:    "warn",
			Detail:    fmt.Sprintf("could not query targets: %v", err),
		})
	} else {
		var qr struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]interface{}    `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &qr); err == nil {
			up := 0
			down := 0
			for _, r := range qr.Result {
				if len(r.Value) == 2 {
					if v, ok := r.Value[1].(string); ok && v == "1" {
						up++
					} else {
						down++
					}
				}
			}
			status := "ok"
			if down > 0 && up == 0 {
				status = "fail"
			} else if down > 0 {
				status = "warn"
			}
			results = append(results, checkResult{
				Component: "Prometheus",
				Check:     "Scrape targets",
				Status:    status,
				Detail:    fmt.Sprintf("%d up, %d down", up, down),
			})
		}
	}

	return results
}

func checkAlertManager() []checkResult {
	url := checkFlags.AlertManagerURL + "/-/healthy"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return []checkResult{{
			Component: "AlertManager",
			Check:     "API Health",
			Status:    "fail",
			Detail:    err.Error(),
		}}
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		return []checkResult{{
			Component: "AlertManager",
			Check:     "API Health",
			Status:    "fail",
			Detail:    fmt.Sprintf("status %d", resp.StatusCode),
		}}
	}
	return []checkResult{{
		Component: "AlertManager",
		Check:     "API Health",
		Status:    "ok",
		Detail:    checkFlags.AlertManagerURL,
	}}
}

func checkDatasources() []checkResult {
	gc := grafana.NewClient(checkFlags.GrafanaURL, checkFlags.GrafanaUser, checkFlags.GrafanaPassword)
	datasources, err := gc.ListDatasources()
	if err != nil {
		return []checkResult{{
			Component: "Grafana",
			Check:     "Datasources",
			Status:    "warn",
			Detail:    fmt.Sprintf("could not list datasources: %v", err),
		}}
	}

	var results []checkResult
	for _, ds := range datasources {
		err := gc.CheckDatasourceHealth(ds.ID)
		if err != nil {
			// Fallback: try querying through the datasource proxy for prometheus types
			if ds.Type == "prometheus" {
				err = checkPrometheusDatasourceProxy(gc, ds)
			}
		}
		status := "ok"
		detail := fmt.Sprintf("%s -> %s", ds.Type, ds.URL)
		if err != nil {
			status = "fail"
			detail = fmt.Sprintf("%s -> %s (%v)", ds.Type, ds.URL, err)
		}
		results = append(results, checkResult{
			Component: "Datasource",
			Check:     ds.Name,
			Status:    status,
			Detail:    detail,
		})
	}
	return results
}

func checkPrometheusDatasourceProxy(gc *grafana.Client, ds grafana.APIDatasource) error {
	// Try querying "up" through Grafana's datasource proxy
	url := fmt.Sprintf("%s/api/datasources/proxy/%d/api/v1/query?query=up", gc.BaseURL, ds.ID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	if gc.Username != "" {
		req.SetBasicAuth(gc.Username, gc.Password)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("proxy query failed: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("proxy query returned status %d", resp.StatusCode)
	}
	return nil
}

func checkAlerts() []checkResult {
	pc := prometheus.NewClient(checkFlags.PrometheusURL)
	alerts, err := pc.QueryAlerts()
	if err != nil {
		return []checkResult{{
			Component: "Prometheus",
			Check:     "Alerts",
			Status:    "warn",
			Detail:    fmt.Sprintf("could not query alerts: %v", err),
		}}
	}

	firing := 0
	pending := 0
	for _, a := range alerts {
		switch a.State {
		case "firing":
			firing++
		case "pending":
			pending++
		}
	}

	if firing == 0 && pending == 0 {
		return []checkResult{{
			Component: "Prometheus",
			Check:     "Alerts",
			Status:    "ok",
			Detail:    "no alerts firing",
		}}
	}

	status := "ok"
	if firing > 0 {
		status = "warn"
	}

	results := []checkResult{{
		Component: "Prometheus",
		Check:     "Alerts",
		Status:    status,
		Detail:    fmt.Sprintf("%d firing, %d pending", firing, pending),
	}}

	// List firing alert names
	if firing > 0 {
		var names []string
		for _, a := range alerts {
			if a.State == "firing" {
				name := a.Labels["alertname"]
				if name == "" {
					name = "unnamed"
				}
				names = append(names, name)
			}
		}
		// Deduplicate
		seen := map[string]int{}
		for _, n := range names {
			seen[n]++
		}
		var summary []string
		for n, c := range seen {
			if c > 1 {
				summary = append(summary, fmt.Sprintf("%s (x%d)", n, c))
			} else {
				summary = append(summary, n)
			}
		}
		results = append(results, checkResult{
			Component: "Prometheus",
			Check:     "Firing alerts",
			Status:    "warn",
			Detail:    strings.Join(summary, ", "),
		})
	}

	return results
}

func printResults(results []checkResult) {
	fmt.Printf("%-14s %-20s %-6s %s\n", "COMPONENT", "CHECK", "STATUS", "DETAIL")
	fmt.Println(strings.Repeat("-", 80))
	for _, r := range results {
		marker := "  "
		switch r.Status {
		case "ok":
			marker = "OK"
		case "warn":
			marker = "!!"
		case "fail":
			marker = "XX"
		}
		fmt.Printf("%-14s %-20s [%s]   %s\n", r.Component, r.Check, marker, r.Detail)
	}
	fmt.Println()
}

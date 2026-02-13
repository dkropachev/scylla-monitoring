package prometheus

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is an HTTP client for the Prometheus API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient creates a new Prometheus API client.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Health checks Prometheus readiness.
func (c *Client) Health() error {
	resp, err := c.HTTP.Get(c.BaseURL + "/-/ready")
	if err != nil {
		return fmt.Errorf("prometheus health check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("prometheus not ready: status %d", resp.StatusCode)
	}
	return nil
}

// Reload triggers a hot-reload of the Prometheus configuration.
// Requires --web.enable-lifecycle flag on Prometheus.
func (c *Client) Reload() error {
	resp, err := c.HTTP.Post(c.BaseURL+"/-/reload", "", nil)
	if err != nil {
		return fmt.Errorf("prometheus reload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("prometheus reload failed (status %d): %s", resp.StatusCode, body)
	}
	return nil
}

// Alert represents a Prometheus alert.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"` // "firing", "pending", "inactive"
	ActiveAt    string            `json:"activeAt,omitempty"`
	Value       string            `json:"value,omitempty"`
}

// AlertsResponse is the response from the alerts API.
type AlertsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Alerts []Alert `json:"alerts"`
	} `json:"data"`
}

// QueryAlerts returns all active alerts from Prometheus.
func (c *Client) QueryAlerts() ([]Alert, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/v1/alerts")
	if err != nil {
		return nil, fmt.Errorf("querying alerts: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading alerts response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("alerts query failed (status %d): %s", resp.StatusCode, body)
	}

	var result AlertsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing alerts response: %w", err)
	}

	return result.Data.Alerts, nil
}

// QueryInstant runs an instant PromQL query and returns the raw result.
func (c *Client) QueryInstant(query string) (json.RawMessage, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/v1/query?query=" + query)
	if err != nil {
		return nil, fmt.Errorf("instant query: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading query response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("query failed (status %d): %s", resp.StatusCode, body)
	}

	var result struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing query response: %w", err)
	}
	if result.Status != "success" {
		return nil, fmt.Errorf("query returned status: %s", result.Status)
	}

	return result.Data, nil
}

// TargetGroup represents a file_sd target group.
type TargetGroup struct {
	Targets []string          `json:"targets" yaml:"targets"`
	Labels  map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// QueryTargetGroups queries /api/v1/targets and reconstructs file_sd target groups
// grouped by the source filepath. Returns a map of filepath → []TargetGroup.
func (c *Client) QueryTargetGroups() (map[string][]TargetGroup, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/v1/targets")
	if err != nil {
		return nil, fmt.Errorf("querying targets: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading targets response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("targets query failed (status %d): %s", resp.StatusCode, body)
	}

	var result struct {
		Data struct {
			ActiveTargets []struct {
				DiscoveredLabels map[string]string `json:"discoveredLabels"`
				Labels           map[string]string `json:"labels"`
				ScrapePool       string            `json:"scrapePool"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing targets response: %w", err)
	}

	// Group by __meta_filepath
	fileTargets := make(map[string][]TargetGroup)
	for _, t := range result.Data.ActiveTargets {
		fp := t.DiscoveredLabels["__meta_filepath"]
		if fp == "" {
			continue // static_configs target, skip
		}
		addr := t.DiscoveredLabels["__address__"]
		if addr == "" {
			continue
		}

		// Collect non-internal labels from discoveredLabels
		labels := make(map[string]string)
		for k, v := range t.DiscoveredLabels {
			if k == "__address__" || k == "__meta_filepath" || k == "__metrics_path__" || k == "__scheme__" || k == "__scrape_interval__" || k == "__scrape_timeout__" {
				continue
			}
			labels[k] = v
		}

		fileTargets[fp] = append(fileTargets[fp], TargetGroup{
			Targets: []string{addr},
			Labels:  labels,
		})
	}

	return fileTargets, nil
}

// SnapshotResponse is the response from the snapshot API.
type SnapshotResponse struct {
	Status string `json:"status"`
	Data   struct {
		Name string `json:"name"`
	} `json:"data"`
}

// CreateSnapshot creates a TSDB snapshot and returns the snapshot name.
// Requires --web.enable-admin-api flag on Prometheus.
func (c *Client) CreateSnapshot() (string, error) {
	resp, err := c.HTTP.Post(c.BaseURL+"/api/v1/admin/tsdb/snapshot", "", nil)
	if err != nil {
		return "", fmt.Errorf("creating snapshot: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading snapshot response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("snapshot failed (status %d): %s", resp.StatusCode, body)
	}

	var result SnapshotResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing snapshot response: %w", err)
	}

	if result.Status != "success" {
		return "", fmt.Errorf("snapshot returned status: %s", result.Status)
	}

	return result.Data.Name, nil
}

package main

import "embed"

//go:embed assets/grafana/types.json
var typesJSON []byte //nolint:unused // wired in a follow-up

//go:embed assets/grafana/*.template.json
var dashboardTemplates embed.FS //nolint:unused // wired in a follow-up

//go:embed assets/prometheus/prometheus.yml.template
var prometheusTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/prometheus/prometheus.consul.yml.template
var prometheusConsulTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/prometheus/prom_rules/*.yml
var alertRules embed.FS //nolint:unused // wired in a follow-up

//go:embed assets/grafana/datasource.yml
var datasourceTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/grafana/datasource.loki.yml
var datasourceLokiTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/grafana/datasource.scylla.yml
var datasourceScyllaTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/grafana/load.yaml
var loadTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/loki/conf/loki-config.template.yaml
var lokiConfigTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/loki/promtail/promtail_config.template.yml
var promtailConfigTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/alertmanager/rule_config.yml
var alertmanagerDefaultConfig []byte //nolint:unused // wired in a follow-up

//go:embed assets/docker-compose.template.yml
var composeTemplate []byte //nolint:unused // wired in a follow-up

//go:embed assets/versions.yaml
var versionsData []byte //nolint:unused // wired in a follow-up

# pint configuration. Docs: https://cloudflare.github.io/pint/
#
# pint lints Prometheus recording/alerting rules. We point it at the chart's
# rendered PrometheusRule manifest (kind: PrometheusRule), which wraps the rule
# groups under spec.groups. pint does not parse that Kubernetes wrapper by
# default, so the relaxed parser is enabled to find the rules nested inside it.
#
# CI runs `pint --offline lint`, so only offline checks run (PromQL syntax,
# alert comparison/template, label/annotation validation, ...); no Prometheus
# server is queried.
parser {
  relaxed = [".*"]
}

# Prometheus metrics endpoint should return k8s-httpcache metrics.
GET http://localhost:9101/metrics
HTTP 200
[Asserts]
body contains "k8s_httpcache_varnishd_up 1"
body contains "k8s_httpcache_endpoints{"
body contains "k8s_httpcache_vcl_reloads_total{"
body contains "k8s_httpcache_endpoint_updates_total{"
body contains "k8s_httpcache_broadcast_fanout_targets "
body contains "k8s_httpcache_build_info{"

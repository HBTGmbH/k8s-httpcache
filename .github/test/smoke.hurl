# Readiness endpoint should return 200.
GET http://localhost:8080/ready
HTTP 200

# Backend request via /backend/ should return 200 with body "ok".
# Also verify that values from both --values (ConfigMap) and --values-dir (mounted directory)
# are available in the VCL template.
GET http://localhost:8080/backend/
HTTP 200
[Asserts]
body == "ok\n"
header "X-Values-Test" == "hello-from-values"
header "X-Values-Nested" == "example.com"
header "X-ValuesDir-Test" == "hello-from-dir"
header "X-ValuesDir-Nested" == "dir.example.com"

# Backend with explicit numeric port.
GET http://localhost:8080/backend-port/
HTTP 200
[Asserts]
body == "ok\n"

# Backend with named port.
GET http://localhost:8080/backend-named/
HTTP 200
[Asserts]
body == "ok\n"

# Backend in a different namespace.
GET http://localhost:8080/backend-xns/
HTTP 200
[Asserts]
body == "ok-xns\n"

# ExternalName backend.
GET http://localhost:8080/backend-ext/
HTTP 200
[Asserts]
body == "ok\n"

# Shard consistency: the same URL should always be routed to the same Varnish pod.
# X-Shard-Owner is set from req.http.x-shard (the shard-selected backend) in vcl_deliver,
# so it is always a single header regardless of Varnish version.
GET http://localhost:8080/backend/shard-test
HTTP 200
[Captures]
shard_owner: header "X-Shard-Owner"

GET http://localhost:8080/backend/shard-test
[Options]
repeat: 500
HTTP 200
[Asserts]
header "X-Shard-Owner" == "{{shard_owner}}"

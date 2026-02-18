# Readiness endpoint should return 200.
GET http://localhost:8080/ready
HTTP 200

# Backend request via /nodejs/ should return 200 with body "ok".
GET http://localhost:8080/backend/
HTTP 200
[Asserts]
body == "ok\n"

# Shard consistency: the same URL should always be routed to the same Varnish pod.
# The first Via entry identifies the shard-selected pod and should be consistent.
GET http://localhost:8080/backend/shard-test
HTTP 200
[Captures]
via_shard: header "Via" regex "(1\\.1 [^,]+)"

GET http://localhost:8080/backend/shard-test
[Options]
repeat: 500
HTTP 200
[Asserts]
header "Via" startsWith "{{via_shard}}"


# Readiness endpoint should return 200.
GET http://localhost:8080/ready
HTTP 200

# Backend request via /backend/ should return 200 with body "ok".
GET http://localhost:8080/backend/
HTTP 200
[Asserts]
body == "ok\n"

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

# Via header must contain at most two entries, ensuring requests are never
# forwarded more than once (no routing loops in the shard cluster).
# Random UUIDs spread requests across different shard owners; some are served
# directly (1 Via entry), others are forwarded once to the owning shard (2 entries).
GET http://localhost:8080/backend/{{newUuid}}
[Options]
repeat: 100
HTTP 200
[Asserts]
header "Via" matches "^[^,]+(, [^,]+)?$"


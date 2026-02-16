# Readiness endpoint should return 200.
GET http://localhost:8080/ready
HTTP 200

# Backend request via /nodejs/ should return 200 with body "ok".
GET http://localhost:8080/nodejs/
HTTP 200
[Asserts]
body == "ok"

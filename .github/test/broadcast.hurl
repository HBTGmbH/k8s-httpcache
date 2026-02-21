# Broadcast should fan out to all 3 Varnish pods and return aggregated JSON.
GET http://localhost:8088/backend/
HTTP 200
[Asserts]
header "Content-Type" contains "application/json"
jsonpath "$.*" count == 3
jsonpath "$.*.body" contains "ok\n"

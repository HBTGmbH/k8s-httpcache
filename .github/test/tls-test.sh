#!/usr/bin/env bash
# Native Varnish 9 TLS e2e test. Verifies that:
#   1. an HTTPS listener serves the initial certificate,
#   2. rotating the TLS Secret is picked up live (new certificate served, no
#      restart), and
#   3. loading an erroneous certificate is rejected while the previously active
#      certificate keeps serving (fallback).
#
# TLS is a Varnish 9+ feature, so this test self-skips on older versions and is
# gated to the varnish9 matrix leg in CI. Requires: kubectl, curl, openssl.
set -eu

cd "$(dirname "$0")/../.."

HTTPS_PORT=18443
METRICS_PORT=9105
SECRET=k8s-httpcache-tls-cert
NS=default
MANIFEST=.github/test/manifest-tls.yaml

certdir=""
pf_pids=()

cleanup() {
  if [ "${#pf_pids[@]}" -gt 0 ]; then
    kill "${pf_pids[@]}" 2>/dev/null || true
  fi
  kubectl delete -f "$MANIFEST" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete secret "$SECRET" -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  [ -n "$certdir" ] && rm -rf "$certdir"
}
trap cleanup EXIT

# --- Skip on Varnish < 9 -----------------------------------------------------
# TLS is a Varnish 9+ feature. The CI matrix already gates this step to the
# varnish9 leg; this guard additionally lets test-all.sh run the script on any
# version. The cache version is read from the main k8s-httpcache deployment
# (deployed by manifest.yaml), so detection is made robust against transient
# unreadiness left by the preceding rollout/drain tests.

if ! kubectl get deployment/k8s-httpcache >/dev/null 2>&1; then
  echo "SKIP: k8s-httpcache deployment not found (apply manifest.yaml first)"
  exit 0
fi
kubectl rollout status deployment/k8s-httpcache --timeout=120s >/dev/null 2>&1 || true

version_output=""
for _ in $(seq 1 10); do
  version_output=$(kubectl exec deploy/k8s-httpcache -- /usr/sbin/varnishd -V 2>&1 || true)
  printf '%s\n' "$version_output" | grep -qE '(varnish|vinyl)-[0-9]+' && break
  sleep 2
done
major=$(printf '%s\n' "$version_output" | grep -oE '(varnish|vinyl)-[0-9]+' | head -n1 | grep -oE '[0-9]+' || true)
if [ -z "$major" ]; then
  echo "SKIP: could not determine cache version from deployment/k8s-httpcache"
  exit 0
fi
if [ "$major" -lt 9 ]; then
  echo "SKIP: native TLS requires Varnish 9+ (detected major $major)"
  exit 0
fi
echo "Detected Varnish major version $major"

# --- Generate certificates ---------------------------------------------------

certdir=$(mktemp -d)

gen_cert() {
  # gen_cert <prefix> <serial>
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
    -keyout "$certdir/$1.key" -out "$certdir/$1.crt" -days 1 -nodes \
    -subj "/CN=tls.local" \
    -addext "subjectAltName=DNS:tls.local,DNS:localhost" \
    -set_serial "$2" >/dev/null 2>&1
}

cert_serial() { openssl x509 -in "$1" -noout -serial | cut -d= -f2; }

gen_cert cert1 4097
gen_cert cert2 4098
cert1_serial=$(cert_serial "$certdir/cert1.crt")
cert2_serial=$(cert_serial "$certdir/cert2.crt")
echo "Generated certificates: cert1 serial=$cert1_serial cert2 serial=$cert2_serial"

# --- Deploy with the initial certificate -------------------------------------

echo "--- Creating initial TLS Secret and deploying ---"
kubectl create secret tls "$SECRET" -n "$NS" \
  --cert="$certdir/cert1.crt" --key="$certdir/cert1.key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$MANIFEST" >/dev/null
kubectl rollout status deployment/k8s-httpcache-tls --timeout=120s

# --- Port-forwards -----------------------------------------------------------

kubectl port-forward svc/k8s-httpcache-tls "$HTTPS_PORT:8443" >/dev/null 2>&1 &
pf_pids+=("$!")
kubectl port-forward svc/k8s-httpcache-tls "$METRICS_PORT:9101" >/dev/null 2>&1 &
pf_pids+=("$!")

for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:$METRICS_PORT/metrics" >/dev/null 2>&1 && break
  sleep 1
done

# --- Helpers -----------------------------------------------------------------

metric_value() {
  curl -sf "http://127.0.0.1:$METRICS_PORT/metrics" \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
}

# served_serial returns the serial of the certificate Varnish presents, sending
# SNI=tls.local. It retries briefly so a transient handshake hiccup over the
# port-forward does not cause a spurious empty result.
served_serial() {
  local out=""
  for _ in $(seq 1 5); do
    out=$(echo | openssl s_client -connect "127.0.0.1:$HTTPS_PORT" -servername tls.local 2>/dev/null \
      | openssl x509 -noout -serial 2>/dev/null | cut -d= -f2)
    [ -n "$out" ] && break
    sleep 1
  done
  printf '%s' "$out"
}

# https_code uses --resolve so curl sends SNI=tls.local (curl omits SNI for bare
# IPs), keeping the request consistent with served_serial's handshake.
https_code() {
  curl -sk -o /dev/null -w '%{http_code}' \
    --resolve "tls.local:$HTTPS_PORT:127.0.0.1" \
    "https://tls.local:$HTTPS_PORT/backend/" || true
}

# --- Test 1: initial certificate serves over HTTPS ---------------------------

echo "--- Test 1: initial certificate ---"
ok=false
for _ in $(seq 1 30); do
  [ "$(https_code)" = "200" ] && { ok=true; break; }
  sleep 1
done
[ "$ok" = "true" ] || { echo "FAIL: HTTPS /backend/ never returned 200"; exit 1; }
echo "PASS: HTTPS /backend/ -> 200"

serial=$(served_serial)
[ "$serial" = "$cert1_serial" ] || { echo "FAIL: served serial=$serial want cert1=$cert1_serial"; exit 1; }
echo "PASS: serving initial certificate (serial=$serial)"

active=$(metric_value 'k8s_httpcache_tls_certs_active')
[ "$active" -ge 1 ] || { echo "FAIL: tls_certs_active=$active want >=1"; exit 1; }
echo "PASS: tls_certs_active=$active"

# --- Test 2: certificate rotation --------------------------------------------

echo "--- Test 2: certificate rotation ---"
before_ok=$(metric_value 'k8s_httpcache_tls_cert_reloads_total{cert="frontend",result="success"}')
before_upd=$(metric_value 'k8s_httpcache_tls_cert_updates_total{cert="frontend"}')

kubectl create secret tls "$SECRET" -n "$NS" \
  --cert="$certdir/cert2.crt" --key="$certdir/cert2.key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

rotated=false
for i in $(seq 1 30); do
  if [ "$(served_serial)" = "$cert2_serial" ]; then
    echo "PASS: serving rotated certificate (serial=$cert2_serial, attempt $i)"
    rotated=true
    break
  fi
  sleep 1
done
[ "$rotated" = "true" ] || { echo "FAIL: rotated certificate not served within 30s"; exit 1; }
[ "$(https_code)" = "200" ] || { echo "FAIL: HTTPS broken after rotation"; exit 1; }

after_ok=$(metric_value 'k8s_httpcache_tls_cert_reloads_total{cert="frontend",result="success"}')
after_upd=$(metric_value 'k8s_httpcache_tls_cert_updates_total{cert="frontend"}')
[ "$((after_ok - before_ok))" -ge 1 ] || { echo "FAIL: tls_cert_reloads_total{result=success} did not increase"; exit 1; }
[ "$((after_upd - before_upd))" -ge 1 ] || { echo "FAIL: tls_cert_updates_total did not increase"; exit 1; }
echo "PASS: rotation metrics increased (success +$((after_ok - before_ok)), updates +$((after_upd - before_upd)))"

# --- Test 3: erroneous certificate -> fallback to active certificate ---------

echo "--- Test 3: erroneous certificate, fallback to active ---"
before_err=$(metric_value 'k8s_httpcache_tls_cert_reloads_total{cert="frontend",result="error"}')

# Malformed tls.crt paired with a valid key. Applied as raw YAML to bypass the
# client-side cert/key pairing validation that `kubectl create secret tls` does;
# the apiserver only checks presence, the controller rejects it at load time.
broken_crt=$(printf -- '-----BEGIN CERTIFICATE-----\nbroken\n-----END CERTIFICATE-----\n' | base64 | tr -d '\n')
good_key=$(base64 < "$certdir/cert2.key" | tr -d '\n')
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Secret
metadata:
  name: $SECRET
  namespace: $NS
type: kubernetes.io/tls
data:
  tls.crt: $broken_crt
  tls.key: $good_key
EOF

errored=false
for _ in $(seq 1 30); do
  after_err=$(metric_value 'k8s_httpcache_tls_cert_reloads_total{cert="frontend",result="error"}')
  if [ "$((after_err - before_err))" -ge 1 ]; then
    errored=true
    break
  fi
  sleep 1
done
[ "$errored" = "true" ] || { echo "FAIL: tls_cert_reloads_total{result=error} did not increase"; exit 1; }
echo "PASS: erroneous certificate rejected (error +$((after_err - before_err)))"

# The previously active certificate (cert2) must still be served.
serial=$(served_serial)
[ "$serial" = "$cert2_serial" ] || { echo "FAIL: expected fallback to cert2 ($cert2_serial), served '$serial'"; exit 1; }
[ "$(https_code)" = "200" ] || { echo "FAIL: HTTPS broken after bad cert load"; exit 1; }
echo "PASS: still serving previous certificate after bad load (serial=$serial)"

echo "All TLS tests passed."

#!/usr/bin/env bash
# Shard distribution test: verify that different URL paths are routed
# to different Varnish shard pods.
set -eu

N=200
for i in $(seq 1 $N); do
  curl -sf -D- -o /dev/null "http://localhost:8080/backend/shard-test/$i" 2>/dev/null \
    | grep -i '^x-shard-owner:' | awk '{print $2}' | tr -d '\r'
done > /tmp/shard-pods.txt

echo "Shard pod distribution:"
sort /tmp/shard-pods.txt | uniq -c | sort -rn

unique=$(sort -u /tmp/shard-pods.txt | wc -l)
if [ "$unique" -ne 3 ]; then
  echo "FAIL: expected 3 unique shard pods, got $unique"
  exit 1
fi
echo "PASS: $unique unique shard pods seen across $N different URLs"

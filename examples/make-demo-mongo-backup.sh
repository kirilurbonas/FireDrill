#!/usr/bin/env bash
# Produce a realistic MongoDB demo backup (gzipped mongodump archive) for
# `firedrill run shop-mongo -f examples/firedrill-mongodb.yaml`.
#
# Needs Docker. Nothing is installed on the host — mongodump runs in the
# container that holds the seed data, and the container is removed afterwards.
set -euo pipefail

image="${MONGO_IMAGE:-mongo:8}"
name="firedrill-demo-mongo-src"
out="$(dirname "$0")/demo-mongo.archive.gz"

cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "▸ starting $image"
docker run -d --name "$name" \
  -e MONGO_INITDB_ROOT_USERNAME=demo -e MONGO_INITDB_ROOT_PASSWORD=demo \
  "$image" >/dev/null

echo "▸ waiting for mongodb"
for _ in $(seq 1 90); do
  if docker exec "$name" mongosh --quiet -u demo -p demo --authenticationDatabase admin \
      --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1; then
    ready=1; break
  fi
  sleep 1
done
[ "${ready:-}" = 1 ] || { echo "mongodb never became ready" >&2; exit 1; }

echo "▸ seeding shop data + ransomware canary"
docker exec "$name" mongosh --quiet -u demo -p demo --authenticationDatabase admin --eval '
  const d = db.getSiblingDB("shop");
  d.ledger.insertMany(Array.from({length: 5000}, (_, i) => ({
    id: i + 1, amount: (i * 37) % 9999, status: i % 3 ? "active" : "closed",
  })));
  d.accounts.insertMany(Array.from({length: 250}, (_, i) => ({
    id: i + 1, name: "customer-" + i, status: i % 5 ? "active" : "suspended",
  })));
  d.firedrill_canary.insertOne({token: "fd-canary-2f8a91c4"});
  print("seeded " + d.ledger.countDocuments({}) + " ledger documents");
'

echo "▸ dumping to $out"
docker exec "$name" mongodump -u demo -p demo --authenticationDatabase admin \
  --db shop --archive --gzip > "$out"

echo "✔ $out ($(du -h "$out" | cut -f1))"
echo "  firedrill run shop-mongo -f examples/firedrill-mongodb.yaml"

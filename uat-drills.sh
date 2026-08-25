#!/usr/bin/env bash
# UAT: exercise the RELEASED firedrill binary against real engines, the way a
# user following the README would. Every step prints PASS/FAIL and the run
# ends non-zero if anything failed.
set -uo pipefail
fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails+1)); }
check() { if [ "$1" = "$2" ]; then pass "$3 (exit $1)"; else fail "$3 (exit $1, want $2)"; fi; }
# stat(1) differs between GNU and BSD; the UAT runs on both.
filemode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
filesize() { stat -c %s "$1" 2>/dev/null || stat -f %z "$1"; }

FD=$(command -v firedrill) || {
  echo "FATAL: firedrill is not installed — the UAT cannot report on a binary that is not there." >&2
  exit 2
}
echo "== binary: $FD ($(firedrill --version))"
cd "$(mktemp -d)" || exit 2
work=$PWD
echo "== workdir: $work"

firedrill keygen --dir "$work/keys" >/dev/null || fail "keygen"
[ -f "$work/keys/firedrill.key" ] && [ -f "$work/keys/firedrill.pub" ] && [ -f "$work/keys/firedrill.cosign.pub" ] \
  && pass "keygen wrote key, pub and cosign pub" || fail "keygen outputs missing"
perms=$(filemode "$work/keys/firedrill.key")
[ "$perms" = "600" ] && pass "private key mode 600" || fail "private key mode $perms"

########## 1. Postgres logical dump, gzipped, discovered by prefix ##########
mkdir -p backups
docker rm -f uat-pg-src >/dev/null 2>&1
docker run -d --name uat-pg-src -e POSTGRES_PASSWORD=src -e POSTGRES_DB=payments postgres:16.10-alpine >/dev/null
# pg_isready passes during the entrypoint's init-phase restart, so poll a real
# query (twice) the way FireDrill's own driver does.
for _ in $(seq 1 90); do
  docker exec uat-pg-src psql -U postgres -d payments -c 'select 1' >/dev/null 2>&1 && sleep 2 &&
    docker exec uat-pg-src psql -U postgres -d payments -c 'select 1' >/dev/null 2>&1 && break
  sleep 1
done
docker exec uat-pg-src psql -U postgres -d payments -v ON_ERROR_STOP=1 -c "
  create table ledger (id bigserial primary key, amount bigint not null);
  insert into ledger (amount) select g from generate_series(1, 25000) g;
  create table accounts (id bigserial primary key, status text);
  insert into accounts (status) select case when g%3=0 then 'closed' else 'active' end from generate_series(1,500) g;
  create table firedrill_canary (token text);
  insert into firedrill_canary values ('fd-canary-uat');" >/dev/null || fail "seeding postgres"
# Yesterday's smaller backup and today's real one — as a nightly pipeline leaves them.
docker exec uat-pg-src pg_dump -U postgres -d payments -Fc -t firedrill_canary | gzip > backups/payments-2026-08-21.dump.gz
docker exec uat-pg-src pg_dump -U postgres -d payments -Fc | gzip > backups/payments-2026-08-22.dump.gz
docker exec uat-pg-src pg_dump -U postgres -d payments -Fc | gzip > backups/orders-2026-08-22.dump.gz
touch -d '2026-08-21 03:00' backups/payments-2026-08-21.dump.gz
touch -d '2026-08-22 03:00' backups/orders-2026-08-22.dump.gz   # newest overall, wrong drill
sleep 1
rows=$(docker exec uat-pg-src psql -U postgres -d payments -tAc 'select count(*) from ledger' 2>/dev/null)
[ "$rows" = "25000" ] && pass "seeded postgres with $rows ledger rows" || fail "postgres seed has '$rows' rows, want 25000"
# Compare against yesterday's canary-only dump rather than an absolute size:
# 25k integer rows gzip down to ~14 KB, so a fixed threshold proves nothing.
sz=$(filesize backups/payments-2026-08-22.dump.gz)
small=$(filesize backups/payments-2026-08-21.dump.gz)
[ "$sz" -gt "$((small * 3))" ] && pass "today's dump (${sz}B) is materially bigger than yesterday's canary-only one (${small}B)" \
  || fail "today's dump ${sz}B vs yesterday's ${small}B — seeding or pg_dump failed"

cat > firedrill.yaml <<YAML
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: payments-db }
spec:
  objectives: { rto: 15m, rpo: 24h }
  source:
    driver: postgres
    from:
      type: file
      uri: $work/backups
      select: latest
      match: "payments-*.dump.gz"
  sandbox: { provider: docker, image: postgres:16.10-alpine, ttl: 20m }
  verify:
    - restoreSucceeded: {}
    - freshness: { maxAge: 24h }
    - rowCount: { query: "select count(*) from ledger", min: 25000 }
    - checksum: { table: ledger, column: id }
    - smoke: { sql: "select 1 from accounts where status='active' limit 1", expectRows: ">=1" }
    - canary: { sql: "select token from firedrill_canary", expect: "fd-canary-uat" }
  report:
    sign: true
    html: true
    controls: [ISO27001-A.8.13, SOC2-A1.2]
    sinks:
      - { type: prometheus, textfileDir: $work/metrics }
      - { type: webhook, urlEnv: UAT_WEBHOOK }
YAML
mkdir -p metrics
# A real receiver for the webhook sink.
python3 - <<'PY' &
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers['Content-Length']))
        open('/tmp/uat-webhook.json','wb').write(body)
        open('/tmp/uat-webhook-headers.txt','w').write(
            f"{self.headers.get('X-FireDrill-Event')} {self.headers.get('X-FireDrill-Drill')} {self.headers.get('Content-Type')}")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', 18099), H).serve_forever()
PY
sleep 2
export UAT_WEBHOOK=http://127.0.0.1:18099/hook

firedrill validate -f firedrill.yaml >/dev/null; check $? 0 "validate the drill spec"
firedrill run payments-db -f firedrill.yaml --key-dir "$work/keys" --no-color | tee run1.log
check ${PIPESTATUS[0]} 0 "postgres drill (gzip + select:latest) verifies"

grep -q "payments-2026-08-22.dump.gz (gzip)" run1.log && pass "restored the newest matching backup, decompressed" || fail "wrong artifact or no decompression: $(grep restore run1.log)"
grep -q "25000 rows" run1.log && pass "row count proves the RIGHT dump (25000, not yesterday's canary-only one)" || fail "row count wrong: $(grep rowCount run1.log)"
grep -q "sentinel restored intact" run1.log && pass "canary check" || fail "canary check"
ev=$(ls evidence/payments-db-*.json | head -1)

########## 2. Evidence ##########
firedrill verify-evidence "$ev" --key-dir "$work/keys"; check $? 0 "verify-evidence (signature + attestation)"
# The auditor's situation: a bundle handed to a machine that has never seen
# the signer, and that has an unrelated firedrill key of its own.
firedrill keygen --dir "$work/stranger" >/dev/null
mkdir -p "$work/auditor" && cp "$ev" "$ev.sig" "$ev.intoto.jsonl" "$work/auditor/"
( cd "$work/auditor" && HOME="$work/stranger-home" firedrill verify-evidence "$(basename "$ev")" --key-dir "$work/stranger" ) >auditor.log 2>&1
check $? 0 "evidence verifies on a machine holding a DIFFERENT firedrill key"
grep -q "signer " auditor.log && pass "verdict names the signing key" || fail "verdict does not name the signer: $(cat auditor.log)"
firedrill verify-evidence "$ev" --public-key "$work/keys/firedrill.pub" >/dev/null; check $? 0 "verify-evidence with pinned key"
firedrill keygen --dir "$work/otherkeys2" >/dev/null
firedrill verify-evidence "$ev" --public-key "$work/otherkeys2/firedrill.pub" >/dev/null 2>&1
check $? 2 "verify-evidence REJECTS a pinned key that did not sign it"
if python3 - "$ev" <<'PY'
import json, sys
e = json.load(open(sys.argv[1]))
b = e['backup']
assert b['resolvedUri'].endswith('payments-2026-08-22.dump.gz'), f"resolvedUri={b.get('resolvedUri')}"
assert b['compression'] == 'gzip', b
assert b['uncompressedBytes'] > b['bytes'], b
assert e['verified'] is True
assert e['sandbox']['destroyed'] is True, "written evidence does not record the sandbox as destroyed"
print(f"  resolvedUri recorded, compression={b['compression']}, "
      f"{b['bytes']}B -> {b['uncompressedBytes']}B, sandbox destroyed")
PY
then pass "evidence content (resolved artifact, compression, verdict, teardown)"; else fail "evidence content assertions"; fi
# Tamper detection.
cp "$ev" tampered.json && cp "$ev.sig" tampered.json.sig
python3 -c "
import json,sys; d=json.load(open('tampered.json')); d['measured']['restoreSeconds']=0.1
json.dump(d,open('tampered.json','w'),indent=2)"
firedrill verify-evidence tampered.json >/dev/null 2>&1; check $? 2 "tampered evidence is rejected"
if [ -f "${ev%.json}.html" ] && grep -q "RECOVERY VERIFIED" "${ev%.json}.html"; then
  pass "HTML report written with the verdict"
else
  fail "HTML report missing or lacks the verdict"
fi
[ -f "$ev.intoto.jsonl" ] && pass "in-toto attestation written" || fail "attestation missing"

########## 3. Sinks ##########
grep -q 'firedrill_drill_verified{drill="payments-db"} 1' metrics/firedrill-payments-db.prom \
  && pass "prometheus textfile sink" || fail "prometheus sink: $(ls metrics)"
if [ -f /tmp/uat-webhook.json ] && python3 -c "
import json;d=json.load(open('/tmp/uat-webhook.json'));assert d['drill']=='payments-db'" 2>/dev/null; then
  pass "webhook sink received evidence JSON"
else
  fail "webhook sink got nothing usable"
fi
hdr=$(cat /tmp/uat-webhook-headers.txt 2>/dev/null)
[ "$hdr" = "drill.verified payments-db application/json" ] && pass "webhook routing headers ($hdr)" || fail "webhook headers: $hdr"

########## 4. Failure paths ##########
# Corrupt backup.
head -c 2000 /dev/urandom | gzip > backups/payments-2026-08-23.dump.gz
firedrill run payments-db -f firedrill.yaml --key-dir "$work/keys" --no-color > run-corrupt.log 2>&1
check $? 1 "corrupt backup fails the drill (exit 1)"
grep -q "SKIP" run-corrupt.log && pass "data checks report SKIP, not false PASS" || fail "expected SKIPs: $(tail -5 run-corrupt.log)"
rm backups/payments-2026-08-23.dump.gz
# Ransomware: canary tampered at the source.
docker exec uat-pg-src psql -U postgres -d payments -c "update firedrill_canary set token='ENCRYPTED'" >/dev/null
docker exec uat-pg-src pg_dump -U postgres -d payments -Fc | gzip > backups/payments-2026-08-24.dump.gz
firedrill run payments-db -f firedrill.yaml --key-dir "$work/keys" --no-color > run-ransom.log 2>&1
check $? 1 "ransomware canary mismatch fails the drill"
grep -q "possible ransomware/corruption" run-ransom.log && pass "canary names the failure mode" || fail "canary detail"
grep -q "ENCRYPTED" run-ransom.log evidence/*.json && fail "sentinel value LEAKED into output/evidence" || pass "sentinel never leaked into logs or evidence"
rm backups/payments-2026-08-24.dump.gz
# Put the sentinel back: a simulation that leaves the source tampered
# poisons every backup taken after it.
docker exec uat-pg-src psql -U postgres -d payments -c "update firedrill_canary set token='fd-canary-uat'" >/dev/null
# No backup matches the glob.
sed 's/payments-\*/ledger-*/' firedrill.yaml > nomatch.yaml
firedrill run payments-db -f nomatch.yaml --key-dir "$work/keys" >/dev/null 2>&1
check $? 2 "no matching backup is an execution error (exit 2), not a pass"

########## 4b. Encrypted backups ##########
# What a security-conscious pipeline writes: dump | gzip | age.
if command -v age >/dev/null 2>&1 && command -v age-keygen >/dev/null 2>&1; then
  age-keygen -o "$work/age.key" 2>/dev/null
  recipient=$(age-keygen -y "$work/age.key")
  docker exec uat-pg-src pg_dump -U postgres -d payments -Fc | gzip | age -r "$recipient" > backups/payments-encrypted.dump.gz.age
  sed -e 's|match: "payments-\*.dump.gz"|match: "payments-encrypted.dump.gz.age"|' \
      -e "s|      select: latest|      select: latest\n      decrypt: { type: age, identityFile: $work/age.key }|" \
      -e 's|name: payments-db|name: payments-encrypted|' firedrill.yaml > encrypted.yaml
  firedrill validate -f encrypted.yaml >/dev/null; check $? 0 "encrypted drill spec validates"
  firedrill run payments-encrypted -f encrypted.yaml --key-dir "$work/keys" --no-color > run-enc.log 2>&1
  check $? 0 "encrypted backup (age + gzip) drills clean"
  grep -q "(age, gzip)" run-enc.log && pass "restore line names both layers" || fail "layers not shown: $(grep restore run-enc.log)"
  encev=$(ls evidence/payments-encrypted-*.json | head -1)
  if python3 - "$encev" <<'ENCPY'
import json, sys
e = json.load(open(sys.argv[1]))
b = e['backup']
assert b['encryption'] == 'age', f"encryption={b.get('encryption')}"
assert b['compression'] == 'gzip', f"compression={b.get('compression')}"
assert e['verified'] is True, "encrypted drill did not verify"
ENCPY
  then pass "evidence records the backup was encrypted"; else fail "evidence encryption fields"; fi
  # The key must never end up in evidence or logs.
  if grep -rq "AGE-SECRET-KEY" evidence/ ./*.log 2>/dev/null; then
    fail "age key leaked into evidence or logs"
  else
    pass "decryption key never appears in evidence or logs"
  fi
  # An encrypted backup with no decrypt block must fail with advice.
  sed 's|      decrypt: .*||' encrypted.yaml > nodecrypt.yaml
  firedrill run payments-encrypted -f nodecrypt.yaml --key-dir "$work/keys" >nodecrypt.log 2>&1
  check $? 2 "encrypted backup without a decrypt block is an execution error"
  grep -q "source.from.decrypt" nodecrypt.log && pass "error tells the operator what to add" || fail "unhelpful error: $(cat nodecrypt.log)"
else
  echo "–  age CLI not installed; skipping the encrypted-backup drills"
fi

########## 5. MySQL + MongoDB ##########
docker rm -f uat-my-src >/dev/null 2>&1
docker run -d --name uat-my-src -e MYSQL_ROOT_PASSWORD=src -e MYSQL_DATABASE=orders mysql:8.4 >/dev/null
# mysqladmin ping answers while the entrypoint's temporary init server is up.
for _ in $(seq 1 120); do
  docker exec uat-my-src mysql -uroot -psrc orders -e 'select 1' >/dev/null 2>&1 && sleep 2 &&
    docker exec uat-my-src mysql -uroot -psrc orders -e 'select 1' >/dev/null 2>&1 && break
  sleep 2
done
docker exec uat-my-src mysql -uroot -psrc orders -e "
  create table orders (id bigint primary key auto_increment, amount bigint);
  insert into orders (amount) values (1),(2),(3),(4),(5);" 2>/dev/null || fail "seeding mysql"
myrows=$(docker exec uat-my-src mysql -uroot -psrc orders -sNe 'select count(*) from orders' 2>/dev/null)
[ "$myrows" = "5" ] && pass "seeded mysql with $myrows rows" || fail "mysql seed has '$myrows' rows, want 5"
docker exec uat-my-src mysqldump -uroot -psrc orders 2>/dev/null | zstd -q > backups/orders.sql.zst 2>/dev/null \
  || docker exec uat-my-src mysqldump -uroot -psrc orders 2>/dev/null | gzip > backups/orders.sql.gz
cat > mysql.yaml <<YAML
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: orders-db }
spec:
  objectives: { rto: 15m, rpo: 24h }
  source:
    driver: mysql
    from: { type: file, uri: $work/backups, select: latest, match: "orders.sql.*" }
  sandbox: { provider: docker, image: mysql:8.4, ttl: 20m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from orders", min: 5 }
    - checksum: { table: orders, column: id }
  report: { sign: true, controls: [ISO27001-A.8.13] }
YAML
firedrill run orders-db -f mysql.yaml --key-dir "$work/keys" --no-color > run-mysql.log 2>&1
check $? 0 "mysql drill (compressed, discovered) verifies"

docker rm -f uat-mongo-src >/dev/null 2>&1
docker run -d --name uat-mongo-src -e MONGO_INITDB_ROOT_USERNAME=src -e MONGO_INITDB_ROOT_PASSWORD=src mongo:8 >/dev/null
for _ in $(seq 1 90); do docker exec uat-mongo-src mongosh --quiet -u src -p src --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1 && break; sleep 2; done
docker exec uat-mongo-src mongosh --quiet -u src -p src --authenticationDatabase admin --eval '
  const d = db.getSiblingDB("shop");
  d.ledger.insertMany(Array.from({length: 1200}, (_, i) => ({id: i+1, status: i%2 ? "active":"closed"})));
  d.firedrill_canary.insertOne({token: "fd-canary-uat"});' >/dev/null || fail "seeding mongo"
mrows=$(docker exec uat-mongo-src mongosh --quiet -u src -p src --authenticationDatabase admin \
  --eval 'print(db.getSiblingDB("shop").ledger.countDocuments({}))' 2>/dev/null | tail -1)
[ "$mrows" = "1200" ] && pass "seeded mongodb with $mrows documents" || fail "mongo seed has '$mrows' docs, want 1200"
docker exec uat-mongo-src mongodump -u src -p src --authenticationDatabase admin --db shop --archive --gzip > backups/shop.archive.gz
cat > mongo.yaml <<YAML
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: shop-mongo }
spec:
  objectives: { rto: 15m, rpo: 24h }
  source:
    driver: mongodb
    format: archive
    database: shop
    from: { type: file, uri: $work/backups/shop.archive.gz }
  sandbox: { provider: docker, image: mongo:8, ttl: 20m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "db.ledger.countDocuments({})", min: 1200 }
    - checksum: { table: ledger, column: id }
    - smoke: { query: "db.ledger.find({status: 'active'}).limit(4)", expectRows: "==4" }
    - canary: { query: "db.firedrill_canary.findOne().token", expect: "fd-canary-uat" }
  report: { sign: true, controls: [SOC2-A1.2] }
YAML
firedrill run shop-mongo -f mongo.yaml --key-dir "$work/keys" --no-color > run-mongo.log 2>&1
check $? 0 "mongodb drill verifies"

########## 6. Fleet scorecard ##########
cat firedrill.yaml > fleet.yaml; echo "---" >> fleet.yaml; cat mongo.yaml >> fleet.yaml
firedrill run --all -f fleet.yaml --key-dir "$work/keys" --no-color > run-all.log 2>&1
check $? 0 "run --all fleet scorecard"
grep -q "2 drill(s): 2 verified" run-all.log && pass "scorecard totals" || fail "scorecard: $(tail -3 run-all.log)"

########## 7. Reporting: history, controls, gate ##########
firedrill history --evidence-dir evidence > history.log 2>&1; check $? 0 "firedrill history"
firedrill controls --evidence-dir evidence > controls.md 2>&1; check $? 0 "firedrill controls (markdown)"
grep -q "ISO27001-A.8.13" controls.md && pass "controls matrix includes declared control" || fail "controls matrix"
firedrill controls --evidence-dir evidence --format json -o controls.json >/dev/null 2>&1; check $? 0 "controls JSON"

firedrill gate --from-spec fleet.yaml --evidence-dir evidence --max-age 24h >gate1.log 2>&1
check $? 0 "gate passes for freshly drilled fleet"
firedrill gate --from-spec fleet.yaml --evidence-dir evidence --max-age 24h --public-key "$work/keys/firedrill.pub" >/dev/null 2>&1
check $? 0 "gate with pinned signing key"
firedrill gate --drill payments-db --drill ledger-db --evidence-dir evidence --max-age 24h >gate2.log 2>&1
check $? 1 "gate FAILS for a drill that never ran"
grep -q "no evidence" gate2.log && pass "gate names the missing drill" || fail "gate message: $(cat gate2.log)"
firedrill gate --evidence-dir evidence --max-age 1s >/dev/null 2>&1; check $? 1 "gate fails on stale evidence"
firedrill gate --by control --control ISO27001-A.8.13 --evidence-dir evidence --max-age 24h >/dev/null 2>&1
check $? 0 "gate by control"
firedrill gate --evidence-dir evidence --format json >gate.json 2>/dev/null
python3 -c "
import json; d=json.load(open('gate.json')); print('PASS  gate --format json parses,', len(d['subjects']), 'subjects')" || fail "gate json"
# Wrong key must fail the gate.
firedrill keygen --dir "$work/otherkeys" >/dev/null
firedrill gate --from-spec fleet.yaml --evidence-dir evidence --public-key "$work/otherkeys/firedrill.pub" >/dev/null 2>&1
check $? 1 "gate rejects evidence signed by a different key"

########## 8. Sandbox hygiene ##########
leaked=$(docker ps -a --filter "label=firedrill" --format '{{.Names}}' | wc -l | tr -d ' ')
[ "$leaked" = "0" ] && pass "no sandboxes leaked (all destroyed)" || fail "$leaked sandbox(es) left behind"
nets=$(docker network ls --filter "name=firedrill-" -q | wc -l | tr -d ' ')
[ "$nets" = "0" ] && pass "no sandbox networks leaked" || fail "$nets network(s) left behind"
firedrill gc --dry-run >/dev/null 2>&1; check $? 0 "firedrill gc runs clean"

docker rm -f uat-pg-src uat-my-src uat-mongo-src >/dev/null 2>&1
if [ -n "${GITHUB_WORKSPACE:-}" ]; then
  cp -r evidence "$GITHUB_WORKSPACE/uat-evidence" 2>/dev/null || true
  cp ./*.log controls.md "$GITHUB_WORKSPACE/uat-evidence/" 2>/dev/null || true
fi

echo
echo "================ UAT SUMMARY: $fails failure(s) ================"
exit $((fails > 0))

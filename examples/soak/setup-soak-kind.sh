#!/usr/bin/env bash
# Deploys the PUBLISHED firedrill operator image into the current kind
# cluster with PVC-backed evidence, an in-cluster MinIO holding a seeded
# pg_dump, and two scheduled soak drills (one passing, one failing).
# Used to validate v1.0 readiness: live leader election + unattended soak.
set -euo pipefail
cd "$(dirname "$0")/../.."

IMAGE="${FIREDRILL_IMAGE:-ghcr.io/kirilurbonas/firedrill:0.10.0}"

echo "▸ applying CRD + operator (image $IMAGE, PVC evidence)…"
kubectl apply -f deploy/crd.yaml
# Enable the PVC variant and pin the image.
python3 - "$IMAGE" <<'PY'
import re, subprocess, sys
y = open('deploy/operator.yaml').read()
y = y.replace('image: ghcr.io/kirilurbonas/firedrill:latest', f'image: {sys.argv[1]}')
# swap emptyDir evidence for the PVC (uncomment the documented block)
y = y.replace("""        - name: evidence
          emptyDir: {}
        # - name: evidence
        #   persistentVolumeClaim:
        #     claimName: firedrill-evidence""",
"""        - name: evidence
          persistentVolumeClaim:
            claimName: firedrill-evidence""")
y = y.replace("""# ---
# apiVersion: v1
# kind: PersistentVolumeClaim
# metadata:
#   name: firedrill-evidence
#   namespace: firedrill-system
# spec:
#   accessModes: [ReadWriteOnce]
#   resources: { requests: { storage: 5Gi } }""",
"""---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: firedrill-evidence
  namespace: firedrill-system
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 1Gi } }""")
subprocess.run(['kubectl', 'apply', '-f', '-'], input=y.encode(), check=True)
PY

echo "▸ deploying MinIO with a seeded backup…"
kubectl create namespace soak --dry-run=client -o yaml | kubectl apply -f -
kubectl -n soak apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: minio }
spec:
  replicas: 1
  selector: { matchLabels: { app: minio } }
  template:
    metadata: { labels: { app: minio } }
    spec:
      containers:
        - name: minio
          image: minio/minio:latest
          args: [server, /data]
          env:
            - { name: MINIO_ROOT_USER, value: minio }
            - { name: MINIO_ROOT_PASSWORD, value: minio123 }
---
apiVersion: v1
kind: Service
metadata: { name: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 9000 }]
YAML
kubectl -n soak wait --for=condition=Available deployment/minio --timeout=180s

echo "▸ seeding backup into MinIO…"
cat > /tmp/soak-dump.sql <<'SQL'
create table ledger (id bigserial primary key, amount bigint not null);
insert into ledger (amount) select g from generate_series(1, 5000) g;
create table firedrill_canary (token text); insert into firedrill_canary values ('fd-soak-token');
SQL
kubectl -n soak exec -i deploy/minio -- sh -c \
  'mc alias set local http://127.0.0.1:9000 minio minio123 >/dev/null && mc mb -p local/backups >/dev/null && mc pipe local/backups/pg/dump.sql' < /tmp/soak-dump.sql
rm -f /tmp/soak-dump.sql

echo "▸ operator AWS creds for MinIO (env secret)…"
kubectl -n firedrill-system create secret generic minio-creds \
  --from-literal=AWS_ACCESS_KEY_ID=minio --from-literal=AWS_SECRET_ACCESS_KEY=minio123 \
  --from-literal=AWS_REGION=us-east-1 --dry-run=client -o yaml | kubectl apply -f -
kubectl -n firedrill-system patch deployment firedrill-operator --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom","value":[{"secretRef":{"name":"minio-creds"}}]}]'
kubectl -n firedrill-system rollout status deployment/firedrill-operator --timeout=300s

echo "▸ creating soak drills…"
kubectl apply -f - <<'YAML'
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: soak-ok, namespace: firedrill-system }
spec:
  schedule: "* * * * *"
  objectives: { rto: 5m, rpo: 24h }
  source:
    driver: postgres
    from: { type: s3, uri: "s3://backups/pg/dump.sql", endpoint: "http://minio.soak.svc:9000" }
  sandbox: { provider: kubernetes, image: "postgres:16.10-alpine", ttl: 5m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from ledger", min: 5000 }
    - canary: { sql: "select token from firedrill_canary", expect: "fd-soak-token" }
  report: { sign: false }
---
apiVersion: firedrill.dev/v1
kind: RecoveryDrill
metadata: { name: soak-fail, namespace: firedrill-system }
spec:
  schedule: "*/2 * * * *"
  objectives: { rto: 5m, rpo: 24h }
  source:
    driver: postgres
    from: { type: s3, uri: "s3://backups/pg/dump.sql", endpoint: "http://minio.soak.svc:9000" }
  sandbox: { provider: kubernetes, image: "postgres:16.10-alpine", ttl: 5m }
  verify:
    - restoreSucceeded: {}
    - rowCount: { query: "select count(*) from ledger", min: 999999 }   # always fails
  report: { sign: false }
YAML

echo "✔ soak environment ready — watch: kubectl get drills -n firedrill-system -w"

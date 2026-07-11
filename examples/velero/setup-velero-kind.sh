#!/usr/bin/env bash
# Installs Velero + MinIO into the CURRENT kubectl context (intended for a
# kind cluster), deploys a small demo app, and takes a Velero backup of it.
# Requires: kubectl, velero CLI (https://velero.io/docs/main/basic-install/).
set -euo pipefail

VELERO_NS=velero
DEMO_NS=shop
BACKUP_NAME="${BACKUP_NAME:-shop-backup}"

echo "▸ deploying MinIO (object store for Velero)…"
kubectl create namespace "$VELERO_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$VELERO_NS" apply -f - <<'YAML'
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
          ports: [{ containerPort: 9000 }]
---
apiVersion: v1
kind: Service
metadata: { name: minio }
spec:
  selector: { app: minio }
  ports: [{ port: 9000 }]
YAML
kubectl -n "$VELERO_NS" wait --for=condition=Available deployment/minio --timeout=180s

echo "▸ creating the velero bucket in MinIO…"
kubectl -n "$VELERO_NS" run mc --rm -i --restart=Never --image=minio/mc --command -- sh -c \
  'mc alias set m http://minio:9000 minio minio123 && mc mb -p m/velero' >/dev/null

echo "▸ installing Velero server components…"
cat > /tmp/velero-credentials <<'EOF'
[default]
aws_access_key_id = minio
aws_secret_access_key = minio123
EOF
velero install \
  --provider aws \
  --plugins velero/velero-plugin-for-aws:v1.10.0 \
  --bucket velero \
  --secret-file /tmp/velero-credentials \
  --backup-location-config region=minio,s3ForcePathStyle=true,s3Url=http://minio.velero.svc:9000 \
  --use-volume-snapshots=false \
  --wait
rm -f /tmp/velero-credentials

echo "▸ deploying the demo app into namespace ${DEMO_NS}…"
kubectl create namespace "$DEMO_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$DEMO_NS" apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: web, labels: { app: web } }
spec:
  replicas: 2
  selector: { matchLabels: { app: web } }
  template:
    metadata: { labels: { app: web } }
    spec:
      containers:
        - name: web
          image: nginx:1.27-alpine
          ports: [{ containerPort: 80 }]
---
apiVersion: v1
kind: Service
metadata: { name: web }
spec:
  selector: { app: web }
  ports: [{ port: 80 }]
---
apiVersion: v1
kind: ConfigMap
metadata: { name: app-config }
data: { greeting: "hello from the backup" }
---
apiVersion: v1
kind: Secret
metadata: { name: app-secret }
stringData: { token: "s3cret-demo-token" }
YAML
kubectl -n "$DEMO_NS" wait --for=condition=Available deployment/web --timeout=180s

echo "▸ taking Velero backup '${BACKUP_NAME}' of namespace ${DEMO_NS}…"
velero backup create "$BACKUP_NAME" --include-namespaces "$DEMO_NS" --wait

echo "✔ ready — run: firedrill run shop-ns -f examples/firedrill-velero.yaml"

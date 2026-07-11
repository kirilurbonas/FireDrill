#!/usr/bin/env bash
# Generates examples/demo.dump — a realistic pg_dump (custom format) of a
# payments database with a seeded ledger, using a throwaway Postgres container.
set -euo pipefail
cd "$(dirname "$0")"

IMAGE=postgres:16.10-alpine
NAME=firedrill-demo-src
ROWS="${ROWS:-120000}"

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "▸ starting throwaway postgres to build the demo backup…"
docker run -d --name "$NAME" -e POSTGRES_PASSWORD=demo -e POSTGRES_DB=payments "$IMAGE" >/dev/null

until docker exec "$NAME" pg_isready -U postgres -d payments >/dev/null 2>&1; do sleep 0.5; done
sleep 2  # entrypoint restarts postgres once during init

echo "▸ seeding $ROWS ledger rows…"
docker exec -i "$NAME" psql -U postgres -d payments -q -v ON_ERROR_STOP=1 <<SQL
create table accounts (id serial primary key, name text not null, status text not null default 'active');
insert into accounts (name, status)
  select 'account-' || g, case when g % 20 = 0 then 'suspended' else 'active' end
  from generate_series(1, 500) g;

create table ledger (
  id bigserial primary key,
  account_id int not null references accounts(id),
  amount_cents bigint not null,
  created_at timestamptz not null default now()
);
insert into ledger (account_id, amount_cents)
  select (g % 500) + 1, (random() * 100000)::bigint
  from generate_series(1, $ROWS) g;
SQL

echo "▸ dumping to demo.dump (pg_dump custom format)…"
docker exec "$NAME" pg_dump -U postgres -d payments -Fc > demo.dump

echo "✔ wrote $(du -h demo.dump | cut -f1) to examples/demo.dump"

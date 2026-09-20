# The catalog the demo site attaches to. The base image carries the binary and
# the baked DuckDB extensions; this adds only data, which is the whole shape of
# a n6kd deployment.
#
# The API key is not baked in: it is passed to Cloud Run at deploy time, so it
# can be rotated without rebuilding.
ARG BASE=docker.io/srossross/n6kd:latest

# Until the base image ships ca-certificates: build the cert store here and
# copy it in, so HTTPS (the BigLake REST catalog) works at runtime.
FROM debian:bookworm-slim AS certs
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates

FROM ${BASE}

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY local.sql              /app/local.sql
COPY iceberg.sql            /app/iceberg.sql
COPY perms.yml              /app/perms.yml
COPY data/bank_failures.parquet  /app/data/bank_failures.parquet
COPY data/titanic.parquet        /app/data/titanic.parquet

CMD ["-f", "/app/local.sql", "-f", "/app/iceberg.sql", "--perms", "/app/perms.yml", "--json-logging"]

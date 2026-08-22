# Build from this directory:
#   docker build -t n6k/n6kd .
#
# A child image adds only data. Without a perms.yml every catalog the boot SQL
# leaves is served read/write to anyone who connects, so set N6KD_API_KEY at
# run time:
#   FROM n6k/n6kd
#   COPY schema.sql /app/schema.sql
#
# Naming a perms.yml serves only the tables it lists, at the access it grants:
#   FROM n6k/n6kd
#   COPY schema.sql /app/schema.sql
#   COPY perms.yml  /app/perms.yml
#   CMD ["-f", "/app/schema.sql", "--perms", "/app/perms.yml"]
#
# duckdb-go links prebuilt DuckDB libraries through cgo against glibc, so the
# runtime is Debian rather than alpine, and there is no cross toolchain here:
# build each architecture natively.

FROM golang:1.26-bookworm AS build

ENV GOTOOLCHAIN=auto \
    CGO_ENABLED=1

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -static-libstdc++/-static-libgcc keep the C++ runtime DuckDB needs inside the
# binary, so the runtime stage installs no packages at all.
RUN go build -trimpath \
    -ldflags '-s -w -extldflags "-static-libstdc++ -static-libgcc"' \
    -o /out/n6kd .

# Bake the extensions in. Running the server binary rather than a duckdb CLI is
# what guarantees the download matches the DuckDB version duckdb-go links. This
# is the only step that needs the network, and it needs the builder's CA
# certificates — which is why it lives here and not in the runtime stage.
RUN N6K_EXTENSION_HOME=/opt/n6k/extensions /out/n6kd install

RUN echo 'n6k:x:10001:10001::/app:/usr/sbin/nologin' >> /etc/passwd \
    && echo 'n6k:x:10001:' >> /etc/group

FROM debian:bookworm-slim AS certs
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates

FROM debian:bookworm-slim

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /etc/passwd /etc/passwd
COPY --from=build /etc/group /etc/group
COPY --from=build /out/n6kd /usr/local/bin/n6kd
# Owned by the runtime user: a child image's boot SQL may INSTALL further
# extensions (iceberg, httpfs, ...) into this same directory.
COPY --from=build --chown=10001:10001 /opt/n6k/extensions /opt/n6k/extensions

RUN mkdir -p /app && chown n6k:n6k /app

ENV N6K_EXTENSION_HOME=/opt/n6k/extensions
ENV PORT=8080

ENV HOME=/app

USER n6k
WORKDIR /app
EXPOSE 8080

# Fails the build unless the runtime user can load the baked extensions and
# bridge a catalog — the permissions and the DuckDB ABI are both checked here
# rather than on a customer's first request.
RUN n6kd selftest

ENTRYPOINT ["/usr/local/bin/n6kd"]
# A child image that ships a perms.yml overrides CMD to add --perms.
CMD ["-f", "/app/schema.sql"]

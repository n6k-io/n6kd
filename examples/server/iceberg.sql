-- The BigLake Iceberg catalog, authenticated as the Cloud Run service account.
-- The attach carries no credentials (AUTHORIZATION_TYPE none): every REST call
-- picks up its Authorization header from the bl_http HTTP secret at request
-- time, so rotating that secret refreshes the live attach without touching it.
-- An ICEBERG-secret token would be captured at attach time and be
-- unrefreshable. x-goog-user-project must be sent exactly once — a duplicate
-- is a 400 — which is why it lives on the attach, not in bl_http.
INSTALL iceberg;
LOAD iceberg;

CREATE SECRET metadata (
    TYPE HTTP,
    EXTRA_HTTP_HEADERS MAP {'Metadata-Flavor': 'Google'},
    SCOPE 'http://metadata.google.internal'
);

-- Every minute because the metadata server serves the same cached token until
-- it nears expiry; a long tick can grab a nearly-dead token. Runs at call, so
-- bl_http exists before the attach. GCP_TOKEN overrides the metadata server
-- for local runs (docker run -e GCP_TOKEN=...); the CASE is lazy, so the
-- metadata fetch never runs when the override is set.
SELECT n6kd_set_interval('lakehouse_token', INTERVAL 1 MINUTE, $$
    SET VARIABLE gcp_token = CASE
        WHEN getenv('GCP_TOKEN') IS NOT NULL THEN getenv('GCP_TOKEN')
        ELSE (
            SELECT content::JSON ->> 'access_token'
            FROM read_text('http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token')
        )
    END;
    CREATE OR REPLACE SECRET bl_http (
        TYPE HTTP,
        EXTRA_HTTP_HEADERS MAP {'Authorization': 'Bearer ' || getvariable('gcp_token')},
        SCOPE 'https://biglake.googleapis.com'
    );
$$);

ATTACH 'bl://projects/n6k-io/catalogs/n6k-datalake' AS lake (
    TYPE ICEBERG,
    ENDPOINT 'https://biglake.googleapis.com/iceberg/v1/restcatalog',
    AUTHORIZATION_TYPE 'none',
    EXTRA_HTTP_HEADERS MAP {'x-goog-user-project': 'n6k-io'},
    ACCESS_DELEGATION_MODE 'vended_credentials'
);

# Compat results

Run on 2026-11-07. Matching native means the same result or the same exception class.

| | n6k | quack |
| --- | --- | --- |
| ✅ matches native | 108 | 100 |
| ➖ unsupported | 2 | 5 |
| 💥 error, crash or timeout | 0 | 5 |
| ❌ wrong result | 0 | 0 |

## Types

| test | native | n6k | quack |
| --- | --- | --- | --- |
| HUGEINT | ✅ | ✅ | ✅ |
| UHUGEINT | ✅ | ✅ | ✅ |
| DECIMAL(38,18) | ✅ | ✅ | ✅ |
| INTERVAL | ✅ | ✅ | ✅ |
| TIMESTAMP_NS | ✅ | ✅ | ✅ |
| TIMESTAMPTZ | ✅ | ✅ | ✅ |
| TIME_NS | ✅ | ✅ | ✅ |
| TIMETZ | ✅ | ✅ | ✅ |
| DATE infinity | ✅ | ✅ | ✅ |
| UUID | ✅ | ✅ | ✅ |
| BLOB | ✅ | ✅ | ✅ |
| BIT | ✅ | ✅ | ✅ |
| ENUM | ✅ | ✅ | ✅ |
| LIST | ✅ | ✅ | ✅ |
| STRUCT | ✅ | ✅ | ✅ |
| MAP | ✅ | ✅ | ✅ |
| UNION | ✅ | ✅ | ✅ |
| ARRAY | ✅ | ✅ | ✅ |
| nested | ✅ | ✅ | ✅ |
| BIGNUM | ✅ | ✅ | ✅ |
| VARIANT | ✅ | ✅ | ✅ |
| GEOMETRY | ✅ | ✅ | ✅ |
| DOUBLE nan | ✅ | ✅ | ✅ |
| NULL | ✅ | ✅ | ✅ |

## SQL coverage

| test | native | n6k | quack |
| --- | --- | --- | --- |
| UPDATE RETURNING | ✅ | ✅ | ✅ |
| DELETE RETURNING | ✅ | ✅ | ✅ |
| ON CONFLICT | ✅ | ✅ | ✅ |
| MERGE | ✅ | ✅ | ✅ |
| CREATE VIEW | ✅ | ✅ | ✅ |
| CREATE SCHEMA | ✅ | ✅ | ✅ |
| CREATE INDEX | ✅ | ➖ | ✅ |
| CREATE SEQUENCE | ✅ | ➖ | ✅ |
| ALTER add column | ✅ | ✅ | ✅ |
| ALTER add column with default | ✅ | ✅ | ✅ |
| ALTER drop column | ✅ | ✅ | ✅ |
| ALTER change type | ✅ | ✅ | ✅ |
| ALTER rename | ✅ | ✅ | ✅ |
| DROP TABLE | ✅ | ✅ | ✅ |
| CREATE TABLE AS | ✅ | ✅ | ✅ |
| COPY out | ✅ | ✅ | ✅ |
| COPY in | ✅ | ✅ | ✅ |
| USE | ✅ | ✅ | ✅ |
| DESCRIBE | ✅ | ✅ | ✅ |
| rowid | ✅ | ✅ | ✅ |
| SAMPLE | ✅ | ✅ | ✅ |
| recursive CTE | ✅ | ✅ | ✅ |
| PIVOT | ✅ | ✅ | ✅ |
| LATERAL | ✅ | ✅ | ✅ |
| EXPLAIN | ✅ | ✅ | ✅ |
| EXPLAIN ANALYZE | ✅ | ✅ | ✅ |
| Prepared parameters | ✅ | ✅ | 💥 |
| Multi-statement | ✅ | ✅ | ✅ |

## Client-side references

| test | native | n6k | quack |
| --- | --- | --- | --- |
| getvariable in a filter | ✅ | ✅ | ✅ |
| Macro in a filter | ✅ | ✅ | ✅ |
| Python UDF in a filter | ✅ | ✅ | 💥 |
| random() in a filter | ✅ | ✅ | ✅ |
| Client setting in the select list | ✅ | ✅ | ✅ |
| Python UDF in the select list | ✅ | ✅ | 💥 |
| Macro in the select list | ✅ | ✅ | ✅ |
| Prepared parameter in a mixed join | ✅ | ✅ | ✅ |
| Prepared parameters in INSERT | ✅ | ✅ | 💥 |
| Prepared parameters in UPDATE | ✅ | ✅ | 💥 |

## Client-side data

| test | native | n6k | quack |
| --- | --- | --- | --- |
| Dataframe join | ✅ | ✅ | ✅ |
| Dataframe in a filter | ✅ | ✅ | ✅ |
| Client file in a filter | ✅ | ✅ | ✅ |
| Client file join | ✅ | ✅ | ✅ |
| Client table macro | ✅ | ✅ | ✅ |
| Client secret in a join | ✅ | ✅ | ✅ |
| Database attached on the client | ✅ | ✅ | ✅ |
| Local table in a filter | ✅ | ✅ | ✅ |
| CTE with client data | ✅ | ✅ | ✅ |
| Inline VALUES join | ✅ | ✅ | ✅ |
| Correlated subquery on a remote table | ✅ | ✅ | ✅ |
| Window over a mixed join | ✅ | ✅ | ✅ |

## Writes with RETURNING and client data

| test | native | n6k | quack |
| --- | --- | --- | --- |
| INSERT RETURNING | ✅ | ✅ | ✅ |
| UPDATE RETURNING an expression | ✅ | ✅ | ✅ |
| ON CONFLICT DO NOTHING | ✅ | ✅ | ✅ |
| INSERT OR REPLACE | ✅ | ✅ | ✅ |
| INSERT from a local table | ✅ | ✅ | ✅ |
| INSERT RETURNING from a local table | ✅ | ✅ | ➖ |
| INSERT from a dataframe | ✅ | ✅ | ✅ |
| UPDATE FROM a local table | ✅ | ✅ | ➖ |
| UPDATE with a remote subquery | ✅ | ✅ | ✅ |
| DELETE USING a local table | ✅ | ✅ | ➖ |
| MERGE from a local table | ✅ | ✅ | ➖ |
| MERGE with insert and delete | ✅ | ✅ | ✅ |
| MERGE RETURNING | ✅ | ✅ | ✅ |
| CREATE TABLE AS from a mixed join | ✅ | ✅ | ➖ |

## Constraints

| test | native | n6k | quack |
| --- | --- | --- | --- |
| PRIMARY KEY over attach: duplicate rejected | ✅ | ✅ | ✅ |
| PRIMARY KEY over attach: UPDATE, MERGE, DELETE work | ✅ | ✅ | ✅ |
| DESCRIBE keeps a key created over attach | ✅ | ✅ | ✅ |
| UPDATE on a table without a key | ✅ | ✅ | ✅ |
| DELETE on a table without a key | ✅ | ✅ | ✅ |
| CHECK over attach | ✅ | ✅ | ✅ |
| Foreign key over attach | ✅ | ✅ | ✅ |
| Generated column over attach | ✅ | ✅ | ✅ |

## Transactions

| test | native | n6k | quack |
| --- | --- | --- | --- |
| Read your own write | ✅ | ✅ | ✅ |
| Other client doesn't see uncommitted write | ✅ | ✅ | ✅ |
| Write conflict | ✅ | ✅ | ✅ |
| Rollback | ✅ | ✅ | ✅ |
| Commit | ✅ | ✅ | ✅ |
| Own write in a mixed join | ✅ | ✅ | ✅ |
| DDL visible to its own transaction | ✅ | ✅ | ✅ |
| Uncommitted DDL hidden from other clients | ✅ | ✅ | ✅ |
| Rolled-back DDL gone | ✅ | ✅ | ✅ |
| Committed DDL persists | ✅ | ✅ | ✅ |

## Errors

| test | native | n6k | quack |
| --- | --- | --- | --- |
| Primary key violation | ✅ | ✅ | ✅ |
| Missing table | ✅ | ✅ | ✅ |
| Conversion failure | ✅ | ✅ | ✅ |
| Missing function | ✅ | ✅ | ✅ |

## Details

- ➖ **n6k, CREATE INDEX:** `Binder Error: crossing: 't30' is served by a source; CREATE INDEX is not supported`
- ➖ **n6k, CREATE SEQUENCE:** `Binder Error: Creating sequences in n6k attached databases is not supported`
- 💥 **quack, Prepared parameters:** `Invalid Input Error: Values were not provided for the following parameters: 1`
- 💥 **quack, Python UDF in a filter:** `Catalog Error: Scalar Function with name under does not exist!`
- 💥 **quack, Python UDF in the select list:** `Catalog Error: Scalar Function with name under does not exist!`
- 💥 **quack, Prepared parameters in INSERT:** `Invalid Input Error: Values were not provided for the following parameters: 1, 2`
- 💥 **quack, Prepared parameters in UPDATE:** `Invalid Input Error: Values were not provided for the following parameters: 1, 2`
- ➖ **quack, INSERT RETURNING from a local table:** `Not implemented Error: RETURNING not yet supported for QUACK_INSERT`
- ➖ **quack, UPDATE FROM a local table:** `Not implemented Error: PlanUpdate not implemented yet`
- ➖ **quack, DELETE USING a local table:** `Not implemented Error: PlanDelete not implemented yet`
- ➖ **quack, MERGE from a local table:** `Not implemented Error: PlanUpdate not implemented yet`
- ➖ **quack, CREATE TABLE AS from a mixed join:** `Not implemented Error: Multiple streaming scans or streaming scans + CTAS / insert in the same query are not currently supported`

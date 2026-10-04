ATTACH ':memory:' AS local;

CREATE TABLE local.bank_failures AS SELECT * FROM read_parquet('/app/data/bank_failures.parquet');
CREATE TABLE local.titanic       AS SELECT * FROM read_parquet('/app/data/titanic.parquet');

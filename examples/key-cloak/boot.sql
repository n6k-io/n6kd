ATTACH ':memory:' AS sales;
CREATE TABLE sales.main.deals AS
  SELECT * FROM read_parquet('data/*/*.parquet', hive_partitioning = true);

ATTACH ':memory:' AS app;
USE app;
CREATE TABLE a (i BIGINT PRIMARY KEY, k BIGINT);
INSERT INTO a SELECT range, range % 100 FROM range(getvariable('rows'));
CREATE TABLE b (k BIGINT PRIMARY KEY, v VARCHAR);
INSERT INTO b SELECT range, 'v' || range FROM range(100);

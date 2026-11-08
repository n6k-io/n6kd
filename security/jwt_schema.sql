ATTACH ':memory:' AS app;
USE app;
CREATE TABLE owned (owner VARCHAR);
INSERT INTO owned VALUES ('alice'), ('bob');

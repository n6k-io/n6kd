ATTACH ':memory:' AS other;
CREATE TABLE other.b (k BIGINT PRIMARY KEY, v VARCHAR);
INSERT INTO other.b SELECT range, 'v' || range FROM range(100);

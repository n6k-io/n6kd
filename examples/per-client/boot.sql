ATTACH ':memory:' AS ops;
CREATE TABLE ops.main.zone_reviews (
  id INTEGER PRIMARY KEY,
  location_id INTEGER NOT NULL,
  note VARCHAR NOT NULL,
  owner VARCHAR NOT NULL
);
INSERT INTO ops.main.zone_reviews VALUES
  (1, 132, 'Check JFK pickup wait times', 'ada@example.com'),
  (2, 138, 'Check LaGuardia pickup wait times', 'grace@example.com');

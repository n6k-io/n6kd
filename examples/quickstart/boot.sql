INSTALL httpfs; LOAD httpfs;

ATTACH ':memory:' AS geo;
CREATE TABLE geo.main.zones AS FROM 'https://d37ci6vzurychx.cloudfront.net/misc/taxi+_zone_lookup.csv';

ATTACH ':memory:' AS transit;
CREATE TABLE transit.main.daily_ridership AS FROM read_csv('https://data.ny.gov/resource/vxuj-8kew.csv?$limit=50000');

ATTACH ':memory:' AS ops;
CREATE TABLE ops.main.zone_reviews (
  id INTEGER PRIMARY KEY,
  location_id INTEGER NOT NULL,
  note VARCHAR NOT NULL,
  status VARCHAR NOT NULL
);
CREATE TABLE ops.main.reviewers (id INTEGER PRIMARY KEY, email VARCHAR NOT NULL);
INSERT INTO ops.main.reviewers VALUES (1, 'ada@example.com');

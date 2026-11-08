CREATE TABLE big AS SELECT range AS i, 'row-' || range AS s FROM range(5000000);

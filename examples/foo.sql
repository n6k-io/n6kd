CREATE TABLE users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO users VALUES (1, 'Ada'), (2, 'Grace');

CREATE TABLE audit_log (id INTEGER PRIMARY KEY, action VARCHAR);
INSERT INTO audit_log VALUES (1, 'login');

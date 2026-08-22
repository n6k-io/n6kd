ATTACH ':memory:' AS app;

CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');

CREATE TABLE app.main.audit_log (id INTEGER PRIMARY KEY, action VARCHAR);
INSERT INTO app.main.audit_log VALUES (1, 'login');

SELECT count(*), sum(i) FROM r.a WHERE k IN (SELECT k FROM r.b WHERE k < 5);

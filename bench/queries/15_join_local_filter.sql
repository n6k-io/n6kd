SELECT count(*), sum(a.i) FROM r.a a JOIN l ON a.k = l.k WHERE a.i < 1000;

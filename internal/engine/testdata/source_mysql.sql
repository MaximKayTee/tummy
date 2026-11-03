-- full []
SELECT * FROM `s`.`orders`

-- select where limit []
SELECT `id`, `at` FROM `s`.`orders` WHERE (id > 0) LIMIT 10

-- incremental first run []
SELECT * FROM `s`.`orders`

-- incremental with key [2026-01-01]
SELECT * FROM `s`.`orders` WHERE `at` >= ?

-- incremental append [2026-01-01]
SELECT * FROM `s`.`orders` WHERE `at` > ?

-- incremental limit [2026-01-01]
SELECT * FROM (SELECT * FROM `s`.`orders` WHERE (id > 0) AND `at` > ?) AS _t ORDER BY `at` LIMIT 100

-- backfill [2026-01-01 2026-02-01]
SELECT * FROM `s`.`orders` WHERE `at` >= ? AND `at` <= ?

-- sql where [2026-01-01]
SELECT * FROM (select * from t order by at) AS _f WHERE (id > 0) AND `at` > ?

-- placeholder twice [2026-01-01 2026-01-01]
select * from a where `at` > ? union all select * from b where `at` > ?

-- placeholder first run []
select * from a where 1=1

-- cte [2026-01-01]
with x as (select * from t) SELECT * FROM (select * from x) AS _f WHERE `at` > ?


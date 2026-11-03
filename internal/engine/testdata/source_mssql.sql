-- full []
SELECT * FROM [s].[orders]

-- select where limit []
SELECT TOP 10 * FROM (SELECT [id], [at] FROM [s].[orders] WHERE (id > 0)) AS _t

-- incremental first run []
SELECT * FROM [s].[orders]

-- incremental with key [2026-01-01]
SELECT * FROM [s].[orders] WHERE [at] >= @p1

-- incremental append [2026-01-01]
SELECT * FROM [s].[orders] WHERE [at] > @p1

-- incremental limit [2026-01-01]
SELECT TOP 100 * FROM (SELECT * FROM [s].[orders] WHERE (id > 0) AND [at] > @p1) AS _t ORDER BY [at]

-- backfill [2026-01-01 2026-02-01]
SELECT * FROM [s].[orders] WHERE [at] >= @p1 AND [at] <= @p2

-- sql where [2026-01-01]
SELECT * FROM (select * from t order by at OFFSET 0 ROWS) AS _f WHERE (id > 0) AND [at] > @p1

-- placeholder twice [2026-01-01 2026-01-01]
select * from a where [at] > @p1 union all select * from b where [at] > @p2

-- placeholder first run []
select * from a where 1=1

-- cte [2026-01-01]
with x as (select * from t) SELECT * FROM (select * from x) AS _f WHERE [at] > @p1


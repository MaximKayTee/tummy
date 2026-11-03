-- create
CREATE TABLE [s].[_tummy_tmp_orders] ([id] bigint, [code] nvarchar(450), [note] nvarchar(max), [total] decimal(10,2), [rate] decimal(38,10), [ok] bit, [at] datetime2, [day] date, [meta] nvarchar(max), [blob] varbinary(max), [f] float)

-- insert 2 rows
INSERT INTO [s].[_tummy_tmp_orders] ([id], [code], [note]) VALUES (@p1, @p2, @p3), (@p4, @p5, @p6)

-- insert select
INSERT INTO [s].[orders] ([id], [code], [note]) SELECT [id], [code], [note] FROM [s].[_tummy_tmp_orders]

-- alter add
ALTER TABLE [s].[orders] ADD [total] decimal(10,2)

-- drop column
ALTER TABLE [s].[_tummy_tmp_orders] DROP COLUMN [_tummy_seq]

-- drop table
DROP TABLE [s].[_tummy_tmp_orders]

-- create index
CREATE INDEX [_tummy_ix_orders] ON [s].[orders] ([id], [code])

-- max
SELECT MAX([at]) FROM [s].[orders]

-- duplicates
SELECT TOP 1 * FROM (SELECT 1 AS d FROM [s].[_tummy_tmp_orders] GROUP BY [id], [code] HAVING COUNT(*) > 1) AS _t

-- rename
EXEC sp_rename '[s].[_tummy_tmp_orders]', 'orders'

-- truncate
TRUNCATE TABLE [s].[orders]

-- delete matching
DELETE _tgt FROM [s].[orders] AS _tgt WHERE EXISTS (SELECT 1 FROM [s].[_tummy_tmp_orders] AS _tmp WHERE _tmp.[id] = _tgt.[id] AND _tmp.[code] = _tgt.[code])

-- dedupe by update key
DELETE _a FROM [s].[_tummy_tmp_orders] AS _a WHERE EXISTS (SELECT 1 FROM [s].[_tummy_tmp_orders] AS _b WHERE _b.[id] = _a.[id] AND _b.[code] = _a.[code] AND (_b.[at] > _a.[at] OR (_a.[at] IS NULL AND _b.[at] IS NOT NULL) OR ((_b.[at] = _a.[at] OR (_a.[at] IS NULL AND _b.[at] IS NULL)) AND _b.[_tummy_seq] > _a.[_tummy_seq])))

-- dedupe by load order
DELETE _a FROM [s].[_tummy_tmp_orders] AS _a WHERE EXISTS (SELECT 1 FROM [s].[_tummy_tmp_orders] AS _b WHERE _b.[id] = _a.[id] AND _b.[_tummy_seq] > _a.[_tummy_seq])

-- swap exists=true tx=true
DROP TABLE [s].[orders];
EXEC sp_rename '[s].[_tummy_tmp_orders]', 'orders'

-- swap exists=false tx=true
EXEC sp_rename '[s].[_tummy_tmp_orders]', 'orders'

-- table exists [s orders]
SELECT count(*) FROM information_schema.tables WHERE table_schema = @p1 AND table_name = @p2

-- list tables [dbo]
SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema = @p1 ORDER BY 2

-- limit
SELECT TOP 5 * FROM (SELECT * FROM [s].[orders]) AS _t

-- order
SELECT * FROM (SELECT * FROM [s].[orders]) AS _t ORDER BY [id] DESC OFFSET 0 ROWS

-- order limit
SELECT TOP 5 * FROM (SELECT * FROM [s].[orders]) AS _t ORDER BY [id]

-- limit over limit
SELECT TOP 5 * FROM (select id from t limit 10) AS _t

-- limit over order by
SELECT TOP 5 * FROM (select id from t order by id OFFSET 0 ROWS) AS _t

-- wrap cte
WITH x AS (SELECT 1 AS n) SELECT * FROM (SELECT n FROM x ORDER BY n OFFSET 0 ROWS) AS _w WHERE n > 0

-- wrap comment
SELECT * FROM (select n from t -- trailing
) AS _w

-- cast types
string=nvarchar(max) int=bigint float=float decimal=decimal(38,10) bool=bit timestamp=datetime2 date=date json=nvarchar(max) bytes=varbinary(max)

-- column types
id=bigint code=nvarchar(max) note=nvarchar(max) total=decimal(10,2) rate=decimal(38,10) ok=bit at=datetime2 day=date meta=nvarchar(max) blob=varbinary(max) f=float
key string=nvarchar(450)

@p1 @p2 

-- create schema


-- create
CREATE TABLE "_tummy_tmp_orders" ("id" INTEGER, "code" TEXT, "note" TEXT, "total" NUMERIC, "rate" NUMERIC, "ok" BOOLEAN, "at" TIMESTAMP, "day" DATE, "meta" TEXT, "blob" BLOB, "f" REAL)

-- insert 2 rows
INSERT INTO "_tummy_tmp_orders" ("id", "code", "note") VALUES (?, ?, ?), (?, ?, ?)

-- insert select
INSERT INTO "orders" ("id", "code", "note") SELECT "id", "code", "note" FROM "_tummy_tmp_orders"

-- alter add
ALTER TABLE "orders" ADD "total" NUMERIC

-- drop column
ALTER TABLE "_tummy_tmp_orders" DROP COLUMN "_tummy_seq"

-- drop table
DROP TABLE "_tummy_tmp_orders"

-- create index
CREATE INDEX "_tummy_ix_orders" ON "orders" ("id", "code")

-- max
SELECT MAX("at") FROM "orders"

-- max bytes
SELECT * FROM (SELECT "blob" FROM "orders" WHERE "blob" IS NOT NULL) AS _t ORDER BY "blob" DESC LIMIT 1

-- duplicates
SELECT 1 AS d FROM "_tummy_tmp_orders" GROUP BY "id", "code" HAVING COUNT(*) > 1 LIMIT 1

-- rename
ALTER TABLE "_tummy_tmp_orders" RENAME TO "orders"

-- truncate
DELETE FROM "orders"

-- delete matching
DELETE FROM "orders" AS _tgt WHERE EXISTS (SELECT 1 FROM "_tummy_tmp_orders" AS _tmp WHERE _tmp."id" = _tgt."id" AND _tmp."code" = _tgt."code")

-- dedupe by update key
DELETE FROM "_tummy_tmp_orders" AS _a WHERE EXISTS (SELECT 1 FROM "_tummy_tmp_orders" AS _b WHERE _b."id" = _a."id" AND _b."code" = _a."code" AND (_b."at" > _a."at" OR (_a."at" IS NULL AND _b."at" IS NOT NULL) OR ((_b."at" = _a."at" OR (_a."at" IS NULL AND _b."at" IS NULL)) AND _b."_tummy_seq" > _a."_tummy_seq")))

-- dedupe by load order
DELETE FROM "_tummy_tmp_orders" AS _a WHERE EXISTS (SELECT 1 FROM "_tummy_tmp_orders" AS _b WHERE _b."id" = _a."id" AND _b."_tummy_seq" > _a."_tummy_seq")

-- swap exists=true tx=true
DROP TABLE "orders";
ALTER TABLE "_tummy_tmp_orders" RENAME TO "orders"

-- swap exists=false tx=true
ALTER TABLE "_tummy_tmp_orders" RENAME TO "orders"

-- table exists [orders]
SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?

-- list tables []
SELECT '', name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY 1

-- limit
SELECT * FROM "orders" LIMIT 5

-- order
SELECT * FROM (SELECT * FROM "orders") AS _t ORDER BY "id" DESC

-- order limit
SELECT * FROM (SELECT * FROM "orders") AS _t ORDER BY "id" LIMIT 5

-- limit over limit
SELECT * FROM (select id from t limit 10) AS _t LIMIT 5

-- limit over order by
select id from t order by id LIMIT 5

-- wrap cte
WITH x AS (SELECT 1 AS n) SELECT * FROM (SELECT n FROM x ORDER BY n) AS _w WHERE n > 0

-- wrap comment
SELECT * FROM (select n from t -- trailing
) AS _w

-- cast types
string=TEXT int=INTEGER float=REAL decimal=NUMERIC bool=INTEGER timestamp=TEXT date=TEXT json=TEXT bytes=BLOB

-- column types
id=INTEGER code=TEXT note=TEXT total=NUMERIC rate=NUMERIC ok=BOOLEAN at=TIMESTAMP day=DATE meta=TEXT blob=BLOB f=REAL
key string=TEXT

? ? 

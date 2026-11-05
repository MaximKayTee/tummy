-- create schema
CREATE SCHEMA IF NOT EXISTS "out"

-- create
CREATE TABLE "s"."_tummy_tmp_orders" ("id" bigint, "code" text, "note" text, "total" numeric, "rate" numeric, "ok" boolean, "at" timestamp, "day" date, "meta" jsonb, "blob" bytea, "f" double precision)

-- insert 2 rows
INSERT INTO "s"."_tummy_tmp_orders" ("id", "code", "note") VALUES ($1, $2, $3), ($4, $5, $6)

-- insert select
INSERT INTO "s"."orders" ("id", "code", "note") SELECT "id", "code", "note" FROM "s"."_tummy_tmp_orders"

-- alter add
ALTER TABLE "s"."orders" ADD "total" numeric

-- drop column
ALTER TABLE "s"."_tummy_tmp_orders" DROP COLUMN "_tummy_seq"

-- drop table
DROP TABLE "s"."_tummy_tmp_orders"

-- create index
CREATE INDEX "_tummy_ix_orders" ON "s"."orders" ("id", "code")

-- max
SELECT MAX("at") FROM "s"."orders"

-- max bytes
SELECT * FROM (SELECT "blob" FROM "s"."orders" WHERE "blob" IS NOT NULL) AS _t ORDER BY "blob" DESC LIMIT 1

-- duplicates
SELECT 1 AS d FROM "s"."_tummy_tmp_orders" GROUP BY "id", "code" HAVING COUNT(*) > 1 LIMIT 1

-- rename
ALTER TABLE "s"."_tummy_tmp_orders" RENAME TO "orders"

-- truncate
TRUNCATE TABLE "s"."orders"

-- delete matching
DELETE FROM "s"."orders" AS _tgt WHERE EXISTS (SELECT 1 FROM "s"."_tummy_tmp_orders" AS _tmp WHERE _tmp."id" = _tgt."id" AND _tmp."code" = _tgt."code")

-- dedupe by update key
DELETE FROM "s"."_tummy_tmp_orders" AS _a WHERE EXISTS (SELECT 1 FROM "s"."_tummy_tmp_orders" AS _b WHERE _b."id" = _a."id" AND _b."code" = _a."code" AND (_b."at" > _a."at" OR (_a."at" IS NULL AND _b."at" IS NOT NULL) OR ((_b."at" = _a."at" OR (_a."at" IS NULL AND _b."at" IS NULL)) AND _b."_tummy_seq" > _a."_tummy_seq")))

-- dedupe by load order
DELETE FROM "s"."_tummy_tmp_orders" AS _a WHERE EXISTS (SELECT 1 FROM "s"."_tummy_tmp_orders" AS _b WHERE _b."id" = _a."id" AND _b."_tummy_seq" > _a."_tummy_seq")

-- swap exists=true tx=true
DROP TABLE "s"."orders";
ALTER TABLE "s"."_tummy_tmp_orders" RENAME TO "orders"

-- swap exists=false tx=true
ALTER TABLE "s"."_tummy_tmp_orders" RENAME TO "orders"

-- table exists [s orders]
SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2

-- list tables [public]
SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type IN ('BASE TABLE','VIEW') ORDER BY 2

-- limit
SELECT * FROM "s"."orders" LIMIT 5

-- order
SELECT * FROM (SELECT * FROM "s"."orders") AS _t ORDER BY "id" DESC

-- order limit
SELECT * FROM (SELECT * FROM "s"."orders") AS _t ORDER BY "id" LIMIT 5

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
string=text int=bigint float=double precision decimal=numeric bool=boolean timestamp=timestamp date=date json=jsonb bytes=bytea

-- column types
id=bigint code=text note=text total=numeric rate=numeric ok=boolean at=timestamp day=date meta=jsonb blob=bytea f=double precision
key string=text

$1 $2 

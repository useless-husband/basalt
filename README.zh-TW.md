# basalt

**一個用 Go 從零寫成、講 PostgreSQL 網路協定的關聯式 SQL 資料庫。** `psql`、Go 的 `pgx` 驅動程式和 Python 的 `psycopg` 不需修改就能直接連線使用。

協定以下的一切都是 basalt 自己的程式碼：以分頁為單位的儲存引擎（緩衝池、預寫式日誌、B+ 樹、當機復原）、具快照隔離的 MVCC 交易、SQL 解析器、以成本為基礎的查詢規劃器和執行器。沒有內嵌 SQLite 或 PostgreSQL，也沒有使用第三方 SQL 解析器；唯一的相依套件是測試用的客戶端驅動程式。

這個專案的目的，是用一個人讀得完的規模（約兩萬九千行 Go）呈現真正的資料庫各個部分如何組合在一起，並且用真實的客戶端和其他資料庫引擎來驗證，而不是只用自己的假設驗證自己。它是單機的研究與教學系統，不適合存放重要資料。

[English](README.md) · [設計文件](docs/DESIGN.md) · [導讀（給初學者）](docs/導讀.zh-TW.md)

## 用 psql 操作的實錄

以下是用 [`docs/demo.sql`](docs/demo.sql) 這份腳本執行 `psql -a -f demo.sql` 的真實輸出（只去掉了行尾空白）：

```text
CREATE TABLE accounts (id serial PRIMARY KEY, owner text NOT NULL, balance numeric(12,2) NOT NULL CHECK (balance >= 0));
CREATE TABLE
CREATE TABLE transfers (id bigserial PRIMARY KEY, src int REFERENCES accounts, dst int REFERENCES accounts, amount numeric(12,2), at timestamptz DEFAULT now());
CREATE TABLE
INSERT INTO accounts (owner, balance) SELECT 'user' || g, 100 FROM generate_series(1, 10000) g;
INSERT 0 10000
\d accounts
                                Table "public.accounts"
 Column  |     Type      | Collation | Nullable |               Default
---------+---------------+-----------+----------+--------------------------------------
 id      | integer       |           | not null | nextval('accounts_id_seq'::regclass)
 owner   | text          |           | not null |
 balance | numeric(12,2) |           | not null |
Indexes:
    "accounts_pkey" PRIMARY KEY, btree (id)
Check constraints:
    "accounts_balance_check" CHECK (balance >= 0)
Referenced by:
    TABLE "transfers" CONSTRAINT "transfers_dst_fkey" FOREIGN KEY (dst) REFERENCES accounts(id)
    TABLE "transfers" CONSTRAINT "transfers_src_fkey" FOREIGN KEY (src) REFERENCES accounts(id)

BEGIN;
BEGIN
UPDATE accounts SET balance = balance - 25 WHERE id = 1;
UPDATE 1
UPDATE accounts SET balance = balance + 25 WHERE id = 2;
UPDATE 1
INSERT INTO transfers (src, dst, amount) VALUES (1, 2, 25) RETURNING id, src, dst, amount;
 id | src | dst | amount
----+-----+-----+--------
  1 |   1 |   2 |  25.00
(1 row)

INSERT 0 1
COMMIT;
COMMIT
UPDATE accounts SET balance = balance - 500 WHERE id = 1;
psql:demo.sql:10: ERROR:  new row for relation "accounts" violates check constraint "accounts_balance_check"
ANALYZE;
ANALYZE
EXPLAIN ANALYZE SELECT a.owner, sum(t.amount) FROM accounts a JOIN transfers t ON t.src = a.id WHERE a.id < 100 GROUP BY a.owner;
                                                              QUERY PLAN
---------------------------------------------------------------------------------------------------------------------------------------
 HashAggregate  (cost=5.04..5.05 rows=1 width=15) (actual time=0.015..0.015 rows=1 loops=1)
   Group Key: a.owner
   ->  Nested Loop  (cost=2.00..5.04 rows=1 width=63) (actual time=0.013..0.013 rows=1 loops=1)
         ->  Seq Scan on transfers t  (cost=0.00..1.01 rows=1 width=40) (actual time=0.002..0.002 rows=1 loops=1)
         ->  Index Scan using accounts_pkey on accounts a  (cost=2.00..4.02 rows=1 width=23) (actual time=0.007..0.007 rows=1 loops=1)
               Index Cond: (a.id = t.src)
               Filter: (a.id < 100)
 Execution Time: 0.022 ms
(8 rows)

SELECT count(*), sum(balance) FROM accounts;
 count |    sum
-------+------------
 10000 | 1000000.00
(1 row)
```

## 實作了什麼

**網路協定 v3。** 啟動與協定版本協商、免密碼或明文密碼驗證、簡單查詢與延伸查詢協定（Parse/Bind/Describe/Execute/Sync、參數型別推斷、常用型別的文字與二進位格式、portal 暫停）、文字與 CSV 格式的 COPY FROM STDIN / TO STDOUT、CancelRequest、notice，以及帶 SQLSTATE 代碼、detail、hint、位置、資料表和約束欄位的錯誤。ReadyForQuery 會回報閒置、交易中和交易失敗三種狀態。`pg_catalog` 與 `information_schema` 模擬到足以讓 psql 的 `\d`、`\dt`、`\d 資料表`、`\d+`、`\di`、`\ds`、`\dn`、`\l`、`\du` 印出和 PostgreSQL 相同的輸出。

**SQL。** CREATE/DROP/TRUNCATE TABLE、CREATE/DROP INDEX、CREATE SEQUENCE、ALTER TABLE（ADD COLUMN、RENAME、ADD CONSTRAINT）；PRIMARY KEY、NOT NULL、UNIQUE、CHECK 與 FOREIGN KEY（支援 ON DELETE/UPDATE CASCADE 和 SET NULL）；serial 與 identity 欄位；帶 RETURNING 和 ON CONFLICT（DO NOTHING / DO UPDATE）的 INSERT；UPDATE ... FROM；DELETE ... USING；SELECT 支援 inner、left、right、full、cross join、WHERE、GROUP BY/HAVING、聚合函式（count、sum、avg、min、max、bool_and/or、string_agg、array_agg、stddev/variance，可搭配 DISTINCT、FILTER、ORDER BY）、含 NULLS FIRST/LAST 的 ORDER BY、LIMIT/OFFSET、DISTINCT 與 DISTINCT ON、純量、IN、EXISTS、ANY/ALL 子查詢（相關或不相關）、非遞迴 CTE、UNION/INTERSECT/EXCEPT [ALL]、CASE、COALESCE/NULLIF/GREATEST/LEAST，以及正確的三值 NULL 邏輯；型別有 smallint、integer、bigint、real、double precision、numeric(p,s)（任意精度）、text、varchar(n)、char(n)、boolean、date、timestamp、timestamptz、interval、bytea 和一維陣列；約 120 個內建函式；EXPLAIN 與 EXPLAIN ANALYZE。DDL 具有交易性。

**查詢規劃器。** 解析名稱並推斷參數型別的 binder、邏輯計畫、改寫規則（常數摺疊、把條件推進 join、掃描和 GROUP BY 之下、外部連接轉內部連接），以及使用 ANALYZE 統計資料（列數、NULL 比例、相異值數量、最常見值、直方圖）的成本式實體規劃。它在循序掃描與索引掃描之間選擇，用子集合動態規劃決定連接順序（超過八張表改用貪婪法），並挑選雜湊、合併、巢狀迴圈或索引巢狀迴圈連接；ORDER BY ... LIMIT 可以依索引順序讀取或使用 top-N 排序。

**儲存。** 單一資料檔、8 KiB 且帶檢查碼的分頁、時鐘演算法的緩衝池、槽式資料頁、會分裂與合併的 B+ 樹索引、分段的預寫式日誌與群組提交、以原子方式記錄頁面差異或整頁影像的迷你交易（因此當機不會留下分裂一半的 B+ 樹，撕裂的頁也會被修復）、尖銳檢查點、重做式復原，以及 macOS 上的 `F_FULLFSYNC`。

**交易。** 64 位元交易編號的 MVCC。READ COMMITTED（預設，與 PostgreSQL 相同：每條語句一個快照；遇到在快照之後才提交的修改時，語句會撤回自己的變更並以新快照重新執行）與 REPEATABLE READ（快照隔離：整筆交易一個快照，先修改的人贏，後者收到 SQLSTATE 40001）。要求 SERIALIZABLE 會直接拒絕，不會偷偷降級。具等待圖死結偵測（40P01）的鎖管理器、多個並行連線、VACUUM 與自動 VACUUM。

## 運作方式

```text
  psql · pgx · psycopg
          │  PostgreSQL protocol v3
  ┌───────▼──────────────────────────────────────────────────────────┐
  │ pgwire    startup · simple/extended query · COPY · cancel        │
  │ engine    sessions · transactions · DDL · ANALYZE · VACUUM ·     │
  │           pg_catalog emulation                                   │
  │ sql ──► planner: binder → logical plan → rewrite → cost-based    │
  │         physical plan ──► executor: Volcano iterators, DML,      │
  │         constraints                                              │
  ├──────────────────────────────────────────────────────────────────┤
  │ txn       snapshots · visibility · commit log · lock manager     │
  ├──────────────────────────────────────────────────────────────────┤
  │ storage   buffer pool · heap pages · B+trees · mini-transactions │
  │           → WAL (group commit) · checkpoints · redo recovery     │
  └───────▲─────────────────────────────▲────────────────────────────┘
     basalt.db (8 KiB pages)       wal/*.wal segments
```

幾個觀念撐起了大部分的設計（細節與考慮過的替代方案見 [docs/DESIGN.md](docs/DESIGN.md)）：

- **迷你交易。** 每次修改分頁都在一個迷你交易裡進行，最後變成一筆日誌記錄，內容是改動的位元組範圍（檢查點後第一次修改則是整頁影像）。多頁的操作在日誌中天然是原子的，不需要為每種資料結構寫重做邏輯。
- **因為 MVCC，所以只需要重做。** 當機後未提交的資料列版本留在硬碟上；復原後的提交紀錄把它們的交易標為中止，因此看不見，之後由 VACUUM 清除。這是 PostgreSQL 的做法，因此不需要撤銷（undo）流程。
- **用欄位編號取代位置。** binder 為查詢中每個欄位分配唯一編號。調換連接順序和下推條件可以自由改變資料列的排列；某個運算子輸入中找不到的欄位，必然是相關子查詢或參數化索引掃描的外部參考。
- **不為 psql 寫特例。** psql 送來的目錄查詢走的是與一般查詢相同的解析器、規劃器和執行器，查的是由 basalt 目錄即時產生的虛擬 `pg_catalog` 表。

## 證據

上面每一項說法都有可以執行的東西佐證。以下結果都在「效能測試」一節所描述的機器上量測。

| 項目 | 指令 | 結果 |
|---|---|---|
| 單元與整合測試（所有套件） | `make test`、`make race` | 通過；CI 在 Linux 和 macOS 上以 `-race` 執行 |
| 真正的 psql 客戶端 | `make psql-test` | 265 行 psql 輸出（DDL、`\d`、`\copy`、連接、帶 SQLSTATE 的錯誤、交易、EXPLAIN）與 [`test/psql/basic.expected`](test/psql/basic.expected) 完全相同 |
| Go 驅動程式 pgx | `go test ./internal/pgwire` | 二進位格式的型別來回轉換（numeric、時間戳、bytea、interval）、批次、錯誤欄位、交易、COPY、取消、多客戶端並行 |
| Python 驅動程式 psycopg 3 | `make python-test` | 7 個測試：型別、例外類別、交易、COPY 進出、隔離、識別字、取消 |
| SQLite 的 sqllogictest 題庫 | `make slt` | **57 個檔案、382,574 筆中通過 382,491 筆（99.98%）**：`select1`–`select5` 10,706/10,706，`random/*`（各類別前 10 個檔案）371,516/371,544，`evidence/*` 269/324；題庫標記 `skipif postgresql` 的 144,203 筆略過 |
| 與 sqlite3 對答案 | `make difftest` | 35,000 條隨機查詢（種子 1–8）在含 NULL 的隨機資料表上結果完全相同 |
| 當機安全（殺掉行程） | `make crash-test` | 4 個並行寫入者下以 PID 送 `SIGKILL` 30 輪：每筆已確認的提交都在、回滾或從未提交的資料都看不到、總金額守恆、索引與資料表一致 |
| 當機安全（停電模型） | `go test ./internal/storage -run Recovery` | 30 次隨機化的故障檔案系統當機（未 sync 的寫入遺失、其中一筆撕裂）：所有已 flush 的記錄都恢復，B+ 樹不變式成立 |
| 隔離 | `go test ./internal/engine -run 'No\|Skew\|Deadlock\|ReadCommitted\|Bank'` | REPEATABLE READ 防止髒讀、不可重複讀、幻讀、更新遺失與讀取偏斜；寫入偏斜被證明可能發生；死結能被偵測；16 個客戶端轉帳時稽核者看到的總額不變 |
| 規劃器使用統計資料 | `go test ./internal/engine -run Planner` | 估計值與實際列數相差 30% 以內；索引或循序掃描、雜湊建表端、依索引順序的 LIMIT 與合併連接都如預期被選中 |

sqllogictest 沒通過的分兩類：一是 PostgreSQL 與 SQLite 規則本來就不同、但題庫沒有標記的地方（整數溢位與除以零會報錯；`NULLIF(int, numeric)` 的結果是 numeric），二是 basalt 沒有的功能（trigger、view、`REPLACE`、`REINDEX`）。測試程式會印出每一個失敗原因；題庫在測試時從固定的 commit 下載，不放在這個儲存庫裡。

## 效能測試

所有數字都在同一台機器上量測：Apple M5（10 核心）、16 GB 記憶體、內建 SSD（APFS）、macOS 27.0、Go 1.27.1、SQLite 3.45.3（Python 的 `sqlite3` 模組）、DuckDB 1.5.5。量測期間這台機器同時在跑其他建置工作，所以重複執行大約有 10% 到 20% 的差異；TPC-B 每個數字是一次 20 秒的執行，查詢時間是五次執行的中位數。每次執行的原始輸出都可以用下面的指令重現。

### TPC-B（pgbench 的交易）

`bench/tpcb` 實作 pgbench 內建的交易：在 BEGIN/COMMIT 之間更新一個帳戶、讀回它、更新一個櫃員和一個分行、新增一筆歷史紀錄。規模 8（800,000 個帳戶、8 個分行），每個客戶端一條連線，語句已預先準備（pgx 的語句快取）。`bench/tpcb/sqlite_tpcb.py` 用 `BEGIN IMMEDIATE` 對 SQLite 執行同樣的交易。嵌入模式直接呼叫 basalt 的引擎，每次執行後都檢查 TPC-B 一致性（帳戶、櫃員、分行與歷史紀錄的總額一致）。

```sh
go run ./bench/tpcb -basalt bin/basalt -scale 8 -clients 1,8,16 -duration 20s               # 持久化
go run ./bench/tpcb -basalt bin/basalt -scale 8 -clients 1,8,16 -duration 20s -no-fsync     # 不 fsync
go run ./bench/tpcb -embedded -scale 8 -clients 1,8 -duration 20s                            # 同一行程內
python3 bench/tpcb/sqlite_tpcb.py --scale 8 --clients 1,8 --duration 20
```

每秒交易數：

| 設定 | 1 個客戶端 | 8 個客戶端 | 16 個客戶端 |
|---|---:|---:|---:|
| basalt 經 TCP，持久化（F_FULLFSYNC、群組提交） | 218 | 608 | 756 |
| SQLite 同一行程，WAL，`synchronous=FULL`、`fullfsync=ON` | 247 | 238 | |
| basalt 經 TCP，`-unsafe-no-fsync` | 1,900 | 12,132 | 15,957 |
| basalt 同一行程，無網路、不 fsync | 2,617 | 33,288 | |
| SQLite 同一行程，WAL，`synchronous=NORMAL`（每次提交不 fsync） | 30,567 | 31,144 | |

這些數字老實地說明了：

- **持久化的提交受限於硬碟 flush。** 這台機器上 `F_FULLFSYNC` 約需 4 毫秒，所以單一客戶端在兩個引擎上都大約每秒 250 次提交。客戶端變多時，basalt 的群組提交讓等待中的交易共用一次 flush（8 個客戶端 608、16 個 756），而 SQLite 的單一寫入者只能一個一個來。basalt 剩下的瓶頸是 8 個分行資料列的競爭：每一列都要被持有到該交易的 flush 完成。
- **不 flush 時，SQLite 每筆交易快得多。** 同一行程、單一客戶端時，SQLite 每秒約 30,000 筆交易，basalt 約 2,600，少了約 12 倍：basalt 每次更新都要寫新版本的資料列和索引項目、記錄頁面差異，並用通用的迭代器執行計畫。經 TCP 時 basalt 每筆交易還要付出七次網路往返（單一客戶端 1,900）。basalt 能隨客戶端數擴展（同一行程 8 個客戶端 33,288），SQLite 不能，但這不是對等的比較：SQLite 本來就不嘗試讓寫入者並行。
- 同一行程中，一個客戶端每筆交易要 0.35 毫秒，八個客戶端時每個只要 0.20 毫秒；我沒有查清楚為什麼單一客戶端每筆交易做得比較少（M5 大小核的 CPU 排程是可能的原因，但我沒有驗證）。

### 分析型查詢

`bench/analytics/run.py` 用固定種子產生一個小型、類似 TPC-H 的資料集（15,000 位顧客、150,000 筆訂單、599,648 筆明細），把相同的資料載入三個引擎（只有主鍵、都執行過 ANALYZE），確認三者回傳相同結果，並計時七個查詢。basalt 透過網路協定以 psycopg 量測，SQLite 在同一行程內，DuckDB 使用其命令列的計時器。

```sh
python3 bench/analytics/run.py --basalt bin/basalt --scale 1 --repeat 5 --plans
```

| 查詢 | basalt 毫秒 | SQLite 毫秒 | DuckDB 毫秒 | basalt / SQLite |
|---|---:|---:|---:|---:|
| Q1 掃描明細 + GROUP BY | 362.8 | 138.9 | 3.0 | 2.6 倍 |
| Q2 連接 + GROUP BY | 44.9 | 15.6 | 1.0 | 2.9 倍 |
| Q3 三表連接 + 前 10 名 | 239.0 | 23.7 | 2.0 | 10.1 倍 |
| Q4 相關 EXISTS | 67.2 | 13.4 | 2.0 | 5.0 倍 |
| Q5 IN（子查詢） | 33.9 | 13.0 | 1.0 | 2.6 倍 |
| Q6 count(DISTINCT) | 200.1 | 72.3 | 2.0 | 2.8 倍 |
| Q7 主鍵查詢 | 0.1 | 0.0 | 0.0 | |

在這些查詢上 basalt 比 SQLite 慢 2.6 到 10 倍，比向量化欄式引擎 DuckDB 慢兩個數量級。載入資料 basalt 花了 3.6 秒（經網路 COPY），SQLite 0.8 秒，DuckDB 0.4 秒。主要原因是已知的：一次一列地經過迭代器和閉包、`numeric` 用任意精度運算（Q1 和 Q3 要對幾十萬列計算 `price * (1 - discount)`，SQLite 用的是浮點數），以及子查詢沒有被轉成連接（Q4 對每一筆符合條件的訂單做一次索引查詢）。

## 限制

- 單機、單一資料庫、單一 schema（`public`）。沒有角色與權限、沒有 TLS；驗證只有免密碼或單一明文密碼（`-password`）。請只綁定在 localhost。
- 沒有 SERIALIZABLE 隔離（SSI）；REPEATABLE READ 允許寫入偏斜，與 PostgreSQL 相同。沒有 savepoint、`SELECT ... FOR UPDATE`、`LOCK`、`LISTEN/NOTIFY`（接受但忽略）或 prepared transaction。
- 未實作：view、trigger、預存程序、視窗函式、遞迴 CTE、LATERAL、`DROP COLUMN`、部分索引或運算式索引、遞減索引順序與反向索引掃描、多維陣列、二進位 COPY、UTC 以外的時區、C 以外的定序。
- 一列資料必須放得進一頁（約 8 KB）；沒有 TOAST。
- 子查詢不會被轉成連接：相關的 EXISTS 會對每一列外層資料執行一次子計畫（有索引時走索引）。
- `numeric` 使用任意精度運算（`math/big`），結果精確但速度慢。
- 尖銳檢查點在寫回髒頁時會暫停寫入。同一個索引的 B+ 樹寫入者會排隊。DDL 由持有到提交為止的目錄鎖序列化。
- DROP 或 TRUNCATE 釋放空間時若當機可能遺失（洩漏）分頁，但不會損毀資料。沒有 `VACUUM FULL`（會當作一般 VACUUM 執行）。
- 查詢結果會先全部算完再送給客戶端。
- 當機安全是用殺掉行程與停電模型（故障注入檔案系統）測試的，沒有真的切斷機器電源。

## 相關專案

- **[PostgreSQL](https://www.postgresql.org/)** 是參考對象：basalt 以小得多的規模重新實作了它的協定、SQLSTATE、目錄結構以及許多機制（xmin/xmax 的 MVCC、提交紀錄、整頁寫入、READ COMMITTED 與 REPEATABLE READ 的語意），不同之處如上所述（以重新執行語句取代 EvalPlanQual、尖銳檢查點、沒有 HOT 更新）。
- **[SQLite](https://sqlite.org/)** 是單檔嵌入式資料庫；basalt 用它的 sqllogictest 題庫和 `sqlite3` 命令列當測試的標準答案，並在效能測試中與它比較。
- **[CockroachDB](https://github.com/cockroachdb/cockroach)** 是用 Go 寫、講 pgwire、有成本式最佳化器的正式分散式 SQL 資料庫；basalt 是單機，規模小好幾個數量級。
- **[toydb](https://github.com/erikgrinaker/toydb)**（Rust）是有 Raft 與 MVCC 的教學用分散式 SQL 資料庫。basalt 沒有複寫，重點放在有 WAL 的分頁式儲存引擎，以及與未修改的 PostgreSQL 客戶端相容。
- **[BusTub](https://github.com/cmu-db/bustub)**（CMU 15-445）是讓學生補完緩衝池、B+ 樹與執行器的教學骨架；basalt 是有網路協定和 SQL 前端的完整系統。
- **[go-mysql-server](https://github.com/dolthub/go-mysql-server)** 是 Go 寫的 MySQL 相容 SQL 引擎，儲存層可替換；**[DuckDB](https://duckdb.org/)** 是欄式、向量化的分析引擎，放在效能測試中作為參考點，顯示一次處理一列的引擎與目前最佳技術的差距。

就我所知，能不經修改直接執行 psql 自己的目錄查詢的從零教學資料庫並不多；basalt 的做法是用一般的 SQL 執行來回答這些查詢，而不是為它們寫特例。

## 建置與執行

需要 Go 1.25 以上。

```sh
make build                                 # bin/basalt
bin/basalt -D ./data -listen 127.0.0.1:5433
psql -h 127.0.0.1 -p 5433 -U me -d demo    # 使用者與資料庫名稱可以任意填
```

參數：`-D` 資料目錄、`-listen` 位址（埠號 0 會自動挑一個空的並印出來）、`-password`、`-pool-mb`（緩衝池大小，預設 128）、`-checkpoint-interval`、`-autovacuum-interval`、`-v`，以及只供效能測試用的 `-unsafe-no-fsync`。

```sh
make test          # 單元與整合測試
make race          # 開啟 race detector
make lint          # gofmt、go vet、staticcheck
make psql-test     # 用真的 psql 測試（必要時設定 PSQL=/path/to/psql）
make python-test   # psycopg 3（pip install "psycopg[binary]"）
make crash-test    # 在負載下 SIGKILL 伺服器、重啟、驗證
make slt           # sqllogictest 子集（約下載 60 MB）
make difftest      # 隨機查詢與 sqlite3 對答案
make bench         # TPC-B 與分析型查詢效能測試
```

## 授權

[MIT](LICENSE)

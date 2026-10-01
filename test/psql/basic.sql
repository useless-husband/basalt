-- basalt driven by psql: schema, catalog commands, queries, errors,
-- transactions and EXPLAIN. Output is compared with basic.expected.

CREATE TABLE customers (
    id      serial PRIMARY KEY,
    name    text NOT NULL,
    email   varchar(64) UNIQUE,
    country char(2) DEFAULT 'TW',
    joined  date NOT NULL DEFAULT '2024-01-01',
    CHECK (length(name) > 0)
);
CREATE TABLE orders (
    id          bigserial PRIMARY KEY,
    customer_id int NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    amount      numeric(10,2) NOT NULL CHECK (amount > 0),
    placed      timestamp NOT NULL,
    note        text
);
CREATE INDEX orders_customer_idx ON orders (customer_id);

\dt
\d customers
\d orders
\di

INSERT INTO customers (name, email, country) VALUES
    ('Ada', 'ada@example.org', 'GB'),
    ('Lin', 'lin@example.org', DEFAULT),
    ('Sam', NULL, 'US');
INSERT INTO orders (customer_id, amount, placed, note) VALUES
    (1, 120.50, '2024-03-01 10:00', 'first'),
    (1, 30.00,  '2024-03-05 18:30', NULL),
    (2, 99.99,  '2024-03-02 09:15', 'gift'),
    (2, 15.25,  '2024-04-11 12:00', NULL),
    (3, 7.00,   '2024-04-12 08:45', 'tiny')
RETURNING id, customer_id, amount;

\copy orders (customer_id, amount, placed) from stdin
3	42.00	2024-05-01 00:00:00
1	8.75	2024-05-02 00:00:00
\.

SELECT c.name, count(o.id) AS orders, sum(o.amount) AS total, max(o.placed)::date AS last
FROM customers c LEFT JOIN orders o ON o.customer_id = c.id
GROUP BY c.name
ORDER BY total DESC;

SELECT name, country,
       (SELECT avg(amount) FROM orders o WHERE o.customer_id = c.id) AS avg_amount
FROM customers c
WHERE EXISTS (SELECT 1 FROM orders o WHERE o.customer_id = c.id AND o.amount > 50)
ORDER BY name;

WITH monthly AS (
    SELECT date_trunc('month', placed) AS month, sum(amount) AS total
    FROM orders GROUP BY 1
)
SELECT month::date, total,
       CASE WHEN total > 100 THEN 'busy' ELSE 'quiet' END AS kind
FROM monthly ORDER BY month;

SELECT customer_id FROM orders WHERE amount > 100
UNION
SELECT id FROM customers WHERE country = 'US'
ORDER BY 1;

-- NULL semantics
SELECT NULL = NULL AS eq, NULL IS NULL AS is_null, 1 IN (2, NULL) AS in_null,
       1 NOT IN (2, NULL) AS not_in_null, coalesce(NULL, 'x') AS coalesced,
       count(note) AS non_null_notes, count(*) AS all_rows
FROM orders;

SELECT note, count(*) FROM orders GROUP BY note ORDER BY note NULLS FIRST;

-- errors with SQLSTATE codes
\set VERBOSITY verbose
INSERT INTO customers (name, email) VALUES ('Dup', 'ada@example.org');
INSERT INTO orders (customer_id, amount, placed) VALUES (99, 1, now());
INSERT INTO orders (customer_id, amount, placed) VALUES (1, -5, now());
SELECT amount / 0 FROM orders LIMIT 1;
SELECT nosuchcolumn FROM orders;
\set VERBOSITY default

-- transactions
BEGIN;
UPDATE orders SET amount = amount * 2 WHERE customer_id = 1;
SELECT sum(amount) FROM orders WHERE customer_id = 1;
ROLLBACK;
SELECT sum(amount) FROM orders WHERE customer_id = 1;

BEGIN;
DELETE FROM customers WHERE id = 3;
SELECT oops;
SELECT 1;
COMMIT;
SELECT count(*) FROM customers;

DELETE FROM customers WHERE id = 3;
SELECT count(*) AS orders_left FROM orders;

-- the planner
ANALYZE;
EXPLAIN (COSTS OFF) SELECT * FROM orders WHERE id = 3;
EXPLAIN (COSTS OFF) SELECT c.name, o.amount FROM customers c JOIN orders o ON o.customer_id = c.id WHERE c.country = 'GB';
EXPLAIN (COSTS OFF) SELECT customer_id, sum(amount) FROM orders GROUP BY customer_id ORDER BY 2 DESC LIMIT 2;

SHOW server_version;
SHOW transaction_isolation;
BEGIN ISOLATION LEVEL REPEATABLE READ;
SHOW transaction_isolation;
COMMIT;
BEGIN ISOLATION LEVEL SERIALIZABLE;

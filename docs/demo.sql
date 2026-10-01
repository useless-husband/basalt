CREATE TABLE accounts (id serial PRIMARY KEY, owner text NOT NULL, balance numeric(12,2) NOT NULL CHECK (balance >= 0));
CREATE TABLE transfers (id bigserial PRIMARY KEY, src int REFERENCES accounts, dst int REFERENCES accounts, amount numeric(12,2), at timestamptz DEFAULT now());
INSERT INTO accounts (owner, balance) SELECT 'user' || g, 100 FROM generate_series(1, 10000) g;
\d accounts
BEGIN;
UPDATE accounts SET balance = balance - 25 WHERE id = 1;
UPDATE accounts SET balance = balance + 25 WHERE id = 2;
INSERT INTO transfers (src, dst, amount) VALUES (1, 2, 25) RETURNING id, src, dst, amount;
COMMIT;
UPDATE accounts SET balance = balance - 500 WHERE id = 1;
ANALYZE;
EXPLAIN ANALYZE SELECT a.owner, sum(t.amount) FROM accounts a JOIN transfers t ON t.src = a.id WHERE a.id < 100 GROUP BY a.owner;
SELECT count(*), sum(balance) FROM accounts;

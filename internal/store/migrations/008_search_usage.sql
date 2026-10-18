-- Spend per search provider and calendar month (UTC), shared by every
-- worker, so paid search APIs stay inside their free tier.
CREATE TABLE search_usage (
    month    text NOT NULL,          -- YYYY-MM
    provider text NOT NULL,
    queries  int NOT NULL DEFAULT 0,
    cost_usd numeric(12, 6) NOT NULL DEFAULT 0,
    PRIMARY KEY (month, provider)
);

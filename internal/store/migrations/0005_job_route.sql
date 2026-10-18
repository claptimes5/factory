-- The routing entry a job ran under (implement.S, review.high_risk, ...),
-- so its step's instructions and skills follow it.
ALTER TABLE jobs ADD COLUMN route TEXT;

CREATE TABLE IF NOT EXISTS cards (
    id SERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    subtasks TEXT,
    status VARCHAR(20) NOT NULL DEFAULT 'todo',
    card_order INTEGER NOT NULL DEFAULT 0
);

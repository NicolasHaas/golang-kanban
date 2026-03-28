CREATE TABLE IF NOT EXISTS categories (
    id        SERIAL PRIMARY KEY,
    name      TEXT NOT NULL,
    slug      TEXT NOT NULL UNIQUE,
    row_order INTEGER NOT NULL DEFAULT 0,
    locked    BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS statuses (
    id        SERIAL PRIMARY KEY,
    name      TEXT NOT NULL,
    slug      TEXT NOT NULL UNIQUE,
    col_order INTEGER NOT NULL DEFAULT 0,
    locked    BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS cards (
    id          SERIAL PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT,
    subtasks    TEXT,
    status      VARCHAR(50) NOT NULL DEFAULT 'todo',
    category    VARCHAR(50) NOT NULL DEFAULT 'work',
    card_order  INTEGER NOT NULL DEFAULT 0
);

INSERT INTO categories (name, slug, row_order, locked) VALUES
    ('Work',     'work',     1, false),
    ('Personal', 'personal', 2, false),
    ('Other',    'other',    3, false)
ON CONFLICT (slug) DO NOTHING;

INSERT INTO statuses (name, slug, col_order, locked) VALUES
    ('Tomorrow',       'tomorrow',      1, false),
    ('To Do',          'todo',          2, true),
    ('In Progress',    'inprogress',    3, false),
    ('Needs Feedback', 'needsfeedback', 4, false),
    ('Done',           'done',          5, true)
ON CONFLICT (slug) DO NOTHING;

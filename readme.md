## Simple Kanban Board
A no-nonsense, lightweight Kanban board built with Golang and HTMX. I couldn’t find a reasonable self-hosted Kanban board that wasn’t using some JavaScript monstrosity like Node or Next.js, I don't need the next [9.1 CVE](https://github.com/advisories/GHSA-f82v-jwr5-mffw) on my server — so I decided to build my own. This project is my sweet little solution to manage tasks simply while learning and sharing a project with the community.

### Overview
This project provides a clean, minimalistic Kanban board with the following technologies:

Backend: Golang

Frontend: HTMX, Bootstrap 5, and SortableJS

Database: PostgreSQL

It’s designed to be simple to deploy and maintain without the extra bloat of modern JavaScript frameworks.

### Features
- Minimalistic Design: A straightforward Kanban board to manage your tasks.
- Dynamic Interactivity: Partial page updates using HTMX for a smooth user experience.
- Drag-and-Drop: Rearrange cards effortlessly using SortableJS.

### Using Docker Compose
A sample docker-compose.yml is provided, just use `docker-compose up --build`
This command will build and run both the PostgreSQL and Kanban services.

### Using the Pre-built Docker Image
Alternatively, you can pull the pre-built Docker image from GitHub Container Registry:

``` bash
docker pull ghcr.io/nicolashaas/golang-kanban:latest
docker run -p 17808:17808 ghcr.io/nicolashaas/golang-kanban:latest
```

### Prerequisites
PostgreSQL: The only external dependency required. If you don’t have a PostgreSQL instance, you can easily run one using Docker.

Create a PostgreSQL database and table. That could look something like this:
``` sql
-- Create user and database
CREATE USER kanban WITH PASSWORD 'your_password_here';
CREATE DATABASE kanban_db OWNER kanban;

-- Connect to the new database (you'll need to do this manually in psql)
-- \c kanban_db

-- Once connected as postgres to kanban_db, transfer schema ownership
GRANT ALL ON SCHEMA public TO kanban;
ALTER SCHEMA public OWNER TO kanban;

-- Connect as kanban user to kanban_db and run this to create the table
CREATE TABLE IF NOT EXISTS cards (
    id SERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    subtasks TEXT,
    status VARCHAR(20) NOT NULL DEFAULT 'todo',
    card_order INTEGER NOT NULL DEFAULT 0
);

```

### Environment Variables:
``` bash
DB_USER=your_db_user
DB_PASS=your_db_password
DB_HOST=localhost
DB_PORT=5432
DB_NAME=kanban
SERVER_PORT=17808
```

#### Todo's
If I feel like it I might work on some of these things:
- [ ] darkmode
- [ ] remove/add/edit collums
- [ ] make it pretty
- [ ] add sqlite option for people too lazy to setup a db
- [ ] tls
- [ ] oidc
- [ ] ...

#### Contributing
Contributions are welcome! If you have ideas, bug fixes, or enhancements, feel free to fork the repository, open an issue, or submit a pull request.

#### License
This project is open-sourced under the MIT License.

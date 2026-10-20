---
autonomy: auto
depends_on: [database, app]
---
# Session cleanup

Every night at 3:00 (Rome time) run the command `python manage.py clearsessions`
from the web app image in `app.md` against the database in `database.md`.

If the job fails two nights in a row, send an alert by email to `platform-alerts@example.com`
through an SNS topic.

## Checks
- There is an active schedule with the stated time.
- The last run (after the first night) finished successfully.

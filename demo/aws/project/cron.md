---
autonomy: approve
depends_on: [database, app]
---
# Session cleanup

Every night at 3:00 (Rome time) run as an ECS Fargate task the command `echo session cleanup`
from the image `public.ecr.aws/docker/library/busybox:1.36`, in the private subnets of the network
in `database.md`, with access to the database in `database.md`.

If the job fails two nights in a row, send an alert by email to the environment's address through an SNS topic
(AWS sends a subscription confirmation email: it must be accepted).

## Checks
- There is an active schedule with the stated time.
- The last run (after the first night) finished successfully.

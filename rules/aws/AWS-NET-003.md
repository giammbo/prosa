---
id: AWS-NET-003
title: Databases are not publicly accessible
applies_to: [aws_db_instance, aws_rds_cluster_instance]
source:
  - label: AWS Security Hub, RDS.2
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/rds-controls.html
---
`publicly_accessible` is `false` (or left unset, as its default is false), and
the database sits in a DB subnet group made of private subnets.

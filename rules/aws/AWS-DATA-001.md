---
id: AWS-DATA-001
title: Databases keep automatic backups
applies_to: [aws_db_instance, aws_rds_cluster]
source:
  - label: AWS Security Hub, RDS.11
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/rds-controls.html
---
`backup_retention_period` is greater than 0; 7 days is the recommended value.
A replica may inherit its source's backups.

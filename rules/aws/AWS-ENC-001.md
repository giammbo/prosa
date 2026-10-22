---
id: AWS-ENC-001
title: Database storage is encrypted at rest
applies_to: [aws_db_instance, aws_rds_cluster]
source:
  - label: AWS Security Hub, RDS.3
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/rds-controls.html
---
`storage_encrypted` is `true`. When a key is named (`kms_key_id`), it is a
customer managed key only if the intent asks for one; otherwise the AWS
managed key is enough.

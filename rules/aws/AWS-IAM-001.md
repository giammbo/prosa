---
id: AWS-IAM-001
title: Policies grant only the actions and resources needed
applies_to: [aws_iam_policy, aws_iam_role_policy]
source:
  - label: AWS Well-Architected, SEC03-BP02
    url: https://docs.aws.amazon.com/wellarchitected/latest/security-pillar/sec_permissions_least_privileges.html
  - label: AWS Security Hub, IAM.1
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/iam-controls.html#iam-1
---
Every `Allow` statement in `policy` lists the specific actions the workload
uses and the ARNs of the resources it touches. No statement has `"Action": "*"`
with `"Resource": "*"`, and a `*` in either is justified only where the API
accepts no resource-level scoping.

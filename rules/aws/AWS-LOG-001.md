---
id: AWS-LOG-001
title: Container logs go to CloudWatch with a retention
applies_to: [aws_ecs_task_definition, aws_cloudwatch_log_group]
source:
  - label: AWS Security Hub, ECS.9
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/ecs-controls.html#ecs-9
  - label: AWS Security Hub, CloudWatch.16
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/cloudwatch-controls.html
---
Each container in `container_definitions` has a `logConfiguration` with the
`awslogs` driver and a log group created by the plan. That
`aws_cloudwatch_log_group` sets `retention_in_days` explicitly; never leave
it unset, which keeps logs forever.

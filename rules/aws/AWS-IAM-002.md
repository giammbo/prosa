---
id: AWS-IAM-002
title: ECS tasks have separate execution and task roles
applies_to: [aws_ecs_task_definition, aws_iam_role]
source:
  - label: Amazon ECS, task execution IAM role
    url: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task_execution_IAM_role.html
  - label: Amazon ECS, task IAM role
    url: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/task-iam-roles.html
---
A task definition sets `execution_role_arn` to a role that pulls images,
writes logs, and reads only the secrets and parameters its container
definitions reference with `valueFrom`, and `task_role_arn` to a different
role holding the application's own permissions. Everything the application
itself calls goes on the task role, never on the execution role.

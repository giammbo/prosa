---
id: AWS-NET-004
title: ECS tasks behind a load balancer have no public IP
applies_to: [aws_ecs_service]
source:
  - label: AWS Security Hub, ECS.2
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/ecs-controls.html#ecs-2
  - label: AWS Well-Architected, SEC05-BP01
    url: https://docs.aws.amazon.com/wellarchitected/latest/security-pillar/sec_network_protection_create_layers.html
---
A service with a `load_balancer` block sets `assign_public_ip` to `false` in
`network_configuration` and runs in private subnets; the load balancer is the
only public entry point. A task that must reach the internet does so through
a NAT gateway or VPC endpoints.

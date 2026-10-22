---
id: AWS-NET-001
title: A database accepts traffic only from its clients
applies_to: [aws_security_group, aws_vpc_security_group_ingress_rule, aws_db_instance]
source:
  - label: AWS Well-Architected, SEC05-BP02
    url: https://docs.aws.amazon.com/wellarchitected/latest/security-pillar/sec_network_protection_layered.html
---
The database's security group allows its port only from the security groups
of the workloads that use it, referenced by their id. Never from a CIDR: not
0.0.0.0/0, not the VPC, not a subnet.

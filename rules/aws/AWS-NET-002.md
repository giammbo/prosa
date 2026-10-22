---
id: AWS-NET-002
title: Ingress from the whole internet only on 80 and 443 of a public load balancer
applies_to: [aws_security_group, aws_vpc_security_group_ingress_rule, aws_security_group_rule]
source:
  - label: AWS Security Hub, EC2.18 and EC2.19
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/ec2-controls.html
  - label: AWS Well-Architected, SEC05-BP01
    url: https://docs.aws.amazon.com/wellarchitected/latest/security-pillar/sec_network_protection_create_layers.html
---
No ingress rule has a source of 0.0.0.0/0 or ::/0 (`cidr_ipv4`, `cidr_ipv6`,
`cidr_blocks`, `ipv6_cidr_blocks`), with one exception: TCP 80 and 443 on the
security group of a public load balancer. Any other port, and any other
resource, is reached from a security group or from a narrower CIDR.

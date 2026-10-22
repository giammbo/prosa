---
id: AWS-ENC-002
title: HTTPS listeners use a current TLS policy
applies_to: [aws_lb_listener]
source:
  - label: AWS Security Hub, ELB.17
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/elb-controls.html#elb-17
  - label: AWS Security Hub, ELB.1
    url: https://docs.aws.amazon.com/securityhub/latest/userguide/elb-controls.html#elb-1
---
A listener with `protocol` HTTPS or TLS sets `ssl_policy` to a TLS 1.2 or
1.3 policy such as `ELBSecurityPolicy-TLS13-1-2-Res-2021-06`, never an older
one such as `ELBSecurityPolicy-2016-08`. When an Application Load Balancer has
an HTTPS listener, its port 80 listener redirects to HTTPS instead of
forwarding. A load balancer with no HTTPS listener is not a violation of this
rule.

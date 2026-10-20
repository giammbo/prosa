---
autonomy: approve
---
# Main database

I want a Postgres 16 with 20 GiB of storage, encrypted at rest, in eu-south-1,
reachable only from the application's private network. Automatic backup every night,
kept for 7 days. Small size.

The private network does not exist yet: create a dedicated VPC `10.40.0.0/16` across two
availability zones, with two public subnets (for the load balancer and the NAT) and two private subnets
(for the database and the containers), an internet gateway and a single NAT gateway for the outbound traffic
of the private subnets.

The administrator password is generated and kept by RDS in Secrets Manager
(managed password); it must never be written in any intent.

## Constraints
- No data outside the European Union.
- Required tag: `team=platform`.
- Estimated cost of the database under 60 euros per month; the network (NAT included) is not counted.

## Checks
- The database accepts connections from the private subnet and rejects them from the internet.
- There is at least one backup newer than 24 hours (after the first day).
- Storage is encrypted.

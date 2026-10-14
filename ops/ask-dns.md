# Recursive chatbot DNS

The Route 53 public zone `ch.at` remains authoritative for the website and all
existing records. Only `ask.ch.at` is delegated to `ch.at.` (whose A record is
maintained in the parent zone). This is an NS delegation, not a wildcard A or
CNAME record. No new hosted zone or domain nameserver change is needed.

Deploy the DNS implementation before creating the delegation. Before merging to
`master`, export the live public feed as described in [board-feed.md](board-feed.md):
the existing CI/CD workflow restarts the in-memory board on every deployment. The child server
must answer authoritative NS and SOA queries for `ask.ch.at` and NODATA queries
for other types. Delegated TXT queries receive an immediate UDP TC response;
recursive resolvers retry over TCP for model generation. This avoids short UDP
retry windows (observed with Google Public DNS). Both UDP and TCP port 53 must
remain reachable.
Do not add a DS record: the child is not DNSSEC-signed.

Apply only to the existing `ch.at` zone:

```sh
aws route53 change-resource-record-sets \
  --hosted-zone-id Z04796131HUISTSBOAP7A \
  --change-batch file://ops/ask-dns-route53.json
```

The change uses CREATE so an existing delegation cannot be silently overwritten.
Wait for Route 53's change status to become INSYNC, then verify:

```sh
dig @ch.at ask.ch.at NS +norecurse
dig @ch.at ask.ch.at SOA +tcp +norecurse
dig @1.1.1.1 what-is-2-plus-2.ask.ch.at TXT
dig @8.8.8.8 what.is.2.plus.2.ask.ch.at TXT
dig what-is-2-plus-2.ask.ch.at TXT
```

Previously cached NXDOMAIN answers may persist for the parent SOA's negative
TTL (currently 900 seconds). Prompts are case-insensitive because recursive
resolvers can randomize DNS-name case. Hyphens and dots separate words; DNS
backslash-escaped label punctuation is rejected. The apex TXT query returns
NODATA rather than invoking the model with an empty prompt.

Successful TXT answers have a 60-second TTL. Resolver caching is intentional;
there is no per-user conversation or cache-busting label syntax. DNS operators
can see prompts and answers. The server's existing IP rate limit applies to the
recursive resolver, not to the end user. Consequently busy shared resolvers may
be throttled; refusals and provider errors are not successful cached TXT answers.
The delegation uses the existing single server, so it does not add redundancy.

Rollback: delete only the exact `ask.ch.at. NS` record from the change file
(change CREATE to DELETE in a separate copy). Leave the parent NS, A, SOA,
ACME TXT, and wildcard `q.ch.at` records untouched. Cached delegations can last
300 seconds after deletion. The code is backward-compatible with direct DNS
queries and does not require rollback merely to remove the delegation.

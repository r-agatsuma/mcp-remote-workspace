#!/usr/bin/env python3
"""Controller-only validation; never queries DNS or connects to a target."""
import ipaddress
import sys

split = sys.argv.index('--self')
resolvers = sys.argv[1:split]
for value in resolvers + sys.argv[split + 1:]:
    address = ipaddress.ip_address(value)
    if '%' in value or not address.is_global or address.is_multicast:
        raise SystemExit('Expected literal public unicast address')
if len(set(resolvers)) != len(resolvers):
    raise SystemExit('Duplicate resolver')

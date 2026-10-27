"""The deliberately small, deterministic deployment YAML/schema boundary.

PyYAML is already an Ansible/controller dependency. Inspect its events and node
graph; never construct arbitrary YAML objects or pass the YAML to Ansible.
Only canonical decimal integers, strings, lists and string-key mappings exist.
"""
import base64
import ipaddress
import re

import yaml

BOOTSTRAP = frozenset({
    'mcp_source_revision', 'mcp_target_ssh_host_key_sha256',
    'mcp_data_device_id', 'mcp_data_expected_serial', 'mcp_data_expected_sector_bytes',
})
FINAL = BOOTSTRAP | {
    'mcp_data_uuid', 'mcp_runner_subuid_start', 'mcp_runner_subgid_start',
    'mcp_host_public_ips', 'mcp_tunnel_id',
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def mapping(value, keys):
    require(type(value) is dict and set(value) == set(keys),
            'exact stage/schema key set required; missing, unknown, policy and secret keys are forbidden')


def match(value, pattern):
    return type(value) is str and re.fullmatch(pattern, value) is not None


def fingerprint(value):
    if not match(value, r'SHA256:[A-Za-z0-9+/]{43}'):
        return False
    decoded = base64.b64decode(value[7:] + '=', validate=True)
    return len(decoded) == 32 and base64.b64encode(decoded).decode().rstrip('=') == value[7:]


def literal_ip(value):
    require(type(value) is str and '%' not in value, 'native unscoped literal IP required')
    try:
        address = ipaddress.ip_address(value)
    except ValueError:
        raise ValueError('native literal IP required') from None
    require(not (address.version == 6 and address.ipv4_mapped), 'native IP required, not IPv4-mapped IPv6')
    return address


def parse(raw):
    """One interpretation, including unconditional duplicate rejection at depth."""
    require(type(raw) is bytes and len(raw) <= 65536, 'input must be at most 64 KiB')
    try:
        source = raw.decode('utf-8')
        require(not source.startswith('\ufeff'), 'UTF-8 BOM is not accepted')
        # Unknown directives are discarded by the parser before document events.
        for token in yaml.scan(source, Loader=yaml.SafeLoader):
            require(not isinstance(token, yaml.tokens.DirectiveToken), 'YAML directives are forbidden')
        documents = 0
        for event in yaml.parse(source, Loader=yaml.SafeLoader):
            require(not isinstance(event, yaml.AliasEvent), 'aliases are forbidden')
            require(getattr(event, 'anchor', None) is None, 'anchors are forbidden')
            require(getattr(event, 'tag', None) is None, 'explicit/custom tags are forbidden')
            if isinstance(event, yaml.DocumentStartEvent):
                documents += 1
                require(not event.version and not event.tags, 'YAML directives are forbidden')
        require(documents == 1, 'exactly one YAML document required')
        node = yaml.compose(source, Loader=yaml.SafeLoader)

        def construct(item):
            if isinstance(item, yaml.MappingNode):
                result = {}
                for key_node, value_node in item.value:
                    require(isinstance(key_node, yaml.ScalarNode)
                            and key_node.tag == 'tag:yaml.org,2002:str', 'string mapping key required')
                    key = key_node.value
                    require(key != '<<', 'merge keys are forbidden')
                    require(key not in result, 'duplicate mapping key')
                    result[key] = construct(value_node)
                return result
            if isinstance(item, yaml.SequenceNode):
                return [construct(child) for child in item.value]
            require(isinstance(item, yaml.ScalarNode), 'unsupported YAML node')
            if item.tag == 'tag:yaml.org,2002:int':
                require(match(item.value, r'0|[1-9][0-9]*'), 'canonical unsigned decimal integer required')
                return int(item.value)
            require(item.tag == 'tag:yaml.org,2002:str', 'only strings and canonical integers are accepted')
            require(not any(c in item.value for c in ('{{', '{%', '{#')),
                    'template expressions are not deployment data')
            require(not any(ord(c) < 32 or ord(c) == 127 for c in item.value),
                    'control characters are not deployment data')
            return item.value

        value = construct(node)
        require(type(value) is dict, 'one top-level mapping required')
        return value
    except (yaml.YAMLError, UnicodeError, RecursionError):
        # Do not echo private input or parser excerpts into diagnostics.
        raise ValueError('malformed or unsupported YAML') from None


def validate(raw, stage):
    require(stage in ('bootstrap', 'final'), 'unknown manifest stage')
    value = parse(raw)
    mapping(value, BOOTSTRAP if stage == 'bootstrap' else FINAL)
    require(match(value['mcp_source_revision'], r'[0-9a-f]{40}'), 'source revision must be 40 lowercase hex')
    require(fingerprint(value['mcp_target_ssh_host_key_sha256']), 'canonical SSH SHA256 fingerprint required')
    identity = value['mcp_data_device_id']
    require(match(identity, r'/dev/disk/by-id/[A-Za-z0-9_.:+-]+')
            and identity.rsplit('/', 1)[1] not in ('.', '..')
            and not re.search(r'-part[0-9]+$', identity), 'whole-device stable by-id required')
    serial = value['mcp_data_expected_serial']
    require(type(serial) is str and serial.strip() == serial and bool(serial), 'exact nonempty serial required')
    sector = value['mcp_data_expected_sector_bytes']
    require(type(sector) is int and sector in (512, 4096), 'integer sector 512 or 4096 required')
    if stage == 'final':
        require(match(value['mcp_data_uuid'], r'[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}'),
                'lowercase filesystem UUID required')
        for key in ('mcp_runner_subuid_start', 'mcp_runner_subgid_start'):
            start = value[key]
            require(type(start) is int and 0 < start <= 2**32 - 1 - 65536,
                    'positive subordinate start within the reference allocation range required')
        addresses = value['mcp_host_public_ips']
        require(type(addresses) is list and bool(addresses), 'nonempty public IP list required')
        parsed = [literal_ip(address) for address in addresses]
        require(len(set(parsed)) == len(parsed), 'duplicate public IP')
        require(match(value['mcp_tunnel_id'], r'tunnel_[0-9a-f]{32}'), 'exact Tunnel ID required')
    return value


def locator(raw):
    """Input is a locator mapping, never an Ansible inventory/variable source."""
    value = parse(raw)
    mapping(value, {'host', 'port', 'user'})
    require(type(value['port']) is int and 1 <= value['port'] <= 65535, 'valid SSH port required')
    require(match(value['user'], r'[a-z_][a-z0-9_-]*'), 'literal SSH user required')
    host = value['host']
    require(type(host) is str, 'literal IP or DNS locator required')
    try:
        literal_ip(host)
    except ValueError:
        require(len(host) <= 253 and all(match(label, r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?')
                for label in host.rstrip('.').split('.')), 'literal IP or DNS locator required')
    return value

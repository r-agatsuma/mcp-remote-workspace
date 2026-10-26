#!/usr/bin/python3
"""Read-only stage-specific Data evidence before any deployment mutation."""
from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.mcp_data import argument_spec, filesystem, inputs, inspect, require, validate


def check(identity, serial, size, sector, stage, uuid):
    require(stage in ('bootstrap', 'final'), 'explicit deployment stage required')
    inputs(identity, serial, size, sector)
    evidence = validate(inspect(identity), identity, serial, size, sector, stage == 'bootstrap')
    if stage == 'final':
        filesystem(evidence, uuid)
    else:
        require(uuid == '', 'bootstrap must not consume a final UUID')
    return evidence


def main():
    spec = argument_spec()
    spec.update(stage=dict(type='str', required=True), uuid=dict(type='str', default=''))
    module = AnsibleModule(argument_spec=spec, supports_check_mode=True)
    try:
        module.exit_json(changed=False, evidence=check(**module.params))
    except Exception as exc:
        module.fail_json(msg='STOP: ' + str(exc))


if __name__ == '__main__':
    main()

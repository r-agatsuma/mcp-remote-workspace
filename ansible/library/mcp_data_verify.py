#!/usr/bin/python3
"""Read-only selected-Data verification, including the hidden Boot guard."""
from ansible.module_utils.basic import AnsibleModule
from ansible.module_utils.mcp_data import (argument_spec, filesystem,
                                          guard, inputs, inspect, require, validate)


def main():
    spec = argument_spec()
    spec.update(uuid=dict(type='str', required=True), guard=dict(type='bool', default=False),
                mounted=dict(type='bool', default=False))
    module = AnsibleModule(argument_spec=spec, supports_check_mode=False)
    p = module.params
    try:
        inputs(p['identity'], p['serial'], p['size'], p['sector'])
        snapshot = validate(inspect(p['identity']), p['identity'], p['serial'], p['size'], p['sector'], False)
        filesystem(snapshot, p['uuid'])
        changed = guard(snapshot, create=True) if p['guard'] else False
        if p['mounted']:
            require(len(snapshot['mounts']) == 1, 'Data must be mounted before any runtime path is created')
        module.exit_json(changed=changed, device=snapshot['target'], uuid=p['uuid'])
    except Exception as exc:
        module.fail_json(msg=str(exc))


if __name__ == '__main__':
    main()

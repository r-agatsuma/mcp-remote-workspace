#!/usr/bin/env python3
"""Source-only tests: never invoke Podman, Buildah, firewall loaders or Tunnel.

Representative rendering is not host convergence or runtime evidence.
"""
import importlib.util
import ipaddress
from pathlib import Path
import json
import os
import shutil
import shlex
import subprocess
import sys
import tempfile
import unittest

import jinja2
import yaml

ROOT = Path(__file__).resolve().parents[2]
ANSIBLE = ROOT / 'ansible'
ROLE = ANSIBLE / 'roles/mcp'
spec = importlib.util.spec_from_file_location('firewall', ANSIBLE / 'filter_plugins/mcp_firewall.py')
firewall = importlib.util.module_from_spec(spec)
spec.loader.exec_module(firewall)


def text(path):
    return (ROOT / path).read_text()


def fixture():
    values = yaml.safe_load((ROLE / 'defaults/main.yml').read_text())
    values.update(yaml.safe_load((ROLE / 'vars/main.yml').read_text()))
    values.update(mcp_runner_uid=2100, mcp_runner_gid=2200,
                  mcp_tunnel_uid=2101, mcp_tunnel_gid=2201,
                  mcp_source_revision='f1f5f7567e5cc955dbff1f96d54897b9d2565926',
                  mcp_data_uuid='00000000-0000-4000-8000-000000000001',
                  mcp_tunnel_id='tunnel_STATICFIXTURE',
                  mcp_image='registry.example.invalid/canonical@sha256:' + 'a'*64)
    return values


def render_all(destination):
    values = fixture()
    env = jinja2.Environment(loader=jinja2.FileSystemLoader(ROLE / 'templates'),
                             undefined=jinja2.StrictUndefined,
                             keep_trailing_newline=True, trim_blocks=True)
    env.filters['quote'] = shlex.quote
    rendered = {}
    for path in (ROLE / 'templates').glob('*.j2'):
        value = env.get_template(path.name).render(**values)
        name = path.name.removesuffix('.j2')
        rendered[name] = value
        (destination / name).write_text(value)
    baseline = '*filter\n:INPUT DROP [0:0]\n:FORWARD DROP [0:0]\n:OUTPUT ACCEPT [0:0]\n-A INPUT -i lo -j ACCEPT\nCOMMIT\n'
    for family in (4, 6):
        value = firewall.render(baseline, family, 2100, [],
                                values[f'mcp_nonpublic_v{family}'],
                                values[f'mcp_public_protocol_v{family}'])
        rendered[f'rules.v{family}'] = value
        (destination / f'rules.v{family}').write_text(value)
    return rendered


class DeploymentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix='mcp-static-')
        cls.directory = Path(cls.temporary.name)
        cls.rendered = render_all(cls.directory)
        cls.values = fixture()
        cls.tasks = '\n'.join(p.read_text() for p in (ROLE / 'tasks').glob('*.yml'))

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def test_yaml_and_python_parse(self):
        for path in ANSIBLE.rglob('*.yml'):
            yaml.safe_load(path.read_text())
        for path in ANSIBLE.rglob('*.py'):
            compile(path.read_text(), str(path), 'exec')
        for name in ('mcp-data-check', 'mcp-store-check'):
            path = ROLE / 'files' / name
            compile(path.read_text(), str(path), 'exec')

    def test_01_02_exact_source_and_toolchain(self):
        self.assertIn('git, -C', self.tasks)
        self.assertIn("mcp_revision.stdout == mcp_source_revision", self.tasks)
        self.assertEqual(self.values['mcp_go_version'], 'go1.26.9')
        for path in ANSIBLE.rglob('*'):
            if path.is_file() and path.suffix not in ('.pyc',):
                self.assertNotIn('@' + 'latest', path.read_text())
        self.assertRegex(self.values['mcp_go_sha256'], r'^[a-f0-9]{64}$')

    def test_03_04_05_packages_and_credential_separation(self):
        self.assertIn('buildah', self.values['mcp_runtime_packages'])
        self.assertIn('/usr/bin/buildah', self.tasks)
        self.assertNotEqual(self.values['mcp_runner'], self.values['mcp_tunnel'])
        tasks = yaml.safe_load((ROLE / 'tasks/tunnel.yml').read_text())
        credential = next(t for t in tasks if 'Provision runtime credential' in t['name'])
        copy = credential['ansible.builtin.copy']
        self.assertEqual((copy['owner'], copy['mode'], copy['force']), ('mcp-tunnel', '0600', False))
        self.assertTrue(credential['no_log'])
        self.assertFalse(credential['diff'])
        self.assertIn('EnvironmentFile=/etc/mcp-tunnel/runtime.env', self.rendered['mcp-tunnel.service'])
        self.assertNotIn('API_KEY', self.rendered['mcp-workspaced.service'])

    def test_06_07_17_loopback_product_and_tunnel_endpoint(self):
        self.assertIn('-daemon -mcp-listen 127.0.0.1:8080', self.rendered['mcp-workspaced.service'])
        profile = yaml.safe_load(self.rendered['tunnel-profile.yml'])
        self.assertEqual(profile['mcp']['server_urls'], [{'channel': 'main', 'url': 'http://127.0.0.1:8080/mcp'}])
        self.assertEqual(profile['control_plane']['api_key'], 'env:CONTROL_PLANE_API_KEY')
        self.assertEqual(profile['health']['listen_addr'], '127.0.0.1:0')
        self.assertNotIn('API_KEY', next(l for l in self.rendered['mcp-tunnel.service'].splitlines() if l.startswith('ExecStart=')))

    def test_08_09_data_identity_guard_and_no_ordinary_format(self):
        self.assertIn('What=/dev/disk/by-uuid/' + self.values['mcp_data_uuid'], self.rendered['srv-mcp\\x2dpodman.mount'])
        self.assertIn('Options=nodev,nosuid,noatime', self.rendered['srv-mcp\\x2dpodman.mount'])
        self.assertIn("mode: '0000'", self.tasks)
        self.assertIn('/usr/local/libexec/mcp-data-check', self.rendered['mcp-launch'])
        self.assertNotIn('mkfs', self.tasks)
        self.assertNotIn('bootstrap-data', text('ansible/playbooks/deploy.yml'))
        self.assertNotIn('wipefs', self.tasks)
        bootstrap = text('ansible/scripts/bootstrap-data.py')
        for gate in ('disk[\'type\'] == \'disk\'', 'signatures == []', 'os.O_EXCL', 'disk[\'serial\'] == serial'):
            self.assertIn(gate, bootstrap)

    def test_10_storage_context_and_no_ambient_volumes(self):
        conf = self.rendered['storage.conf']
        self.assertIn('driver = "overlay"', conf)
        self.assertIn('graphroot = "/srv/mcp-podman/runner/storage"', conf)
        self.assertIn('runroot = "/run/user/2100/containers"', conf)
        for key in ('image_copy_tmp_dir', 'events_logfile_path'):
            self.assertRegex(self.rendered['containers.conf'], key + r' = "/srv/mcp-podman/')
        self.assertIn('mounts = []', self.rendered['containers.conf'])
        self.assertIn('mcp-store-check', self.rendered['mcp-image-prepare.service'])

    def test_11_12_manager_bus_and_exact_build_policy(self):
        unit = self.rendered['mcp-workspaced.service']
        self.assertIn('user@2100.service', unit)
        self.assertIn('DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/2100/bus', unit)
        self.assertIn('Delegate=cpu memory pids', self.rendered['runner-manager.conf'])
        self.assertIn('loginctl enable-linger mcp-runner', self.tasks)
        self.assertIn('-pids-max 512', self.rendered['mcp-launch'])
        self.assertNotIn('Slice=mcpws', unit)
        source = text('internal/workspace/build_parent_linux.go')
        self.assertIn('"TasksMax=" + strconv.FormatUint(policy.PIDsMax, 10)', source)

    def test_13_14_firewall_and_gid_no_dns_exception(self):
        for family in (4, 6):
            rules = self.rendered[f'rules.v{family}']
            self.assertIn('--uid-owner 2100', rules)
            self.assertIn('--dst-type LOCAL', rules)
            self.assertIn('--ctdir REPLY', rules)
            self.assertNotIn('--dport 53', rules)
            self.assertNotIn('127.0.0.53', rules)
            self.assertNotIn('-A INPUT -j', rules)
        self.assertIn('net.ipv4.ping_group_range = 2200 2200', self.rendered['ping-group.conf'])
        self.assertIn('netfilter-persistent.service', self.tasks)

    def test_firewall_policy_classes_and_order(self):
        for family in (4, 6):
            lines = self.rendered[f'rules.v{family}'].splitlines()
            self.assertIn('-A OUTPUT -m owner --uid-owner 2100 -j MCP_RUNNER', lines)
            rules = [l for l in lines if l.startswith('-A MCP_RUNNER ')]
            self.assertIn('--ctdir REPLY', rules[0])
            self.assertIn('--dst-type LOCAL', rules[1])
        def denied(family, address):
            ip = ipaddress.ip_address(address)
            for line in self.rendered[f'rules.v{family}'].splitlines():
                args = shlex.split(line)
                if '-d' in args and ip in ipaddress.ip_network(args[args.index('-d')+1]):
                    return args[args.index('-j')+1] == 'REJECT'
            return False
        for ip in ('10.1.2.3', '169.254.169.254', '192.0.0.8', '198.18.0.1', '224.0.0.1', '240.0.0.1'):
            self.assertTrue(denied(4, ip), ip)
        for ip in ('::1', 'fc00::1', 'fe80::1', 'fec0::1', '2001:2::1', '2001:db8::1', '3fff::1'):
            self.assertTrue(denied(6, ip), ip)
        for ip in ('8.8.8.8', '8.8.4.4', '192.0.0.9'):
            self.assertFalse(denied(4, ip), ip)
        self.assertFalse(denied(6, '2001:4860:4860::8888'))
        baseline = '*filter\n:INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n'
        changed = baseline.replace(':OUTPUT ACCEPT', ':OUTPUT DROP')
        with self.assertRaises(ValueError):
            firewall.render(changed, 4, 2100, [], [], [])
        for family in (4, 6):
            first = firewall.normalize(self.rendered[f'rules.v{family}'])
            self.assertEqual(first, firewall.normalize(first))

    def test_15_16_trusted_dns_separate_host_and_application_surfaces(self):
        self.assertEqual(self.values['mcp_public_dns'], ['8.8.8.8', '8.8.4.4'])
        for ip in self.values['mcp_public_dns']:
            self.assertIn('-public-dns ' + ip, self.rendered['mcp-launch'])
        bind = 'BindReadOnlyPaths=/etc/mcp-deployment/resolv.conf:/etc/resolv.conf'
        self.assertIn(bind, self.rendered['mcp-workspaced.service'])
        self.assertIn(bind, self.rendered['runner-manager.conf'])
        self.assertIn(bind, self.rendered['mcp-image-prepare.service'])
        self.assertEqual([line for line in self.rendered['resolv.conf'].splitlines() if line.startswith('nameserver ')],
                         ['nameserver 8.8.8.8', 'nameserver 8.8.4.4'])

    def test_18_19_20_pinned_tunnel_no_extra_runtime_authority(self):
        self.assertEqual(self.values['mcp_tunnel_version'], 'v0.0.15')
        self.assertIn('/v0.0.15/tunnel-client-v0.0.15-linux-arm64.zip', self.tasks)
        for value in self.rendered.values():
            self.assertNotIn('AmbientCapabilities=', value)
            self.assertNotIn('CAP_NET_ADMIN', value)
            self.assertNotIn('podman.sock', value)
            self.assertNotIn('docker.sock', value)
            self.assertNotIn('embedded-mcp-stub', value)
        self.assertIn('--profile-file /etc/mcp-tunnel/profile.yml', self.rendered['mcp-tunnel.service'])

    def test_21_recovery_is_existing_cli(self):
        docs = text('docs/deployment.md')
        for action in ('inspect', 'backend', 'storage', 'cleanup'):
            self.assertIn('-recover ' + action, docs)
        self.assertIn('exec /usr/local/libexec/mcp-workspaced "$@"', self.rendered['mcp-launch'])

    def test_22_no_literal_secret_and_23_provenance(self):
        for path in ANSIBLE.rglob('*'):
            if path.is_file() and path.suffix != '.pyc':
                self.assertNotRegex(path.read_text(), r'sk-[A-Za-z0-9_-]{20,}')
        build = (ROLE / 'files/build-candidate.sh').read_text()
        for key in ('source_revision', 'go_toolchain', 'binary_sha256', 'source_archive_sha256', 'go_sum_sha256'):
            self.assertIn(key, build)
        self.assertIn('readelf -l -d', build)
        self.assertIn('ldd mcp-workspaced', build)
        self.assertIn('go mod verify', build)
        self.assertIn("mode: '0755'", (ROLE / 'tasks/build.yml').read_text())

    def test_real_ansible_template_rendering(self):
        executable = shutil.which('ansible-playbook')
        if not executable:
            self.skipTest('SKIPPED — tooling unavailable: ansible-playbook')
        with tempfile.TemporaryDirectory(prefix='mcp-ansible-render-') as directory:
            values = dict(self.values, mcp_render_dir=directory)
            variables = Path(directory) / 'fixture.json'
            variables.write_text(json.dumps(values))
            environment = dict(os.environ, ANSIBLE_CONFIG=str(ANSIBLE / 'ansible.cfg'))
            subprocess.run([executable, '-i', 'localhost,', 'tests/render.yml',
                            '-e', '@' + str(variables)], cwd=ANSIBLE, env=environment,
                           check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            for name, expected in self.rendered.items():
                self.assertEqual((Path(directory) / name).read_text(), expected, name)

    def test_launch_shell_and_exact_flags(self):
        for name in ('mcp-launch', 'mcp-image-prepare'):
            subprocess.run(['/bin/sh', '-n', str(self.directory / name)], check=True)
        subprocess.run(['/bin/bash', '-n', str(ROLE / 'files/build-candidate.sh')], check=True)
        launch = self.rendered['mcp-launch']
        # Every rendered policy flag is a real landed CLI flag, not a guessed unit option.
        source = text('cmd/mcp-workspaced/main.go')
        for flag in self.values['mcp_policy']:
            self.assertIn('"' + flag + '"', source)
            self.assertIn('-' + flag + ' ', launch)
        self.assertNotIn('\\\\\n', launch)


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--render':
        destination = Path(sys.argv[2])
        destination.mkdir(parents=True, exist_ok=True)
        render_all(destination)
    else:
        unittest.main()

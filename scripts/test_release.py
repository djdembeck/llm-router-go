"""Real temporary git-history release reservation regressions; no production writes."""


import os
from pathlib import Path

import subprocess
import sys
import tempfile

import unittest

import release

class LocalGitHub(release.Repository):

    def __init__(self, root, remote):
        super().__init__(root)
        self.remote = remote
        self.releases = []
        self.release_commands = []
        self.latest = None

    def repository(self):
        return 'example/m4b-merge'

    def remote_git(self, *args):
        return self.run('git', '--git-dir', str(self.remote), *args)

    def api(self, endpoint, method='GET', fields=None):
        path = endpoint.removeprefix('repos/example/m4b-merge/')
        if path == 'git/refs' and method == 'POST':
            self.remote_git('update-ref', fields['ref'], fields['sha'], '0' * 40)
            return {'ref': fields['ref']}
        if path.startswith('git/ref/'):
            ref = path.removeprefix('git/ref/')
            return {'object': {'type': 'commit', 'sha': self.remote_git('rev-parse', f'refs/{ref}^{{commit}}')}}
        if path.startswith('releases/tags/'):
            # GitHub documents releases/tags/{tag} as a published-release lookup.
            tag = path.removeprefix('releases/tags/')
            for record in self.releases:
                if record['tag_name'] == tag and not record['draft']:
                    return dict(record)
            raise release.ReleaseError('HTTP 404: Not Found')
        if path == 'releases/latest':
            record = next((r for r in self.releases if r['tag_name'] == self.latest), None) \
                if self.latest is not None else None
            if record is None:
                raise release.ReleaseError('HTTP 404: Not Found')
            return dict(record)
        if path.startswith('releases?'):
            page = int(endpoint.rsplit('page=', 1)[1])
            start = (page - 1) * 100
            return [dict(record) for record in self.releases[start:start + 100]]
        raise AssertionError((endpoint, method, fields))

    def run(self, *args, ok=(0,)):
        if len(args) >= 4 and args[0] == 'gh' and args[1] == 'release' and args[2] in ('create', 'edit'):
            tag, existing = args[3], [record for record in self.releases
                                      if record['tag_name'] == args[3]]
            if args[2] == 'create':
                if existing:
                    raise release.ReleaseError(f'gh release create: release {tag} already exists')
                self.release_commands.append(('create', args))
                self.releases.append({'tag_name': tag, 'draft': '--draft' in args, 'prerelease': False})
                return ''
            self.release_commands.append((args[2], args))
            record = existing[0]
            if '--draft=false' in args:
                # Newly-published releases default to GitHub Latest unless
                # the edit explicitly opts out.
                if record['draft'] and '--latest=false' not in args:
                    self.latest = record['tag_name']
                record['draft'] = False
            elif '--latest=true' in args:
                self.latest = record['tag_name']
            return ''
        return super().run(*args, ok=ok)

class ReleaseBehavior(unittest.TestCase):

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='m4b-reservation-')
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.remote = root / 'remote.git'
        self.checkout = root / 'checkout'
        self.cmd(root, 'git', 'init', '--bare', str(self.remote))
        self.cmd(root, 'git', 'init', '-b', 'main', str(self.checkout))
        self.repo = LocalGitHub(self.checkout, self.remote)
        for key, value in (('user.name', 'Release Test'), ('user.email', 'test@example.invalid'), ('commit.gpgsign', 'false'), ('tag.gpgsign', 'false')):
            self.git('config', key, value)
        self.git('remote', 'add', 'origin', str(self.remote))
        self.commit('initial', 'one')
        self.git('tag', 'v1.0.0')
        self.git('push', 'origin', 'main', '--tags')
        self.git('switch', '-c', 'develop')
        self.commit('feat: new behavior', 'two')
        self.git('push', 'origin', 'develop')
        self.git('switch', 'main')
        self.git('merge', '--no-ff', 'develop', '-m', 'Promote develop')
        self.git('push', 'origin', 'main')
        self.source = self.git('rev-parse', 'HEAD')
        self.repo.fetch()

    def cmd(self, cwd, *args):
        result = subprocess.run(args, cwd=cwd, env=release.isolated_git_environment(), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def git(self, *args):
        return self.cmd(self.checkout, 'git', *args)

    def commit(self, message, content):
        (self.checkout / 'source.txt').write_text(content)
        self.git('add', 'source.txt')
        self.git('commit', '-m', message)

    def next_develop(self):
        self.git('switch', 'develop')
        self.commit('fix: next behavior', 'three')
        self.git('push', 'origin', 'develop')
        self.repo.fetch()
        return self.git('rev-parse', 'HEAD')

    def test_reservation_reuses_exact_source_after_newer_release(self):
        tag = self.repo.reserve(self.source)
        self.assertEqual(tag, 'v1.1.0')
        self.next_develop()
        self.git('switch', 'main')
        self.git('merge', '--no-ff', 'develop', '-m', 'Next promotion')
        self.git('push', 'origin', 'main')
        newer = self.git('rev-parse', 'HEAD')
        self.assertEqual(self.repo.reserve(newer), 'v1.1.1')
        self.assertEqual(self.repo.reserve(self.source), tag)
        self.assertEqual(self.repo.remote_git('rev-parse', 'refs/tags/v1.1.0'), self.source)

    def test_unfinished_reservation_is_not_reallocated(self):
        self.assertEqual(self.repo.reserve(self.source), 'v1.1.0')
        self.assertEqual(self.repo.reserve(self.source, bump='major'), 'v1.1.0')
        self.assertEqual(self.repo.select(self.source, 'v1.1.0'), 'v1.1.0')
        with self.assertRaises(release.ReleaseError):
            self.repo.reserve(self.source, 'v2.0.0')

    def test_other_branch_tag_cannot_set_reachable_baseline(self):
        self.git('switch', '-c', 'unrelated', 'v1.0.0')
        self.commit('unrelated', 'elsewhere')
        self.git('tag', 'v99.0.0')
        self.git('push', 'origin', 'v99.0.0')
        self.repo.fetch()
        self.assertEqual(self.repo.select(self.source), 'v1.1.0')

    def test_explicit_different_source_collision_never_moves_tag(self):
        with self.assertRaises(release.ReleaseError):
            self.repo.reserve(self.source, 'v1.0.0')
        self.assertEqual(self.repo.remote_git('rev-parse', 'refs/tags/v1.0.0'), self.git('rev-parse', 'v1.0.0'))

    def test_ambiguous_exact_source_reservations_fail(self):
        for tag in ('v1.1.0', 'v1.2.0'):
            self.git('tag', tag, self.source)
            self.git('push', 'origin', tag)
        self.repo.fetch()
        with self.assertRaises(release.ReleaseError):
            self.repo.select(self.source)

    def test_release_refuses_tag_pointing_at_another_source(self):
        initial = self.git('rev-parse', 'v1.0.0')
        self.assertNotEqual(initial, self.source)
        with self.assertRaisesRegex(release.ReleaseError, 'does not point at the frozen source'):
            self.repo.release('v1.0.0', self.source, publish=True)
        self.assertEqual(self.repo.remote_git('rev-parse', 'refs/tags/v1.0.0'), initial)

    def test_unreserved_automatic_candidate_is_coalesced_but_manual_is_not(self):
        old = self.source
        self.next_develop()
        self.git('switch', 'main')
        self.git('merge', '--no-ff', 'develop', '-m', 'Next promotion')
        self.git('push', 'origin', 'main')
        self.repo.reserve(self.git('rev-parse', 'HEAD'), 'v1.2.0')
        self.repo.fetch()
        self.assertIsNone(self.repo.select(old, automatic=True))
        self.assertEqual(self.repo.select(old), 'v1.1.0')

    def test_release_publish_cycle_advances_latest_for_highest_release(self):
        # v1.10.0 beats the published v1.9.0 numerically, not lexically.
        self.repo.releases.append({'tag_name': 'v1.9.0', 'draft': False, 'prerelease': False})
        self.repo.latest = 'v1.9.0'
        self.assertEqual(self.repo.reserve(self.source, version='v1.10.0'), 'v1.10.0')
        self.repo.fetch()
        self.repo.release('v1.10.0', self.source)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create'])
        draft = self.repo.releases[-1]
        self.assertTrue(draft['draft'])
        self.repo.release('v1.10.0', self.source)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create'])
        self.assertIs(self.repo.releases[-1], draft)
        self.repo.release('v1.10.0', self.source, publish=True)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create', 'edit'])
        self.assertFalse(draft['draft'])
        self.assertEqual(self.repo.api('repos/example/m4b-merge/releases/latest')['tag_name'],
                         'v1.10.0')
        self.repo.release('v1.10.0', self.source, publish=True)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create', 'edit'])
        self.assertEqual(len(self.repo.releases), 2)
        self.assertEqual(self.repo.remote_git('rev-parse', 'refs/tags/v1.10.0'), self.source)

    def test_delayed_lower_publish_does_not_rollback_latest(self):
        self.repo.releases.append({'tag_name': 'v1.3.0', 'draft': False, 'prerelease': False})
        self.repo.releases.append({'tag_name': 'v1.3.1-rc.1', 'draft': False, 'prerelease': True})
        self.repo.releases.append({'tag_name': 'v9.9.9', 'draft': True, 'prerelease': False})
        self.repo.latest = 'v1.3.0'
        initial = self.git('rev-parse', 'v1.0.0')
        self.assertEqual(self.repo.api('repos/example/m4b-merge/releases/latest')['tag_name'],
                         'v1.3.0')
        self.repo.release('v1.0.0', initial)
        self.repo.release('v1.0.0', initial, publish=True)
        self.assertIn('--latest=false', self.repo.release_commands[-1][1])
        self.assertEqual(self.repo.api('repos/example/m4b-merge/releases/latest')['tag_name'],
                         'v1.3.0')

    def test_release_listing_paginates_beyond_one_page(self):
        self.repo.releases.extend({'tag_name': f'v2.{index // 100}.{index % 100}',
                                   'draft': False, 'prerelease': False} for index in range(205))
        self.repo.latest = 'v2.2.4'
        initial = self.git('rev-parse', 'v1.0.0')
        self.assertEqual(self.repo.api('repos/example/m4b-merge/releases/latest')['tag_name'],
                         'v2.2.4')
        self.repo.release('v1.0.0', initial)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create'])
        draft = self.repo.releases[-1]
        self.assertTrue(draft['draft'])
        self.repo.release('v1.0.0', initial)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create'])
        self.assertIs(self.repo.releases[-1], draft)
        self.repo.release('v1.0.0', initial, publish=True)
        self.assertEqual([name for name, _ in self.repo.release_commands], ['create', 'edit'])
        self.assertIn('--latest=false', self.repo.release_commands[-1][1])
        self.assertEqual(self.repo.api('repos/example/m4b-merge/releases/latest')['tag_name'],
                         'v2.2.4')

    def test_duplicate_release_records_fail_closed_without_writes(self):
        records = [{'tag_name': 'v1.0.0', 'draft': False, 'prerelease': False},
                   {'tag_name': 'v1.0.0', 'draft': True, 'prerelease': False}]
        self.repo.releases.extend(records)
        initial = self.git('rev-parse', 'v1.0.0')
        with self.assertRaisesRegex(release.ReleaseError, r'Multiple release records share tag v1\.0\.0'):
            self.repo.release('v1.0.0', initial, publish=True)
        self.assertEqual(self.repo.release_commands, [])
        self.assertEqual(self.repo.releases, records)

    def test_listing_failure_propagates_without_release_writes(self):
        def fail(repository):
            raise release.ReleaseError('HTTP 403: Resource forbidden by rate limit')

        self.repo.release_records = fail
        initial = self.git('rev-parse', 'v1.0.0')
        with self.assertRaisesRegex(release.ReleaseError, 'HTTP 403'):
            self.repo.release('v1.0.0', initial, publish=True)
        self.assertEqual(self.repo.release_commands, [])

    def test_registry_failure_preserves_captured_streams_at_cli_boundary(self):
        stub = self.checkout / 'docker'
        stub.write_text("#!/bin/sh\necho 'buildx stdout-marker-72'\necho 'unauthorized: authentication required stderr-marker-73' >&2\nexit 1\n")
        stub.chmod(0o755)
        self.git('remote', 'set-url', 'origin', 'git@github.com:example/m4b-merge.git')
        environment = release.isolated_git_environment()
        environment['PATH'] = f"{self.checkout}" + os.pathsep + environment.get('PATH', '')
        try:
            result = subprocess.run([sys.executable, release.__file__, 'image',
                                     '--source', self.source, '--version', 'v9.9.9'],
                                    cwd=self.checkout, env=environment, text=True, capture_output=True)
        finally:
            stub.unlink()
            self.git('remote', 'set-url', 'origin', self.remote)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('stderr-marker-73', result.stderr)
        self.assertIn('stdout-marker-72', result.stderr)


if __name__ == "__main__":
    unittest.main()

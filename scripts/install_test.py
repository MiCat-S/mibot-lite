#!/usr/bin/env python3
"""离线执行安装脚本：所有外部命令使用临时桩，不访问网络或系统服务。"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("install.sh").read_text()


class InstallerTest(unittest.TestCase):
    def run_installer(self, failure="", no_service=False, fresh=False):
        with tempfile.TemporaryDirectory(prefix="mibot-install-") as folder:
            temp = Path(folder)
            root, tools = temp / "deployment", temp / "bin"
            root.mkdir()
            tools.mkdir()
            (root / "config.json").write_text('{"session":"existing"}')
            if not fresh:
                (root / "mibot-lite").write_text("old working binary\n")
            unit = temp / "mibot-lite.service"
            if not fresh:
                unit.write_text("old unit\n")
            state = temp / "state"
            state.write_text("inactive" if fresh else "active")
            asset = temp / "mibot-lite-linux-amd64"
            asset.write_text('#!/bin/bash\n[ "$FAILURE" != check ]\n')
            asset.chmod(0o755)
            (temp / "checksums.txt").write_text(
                hashlib.sha256(asset.read_bytes()).hexdigest() + "  " + asset.name + "\n")
            (temp / "release.json").write_text(json.dumps({
                "tag_name": "v999.0.0", "assets": [
                    {"browser_download_url": "https://fixture/" + asset.name},
                    {"browser_download_url": "https://fixture/checksums.txt"}]}, indent=2))
            stubs = {
                "uname": 'case "$1" in -s) echo Linux;; -m) echo x86_64;; esac',
                "curl": '''
url="$2"; out="$4"
case "$url" in
 *releases/latest) cp "$FIXTURE/release.json" "$out";;
 *checksums.txt) cp "$FIXTURE/checksums.txt" "$out";;
 *mibot-lite-linux-amd64) cp "$FIXTURE/mibot-lite-linux-amd64" "$out";;
 */deploy/mibot-lite.service)
  [ "$FAILURE" != template ] || exit 22
  printf '[Service]\\nWorkingDirectory=@ROOT@\\nExecStart=@BINARY@ --serve\\n' > "$out";;
 *) exit 22;;
esac''',
                "systemctl": '''
printf '%s\\n' "$*" >> "$FIXTURE/calls"
case "$1" in
 is-active) [ "$(cat "$FIXTURE/state")" = active ];;
 show) echo "$FIXTURE/deployment";;
 stop) echo stopped > "$FIXTURE/state";;
 enable)
  [ "$FAILURE" != start ] || exit 1
  echo active > "$FIXTURE/state";;
 start) echo active > "$FIXTURE/state";;
esac''',
                "systemd-analyze": '''
[ "$FAILURE" != verify ] || exit 1
binary=$(sed -n 's/^ExecStart=\\([^ ]*\\).*/\\1/p' "$2")
[ -x "$binary" ]''',
                "journalctl": '[ "$FAILURE" = readiness ] || echo msg=runtime.ready',
                "sleep": ':',
                "mv": '''
case "$1" in */candidate) [ "$FAILURE" != swap ] || exit 1;; esac
/bin/mv "$@"''',
            }
            for name, body in stubs.items():
                path = tools / name
                path.write_text("#!/bin/bash\n" + body + "\n")
                path.chmod(0o755)
            # 只替换 root 门槛和单元文件位置；被测控制流仍是实际安装脚本。
            script = SCRIPT.replace(
                '[ "${EUID:-$(id -u)}" = 0 ] || die "run as root (the service and its unit are installed system-wide)"',
                ': # 离线桩不需要 root')
            script = script.replace("UNIT=/etc/systemd/system/$SERVICE", 'UNIT="$FIXTURE/mibot-lite.service"')
            copied = temp / "install.sh"
            copied.write_text(script)
            env = dict(os.environ, PATH=str(tools) + ":" + os.environ["PATH"],
                       MIBOT_ROOT=str(root), FIXTURE=str(temp), FAILURE=failure)
            args = ["bash", str(copied)] + (["--no-service"] if no_service else [])
            result = subprocess.run(args, env=env, capture_output=True, text=True, timeout=15)
            calls = (temp / "calls").read_text() if (temp / "calls").exists() else ""
            binary = (root / "mibot-lite").read_text() if (root / "mibot-lite").exists() else ""
            return result, calls, binary, unit.read_text() if unit.exists() else "", state.read_text().strip()

    def test_preflight_failures_leave_running_deployment(self):
        for failure in ["check", "template", "verify"]:
            with self.subTest(failure=failure):
                result, calls, binary, unit, state = self.run_installer(failure)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertNotIn("stop ", calls)
                self.assertEqual(binary, "old working binary\n")
                self.assertEqual(unit, "old unit\n")
                self.assertEqual(state, "active")

    def test_post_switch_failures_restore_binary_unit_and_service(self):
        for failure in ["swap", "start", "readiness"]:
            with self.subTest(failure=failure):
                result, calls, binary, unit, state = self.run_installer(failure)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("start mibot-lite.service", calls)
                self.assertEqual(binary, "old working binary\n")
                self.assertEqual(unit, "old unit\n")
                self.assertEqual(state, "active")

    def test_success_keeps_candidate(self):
        result, calls, binary, unit, state = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotEqual(binary, "old working binary\n")
        self.assertNotEqual(unit, "old unit\n")
        self.assertEqual(state, "active")

    def test_fresh_install_verifies_candidate_before_publishing(self):
        result, calls, binary, unit, state = self.run_installer(fresh=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(binary)
        self.assertTrue(unit)
        self.assertEqual(state, "active")

    def test_no_service_does_not_touch_systemd(self):
        result, calls, binary, unit, state = self.run_installer(no_service=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(calls, "")
        self.assertNotEqual(binary, "old working binary\n")
        self.assertEqual(unit, "old unit\n")
        self.assertEqual(state, "active")


if __name__ == "__main__":
    unittest.main()

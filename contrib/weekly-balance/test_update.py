import json
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import update


class InPlaceInstallTest(unittest.TestCase):
    def test_config_and_app_survive_a_failed_switch(self):
        self.check_switch(fail=True)

    def test_successful_switch_reuses_existing_config(self):
        self.check_switch(fail=False)

    def check_switch(self, *, fail):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            app_dir = root / "Applications"
            config = root / ".config/magpie"
            state = config / "weekly-balance"
            staging = state / "stage"
            staging.mkdir(parents=True)
            layout = update.Installation(app_dir, config, state, True)
            replacement = staging / "unpacked" / update.APP
            for app, name in ((layout.app, "official"), (replacement, "fork")):
                app.mkdir(parents=True)
                (app / "build").write_text(name)
            settings = config / "settings.json"
            settings.write_text('{"theme":"dark","unknown":{"port":42}}')
            plugins = config / "plugins.json"
            plugins.write_text('{"plugins":[{"spec":"other-plugin"}]}')
            original_plugins = plugins.read_bytes()
            (staging / update.PLUGIN).write_text("fork plugin")

            def run(*args, **kwargs):
                if args[0] == "/usr/bin/ditto":
                    shutil.copytree(args[1], args[2])
                else:
                    data = json.loads(plugins.read_text())
                    data["plugins"].append({"spec": args[-1]})
                    plugins.write_text(json.dumps(data))

            def exchange(first, second):
                other = first.with_suffix(".swap")
                first.rename(other)
                second.rename(first)
                other.rename(second)

            def wait(app_root, address, version, *, timeout):
                kind = "fork" if "weekly" in version else "official"
                self.assertEqual((app_root / update.APP / "build").read_text(), kind)
                if fail and kind == "fork":
                    raise RuntimeError("unhealthy gateway")

            with (
                patch.object(update, "check_official", return_value="0.1.4533"),
                patch.object(update, "unpack", return_value=replacement),
                patch.object(update, "run", side_effect=run),
                patch.object(update, "exchange", side_effect=exchange),
                patch.object(update, "check_plugin"),
                patch.object(update, "restart"),
                patch.object(update, "wait_for_version", side_effect=wait),
            ):
                args = (layout, "127.0.0.1:3425", staging, "0.1.4533-weekly.2")
                if fail:
                    with self.assertRaisesRegex(RuntimeError, "unhealthy gateway"):
                        update.install_in_place(*args)
                else:
                    update.install_in_place(*args)

            self.assertEqual((state / "official" / update.APP / "build").read_text(), "official")
            self.assertEqual(layout.plugin.is_file(), not fail)
            self.assertFalse((app_dir / ".portable").exists())
            expected = {"theme": "dark", "unknown": {"port": 42}}
            if fail:
                self.assertEqual(plugins.read_bytes(), original_plugins)
            else:
                expected["noAutoUpdate"] = True
            self.assertEqual(json.loads(settings.read_text()), expected)

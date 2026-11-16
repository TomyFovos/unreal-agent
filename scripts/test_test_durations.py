import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import test_durations


class DurationTests(unittest.TestCase):
    def test_terminal_elapsed_sorting_top_twenty_and_package_times(self):
        events = [dict(Action="pass", Package="p", Test=f"Test{i:02}", Elapsed=i)
                  for i in range(25)]
        events += [dict(Action="pass", Package="p", Elapsed=99),
                   dict(Action="fail", Package="q", Elapsed=101),
                   dict(Action="pass", Package="p", Test="Test24/child", Elapsed=24.5),
                   dict(Action="pause", Package="p", Test="not terminal", Elapsed=999),
                   dict(Action="output", Package="p", Output="logs", Elapsed=999)]
        result = test_durations.summary(map(json.dumps, events))
        tests, packages = result.split("## Package elapsed time")
        self.assertEqual(tests.count("| pass |"), 20)
        self.assertLess(tests.index("Test24/child"), tests.index("Test24 |"))
        self.assertIn("Test06 |", tests)
        self.assertNotIn("Test05 |", tests)
        self.assertNotIn("999", result)
        self.assertIn("99.000 | pass | p", packages)
        self.assertLess(packages.index("q |"), packages.index("p |"))

    def test_failure_skip_partial_input_and_repeated_runs_are_not_lost(self):
        lines = ["build diagnostic\n", "{partial", "null", "[]"]
        lines += map(json.dumps, [
            dict(Action="fail", Package="p", Test="Test|failure", Elapsed=4),
            dict(Action="skip", Package="p", Test="TestSkip", Elapsed=1),
            dict(Action="pass", Package="p", Test="TestAgain", Elapsed=2),
            dict(Action="pass", Package="p", Test="TestAgain", Elapsed=3),
            dict(Action="fail", Package="p"),
            dict(Action="pass", Package="p", Test="invalid", Elapsed="slow"),
            dict(Action="pass", Package="p", Test="invalid", Elapsed=float("nan")),
        ])
        result = test_durations.summary(lines)
        self.assertIn(r"4.000 | fail | p / Test\|failure", result)
        self.assertIn("1.000 | skip | p / TestSkip", result)
        self.assertEqual(result.count("TestAgain"), 2)
        self.assertIn("0.000 | fail | p", result)
        self.assertNotIn("invalid", result)

    def test_stdin_and_unreadable_input_are_informational(self):
        with patch.object(sys, "stdin", io.StringIO('{}\n')), \
                patch.object(sys, "stdout", new_callable=io.StringIO) as output:
            self.assertEqual(test_durations.main(["-"]), 0)
        self.assertIn("Package elapsed time", output.getvalue())
        with patch.object(test_durations.Path, "open", side_effect=OSError("unreadable")), \
                patch.object(sys, "stderr", new_callable=io.StringIO) as error:
            self.assertEqual(test_durations.main(["missing"]), 0)
        self.assertIn("summary unavailable", error.getvalue())


class MakeTargetTests(unittest.TestCase):
    def run_target(self, status, summary_fails=False):
        root = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            go = directory / "go"
            go.write_text('#!/bin/sh\nprintf \'%s\\n\' \'{"Action":"pass","Package":"fake","Test":"TestCI","Elapsed":1}\'\nexit "$FAKE_GO_STATUS"\n')
            go.chmod(0o755)
            if summary_fails:
                python = directory / "python3"
                python.write_text("#!/bin/sh\nexit 9\n")
                python.chmod(0o755)
            artifact, step_summary = directory / "test results.json", directory / "summary"
            step_summary.write_text("existing summary\n")
            environment = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ["PATH"],
                               FAKE_GO_STATUS=str(status), GITHUB_STEP_SUMMARY=str(step_summary))
            run = subprocess.run(["make", "-s", "test-ci", f"TEST_JSON={artifact}"], cwd=root,
                                 env=environment, text=True, capture_output=True)
            return run, artifact.read_text(), step_summary.read_text()

    def test_go_result_controls_target_and_json_is_teed(self):
        for status in (0, 7):
            with self.subTest(status=status):
                run, artifact, summary = self.run_target(status)
                self.assertEqual(run.returncode == 0, status == 0, run.stderr)
                self.assertIn('"Test":"TestCI"', artifact)
                self.assertIn(artifact.strip(), run.stdout)
                self.assertIn("Slowest tests", run.stdout)
                self.assertTrue(summary.startswith("existing summary\n"))
                self.assertIn("fake / TestCI", summary)
                if status:
                    self.assertIn("Error 7", run.stderr)

    def test_summary_failure_cannot_change_go_result(self):
        for status in (0, 7):
            with self.subTest(status=status):
                run, artifact, _ = self.run_target(status, summary_fails=True)
                self.assertEqual(run.returncode == 0, status == 0, run.stderr)
                self.assertIn("TestCI", artifact)


if __name__ == "__main__":
    unittest.main()

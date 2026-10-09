import argparse
import contextlib
import copy
import hashlib
import http.client
import http.server
import io
import json
import os
import pty
import select
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import permission_acceptance as gate
import permission_acceptance_bundle as bundle
import pilot_kit
import pilot_provider as pilot
from test_pilot_kit import source_archive


class PermissionAcceptanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="pg-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.env = {"HOME": str(self.home), "PATH": os.environ["PATH"], "SBX_CRED_OPENAI_MODE": "oauth"}
        self.meta = {"kind": "permission-acceptance-kit", "sandbox": "asp", "baseline": "b"*40}
        self.p = gate.paths(self.home, self.meta, "permissions-bbbbbbb")
        self.package = self.home / "package"; self.package.mkdir()
        (self.package / "SHA256SUMS").write_bytes(b"fixture sums")
        self.installed = self.home / ".local/opt/unreal-agent-pilot/asp" / ("bbbbbbb-"+hashlib.sha256(b"fixture sums").hexdigest()[:16])
        self.args = argparse.Namespace(instance="permissions-bbbbbbb", model="gpt-6.1-sol", effort="high", claude_binary="claude", policy_audited=False, trust_managed_policy=False)

    def setup_gate(self):
        with patch.dict(os.environ, self.env, clear=True), patch.object(gate.asp, "preflight", return_value="/existing/auth.json"), \
                patch.object(pilot, "bounded_probe", return_value=(b"",0)), patch.object(gate, "kit", return_value=(self.installed,self.meta)), contextlib.redirect_stdout(io.StringIO()):
            gate.setup(self.package, self.meta, self.p, self.args)

    def test_provider_split_and_no_configuration_approval(self):
        for sandbox in ("asp","devlore"):
            cfg = gate.configuration(sandbox, self.p["workspace"], "explicit-model", "high", "/unused/claude", audited=sandbox=="devlore", trust=sandbox=="devlore")
            self.assertEqual(cfg["Permissions"]["Tools"], ["read","grep","glob"])
            self.assertEqual(cfg["Permissions"]["ProcessMode"], "")
            self.assertFalse(cfg["Permissions"]["FilesystemUnrestricted"])
            self.assertFalse(cfg["Permissions"]["NetworkUnrestricted"])
            self.assertTrue(cfg["PermissionModes"]["Enabled"])
            self.assertNotIn("Mode", cfg["PermissionModes"])
            self.assertEqual(cfg["PermissionModes"]["Maximum"]["Tools"], ["read","grep","glob","write","edit","Bash"])
            self.assertFalse(cfg["Launcher"]["DiscoverProviders"])
            if sandbox=="asp": self.assertEqual(cfg["Runtime"]["Provider"]["auth"]["method"], "docker_sandbox_oauth")
            else:
                self.assertEqual(cfg["Runtime"]["ClaudeCode"]["managedPolicyMode"], "trust")
                self.assertEqual(cfg["Runtime"]["ClaudeCode"]["ToolBridge"]["Mode"], "structured")
        with self.assertRaises(pilot.PilotError): gate.configuration("devlore",self.home,"opus","high")

    def test_safe_setup_rerun_preserves_all_existing_settings_and_modified_work(self):
        keep = []
        for name in (".codex/auth.json", ".claude/credentials", ".config/unreal-agent/runtime.json", ".local/bin/unreal", ".local/state/unreal-agent/sessions/old", ".local/state/unreal-agent-pilot/devlore/launcher-v1/sessions/old"):
            p = self.home / name; p.parent.mkdir(parents=True,exist_ok=True); p.write_bytes(b"PRIVATE_DUMMY_KEEP"); keep.append(p)
        before = {p:(p.read_bytes(),p.stat().st_mtime_ns) for p in keep}
        self.setup_gate()
        self.assertEqual(self.p["config"].stat().st_mode & 0o777,0o600)
        self.assertEqual(self.p["state"].stat().st_mode & 0o777,0o700)
        self.assertEqual(self.p["wrapper"].stat().st_mode & 0o777,0o700)
        changed = self.p["workspace"] / "answer.py"; changed.write_bytes(b"user work remains\n")
        config_before = self.p["config"].read_bytes(), self.p["config"].stat().st_mtime_ns
        self.setup_gate()
        self.assertEqual(changed.read_bytes(),b"user work remains\n")
        self.assertEqual((self.p["config"].read_bytes(),self.p["config"].stat().st_mtime_ns),config_before)
        self.assertEqual({p:(p.read_bytes(),p.stat().st_mtime_ns) for p in keep},before)
        self.assertIn(str(self.installed),self.p["wrapper"].read_text())
        with self.assertRaises(pilot.PilotError):
            self.args.model="different-model"; self.setup_gate()
        self.assertEqual((self.p["config"].read_bytes(),self.p["config"].stat().st_mtime_ns),config_before)

    def test_missing_auth_or_audit_fail_before_publication(self):
        with patch.dict(os.environ,self.env,clear=True), patch.object(gate.asp,"preflight",side_effect=pilot.PilotError("auth_missing","No changes.")), self.assertRaises(pilot.PilotError):
            gate.setup(self.package,self.meta,self.p,self.args)
        self.assertFalse(self.p["config"].exists())
        self.meta["sandbox"]="devlore"
        with patch.object(pilot,"claude_preflight",side_effect=AssertionError("must not run")),self.assertRaises(pilot.PilotError):
            gate.setup(self.package,self.meta,self.p,self.args)

    def test_instructions_wait_only_for_interactive_enter(self):
        (self.package / "README.md").write_text("日本語の受入手順\n", encoding="utf-8")
        class TTY(io.StringIO):
            def isatty(self): return True
        stdin, stdout = TTY("\n"), TTY()
        with patch.object(sys,"stdin",stdin), patch.object(sys,"stdout",stdout):
            gate.instructions(self.package,wait=True)
        self.assertNotIn("日本語の受入手順",stdout.getvalue())
        self.assertIn("[Enterで起動]",stdout.getvalue())
        self.assertEqual(stdin.tell(),1)
        for contents in ("", "typed input\n", "x"*512+"\n"):
            with patch.object(sys,"stdin",TTY(contents)), patch.object(sys,"stdout",TTY()), self.assertRaises(pilot.PilotError):
                gate.instructions(self.package,wait=True)
        for contents in ("typed input\n\n", "expired-confirmation\n\n", "x"*512+"\n\n", "\r\n"):
            stdout=TTY()
            with patch.object(sys,"stdin",TTY(contents)), patch.object(sys,"stdout",stdout), patch.object(time,"monotonic",side_effect=AssertionError("no deadline before human Enter")):
                gate.instructions(self.package,wait=True)
            self.assertNotIn("typed input",stdout.getvalue())
            self.assertNotIn("expired-confirmation",stdout.getvalue())
        with patch.object(sys,"stdin",io.StringIO("do not consume")), patch.object(sys,"stdout",TTY()):
            gate.instructions(self.package,wait=True)
            self.assertEqual(sys.stdin.tell(),0)
        with patch.object(sys,"stdin",TTY("do not consume")), patch.object(sys,"stdout",io.StringIO()):
            gate.instructions(self.package,wait=True)
            self.assertEqual(sys.stdin.tell(),0)

    def test_quick_start_does_not_display_readme(self):
        (self.package / "README.md").write_text("private-readme-marker\n", encoding="utf-8")
        output = io.StringIO()
        with patch.object(sys, "stdout", output):
            gate.instructions(self.package)
        self.assertIn("Unreal Agent - Quick Start", output.getvalue())
        self.assertNotIn("private-readme-marker", output.getvalue())

    def test_readme_replay_and_ctrl_c_do_not_launch_or_grant(self):
        (self.package / "README.md").write_text("日本語の受入手順\n", encoding="utf-8")
        output = io.StringIO()
        with contextlib.redirect_stdout(output): gate.readme(self.package)
        self.assertIn("日本語の受入手順", output.getvalue())
        stdin = io.StringIO(); stdin.isatty = lambda: True
        stdin.readline = lambda *args: (_ for _ in ()).throw(KeyboardInterrupt())
        stdout = io.StringIO(); stdout.isatty = lambda: True
        with patch.object(sys, "stdin", stdin), patch.object(sys, "stdout", stdout), self.assertRaises(pilot.PilotError) as failed:
            gate.instructions(self.package, wait=True)
        self.assertEqual(failed.exception.code, "gate_instructions_canceled")

    def test_quick_start_pty_requires_a_separate_empty_enter(self):
        master, slave = pty.openpty()
        self.addCleanup(os.close, master)
        script = "import pathlib,sys; sys.path.insert(0,sys.argv[1]); import permission_acceptance as g; g.instructions(pathlib.Path('.'),wait=True); print('LAUNCH_CONFIRMED',flush=True)"
        child = subprocess.Popen([sys.executable, "-B", "-c", script, str(Path(gate.__file__).parent)], stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        def stop_child():
            if child.poll() is None: child.kill()
            child.wait(timeout=5)
        self.addCleanup(stop_child)
        captured = bytearray()
        def until(marker, count=1):
            deadline=time.monotonic()+5
            while captured.count(marker) < count:
                if time.monotonic() >= deadline: self.fail("PTY prompt not observed")
                readable, _, _ = select.select([master], [], [], max(0, deadline-time.monotonic()))
                if readable: captured.extend(os.read(master, 4096))
        until("[Enterで起動]".encode())
        self.assertIsNone(child.poll())
        os.write(master, b"expired-confirmation\n")
        until("[Enterで起動]".encode(), 2)
        # An arbitrary input cannot launch; no timeout or pending challenge is
        # involved. A distinct empty Enter is still required.
        self.assertIsNone(child.poll())
        os.write(master, b"\n")
        until(b"LAUNCH_CONFIRMED")
        self.assertEqual(child.wait(timeout=5), 0)
        self.assertNotIn(b"policy-audited", captured)

    def test_failed_view_diagnostics_are_closed_and_read_only(self):
        self.setup_gate()
        (self.p["workspace"] / "sample.txt").write_bytes(b"after\n")
        op={"ID":"private-operation-id","ToolName":"write","Status":"completed","State":{"Plan":{"Data":{"path":"private-path","content":"private-body"}}}}
        view={"Failure":'call model for turn "private-turn-id": provider: remote_failure stage=http_status status=502 reason=server_error retryable=true',"Operations":[op], "History":{"More":False}}
        result=gate.safe_diagnosis(self.p,view)
        self.assertEqual(result["stage"],"http_status")
        self.assertEqual(result["status"],502)
        self.assertEqual(result["reason"],"server_error")
        self.assertTrue(result["retryable"])
        self.assertEqual(result["receipts"],{"write:completed":1})
        self.assertEqual(result["sample_state"],"expected_after")
        self.assertNotIn("private",json.dumps(result))
        for field in ("stage", "reason"):
            changed=copy.deepcopy(view)
            changed["Failure"]=view["Failure"].replace(field+"="+result[field],field+"=private_payload")
            summary=gate.safe_diagnosis(self.p,changed)
            self.assertEqual(summary[field],"unknown")
            self.assertNotIn("private",json.dumps(summary))
        legacy=copy.deepcopy(view);legacy["Failure"]="provider: remote_failure"
        summary=gate.safe_diagnosis(self.p,legacy)
        self.assertIsNone(summary["status"]);self.assertIsNone(summary["retryable"])
        legacy["Operations"].append(op)
        self.assertEqual(gate.safe_diagnosis(self.p,legacy)["duplicate_operation_count"],1)
        (self.p["workspace"] / "sample.txt").unlink()
        (self.p["workspace"] / "sample.txt").symlink_to(self.home / "secret-missing")
        self.assertEqual(gate.safe_diagnosis(self.p,view)["sample_state"],"unreadable")
        # Failed sessions remain failed evidence, even if actual work succeeded.
        with patch.object(gate,"inspect_view",return_value=view),self.assertRaises(pilot.PilotError): gate.view(self.p,{"CodexFile":""},"gate-e")

    def test_open_displays_and_confirms_instructions_before_exec(self):
        self.setup_gate()
        order=[]
        with patch.dict(os.environ,self.env,clear=True), patch.object(gate,"kit",return_value=(self.installed,self.meta)), \
                patch.object(gate,"preflight",side_effect=lambda *a:order.append("preflight")), \
                patch.object(gate,"instructions",side_effect=lambda *a,**k:order.append(("quick-start",k))), \
                patch.object(os,"chdir"),patch.object(os,"execve",side_effect=lambda *a:order.append("exec")):
            self.assertEqual(gate.main(["open","gate-e","--instance",self.args.instance]),0)
        self.assertEqual(order,[("quick-start",{"wait":True}),"preflight","exec"])
        with patch.dict(os.environ,self.env,clear=True), patch.object(gate,"kit",return_value=(self.installed,self.meta)), \
                patch.object(gate,"preflight") as probe,patch.object(gate,"instructions",side_effect=pilot.PilotError("gate_instructions_canceled","No TUI started.")), \
                patch.object(os,"execve") as execute, contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(gate.main(["open","gate-e","--instance",self.args.instance]),1)
            execute.assert_not_called()
            probe.assert_not_called()
        # Readme replay does not require a Session, authentication or any Host.
        with patch.object(gate,"kit",return_value=(self.package,self.meta)), patch.object(gate,"readme") as show, patch.object(gate,"binding",side_effect=AssertionError("not needed")):
            self.assertEqual(gate.main(["readme"]),0)
            show.assert_called_once_with(self.package)

    def test_work_requires_exact_native_receipts_and_real_content_assertion(self):
        self.setup_gate(); root=self.p["workspace"]
        (root/"sample.txt").write_bytes(b"after\n")
        ops=[{"ID":tool,"ToolName":tool,"Status":"completed","Permission":{"State":"full_access"},"State":{"Plan":{"Data":{"path":"sample.txt"}}}} for tool in ("write","edit","read")]
        ops.append({"ID":"bash","ToolName":"Bash","Status":"completed","Permission":{"State":"full_access"},"State":{"Input":{"Command":gate.WORK_COMMAND},"Result":{"ExitCode":0}}})
        with contextlib.redirect_stdout(io.StringIO()):gate.verify_stage(self.p,{"Operations":ops},"work")
        for altered in (ops[1:],ops[:-1]):
            with self.assertRaises(pilot.PilotError):gate.verify_stage(self.p,{"Operations":altered},"work")
        wrong=copy.deepcopy(ops);wrong[0]["State"]["Plan"]["Data"]["path"]="/tmp/elsewhere/sample.txt"
        with self.assertRaises(pilot.PilotError):gate.verify_stage(self.p,{"Operations":wrong},"work")
        wrong=copy.deepcopy(ops);wrong[-1]["State"]["Result"]["ExitCode"]=1
        with self.assertRaises(pilot.PilotError):gate.verify_stage(self.p,{"Operations":wrong},"work")
        (root/"sample.txt").write_bytes(b"wrong\n")
        with self.assertRaises(pilot.PilotError):gate.verify_stage(self.p,{"Operations":ops},"work")
        self.assertIn("expected={exists:false}",gate.PROMPTS["work"])
        self.assertIn(gate.WORK_COMMAND,gate.PROMPTS["work"])
        self.assertIn("immutable workspace boundary",gate.configuration("asp",root,"m","high")["Runtime"]["SystemPrompt"])

    def test_private_config_paths_and_session_names(self):
        with self.assertRaises(pilot.PilotError): gate.paths(self.home,self.meta,"../default")
        with self.assertRaises(pilot.PilotError): gate.session_id("existing-session")
        self.assertEqual(gate.session_id("gate-e"),"gate-e")
        self.p["config_dir"].parent.mkdir(parents=True)
        self.p["config_dir"].symlink_to(self.home)
        with self.assertRaises(pilot.PilotError): self.setup_gate()

    def test_binding_rejects_configuration_mutation_and_claude_path_is_pinned(self):
        self.setup_gate()
        self.assertEqual(gate.binding(self.p,self.meta)["ConfigurationHash"],gate.digest(pilot.decode(self.p["config"].read_bytes())))
        cfg=pilot.decode(self.p["config"].read_bytes());cfg["PermissionModes"]["Maximum"]["Tools"].append("unexpected")
        pilot.publish_json(self.p["config"],cfg,replace=True)
        with self.assertRaises(pilot.PilotError):gate.binding(self.p,self.meta)
        meta=dict(self.meta,sandbox="devlore")
        bound={"ClaudeBinary":"/owned/explicit/claude","PolicyAudited":True,"TrustManagedPolicy":True}
        with patch.object(pilot,"claude_preflight",return_value=bound["ClaudeBinary"]) as preflight:
            gate.preflight(self.package,meta,self.p,bound)
            self.assertEqual(preflight.call_args.args[0],bound["ClaudeBinary"])
        with patch.object(pilot,"claude_preflight",return_value="/different/claude"),self.assertRaises(pilot.PilotError):
            gate.preflight(self.package,meta,self.p,bound)

    def test_no_prose_permission_pass_and_real_receipt_checks(self):
        v={"Operations":[],"Permissions":{"Mode":"read_only"}}
        with self.assertRaises(pilot.PilotError): gate.verify_stage(self.p,v,"deny")
        v["Operations"]=[{"ID":name,"ToolName":name,"Status":"failed","Permission":{"Mode":"read_only","State":"read_only"},"Denial":{"Code":"denied"}} for name in ("write","Bash")]
        with contextlib.redirect_stdout(io.StringIO()): gate.verify_stage(self.p,v,"deny")
        v["Operations"].append(copy.deepcopy(v["Operations"][0]))
        with self.assertRaises(pilot.PilotError): gate.verify_stage(self.p,v,"deny")

    def test_full_requires_actual_fail_fix_pass_and_unchanged_test(self):
        self.setup_gate();root=self.p["workspace"]
        (root/"answer.py").write_bytes(b"def answer():\n    return 42\n")
        (root/"work.txt").write_bytes(b"full work completed\n")
        ops=[{"ID":tool,"ToolName":tool,"Status":"completed","Permission":{"State":"full_access"}} for tool in ("edit","write")]
        for exit_code in (1,0):
            ops.append({"ID":str(exit_code),"ToolName":"Bash","Status":"completed","Permission":{"State":"full_access"},"State":{"Input":{"Command":"python3 -m unittest -v"},"Result":{"ExitCode":exit_code}}})
        with patch.dict(os.environ,self.env,clear=True), contextlib.redirect_stdout(io.StringIO()): gate.verify_stage(self.p,{"Operations":ops},"full")
        with self.assertRaises(pilot.PilotError): gate.verify_stage(self.p,{"Operations":ops[:-1]},"full")
        (root/"test_answer.py").write_bytes(b"# bypass")
        with self.assertRaises(pilot.PilotError): gate.verify_stage(self.p,{"Operations":ops},"full")

    def test_resume_history_and_no_duplicate_operations(self):
        self.setup_gate()
        v={"History":{"Items":[{"Kind":"model_response","Data":{"Response":{"Stop":"complete"}}}]},"Operations":[{"ID":"one","Status":"completed","Permission":{"State":"full_access"}}],"Permissions":{"Mode":"read_only"}}
        with patch.object(gate,"view",return_value=v), contextlib.redirect_stdout(io.StringIO()):
            gate.checkpoint(self.p,{},"gate-e"); gate.verify_resume(self.p,{},"gate-e")
        bad=copy.deepcopy(v);bad["Operations"].append({"ID":"duplicate-effect","Status":"completed"})
        with patch.object(gate,"view",return_value=bad),self.assertRaises(pilot.PilotError):gate.verify_resume(self.p,{},"gate-e")
        bad=copy.deepcopy(v);bad["Permissions"]["Mode"]="full_access"
        with patch.object(gate,"view",return_value=bad),self.assertRaises(pilot.PilotError):gate.verify_resume(self.p,{},"gate-e")

    def test_pid_validation_rejects_current_non_host_without_signal(self):
        with patch.object(gate.os,"pidfd_open",side_effect=AssertionError("must not signal")),self.assertRaises(pilot.PilotError):
            gate.validate_pid(os.getpid(),self.package,self.p,{"CodexFile":"/unused/auth.json"})

    def test_gate_sh_and_prompts_do_not_call_inference(self):
        shell=Path(__file__).with_name("pilot")/"gate.sh"
        subprocess.run(["bash","-n",str(shell)],check=True)
        with patch.object(gate,"kit",side_effect=AssertionError("prompt must not probe a CLI")),contextlib.redirect_stdout(io.StringIO()):self.assertEqual(gate.main(["prompt","deny"]),0)


class PermissionBundleTests(unittest.TestCase):
    def test_both_archives_exact_inventory_checksums_install_scope_and_no_credentials(self):
        for sandbox in ("asp","devlore"):
            with self.subTest(sandbox=sandbox), tempfile.TemporaryDirectory(prefix="pg-") as temporary:
                root=Path(temporary);source=source_archive(root,revision="b"*40); output=root/"output";output.mkdir()
                with patch.object(pilot_kit,"binary_requirements",return_value="2.34"):
                    archive=bundle.assemble(source,output,"b"*40,sandbox)
                name=f"unreal-agent-{sandbox}-permissions-bbbbbbb"
                with tarfile.open(archive) as tar:
                    members=tar.getmembers(); self.assertTrue(all(m.isfile() for m in members))
                    meta=json.load(tar.extractfile(name+"/KIT.json"));self.assertEqual(meta["sandbox"],sandbox)
                    self.assertEqual(meta["kind"],"permission-acceptance-kit")
                    sums=tar.extractfile(name+"/SHA256SUMS").read().decode()
                    for line in sums.splitlines():
                        value,path=line.split("  ");self.assertEqual(hashlib.sha256(tar.extractfile(name+"/"+path).read()).hexdigest(),value)
                    install=tar.extractfile(name+"/install.sh").read().decode()
                    self.assertIn("for part in .local opt unreal-agent-pilot "+sandbox+";",install)
                    readme=tar.extractfile(name+"/README.md").read().decode()
                    self.assertNotIn("@",readme);self.assertIn("OS sandbox",readme)
                    self.assertFalse(any(m.name.endswith(("auth.json","config.toml")) for m in members))


@unittest.skipUnless(os.environ.get("UNREAL_PERMISSION_KIT_DIRECTORY"),"set UNREAL_PERMISSION_KIT_DIRECTORY for packaged Host/installer offline tests")
class PermissionPackagedTests(unittest.TestCase):
    """Fresh packaged binary, local fake provider only; no Sandbox acceptance claim."""

    def test_install_idle_host_final_history_stop_and_resume(self):
        package=Path(os.environ["UNREAL_PERMISSION_KIT_DIRECTORY"])
        meta=pilot.decode((package/"KIT.json").read_bytes())
        with tempfile.TemporaryDirectory(prefix="pg-",dir="/tmp") as temporary:
            home=Path(temporary)
            env={"HOME":str(home),"PATH":os.environ["PATH"],"SBX_CRED_OPENAI_MODE":"oauth",
                 "HTTPS_PROXY":"http://host.docker.internal:3128","PYTHONDONTWRITEBYTECODE":"1"}
            codex=home/".codex";codex.mkdir()
            (codex/"auth.json").write_text('{"OPENAI_API_KEY":"proxy-managed"}')
            (codex/"config.toml").write_text('forced_login_method="api"\nmodel_provider="sandboxd"\n[model_providers.sandboxd]\nname="Sandbox Proxy"\nbase_url="https://chatgpt.com/backend-api/codex"\nexperimental_bearer_token="oai-oat01-proxy-managed"\nrequires_openai_auth=false\n')
            for path in codex.iterdir():path.chmod(0o600)
            old=home/".local/state/unreal-agent/sessions/old";old.parent.mkdir(parents=True);old.write_bytes(b"immutable synthetic old history")
            before={path:(path.read_bytes(),path.stat().st_mtime_ns) for path in (*codex.iterdir(),old)}
            fake_claude=os.environ.get("UNREAL_PERMISSION_FAKE_CLAUDE","")
            if meta["sandbox"]=="devlore":
                self.assertTrue(fake_claude,"Explicit offline fake Claude binary required; never discover the real CLI")
            args=argparse.Namespace(instance="fixture",model="opus" if meta["sandbox"]=="devlore" else "gpt-6.1-sol",effort="high",claude_binary=fake_claude,
                                    policy_audited=meta["sandbox"]=="devlore",trust_managed_policy=meta["sandbox"]=="devlore")
            p=gate.paths(home,meta,args.instance)
            with patch.dict(os.environ,env,clear=True),contextlib.redirect_stdout(io.StringIO()):
                gate.kit(package)
                gate.setup(package,meta,p,args)
                gate.setup(package,meta,p,args)
            b=gate.binding(p,meta);installed=Path(b["BinaryDirectory"])
            self.assertFalse(p["sessions"].exists(),"setup must start no Host/inference")

            class FakeProvider(http.server.BaseHTTPRequestHandler):
                def log_message(self,*_):pass
                def do_POST(self):
                    self.rfile.read(int(self.headers.get("Content-Length","0")))
                    self.server.calls+=1
                    event={"type":"response.completed","response":{"id":"offline-response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PERMISSION_LIFECYCLE_OK"}]}]}}
                    output=("data: "+json.dumps(event)+"\n\n").encode()
                    self.send_response(200);self.send_header("Content-Type","text/event-stream");self.send_header("Content-Length",str(len(output)));self.end_headers();self.wfile.write(output)

            server=http.server.ThreadingHTTPServer(("127.0.0.1",0),FakeProvider);server.calls=0
            worker=threading.Thread(target=server.serve_forever,daemon=True);worker.start()
            self.addCleanup(server.server_close);self.addCleanup(server.shutdown)
            cfg=pilot.decode(p["config"].read_bytes());auth=""
            if meta["sandbox"]=="asp":
                # The compiled Docker kit schema is tested separately. This
                # lifecycle fixture replaces only its local synthetic config
                # with native OAuth/loopback; no external proxy is contacted.
                endpoint=f"http://127.0.0.1:{server.server_port}"
                cfg["Runtime"]["Provider"]["endpoint"]=endpoint
                cfg["Runtime"]["Provider"]["auth"]={"provider":"openai-codex","method":"oauth","id":"external-codex"}
                cfg["Permissions"]["NetworkOrigins"]=[endpoint]
                auth=str(home/"fixture-auth.json")
                files={"auth_mode":"chatgpt","tokens":{"access_token":"fixture-not-a-real-token","account_id":"fixture-not-an-account"}}
                pilot.publish_json(Path(auth),files)
                env.pop("HTTPS_PROXY")
            b["CodexFile"]=auth;b["ConfigurationHash"]=gate.digest(cfg)
            pilot.publish_json(p["config"],cfg,replace=True);pilot.publish_json(p["binding"],b,replace=True)

            def start():
                process=subprocess.Popen(pilot.host_arguments(installed,p,auth),env=env,cwd=p["workspace"],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
                self.addCleanup(lambda: process.poll() is not None or (process.terminate(),process.wait(timeout=15)))
                gateway=pilot.Gateway(p["socket"]);deadline=time.monotonic()+20
                while time.monotonic()<deadline:
                    self.assertIsNone(process.poll(),"packaged Host exited before gateway readiness; raw logs withheld")
                    try:gateway.call("auth/methods",{});return process,gateway
                    except (OSError,http.client.HTTPException):time.sleep(.05)
                self.fail("packaged gateway readiness timeout")

            process,gateway=start()
            v=gateway.call("open",{"ID":"gate-d","Mode":"create"})["View"]
            self.assertEqual(v["Permissions"]["Mode"],"read_only")
            self.assertFalse(v.get("Operations"))
            gateway.call("submit",{"ID":"gate-d","Generation":v["Generation"],"Input":{"ID":"offline-input","Kind":"external","Payload":gate.PROMPTS["d"]}})
            deadline=time.monotonic()+20
            while time.monotonic()<deadline:
                v=gateway.call("inspect",{"ID":"gate-d","Limit":256})["View"]
                outcome=pilot.smoke_outcome(v,"PERMISSION_LIFECYCLE_OK")
                if outcome=="pass":break
                self.assertEqual(outcome,"pending","offline provider/protocol failed; payload withheld")
                time.sleep(.05)
            else:self.fail("offline final missing")
            with contextlib.redirect_stdout(io.StringIO()):
                gate.verify_stage(p,v,"d");gate.checkpoint(p,b,"gate-d");gate.stop(installed,p,b)
            self.assertEqual(process.wait(timeout=15),0)
            process,gateway=start()
            gateway.call("open",{"ID":"gate-d","Mode":"resume"})
            with contextlib.redirect_stdout(io.StringIO()):gate.verify_resume(p,b,"gate-d");gate.stop(installed,p,b)
            self.assertEqual(process.wait(timeout=15),0)
            self.assertEqual({path:(path.read_bytes(),path.stat().st_mtime_ns) for path in before},before)
            if meta["sandbox"]=="asp":self.assertEqual(server.calls,1,"restart/resume must never replay inference")
            else:
                calls=Path(fake_claude).parent/"calls.jsonl"
                self.assertEqual(len(calls.read_text().splitlines()),1,"restart/resume must never replay structured generation")


if __name__ == "__main__": unittest.main()

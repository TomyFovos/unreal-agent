#!/usr/bin/env python3
"""Build provider-specific operator scripts without binaries or authentication."""

import argparse
import gzip
from pathlib import Path
import tarfile

import pilot_kit
import snapshot


def build(output, sandbox):
    name = f"unreal-agent-{sandbox}-provider-6284e49"
    root = output / name
    root.mkdir(mode=0o700)
    source = Path(__file__).parent
    (root / "pilot_provider.py").write_bytes((source / "pilot_provider.py").read_bytes())
    values = {"SANDBOX": sandbox, "PROVIDER": "openai-codex" if sandbox == "asp" else "claude-code",
              "ROOT": name, "ARCHIVE": name + ".tar.gz", "MODEL": "gpt-6.1-sol" if sandbox == "asp" else "claude-opus-5-5",
              "EFFORT": "medium" if sandbox == "asp" else "high"}
    if sandbox == "asp":
        values["AUTH_INSTRUCTIONS"] = "External Codex認証は既存のパスだけ参照し、statによる存在・所有者・private permission検査のみを行います。\n優先順位は`OPENAI_CODEX_AUTH_FILE`、`CODEX_HOME/auth.json`、`~/.codex/auth.json`。\n内容・token/account・有効期限は読みません。欠落時は既存Codexのloginを別途実施してください。\n実requestでは既存Unreal External Codex resolverが毎turn認証を解決します。認証importやAPI key fallbackは行いません。"
    else:
        values["AUTH_INSTRUCTIONS"] = "**devloreのmanaged policyは未監査です。上のコマンドは監査未確認としてClaude起動前に停止します。**\n環境ごとに管理者のlocal/remote policy、managed hooks/helpers/環境設定、telemetryの例外を監査してください。\n確認したoperatorがその環境の管理者境界を明示的に信頼する場合だけ、同じ実行に\n`--policy-audited --trust-managed-policy`を追加します。これはoperatorの監査済み宣言であり、\n自動監査証明・remote policyの不変性保証ではありません。組織policyは起動後にも変更され得ます。\nこの2条件がなければconfigにtrustを書き込まず、Claudeのversion/help/auth statusすら実行しません。\n同意後も`claude auth status`は隔離flag/restricted envで実行し、loggedIn/firstParty/claude.aiとsupported subscriptionを確認します。\n未ログイン・CLI欠落なら停止し、認証ファイルは読みません。通常のauth loginはユーザーが別途実施してください。\nToolBridge.Modeは**structured固定**、MCP toolsは空、side-effecting built-inはdisabledです。\nStructuredOutputだけをserializerとして許可するのは既存Unreal adapterの責務です。\nmanaged MCP exact allow grantを要求せず、policy拒否をbuilt-in/MCP/provider fallbackで回避しません。"
    for filename, template in (("pilot.sh", "provider.sh"), ("README.md", "provider-README.md")):
        text = (source / "pilot" / template).read_text()
        for key, value in values.items():
            text = text.replace("@" + key + "@", value)
        (root / filename).write_text(text)
    names = ("pilot.sh", "pilot_provider.py", "README.md")
    for filename in names:
        (root / filename).chmod(0o700 if filename == "pilot.sh" else 0o600)
    (root / "SHA256SUMS").write_text("".join(f"{pilot_kit.digest(root / filename)}  {filename}\n" for filename in names))
    (root / "SHA256SUMS").chmod(0o600)
    archive = output / (name + ".tar.gz")
    with archive.open("xb") as stream, gzip.GzipFile(filename="", mode="wb", fileobj=stream, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as bundle:
            for filename in (*names, "SHA256SUMS"):
                info = bundle.gettarinfo(str(root / filename), name + "/" + filename)
                info.mtime = info.uid = info.gid = 0
                info.uname = info.gname = ""
                with (root / filename).open("rb") as reader:
                    bundle.addfile(info, reader)
    archive.chmod(0o600)
    checksum = archive.with_name(archive.name + ".sha256")
    checksum.write_text(f"{pilot_kit.digest(archive)}  {archive.name}\n")
    checksum.chmod(0o600)
    return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--copy-to", type=Path)
    args = parser.parse_args()
    output = pilot_kit.private_output(snapshot.ROOT)
    for sandbox in ("asp", "devlore"):
        archive = build(output, sandbox)
        print(f"{sandbox}: {archive}")
        if args.copy_to:
            delivery = pilot_kit.copy_delivery(archive, args.copy_to / sandbox, prefix=f"unreal-agent-{sandbox}-provider-6284e49-")
            print(f"delivery: {delivery}")


if __name__ == "__main__":
    main()

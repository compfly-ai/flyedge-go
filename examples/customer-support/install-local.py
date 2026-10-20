#!/usr/bin/env python3
"""Install this agent's downloaded identity into the existing local k3d stack."""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile


def kubectl(*args, **kwargs):
    return subprocess.run(["kubectl", "--context", "k3d-compfly-local", "-n", "compfly-local", *args],
                          capture_output=True, check=True, timeout=30, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--did", required=True)
    parser.add_argument("--key-file", type=Path, required=True)
    parser.add_argument("--infra-dir", type=Path, default=Path.home() / "dev/local-terraform")
    args = parser.parse_args()
    if not args.did.startswith("did:compfly:") or "\n" in args.did:
        raise SystemExit("Expected the new local agent's CompFly DID.")
    if not args.key_file.is_file():
        raise SystemExit("Downloaded agent key is missing.")
    tiltfile = args.infra_dir / "local/Tiltfile"
    if "customer-support-go.enabled" not in tiltfile.read_text():
        raise SystemExit("Local Tilt agent wiring is missing; no changes made.")
    # Reject replacing another identity under this Secret name.
    existing = subprocess.run(["kubectl", "--context", "k3d-compfly-local", "-n", "compfly-local",
                               "get", "secret", "customer-support-go-identity", "--ignore-not-found", "-o", "json"],
                              check=True, capture_output=True, timeout=30)
    if existing.stdout.strip():
        identity = json.loads(existing.stdout)["data"]
        if base64.b64decode(identity["did"]).decode() != args.did:
            raise SystemExit("An unrelated agent identity already uses this Secret; refusing to replace it.")
    os.chmod(args.key_file, 0o600)
    with tempfile.TemporaryDirectory(prefix="compfly-go-identity-") as temporary:
        did_file = Path(temporary) / "did"
        did_file.write_text(args.did)
        os.chmod(did_file, 0o600)
        payload = kubectl("create", "secret", "generic", "customer-support-go-identity",
                          "--from-file=did=" + str(did_file), "--from-file=agent.pem=" + str(args.key_file),
                          "--dry-run=client", "-o", "json").stdout
        kubectl("apply", "-f", "-", input=payload)
    marker = args.infra_dir / "local/keys/customer-support-go.enabled"
    marker.write_text("Local Go support-agent validation enabled.\n")
    print("Installed the separate local identity and enabled the existing Tilt resource. No credentials printed.")


if __name__ == "__main__":
    main()

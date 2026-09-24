#!/usr/bin/env python3
"""Moves each project's .yoake/ to <state-dir>/projects/<id>/ (SPEC.md §6).

Stop the server first. Dry run unless --apply. Projects come from
projects.json plus any paths given. Never overwrites: conflicts stay in
.yoake/ and are listed.
"""

import argparse
import hashlib
import json
import os
import shutil
import socket
import sys


# Must match projectstore.DataDir.
def data_dir(state_dir, root):
    return os.path.join(state_dir, "projects", hashlib.sha256(root.encode()).hexdigest()[:16])


def server_running():
    runtime = os.environ.get("XDG_RUNTIME_DIR")
    path = os.path.join(runtime, "yoake", "yoakeland.sock") if runtime else os.path.expanduser("~/.yoake/yoakeland.sock")
    s = socket.socket(socket.AF_UNIX)
    try:
        s.connect(path)
        return True
    except OSError:
        return False
    finally:
        s.close()


def move_tree(src, dest, apply, conflicts):
    if not os.path.lexists(dest):
        if apply:
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            shutil.move(src, dest)
        return
    if os.path.isdir(src) and os.path.isdir(dest) and not os.path.islink(src):
        for name in sorted(os.listdir(src)):
            move_tree(os.path.join(src, name), os.path.join(dest, name), apply, conflicts)
        if apply and not os.listdir(src):
            os.rmdir(src)
        return
    conflicts.append(src)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("paths", nargs="*", help="extra project paths not in projects.json")
    p.add_argument("--state-dir", default=os.path.expanduser("~/.yoake"), help="the server's -state-dir (default ~/.yoake)")
    p.add_argument("--apply", action="store_true", help="actually move files (default: dry run)")
    args = p.parse_args()

    if args.apply and server_running():
        sys.exit("a yoakeland server is running; stop it first (it would keep writing to the old locations)")

    roots = list(args.paths)
    try:
        with open(os.path.join(args.state_dir, "projects.json")) as f:
            roots += [p["path"] for p in json.load(f).get("projects", [])]
    except FileNotFoundError:
        pass

    seen = set()
    for root in roots:
        root = os.path.realpath(root)
        if root in seen:
            continue
        seen.add(root)
        src = os.path.join(root, ".yoake")
        dest = data_dir(args.state_dir, root)
        if not os.path.isdir(src):
            print(f"skip   {root} (no .yoake)")
            continue

        print(f"{'move' if args.apply else 'would move'} {src}\n    -> {dest}")
        conflicts = []
        if args.apply:
            os.makedirs(dest, mode=0o700, exist_ok=True)
        for name in sorted(os.listdir(src)):
            move_tree(os.path.join(src, name), os.path.join(dest, name), args.apply, conflicts)
        for c in conflicts:
            print(f"    left in place, already exists at destination: {c}")
        if args.apply:
            with open(os.path.join(dest, "project.json"), "w") as f:
                json.dump({"path": root}, f)
            if not os.listdir(src):
                os.rmdir(src)

    if not args.apply:
        print("\ndry run; pass --apply to move")


if __name__ == "__main__":
    main()

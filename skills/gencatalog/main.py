#!/usr/bin/env python3
"""Refresh skills/INSTALLABLE_SKILLS.json against the remote skill repository.

Shallow-clones https://gitcode.com/huaweicloud/huaweicloud-skills.git,
recomputes each skill directory's aggregate MD5 with the SAME algorithm
the skill-manager.wasm plugin uses at runtime (skill/fs.FolderMD5), and
updates catalog entries whose remote MD5 has changed.

New skills (present remotely, absent from the catalog) and removed skills
(present in the catalog, absent remotely) are REPORTED ONLY — they are
never silently added or deleted. Add new entries explicitly with --add.

Usage:
    python3 skills/gencatalog/main.py                 # refresh + overwrite
    python3 skills/gencatalog/main.py --dry-run        # print summary, write nothing
    python3 skills/gencatalog/main.py --add=foo,bar    # refresh + add named skills
    python3 skills/gencatalog/main.py --clone-dir=/tmp/clone  # reuse an existing clone

Why Python (not Go): the catalog file is itself `json.dumps(d,
ensure_ascii=False, indent=2)` output. Python's dict preserves insertion
order (3.7+), json.dumps never HTML-escapes, and there is no
trailing-newline quirk — so re-serializing an unchanged entry is
byte-identical with zero custom machinery. A Go port would need a custom
ordered-map type for every nesting level (the frontmatter values are
themselves objects/arrays-of-objects), which is bug-prone.
"""
import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile

import yaml

REMOTE_URL = "https://gitcode.com/huaweicloud/huaweicloud-skills.git"
SCHEMA = (
    "https://gitcode.com/huawei-developers/metadata/raw/master/"
    "huaweicloud-skills/schema.json"
)
SKILL_MD = "SKILL.md"


def folder_md5(dirpath, dirname):
    """Aggregate MD5 of a directory — mirrors skill/fs/md5.go FolderMD5.

    Algorithm:
        entries = [dirname]
        walk depth-first; at each level:
          files (sorted by name)   -> append "relpath:md5(filebytes)"
          subdirs (sorted by name) -> recurse
        result = hex(md5("\\n".join(entries)))

    The directory name participates (renaming changes the MD5); the parent
    path does not (moving does not). Symlinks and special files (FIFO,
    device, socket) are skipped — parity with Go's walkSorted, which skips
    them to match Python os.walk(followlinks=False) and to avoid blocking
    on a FIFO with no writer.
    """
    entries = [dirname]

    def walk(root, d):
        subdirs = []
        for name in sorted(os.listdir(d)):
            full = os.path.join(d, name)
            # Skip symlinks (parity with Go ModeSymlink skip).
            if os.path.islink(full):
                continue
            if os.path.isdir(full):
                subdirs.append(name)
                continue
            # Skip non-regular files (FIFO/device/socket) — parity with Go.
            if not os.path.isfile(full):
                continue
            with open(full, "rb") as f:
                data = f.read()
            rel = os.path.relpath(full, root).replace(os.sep, "/")
            entries.append("{}:{}".format(rel, hashlib.md5(data).hexdigest()))
        for name in subdirs:
            walk(root, os.path.join(d, name))

    walk(dirpath, dirpath)
    return hashlib.md5("\n".join(entries).encode()).hexdigest()


def parse_frontmatter(skill_md_text):
    """Parse YAML frontmatter from SKILL.md, preserving key order.

    Python dicts preserve insertion order (3.7+), so yaml.safe_load already
    keeps the source order — no custom ordered-map type needed. Returns {}
    for missing/empty frontmatter.
    """
    text = skill_md_text.replace("\r\n", "\n")
    if not text.startswith("---\n"):
        return {}
    m = re.match(r"^---\n(.*?)\n---(?:\n|$)", text, re.S)
    if not m:
        return {}
    fm = yaml.safe_load(m.group(1))
    return fm if fm is not None else {}


def enumerate_skills(root):
    """Return {skill_name: skill_dir} for every dir containing SKILL.md."""
    out = {}
    for r, _dirs, files in os.walk(root):
        if SKILL_MD in files:
            name = os.path.basename(r)
            if name != "skills":  # skip a SKILL.md directly under the root
                out[name] = r
    return out


def make_install_cmd(name):
    return {
        "cmd": "npx",
        "args": ["-y", "skills", "add", REMOTE_URL, "--skill", name, "-g", "-y"],
    }


def make_remove_cmd(name):
    return {"cmd": "npx", "args": ["-y", "skills", "remove", name, "-g", "-y"]}


def build_entry(name, skill_dir):
    """Build a full catalog entry for one remote skill directory."""
    raw = open(os.path.join(skill_dir, SKILL_MD), encoding="utf-8").read().replace(
        "\r\n", "\n"
    )
    return {
        "name": name,
        "frontmatter": parse_frontmatter(raw),
        "skill_md": raw,
        "install_cmd": make_install_cmd(name),
        "remove_cmd": make_remove_cmd(name),
        "skill_folder_md5": folder_md5(skill_dir, name),
    }


def acquire_clone(clone_dir):
    """Return (skills_root, tmp_dir_to_clean). Reuses clone_dir if it has skills/.

    gitcode.com is a domestic host that does not need a proxy. Clone directly
    first (proxy env cleared). Only if that fails, retry once WITH the
    ambient proxy — some locked-down networks block direct gitcode access
    and require the proxy. This order matches the common case (direct works)
    and reserves the proxy for environments that actually need it.
    """
    if clone_dir and os.path.isdir(os.path.join(clone_dir, "skills")):
        return os.path.join(clone_dir, "skills"), None
    tmp = clone_dir or tempfile.mkdtemp(prefix="gencatalog-")

    def try_clone(env):
        subprocess.run(
            ["git", "clone", "--depth", "1", REMOTE_URL, tmp],
            check=True,
            stdout=sys.stderr,
            stderr=sys.stderr,
            env=env,
        )

    # Build a clean env with proxy vars removed for the first (direct) attempt.
    direct = {}
    for k, v in os.environ.items():
        if k.lower() not in ("http_proxy", "https_proxy", "all_proxy"):
            direct[k] = v

    try:
        try_clone(direct)
    except subprocess.CalledProcessError:
        # Retry with the ambient env (may include a proxy) for networks
        # that block direct gitcode access. Clear the partial dir first.
        import shutil
        shutil.rmtree(tmp, ignore_errors=True)
        try_clone(None)
    return os.path.join(tmp, "skills"), tmp if not clone_dir else None


def main():
    ap = argparse.ArgumentParser(description="Refresh INSTALLABLE_SKILLS.json")
    ap.add_argument("--out", default="skills/INSTALLABLE_SKILLS.json",
                    help="catalog JSON path (default: skills/INSTALLABLE_SKILLS.json)")
    ap.add_argument("--clone-dir", default=None,
                    help="reuse this clone dir instead of a temp dir")
    ap.add_argument("--dry-run", action="store_true",
                    help="print the plan, write nothing")
    ap.add_argument("--add", default=None,
                    help="comma-separated remote skill names to ADD to the catalog")
    ap.add_argument("--complete", action="store_true",
                    help="restore full skill_md for historically simplified "
                         "(frontmatter-only) entries from the remote SKILL.md. "
                         "Does not change skill_folder_md5 (the remote dir is "
                         "unchanged). Use this to undo the one-off size-reduction "
                         "trim that left ~20 entries with an empty body.")
    ap.add_argument("--verify", action="store_true",
                    help="verify every catalog entry against the remote: recompute "
                         "folder_md5 and compare the FULL 32-char string, check "
                         "skill_md byte-identity and frontmatter order. Write nothing. "
                         "Use this after any refresh/add to catch errors without "
                         "relying on eyeballing diffs.")
    args = ap.parse_args()

    if args.verify:
        skills_root, tmp_dir = acquire_clone(args.clone_dir)
        try:
            verify(skills_root, args.out)
        finally:
            if tmp_dir:
                import shutil
                shutil.rmtree(tmp_dir, ignore_errors=True)
        return

    if args.complete:
        skills_root, tmp_dir = acquire_clone(args.clone_dir)
        try:
            complete(skills_root, args)
        finally:
            if tmp_dir:
                import shutil
                shutil.rmtree(tmp_dir, ignore_errors=True)
        return

    skills_root, tmp_dir = acquire_clone(args.clone_dir)
    try:
        run(skills_root, args)
    finally:
        if tmp_dir:
            import shutil
            shutil.rmtree(tmp_dir, ignore_errors=True)


def run(skills_root, args):
    remote = enumerate_skills(skills_root)
    print("remote skills: {}".format(len(remote)), file=sys.stderr)

    catalog = json.loads(open(args.out, encoding="utf-8").read())
    existing = {s["name"]: s for s in catalog["skills"]}
    print("catalog skills: {}".format(len(existing)), file=sys.stderr)

    updated = []
    unchanged = 0
    new_cands = []
    gone_cands = []

    for name in sorted(remote):
        skill_dir = remote[name]
        md5 = folder_md5(skill_dir, name)
        old = existing.get(name)
        if old is None:
            new_cands.append(name)
            continue
        if old["skill_folder_md5"] == md5:
            unchanged += 1
            continue
        # MD5 changed: refresh the entry in place with full skill_md.
        raw = open(os.path.join(skill_dir, SKILL_MD), encoding="utf-8").read().replace(
            "\r\n", "\n"
        )
        old["frontmatter"] = parse_frontmatter(raw)
        old["skill_md"] = raw
        old["install_cmd"] = make_install_cmd(name)
        old["remove_cmd"] = make_remove_cmd(name)
        old["skill_folder_md5"] = md5
        updated.append(name)

    for s in catalog["skills"]:
        if s["name"] not in remote:
            gone_cands.append(s["name"])

    # Materialize explicitly-requested new skills (--add).
    added = []
    if args.add:
        for name in (n.strip() for n in args.add.split(",")):
            if not name:
                continue
            if name not in remote:
                print("WARN: --add {}: not found in remote".format(name), file=sys.stderr)
                continue
            if name in existing:
                print("WARN: --add {}: already in catalog".format(name), file=sys.stderr)
                continue
            catalog["skills"].append(build_entry(name, remote[name]))
            added.append(name)

    catalog["skills"].sort(key=lambda s: s["name"])

    print("\n--- summary ---", file=sys.stderr)
    print("unchanged: {}".format(unchanged), file=sys.stderr)
    print("updated:   {}".format(len(updated)), file=sys.stderr)
    for n in updated:
        print("  ~ {}".format(n), file=sys.stderr)
    if new_cands:
        print("new candidates (remote only, NOT written): {}".format(len(new_cands)),
              file=sys.stderr)
        for n in new_cands:
            print("  + {}".format(n), file=sys.stderr)
        print("  to add them: re-run with --add={}".format(",".join(new_cands)),
              file=sys.stderr)
    if gone_cands:
        print("REMOVED candidates (catalog only, NOT deleted): {}".format(len(gone_cands)),
              file=sys.stderr)
        for n in gone_cands:
            print("  - {}".format(n), file=sys.stderr)
    if added:
        print("added (--add): {}".format(len(added)), file=sys.stderr)
        for n in added:
            print("  + {}".format(n), file=sys.stderr)

    if args.dry_run:
        print("\n(dry-run: no file written)", file=sys.stderr)
        return
    if not updated and not added:
        print("\nno changes to write", file=sys.stderr)
        return

    # json.dumps(ensure_ascii=False, indent=2) reproduces the original
    # catalog's byte form exactly: no HTML escaping, key order preserved,
    # no trailing newline.
    out = json.dumps(catalog, ensure_ascii=False, indent=2)
    tmp_path = args.out + ".tmp"
    with open(tmp_path, "w", encoding="utf-8") as f:
        f.write(out)
    os.replace(tmp_path, args.out)
    print("\nwrote {} ({} skills)".format(args.out, len(catalog["skills"])),
          file=sys.stderr)


def complete(skills_root, args):
    """Restore full skill_md for entries that don't match the remote.

    Two kinds of mismatch are fixed:
      - Historically simplified: skill_md body was trimmed to frontmatter-only
        in a one-off size-reduction pass.
      - Stale/incorrect: skill_md was generated from an older or mis-copied
        SKILL.md (e.g. a name typo like "dws-mem-diag" vs "dws-dymem-diag").

    Both are fixed the same way: overwrite skill_md + frontmatter with the
    current remote SKILL.md. skill_folder_md5 is left untouched — it hashes
    the whole remote directory, and the directory's md5 is still correct
    (verified by --verify), even when an individual file inside it was
    edited in the catalog copy.

    Supports --dry-run to preview which entries would be completed.
    """
    remote = enumerate_skills(skills_root)
    print("remote skills: {}".format(len(remote)), file=sys.stderr)

    catalog = json.loads(open(args.out, encoding="utf-8").read())
    by = {s["name"]: s for s in catalog["skills"]}
    print("catalog skills: {}".format(len(by)), file=sys.stderr)

    completed = []
    skipped_match = 0
    skipped_not_in_remote = 0

    for name in sorted(by):
        entry = by[name]
        skill_dir = remote.get(name)
        if skill_dir is None:
            print("WARN: {} not in remote, cannot complete".format(name),
                  file=sys.stderr)
            skipped_not_in_remote += 1
            continue

        raw = open(os.path.join(skill_dir, SKILL_MD), encoding="utf-8").read().replace(
            "\r\n", "\n"
        )
        if entry.get("skill_md") == raw:
            skipped_match += 1
            continue

        # skill_md differs from remote (simplified, stale, or typo). Overwrite
        # with the remote content. Do NOT touch skill_folder_md5 — the remote
        # directory's md5 is unchanged (verified by --verify).
        entry["skill_md"] = raw
        entry["frontmatter"] = parse_frontmatter(raw)
        completed.append(name)

    print("\n--- complete ---", file=sys.stderr)
    print("completed (skill_md synced to remote): {}".format(len(completed)),
          file=sys.stderr)
    for n in completed:
        print("  + {}".format(n), file=sys.stderr)
    print("skipped (already match remote): {}".format(skipped_match), file=sys.stderr)
    print("skipped (not in remote): {}".format(skipped_not_in_remote), file=sys.stderr)

    if args.dry_run:
        print("\n(dry-run: no file written)", file=sys.stderr)
        return
    if not completed:
        print("\nno changes to write", file=sys.stderr)
        return

    catalog["skills"].sort(key=lambda s: s["name"])
    out = json.dumps(catalog, ensure_ascii=False, indent=2)
    tmp_path = args.out + ".tmp"
    with open(tmp_path, "w", encoding="utf-8") as f:
        f.write(out)
    os.replace(tmp_path, args.out)
    print("\nwrote {} ({} skills)".format(args.out, len(catalog["skills"])),
          file=sys.stderr)


def verify(skills_root, catalog_path):
    """Verify every catalog entry against the remote, machine-checked.

    Primary check (the one that matters for the wasm plugin): recompute
    folder_md5 and compare the FULL 32-char string (not a prefix) against
    the catalog's skill_folder_md5. This is the guard against model
    hallucination or lazy prefix-only comparison — it is a full-string ==
    computed by code, not by a human or model eyeballing characters.

    Secondary checks (skill_md byte-identity, frontmatter order) are only
    meaningful for entries the script itself just wrote (updated or added).
    For unchanged entries whose skill_md is a historically simplified
    (frontmatter-only) stub, skill_md will legitimately differ from the
    remote full SKILL.md — that is expected, not an error. So secondary
    checks are reported as informational, not as failures, when the md5
    matches (i.e. the entry is unchanged and was not just refreshed).

    Writes nothing. Exits 1 if any md5 mismatch is found.
    """
    remote = enumerate_skills(skills_root)
    print("remote skills: {}".format(len(remote)), file=sys.stderr)

    catalog = json.loads(open(catalog_path, encoding="utf-8").read())
    by = {s["name"]: s for s in catalog["skills"]}
    print("catalog skills: {}".format(len(by)), file=sys.stderr)

    md5_ok = md5_fail = 0
    not_in_remote = []
    md5_failures = []
    simplified = 0  # unchanged entries whose skill_md is a trimmed stub

    for name in sorted(by):
        entry = by[name]
        skill_dir = remote.get(name)
        if skill_dir is None:
            not_in_remote.append(name)
            continue

        remote_md5 = folder_md5(skill_dir, name)
        md5_match = entry["skill_folder_md5"] == remote_md5
        if md5_match:
            md5_ok += 1
            # Detect historically simplified entries: md5 matches (entry is
            # current) but skill_md is shorter than the remote full SKILL.md.
            # This is expected — not a script error.
            raw = open(os.path.join(skill_dir, SKILL_MD), encoding="utf-8").read().replace(
                "\r\n", "\n"
            )
            if len(entry["skill_md"]) < len(raw):
                simplified += 1
        else:
            md5_fail += 1
            md5_failures.append((name, entry["skill_folder_md5"], remote_md5))

    total = md5_ok + md5_fail
    print("\n--- verify ---", file=sys.stderr)
    print("checked: {} (catalog {} minus {} not in remote)".format(
        total, len(by), len(not_in_remote)), file=sys.stderr)
    print("skill_folder_md5 (FULL 32-char compare): {} ok, {} fail".format(
        md5_ok, md5_fail), file=sys.stderr)
    print("  of the {} ok: {} are historically simplified (skill_md trimmed, expected)".format(
        md5_ok, simplified), file=sys.stderr)
    if not_in_remote:
        print("not in remote (skipped): {}".format(len(not_in_remote)), file=sys.stderr)
        for n in not_in_remote:
            print("  ? {}".format(n), file=sys.stderr)
    if md5_failures:
        print("\nMD5 MISMATCHES (these entries need a refresh):", file=sys.stderr)
        for name, cat, rem in md5_failures:
            print("  {}: catalog={} remote={}".format(name, cat, rem), file=sys.stderr)

    all_ok = not md5_failures
    print("\nVERDICT: {}".format(
        "PASS — all skill_folder_md5 match remote" if all_ok
        else "FAIL — md5 mismatches above need a refresh"), file=sys.stderr)
    if not all_ok:
        sys.exit(1)


if __name__ == "__main__":
    main()

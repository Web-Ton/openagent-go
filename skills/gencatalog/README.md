# gencatalog — refresh `INSTALLABLE_SKILLS.json`

`skills/gencatalog/main.py` is a Python script that keeps
`skills/INSTALLABLE_SKILLS.json` in sync with the remote skill repository
`https://gitcode.com/huaweicloud/huaweicloud-skills.git`.

It is the tool you run when:

1. **A remote skill's content changed** — recompute its `skill_folder_md5` and
   refresh the entry.
2. **A new skill landed on the remote** — report it as a candidate, then
   materialize it after confirming.

## Why `skill_folder_md5` must be the *remote* directory's MD5

The catalog is consumed at runtime by the `skill-manager.wasm` plugin
(see `examples/plugin/plugins/skill-manager/src/lib.rs` on `feat/skill-upgrade`).
Its `is_upgradeable` logic is:

```rust
let local = host::directory_md5(&format!("{}/{}", skills_dir, name)); // local installed dir
local != remote_md5   // remote_md5 == catalog's skill_folder_md5
```

`host::directory_md5` calls `plugin/wasmhost/fs.go` `DirectoryMD5`, which
calls `skill/fs.FolderMD5(path, filepath.Base(path))` — **the same
algorithm this tool uses** (reimplemented in Python below). So the catalog's
`skill_folder_md5` is compared against an MD5 of the **local installed
directory** computed with the same algorithm. For the comparison to detect
remote changes, the catalog value must be the **remote** directory's MD5.
If it were the local MD5, the two would always match and upgrades would
never be detected.

Because `FolderMD5` does not depend on the parent path (only the directory
name + file contents), an MD5 computed over a local shallow clone of the
remote is **identical** to one computed over the true remote directory.
This is verified: three known skills' catalog MD5s match the values
recomputed from a local clone exactly.

## The MD5 algorithm

Defined in [`skill/fs/md5.go`](../fs/md5.go) `FolderMD5(dirpath, dirname)`,
and mirrored byte-for-byte by `folder_md5()` in `main.py`:

```
entries = [dirname]
walk depth-first; at each level:
  files (sorted by name)   -> append "relpath:md5(filebytes)"
  subdirs (sorted by name) -> recurse
result = hex(md5("\n".join(entries)))
```

- The **directory name** participates (renaming the dir changes the MD5);
  the **parent path does not** (moving the dir does not).
- **File contents** participate.
- **Symlinks, FIFOs, devices, sockets are skipped** (parity with Go's
  `walkSorted`; also avoids blocking on a FIFO with no writer).
- It hashes the **entire skill directory** (SKILL.md + references/ +
  scripts/ + everything), not just SKILL.md.

`folder_md5()` is verified to produce identical output to the Go
`FolderMD5` on known skills — the Python and Go implementations are
interchangeable.

## Usage

```sh
# Refresh the catalog in place: clone remote, recompute MD5s, overwrite
# entries whose MD5 changed. New/removed skills are reported only.
python3 skills/gencatalog/main.py

# Dry run: print the summary, write nothing.
python3 skills/gencatalog/main.py --dry-run

# Materialize specific new-skill candidates after reviewing the summary.
python3 skills/gencatalog/main.py --add=huawei-cloud-dew-key-management,huawei-cloud-cts-trace-management

# Reuse an existing clone instead of fetching (handy for offline iteration).
python3 skills/gencatalog/main.py --clone-dir=/tmp/huaweicloud-skills-probe
```

Requires Python 3.7+ and `pyyaml` (`pip install pyyaml`). No other
dependencies.

Options:

| option         | default                                                         | meaning                                                          |
| -------------- | --------------------------------------------------------------- | ---------------------------------------------------------------- |
| `--out`        | `skills/INSTALLABLE_SKILLS.json`                                | catalog path to read and write                                   |
| `--clone-dir`  | (temp dir)                                                      | reuse this directory as the clone root (skips clone if `skills/` exists there) |
| `--dry-run`    | off                                                             | print the plan, write nothing                                    |
| `--add`        | (none)                                                          | comma-separated remote skill names to ADD to the catalog         |

## What it does — and does NOT — do

**Does:**

- Shallow-clone the remote repo to a temp dir (cleaned up on exit).
- For every remote skill, recompute `skill_folder_md5` with `folder_md5`
  (the plugin's algorithm) and read the full `SKILL.md`.
- For each **existing** catalog entry: if the MD5 changed, overwrite the
  entry with the fresh MD5, **full** `skill_md`, parsed `frontmatter`, and
  regenerated `install_cmd` / `remove_cmd`. If unchanged, leave the entry
  untouched.
- Sort the skills array by name (the shipped file is name-sorted).
- Write atomically (`.tmp` + `os.replace`).

**Does NOT:**

- **Auto-add** remote-only skills. They are printed as `new candidates`
  with the exact `--add=...` command to run. Add them explicitly.
- **Auto-delete** catalog-only skills. They are printed as `REMOVED
  candidates`. Remove them by hand if confirmed.
- Touch entries whose MD5 is unchanged — including the ~20 historically
  **simplified** entries whose `skill_md` was trimmed to frontmatter-only
  to shrink the file. Those keep their simplified `skill_md` until their
  remote content actually changes, at which point they are refreshed to
  the full `SKILL.md`. **When adding a new skill, always use the full
  `SKILL.md`** — do not imitate the simplified entries; they are an
  artifact of a one-off size reduction, not a format to follow.

## Why Python (not Go)

The catalog file is itself `json.dumps(d, ensure_ascii=False, indent=2)`
output (verified: a round-trip through Python's `json` is byte-identical
to the shipped file). Python's `dict` preserves insertion order (3.7+),
`json.dumps(ensure_ascii=False)` never HTML-escapes `<`/`>`/`&`, and there
is no trailing-newline quirk. So re-serializing an unchanged entry is
byte-identical with **zero custom machinery**.

A Go port needs a custom ordered-map type for **every nesting level** —
`frontmatter` values are themselves objects and arrays-of-objects
(`trigger{keywords,resource_types,hypotheses}`,
`input_schema{required,optional}`,
`output_schema[{name,type,description}]`), and Go's `map` does not
preserve order while `encoding/json` alphabetizes map keys and HTML-escapes
by default. Getting all of that right recursively is bug-prone (an earlier
Go attempt reordered nested keys, escaped HTML, and added a trailing
newline). Python sidesteps every one of those pitfalls for free.

## Catalog entry format

2-space indentation, fixed field order:

```json
{
  "name": "huawei-cloud-<...>",
  "frontmatter": { "name": "...", "description": "...", "...": "..." },
  "skill_md": "---\nname: ...\n---\n\n# full SKILL.md body...\n",
  "install_cmd": { "cmd": "npx", "args": ["-y", "skills", "add", "<url>", "--skill", "<name>", "-g", "-y"] },
  "remove_cmd": { "cmd": "npx", "args": ["-y", "skills", "remove", "<name>", "-g", "-y"] },
  "skill_folder_md5": "<32 hex chars>"
}
```

- `frontmatter` is the **complete** YAML frontmatter from `SKILL.md` — all
  keys (`name`, `description`, `tags`, `version`, `allowed-tools`,
  `compatibility`, …) in the order the SKILL.md author wrote them, not just
  `name`/`description`.
- `skill_md` is the **full** `SKILL.md` text (frontmatter + body), with
  CRLF normalized to LF.
- `install_cmd` / `remove_cmd` all use the same remote URL.

## Remote repository layout

```
huaweicloud-skills/
└── skills/
    ├── agentorchard/common/huawei-cloud-agentorchard-find-skills/SKILL.md
    ├── ai/modelarts/huawei-cloud-ascend-command/SKILL.md
    ├── bigdata/dws/huawei-cloud-dws-io-diag/SKILL.md
    └── ...   (skills/<category>/<subcat?>/<skill-name>/SKILL.md)
```

A skill is any directory containing `SKILL.md`. The skill **name** is the
directory's base name (the leaf), which matches the catalog `name` field.

## Verification checklist

After a real run:

1. `python3 -m json.tool skills/INSTALLABLE_SKILLS.json > /dev/null` — valid JSON.
2. `git diff skills/INSTALLABLE_SKILLS.json` — only intended entries changed;
   indentation and field order match the prior file.
3. Spot-check one **unchanged** entry with nested frontmatter (e.g.
   `huawei-cloud-dws-cpu-diag`): its `trigger` keys are still
   `keywords, resource_types, hypotheses` (source order, not alphabetical)
   — proving unchanged entries are left byte-identical.
4. Spot-check one **updated** entry: its `skill_folder_md5` equals the
   remote-computed value (the summary lists which were updated).
5. Entry count: after a plain `python3 skills/gencatalog/main.py` (no
   `--add`), the count is unchanged; only after `--add=...` does it grow.

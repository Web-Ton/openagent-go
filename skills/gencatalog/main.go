// Command gencatalog refreshes skills/INSTALLABLE_SKILLS.json against the
// remote huaweicloud-skills repository.
//
// It shallow-clones https://gitcode.com/huaweicloud/huaweicloud-skills.git,
// recomputes each skill directory's aggregate MD5 with the SAME algorithm
// the skill-manager.wasm plugin uses at runtime (skill/fs.FolderMD5), and
// updates catalog entries whose remote MD5 has changed.
//
// New skills (present remotely, absent from the catalog) and removed skills
// (present in the catalog, absent remotely) are REPORTED ONLY — they are
// never silently added or deleted. Add new entries by hand after confirming,
// or re-run with -add=name1,name2 to materialize specific candidates.
//
// Usage:
//
//	go run ./skills/gencatalog                 # refresh + overwrite
//	go run ./skills/gencatalog -dry-run        # print summary, write nothing
//	go run ./skills/gencatalog -add=foo,bar    # refresh + add named skills
//
// The catalog path defaults to skills/INSTALLABLE_SKILLS.json relative to
// the current working directory.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	skillfs "github.com/yusheng-g/openagent-go/skill/fs"
)

// defaultRemoteURL is the repository the catalog is sourced from.
// Every shipped install_cmd points here; changing it would invalidate
// existing entries.
const defaultRemoteURL = "https://gitcode.com/huaweicloud/huaweicloud-skills.git"

// defaultCatalogPath is relative to the working directory (repo root).
const defaultCatalogPath = "skills/INSTALLABLE_SKILLS.json"

// skillMDName is the marker file that identifies a skill leaf directory.
const skillMDName = "SKILL.md"

// catalog is the on-disk JSON shape.
type catalog struct {
	Schema string  `json:"schema"`
	Skills []entry `json:"skills"`
}

// entry is one skill in the catalog. Field order is fixed to match the
// existing file (name, frontmatter, skill_md, install_cmd, remove_cmd,
// skill_folder_md5) — json.Marshal preserves struct field order.
type entry struct {
	Name           string     `json:"name"`
	Frontmatter    orderedMap `json:"frontmatter"`
	SkillMD        string     `json:"skill_md"`
	InstallCmd     cmdSpec    `json:"install_cmd"`
	RemoveCmd      cmdSpec    `json:"remove_cmd"`
	SkillFolderMD5 string     `json:"skill_folder_md5"`
}

// cmdSpec is the {cmd, args} shape used by install_cmd / remove_cmd.
// Field order (cmd, args) matches the existing catalog.
type cmdSpec struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

// orderedMap is a JSON object that preserves key insertion order on both
// unmarshal and marshal. It is used for frontmatter so that re-serializing
// an unchanged entry produces byte-identical output: the existing catalog
// preserves the YAML source's key order (name, description, tags, ... in
// whatever order each SKILL.md author wrote them), which is neither
// alphabetical nor a single fixed order. A plain map[string]any would
// re-sort keys alphabetically and churn every entry in the diff.
//
// Values are stored as their natural JSON types (string, []any, etc.) just
// like map[string]any would hold, so the only behavioral difference from
// map[string]any is key ordering.
type orderedMap struct {
	keys []string
	vals map[string]any
}

func newOrderedMap() orderedMap {
	return orderedMap{vals: map[string]any{}}
}

// Set inserts or overwrites a key, appending to the key order on first
// insertion.
func (m *orderedMap) Set(k string, v any) {
	if m.vals == nil {
		m.vals = map[string]any{}
	}
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// MarshalJSON renders the object with keys in insertion order. It uses a
// json.Encoder with SetEscapeHTML(false) for the values so that literal
// '<', '>', '&' in frontmatter strings (e.g. "Python3 >= 3.8") are NOT
// emitted as </>/& — matching the top-level writeCatalog
// encoder and the existing catalog's literal form. A plain json.Marshal
// here would re-escape and churn the diff even with writeCatalog's encoder
// configured, because the value bytes are already escaped before they
// reach the top-level encoder.
func (m orderedMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshalNoEscape(m.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalNoEscape serializes v with HTML escaping disabled, so '<', '>',
// '&' survive as literal bytes. Used for frontmatter keys/values where the
// existing catalog stores them literally.
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a trailing newline; trim it so the bytes compose
	// cleanly into the surrounding object.
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// UnmarshalJSON reads an object preserving key order. Non-object JSON
// yields an empty map rather than an error, matching the leniency of
// map[string]any for a missing/null frontmatter.
func (m *orderedMap) UnmarshalJSON(data []byte) error {
	*m = newOrderedMap()
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != json.Delim('{') {
		// null or non-object: leave empty.
		return nil
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("expected string key, got %T", t)
		}
		var val any
		if err := dec.Decode(&val); err != nil {
			return err
		}
		m.Set(key, val)
	}
	_, err = dec.Token() // consume closing '}'
	return err
}

func main() {
	var (
		remoteURL  = flag.String("url", defaultRemoteURL, "remote skills repository URL")
		catalogArg = flag.String("out", defaultCatalogPath, "catalog JSON path to read/write")
		cloneDir   = flag.String("clone-dir", "", "reuse this clone dir instead of a temp dir (skip clone if it already contains skills/)")
		dryRun     = flag.Bool("dry-run", false, "print the planned changes; write nothing")
		addCSV     = flag.String("add", "", "comma-separated remote skill names to ADD to the catalog (materialize new-skill candidates)")
	)
	flag.Parse()

	if err := run(*remoteURL, *catalogArg, *cloneDir, *dryRun, *addCSV); err != nil {
		fmt.Fprintf(os.Stderr, "gencatalog: %v\n", err)
		os.Exit(1)
	}
}

func run(remoteURL, catalogPath, cloneDir string, dryRun bool, addCSV string) error {
	// 1. Acquire the remote tree.
	skillsRoot, tmpDir, err := acquireClone(remoteURL, cloneDir)
	if err != nil {
		return err
	}
	if tmpDir != "" {
		defer os.RemoveAll(tmpDir)
	}

	// 2. Enumerate remote skills: name -> absolute dir.
	remote, err := enumerateSkills(skillsRoot)
	if err != nil {
		return fmt.Errorf("enumerate remote skills: %w", err)
	}
	fmt.Fprintf(os.Stderr, "remote skills: %d\n", len(remote))

	// 3. Load the existing catalog.
	cat, err := loadCatalog(catalogPath)
	if err != nil {
		return err
	}
	existing := make(map[string]*entry, len(cat.Skills))
	for i := range cat.Skills {
		e := &cat.Skills[i]
		existing[e.Name] = e
	}
	fmt.Fprintf(os.Stderr, "catalog skills: %d\n", len(existing))

	// 4. Diff remote vs catalog and build the updated entry list.
	var (
		updated   []string
		unchanged int
		newCands  []string
		goneCands []string
	)

	// Walk remote in stable (name-sorted) order.
	remoteNames := make([]string, 0, len(remote))
	for n := range remote {
		remoteNames = append(remoteNames, n)
	}
	sort.Strings(remoteNames)

	for _, name := range remoteNames {
		dir := remote[name]
		md5Val, skillMD, fm, err := computeSkill(dir, name)
		if err != nil {
			// A single broken skill should not abort the whole run;
			// report and skip.
			fmt.Fprintf(os.Stderr, "WARN: skip %s: %v\n", name, err)
			continue
		}
		old, ok := existing[name]
		if !ok {
			// Remote-only: candidate for addition. Do not write unless
			// explicitly requested via -add.
			newCands = append(newCands, name)
			continue
		}
		if old.SkillFolderMD5 == md5Val {
			unchanged++
			continue
		}
		// MD5 changed: refresh the entry in place with full skill_md.
		updated = append(updated, name)
		old.Frontmatter = fm
		old.SkillMD = skillMD
		old.InstallCmd = makeInstallCmd(remoteURL, name)
		old.RemoveCmd = makeRemoveCmd(name)
		old.SkillFolderMD5 = md5Val
	}

	// Catalog entries not seen remotely: removal candidates.
	for i := range cat.Skills {
		name := cat.Skills[i].Name
		if _, ok := remote[name]; !ok {
			goneCands = append(goneCands, name)
		}
	}
	sort.Strings(goneCands)

	// 5. Materialize explicitly-requested new skills (-add).
	added := []string{}
	if addCSV != "" {
		want := strings.Split(addCSV, ",")
		for _, w := range want {
			name := strings.TrimSpace(w)
			if name == "" {
				continue
			}
			dir, ok := remote[name]
			if !ok {
				fmt.Fprintf(os.Stderr, "WARN: -add %s: not found in remote\n", name)
				continue
			}
			if _, already := existing[name]; already {
				fmt.Fprintf(os.Stderr, "WARN: -add %s: already in catalog\n", name)
				continue
			}
			md5Val, skillMD, fm, err := computeSkill(dir, name)
			if err != nil {
				return fmt.Errorf("-add %s: %w", name, err)
			}
			cat.Skills = append(cat.Skills, entry{
				Name:           name,
				Frontmatter:    fm,
				SkillMD:        skillMD,
				InstallCmd:     makeInstallCmd(remoteURL, name),
				RemoveCmd:      makeRemoveCmd(name),
				SkillFolderMD5: md5Val,
			})
			existing[name] = &cat.Skills[len(cat.Skills)-1]
			added = append(added, name)
		}
	}

	// 6. Sort skills by name (the shipped file is name-sorted) and print
	// the summary BEFORE writing, so a dry run is still useful.
	sort.Slice(cat.Skills, func(i, j int) bool {
		return cat.Skills[i].Name < cat.Skills[j].Name
	})

	printSummary(unchanged, updated, newCands, goneCands, added)

	// 7. Write (unless dry-run). When nothing changed and nothing was
	// added, skip the write to avoid a needless diff churn.
	if dryRun {
		fmt.Fprintln(os.Stderr, "\n(dry-run: no file written)")
		return nil
	}
	if len(updated) == 0 && len(added) == 0 {
		fmt.Fprintln(os.Stderr, "\nno changes to write")
		return nil
	}
	return writeCatalog(catalogPath, cat)
}

// acquireClone returns the path to the remote skills/ directory and the
// temp dir to clean up ("" if reusing a caller-provided clone).
func acquireClone(remoteURL, cloneDir string) (skillsRoot, tmpDir string, err error) {
	if cloneDir != "" {
		// Reuse an existing clone. If skills/ is already inside, assume
		// it was cloned previously; otherwise clone into it now.
		candidate := filepath.Join(cloneDir, "skills")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, "", nil
		}
		// Fall through and clone into cloneDir.
		tmpDir = cloneDir
	} else {
		tmpDir, err = os.MkdirTemp("", "gencatalog-")
		if err != nil {
			return "", "", fmt.Errorf("create temp dir: %w", err)
		}
	}

	if err := gitClone(remoteURL, tmpDir); err != nil {
		if cloneDir == "" {
			os.RemoveAll(tmpDir)
		}
		return "", "", err
	}
	return filepath.Join(tmpDir, "skills"), tmpDir, nil
}

// gitClone runs a shallow clone of remoteURL into dest.
func gitClone(remoteURL, dest string) error {
	cmd := exec.Command("git", "clone", "--depth", "1", remoteURL, dest)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w", remoteURL, err)
	}
	return nil
}

// enumerateSkills walks root and returns name -> skill directory for every
// directory containing a SKILL.md. The directory's base name is the skill
// name (matching the catalog convention).
func enumerateSkills(root string) (map[string]string, error) {
	out := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() != skillMDName {
			return nil
		}
		dir := filepath.Dir(path)
		name := filepath.Base(dir)
		if name == "." || name == "/" || name == "skills" {
			// SKILL.md directly under the category root — skip; a real
			// skill leaf has its own named directory.
			return nil
		}
		if _, dup := out[name]; dup {
			// Duplicate leaf name across categories: keep the first
			// encountered (stable walk order) and warn.
			fmt.Fprintf(os.Stderr, "WARN: duplicate skill name %s at %s (already seen)\n", name, dir)
			return nil
		}
		out[name] = dir
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// computeSkill returns the aggregate MD5, the full SKILL.md text (CRLF
// normalized), and the parsed frontmatter (key order preserved) for one
// skill directory.
func computeSkill(dir, name string) (md5Val, skillMD string, fm orderedMap, err error) {
	md5Val, err = skillfs.FolderMD5(dir, name)
	if err != nil {
		return "", "", newOrderedMap(), fmt.Errorf("md5 %s: %w", name, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, skillMDName))
	if err != nil {
		return "", "", newOrderedMap(), fmt.Errorf("read SKILL.md %s: %w", name, err)
	}
	// Normalize CRLF -> LF so the stored skill_md matches what the
	// skill/fs loader does (skill/fs/loader.go splitFrontmatter).
	skillMD = strings.ReplaceAll(string(raw), "\r\n", "\n")
	fm, err = parseFrontmatter([]byte(skillMD))
	if err != nil {
		return "", "", newOrderedMap(), fmt.Errorf("frontmatter %s: %w", name, err)
	}
	return md5Val, skillMD, fm, nil
}

// parseFrontmatter splits a SKILL.md into its YAML frontmatter as an
// orderedMap (key order preserved from the source). The body is not
// returned (the caller keeps the full skill_md). The boundary logic
// mirrors skill/fs/loader.go splitFrontmatter so the two agree on what
// counts as frontmatter; the YAML decode uses yaml.Node so that key
// insertion order — which the existing catalog preserves per-skill —
// survives the round trip instead of being alphabetized.
func parseFrontmatter(data []byte) (orderedMap, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return newOrderedMap(), fmt.Errorf("no frontmatter")
	}
	// Find the closing "---" on its own line. It is either "\n---\n"
	// (body follows) or "\n---" at EOF (no body). In both cases the YAML
	// block is text[4:4+idx] where idx is the offset of "\n---".
	idx := strings.Index(text[4:], "\n---\n")
	if idx == -1 {
		if strings.HasSuffix(text[4:], "\n---") {
			idx = len(text[4:]) - 4
		} else {
			return newOrderedMap(), fmt.Errorf("unclosed frontmatter")
		}
	}
	yamlBlock := text[4 : 4+idx]
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(yamlBlock), &node); err != nil {
		return newOrderedMap(), fmt.Errorf("invalid YAML: %w", err)
	}
	return nodeToOrderedMap(&node)
}

// nodeToOrderedMap converts a yaml.Node (expected to be a mapping) into an
// orderedMap, preserving key order. A top-level DocumentNode (the wrapper
// yaml.Unmarshal produces for a full YAML document) is unwrapped to its
// single Content[0] child first. Non-mapping nodes yield an empty map.
func nodeToOrderedMap(node *yaml.Node) (orderedMap, error) {
	m := newOrderedMap()
	if node == nil {
		return m, nil
	}
	// Unmarshaling a full document yields a DocumentNode whose Content[0]
	// is the actual mapping; unwrap it so the Kind check below sees the
	// real node. Without this, frontmatter would silently come back empty
	// (DocumentNode.Kind != MappingNode).
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return m, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		var val any
		if err := valNode.Decode(&val); err != nil {
			return m, fmt.Errorf("decode %q: %w", keyNode.Value, err)
		}
		m.Set(keyNode.Value, val)
	}
	return m, nil
}

// makeInstallCmd builds the install command for a skill. Matches the
// shipped catalog: npx -y skills add <url> --skill <name> -g -y.
func makeInstallCmd(remoteURL, name string) cmdSpec {
	return cmdSpec{
		Cmd: "npx",
		Args: []string{
			"-y", "skills", "add", remoteURL,
			"--skill", name, "-g", "-y",
		},
	}
}

// makeRemoveCmd builds the remove command for a skill.
func makeRemoveCmd(name string) cmdSpec {
	return cmdSpec{
		Cmd:  "npx",
		Args: []string{"-y", "skills", "remove", name, "-g", "-y"},
	}
}

// loadCatalog reads and parses the existing catalog file.
func loadCatalog(path string) (*catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	return &c, nil
}

// writeCatalog serializes the catalog with 2-space indentation, preserving
// the schema -> skills field order, and writes it atomically over the
// existing file.
//
// A json.Encoder is used with SetEscapeHTML(false) so that literal '<',
// '>', '&' in skill_md text (common in markdown like "<user input>") are
// NOT emitted as </>/&. The existing catalog mixes escaped
// and literal forms across entries (it was generated at different times);
// disabling HTML escaping yields readable literal output and keeps diffs
// quiet for unchanged entries once they settle.
func writeCatalog(path string, c *catalog) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("marshal catalog: %w", err)
	}
	data := buf.Bytes()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	fmt.Fprintf(os.Stderr, "\nwrote %s (%d skills)\n", path, len(c.Skills))
	return nil
}

func printSummary(unchanged int, updated, newCands, goneCands, added []string) {
	fmt.Fprintf(os.Stderr, "\n--- summary ---\n")
	fmt.Fprintf(os.Stderr, "unchanged: %d\n", unchanged)
	fmt.Fprintf(os.Stderr, "updated:   %d\n", len(updated))
	for _, n := range updated {
		fmt.Fprintf(os.Stderr, "  ~ %s\n", n)
	}
	if len(newCands) > 0 {
		fmt.Fprintf(os.Stderr, "new candidates (remote only, NOT written): %d\n", len(newCands))
		for _, n := range newCands {
			fmt.Fprintf(os.Stderr, "  + %s\n", n)
		}
		fmt.Fprintf(os.Stderr, "  to add them: re-run with -add=%s\n", strings.Join(newCands, ","))
	}
	if len(goneCands) > 0 {
		fmt.Fprintf(os.Stderr, "REMOVED candidates (catalog only, NOT deleted): %d\n", len(goneCands))
		for _, n := range goneCands {
			fmt.Fprintf(os.Stderr, "  - %s\n", n)
		}
	}
	if len(added) > 0 {
		fmt.Fprintf(os.Stderr, "added (-add): %d\n", len(added))
		for _, n := range added {
			fmt.Fprintf(os.Stderr, "  + %s\n", n)
		}
	}
}
